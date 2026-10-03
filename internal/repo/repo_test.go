package repo

import (
	"path/filepath"
	"testing"
	"time"
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
