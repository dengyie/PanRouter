package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/service"
)

// verifySig 校验 /d、/stream 的 HMAC 签名(签名 payload 与签发端一致)。
func (d *Deps) verifySig(w http.ResponseWriter, r *http.Request) (pan, key, fid string, ok bool) {
	pan = chi.URLParam(r, "pan")
	key = chi.URLParam(r, "key")
	fid = chi.URLParam(r, "fid")
	if err := d.Signer.Verify(pan+"|"+key+"|"+fid, r.URL.Query().Get("sig")); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "签名无效或已过期,请重新解析", err))
		return "", "", "", false
	}
	return pan, key, fid, true
}

// handleDownload302 GET /d/{pan}/{key}/{fid}:302 直链;Can302 不满足时无感降级到 /stream。
func (d *Deps) handleDownload302(w http.ResponseWriter, r *http.Request) {
	pan, key, fid, ok := d.verifySig(w, r)
	if !ok {
		return
	}
	ua := r.Header.Get("User-Agent")
	link, err := d.Resolver.GetFreshLink(r.Context(), key, fid, ua)
	if err != nil {
		writeErr(w, err)
		return
	}
	cookie, _ := d.AES.DecryptBytes(link.CookieEnc)
	cfg := d.Cfg.Get()
	meta := service.LinkMeta{UA: link.UA, Referer: link.Referer, Cookie: string(cookie), BindIP: link.BindIP}
	env := service.RouteEnv{Profile: cfg.Server.DeployProfile, ClientUA: ua, Aria2SameHost: cfg.Aria2.SameHost}
	if !service.Can302(meta, env) {
		// 浏览器无法满足直链约束 → 降级中转,用户无感
		sig := d.Signer.Sign(pan+"|"+key+"|"+fid, cfg.Server.EffectiveSignTTL())
		d.Met.Inc("panrouter_download_route_total", map[string]string{"pan": pan, "route": "stream"})
		http.Redirect(w, r, fmt.Sprintf("/stream/%s/%s/%s?sig=%s", pan, key, fid, sig), http.StatusFound)
		return
	}
	d.Met.Inc("panrouter_download_route_total", map[string]string{"pan": pan, "route": "302"})
	http.Redirect(w, r, link.DirectLink, http.StatusFound)
}

// handleStream GET /stream/{pan}/{key}/{fid}:服务端中转流。
func (d *Deps) handleStream(w http.ResponseWriter, r *http.Request) {
	pan, key, fid, ok := d.verifySig(w, r)
	if !ok {
		return
	}
	err := d.Relay.Serve(r.Context(), w, service.StreamInput{
		Pan: pan, ShareKey: key, FID: fid, ClientUA: r.Header.Get("User-Agent"),
	}, r.Header.Get("Range"))
	if err != nil {
		writeErr(w, err)
	}
}

// handleDirectJSON GET /api/v1/json/{pan}/{key}/{fid}:脚本用的 JSON 版直链(需登录态)。
func (d *Deps) handleDirectJSON(w http.ResponseWriter, r *http.Request) {
	pan, key, fid, ok := d.verifySig(w, r)
	if !ok {
		return
	}
	ua := r.Header.Get("User-Agent")
	link, err := d.Resolver.GetFreshLink(r.Context(), key, fid, ua)
	if err != nil {
		writeErr(w, err)
		return
	}
	cookie, _ := d.AES.DecryptBytes(link.CookieEnc)
	cfg := d.Cfg.Get()
	meta := service.LinkMeta{UA: link.UA, Referer: link.Referer, Cookie: string(cookie), BindIP: link.BindIP}
	env := service.RouteEnv{Profile: cfg.Server.DeployProfile, ClientUA: ua, Aria2SameHost: cfg.Aria2.SameHost}
	sig := d.Signer.Sign(pan+"|"+key+"|"+fid, cfg.Server.EffectiveSignTTL())
	base := cfg.Server.BaseURL
	writeJSON(w, http.StatusOK, map[string]any{
		"pan":          pan,
		"share_key":    key,
		"fid":          fid,
		"file_name":    link.FileName,
		"size":         link.Size,
		"route":        service.Route(meta, env),
		"direct_link":  link.DirectLink,
		"download_url": fmt.Sprintf("%s/d/%s/%s/%s?sig=%s", base, pan, key, fid, sig),
		"stream_url":   fmt.Sprintf("%s/stream/%s/%s/%s?sig=%s", base, pan, key, fid, sig),
		"ua":           link.UA,
		"referer":      link.Referer,
		"need_headers": service.NeedHeaders(link.UA, link.Referer, string(cookie), ua),
		"expires_at":   link.ExpiresAt,
	})
}
