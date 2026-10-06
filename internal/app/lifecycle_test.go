package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"go.uber.org/zap"
)

// cycleDriver 只做凭据校验计数,验证看门狗真实跑过一轮。
type cycleDriver struct{ calls atomic.Int64 }

func (d *cycleDriver) ID() string { return "fake" }
func (d *cycleDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return nil, nil
}
func (d *cycleDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{}, nil
}
func (d *cycleDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	d.calls.Add(1)
	return driver.CredStatus{Valid: true}, nil
}

func newTestApp(t *testing.T) (*App, *cycleDriver) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Drivers["fake"] = config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3}
	drv := &cycleDriver{}
	a, err := Build(Options{
		Cfg: cfg, Logger: zap.NewNop().Sugar(), MasterKey: "test-key",
		ExtraDrivers: []ExtraDriver{{Driver: drv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) }) // 未启动后台时为空操作
	return a, drv
}

// §15.2 item 4 回归:后台任务归 App——看门狗按注入周期真实校验账号;
// stop 等待协程退出后才返回,关库不再与协程竞态。
func TestBackgroundWatchdogRunsAndStopWaits(t *testing.T) {
	a, drv := newTestApp(t)
	if _, err := a.Accounts.Create("fake", "n", "c=1", func(pan string) bool {
		_, ok := a.Registry.Get(pan)
		return ok
	}); err != nil {
		t.Fatal(err)
	}

	stop := a.StartBackground(BackgroundOptions{WatchdogInterval: 20 * time.Millisecond})
	deadline := time.Now().Add(2 * time.Second)
	for drv.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watchdog never checked the account")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stop()
	stop() // 重复调用安全
	// stop 已返回 = 协程退出;此刻关库不会撞上在途查询
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Shutdown 未启动后台任务时必须可安全调用(e2e/纯装配路径)。
func TestShutdownWithoutBackground(t *testing.T) {
	a, _ := newTestApp(t)
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// slowCheckDriver 校验阻塞至 gate 放行,制造"后台任务在途且卡死"的停机场景。
type slowCheckDriver struct {
	calls   atomic.Int64
	entered chan struct{}
	gate    chan struct{}
}

func (d *slowCheckDriver) ID() string { return "fake" }
func (d *slowCheckDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return nil, nil
}
func (d *slowCheckDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{}, nil
}
func (d *slowCheckDriver) CheckCredential(ctx context.Context, c driver.Credential) (driver.CredStatus, error) {
	d.calls.Add(1)
	select {
	case <-d.entered:
	default:
		close(d.entered)
	}
	// 等待 gate 或 ctx 取消:模拟"修复后取消能打断卡死校验"的路径
	select {
	case <-d.gate:
	case <-ctx.Done():
		return driver.CredStatus{}, ctx.Err()
	}
	return driver.CredStatus{Valid: true}, nil
}

// 回归(review 2026-10-07):Shutdown 的 ctx 必须真实约束等待上限——后台任务
// 卡死时,ctx 超时后 Shutdown 仍返回并继续关闭 Store,不得无限挂死。
func TestShutdownCtxBoundsWait(t *testing.T) {
	drv := &slowCheckDriver{entered: make(chan struct{}), gate: make(chan struct{})}
	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir()
	cfg.Drivers["fake"] = config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3}
	a, err := Build(Options{
		Cfg: cfg, Logger: zap.NewNop().Sugar(), MasterKey: "test-key",
		ExtraDrivers: []ExtraDriver{{Driver: drv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accounts.Create("fake", "n", "c=1", func(pan string) bool {
		_, ok := a.Registry.Get(pan)
		return ok
	}); err != nil {
		t.Fatal(err)
	}

	a.StartBackground(BackgroundOptions{WatchdogInterval: 20 * time.Millisecond})
	deadline := time.Now().Add(2 * time.Second)
	for drv.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watchdog never started a check")
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-drv.entered // 校验已进入阻塞点,gate 仍关着

	// gate 不放行,协程卡在 CheckCredential;ctx 200ms 超时后 Shutdown 必须返回
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 应在 ctx 超时后仍返回并关库, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown 未受 ctx 约束, 耗时 %v", elapsed)
	}
	close(drv.gate) // 收尾:放行协程使其退出(cancel 已广播,协程不会再碰已关的 Store)
}
