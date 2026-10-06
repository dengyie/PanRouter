package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
)

func TestWriteErrKeepsAuthenticationKindsDistinct(t *testing.T) {
	cases := []struct {
		name string
		kind driver.Kind
	}{
		{name: "upstream credential", kind: driver.KindAuthExpired},
		{name: "site session", kind: driver.KindSessionExpired},
		{name: "login credential", kind: driver.KindAuthInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, driver.NewErr(tc.kind, "test", nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want 401", rec.Code)
			}
			var body struct {
				Kind driver.Kind `json:"kind"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Kind != tc.kind {
				t.Fatalf("kind=%q, want %q", body.Kind, tc.kind)
			}
		})
	}
}

func TestOptionalAuthInvalidBearerUsesSessionExpired(t *testing.T) {
	provider := &config.Provider{}
	provider.Set(config.Default())
	deps := Deps{Cfg: provider, Log: zap.NewNop().Sugar(), Met: metrics.New()}
	h := deps.optionalAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/resolve", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	var body struct {
		Kind driver.Kind `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != driver.KindSessionExpired {
		t.Fatalf("kind=%q, want %q", body.Kind, driver.KindSessionExpired)
	}
}

func TestAuthMiddlewareUsesSessionExpired(t *testing.T) {
	cfg := config.Default()
	provider := &config.Provider{}
	provider.Set(cfg)
	deps := Deps{Cfg: provider, Log: zap.NewNop().Sugar(), Met: metrics.New()}
	h := deps.auth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "missing", header: ""},
		{name: "invalid", header: "Bearer invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want 401", rec.Code)
			}
			var body struct {
				Kind driver.Kind `json:"kind"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Kind != driver.KindSessionExpired {
				t.Fatalf("kind=%q, want %q", body.Kind, driver.KindSessionExpired)
			}
		})
	}
}
