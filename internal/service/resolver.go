// Resolver:解析编排(设计文档 §4.4)——识别 → 缓存 → driver → 决策 → 落库。
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
	// 单网盘在途并发闸(设计文档 §7.7,capacity 来自 driver 配置 download_concurrency)
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

// acquire 获取单网盘在途并发额度(设计文档 §7.7)。
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

// autoLinkMax 一次分享解析自动提链的文件数上限,避免夸克转存链把请求拖死或触发风控。
const autoLinkMax = 8

type FileItem struct {
	FID         string     `json:"fid"`
	Name        string     `json:"name"`
	Size        int64      `json:"size"`
	IsDir       bool       `json:"is_dir"`
	Route       string     `json:"route,omitempty"`
	DownloadURL string     `json:"download_url,omitempty"`
	StreamURL   string     `json:"stream_url,omitempty"`
	DirectURL   string     `json:"direct_url,omitempty"`
	UA          string     `json:"ua,omitempty"`
	Referer     string     `json:"referer,omitempty"`
	NeedHeaders bool       `json:"need_headers,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CacheHit    bool       `json:"cache_hit,omitempty"`
	LinkError   string     `json:"link_error,omitempty"`
}

type ResolveShareResult struct {
	Pan      string     `json:"pan"`
	ShareKey string     `json:"share_key"`
	Files    []FileItem `json:"files"`
	Hint     string     `json:"hint,omitempty"`
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

// guard 统一执行 熔断检查 → 本地限频 → 调用 → 熔断记录。
func (r *Resolver) guard(pan string, fn func() error) error {
	b := r.breakers.Get(pan)
	if !b.Allow() {
		r.met.Inc("panrouter_driver_error_total", map[string]string{"pan": pan, "kind": "breaker_open"})
		return driver.NewErr(driver.KindRiskControl, "该网盘熔断中(近期风控/异常过多),请稍后再试", nil)
	}
	if !r.limiter.Allow(pan, r.driverCfg(pan).LimitQPS) {
		return driver.NewErr(driver.KindRiskControl, "本地限频:请求过于频繁,请稍后", nil)
	}
	err := fn()
	fail := false
	var de *driver.Error
	if err != nil && errors.As(err, &de) {
		fail = de.Kind == driver.KindRiskControl || de.Kind == driver.KindUpstream
		r.met.Inc("panrouter_driver_error_total", map[string]string{"pan": pan, "kind": string(de.Kind)})
		if de.Kind == driver.KindInterfaceChanged {
			// 接口改版是高优告警:不熔断、不重试,直接通知维护者(设计文档 §4.2 禁手)
			r.log.Errorf("[ALARM] driver interface changed: pan=%s err=%s", pan, de.Error())
		}
	}
	b.Record(!fail, r.breakers.MaxFails(), r.breakers.Cooldown())
	r.met.Set("panrouter_breaker_open", map[string]string{"pan": pan}, boolGauge(b.Opened()))
	return err
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// mapErr 处理 driver 错误的副作用(账号标记等),原样返回给上层映射 HTTP 状态。
func (r *Resolver) mapErr(pan string, acc *repo.Account, err error) error {
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
		} else {
			// 无账号时的提示要指向"添加账号"而不是"更新 Cookie"
			de.UserHint = "该网盘需要登录态,请先在「账号管理」添加账号;" + de.UserHint
		}
	case driver.KindRiskControl:
		if acc != nil {
			r.accounts.MarkCooldown(acc.ID, 5*time.Minute)
		}
	}
	return err
}

func (r *Resolver) pick(pan string) (*repo.Account, *driver.Credential, error) {
	acc, cred, err := r.accounts.Pick(pan)
	if err != nil {
		return nil, nil, driver.NewErr(driver.KindUpstream, "读取账号凭据失败", err)
	}
	return acc, cred, nil
}

// ---- 对外方法 ----

func (r *Resolver) ResolveShare(ctx context.Context, rawURL, pwd, clientUA string) (*ResolveShareResult, error) {
	drv, err := r.reg.Detect(rawURL)
	if err != nil {
		return nil, err
	}
	pan := drv.ID()
	acc, cred, err := r.pick(pan)
	if err != nil {
		return nil, err
	}
	var nodes []driver.FileNode
	err = r.guard(pan, func() error {
		nodes, err = drv.ResolveShare(ctx, driver.ShareLink{URL: rawURL, Pwd: pwd}, cred)
		return err
	})
	if err := r.mapErr(pan, acc, err); err != nil {
		return nil, err
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
	linked, authBlocked := 0, false
	hint := ""
	for _, n := range nodes {
		it := FileItem{FID: n.FID, Name: n.Name, Size: n.Size, IsDir: n.IsDir}
		if !n.IsDir && n.FID != "" && linked < autoLinkMax && !authBlocked {
			linked++
			file, lerr := r.ResolveFile(ctx, rawURL, pwd, n.FID, false, clientUA)
			if lerr != nil {
				var de *driver.Error
				if errors.As(lerr, &de) && de.Kind == driver.KindAuthExpired {
					authBlocked = true
					hint = de.UserHint
					it.LinkError = de.UserHint
				} else {
					it.LinkError = lerr.Error()
				}
			} else {
				exp := file.ExpiresAt
				it.Route = file.Route
				it.DownloadURL = file.DownloadURL
				it.StreamURL = file.StreamURL
				it.DirectURL = file.DirectURL
				it.UA = file.UA
				it.Referer = file.Referer
				it.NeedHeaders = file.NeedHeaders
				it.ExpiresAt = &exp
				it.CacheHit = file.CacheHit
			}
		}
		items = append(items, it)
	}
	r.met.Inc("panrouter_resolve_total", map[string]string{"pan": pan, "kind": "share"})
	return &ResolveShareResult{Pan: pan, ShareKey: key, Files: items, Hint: hint}, nil
}

// ResolveFile 获取单个文件直链:缓存未过期直接返回,否则走 driver 并落库。
func (r *Resolver) ResolveFile(ctx context.Context, rawURL, pwd, fid string, own bool, clientUA string) (*ResolveFileResult, error) {
	drv, err := r.reg.Detect(rawURL)
	if err != nil {
		return nil, err
	}
	pan := drv.ID()
	key := r.shareKey(pan, rawURL, pwd)

	// 缓存命中
	if l, _ := r.store.GetLink(key, fid); l != nil && l.ExpiresAt.After(time.Now()) {
		r.met.Inc("panrouter_link_cache_hit_total", map[string]string{"pan": pan})
		return r.buildResult(pan, key, l, clientUA, true), nil
	}

	acc, cred, err := r.pick(pan)
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
	err = r.guard(pan, func() error {
		dl, err = drv.GetDirectLink(ctx, cred, *ref)
		return err
	})
	release()
	if err := r.mapErr(pan, acc, err); err != nil {
		return nil, err
	}
	name, size, ext := r.fileMeta(key, fid, ref)
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
	if err := r.store.UpsertLink(l); err != nil {
		r.log.Warnf("save link: %v", err)
	}
	r.met.Inc("panrouter_resolve_total", map[string]string{"pan": pan, "kind": "file"})
	return r.buildResult(pan, key, l, clientUA, false), nil
}

// buildRef 组装 FileRef:优先用已缓存 link 的 ext,其次从分享快照的节点里取。
func (r *Resolver) buildRef(key, fid string, own bool) (*driver.FileRef, error) {
	ref := &driver.FileRef{Own: own, FID: fid, ShareKey: key}
	if own {
		return ref, nil
	}
	if l, _ := r.store.GetLink(key, fid); l != nil && l.Ext != "" {
		_ = json.Unmarshal([]byte(l.Ext), &ref.Ext)
	}
	if len(ref.Ext) == 0 {
		var sh repo.Share
		if err := r.store.DB().Where("share_key = ?", key).First(&sh).Error; err == nil {
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
	}
	return ref, nil
}

func (r *Resolver) fileMeta(key, fid string, ref *driver.FileRef) (string, int64, map[string]string) {
	if l, _ := r.store.GetLink(key, fid); l != nil && l.FileName != "" {
		var ext map[string]string
		_ = json.Unmarshal([]byte(l.Ext), &ext)
		return l.FileName, l.Size, ext
	}
	var sh repo.Share
	if err := r.store.DB().Where("share_key = ?", key).First(&sh).Error; err == nil {
		var nodes []driver.FileNode
		if json.Unmarshal([]byte(sh.RawTree), &nodes) == nil {
			for _, n := range nodes {
				if n.FID == fid {
					return n.Name, n.Size, n.Ext
				}
			}
		}
	}
	return "文件", 0, ref.Ext
}

// buildResult 由 repo.Link 派生下载路径与签名链接。
func (r *Resolver) buildResult(pan, key string, l *repo.Link, clientUA string, cacheHit bool) *ResolveFileResult {
	cookie, _ := r.aes.DecryptBytes(l.CookieEnc)
	meta := LinkMeta{UA: l.UA, Referer: l.Referer, Cookie: string(cookie), BindIP: l.BindIP}
	cfg := r.cfg.Get()
	env := RouteEnv{Profile: cfg.Server.DeployProfile, ClientUA: clientUA, Aria2SameHost: cfg.Aria2.SameHost}
	route := Route(meta, env)
	ttl := time.Until(l.ExpiresAt)
	if ttl <= 0 {
		ttl = time.Minute
	}
	sig := r.signer.Sign(pan+"|"+key+"|"+l.FID, ttl)
	base := strings.TrimSuffix(cfg.Server.BaseURL, "/")
	res := &ResolveFileResult{
		Pan: pan, ShareKey: key, FID: l.FID, FileName: l.FileName, Size: l.Size,
		Route:       route,
		DownloadURL: fmt.Sprintf("%s/d/%s/%s/%s?sig=%s", base, pan, key, l.FID, sig),
		StreamURL:   fmt.Sprintf("%s/stream/%s/%s/%s?sig=%s", base, pan, key, l.FID, sig),
		DirectURL:   l.DirectLink,
		UA:          l.UA,
		Referer:     l.Referer,
		NeedHeaders: len(cookie) > 0 || l.Referer != "" || (l.UA != "" && !strings.EqualFold(l.UA, clientUA)),
		ExpiresAt:   l.ExpiresAt,
		CacheHit:    cacheHit,
	}
	// 路由计数在"实际服务决策点"记录:/d(302 或降级)与 aria2 推送,而非解析时的预测
	return res
}
func (r *Resolver) GetFreshLink(ctx context.Context, shareKey, fid, clientUA string) (*repo.Link, error) {
	if l, _ := r.store.GetLink(shareKey, fid); l != nil && l.ExpiresAt.After(time.Now()) {
		return l, nil
	}
	// singleflight:并发请求同一过期链接收敛为一次真实刷新(其余等待复用结果)
	v, err, _ := r.sf.Do("link:"+shareKey+"|"+fid, func() (any, error) {
		// double-check:等待期间可能已被前一个调用刷新
		if l, _ := r.store.GetLink(shareKey, fid); l != nil && l.ExpiresAt.After(time.Now()) {
			return l, nil
		}
		// 需要原始分享链接才能重解析
		var sh repo.Share
		if err := r.store.DB().Where("share_key = ?", shareKey).First(&sh).Error; err != nil {
			return nil, fmt.Errorf("share %s not found, cannot refresh link", shareKey)
		}
		if _, err := r.ResolveFile(ctx, sh.ShareURL, sh.Pwd, fid, false, clientUA); err != nil {
			return nil, err
		}
		return r.store.GetLink(shareKey, fid)
	})
	if err != nil {
		return nil, err
	}
	l, ok := v.(*repo.Link)
	if !ok || l == nil {
		// ResolveFile 落库失败时仅告警,此处兜底为明确错误,避免 nil 解引用 panic。
		return nil, fmt.Errorf("link %s/%s 刷新后未落库", shareKey, fid)
	}
	return l, nil
}
