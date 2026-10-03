package driver

import (
	"context"
	"testing"
)

type stubDriver struct{ id string }

func (s *stubDriver) ID() string { return s.id }
func (s *stubDriver) ResolveShare(context.Context, ShareLink, *Credential) ([]FileNode, error) {
	return nil, nil
}
func (s *stubDriver) GetDirectLink(context.Context, *Credential, FileRef) (DirectLink, error) {
	return DirectLink{}, nil
}
func (s *stubDriver) CheckCredential(context.Context, Credential) (CredStatus, error) {
	return CredStatus{}, nil
}

func TestDetectLongestSuffix(t *testing.T) {
	reg := NewRegistry(
		[]Driver{&stubDriver{id: "quark"}, &stubDriver{id: "lanzou"}},
		map[string][]string{
			"quark":  {"pan.quark.cn"},
			"lanzou": {"lanzou.com", "lanzouw.com"},
		},
	)
	d, err := reg.Detect("https://pan.quark.cn/s/abc123")
	if err != nil || d.ID() != "quark" {
		t.Fatalf("quark detect: %v %v", d, err)
	}
	d, err = reg.Detect("https://foo.lanzouw.com/xyz")
	if err != nil || d.ID() != "lanzou" {
		t.Fatalf("lanzou detect: %v %v", d, err)
	}
	d, err = reg.Detect("HTTPS://PAN.QUARK.CN/s/abc")
	if err != nil || d.ID() != "quark" {
		t.Fatalf("case-insensitive detect: %v %v", d, err)
	}
	if _, err := reg.Detect("https://example.com/x"); err == nil {
		t.Fatal("未知域名应报错")
	}
	if _, err := reg.Detect("not a url"); err == nil {
		t.Fatal("非法链接应报错")
	}
}

func TestDetectRouteWithoutEnabledDriverSkipped(t *testing.T) {
	// 只启用 quack;lanzou 路由即使配置了也不应命中
	reg := NewRegistry([]Driver{&stubDriver{id: "quark"}}, map[string][]string{
		"quark":  {"quark.cn"},
		"lanzou": {"lanzou.com"},
	})
	if _, err := reg.Detect("https://x.lanzou.com/a"); err == nil {
		t.Fatal("未启用 driver 的路由不应命中")
	}
}

// 优化回归:域名路由表必须支持热更新(设计文档 §7.11)。
func TestUpdateRoutesHotReload(t *testing.T) {
	reg := NewRegistry([]Driver{&stubDriver{id: "quark"}}, map[string][]string{
		"quark": {"old.example"},
	})
	if _, err := reg.Detect("https://a.old.example/x"); err != nil {
		t.Fatalf("initial route: %v", err)
	}

	reg.UpdateRoutes(map[string][]string{"quark": {"new.example"}})
	if _, err := reg.Detect("https://a.old.example/x"); err == nil {
		t.Fatal("old route should be removed after update")
	}
	if d, err := reg.Detect("https://b.new.example/x"); err != nil || d.ID() != "quark" {
		t.Fatalf("new route: %v %v", d, err)
	}

	// 指向未启用 driver 的路由应被忽略
	reg.UpdateRoutes(map[string][]string{"ghost": {"g.example"}})
	if _, err := reg.Detect("https://g.example/x"); err == nil {
		t.Fatal("route of unknown driver must be ignored")
	}
}
