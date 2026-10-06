package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenNoFile(t *testing.T) {
	// 包目录下无 config.yaml:应使用默认值且 usedPath 为空
	c, used, err := Load("")
	if err != nil {
		t.Fatalf("defaults load: %v", err)
	}
	if used != "" {
		t.Fatalf("used path should be empty, got %q", used)
	}
	if c.Server.Listen != "127.0.0.1:6400" || c.Server.DeployProfile != "home" {
		t.Fatalf("unexpected defaults: %+v", c.Server)
	}
	if !c.Drivers["quark"].Enabled {
		t.Fatal("quark should be enabled by default")
	}
	if c.Auth.APIToken != DefaultAPIToken {
		t.Fatal("default token constant mismatch")
	}
}

func TestLoadValidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	yaml := `
server:
  listen: 0.0.0.0:7000
  deploy_profile: cloud
domain_routes:
  quark: [pan.quark.cn]
drivers:
  quark:
    enabled: true
    limit_qps: 3
`
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, used, err := Load(p)
	if err != nil || used != p {
		t.Fatalf("load: used=%q err=%v", used, err)
	}
	if c.Server.DeployProfile != "cloud" || c.Server.Listen != "0.0.0.0:7000" {
		t.Fatalf("server not applied: %+v", c.Server)
	}
	if !c.Drivers["quark"].Enabled || c.Drivers["quark"].LimitQPS != 3 {
		t.Fatalf("driver config not applied: %+v", c.Drivers["quark"])
	}
	if len(c.DomainRoutes["quark"]) != 1 {
		t.Fatalf("domain routes not applied: %+v", c.DomainRoutes)
	}
}

// 评审 P2 回归:配置文件存在但解析失败必须报错,不得静默回落默认密钥。
func TestLoadBrokenFileIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(p, []byte("server: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(p); err == nil {
		t.Fatal("broken config must return error, not silent defaults")
	}
}

func TestLoadMissingExplicitPathIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.yaml")
	if _, _, err := Load(p); err == nil {
		t.Fatal("explicit missing path must return error")
	}
}

func TestLoadReloadPreservesOmittedFields(t *testing.T) {
	current := Default()
	current.Auth.APIToken = "stable-token"
	current.Auth.JWTSecret = "stable-secret"
	current.Server.BaseURL = "https://example.test"
	p := filepath.Join(t.TempDir(), "reload.yaml")
	if err := os.WriteFile(p, []byte("server:\n  deploy_profile: cloud\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, _, err := LoadReload(p, current)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if next.Auth != current.Auth || next.Server.BaseURL != current.Server.BaseURL {
		t.Fatalf("reload reverted startup fields: got auth=%+v server=%+v", next.Auth, next.Server)
	}
	if next.Server.DeployProfile != "cloud" {
		t.Fatalf("reloadable profile not applied: %q", next.Server.DeployProfile)
	}
}

func TestLoadReloadRejectsStartupChanges(t *testing.T) {
	current := Default()
	p := filepath.Join(t.TempDir(), "reload.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  jwt_secret: rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadReload(p, current); err == nil {
		t.Fatal("reload must reject startup-only auth changes")
	}
}
