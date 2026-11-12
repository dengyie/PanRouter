package service

// v2.12 账号池根因回归:一个解不开的凭据不得毒化整个 pan 的账号池。
//
// 现场:历史账号可能由另一把主密钥加密(PANROUTER_MASTER_KEY 未设置时回落
// jwt_secret,或密钥轮换)。旧实现里 Pick 轮转选中它后直接返回解密错误:
//   ① 该 pan 的**全部**登录态请求 502「读取账号凭据失败」;
//   ② 账号状态仍是 ok(错误没有落库),下一轮继续撞同一个坏号;
//   ③ 管理页手动刷新 / watchdog 的 checkAcc 同样只在响应里给 Valid=false,
//      不落库,坏号永远留在可用池。
// 修复口径:解密失败 = 确定性故障 → 跳过并落库 expired,继续尝试其余候选;
// 全部不可用时退化为「无可用账号」(免登录盘可匿名),不再用 502 掩盖根因。

import (
	"context"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/repo"
)

// corruptCred 把账号密文替换成不可解密的内容,模拟主密钥不匹配/数据损坏。
func corruptCred(t *testing.T, store *repo.Store, id uint) {
	t.Helper()
	acc, err := store.GetAccount(id)
	if err != nil || acc == nil {
		t.Fatalf("get account %d: %v", id, err)
	}
	acc.CredEnc = []byte("corrupted-ciphertext")
	if err := store.UpdateAccount(acc); err != nil {
		t.Fatalf("corrupt account %d: %v", id, err)
	}
}

func mustCreateAcc(t *testing.T, svc *AccountService, name, cookie string) *repo.Account {
	t.Helper()
	acc, err := svc.Create("fake", name, cookie, panKnown)
	if err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	return acc
}

// 核心回归:坏号排在轮转首位时,必须跳过并选中下一个可用账号。
func TestPickSkipsUnreadableAccount(t *testing.T) {
	svc, store, met := newTestAccService(t)
	bad := mustCreateAcc(t, svc, "bad", "c=bad")
	good := mustCreateAcc(t, svc, "good", "c=good")
	corruptCred(t, store, bad.ID)

	acc, cred, err := svc.Pick("fake")
	if err != nil {
		t.Fatalf("Pick 不得因单个坏号失败: %v", err)
	}
	if acc == nil || acc.ID != good.ID {
		t.Fatalf("应跳过坏号选中可用账号, got=%+v", acc)
	}
	if cred == nil || cred.Cookie != "c=good" {
		t.Fatalf("选中账号的凭据不匹配: %+v", cred)
	}

	got, _ := store.GetAccount(bad.ID)
	if got == nil || got.Status != "expired" {
		t.Fatalf("坏号必须落库 expired, got=%+v", got)
	}
	if body := scrapeMetrics(t, met); !strings.Contains(body, `panrouter_account_status{pan="fake",status="expired"} 1`) {
		t.Fatalf("expired gauge 未刷新: %s", body)
	}

	// 坏号已退出可选集合,后续轮转不再重复尝试。
	acc2, _, err := svc.Pick("fake")
	if err != nil || acc2 == nil || acc2.ID != good.ID {
		t.Fatalf("后续 Pick 应稳定选中可用账号: acc=%+v err=%v", acc2, err)
	}
}

// 对照组:全部坏号时退化为「无可用账号」而不是 502;每个坏号都要落库。
func TestPickAllUnreadableDegradesToNoAccount(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	a := mustCreateAcc(t, svc, "a", "c=a")
	b := mustCreateAcc(t, svc, "b", "c=b")
	corruptCred(t, store, a.ID)
	corruptCred(t, store, b.ID)

	acc, cred, err := svc.Pick("fake")
	if err != nil {
		t.Fatalf("全部不可解密应按无可用账号处理, got err=%v", err)
	}
	if acc != nil || cred != nil {
		t.Fatalf("不应返回账号: acc=%+v cred=%+v", acc, cred)
	}
	for _, id := range []uint{a.ID, b.ID} {
		got, _ := store.GetAccount(id)
		if got == nil || got.Status != "expired" {
			t.Fatalf("account %d 必须落库 expired, got=%+v", id, got)
		}
	}
}

// 回归护栏:跳过逻辑不得破坏多账号轮转(池子退化成单账号会集中风控)。
func TestPickRotatesAcrossHealthyAccounts(t *testing.T) {
	svc, _, _ := newTestAccService(t)
	a := mustCreateAcc(t, svc, "a", "c=a")
	b := mustCreateAcc(t, svc, "b", "c=b")

	seen := map[uint]bool{}
	for i := 0; i < 2; i++ {
		acc, _, err := svc.Pick("fake")
		if err != nil || acc == nil {
			t.Fatalf("pick %d: acc=%+v err=%v", i, acc, err)
		}
		seen[acc.ID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Fatalf("两次 Pick 必须各自命中一个账号, got=%v", seen)
	}
}

// 管理页刷新/watchdog 路径:不可解密必须落库 expired(last_check_at 也要写),
// 不能只把 Valid=false 放进响应。
func TestCheckUnreadableCredentialMarksExpired(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc := mustCreateAcc(t, svc, "n", "c=1")
	corruptCred(t, store, acc.ID)

	drv := &stubDriver{valid: true}
	st, err := svc.Check(context.Background(), acc.ID, drv)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if st.Valid {
		t.Fatalf("不可解密的凭据不得报有效: %+v", st)
	}
	if drv.calls != 0 {
		t.Fatalf("解密失败不应打上游, calls=%d", drv.calls)
	}
	got, _ := store.GetAccount(acc.ID)
	if got == nil || got.Status != "expired" {
		t.Fatalf("必须落库 expired, got=%+v", got)
	}
	if got.LastCheckAt == nil {
		t.Fatal("last_check_at 必须随校验落库")
	}
}

// banned 是人工终态:校验即使发现凭据不可解密,也不得把 banned 改成 expired。
func TestCheckUnreadableCredentialKeepsBanned(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	acc := mustCreateAcc(t, svc, "n", "c=1")
	corruptCred(t, store, acc.ID)
	if err := store.MarkAccountStatus(acc.ID, "banned"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Check(context.Background(), acc.ID, &stubDriver{valid: true}); err != nil {
		t.Fatalf("check: %v", err)
	}
	got, _ := store.GetAccount(acc.ID)
	if got == nil || got.Status != "banned" {
		t.Fatalf("banned 终态不得被校验改写, got=%+v", got)
	}
}

// 启动自检:只读统计不可解密账号数,不修改状态。
func TestAuditUnreadable(t *testing.T) {
	svc, store, _ := newTestAccService(t)
	mustCreateAcc(t, svc, "good", "c=good")
	bad := mustCreateAcc(t, svc, "bad", "c=bad")
	corruptCred(t, store, bad.ID)

	total, unreadable := svc.AuditUnreadable()
	if total != 2 || unreadable != 1 {
		t.Fatalf("audit total=%d unreadable=%d want 2/1", total, unreadable)
	}
	got, _ := store.GetAccount(bad.ID)
	if got == nil || got.Status != "ok" {
		t.Fatalf("启动自检必须只读,不得改状态, got=%+v", got)
	}
}
