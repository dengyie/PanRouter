package service

// v2.12.3 回归:本地准入拒绝(令牌桶空/熔断打开)不得冷却账号。
//
// 现场:6 个并发登录态解析把 quark 的本地令牌桶(limit_qps=2)打空,3 个请求拿到
// 429 后账号被标成 cooling 5 分钟。根因是 guard 的本地限频/熔断复用了
// driver.KindRiskControl,mapErr 于是把「我们自己的闸门」当成「上游风控」去惩罚
// 账号。修复后对外契约不变(仍 risk_control → 429),内部用 errLocalThrottle
// 区分来源:本地拒绝不改账号状态,上游 risk_control 仍然冷却。

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// failLinkDriver:列目录沿用 fakeDriver,提链固定返回指定错误并计数。
type failLinkDriver struct {
	fakeDriver
	err   error
	calls atomic.Int32
}

func (d *failLinkDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	d.calls.Add(1)
	return driver.DirectLink{}, d.err
}

// seedSnapshot 直接落一份分享快照,避免 ResolveShare 的自动提链干扰
// 「本地限频/熔断」的调用计数与账号状态。
func seedSnapshot(t *testing.T, store *repo.Store, key string) {
	t.Helper()
	if err := store.UpsertShare(&repo.Share{
		PanType:    "fake",
		ShareURL:   testShareURL,
		ShareKey:   key,
		RawTree:    `[{"FID":"f1","Name":"n.bin","Size":300}]`,
		ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed share snapshot: %v", err)
	}
}

func authedCtx() context.Context { return WithAuthed(context.Background(), true) }

func accountStatus(t *testing.T, store *repo.Store, id uint) string {
	t.Helper()
	acc, err := store.GetAccount(id)
	if err != nil || acc == nil {
		t.Fatalf("get account %d: acc=%+v err=%v", id, acc, err)
	}
	return acc.Status
}

// 本地限频:仍返回 429 语义,但账号必须保持 ok。
func TestLocalRateLimitDoesNotCoolAccount(t *testing.T) {
	drv := &fakeDriver{linkURL: "https://up.invalid/f"}
	dc := config.DriverCommon{Enabled: true, LimitQPS: 0.01, DownloadConc: 3}
	resolver, store := newTestResolver(t, drv, dc)
	acc, err := resolver.accounts.Create("fake", "a", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, store, resolver.shareKey("fake", testShareURL, ""))

	// 消耗桶里唯一的令牌:等价于并发请求先到,下一个请求必然被本地限频拒绝
	// (0.01 QPS 下补满一个令牌要 100s,测试窗口内不可能恢复)。
	if !resolver.limiter.Allow("fake", dc.LimitQPS) {
		t.Fatal("初始令牌应可用")
	}

	_, err = resolver.ResolveFile(authedCtx(), testShareURL, "", "f1", false, "")
	if err == nil {
		t.Fatal("令牌桶已空,必须被本地限频拒绝")
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.KindRiskControl {
		t.Fatalf("对外契约应保持 risk_control/429, got=%v", err)
	}
	var lt *errLocalThrottle
	if !errors.As(err, &lt) {
		t.Fatalf("拒绝必须标记为本地准入(errLocalThrottle), got=%T", err)
	}
	if got := accountStatus(t, store, acc.ID); got != "ok" {
		t.Fatalf("本地限频不得冷却账号, status=%s", got)
	}
	if body := scrapeMetrics(t, resolver.met); !strings.Contains(body, `panrouter_local_throttle_total{pan="fake",reason="rate_limit"} 1`) {
		t.Fatalf("本地限频计数缺失: %s", body)
	}
}

// 熔断打开:同样只拒绝、不冷却账号(账号本来就是健康的,失败来自上游)。
func TestBreakerOpenDoesNotCoolAccount(t *testing.T) {
	drv := &failLinkDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"},
		err: driver.NewErr(driver.KindUpstream, "上游 500", nil)}
	dc := config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3}
	resolver, store := newTestResolver(t, drv, dc)
	acc, err := resolver.accounts.Create("fake", "a", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, store, resolver.shareKey("fake", testShareURL, ""))

	// 连续失败触发熔断(Registry 为 5 次/min)。
	for i := 0; i < 5; i++ {
		if _, err := resolver.ResolveFile(authedCtx(), testShareURL, "", "f1", false, ""); err == nil {
			t.Fatalf("第 %d 次调用应失败", i+1)
		}
	}
	if got := accountStatus(t, store, acc.ID); got != "ok" {
		t.Fatalf("上游错误不得改写账号状态, status=%s", got)
	}
	before := drv.calls.Load()

	_, err = resolver.ResolveFile(authedCtx(), testShareURL, "", "f1", false, "")
	var lt *errLocalThrottle
	if err == nil || !errors.As(err, &lt) {
		t.Fatalf("熔断后必须由本地准入直接拒绝, got=%v", err)
	}
	if got := drv.calls.Load(); got != before {
		t.Fatalf("熔断打开后不应再打上游, calls=%d want %d", got, before)
	}
	if got := accountStatus(t, store, acc.ID); got != "ok" {
		t.Fatalf("熔断拒绝不得冷却账号, status=%s", got)
	}
	if body := scrapeMetrics(t, resolver.met); !strings.Contains(body, `panrouter_local_throttle_total{pan="fake",reason="breaker"} 1`) {
		t.Fatalf("熔断计数缺失: %s", body)
	}
}

// 对照组:上游真风控仍必须冷却账号(修复不能把副作用一并抹掉)。
func TestUpstreamRiskControlStillCoolsAccount(t *testing.T) {
	drv := &failLinkDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"},
		err: driver.NewErr(driver.KindRiskControl, "上游风控", nil)}
	dc := config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3}
	resolver, store := newTestResolver(t, drv, dc)
	acc, err := resolver.accounts.Create("fake", "a", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	seedSnapshot(t, store, resolver.shareKey("fake", testShareURL, ""))

	_, err = resolver.ResolveFile(authedCtx(), testShareURL, "", "f1", false, "")
	if err == nil {
		t.Fatal("上游风控必须向上返回错误")
	}
	var lt *errLocalThrottle
	if errors.As(err, &lt) {
		t.Fatalf("上游风控不应被标记成本地准入")
	}
	if got := accountStatus(t, store, acc.ID); got != "cooling" {
		t.Fatalf("上游风控必须冷却账号, status=%s", got)
	}
}
