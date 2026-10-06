package repo

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() }) // Windows 下必须显式关闭,否则 TempDir 清理失败
	return s
}

// 账号状态机:ok → cooling(未到期不可选)→ cooling(到期可选中并自动复位 ok)→ expired 不可选。
// 该测试是评审 P2(冷却后永不恢复)的回归护栏。
func TestPickAccountStateMachine(t *testing.T) {
	s := newTestStore(t)

	if acc, err := s.PickAccount("quark"); err != nil || acc != nil {
		t.Fatalf("empty store: acc=%v err=%v", acc, err)
	}

	a := &Account{PanType: "quark", Name: "n", CredEnc: []byte("x"), Status: "ok", CredVersion: 1}
	if err := s.CreateAccount(a); err != nil {
		t.Fatal(err)
	}

	got, err := s.PickAccount("quark")
	if err != nil || got == nil || got.Status != "ok" {
		t.Fatalf("ok account should be pickable: %+v %v", got, err)
	}

	// cooling 且未到期 → 不可选
	future := time.Now().Add(5 * time.Minute)
	got.Status, got.CooldownUntil = "cooling", &future
	if err := s.UpdateAccount(got); err != nil {
		t.Fatal(err)
	}
	if acc, _ := s.PickAccount("quark"); acc != nil {
		t.Fatalf("cooling account must not be pickable before cooldown expires")
	}

	// cooling 且已到期 → 可选,且状态自动复位为 ok
	past := time.Now().Add(-time.Minute)
	a2, _ := s.GetAccount(got.ID)
	a2.CooldownUntil = &past
	if err := s.UpdateAccount(a2); err != nil {
		t.Fatal(err)
	}
	acc, err := s.PickAccount("quark")
	if err != nil || acc == nil {
		t.Fatalf("expired cooldown must be pickable: %v", err)
	}
	if acc.Status != "ok" {
		t.Fatalf("picked cooling account must reset to ok, got %s", acc.Status)
	}
	if a3, _ := s.GetAccount(acc.ID); a3.Status != "ok" {
		t.Fatalf("reset must be persisted, got %s", a3.Status)
	}

	// expired → 永不可选
	a4, _ := s.GetAccount(acc.ID)
	a4.Status = "expired"
	_ = s.UpdateAccount(a4)
	if acc, _ := s.PickAccount("quark"); acc != nil {
		t.Fatal("expired account must not be pickable")
	}
}

func TestAccountUpdatesDoNotResurrectDeletedRows(t *testing.T) {
	s := newTestStore(t)
	a := &Account{PanType: "quark", Name: "n", CredEnc: []byte("secret"), Status: "ok", CredVersion: 1}
	if err := s.CreateAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Status = "expired"
	if err := s.UpdateAccount(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccount(a.ID); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := s.db.Model(&Account{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted account was resurrected: %d rows", count)
	}
	if err := s.UpdateAccountCheck(a.ID, "ok", time.Now(), "ok"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleted account check should report not found, got %v", err)
	}
}

// cred_version 语义(§15.3):凭据代际,仅凭据内容变更(重新保存)时递增;
// 健康检查只更新状态与 last_check_at,不得递增——否则管理页每次刷新都会制造虚假版本变更。
func TestUpdateAccountCheckKeepsCredVersion(t *testing.T) {
	s := newTestStore(t)
	a := &Account{PanType: "quark", Name: "n", CredEnc: []byte("x"), Status: "ok", CredVersion: 7}
	if err := s.CreateAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccountCheck(a.ID, "ok", time.Now(), "ok"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(a.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.CredVersion != 7 {
		t.Fatalf("health check must not bump cred_version: got %d", got.CredVersion)
	}
	if got.LastCheckAt.IsZero() {
		t.Fatal("last_check_at must be updated")
	}
}

func TestUpsertLinkIdempotent(t *testing.T) {
	s := newTestStore(t)
	l1 := &Link{ShareKey: "k", FID: "f", FileName: "a", DirectLink: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.UpsertLink(l1); err != nil {
		t.Fatal(err)
	}
	l2 := &Link{ShareKey: "k", FID: "f", FileName: "b", DirectLink: "u2", ExpiresAt: time.Now().Add(2 * time.Hour)}
	if err := s.UpsertLink(l2); err != nil {
		t.Fatal(err)
	}
	if l2.ID != l1.ID {
		t.Fatalf("upsert should reuse row: %d vs %d", l1.ID, l2.ID)
	}
	got, err := s.GetLink("k", "f")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.FileName != "b" || got.DirectLink != "u2" {
		t.Fatalf("upsert should update fields: %+v", got)
	}
}

// ExpireLink:上游否决后立即失效缓存,下次读取视为过期(触发重解析)。
func TestExpireLink(t *testing.T) {
	s := newTestStore(t)
	l := &Link{ShareKey: "k", FID: "f", FileName: "a", DirectLink: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.UpsertLink(l); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireLink("k", "f"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLink("k", "f")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.ExpiresAt.After(time.Now()) {
		t.Fatalf("link should be expired, expires_at=%v", got.ExpiresAt)
	}
}

// Store.Ping:readyz 探活的唯一入口,api 层不得直接依赖 gorm.DB。
func TestStorePing(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ping(); err != nil {
		t.Fatalf("open store should ping ok: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(); err == nil {
		t.Fatal("ping must fail after close")
	}
}
