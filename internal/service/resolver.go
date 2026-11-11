// Resolver:解析编排——识别 → 缓存 → driver → 决策 → 落库。
package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/driver/mangodrive"
	"github.com/dengyie/panrouter/internal/pkg/breaker"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/limiter"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
)

// loggerType 由 main 注入(zap.SugaredLogger 别名,避免包级全局)。
type loggerType = zap.SugaredLogger

type Resolver struct {
	cfg      *config.Provider
	reg      *driver.Registry
	store    *repo.Store
	aes      *crypto.AES
	accounts *AccountService
	limiter  *limiter.Limiter
	breakers *breaker.Registry
	signer   *sign.Signer
	met      *metrics.Registry
	log      *loggerType

	// 过期直链刷新收敛:并发 /d 请求同一过期链接只触发一次真实刷新
	sf singleflight.Group
	// 单网盘在途并发闸(capacity 来自 driver 配置 download_concurrency)
	semMu sync.Mutex
	sems  map[string]chan struct{}
}

func NewResolver(cfg *config.Provider, reg *driver.Registry, store *repo.Store, aes *crypto.AES,
	accounts *AccountService, lim *limiter.Limiter, br *breaker.Registry, sg *sign.Signer,
	met *metrics.Registry, log *loggerType) *Resolver {
	return &Resolver{cfg: cfg, reg: reg, store: store, aes: aes, accounts: accounts,
		limiter: lim, breakers: br, signer: sg, met: met, log: log,
		sems: map[string]chan struct{}{}}
}

// acquire 获取单网盘在途并发额度。
// 满载时排队等待(保护账号不被并发拉爆),ctx 取消或超过等待上限才失败。
func (r *Resolver) acquire(ctx context.Context, pan string) (release func(), err error) {
	limit := int(r.driverCfg(pan).DownloadConc)
	if limit <= 0 {
		limit = 3
	}
	r.semMu.Lock()
	ch, exists := r.sems[pan]
	if !exists || cap(ch) != limit {
		ch = make(chan struct{}, limit)
		r.sems[pan] = ch
	}
	r.semMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, driver.NewErr(driver.KindUpstream, "等待并发额度时请求已取消", ctx.Err())
	case <-time.After(15 * time.Second):
		return nil, driver.NewErr(driver.KindRiskControl, "等待并发额度超时,请稍后重试", nil)
	}
}

// ---- 对外结果结构 ----

const (
	defaultAutoLinkMax = 8
	quarkAutoLinkMax   = 2 // 转存链单文件可达 ~30s;2 个仍低于 CF 524
	resolveShareBudget = 90 * time.Second
	minAutoLinkBudget  = 15 * time.Second
)

func autoLinkMaxFor(pan string) int {
	if pan == "quark" {
		return quarkAutoLinkMax
	}
	return defaultAutoLinkMax
}

func withDeadlineIfNone(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func canAutoLink(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) >= minAutoLinkBudget
}

func listingTruncated(nodes []driver.FileNode) bool {
	truncated := false
	for i := range nodes {
		if nodes[i].Ext == nil {
			continue
		}
		if nodes[i].Ext["truncated"] == "1" {
			truncated = true
			delete(nodes[i].Ext, "truncated")
		}
	}
	return truncated
}

func joinHints(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ";")
}

// FileItem 是分享列表行:只带页面需要的提链结果,避免把 ResolveFileResult 整份拷进来。
type FileItem struct {
	FID         string `json:"fid"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	IsDir       bool   `json:"is_dir"`
	Route       string `json:"route,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	LinkError   string `json:"link_error,omitempty"`
}

type ResolveShareResult struct {
	Pan       string     `json:"pan"`
	ShareKey  string     `json:"share_key"`
	Files     []FileItem `json:"files"`
	Hint      string     `json:"hint,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

type ResolveFileResult struct {
	Pan         string    `json:"pan"`
	ShareKey    string    `json:"share_key"`
	FID         string    `json:"fid"`
	FileName    string    `json:"file_name"`
	Size        int64     `json:"size"`
	Route       string    `json:"route"` // 302 | aria2 | stream
	DownloadURL string    `json:"download_url"`
	StreamURL   string    `json:"stream_url"`
	DirectURL   string    `json:"direct_url,omitempty"`
	UA          string    `json:"ua,omitempty"`
	Referer     string    `json:"referer,omitempty"`
	NeedHeaders bool      `json:"need_headers"`
	ExpiresAt   time.Time `json:"expires_at"`
	CacheHit    bool      `json:"cache_hit"`
}

// ---- 内部工具 ----

// Registry 暴露 driver 注册表(账号管理等场景需要按 pan 查 driver)。
func (r *Resolver) Registry() *driver.Registry { return r.reg }

func (r *Resolver) shareKey(pan, rawURL, pwd string) string {
	h := sha1.Sum([]byte(pan + "|" + rawURL + "|" + pwd))
	return hex.EncodeToString(h[:])[:16]
}

func (r *Resolver) driverCfg(pan string) config.DriverCommon {
	if dc, ok := r.cfg.Get().Drivers[pan]; ok {
		return dc
	}
	return config.DriverCommon{Enabled: true, LimitQPS: 2, DownloadConc: 3}
}

func (r *Resolver) guardKey(ctx context.Context, pan string) string {
	if canPick(ctx) {
		return pan
	}
	return pan + ":guest"
}

// guard 统一执行 熔断检查 → 本地限频 → 调用 → 熔断记录。
// 游客与已登录分桶,避免匿名刷分享把管理员提链熔断。
func (r *Resolver) guard(ctx context.Context, pan string, fn func() error) error {
	key := r.guardKey(ctx, pan)
	scope := "login"
	if key != pan {
		scope = "guest"
	}
	b := r.breakers.Get(key)
	r.met.Set("panrouter_breaker_open", map[string]string{"pan": pan, "scope": scope}, boolGauge(b.Opened()))
	if !b.Allow() {
		r.met.Inc("panrouter_driver_error_total", map[string]string{"pan": pan, "kind": "breaker_open"})
		return driver.NewErr(driver.KindRiskControl, "该网盘熔断中(近期风控/异常过多),请稍后再试", nil)
	}
	if !r.limiter.Allow(key, r.driverCfg(pan).LimitQPS) {
		return driver.NewErr(driver.KindRiskControl, "本地限频:请求过于频繁,请稍后", nil)
	}
	err := fn()
	fail := false
	var de *driver.Error
	if err != nil && errors.As(err, &de) {
		fail = de.Kind == driver.KindRiskControl || de.Kind == driver.KindUpstream
		r.met.Inc("panrouter_driver_error_total", map[string]string{"pan": pan, "kind": string(de.Kind)})
		if de.Kind == driver.KindInterfaceChanged {
			// 接口改版是高优告警:不熔断、不重试,直接通知维护者(禁手)
			r.log.Errorf("[ALARM] driver interface changed: pan=%s err=%s", pan, de.Error())
		}
	}
	b.Record(!fail, r.breakers.MaxFails(), r.breakers.Cooldown())
	r.met.Set("panrouter_breaker_open", map[string]string{"pan": pan, "scope": scope}, boolGauge(b.Opened()))
	return err
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// mapErr 处理 driver 错误的副作用(账号标记等),原样返回给上层映射 HTTP 状态。
func (r *Resolver) mapErr(ctx context.Context, pan string, acc *repo.Account, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if !errors.As(err, &de) {
		return err
	}
	switch de.Kind {
	case driver.KindAuthExpired:
		if acc != nil {
			r.accounts.MarkStatus(acc.ID, "expired")
		} else if !canPick(ctx) {
			de.UserHint = "该网盘需要登录后才能提链,请先登录;" + de.UserHint
		} else {
			de.UserHint = "该网盘需要登录态,请先在「账号管理」添加账号;" + de.UserHint
		}
	case driver.KindRiskControl:
		if acc != nil {
			r.accounts.MarkCooldown(acc.ID, 5*time.Minute)
		}
	}
	return err
}

type resolverCtxKey int

const pickOKCtxKey resolverCtxKey = 1

// WithAuthed 标记请求已通过管理员登录;游客解析不得读取网盘 Cookie。
func WithAuthed(ctx context.Context, ok bool) context.Context {
	if !ok {
		return ctx
	}
	return withPick(ctx)
}

func withPick(ctx context.Context) context.Context {
	return context.WithValue(ctx, pickOKCtxKey, true)
}

func canPick(ctx context.Context) bool {
	v, _ := ctx.Value(pickOKCtxKey).(bool)
	return v
}

func (r *Resolver) pick(ctx context.Context, pan string) (*repo.Account, *driver.Credential, error) {
	if !canPick(ctx) {
		return nil, nil, nil
	}
	acc, cred, err := r.accounts.Pick(pan)
	if err != nil {
		return nil, nil, driver.NewErr(driver.KindUpstream, "读取账号凭据失败", err)
	}
	return acc, cred, nil
}

// linkHoldsCookie 判断直链缓存是否带上游 Cookie。空明文(免登录盘)不算。
func (r *Resolver) linkHoldsCookie(l *repo.Link) bool {
	if l == nil || len(l.CookieEnc) == 0 {
		return false
	}
	plain, err := r.aes.DecryptBytes(l.CookieEnc)
	if err != nil {
		return true
	}
	return len(plain) > 0
}

// ---- 对外方法 ----

func (r *Resolver) ResolveShare(ctx context.Context, rawURL, pwd, clientUA string) (*ResolveShareResult, error) {
	ctx, cancel := withDeadlineIfNone(ctx, resolveShareBudget)
	defer cancel()

	drv, err := r.reg.Detect(rawURL)
	if err != nil {
		return nil, err
	}
	pan := drv.ID()
	acc, cred, err := r.pick(ctx, pan)
	if err != nil {
		return nil, err
	}
	var nodes []driver.FileNode
	err = r.guard(ctx, pan, func() error {
		nodes, err = drv.ResolveShare(ctx, driver.ShareLink{URL: rawURL, Pwd: pwd}, cred)
		return err
	})
	if err := r.mapErr(ctx, pan, acc, err); err != nil {
		return nil, err
	}
	truncated := listingTruncated(nodes)
	if truncated && len(nodes) == 1 && nodes[0].FID == "" && nodes[0].Name == "" {
		nodes = nil
	}
	key := r.shareKey(pan, rawURL, pwd)
	// ★ 快照落库必须是「有效的 JSON 数组」,不能是 json.Marshal(nil slice) 的
	//   字面量 "null"。原因:快照里带着 driver 私有上下文(quark 的
	//   pwd_id/stoken、lanzou 的 share_url/pwd),没有它该分享下所有文件都
	//   提不了链(quark「缺少夸克分享上下文」)。一次空解析就把快照写成空壳,
	//   会把**唯一**的上下文来源毁掉。
	//   故:nil 切片规范成空数组 "[]" —— 空数组是「解析成功但没有节点」的
	//   诚实表达,不与「null / 解析失败」混淆;repo.UpsertShare 侧另有写保护
	//   兜底(无有效内容的快照不覆盖已有好快照)。
	raw := []byte("[]")
	if nodes != nil {
		if b, err := json.Marshal(nodes); err == nil {
			raw = b
		}
	}
	if err := r.store.UpsertShare(&repo.Share{
		PanType: pan, ShareURL: rawURL, Pwd: pwd, ShareKey: key,
		RawTree: string(raw), ResolvedAt: time.Now(),
	}); err != nil {
		r.log.Warnw("save share: %v", err)
	}
	items := make([]FileItem, 0, len(nodes))
	linked, authBlocked, budgetStop := 0, false, false
	hint := ""
	limit := autoLinkMaxFor(pan)
	for _, n := range nodes {
		it := FileItem{FID: n.FID, Name: n.Name, Size: n.Size, IsDir: n.IsDir}
		if !n.IsDir && n.FID != "" && linked < limit && !authBlocked && !budgetStop {
			if !canAutoLink(ctx) {
				budgetStop = true
				hint = joinHints(hint, "请求剩余时间不足,未继续自动提链")
				r.log.Warnw("auto-link skipped: deadline", "pan", pan, "fid", n.FID)
			} else {
				linked++
				file, lerr := r.ResolveFile(ctx, rawURL, pwd, n.FID, false, clientUA)
				if lerr != nil {
					kind := "upstream_error"
					var de *driver.Error
					if errors.As(lerr, &de) {
						kind = string(de.Kind)
						it.LinkError = de.UserHint
						if de.Kind == driver.KindAuthExpired {
							authBlocked = true
							hint = joinHints(hint, de.UserHint)
						}
					} else {
						it.LinkError = lerr.Error()
					}
					if errors.Is(lerr, context.DeadlineExceeded) || errors.Is(lerr, context.Canceled) {
						budgetStop = true
						hint = joinHints(hint, "请求已超时或取消,未继续自动提链")
					}
					r.log.Warnw("auto-link failed", "pan", pan, "fid", n.FID, "kind", kind, "hint", it.LinkError)
				} else {
					it.Route = file.Route
					it.DownloadURL = file.DownloadURL
				}
			}
		}
		items = append(items, it)
	}
	if truncated {
		hint = joinHints(hint, fmt.Sprintf("分享过大,仅展开前 %d 个文件", len(nodes)))
	}
	r.met.Inc("panrouter_resolve_total", map[string]string{"pan": pan, "kind": "share"})
	return &ResolveShareResult{Pan: pan, ShareKey: key, Files: items, Hint: hint, Truncated: truncated}, nil
}

// ResolveFile 获取单个文件直链:缓存未过期直接返回,否则走 driver 并落库。
func (r *Resolver) ResolveFile(ctx context.Context, rawURL, pwd, fid string, own bool, clientUA string) (*ResolveFileResult, error) {
	drv, err := r.reg.Detect(rawURL)
	if err != nil {
		return nil, err
	}
	pan := drv.ID()
	key := r.shareKey(pan, rawURL, pwd)

	cached, err := r.store.GetLink(key, fid)
	if err != nil {
		return nil, fmt.Errorf("query link cache: %w", err)
	}
	if cached != nil {
		// 游客不得复用或覆盖带账号 Cookie 的直链(管理员提链结果不能共享给匿名 /resolve)。
		if !canPick(ctx) && r.linkHoldsCookie(cached) {
			return nil, r.mapErr(ctx, pan, nil, driver.NewErr(driver.KindAuthExpired, "该分享直链需要登录态", nil))
		}
		if cached.ExpiresAt.After(time.Now()) {
			r.met.Inc("panrouter_link_cache_hit_total", map[string]string{"pan": pan})
			res := r.buildResult(pan, key, cached, clientUA, true)
			if linkMetaIsPlaceholder(res.FileName, res.Size) {
				// 直链仍新鲜但元数据是历史占位值:直链复用缓存,展示名/大小
				// 再找一遍(快照/自愈/历史),不让「文件」+0 顶着真文件出场。
				if name, size, _, merr := r.fileMeta(key, fid, &driver.FileRef{FID: fid}); merr == nil &&
					!linkMetaIsPlaceholder(name, size) {
					res.FileName, res.Size = name, size
				}
			}
			return res, nil
		}
	}

	acc, cred, err := r.pick(ctx, pan)
	if err != nil {
		return nil, err
	}
	ref, err := r.buildRef(key, fid, own)
	if err != nil {
		return nil, err
	}
	release, err := r.acquire(ctx, pan)
	if err != nil {
		return nil, err
	}
	var dl driver.DirectLink
	err = r.guard(ctx, pan, func() error {
		dl, err = drv.GetDirectLink(ctx, cred, *ref)
		return err
	})
	release()
	if err := r.mapErr(ctx, pan, acc, err); err != nil {
		return nil, err
	}
	name, size, ext, merr := r.fileMeta(key, fid, ref)
	if merr != nil {
		return nil, merr
	}
	if linkMetaIsPlaceholder(name, size) {
		// driver 提链途中带回的元数据(mangodrive 的 fs/get 响应)优先于回问
		// driver 列目录:信息就在手上,不必再打一次上游。
		if dl.Name != "" {
			name, size = dl.Name, dl.Size
		}
	}
	if linkMetaIsPlaceholder(name, size) {
		// 调用方直接给了 fid(如 mangodrive 的 FID 就是存储路径)却没先列过分享:
		// 缓存与分享快照里都没有名字/大小,回问 driver 一次,别把「文件」+0 发出去。
		if n, s := r.driverStat(ctx, drv, rawURL, fid, cred); n != "" {
			name, size = n, s
		}
	}
	if linkMetaIsPlaceholder(name, size) {
		name = placeholderFileName
		size = 0
	}
	cookieEnc, err := r.aes.EncryptBytes([]byte(dl.Cookie))
	if err != nil {
		return nil, fmt.Errorf("encrypt cookie: %w", err)
	}
	// ★ ext 落库必须是「有效 JSON 对象」,不能是 json.Marshal(nil map) 的
	// 字面量 "null"。原因:mangodrive 的 Ext["path"] 是哈希 fid 反推真实
	// 存储路径的**唯一**来源;一旦某次解析没拿到上下文却把 "null" 写进
	// links.ext,该行就成了「有列无内容」的坏行 —— 反查若取到它就等于
	// 没恢复,裸哈希提链必然 404。空上下文一律落空串(不写列),
	// repo.UpsertLink 侧另有写保护兜底(空值不覆盖原值)。
	extJSON := ""
	if len(ext) > 0 {
		if b, err := json.Marshal(ext); err == nil {
			extJSON = string(b)
		}
	}
	l := &repo.Link{
		ShareKey: key, FID: fid, FileName: name, Size: size,
		DirectLink: dl.URL, ServerLink: dl.ServerURL, UA: dl.UA, Referer: dl.Referer, CookieEnc: cookieEnc,
		BindIP: dl.BindIP, ExpiresAt: dl.ExpiresAt, Ext: extJSON,
	}
	// 落库失败必须报错:直链未持久化时不得返回看似成功的下载入口(P2-6)。
	if err := r.store.UpsertLink(l); err != nil {
		return nil, fmt.Errorf("save link: %w", err)
	}
	r.met.Inc("panrouter_resolve_total", map[string]string{"pan": pan, "kind": "file"})
	return r.buildResult(pan, key, l, clientUA, false), nil
}

// buildRef 组装 FileRef:优先用已缓存 link 的 ext,其次从分享快照的节点里取。
// DB 故障向上传播;只有记录确实不存在时才允许降级为空上下文。
//
// 哈希 fid 兜底:mangodrive 对超长路径生成不可逆哈希 fid("p:<sha1>"),调用方
// (桌面端 parentDirOf 推不出父目录时)只能以任意 url + 裸 fid 提链,此时
// 本分享的缓存/快照都查不到 path。links 表按 fid 全局扫一条历史行——当初
// auto-link 它的分享快照把真实路径写进了 Ext["path"],从那里恢复。
func (r *Resolver) buildRef(key, fid string, own bool) (*driver.FileRef, error) {
	ref := &driver.FileRef{Own: own, FID: fid, ShareKey: key}
	if own {
		return ref, nil
	}
	l, err := r.store.GetLink(key, fid)
	if err != nil {
		return nil, fmt.Errorf("query link cache: %w", err)
	}
	if l != nil && l.Ext != "" {
		_ = json.Unmarshal([]byte(l.Ext), &ref.Ext)
	}
	// ★ 上下文有效性按「语义有效」判定(extHasValue),不能只看 len(map)>0:
	//   历史坏行 {"api":""}(mangodrive 的恒空 api 键)是「有键无值」,拿它当
	//   有效上下文会把后面的快照/自愈/历史恢复链全部短路 → 哈希 fid 提链必然
	//   404,而库里明明有可用行(v2.9 之前写入的形态)。
	if !extHasValue(ref.Ext) {
		sh, err := r.store.GetShare(key)
		if err != nil {
			return nil, fmt.Errorf("query share snapshot: %w", err)
		}
		if sh != nil {
			var nodes []driver.FileNode
			if json.Unmarshal([]byte(sh.RawTree), &nodes) == nil {
				for _, n := range nodes {
					if n.FID == fid && extHasValue(n.Ext) {
						ref.Ext = n.Ext
						break
					}
				}
			}
		}
	}
	// ★ 坏快照自愈:本分享的快照是「空壳」(raw_tree 为 "null"/"[]" —— 生产
	//   历史数据确有此形态,成因是 json.Marshal(nil slice) 产出字面量 "null")
	//   时,快照里读不到任何节点上下文,于是 ref.Ext 仍为空 → driver 报
	//   「缺少夸克分享上下文」/「缺少蓝奏云分享上下文」,该分享下**所有**文件
	//   都提不了链。
	//   自愈来源:同一网盘下**别的 share_key** 的快照。share_key 是
	//   sha1(pan|url|pwd),同一份分享的「/s/xxx」与「/s/xxx#/list/share/yyy」
	//   是两个 key、两条快照 —— 另一条里往往还留着完整的 pwd_id/stoken。
	//   只在同一 pan_type 内扫:不同网盘的 fid 空间互不相通,跨网盘取上下文必然串味。
	if !extHasValue(ref.Ext) && !own {
		healed, herr := r.healShareContext(key, fid)
		if herr != nil {
			return nil, fmt.Errorf("heal share context: %w", herr)
		}
		if extHasValue(healed) {
			ref.Ext = healed
		}
	}
	if !extHasValue(ref.Ext) && isHashFid(fid) {
		// 哈希 fid 无法从上下文推导路径时,任何一条同 fid 历史缓存都是恢复源。
		// 仅哈希形态启用:path 形态未命中由 driverStat 兜底,不必扫历史表;
		// 也杜绝其它驱动形态 fid 撞名时把无关 Ext 注入 FileRef。
		ext, herr := r.recoverExtFromHistory(fid)
		if herr != nil {
			return nil, fmt.Errorf("query link history: %w", herr)
		}
		if extHasValue(ext) {
			ref.Ext = ext
		}
	}
	return ref, nil
}

// extHasValue 判断解码后的 driver 上下文是否**语义有效**:至少携带一个非空值。
// len(map)>0 只说明有键 —— 历史坏行 {"api":""} 形态(mangodrive 的恒空 api 键,
// mangodrive.nodeOf 现行写法)会以「有键无值」冒充有效上下文。与 repo 层的
// extHasContent(针对**原始字符串**列的写保护口径)是两回事:一个判列内容,
// 一个判解码后的语义。
func extHasValue(m map[string]string) bool {
	for _, v := range m {
		if v != "" {
			return true
		}
	}
	return false
}

// healShareNode 在本分享快照是空壳时,从同一网盘**别的** share_key 快照里
// 找回该 fid 的节点(名字/大小/Ext 上下文:quark 的 pwd_id/stoken、
// lanzou 的 share_url/pwd)。
//
// ★ 这是坏快照唯一的自愈来源:share_key = sha1(pan|url|pwd),同一份分享的
// 不同 URL 形态(根链接 vs 带 #/list/share/yyy 的深链)是两个 key、两条快照。
// 一条被写坏("null"/"[]")时,另一条里往往还留着完整上下文。
//
// 口径:
//   - 只在同一 pan_type 内扫(不同网盘 fid 空间互不相通,跨网盘必然串味);
//   - 只取「有内容」的快照行("null"/"[]"/空串跳过,它们没有节点可言);
//   - 节点按 fid 精确匹配,且该节点的 Ext 必须携带内容(非空 map);
//   - 命中即返回,不做「合并多个来源」——不同来源的 stoken 可能互相矛盾,
//     混用比不混用更危险(夸克 stoken 与 pwd_id 必须同源)。
//
// ★ 必须校验「同一份分享」:fid 不是全局唯一的 ——
//   - 蓝奏云的 fid 恒为 "file"(每个分享的单文件节点都叫 "file");
//   - 不同分享/不同目录的节点 fid 也可能撞名。
//     若不校验分享身份,自愈会把**别的分享**的上下文(蓝奏云的 share_url/pwd)
//     注入本次请求 → 提链指向另一个分享的文件,且用户毫不知情 ——
//     静默给错文件比提链失败危险得多。
//     判定口径见 sameShareIdentity。
func (r *Resolver) healShareNode(key, fid string) (*driver.FileNode, error) {
	// 本分享的快照行(含 share_url 与网盘类型):既是「是否同一份分享」的判据源,
	// 也避免为拿 pan_type 再查一次库。
	self, err := r.store.GetShare(key)
	if err != nil {
		return nil, err
	}
	if self == nil || self.PanType == "" {
		return nil, nil
	}
	shares, err := r.store.ListSharesByPan(self.PanType)
	if err != nil {
		return nil, err
	}
	// 逆序消费(id 降序 = 最新快照优先):同一份分享的多条快照里,最新的那条
	// 上下文最新(quark 的 stoken 有时效)。取最旧的命中会拿过期上下文 ——
	// 夸克还有 stoken 过期重试兜底,mangodrive 的 path 缺失就直接 404。
	//
	// ★ 扫描上限 healScanLimit:这是「快照空壳」这条异常路径上的防御性扫描,
	//   命中率随分享总量增长而稀释(自愈找的是同一份分享的另一条快照,通常
	//   就在最近几条里);上限封顶后,空壳 miss 的最坏成本是常数次 JSON 解析,
	//   不会随 shares 总量线性膨胀。同 pan_type 超过上限的老快照不参与自愈
	//   (它们同时也是最不可能有新鲜上下文的)。
	scanFrom := len(shares) - healScanLimit
	if scanFrom < 0 {
		scanFrom = 0
	}
	for i := len(shares) - 1; i >= scanFrom; i-- {
		if shares[i].ShareKey == key {
			continue // 本分享自己的快照已查过
		}
		if !repo.RawTreeHasContent(shares[i].RawTree) {
			continue // 空壳快照没有节点
		}
		var nodes []driver.FileNode
		if json.Unmarshal([]byte(shares[i].RawTree), &nodes) != nil {
			continue
		}
		for j := range nodes {
			if nodes[j].FID != fid || !extHasValue(nodes[j].Ext) {
				continue
			}
			if !sameShareIdentity(self, &shares[i], &nodes[j]) {
				continue // 不是同一份分享的节点:不得拿它的上下文
			}
			n := nodes[j]
			return &n, nil
		}
	}
	return nil, nil
}

// sameShareIdentity 判断候选快照里的节点是否属于**同一份分享**。
//
// ★ 自愈的安全闸:fid 不是全局唯一标识 ——
//   - 蓝奏云每个分享的单文件节点 fid 恒为 "file";
//   - 不同分享的节点 fid 也可能撞名。
//     拿错上下文的后果是「提链指向另一个分享的文件」,且用户完全无感。
//
// 硬门槛:host 必须相同。不同 host = 不同的分享站点(夸克 / 蓝奏云 /
// Google Drive 聚合站),绝不可能是同一份分享;而「同一份分享的不同 URL 形态」
// (根链 vs 深链)host 必然相同。
//
// 硬门槛之后,按「由强到弱」逐条比对,任一条命中即视为同一份分享:
//
//  1. 分享 id:quark 的 /s/<id> 与节点 Ext["pwd_id"] 是同一个值,
//     蓝奏云的 /iXXXX 也在路径里。id 相同即同一份分享(与 URL 形态无关)。
//  2. pwd_id 相同(两侧快照都带时)。
//  3. host+path 相同:同一份分享的两种 URL 形态
//     (根链 "/s/xxx" 与深链 "/s/xxx#/list/share/yyy")path 相同。
//  4. 路径前缀(mangodrive):节点的 Ext["path"] 以本分享 URL 的 path 为前缀
//     → 同一棵目录树下的文件。
//
// 四条都判不出来 → 视为**不是**同一份分享(宁可不自愈,也不给错文件)。
func sameShareIdentity(self *repo.Share, cand *repo.Share, node *driver.FileNode) bool {
	if self == nil || cand == nil {
		return false
	}
	selfHost := hostOf(self.ShareURL)
	candHost := hostOf(cand.ShareURL)
	// 节点自带的 share_url(蓝奏云)也参与 host 判定:它才是真正被解析的地址。
	nodeURLs := []string{node.Ext["share_url"]}
	if selfHost == "" {
		return false
	}
	if candHost != "" && candHost != selfHost {
		return false
	}
	for _, u := range nodeURLs {
		if h := hostOf(u); h != "" && h != selfHost {
			return false
		}
	}
	selfID := shareIDOf(self.ShareURL)
	selfPwd := shareIdentityOf(self, "pwd_id")
	selfPath := hostPathOf(self.ShareURL)
	candURLs := append([]string{cand.ShareURL}, nodeURLs...)

	// ① 分享 id:候选节点的 pwd_id 或候选 URL 里的 id 与本分享 id 相同。
	if selfID != "" {
		if node.Ext["pwd_id"] != "" && node.Ext["pwd_id"] == selfID {
			return true
		}
		for _, u := range candURLs {
			if shareIDOf(u) == selfID {
				return true
			}
		}
	}
	// ② pwd_id 相同(两侧快照都带时)。
	if selfPwd != "" && node.Ext["pwd_id"] != "" && selfPwd == node.Ext["pwd_id"] {
		return true
	}
	// ③ host+path 相同。
	if selfPath != "" {
		for _, u := range candURLs {
			if hostPathOf(u) == selfPath {
				return true
			}
		}
	}
	// ④ 路径前缀(mangodrive 的 fid 就是存储路径,目录树关系最可靠)。
	if p := node.Ext["path"]; p != "" && sharePathOf(self.ShareURL) != "" {
		if pathHasPrefix(p, sharePathOf(self.ShareURL)) {
			return true
		}
	}
	return false
}

// hostOf 取 URL 的 host(小写);解析失败或为空返回空串。
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// pathHasPrefix 判定 b 是否为 a 的**目录段**前缀("/a/b" 以 "/a" 为前缀,
// 但 "/ab" 不是 "/a" 的前缀 —— 段边界必须对齐)。
func pathHasPrefix(p, prefix string) bool {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return false
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// shareIdentityOf 从快照自身推导某个身份字段:取其第一个节点的 Ext 值。
// 快照是空壳(坏行)时返回空串 —— 此时只能靠分享 URL 侧的判据。
func shareIdentityOf(sh *repo.Share, field string) string {
	if sh == nil || !repo.RawTreeHasContent(sh.RawTree) {
		return ""
	}
	var nodes []driver.FileNode
	if json.Unmarshal([]byte(sh.RawTree), &nodes) != nil {
		return ""
	}
	for i := range nodes {
		if v := nodes[i].Ext[field]; v != "" {
			return v
		}
	}
	return ""
}

// hostPathOf 取分享 URL 的 host+path(去掉 query、fragment 与尾部斜杠)。
// quark 的 "/s/xxx" 与 "/s/xxx#/list/share/yyy" 的 path 都是 "/s/xxx",
// 故 host+path 相同即可判定为同一份分享的不同 URL 形态。
func hostPathOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimSuffix(u.Path, "/")
	if p == "" {
		return ""
	}
	return strings.ToLower(u.Host) + p
}

// shareIDOf 从分享 URL 的路径里取网盘自己的分享 id
// (quark 的 "/s/AbCdEf123456" → "AbCdEf123456",蓝奏云的 "/iNrjO4afcy0b" → "iNrjO4afcy0b"),
// 取不到返回空串。
//
// ★ 这是最可靠的「同一份分享」判据:分享 id 直接嵌在路径里,
//
//	与 URL 形态(根链/深链)无关 —— 同一份分享的两种形态拿到同一个 id。
func shareIDOf(raw string) string {
	p := sharePathOf(raw)
	if p == "" {
		return ""
	}
	seg := strings.Split(strings.Trim(p, "/"), "/")
	last := seg[len(seg)-1]
	if last == "" {
		return ""
	}
	// id 形态:字母数字组合且至少含一个数字,长度 >= 6 —— 排除 "share"/"list"/
	// "out" 这类纯路径词,避免把目录名误当分享 id。
	if len(last) >= 6 && isShareIDLike(last) {
		return last
	}
	return ""
}

// sharePathOf 取分享 URL 的路径部分(去掉 host、query、fragment 与尾部斜杠)。
func sharePathOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u.Path, "/")
}

// isShareIDLike 判定末段是否像网盘分享 id(纯字母数字,且至少含一个数字)。
func isShareIDLike(s string) bool {
	hasDigit := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return hasDigit
}

// healShareContext 是 healShareNode 的上下文-only 包装。
func (r *Resolver) healShareContext(key, fid string) (map[string]string, error) {
	n, err := r.healShareNode(key, fid)
	if err != nil || n == nil {
		return nil, err
	}
	return n.Ext, nil
}

// recoverExtFromHistory 从同 fid 的历史缓存行里恢复 driver 上下文(含真实 path)。
//
// ★ 必须「挑一条**有 path** 的行」,而不是简单取最新一行:同一 fid 的多行里
// 可能混有「有列无内容」的坏行 —— 线上历史数据中确有 ext 被写成字面量 "null"
// 的形态(json.Marshal(nil map) 的产物)。这类行 Unmarshal 后是 nil map,
// 取到它就等于没恢复 → 裸哈希提链必然 404,而库里明明存在带 path 的可用历史行。
// 故按新→旧遍历,取第一条能解出非空 path 的行;全都没有才算没历史。
func (r *Resolver) recoverExtFromHistory(fid string) (map[string]string, error) {
	ext, _, err := r.recoverHistory(fid)
	return ext, err
}

// recoverHistory 一次扫描同时取回「可用历史行」与「它的 ext」。
// 只扫一遍历史表:两个调用方(buildRef 要 ext、fileMeta 要 ext+名字/大小)
// 关心的始终是同一行。
//
// 候选口径:优先取「带非空 path」的最新行(那是路径恢复源);
// 若所有行都无 path,退而取最新的**有文件名**的行 —— 仅补展示元数据,
// 不给 driver 供路径(没有 path 的行对恢复毫无价值)。
func (r *Resolver) recoverHistory(fid string) (map[string]string, *repo.Link, error) {
	hist, err := r.store.ListLinksByFID(fid, historyScanLimit)
	if err != nil {
		return nil, nil, err
	}
	var named *repo.Link
	for i := range hist {
		if hist[i].Ext == "" {
			continue
		}
		var ext map[string]string
		if json.Unmarshal([]byte(hist[i].Ext), &ext) != nil {
			continue
		}
		if ext["path"] == "" {
			if named == nil && !linkMetaIsPlaceholder(hist[i].FileName, hist[i].Size) {
				named = &hist[i]
			}
			continue
		}
		return ext, &hist[i], nil
	}
	return nil, named, nil
}

// fileMeta 取文件展示元数据:先查直链缓存行,再查分享快照;都没有返回空 name 交由调用方兜底。
//
// ★ 直链缓存行可能是「占位元数据」(兜底名「文件」+0)—— 某次解析在缓存/快照/
//   历史都查不到时落的兜底值。读侧必须跳过它继续找,否则快照/历史里的真实
//   名字/大小会被占位行**永久遮蔽**(与 v2.9 ext、v2.10 raw_tree 同型的
//   「有列无内容」问题,只是载体换成 file_name/size)。
func (r *Resolver) fileMeta(key, fid string, ref *driver.FileRef) (string, int64, map[string]string, error) {
	l, err := r.store.GetLink(key, fid)
	if err != nil {
		return "", 0, nil, fmt.Errorf("query link cache: %w", err)
	}
	if l != nil && !linkMetaIsPlaceholder(l.FileName, l.Size) {
		var ext map[string]string
		_ = json.Unmarshal([]byte(l.Ext), &ext)
		return l.FileName, l.Size, ext, nil
	}
	sh, err := r.store.GetShare(key)
	if err != nil {
		return "", 0, nil, fmt.Errorf("query share snapshot: %w", err)
	}
	if sh != nil {
		var nodes []driver.FileNode
		if json.Unmarshal([]byte(sh.RawTree), &nodes) == nil {
			for _, n := range nodes {
				if n.FID == fid {
					return n.Name, n.Size, n.Ext, nil
				}
			}
		}
	}
	// ★ 坏快照自愈(与 buildRef 同源):本分享快照是空壳时,展示元数据
	//   (名字/大小)同样拿不到 → 只能回落到兜底名「文件」+0,用户看到的是
	//   「一个叫文件的 0 字节文件」。从同一网盘别的 share_key 快照里找回
	//   该 fid 的节点即可(见 healShareNode)。
	if healed, herr := r.healShareNode(key, fid); herr == nil && healed != nil {
		return healed.Name, healed.Size, healed.Ext, nil
	}
	// 哈希 fid 在本分享查不到名字时,同 fid 的历史缓存行有完整元数据。
	// 残缺行(FileName 空)不算命中,落到调用方的 driverStat 兜底。
	if isHashFid(fid) {
		ext, hist, herr := r.recoverHistory(fid)
		if herr != nil {
			return "", 0, nil, fmt.Errorf("query link history: %w", herr)
		}
		// 名字/大小同样取「内容完整」的那条历史行:坏行(ext=null)的 FileName
		// 常常也是兜底出来的「文件」+0,拿它当展示元数据不如不拿。
		if hist != nil && !linkMetaIsPlaceholder(hist.FileName, hist.Size) {
			return hist.FileName, hist.Size, ext, nil
		}
	}
	return "", 0, ref.Ext, nil
}

// placeholderFileName / linkMetaIsPlaceholder:占位元数据的判定口径单一来源在
// repo.LinkMetaIsPlaceholder(写侧覆盖保护与读侧遮蔽跳过必须同口径),这里只
// 做包内转发,防止两处口径漂移。
const placeholderFileName = repo.PlaceholderFileName

func linkMetaIsPlaceholder(name string, size int64) bool {
	return repo.LinkMetaIsPlaceholder(name, size)
}

// historyScanLimit 历史反查时扫描同 fid 历史行的上限。
// 行数 = 曾缓存过该 fid 的分享上下文数(share_key 各不相同),量级很小;
// 上限要盖得住「前面若干行都是坏行(无有效 path)、可用行在更早」的情形,
// 故取宽松值。扫描按 idx_fid_created 索引序进行,本身廉价。
const historyScanLimit = 64

// healScanLimit 快照自愈时扫描同 pan_type 分享快照的上限(从最新往回数)。
// 自愈找的是「同一份分享的另一条快照」(同一 URL 的另一种形态),这类兄弟
// 快照几乎必然在最近几条里;上限封顶后空壳 miss 的最坏成本是常数次 JSON
// 解析,不随 shares 总量增长。取值盖住生产量级(个位数 shares)的数百倍冗余。
const healScanLimit = 64

// isHashFid 哈希形态 fid 判定(mangodrive 对超长路径生成 "p:<sha1>",不可逆)。
// 仅哈希形态启用历史反查:path 形态 fid 未命中由 driverStat 兜底,省一次全局扫;
// 同时保证其它驱动形态的 fid 不会撞名进 mangodrive 的历史恢复路径。
func isHashFid(fid string) bool {
	return strings.HasPrefix(fid, mangodrive.FidHashPrefix)
}

// driverStat 回问 driver:把 rawURL 当作文件路径解析,命中 fid 时取名字与大小。
// 只用于「缓存与分享快照都查不到」的场景(driver 的 FID 自带定位信息,如 mangodrive 的路径)。
// 失败不致命:名字/大小只是展示信息,不该让一次可用的提链整体失败。
func (r *Resolver) driverStat(ctx context.Context, drv driver.Driver, rawURL, fid string,
	cred *driver.Credential) (string, int64) {
	var nodes []driver.FileNode
	err := r.guard(ctx, drv.ID(), func() error {
		var gerr error
		nodes, gerr = drv.ResolveShare(ctx, driver.ShareLink{URL: rawURL}, cred)
		return gerr
	})
	if err != nil {
		return "", 0
	}
	for _, n := range nodes {
		if n.FID == fid && !n.IsDir && n.Name != "" {
			return n.Name, n.Size
		}
	}
	return "", 0
}

// buildResult 由 repo.Link 派生下载路径与签名链接。
func (r *Resolver) buildResult(pan, key string, l *repo.Link, clientUA string, cacheHit bool) *ResolveFileResult {
	cookie, _ := r.aes.DecryptBytes(l.CookieEnc)
	meta := LinkMeta{UA: l.UA, Referer: l.Referer, Cookie: string(cookie), BindIP: l.BindIP}
	cfg := r.cfg.Get()
	env := RouteEnv{Profile: cfg.Server.DeployProfile, ClientUA: clientUA, Aria2SameHost: cfg.Aria2.SameHost}
	route := Route(meta, env)
	// 下载入口签名统一使用配置 sign_ttl,与上游直链剩余有效期(ExpiresAt)解耦:
	// 短 TTL 直链过期后签名仍有效,GetFreshLink 可在入口有效期内续命(P2-5)。
	sig := r.signer.Sign(pan+"|"+key+"|"+l.FID, cfg.Server.EffectiveSignTTL())
	base := strings.TrimSuffix(cfg.Server.BaseURL, "/")
	res := &ResolveFileResult{
		Pan: pan, ShareKey: key, FID: l.FID, FileName: l.FileName, Size: l.Size,
		Route:       route,
		DownloadURL: EntryURL(base, "d", pan, key, l.FID, sig),
		StreamURL:   EntryURL(base, "stream", pan, key, l.FID, sig),
		DirectURL:   l.DirectLink,
		UA:          l.UA,
		Referer:     l.Referer,
		NeedHeaders: NeedHeaders(l.UA, l.Referer, string(cookie), clientUA),
		ExpiresAt:   l.ExpiresAt,
		CacheHit:    cacheHit,
	}
	// 路由计数在"实际服务决策点"记录:/d(302 或降级)与 aria2 推送,而非解析时的预测
	return res
}

// refreshBudget 是共享刷新任务的独立期限:不跟随任何一个 HTTP 请求的生命周期,
// 但必须有界,避免异常 driver 卡死后续所有等待者。
const refreshBudget = 120 * time.Second

// GetFreshLink 返回未过期直链;过期/缺失时经 singleflight 收敛为一次真实刷新。
// 共享任务使用独立有界 ctx(不绑定首个调用者),首调取消不拖垮等待者(P2-12);
// 每个等待者仍会响应自身 ctx 的取消。
func (r *Resolver) GetFreshLink(ctx context.Context, shareKey, fid, clientUA string) (*repo.Link, error) {
	l, err := r.store.GetLink(shareKey, fid)
	if err != nil {
		return nil, fmt.Errorf("query link cache: %w", err)
	}
	if l != nil && l.ExpiresAt.After(time.Now()) {
		return l, nil
	}
	v, err, _ := r.sf.Do("link:"+shareKey+"|"+fid, func() (any, error) {
		existing, err := r.store.GetLink(shareKey, fid)
		if err != nil {
			return nil, fmt.Errorf("query link cache: %w", err)
		}
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshBudget)
		defer cancel()
		if existing != nil {
			refreshCtx = withPick(refreshCtx)
		}
		sh, err := r.store.GetShare(shareKey)
		if err != nil {
			return nil, fmt.Errorf("query share snapshot: %w", err)
		}
		if sh == nil {
			return nil, fmt.Errorf("share %s not found, cannot refresh link", shareKey)
		}
		if _, err := r.ResolveFile(refreshCtx, sh.ShareURL, sh.Pwd, fid, false, clientUA); err != nil {
			return nil, err
		}
		fresh, err := r.store.GetLink(shareKey, fid)
		if err != nil {
			return nil, fmt.Errorf("query link cache: %w", err)
		}
		return fresh, nil
	})
	if err != nil {
		return nil, err
	}
	// 等待者在其自身 ctx 已取消时立即失败,不再消费共享结果。
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	l, ok := v.(*repo.Link)
	if !ok || l == nil {
		// ResolveFile 落库失败时仅告警,此处兜底为明确错误,避免 nil 解引用 panic。
		return nil, fmt.Errorf("link %s/%s 刷新后未落库", shareKey, fid)
	}
	return l, nil
}
