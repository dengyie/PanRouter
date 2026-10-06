package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
	"go.uber.org/zap"
)

// scrapeMetrics 拉取 /metrics 文本(经 Handler 导出路径,与线上格式一致)。
func scrapeMetrics(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// P2 观测接线回归:账号状态流转必须刷新 panrouter_account_status{pan,status},
// 旧状态序列补零,不得残留陈旧值。
func TestAccountStatusGaugeWired(t *testing.T) {
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	met := metrics.New()
	acc := NewAccountService(store, crypto.New("k"), met, zap.NewNop().Sugar())

	a := &repo.Account{PanType: "fake", Name: "n", CredEnc: []byte("x"), Status: "ok", CredVersion: 1}
	if err := store.CreateAccount(a); err != nil {
		t.Fatal(err)
	}
	acc.MarkStatus(a.ID, "expired")

	body := scrapeMetrics(t, met)
	if !strings.Contains(body, `panrouter_account_status{pan="fake",status="expired"} 1`) {
		t.Fatalf("expired 计数缺失: %s", body)
	}
	if !strings.Contains(body, `panrouter_account_status{pan="fake",status="ok"} 0`) {
		t.Fatalf("旧状态序列未补零: %s", body)
	}
}

// P2 观测接线回归:breaker_open gauge 必须带 scope 维度区分登录/游客分键,
// 且熔断开启后的提前返回路径也要刷新 gauge(入口即刷新)。
func TestBreakerOpenGaugeScopeAndEarlyReturn(t *testing.T) {
	d := &twoFileDriver{fn: func(context.Context, string) (driver.DirectLink, error) {
		return driver.DirectLink{}, driver.NewErr(driver.KindRiskControl, "触发风控", nil)
	}}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// 连续 5 次风控触发熔断(maxFails=5)
	for i := 0; i < 5; i++ {
		_, _ = resolver.ResolveFile(ctx, testShareURL, "", "f1", false, "")
	}
	// 熔断提前返回:不得触达 driver
	_, _ = resolver.ResolveFile(ctx, testShareURL, "", "f1", false, "")
	body := scrapeMetrics(t, resolver.met)
	if !strings.Contains(body, `panrouter_breaker_open{pan="fake",scope="login"} 1`) {
		t.Fatalf("熔断开启未在提前返回路径刷新带 scope 的 gauge: %s", body)
	}

	// 游客分键隔离:guest 通道独立计数,值为 0
	_, _ = resolver.ResolveFile(WithAuthed(context.Background(), false), testShareURL, "", "f1", false, "")
	body = scrapeMetrics(t, resolver.met)
	if !strings.Contains(body, `panrouter_breaker_open{pan="fake",scope="guest"} 0`) {
		t.Fatalf("guest 分键序列缺失: %s", body)
	}
}
