// AccountService:凭据生命周期——加解密、互斥刷新(singleflight)、状态流转。
package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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

// Pick 选出一个可用账号(无账号返回 nil,nil —— 免登录盘允许匿名)。
func (s *AccountService) Pick(pan string) (*repo.Account, *driver.Credential, error) {
	acc, err := s.store.PickAccount(pan)
	if err != nil || acc == nil {
		return nil, nil, err
	}
	cred, err := s.DecryptCred(acc)
	if err != nil {
		return nil, nil, err
	}
	return acc, cred, nil
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
func (s *AccountService) MarkStatus(accID uint, status string) {
	if acc, err := s.store.GetAccount(accID); err == nil && acc != nil {
		acc.Status = status
		_ = s.store.UpdateAccount(acc)
	}
	s.RefreshStatusMetrics()
}

// MarkCooldown 风控时给账号降温。
func (s *AccountService) MarkCooldown(accID uint, d time.Duration) {
	if acc, err := s.store.GetAccount(accID); err == nil && acc != nil {
		until := time.Now().Add(d)
		acc.CooldownUntil = &until
		acc.Status = "cooling"
		_ = s.store.UpdateAccount(acc)
	}
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
