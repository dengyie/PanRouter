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
