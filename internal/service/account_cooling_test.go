package service

// v2.11 回归:账号状态机的两个并发面/旁路面。
//
// ① 健康检查成功不得解除风控冷却:cooling 的复位路径只有一条
//   (PickAccount 里 cooldown_until 到期自动复位)。修复前 checkAcc 把
//   「凭据有效」直接写回 ok —— 一次探测就把被风控的账号复活。
// ② MarkStatus / MarkCooldown 改为定向列更新:整行读改写在并发下会拿过期
//   快照把 cooldown_until / cred_version 一起冲掉。

import (
	"context"
	"testing"
	"time"
)

func TestCheckKeepsCoolingOnValidCredential(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	// 置为冷却中(风控生效)
	if err := store.MarkAccountCooldown(acc.ID, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// 凭据有效(stubDriver.valid=true)
	if _, err := svc.Check(context.Background(), acc.ID, &stubDriver{valid: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
	got, _ := store.GetAccount(acc.ID)
	if got.Status != "cooling" {
		t.Fatalf("健康检查成功不得解除冷却: status=%q want cooling", got.Status)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.After(time.Now()) {
		t.Fatalf("冷却到期时间不得被清除: %+v", got.CooldownUntil)
	}
}

// 对照组:非冷却账号校验成功 → 正常回到 ok(不得被误伤成永久 cooling)。
func TestCheckOkStaysOkOnValidCredential(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Check(context.Background(), acc.ID, &stubDriver{valid: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
	got, _ := store.GetAccount(acc.ID)
	if got.Status != "ok" {
		t.Fatalf("有效凭据应写回 ok: status=%q", got.Status)
	}
}

// 凭据失效时 cooling → expired 是真实的状态迁移,必须放行。
func TestCheckExpiredTransitionsFromCooling(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAccountCooldown(acc.ID, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// stubDriver.valid=false → CheckCredential 返回 KindAuthExpired;
	// checkAcc 对 driver 层错误直接上抛(与修复前一致),这里断言的是
	// MarkStatus 旁路:resolver 在 AuthExpired 时标记 expired。
	svc.MarkStatus(acc.ID, "expired")
	got, _ := store.GetAccount(acc.ID)
	if got.Status != "expired" {
		t.Fatalf("失效标记应生效: status=%q", got.Status)
	}
	if got.CooldownUntil == nil {
		t.Fatal("定向更新不得冲掉 cooldown_until(整行回写会把它清成 NULL/零值)")
	}
}

// MarkStatus 定向更新:不得经手整行(回归由 TestCheckExpiredTransitionsFromCooling
// 的 cooldown_until 断言覆盖)。这里补一个「冷却标记自身」的直接断言。
func TestMarkCooldownSetsCoolingAndUntil(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc, err := svc.Create("fake", "n", "c=1", panKnown)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkAccountCooldown(acc.ID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetAccount(acc.ID)
	if got.Status != "cooling" {
		t.Fatalf("MarkCooldown 应置 cooling: status=%q", got.Status)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown_until 未写入: %+v", got.CooldownUntil)
	}
}
