package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// twoFileDriver 列出两个文件,GetDirectLink 行为由 fn 决定。
type twoFileDriver struct {
	calls atomic.Int32
	fn    func(ctx context.Context, fid string) (driver.DirectLink, error)
}

func (d *twoFileDriver) ID() string { return "fake" }
func (d *twoFileDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{
		{FID: "f1", Name: "a.bin", Size: 1},
		{FID: "f2", Name: "b.bin", Size: 1},
	}, nil
}
func (d *twoFileDriver) GetDirectLink(ctx context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	d.calls.Add(1)
	if d.fn != nil {
		return d.fn(ctx, ref.FID)
	}
	return driver.DirectLink{URL: "https://up.invalid/" + ref.FID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *twoFileDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true}, nil
}

type truncListDriver struct{ fakeDriver }

func (d *truncListDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{{
		FID: "f1", Name: "a.bin", Size: 1,
		Ext: map[string]string{"truncated": "1"},
	}}, nil
}

func TestResolveShareAuthExpiredStops(t *testing.T) {
	d := &twoFileDriver{fn: func(context.Context, string) (driver.DirectLink, error) {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "Cookie 已失效,请到账号页更新", nil)
	}}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatalf("ResolveShare 遇 AuthExpired 应仍返回列表, got %v", err)
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "账号") {
		t.Fatalf("hint 应提示加账号, got %q", res.Hint)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files=%d", len(res.Files))
	}
	if res.Files[0].LinkError == "" {
		t.Fatalf("首个文件应有 link_error: %+v", res.Files[0])
	}
	if res.Files[0].DownloadURL != "" {
		t.Fatalf("AuthExpired 不应给出 download_url")
	}
	if res.Files[1].LinkError != "" || res.Files[1].DownloadURL != "" {
		t.Fatalf("停后续提链后第二文件应保持未提链: %+v", res.Files[1])
	}
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("AuthExpired 后应只调一次 GetDirectLink, 实际 %d", got)
	}
}

func TestResolveShareDeadlineSkipsAutoLink(t *testing.T) {
	d := &twoFileDriver{}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := resolver.ResolveShare(ctx, testShareURL, "", "")
	if err != nil {
		t.Fatalf("列表仍应成功: %v", err)
	}
	if d.calls.Load() != 0 {
		t.Fatalf("剩余时间不足时不应自动提链, calls=%d", d.calls.Load())
	}
	if !strings.Contains(res.Hint, "剩余时间不足") {
		t.Fatalf("hint=%q", res.Hint)
	}
	for _, f := range res.Files {
		if f.DownloadURL != "" {
			t.Fatalf("不应带 download_url: %+v", f)
		}
	}
}

func TestResolveShareTruncationHint(t *testing.T) {
	resolver, _ := newTestResolver(t, &truncListDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("truncated 应为 true")
	}
	if !strings.Contains(res.Hint, "仅展开前") {
		t.Fatalf("hint=%q", res.Hint)
	}
}

func TestAutoLinkMaxFor(t *testing.T) {
	if autoLinkMaxFor("quark") != quarkAutoLinkMax {
		t.Fatalf("quark auto-link max=%d want %d", autoLinkMaxFor("quark"), quarkAutoLinkMax)
	}
	if autoLinkMaxFor("lanzou") != defaultAutoLinkMax {
		t.Fatalf("lanzou auto-link max=%d want %d", autoLinkMaxFor("lanzou"), defaultAutoLinkMax)
	}
}

func TestJoinHints(t *testing.T) {
	if got := joinHints("", "a", "", "b"); got != "a;b" {
		t.Fatalf("got %q", got)
	}
}

func TestCanAutoLink(t *testing.T) {
	if !canAutoLink(context.Background()) {
		t.Fatal("无 deadline 应允许提链")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if canAutoLink(ctx) {
		t.Fatal("剩余 <15s 不应继续提链")
	}
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if canAutoLink(done) {
		t.Fatal("已取消不应继续提链")
	}
}
