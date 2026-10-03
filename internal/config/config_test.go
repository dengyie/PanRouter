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
