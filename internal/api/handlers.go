package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/skip2/go-qrcode"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/driver/quark"
	"github.com/dengyie/panrouter/internal/repo"
	"github.com/dengyie/panrouter/internal/service"
)

// ---- 解析 ----

// 批量解析预算契约(§15.3):单项预算与单请求上限。
// 超限返回 400(unsupported 语义),部分成功——单项失败不中断批,逐项回填 error。
const (
	ResolvePerItemTimeout = 90 * time.Second
	ResolveBatchMaxItems  = 50
)

type resolveReq struct {
	URL string `json:"url"`
	Pwd string `json:"pwd"`
	FID string `json:"fid"`
	UA  string `json:"ua"`
	// own 模式(网盘内文件提链)需先有 /files 端点提供 fid,M2 引入;
	// driver 层 own 分支已就绪,公开 DTO 暂不暴露。
}

func clientUA(r *http.Request, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return r.Header.Get("User-Agent")
}

func (d *Deps) doResolve(r *http.Request, q resolveReq) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), ResolvePerItemTimeout)
	defer cancel()
	ua := clientUA(r, q.UA)
	if q.FID == "" {
		return d.Resolver.ResolveShare(ctx, q.URL, q.Pwd, ua)
	}
	return d.Resolver.ResolveFile(ctx, q.URL, q.Pwd, q.FID, false, ua)
}

// handleResolve POST /api/v1/resolve
func (d *Deps) handleResolve(w http.ResponseWriter, r *http.Request) {
	var q resolveReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		return
	}
	if q.URL == "" {
		writeErr(w, driver.NewErr(driver.KindNotFound, "url 不能为空", nil))
		return
	}
	res, err := d.doResolve(r, q)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleResolveBatch POST /api/v1/resolve/batch(文件夹批量提链;串行经 limiter)
type batchReq struct {
	Items []resolveReq `json:"items"`
}

type batchItem struct {
	Index  int    `json:"index"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (d *Deps) handleResolveBatch(w http.ResponseWriter, r *http.Request) {
	var q batchReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		return
	}
	if len(q.Items) == 0 || len(q.Items) > ResolveBatchMaxItems {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "items 数量须在 1-"+strconv.Itoa(ResolveBatchMaxItems), nil))
		return
	}
	out := make([]batchItem, 0, len(q.Items))
	for i, item := range q.Items {
		res, err := d.doResolve(r, item)
		if err != nil {
			out = append(out, batchItem{Index: i, OK: false, Error: err.Error()})
			continue
		}
		out = append(out, batchItem{Index: i, OK: true, Result: res})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// ---- 账号 ----

type accountReq struct {
	PanType string `json:"pan_type"`
	Name    string `json:"name"`
	Cookie  string `json:"cookie"`
}

type accountView struct {
	ID            uint       `json:"id"`
	PanType       string     `json:"pan_type"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	CredVersion   int        `json:"cred_version"`
	CooldownUntil *time.Time `json:"cooldown_until"`
	LastCheckAt   *time.Time `json:"last_check_at"`
}

func view(a *repo.Account) accountView {
	return accountView{ID: a.ID, PanType: a.PanType, Name: a.Name, Status: a.Status,
		CredVersion: a.CredVersion, CooldownUntil: a.CooldownUntil, LastCheckAt: a.LastCheckAt}
}

func (d *Deps) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := d.Store.ListAccounts(r.URL.Query().Get("pan"))
	if err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "查询账号失败", err))
		return
	}
	out := make([]accountView, 0, len(list))
	for i := range list {
		out = append(out, view(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func (d *Deps) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var q accountReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		return
	}
	acc, err := d.Accounts.Create(q.PanType, q.Name, q.Cookie, func(pan string) bool {
		_, ok := d.Resolver.Registry().Get(pan)
		return ok
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": view(acc)})
}

// ---- 扫码登录(夸克) ----

// qrDriver 取夸克 driver 的扫码能力;未启用时返回 nil。
func (d *Deps) qrDriver() *quark.Driver {
	drv, ok := d.Resolver.Registry().Get("quark")
	if !ok {
		return nil
	}
	qd, ok := drv.(*quark.Driver)
	if !ok {
		return nil
	}
	return qd
}

// handleQRToken POST /api/v1/accounts/quark/qr:生成扫码二维码(一次性 token,前端渲染)。
func (d *Deps) handleQRToken(w http.ResponseWriter, r *http.Request) {
	qd := d.qrDriver()
	if qd == nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "夸克 driver 未启用", nil))
		return
	}
	qr, err := qd.QRToken(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	// png 直出 data URI,前端 <img src> 零依赖渲染;生成失败降级为省略字段
	// (前端回退展示链接),不因表现层失败拒掉扫码会话。
	resp := map[string]any{"token": qr.Token, "url": qr.URL}
	if png, perr := qrPNG(qr.URL); perr == nil {
		resp["png"] = png
	}
	writeJSON(w, http.StatusOK, resp)
}

// qrPNG 把扫码 URL 渲染成 data URI PNG。纯本地计算,无外部 I/O。
func qrPNG(u string) (string, error) {
	raw, err := qrcode.Encode(u, qrcode.Medium, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), nil
}

type qrPollReq struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

// handleQRPoll POST /api/v1/accounts/quark/qr/poll:轮询扫码状态。
// pending 直接返回;confirmed 即以换好的 Cookie 建号入库(免手动贴 Cookie 的核心闭环)。
func (d *Deps) handleQRPoll(w http.ResponseWriter, r *http.Request) {
	qd := d.qrDriver()
	if qd == nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "夸克 driver 未启用", nil))
		return
	}
	var q qrPollReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil || q.Token == "" {
		writeErr(w, driver.NewErr(driver.KindNotFound, "token 必填", nil))
		return
	}
	cookie, state, err := qd.QRPoll(r.Context(), q.Token)
	if err != nil {
		writeErr(w, err)
		return
	}
	if state != "confirmed" {
		writeJSON(w, http.StatusOK, map[string]any{"status": state})
		return
	}
	acc, err := d.Accounts.Create("quark", q.Name, cookie, func(pan string) bool {
		_, ok := d.Resolver.Registry().Get(pan)
		return ok
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "confirmed", "account": view(acc)})
}

func (d *Deps) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err := d.Accounts.Delete(uint(id)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (d *Deps) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	st, err := d.Accounts.Refresh(r.Context(), uint(id), func(pan string) (driver.Driver, bool) {
		return d.Resolver.Registry().Get(pan)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": st})
}

// ---- 下载 ----

type pushReq struct {
	Pan      string `json:"pan"`
	ShareKey string `json:"share_key"`
	FID      string `json:"fid"`
	Dest     string `json:"dest"`
}

func (d *Deps) handlePushDownload(w http.ResponseWriter, r *http.Request) {
	var q pushReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "请求格式错误", err))
		return
	}
	if q.ShareKey == "" || q.FID == "" {
		writeErr(w, driver.NewErr(driver.KindNotFound, "share_key 与 fid 必填", nil))
		return
	}
	res, err := d.Aria2.Push(r.Context(), service.PushRequest{
		Pan: q.Pan, ShareKey: q.ShareKey, FID: q.FID, Dest: q.Dest,
		ClientUA: r.Header.Get("User-Agent"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Deps) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	res, err := d.Aria2.Status(r.Context(), chi.URLParam(r, "gid"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Deps) handleListDownloads(w http.ResponseWriter, _ *http.Request) {
	list, err := d.Store.ListDownloads()
	if err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "查询失败", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"downloads": list})
}

// ---- 系统 ----

func (d *Deps) handleReady(w http.ResponseWriter, _ *http.Request) {
	if err := d.Store.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "db_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
