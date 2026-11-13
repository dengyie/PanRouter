// AccountService:凭据生命周期——加解密、互斥刷新(singleflight)、状态流转。
package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
)

type AccountService struct {
	store *repo.Store
	aes   *crypto.AES
	met   *metrics.Registry
	log   *loggerType

	sf singleflight.Group // 内部并发安全,无需外层互斥

	// rr 是每个网盘的账号轮转游标:多账号池必须轮转,否则
	// store.PickAccount 的「最近检查优先」会让同一个账号吃掉全部请求,
	// 池子退化成单账号(风控命中时整条链路一起停摆)。
	rrMu sync.Mutex
	rr   map[string]int
}

// credCheckBudget 是共享凭据检查任务的独立期限:不跟随任何一个 HTTP 请求的
// 生命周期,也不受看门狗 ticker 取消影响;上游卡死时单轮最多占用该时长。
const credCheckBudget = 60 * time.Second

func NewAccountService(store *repo.Store, aes *crypto.AES, met *metrics.Registry, log *loggerType) *AccountService {
	return &AccountService{store: store, aes: aes, met: met, log: log}
}

func (s *AccountService) EncryptCred(c *driver.Credential) ([]byte, error) {
	return s.aes.EncryptBytes([]byte(c.Cookie))
}

func (s *AccountService) DecryptCred(acc *repo.Account) (*driver.Credential, error) {
	plain, err := s.aes.DecryptBytes(acc.CredEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	return &driver.Credential{Cookie: string(plain)}, nil
}

// Pick 轮转选出一个可用账号(无可用账号返回 nil,nil —— 免登录盘允许匿名)。
//
// 可选集合与 store.PickAccount 一致:status=ok,或 cooling 且冷却已到期
// (选中即复位 ok);expired/banned 永远不可选。差异只在选择策略:
// 这里按「网盘维度轮转」而不是「最近检查优先」,保证多账号池真正分散负载。
func (s *AccountService) Pick(pan string) (*repo.Account, *driver.Credential, error) {
	list, err := s.store.ListAccounts(pan)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	eligible := make([]repo.Account, 0, len(list))
	for i := range list {
		switch list[i].Status {
		case "ok":
			eligible = append(eligible, list[i])
		case "cooling":
			if list[i].CooldownUntil != nil && list[i].CooldownUntil.Before(now) {
				eligible = append(eligible, list[i])
			}
		}
	}
	if len(eligible) == 0 {
		return nil, nil, nil
	}
	s.rrMu.Lock()
	if s.rr == nil {
		s.rr = map[string]int{}
	}
	start := s.rr[pan] % len(eligible)
	s.rr[pan]++
	s.rrMu.Unlock()

	// ★ 坏凭据不得毒化整个账号池:解密失败是确定性故障(主密钥变更/数据损坏),
	//   轮转撞上坏号时必须跳过并落库 expired,继续尝试下一个候选。旧实现直接
	//   返回错误,结果是一个解不开的账号就让该 pan 的**全部**登录态请求 502
	//   「读取账号凭据失败」,而且状态仍是 ok,下一轮继续撞同一堵墙。
	for i := 0; i < len(eligible); i++ {
		acc := eligible[(start+i)%len(eligible)]
		if acc.Status == "cooling" {
			// 冷却到期的账号被选中即恢复可用;落库失败不影响本次使用。
			acc.Status = "ok"
			_ = s.store.MarkAccountStatus(acc.ID, "ok")
		}
		cred, derr := s.DecryptCred(&acc)
		if derr == nil {
			// gauge 以库内全量为准,一次 Pick 至多刷一次:循环内逐号刷新会让
			// n 个坏号触发 n 次全量 ListAccounts 重算,放大 DB 压力。
			s.RefreshStatusMetrics()
			return &acc, cred, nil
		}
		s.log.Warnf("account %d pan=%s credential unreadable, mark expired: %v", acc.ID, acc.PanType, derr)
		if merr := s.store.MarkAccountStatus(acc.ID, "expired"); merr != nil {
			s.log.Warnf("mark account %d expired: %v", acc.ID, merr)
		}
		s.store.AddAudit("cred_unreadable",
			fmt.Sprintf("account=%d pan=%s: %v", acc.ID, acc.PanType, derr))
	}
	// 全部候选都解不开:按「无可用账号」处理(免登录盘可继续匿名解析),
	// 上面已逐个标记 expired 并留审计,不再用 502 掩盖真实原因。
	s.RefreshStatusMetrics()
	return nil, nil, nil
}

// AuditUnreadable 启动自检:统计无法用当前主密钥解密的凭据数。
//
// 主密钥来源是 PANROUTER_MASTER_KEY,未设置时回落 jwt_secret;两者任一变化,
// 历史账号就会全部解不开。若不在启动时暴露,这种故障只会在请求期表现为
// 「读取账号凭据失败」,根因不可见。只读,不改库——落库 expired 由实际使用
// (Pick / checkAcc)完成。
func (s *AccountService) AuditUnreadable() (total, unreadable int) {
	list, err := s.store.ListAccounts("")
	if err != nil {
		s.log.Warnf("credential self-check: %v", err)
		return 0, 0
	}
	for i := range list {
		if _, derr := s.DecryptCred(&list[i]); derr != nil {
			unreadable++
			s.log.Warnf("credential self-check: account=%d pan=%s unreadable: %v",
				list[i].ID, list[i].PanType, derr)
		}
	}
	return len(list), unreadable
}

// Check 凭据校验:看门狗与请求失败触发的刷新合并为一次执行(singleflight),
// 避免一次性 refresh token 并发双刷导致互踢(强制项)。
func (s *AccountService) Check(ctx context.Context, accID uint, drv driver.Driver) (driver.CredStatus, error) {
	acc, err := s.store.GetAccount(accID)
	if err != nil || acc == nil {
		return driver.CredStatus{}, fmt.Errorf("account %d not found", accID)
	}
	return s.checkAcc(ctx, acc, drv)
}

// checkAcc 是 singleflight 校验链的核心;Check 与 Refresh 共用,账号只查一次。
// 共享任务使用独立有界 ctx(credCheckBudget),不绑定首个调用者——首调(管理页
// HTTP 请求)取消不拖垮看门狗与其他等待者(P2-12 同类);每个调用者在消费结果前
// 仍会检查自身 ctx 的取消。
func (s *AccountService) checkAcc(ctx context.Context, acc *repo.Account, drv driver.Driver) (driver.CredStatus, error) {
	v, err, _ := s.sf.Do(fmt.Sprintf("acc:%d", acc.ID), func() (any, error) {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credCheckBudget)
		defer cancel()
		cred, err := s.DecryptCred(acc)
		if err != nil {
			// ★ 凭据不可解密是确定性故障(主密钥变更/数据损坏),必须落库:
			//   旧实现只把 Valid=false 写进返回值,status 仍是 ok —— 管理页手动
			//   刷新与 watchdog 每轮都"检查成功",坏号永远留在可用池里,还会被
			//   请求链路反复选中。banned 是人工终态,校验不得越权解除。
			now := time.Now()
			status := "expired"
			if acc.Status == "banned" {
				status = "banned"
			}
			if uerr := s.store.UpdateAccountCheck(acc.ID, status, now, acc.Status); uerr != nil {
				s.log.Warnf("persist unreadable credential status account=%d: %v", acc.ID, uerr)
			}
			s.store.AddAudit("cred_check",
				fmt.Sprintf("account=%d pan=%s valid=false reason=unreadable", acc.ID, acc.PanType))
			s.RefreshStatusMetrics()
			s.log.Warnf("credential unreadable: account=%d pan=%s: %v", acc.ID, acc.PanType, err)
			return driver.CredStatus{Valid: false, Message: err.Error()}, nil
		}
		st, err := drv.CheckCredential(tctx, *cred)
		if err != nil {
			return driver.CredStatus{}, err
		}
		now := time.Now()
		status := "expired"
		if st.Valid {
			status = "ok"
			// ★ 健康检查成功**不得**解除风控冷却:cooling 的复位路径只有一条
			//   (PickAccount 里 cooldown_until 到期自动复位),否则一次「碰巧
			//   有效」的探测就把被风控的账号写回 ok,冷却形同虚设。
			//   banned 同理:banned 是人工/风控终态(该 pan 当前无写入方,保留
			//   语义供运维手改与仪表盘展示),探测成功不能越权复活 —— 状态机
			//   的单向逃逸只有冷却到期,其余终态的解除都该走人工路径。
			switch acc.Status {
			case "cooling", "banned":
				status = acc.Status
			}
		}
		if err := s.store.UpdateAccountCheck(acc.ID, status, now, acc.Status); err != nil {
			return st, err
		}
		s.RefreshStatusMetrics()
		s.store.AddAudit("cred_check", fmt.Sprintf("account=%d pan=%s valid=%v", acc.ID, acc.PanType, st.Valid))
		s.log.Infof("credential checked: account=%d pan=%s valid=%v", acc.ID, acc.PanType, st.Valid)
		return st, nil
	})
	if cerr := ctx.Err(); cerr != nil {
		return driver.CredStatus{}, cerr
	}
	if err != nil {
		return driver.CredStatus{}, err
	}
	return v.(driver.CredStatus), nil
}

// MarkStatus 供 resolver 在 AuthExpired 时标记账号失效。
// ★ 只写 status 列,不做「读整行 → 改内存 → 写回整行」:整行回写会拿一次
//   可能已过期的快照把 cooldown_until / cred_version 等并发改动一起冲掉
//   (如 MarkStatus 与 MarkCooldown 并发时,后写者用旧值复活冷却/凭据代际)。
func (s *AccountService) MarkStatus(accID uint, status string) {
	_ = s.store.MarkAccountStatus(accID, status)
	s.RefreshStatusMetrics()
}

// MarkCooldown 风控时给账号降温。同样只写涉及的列(见 MarkStatus)。
func (s *AccountService) MarkCooldown(accID uint, d time.Duration) {
	_ = s.store.MarkAccountCooldown(accID, time.Now().Add(d))
	s.RefreshStatusMetrics()
}

// accountStatuses 是账号状态全集(§9):gauge 刷新时对缺失状态补零,防陈旧序列残留。
var accountStatuses = [...]string{"ok", "cooling", "expired", "banned"}

// Create 管理页建号:网盘类型校验(panKnown 由调用方注入 driver 注册表)、
// Cookie 非空、加密落库、审计与指标刷新——业务规则收口在 service,api 层只做参数与响应。
func (s *AccountService) Create(pan, name, cookie string, panKnown func(string) bool) (*repo.Account, error) {
	if !panKnown(pan) {
		return nil, driver.NewErr(driver.KindNotFound, "不支持的网盘类型:"+pan, nil)
	}
	if strings.TrimSpace(cookie) == "" {
		return nil, driver.NewErr(driver.KindNotFound, "cookie 不能为空", nil)
	}
	enc, err := s.EncryptCred(&driver.Credential{Cookie: cookie})
	if err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "凭据加密失败", err)
	}
	acc := &repo.Account{PanType: pan, Name: name, CredEnc: enc, Status: "ok", CredVersion: 1}
	if err := s.store.CreateAccount(acc); err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "保存账号失败", err)
	}
	s.store.AddAudit("account_create", "pan="+pan+" id="+strconv.FormatUint(uint64(acc.ID), 10))
	s.RefreshStatusMetrics()
	return acc, nil
}

// Delete 删号 + 审计 + 指标刷新;缺失 ID 幂等(GetAccount 对不存在返回 nil,nil)。
// 该 pan 的最后一个账号被删后,gauge 无法表达"序列不存在",对其状态全集补零。
func (s *AccountService) Delete(id uint) error {
	acc, err := s.store.GetAccount(id)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "删除失败", err)
	}
	if err := s.store.DeleteAccount(id); err != nil {
		return driver.NewErr(driver.KindUpstream, "删除失败", err)
	}
	s.store.AddAudit("account_delete", "id="+strconv.FormatUint(uint64(id), 10))
	s.RefreshStatusMetrics()
	if acc != nil && s.met != nil {
		if list, lerr := s.store.ListAccounts(acc.PanType); lerr == nil && len(list) == 0 {
			for _, st := range accountStatuses {
				s.met.Set("panrouter_account_status", map[string]string{"pan": acc.PanType, "status": st}, 0)
			}
		}
	}
	return nil
}

// Refresh 管理页手动校验:定位账号 → 注入 driver(driverFor 由调用方注入注册表) → 复用 singleflight 校验链。
func (s *AccountService) Refresh(ctx context.Context, id uint, driverFor func(pan string) (driver.Driver, bool)) (driver.CredStatus, error) {
	acc, err := s.store.GetAccount(id)
	if err != nil || acc == nil {
		return driver.CredStatus{}, driver.NewErr(driver.KindNotFound, "账号不存在", nil)
	}
	drv, ok := driverFor(acc.PanType)
	if !ok {
		return driver.CredStatus{}, driver.NewErr(driver.KindNotFound, "网盘 driver 未启用", nil)
	}
	return s.checkAcc(ctx, acc, drv)
}

// RefreshStatusMetrics 重算 panrouter_account_status{pan,status} gauge:
// 以 ListAccounts 全量为准,对出现过的 pan × 状态全集逐一 Set(无值补零)。
// 账号量级为个位数(单管理员),O(n) 重算可忽略,免去逐路径维护增量计数。
func (s *AccountService) RefreshStatusMetrics() {
	if s.met == nil {
		return
	}
	list, err := s.store.ListAccounts("")
	if err != nil {
		s.log.Warnf("refresh account_status: %v", err)
		return
	}
	counts := map[string]float64{}
	pans := map[string]struct{}{}
	for i := range list {
		a := &list[i]
		counts[a.PanType+"\x00"+a.Status]++
		pans[a.PanType] = struct{}{}
	}
	for pan := range pans {
		for _, st := range accountStatuses {
			s.met.Set("panrouter_account_status", map[string]string{"pan": pan, "status": st}, counts[pan+"\x00"+st])
		}
	}
}
