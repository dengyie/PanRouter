package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
	"github.com/dengyie/panrouter/internal/service"
)

// Deps 是 api 层的全部依赖(由 main 组装),handler 只做参数转换与响应。
type Deps struct {
	Version  string
	Cfg      *config.Provider
	Resolver *service.Resolver
	Relay    *service.Relay
	Aria2    *service.Aria2
	Accounts *service.AccountService
	Store    *repo.Store
	AES      *crypto.AES
	Signer   *sign.Signer
	Log      *zap.SugaredLogger
	Met      *metrics.Registry
	WebFS    fs.FS // web/dist(可为 nil)
}

func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(d.requestID)
	r.Use(d.zapLogger)
	r.Use(d.recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": d.Version})
	})
	r.Get("/readyz", d.handleReady)
	r.Handle("/metrics", d.Met.Handler())

	// 公开下载端点(HMAC 签名校验,无需登录态)
	r.Get("/d/{pan}/{key}/{fid}", d.handleDownload302)
	r.Get("/stream/{pan}/{key}/{fid}", d.handleStream)

	if d.WebFS != nil {
		r.Get("/", d.handleIndex)
	}

	r.Route("/api/v1", func(api chi.Router) {
		api.Post("/auth/login", d.handleLogin)
		api.Group(func(pub chi.Router) {
			pub.Use(d.optionalAuth)
			pub.Post("/resolve", d.handleResolve)
			pub.Post("/resolve/batch", d.handleResolveBatch)
		})
		api.Group(func(pr chi.Router) {
			pr.Use(d.auth)
			pr.Get("/json/{pan}/{key}/{fid}", d.handleDirectJSON)
			pr.Get("/accounts", d.handleListAccounts)
			pr.Post("/accounts", d.handleCreateAccount)
			pr.Delete("/accounts/{id}", d.handleDeleteAccount)
			pr.Post("/accounts/{id}/refresh", d.handleRefreshAccount)
			pr.Post("/downloads", d.handlePushDownload)
			pr.Get("/downloads", d.handleListDownloads)
			pr.Get("/downloads/{gid}", d.handleDownloadStatus)
		})
	})

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
	})
	return r
}

// ---- 响应工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 把业务错误(driver.Error 契约)映射为 HTTP 状态与可操作提示。
func writeErr(w http.ResponseWriter, err error) {
	de := &driver.Error{Kind: driver.KindUpstream, UserHint: err.Error()}
	if !errors.As(err, &de) {
		de = &driver.Error{Kind: driver.KindUpstream, UserHint: err.Error()}
	}
	writeJSON(w, kindStatus(de.Kind), map[string]any{
		"code":      kindStatus(de.Kind),
		"kind":      de.Kind,
		"message":   de.UserHint,
		"retriable": de.Retriable,
	})
}

func kindStatus(k driver.Kind) int {
	switch k {
	case driver.KindNotFound, driver.KindShareGone:
		return http.StatusNotFound
	case driver.KindAuthExpired:
		return http.StatusUnauthorized
	case driver.KindRiskControl:
		return http.StatusTooManyRequests
	case driver.KindUnsupported:
		return http.StatusBadRequest
	default: // Upstream / InterfaceChanged
		return http.StatusBadGateway
	}
}
