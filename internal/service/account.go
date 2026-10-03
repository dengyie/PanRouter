// AccountService:凭据生命周期——加解密、互斥刷新(singleflight)、状态流转。
package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/repo"
)

type AccountService struct {
	store *repo.Store
	aes   *crypto.AES
	log   *loggerType

	sfMu sync.Mutex
	sf   singleflight.Group
}

func NewAccountService(store *repo.Store, aes *crypto.AES, log *loggerType) *AccountService {
	return &AccountService{store: store, aes: aes, log: log}
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
// 避免一次性 refresh token 并发双刷导致互踢(设计文档 §7.6,强制项)。
func (s *AccountService) Check(ctx context.Context, accID uint, drv driver.Driver) (driver.CredStatus, error) {
	acc, err := s.store.GetAccount(accID)
	if err != nil || acc == nil {
		return driver.CredStatus{}, fmt.Errorf("account %d not found", accID)
	}
	v, err, _ := s.sf.Do(fmt.Sprintf("acc:%d", acc.ID), func() (any, error) {
		cred, err := s.DecryptCred(acc)
		if err != nil {
			return driver.CredStatus{Valid: false, Message: err.Error()}, nil
		}
		st, err := drv.CheckCredential(ctx, *cred)
		if err != nil {
			return driver.CredStatus{}, err
		}
		now := time.Now()
		if st.Valid {
			acc.Status = "ok"
		} else {
			acc.Status = "expired"
		}
		acc.LastCheckAt = &now
		acc.CredVersion++
		if err := s.store.UpdateAccount(acc); err != nil {
			return st, err
		}
		s.store.AddAudit("cred_check", fmt.Sprintf("account=%d pan=%s valid=%v", acc.ID, acc.PanType, st.Valid))
		s.log.Infof("credential checked: account=%d pan=%s valid=%v", acc.ID, acc.PanType, st.Valid)
		return st, nil
	})
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
}

// MarkCooldown 风控时给账号降温。
func (s *AccountService) MarkCooldown(accID uint, d time.Duration) {
	if acc, err := s.store.GetAccount(accID); err == nil && acc != nil {
		until := time.Now().Add(d)
		acc.CooldownUntil = &until
		acc.Status = "cooling"
		_ = s.store.UpdateAccount(acc)
	}
}
