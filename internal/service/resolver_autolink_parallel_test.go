package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// parLinkDriver 是声明了 driver.LinkParallel 的 fake:统计峰值并发与调用数,
// 用于验证有界并发提链的顺序、上限与停链语义。
type parLinkDriver struct {
	maxConc int
	delay   time.Duration
	files   []driver.FileNode
	failFID string

	mu             sync.Mutex
	inflight, peak int
	calls          int
}

func (d *parLinkDriver) ID() string            { return "fake" }
func (d *parLinkDriver) MaxParallelLinks() int { return d.maxConc }

func (d *parLinkDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return d.files, nil
}

func (d *parLinkDriver) GetDirectLink(ctx context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	d.mu.Lock()
	d.inflight++
	d.calls++
	if d.inflight > d.peak {
		d.peak = d.inflight
	}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inflight--
		d.mu.Unlock()
	}()
	// 鉴权失败按现实即刻返回,不参与 delay——保证「首错立即停链」的用例确定性。
	if d.failFID == ref.FID {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "Cookie 已失效", nil)
	}
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return driver.DirectLink{}, ctx.Err()
		}
	}
	return driver.DirectLink{URL: "https://up.invalid/" + ref.FID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (d *parLinkDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true}, nil
}

func (d *parLinkDriver) stats() (peak, calls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peak, d.calls
}

func parFiles(n int) []driver.FileNode {
	files := make([]driver.FileNode, n)
	for i := range files {
		files[i] = driver.FileNode{FID: fmt.Sprintf("f%d", i), Name: fmt.Sprintf("n%d.bin", i), Size: 1}
	}
	return files
}

// 并发提链:全部拿到直链、顺序不变、并发不超配置上限,且确实并行(耗时远低于串行)。
func TestAutoLinkParallelBoundedOrdered(t *testing.T) {
	files := parFiles(6)
	d := &parLinkDriver{maxConc: 4, delay: 150 * time.Millisecond, files: files}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 6 {
		t.Fatalf("files=%d", len(res.Files))
	}
	for i := range files {
		if res.Files[i].FID != files[i].FID {
			t.Fatalf("顺序被打乱: idx=%d fid=%s want=%s", i, res.Files[i].FID, files[i].FID)
		}
		// buildResult 会把直链改写为服务端 302 路径(/d/{pan}/{key}/{fid}?sig=…),
		// 校验 URL 尾段就是该 fid。
		u := strings.SplitN(res.Files[i].DownloadURL, "?", 2)[0]
		if !strings.HasSuffix(u, "/"+files[i].FID) {
			t.Fatalf("第 %d 个文件未拿到直链: %+v", i, res.Files[i])
		}
	}
	peak, calls := d.stats()
	if calls != 6 {
		t.Fatalf("calls=%d want 6", calls)
	}
	if peak < 2 {
		t.Fatalf("未并行, peak=%d", peak)
	}
	if peak > 3 {
		t.Fatalf("峰值并发 %d 超出 download_concurrency=3", peak)
	}
	// 并发上限靠 driver rugged 墙钟计数断言,不靠总耗时:单文件路径里
	// 还有 upsert 等仓储开销,在慢盘/竞态检测下会淹没 100ms 级差值。
}

// 并发提链遇鉴权失败:首个文件带错误+hint,在途/未发起的文件保持未提链,
// 且在途请求不得被记成额外失败。
func TestAutoLinkParallelAuthStops(t *testing.T) {
	files := parFiles(6)
	d := &parLinkDriver{maxConc: 4, delay: 200 * time.Millisecond, files: files, failFID: "f0"}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatalf("鉴权失败仍应返回列表: %v", err)
	}
	if res.Files[0].LinkError == "" {
		t.Fatalf("首个文件应有鉴权错误: %+v", res.Files[0])
	}
	if !strings.Contains(res.Hint, "Cookie 已失效") {
		t.Fatalf("hint=%q 应带上 driver 提示", res.Hint)
	}
	for i := 1; i < len(res.Files); i++ {
		if res.Files[i].LinkError != "" || res.Files[i].DownloadURL != "" {
			t.Fatalf("停链后第 %d 个文件应保持未提链: %+v", i, res.Files[i])
		}
	}
	if _, calls := d.stats(); calls > 3 {
		t.Fatalf("鉴权失败后仍继续提链: calls=%d", calls)
	}
}

// 并发度取值:driver 声明值再与 download_concurrency / limit_qps / 待提链数取小;
// 未声明并发能力的 driver 恒为 1(串行)。
func TestAutoLinkConcurrency(t *testing.T) {
	d := &parLinkDriver{maxConc: 4}
	wide, _ := newTestResolver(t, &twoFileDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 10, DownloadConc: 8})
	if got := wide.autoLinkConcurrency(d, "fake", 8); got != 4 {
		t.Fatalf("conc=%d want 4", got)
	}
	if got := wide.autoLinkConcurrency(d, "fake", 2); got != 2 {
		t.Fatalf("conc=%d want 2(pending 上限)", got)
	}
	if got := wide.autoLinkConcurrency(d, "fake", 1); got != 1 {
		t.Fatalf("conc=%d want 1(单文件不并发)", got)
	}
	narrow, _ := newTestResolver(t, &twoFileDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 10, DownloadConc: 1})
	if got := narrow.autoLinkConcurrency(d, "fake", 8); got != 1 {
		t.Fatalf("conc=%d want 1(download_concurrency 钳制)", got)
	}
	slow, _ := newTestResolver(t, &twoFileDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 2, DownloadConc: 8})
	if got := slow.autoLinkConcurrency(d, "fake", 8); got != 2 {
		t.Fatalf("conc=%d want 2(limit_qps 钳制)", got)
	}
	if got := wide.autoLinkConcurrency(&twoFileDriver{}, "fake", 8); got != 1 {
		t.Fatalf("conc=%d want 1(未声明并发能力)", got)
	}
}
