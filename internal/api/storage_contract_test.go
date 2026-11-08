package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/app"
	"github.com/dengyie/panrouter/internal/config"
)

// // newStorageEnv 起一个 App 并只返回 Router(不连任何上游),用于探测 /api/v1/storage。
func newStorageEnv(t *testing.T, mut func(*config.Config)) (*httptest.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.APIToken = "tok"
	cfg.Auth.JWTSecret = "jwt"
	cfg.Server.DataDir = t.TempDir()
	if mut != nil {
		mut(cfg)
	}
	a, err := app.Build(app.Options{Cfg: cfg, Logger: zap.NewNop().Sugar(), MasterKey: "k", AllowPrivateClients: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ts := httptest.NewServer(Router(Deps{
		Version: "t", Cfg: a.Cfg, Resolver: a.Resolver, Transfer: a.Transfer, Store: a.Store,
		AES: a.AES, Signer: a.Signer, Log: a.Log, Met: a.Met,
	}))
	t.Cleanup(ts.Close)
	return ts, "tok"
}

// 默认部署:public_base্ধ由 domain_routes 派生,不写死在客户端。
func TestStorageInfoDefaultsDeriveBase(t *testing.T) {
	ts, token := newStorageEnv(t, nil)
	st, body := doRaw(t, ts, token, "GET", "/api/v1/storage", "")
	if st != http.StatusOK {
		t.Fatalf("status=%d body=%v", st, body)
	}
	if body["enabled"] != true {
		t.Fatalf("enabled 应为 true: %v", body)
	}
	if got := body["public_base"]; got != "https://drive.example.com" {
		t.Fatalf("public_base=%v", got)
	}
	if got := body["dest_dir"]; got != "/" {
		t.Fatalf("未配置 dest_dir 时应为端点根目录: %v", got)
	}
}

// 换部署:只改 public_base,客户端无需改代码。
func TestStorageInfoPublicBaseOverride(t *testing.T) {
	ts, token := newStorageEnv(t, func(c *config.Config) {
		c.Transfer.PublicBase = "https://pool.example.net/"
		c.Transfer.DestDir = "/dav/inbox"
	})
	_, body := doRaw(t, ts, token, "GET", "/api/v1/storage", "")
	if got := body["public_base"]; got != "https://pool.example.net" {
		t.Fatalf("public_base=%v", got)
	}
	if got := body["dest_dir"]; got != "/dav/inbox" {
		t.Fatalf("dest_dir=%v", got)
	}
}

// 未配置域名也没有 transfer → enabled=false(而不是报错)。
func TestStorageInfoDisabledWithoutBase(t *testing.T) {
	ts, token := newStorageEnv(t, func(c *config.Config) {
		c.DomainRoutes = nil
		c.Drivers = nil
	})
	st, body := doRaw(t, ts, token, "GET", "/api/v1/storage", "")
	if st != http.StatusOK {
		t.Fatalf("status=%d", st)
	}
	if body["enabled"] != false {
		t.Fatalf("应为 enabled=false: %v", body)
	}
	if _, ok := body["public_base"]; ok {
		t.Fatalf("disabled 时不应给 public_base: %v", body)
	}
}

// 未登录不可读(与 transfer 其它端点一致)。
func TestStorageInfoRequiresAuth(t *testing.T) {
	ts, _ := newStorageEnv(t, nil)
	st, _ := doRawNoAuth(t, ts, "GET", "/api/v1/storage", "")
	if st != http.StatusUnauthorized {
		t.Fatalf("anon: status=%d", st)
	}
}
