// Resolver:解析编排——识别 → 缓存 → driver → 决策 → 落库。
package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
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
	raw, _ := json.Marshal(nodes)
	if err := r.store.UpsertShare(&repo.Share{
		PanType: pan, ShareURL: rawURL, Pwd: pwd, ShareKey: key,
		RawTree: string(raw), ResolvedAt: time.Now(),
	}); err != nil {
		r.log.Warnf("save share: %v", err)
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
			return r.buildResult(pan, key, cached, clientUA, true), nil
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
	cookieEnc, err := r.aes.EncryptBytes([]byte(dl.Cookie))
	if err != nil {
		return nil, fmt.Errorf("encrypt cookie: %w", err)
	}
	extJSON, _ := json.Marshal(ext)
	l := &repo.Link{
		ShareKey: key, FID: fid, FileName: name, Size: size,
		DirectLink: dl.URL, UA: dl.UA, Referer: dl.Referer, CookieEnc: cookieEnc,
		BindIP: dl.BindIP, ExpiresAt: dl.ExpiresAt, Ext: string(extJSON),
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
	if len(ref.Ext) == 0 {
		sh, err := r.store.GetShare(key)
		if err != nil {
			return nil, fmt.Errorf("query share snapshot: %w", err)
		}
		if sh == nil {
			return ref, nil
		}
		var nodes []driver.FileNode
		if json.Unmarshal([]byte(sh.RawTree), &nodes) == nil {
			for _, n := range nodes {
				if n.FID == fid {
					ref.Ext = n.Ext
					break
				}
			}
		}
	}
	return ref, nil
}

func (r *Resolver) fileMeta(key, fid string, ref *driver.FileRef) (string, int64, map[string]string, error) {
	l, err := r.store.GetLink(key, fid)
	if err != nil {
		return "", 0, nil, fmt.Errorf("query link cache: %w", err)
	}
	if l != nil && l.FileName != "" {
		var ext map[string]string
		_ = json.Unmarshal([]byte(l.Ext), &ext)
		return l.FileName, l.Size, ext, nil
	}
	sh, err := r.store.GetShare(key)
	if err != nil {
		return "", 0, nil, fmt.Errorf("query share snapshot: %w", err)
	}
	if sh == nil {
		return "文件", 0, ref.Ext, nil
	}
	var nodes []driver.FileNode
	if json.Unmarshal([]byte(sh.RawTree), &nodes) == nil {
		for _, n := range nodes {
			if n.FID == fid {
				return n.Name, n.Size, n.Ext, nil
			}
		}
	}
	return "文件", 0, ref.Ext, nil
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
		DownloadURL: fmt.Sprintf("%s/d/%s/%s/%s?sig=%s", base, pan, key, l.FID, sig),
		StreamURL:   fmt.Sprintf("%s/stream/%s/%s/%s?sig=%s", base, pan, key, l.FID, sig),
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
