package service

import (
	"context"
	"strings"
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
