package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/service"
)

// POST /api/v1/transfer/mangodrive 触发一次云对云转存,202 返回任务句柄。
func (d *Deps) handleTransferMangodrive(w http.ResponseWriter, r *http.Request) {
	if d.Transfer == nil {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "转存未启用", nil))
		return
	}
	var req service.TransferRequest
	if !decodeBoundedBody(w, r, &req) {
		return
	}
	task, err := d.Transfer.Submit(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"code": 202, "task": task.Snapshot(), "poll": "/api/v1/transfer/" + task.ID,
	})
}

// GET /api/v1/transfer 任务列表。
func (d *Deps) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	if d.Transfer == nil {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "转存未启用", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "tasks": d.Transfer.List()})
}

// GET /api/v1/transfer/{id} 单任务进度。
func (d *Deps) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	if d.Transfer == nil {
		writeErr(w, driver.NewErr(driver.KindUnsupported, "转存未启用", nil))
		return
	}
	t := d.Transfer.Get(chi.URLParam(r, "id"))
	if t == nil {
		writeErr(w, driver.NewErr(driver.KindNotFound, "转存任务不存在", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": 200, "task": t.Snapshot()})
}
