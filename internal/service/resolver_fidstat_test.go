package service

import (
	"context"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// fidOnlyDriver 模拟 mangodrive:FID 就是存储路径,单文件解析时 ResolveShare 直返一条。
type fidOnlyDriver struct{ fakeDriver }

func (d *fidOnlyDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{{
		FID:  "/Pool/a.mkv",
		Name: "a.mkv",
		Size: 777,
		Ext:  map[string]string{"path": "/Pool/a.mkv"},
	}}, nil
}

func (d *fidOnlyDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{URL: "https://up.invalid/a.mkv", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// 回归:客户端只带 fid 直接提链(从未列分享)时,名字/大小必须回问 driver,
// 不能把「文件」+0 发给下载端(桌面端要靠它命名与预分配)。
func TestResolveFileFidOnlyStatsFromDriver(t *testing.T) {
	d := &fidOnlyDriver{}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "/Pool/a.mkv", false, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.FileName != "a.mkv" || res.Size != 777 {
		t.Fatalf("metadata not from driver: name=%q size=%d want a.mkv/777", res.FileName, res.Size)
	}
}

// driver 也查不到时仍要给出可用结果(名字兜底「文件」,不报错)。
func TestResolveFileFidOnlyFallback(t *testing.T) {
	d := &fakeDriver{linkURL: "https://up.invalid/x"}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "unknown", false, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.FileName != "文件" || res.Size != 0 {
		t.Fatalf("fallback changed: name=%q size=%d", res.FileName, res.Size)
	}
}
