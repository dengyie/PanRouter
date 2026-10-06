// Aria2:下载任务推送与状态查询。
// URL 由服务端按 §4.3 决策选择:CanAria2 → 直链+headers;否则 → /stream 签名 URL。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
)

type Aria2 struct {
	cfg      *config.Provider
	signer   *sign.Signer
	resolver *Resolver
	store    *repo.Store
	aes      aesCipher
	client   *httpx.Client // AllowPrivate:aria2 通常在 127.0.0.1,属可信内网
	met      *metrics.Registry
	log      *loggerType
}

func NewAria2(cfg *config.Provider, sg *sign.Signer, resolver *Resolver, store *repo.Store, aes aesCipher, met *metrics.Registry, log *loggerType) (*Aria2, error) {
	c, err := httpx.New(httpx.Options{Timeout: 20 * time.Second, AllowPrivate: true})
	if err != nil {
		return nil, err
	}
	return &Aria2{cfg: cfg, signer: sg, resolver: resolver, store: store, aes: aes, client: c, met: met, log: log}, nil
}

type PushRequest struct {
	Pan      string `json:"pan"`
	ShareKey string `json:"share_key"`
	FID      string `json:"fid"`
	Dest     string `json:"dest"`
	ClientUA string `json:"-"`
}

type PushResult struct {
	GID   string `json:"gid"`
	Route string `json:"route"`
}

// Push 推送下载任务到 aria2。
func (a *Aria2) Push(ctx context.Context, req PushRequest) (*PushResult, error) {
	if req.Pan == "" || req.ShareKey == "" || req.FID == "" {
		return nil, driver.NewErr(driver.KindNotFound, "pan/share_key/fid 必填", nil)
	}
	// pan 一致性校验:share 快照存在时比对;DB 故障显式报错,不存在交由 /stream 签名校验兜底
	sh, err := a.store.GetShare(req.ShareKey)
	if err != nil {
		return nil, fmt.Errorf("query share snapshot: %w", err)
	}
	if sh != nil && sh.PanType != req.Pan {
		return nil, driver.NewErr(driver.KindNotFound, fmt.Sprintf("pan=%s 与解析结果 %s 不一致", req.Pan, sh.PanType), nil)
	}
	link, err := a.resolver.GetFreshLink(ctx, req.ShareKey, req.FID, req.ClientUA)
	if err != nil {
		return nil, err
	}
	cookie, err := a.aes.DecryptBytes(link.CookieEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt link cookie: %w", err)
	}
	c := a.cfg.Get()
	meta := LinkMeta{UA: link.UA, Referer: link.Referer, Cookie: string(cookie), BindIP: link.BindIP}
	env := RouteEnv{Profile: c.Server.DeployProfile, ClientUA: req.ClientUA, Aria2SameHost: c.Aria2.SameHost}

	var target string
	var headers []string
	route := "aria2"
	if CanAria2(meta, env) {
		target = link.DirectLink
		if link.UA != "" {
			headers = append(headers, "User-Agent: "+link.UA)
		}
		if link.Referer != "" {
			headers = append(headers, "Referer: "+link.Referer)
		}
		if len(cookie) > 0 {
			headers = append(headers, "Cookie: "+string(cookie))
		}
	} else {
		// 绑 IP 且 aria2 不同机:走中转 URL(aria2 下载 PanRouter 的 /stream)
		sig := a.signer.Sign(req.Pan+"|"+req.ShareKey+"|"+req.FID, c.Server.EffectiveSignTTL())
		target = fmt.Sprintf("%s/stream/%s/%s/%s?sig=%s", c.Server.BaseURL, req.Pan, req.ShareKey, req.FID, sig)
		route = "stream"
	}

	opts := map[string]any{}
	if req.Dest != "" {
		opts["dir"] = req.Dest
	}
	if len(headers) > 0 {
		opts["header"] = headers
	}
	var out struct {
		Result string `json:"result"`
		Err    *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      "panrouter",
		"method":  "aria2.addUri",
		"params":  []any{"token:" + c.Aria2.Secret, []string{target}, opts},
	}
	if _, err := a.client.DoJSON(ctx, "POST", c.Aria2.Endpoint, nil, body, &out); err != nil {
		return nil, fmt.Errorf("aria2 rpc: %w", err)
	}
	if out.Err != nil {
		return nil, fmt.Errorf("aria2: %s", out.Err.Message)
	}
	rec := &repo.Download{GID: out.Result, FID: req.FID, Dest: req.Dest, Route: route, Status: "active"}
	if err := a.store.CreateDownload(rec); err != nil {
		a.log.Warnf("save download: %v", err)
	}
	a.met.Inc("panrouter_download_route_total", map[string]string{"pan": req.Pan, "route": route})
	return &PushResult{GID: out.Result, Route: route}, nil
}

type TaskStatus struct {
	GID       string `json:"gid"`
	Status    string `json:"status"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Speed     int64  `json:"speed"`
	Error     string `json:"error,omitempty"`
}

// Status 查询并回写任务状态。
func (a *Aria2) Status(ctx context.Context, gid string) (*TaskStatus, error) {
	c := a.cfg.Get()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      "panrouter",
		"method":  "aria2.tellStatus",
		"params":  []any{"token:" + c.Aria2.Secret, gid, []string{"gid", "status", "totalLength", "completedLength", "downloadSpeed", "errorMessage"}},
	}
	var out struct {
		Result *struct {
			GID       string `json:"gid"`
			Status    string `json:"status"`
			Total     string `json:"totalLength"`
			Completed string `json:"completedLength"`
			Speed     string `json:"downloadSpeed"`
			ErrorMsg  string `json:"errorMessage"`
		} `json:"result"`
		Err *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if _, err := a.client.DoJSON(ctx, "POST", c.Aria2.Endpoint, nil, body, &out); err != nil {
		return nil, fmt.Errorf("aria2 rpc: %w", err)
	}
	if out.Err != nil {
		return nil, fmt.Errorf("aria2: %s", out.Err.Message)
	}
	r := out.Result
	task := &TaskStatus{
		GID:       gid,
		Status:    r.Status,
		Total:     parseNum(r.Total),
		Completed: parseNum(r.Completed),
		Speed:     parseNum(r.Speed),
		Error:     r.ErrorMsg,
	}
	if rec, _ := a.store.GetDownload(gid); rec != nil {
		rec.Status = r.Status
		rec.ErrMsg = r.ErrorMsg
		_ = a.store.UpdateDownload(rec)
	}
	return task, nil
}

func parseNum(s string) int64 {
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}
