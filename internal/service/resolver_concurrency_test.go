package service

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/breaker"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/limiter"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
)

const testShareURL = "https://fake.example/s/x"

// concDriver:在 fakeDriver 之上统计调用次数与峰值在途并发。
type concDriver struct {
	fakeDriver
	calls    atomic.Int32
	inflight atomic.Int32
	max      atomic.Int32
	delay    time.Duration
}

func (d *concDriver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	d.calls.Add(1)
	cur := d.inflight.Add(1)
	for {
		m := d.max.Load()
		if cur <= m || d.max.CompareAndSwap(m, cur) {
			break
		}
	}
	time.Sleep(d.delay)
	link, err := d.fakeDriver.GetDirectLink(ctx, cred, ref)
	d.inflight.Add(-1)
	return link, err
}

func newTestResolver(t *testing.T, drv driver.Driver, dc config.DriverCommon) (*Resolver, *repo.Store) {
	t.Helper()
	store, err := repo.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	cfg.Drivers["fake"] = dc // 调用方必须传 Enabled=true 的完整配置
	cfgp := &config.Provider{}
	cfgp.Set(cfg)
	log := zap.NewNop().Sugar()
	aes := crypto.New("k")
	reg := driver.NewRegistry([]driver.Driver{drv}, map[string][]string{"fake": {"fake.example"}})

	met := metrics.New()
	return NewResolver(cfgp, reg, store, aes,
		NewAccountService(store, aes, met, log), limiter.New(),
		breaker.NewRegistry(5, time.Minute), sign.New("k"), met, log), store
}

// 优化回归:过期直链的并发刷新必须经 singleflight 收敛为一次真实 driver 调用。
func TestGetFreshLinkSingleflight(t *testing.T) {
	cd := &concDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}, delay: 50 * time.Millisecond}
	resolver, store := newTestResolver(t, cd, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	if _, err := resolver.ResolveShare(context.Background(), testShareURL, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveFile(context.Background(), testShareURL, "", "f1", false, ""); err != nil {
		t.Fatal(err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	// 把已存直链置为过期
	if err := store.ExpireLink(key, "f1"); err != nil {
		t.Fatal(err)
	}

	base := cd.calls.Load() // 种子阶段的调用不计入收敛断言
	const n = 8
	var wg sync.WaitGroup
	links := make([]*repo.Link, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			links[i], errs[i] = resolver.GetFreshLink(context.Background(), key, "f1", "")
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if links[i] == nil {
			t.Fatalf("call %d: nil link", i)
		}
	}
	if got := cd.calls.Load() - base; got != 1 {
		t.Fatalf("并发 %d 次请求应收敛为 1 次真实刷新,实际 %d 次", n, got)
	}
}

// 优化回归:单网盘在途并发不得超过 download_concurrency。
func TestResolveFileConcurrencyCap(t *testing.T) {
	cd := &concDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}, delay: 50 * time.Millisecond}
	resolver, _ := newTestResolver(t, cd, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 1})

	if _, err := resolver.ResolveShare(context.Background(), testShareURL, "", ""); err != nil {
		t.Fatal(err)
	}
	base := cd.calls.Load() // 分享解析会自动提链 f1,不计入后续并发断言

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = resolver.ResolveFile(context.Background(), testShareURL, "", string(rune('a'+i)), false, "")
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
	}
	if got := cd.max.Load(); got != 1 {
		t.Fatalf("download_concurrency=1 时峰值在途应为 1,实际 %d", got)
	}
	if got := cd.calls.Load() - base; got != n {
		t.Fatalf("应完成 %d 次真实调用,实际 %d", n, got)
	}
}
