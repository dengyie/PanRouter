// Package repo 是数据访问层:GORM 模型 + 查询,SQLite(WAL)单文件库。
package repo

import (
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---- 模型 ----

type Account struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	PanType       string     `gorm:"size:32;index" json:"pan_type"`
	Name          string     `gorm:"size:128" json:"name"`
	CredEnc       []byte     `json:"-"` // AES-GCM 密文
	CredVersion   int        `json:"cred_version"`
	Status        string     `gorm:"size:16;index" json:"status"` // ok|cooling|expired|banned
	CooldownUntil *time.Time `json:"cooldown_until"`
	LastCheckAt   *time.Time `json:"last_check_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Share struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	PanType    string    `gorm:"size:32;index" json:"pan_type"`
	ShareURL   string    `gorm:"size:512" json:"share_url"`
	Pwd        string    `gorm:"size:64" json:"pwd"`
	ShareKey   string    `gorm:"uniqueIndex;size:64" json:"share_key"`
	RawTree    string    `gorm:"type:text" json:"raw_tree"`
	ResolvedAt time.Time `json:"resolved_at"`
}

type Link struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	ShareKey   string    `gorm:"uniqueIndex:idx_share_fid;size:64" json:"share_key"`
	FID        string    `gorm:"column:fid;uniqueIndex:idx_share_fid;size:128" json:"fid"`
	FileName   string    `gorm:"size:512" json:"file_name"`
	Size       int64     `json:"size"`
	DirectLink string    `gorm:"type:text" json:"-"`
	UA         string    `gorm:"size:512" json:"ua"`
	Referer    string    `gorm:"size:512" json:"referer"`
	CookieEnc  []byte    `json:"-"`
	BindIP     bool      `json:"bind_ip"`
	ExpiresAt  time.Time `json:"expires_at"`
	Ext        string    `gorm:"type:text" json:"-"` // JSON: driver FileRef.Ext
	CreatedAt  time.Time `json:"created_at"`
}

type Download struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	GID       string    `gorm:"column:gid;index;size:64" json:"gid"`
	FID       string    `gorm:"column:fid;size:128" json:"fid"`
	Dest      string    `gorm:"size:512" json:"dest"`
	Route     string    `gorm:"size:16" json:"route"`
	Status    string    `gorm:"size:16" json:"status"`
	ErrMsg    string    `gorm:"size:512" json:"err_msg"`
	CreatedAt time.Time `json:"created_at"`
}

type Setting struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string `gorm:"type:text"`
}

type AuditLog struct {
	ID     uint      `gorm:"primaryKey"`
	Ts     time.Time `gorm:"index"`
	Action string    `gorm:"size:64"`
	Detail string    `gorm:"type:text"`
}

// ---- Store ----

type Store struct {
	db *gorm.DB
}

func Open(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		// SQL 错误由 repo 层显式处理并向上返回;record-not-found 属预期流程,不进应用日志
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;").Error; err != nil {
		return nil, fmt.Errorf("sqlite pragmas: %w", err)
	}
	if err := db.AutoMigrate(&Account{}, &Share{}, &Link{}, &Download{}, &Setting{}, &AuditLog{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) DB() *gorm.DB { return s.db }

// Close 关闭底层连接(测试与优雅停机用)。
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *Store) AddAudit(action, detail string) {
	s.db.Create(&AuditLog{Ts: time.Now(), Action: action, Detail: detail})
}

// ---- Share ----

func (s *Store) UpsertShare(sh *Share) error {
	var exist Share
	err := s.db.Where("share_key = ?", sh.ShareKey).First(&exist).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.Create(sh).Error
	}
	if err != nil {
		return err
	}
	sh.ID = exist.ID
	return s.db.Model(&exist).Updates(map[string]any{
		"raw_tree": sh.RawTree, "resolved_at": sh.ResolvedAt,
	}).Error
}

// ---- Link ----

func (s *Store) UpsertLink(l *Link) error {
	var exist Link
	err := s.db.Where("share_key = ? AND fid = ?", l.ShareKey, l.FID).First(&exist).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.Create(l).Error
	}
	if err != nil {
		return err
	}
	l.ID = exist.ID
	return s.db.Model(&exist).Updates(map[string]any{
		"file_name": l.FileName, "size": l.Size, "direct_link": l.DirectLink,
		"ua": l.UA, "referer": l.Referer, "cookie_enc": l.CookieEnc,
		"bind_ip": l.BindIP, "expires_at": l.ExpiresAt, "ext": l.Ext,
	}).Error
}

func (s *Store) GetLink(shareKey, fid string) (*Link, error) {
	var l Link
	err := s.db.Where("share_key = ? AND fid = ?", shareKey, fid).First(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ---- Account ----

func (s *Store) CreateAccount(a *Account) error { return s.db.Create(a).Error }

func (s *Store) ListAccounts(panType string) ([]Account, error) {
	var out []Account
	q := s.db.Order("id")
	if panType != "" {
		q = q.Where("pan_type = ?", panType)
	}
	err := q.Find(&out).Error
	return out, err
}

func (s *Store) GetAccount(id uint) (*Account, error) {
	var a Account
	err := s.db.First(&a, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) UpdateAccount(a *Account) error { return s.db.Save(a).Error }

func (s *Store) DeleteAccount(id uint) error { return s.db.Delete(&Account{}, id).Error }

// PickAccount 选出一个可用账号:状态 ok,或 cooling 且冷却已过(选中时自动复位)。
// 状态机:ok →(风控)cooling →(冷却到期)ok;不存在其他恢复路径,见设计文档 §7.7。
func (s *Store) PickAccount(panType string) (*Account, error) {
	var a Account
	now := time.Now()
	err := s.db.Where(
		"pan_type = ? AND (status = 'ok' OR (status = 'cooling' AND cooldown_until IS NOT NULL AND cooldown_until < ?))",
		panType, now,
	).Order("last_check_at DESC").First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.Status == "cooling" {
		a.Status = "ok"
		if uerr := s.db.Model(&a).Update("status", "ok").Error; uerr != nil {
			return nil, uerr
		}
	}
	return &a, nil
}

// ---- Setting ----

func (s *Store) GetSetting(key string) (string, bool) {
	var st Setting
	err := s.db.Where("key = ?", key).First(&st).Error
	if err != nil {
		return "", false
	}
	return st.Value, true
}

func (s *Store) SetSetting(key, value string) error {
	var st Setting
	err := s.db.Where("key = ?", key).First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.Create(&Setting{Key: key, Value: value}).Error
	}
	if err != nil {
		return err
	}
	st.Value = value
	return s.db.Model(&st).Update("value", value).Error
}

// ---- Download ----

func (s *Store) CreateDownload(d *Download) error { return s.db.Create(d).Error }

func (s *Store) GetDownload(gid string) (*Download, error) {
	var d Download
	err := s.db.Where("gid = ?", gid).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) ListDownloads() ([]Download, error) {
	var out []Download
	err := s.db.Order("id DESC").Limit(200).Find(&out).Error
	return out, err
}

func (s *Store) UpdateDownload(d *Download) error { return s.db.Save(d).Error }
