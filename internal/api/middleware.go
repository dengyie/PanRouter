// Package api 是传输层:chi 路由、中间件与 handler,不含业务逻辑。
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/service"
)

type ctxKey int

const requestIDKey ctxKey = 1

func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

func (d *Deps) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func (d *Deps) zapLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			d.Log.Infow("http", "method", r.Method, "path", r.URL.Path,
				"status", ww.Status(), "bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(), "request_id", RequestIDFrom(r.Context()))
		}()
		next.ServeHTTP(ww, r)
	})
}

func (d *Deps) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				d.Log.Errorf("panic: %v", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"code": 500, "kind": "internal", "message": "internal error",
					"request_id": RequestIDFrom(r.Context()),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (d *Deps) tokenOK(token string) bool {
	if token == "" {
		return false
	}
	apiToken := d.Cfg.Get().Auth.APIToken
	if apiToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(apiToken)) == 1 {
		return true
	}
	_, err := verifyJWT(d.Cfg.Get().Auth.JWTSecret, token)
	return err == nil
}

// optionalAuth:无 token 当游客;坏 token 401(让前端清会话);有效 token 才允许 Pick Cookie。
func (d *Deps) optionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !d.tokenOK(token) {
			writeErr(w, driver.NewErr(driver.KindAuthExpired, "登录已过期,请重新登录", nil))
			return
		}
		next.ServeHTTP(w, r.WithContext(service.WithAuthed(r.Context(), true)))
	})
}

// auth:Web 端 JWT 与脚本端 API Token 二选一(设计文档 §6)。
func (d *Deps) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeErr(w, driver.NewErr(driver.KindAuthExpired, "未登录或缺少 Authorization", nil))
			return
		}
		if !d.tokenOK(token) {
			writeErr(w, driver.NewErr(driver.KindAuthExpired, "登录已过期,请重新登录", nil))
			return
		}
		next.ServeHTTP(w, r.WithContext(service.WithAuthed(r.Context(), true)))
	})
}
