package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
	"github.com/dengyie/panrouter/internal/service"
)

// ---- 解析 ----

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
	ua := clientUA(r, q.UA)
	if q.FID == "" {
		return d.Resolver.ResolveShare(r.Context(), q.URL, q.Pwd)
	}
	return d.Resolver.ResolveFile(r.Context(), q.URL, q.Pwd, q.FID, false, ua)
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
	if len(q.Items) == 0 || len(q.Items) > 50 {
		writeErr(w, driver.NewErr(driver.KindNotFound, "items 数量须在 1-50", nil))
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
	if _, ok := d.Resolver.Registry().Get(q.PanType); !ok {
		writeErr(w, driver.NewErr(driver.KindNotFound, "不支持的网盘类型:"+q.PanType, nil))
		return
	}
	if strings.TrimSpace(q.Cookie) == "" {
		writeErr(w, driver.NewErr(driver.KindNotFound, "cookie 不能为空", nil))
		return
	}
	enc, err := d.Accounts.EncryptCred(&driver.Credential{Cookie: q.Cookie})
	if err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "凭据加密失败", err))
		return
	}
	acc := &repo.Account{PanType: q.PanType, Name: q.Name, CredEnc: enc, Status: "ok", CredVersion: 1}
	if err := d.Store.CreateAccount(acc); err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "保存账号失败", err))
		return
	}
	d.Store.AddAudit("account_create", "pan="+q.PanType+" id="+strconv.FormatUint(uint64(acc.ID), 10))
	writeJSON(w, http.StatusOK, map[string]any{"account": view(acc)})
}

func (d *Deps) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err := d.Store.DeleteAccount(uint(id)); err != nil {
		writeErr(w, driver.NewErr(driver.KindUpstream, "删除失败", err))
		return
	}
	d.Store.AddAudit("account_delete", "id="+strconv.FormatUint(id, 10))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (d *Deps) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	acc, err := d.Store.GetAccount(uint(id))
	if err != nil || acc == nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "账号不存在", nil))
		return
	}
	drv, ok := d.Resolver.Registry().Get(acc.PanType)
	if !ok {
		writeErr(w, driver.NewErr(driver.KindNotFound, "网盘 driver 未启用", nil))
		return
	}
	st, err := d.Accounts.Check(r.Context(), acc.ID, drv)
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
	sqlDB, err := d.Store.DB().DB()
	if err != nil || sqlDB.Ping() != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "db_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (d *Deps) handleIndex(w http.ResponseWriter, _ *http.Request) {
	if d.WebFS == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "web not embedded"})
		return
	}
	data, err := fs.ReadFile(d.WebFS, "index.html")
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "index.html missing"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
