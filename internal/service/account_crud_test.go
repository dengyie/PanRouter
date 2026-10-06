package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
	"go.uber.org/zap"
)

func newTestAccService(t *testing.T) (*AccountService, *repo.Store, *metrics.Registry) {
	t.Helper()
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	met := metrics.New()
	return NewAccountService(store, crypto.New("k"), met, zap.NewNop().Sugar()), store, met
}

var (
	panKnown   = func(string) bool { return true }
	panUnknown = func(string) bool { return false }
)

// stubDriver 只实现凭据校验计数,其余接口返回零值。
type stubDriver struct {
	valid bool
	calls int
}

func (d *stubDriver) ID() string { return "fake" }
func (d *stubDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return nil, nil
}
func (d *stubDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{}, nil
}
func (d *stubDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	d.calls++
	if d.valid {
		return driver.CredStatus{Valid: true, Nickname: "n"}, nil
	}
	return driver.CredStatus{}, driver.NewErr(driver.KindAuthExpired, "失效", nil)
}

// §15.2 item 1 回归:账号创建业务规则(网盘校验/Cookie 非空/加密/落库/审计/指标)收口在 AccountService。
func TestAccountServiceCreate(t *testing.T) {
	svc, store, met := newTestAccService(t)
	if _, err := svc.Create("fake", "n", "c=1", panUnknown); err == nil {
		t.Fatal("unknown pan must fail")
	}
	if _, err := svc.Create("fake", "n", "   ", panKnown); err == nil {
		t.Fatal("empty cookie must fail")
	}
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID == 0 || acc.Status != "ok" || acc.CredVersion != 1 {
		t.Fatalf("unexpected account: %+v", acc)
	}
	got, err := store.GetAccount(acc.ID)
	if err != nil || got == nil || len(got.CredEnc) == 0 {
		t.Fatalf("account not persisted encrypted: %v %v", got, err)
	}
	body := scrapeMetrics(t, met)
	if !strings.Contains(body, `panrouter_account_status{pan="fake",status="ok"} 1`) {
		t.Fatalf("gauge not refreshed on create: %s", body)
	}
}

// §15.2 item 1 回归:删除收口在 service;缺失 ID 幂等;该 pan 清空后残留序列补零。
func TestAccountServiceDelete(t *testing.T) {
	svc, store, met := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(acc.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetAccount(acc.ID); got != nil {
		t.Fatal("account should be gone")
	}
	if err := svc.Delete(acc.ID); err != nil {
		t.Fatalf("delete of missing id must stay idempotent: %v", err)
	}
	if body := scrapeMetrics(t, met); strings.Contains(body, `panrouter_account_status{pan="fake",status="ok"} 1`) {
		t.Fatalf("stale gauge after last delete: %s", body)
	}
}

// §15.2 item 1 回归:手动校验收口在 service(账号定位/驱动注入/状态流转)。
func TestAccountServiceRefresh(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	drv := &stubDriver{valid: true}
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(context.Background(), acc.ID, func(string) (driver.Driver, bool) { return drv, true }); err != nil {
		t.Fatal(err)
	}
	if drv.calls != 1 {
		t.Fatalf("driver must be checked once, calls=%d", drv.calls)
	}
	if got, _ := store.GetAccount(acc.ID); got == nil || got.Status != "ok" {
		t.Fatalf("status should be ok after refresh: %+v", got)
	}
	if _, err := svc.Refresh(context.Background(), 999, func(string) (driver.Driver, bool) { return drv, true }); err == nil {
		t.Fatal("missing account must fail")
	}
	if _, err := svc.Refresh(context.Background(), acc.ID, func(string) (driver.Driver, bool) { return nil, false }); err == nil {
		t.Fatal("missing driver must fail")
	}
}

// gateCheckDriver 校验阻塞在 gate 上,验证共享任务独立于首调者 ctx。
type gateCheckDriver struct {
	stubDriver
	enteredOnce sync.Once
	entered     chan struct{}
	gate        chan struct{}
}

func (d *gateCheckDriver) CheckCredential(ctx context.Context, c driver.Credential) (driver.CredStatus, error) {
	d.enteredOnce.Do(func() { close(d.entered) })
	<-d.gate
	return d.stubDriver.CheckCredential(ctx, c)
}

// 回归(review 2026-10-07,P2-12 同类):singleflight 共享的凭据检查任务必须
// 使用独立有界 ctx,不绑定首个调用者。首调(管理页手动 Refresh,HTTP ctx)
// 取消后,共享任务继续完成并落库,后续等待者仍拿到结果;首调自身响应其 ctx。
func TestRefreshFirstCallerCancelDoesNotKillSharedCheck(t *testing.T) {
	drv := &gateCheckDriver{
		stubDriver: stubDriver{valid: true},
		entered:    make(chan struct{}),
		gate:       make(chan struct{}),
	}
	svc, store, _ := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	driverFor := func(string) (driver.Driver, bool) { return drv, true }

	type res struct {
		st  driver.CredStatus
		err error
	}
	ctx1, cancel := context.WithCancel(context.Background())
	ch1 := make(chan res, 1)
	go func() {
		st, e := svc.Refresh(ctx1, acc.ID, driverFor)
		ch1 <- res{st, e}
	}()
	<-drv.entered // 共享任务已进入真实校验
	cancel()      // 首调取消

	ch2 := make(chan res, 1)
	go func() {
		st, e := svc.Refresh(context.Background(), acc.ID, driverFor)
		ch2 <- res{st, e}
	}()

	close(drv.gate) // 放行真实校验
	r1 := <-ch1
	if !errors.Is(r1.err, context.Canceled) {
		t.Fatalf("首调应响应自身 ctx 取消, got %v", r1.err)
	}
	r2 := <-ch2
	if r2.err != nil {
		t.Fatalf("等待者应在首调取消后仍拿到结果, got %v", r2.err)
	}
	if !r2.st.Valid {
		t.Fatalf("共享结果丢失: %+v", r2.st)
	}
	if got, _ := store.GetAccount(acc.ID); got == nil || got.Status != "ok" {
		t.Fatalf("共享任务应已完成落库: %+v", got)
	}
}
