// Package repo 是数据访问层:GORM 模型 + 查询,SQLite(WAL)单文件库。
package repo

import (
	"errors"
	"fmt"
	"strings"
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
	// ID 主键。SQLite 索引隐含 rowid,故 (fid, created_at) 两列索引已能覆盖
	// id 列的取值;真正的排序消除靠的是 tiebreaker 方向(id DESC)而非多一列。
	ID       uint   `gorm:"primaryKey" json:"id"`
	ShareKey string `gorm:"uniqueIndex:idx_share_fid;size:64" json:"share_key"`
	// FID 双索引:与 ShareKey 组成业务唯一键;复合索引 (fid, created_at) 支撑
	// ListLinksByFID 的哈希 fid 全局反查——过滤与 created_at 排序全走索引。
	// 消除临时 B 树的关键是 tiebreaker **方向**:GORM First() 追加的是 id 升序,
	// 与索引降序扫描相反 → SQLite 仍要 "USE TEMP B-TREE FOR RIGHT PART OF ORDER BY";
	// 改用显式 "created_at DESC, id DESC"(与索引同向)后彻底消除。
	// 回归见 repo_fid_index_test.go(EXPLAIN 断言真实 SQL 无 TEMP B-TREE)。
	FID        string `gorm:"column:fid;uniqueIndex:idx_share_fid;index:idx_fid_created,priority:1;size:128" json:"fid"`
	FileName   string `gorm:"size:512" json:"file_name"`
	Size       int64  `json:"size"`
	DirectLink string `gorm:"type:text" json:"-"`
	// ServerLink 服务端自取直链(转存用);空 = 复用 DirectLink。
	ServerLink string    `gorm:"type:text" json:"-"`
	UA         string    `gorm:"size:512" json:"ua"`
	Referer    string    `gorm:"size:512" json:"referer"`
	CookieEnc  []byte    `json:"-"`
	BindIP     bool      `json:"bind_ip"`
	ExpiresAt  time.Time `json:"expires_at"`
	Ext        string    `gorm:"type:text" json:"-"` // JSON: driver FileRef.Ext
	CreatedAt  time.Time `gorm:"index:idx_fid_created,priority:2" json:"created_at"`
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

// SyncItem 设备同步条目:桌面端把收藏/解析历史/下载记录以不透明 JSON payload 上报,
// 服务端只做存储与 LWW 仲裁,不解析 payload 语义(两仓零共享代码边界)。
type SyncItem struct {
	ID              uint      `gorm:"primaryKey" json:"id"`                         // 全局变更序号:每次变更 DELETE+INSERT 产生新 rowid,pull 游标严格单调
	Kind            string    `gorm:"size:24;uniqueIndex:idx_kind_uid" json:"kind"` // bookmark | link_history | download_record
	UID             string    `gorm:"size:64;uniqueIndex:idx_kind_uid" json:"uid"`  // 客户端生成的稳定 UUID
	DeviceID        string    `gorm:"size:64;index" json:"device_id"`
	Payload         string    `gorm:"type:text" json:"payload"`
	Deleted         bool      `json:"deleted"`           // 墓碑:删除也要传播
	ClientUpdatedAt int64     `json:"client_updated_at"` // 客户端时钟毫秒,LWW 仲裁键
	ServerUpdatedAt time.Time `json:"server_updated_at"`
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
	if err := db.AutoMigrate(&Account{}, &Share{}, &Link{}, &Download{}, &Setting{}, &AuditLog{}, &SyncItem{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Ping 探测底层连接可用性(readyz 用);api 层经此收口,不直接依赖 gorm.DB。
func (s *Store) Ping() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("get sql db: %w", err)
	}
	return sqlDB.Ping()
}

// ClearLinks 清空直链缓存(测试隔离用)。
func (s *Store) ClearLinks() error {
	return s.db.Where("1 = 1").Delete(&Link{}).Error
}

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

// UpsertShare 落库分享快照。
//
// ★ raw_tree 的写保护(与 links.ext 同型):raw_tree 是 driver 节点列表的 JSON,
// 一次「没拿到节点」的解析(json.Marshal(nil slice) → 字面量 "null")若
// 无条件覆盖,就把该分享**唯一**的快照清成空壳 —— 而快照里带着
// quark 的 pwd_id/stoken 等上下文,没有它该分享下所有文件都提不了链
// (「缺少夸克分享上下文」)。故:raw_tree 无有效内容时**不更新该列**。
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
	updates := map[string]any{"resolved_at": sh.ResolvedAt}
	// 快照有内容才覆盖:空壳快照("null"/"[]"/空串)覆盖好快照 = 把可恢复的
	// 上下文毁掉。首次落库(不存在旧行)时仍按原样写入,故 Create 分支不受此限。
	if rawTreeHasContent(sh.RawTree) {
		updates["raw_tree"] = sh.RawTree
	}
	return s.db.Model(&exist).Updates(updates).Error
}

// RawTreeHasContent 判断分享快照是否携带有效节点内容:非空、非 JSON null、非空数组。
// "null" 是 json.Marshal(nil slice) 的产物 —— 它作为「有列」存在,读回来时
// Unmarshal 得到 nil slice,等于没有内容(线上历史数据里确有此形态)。
// "[]" 同样是「有列无内容」:分享真的空,和「没解析出来」无法区分,一律视为无内容。
//
// 导出供 service 层复用(读侧坏快照自愈要按同一口径挑「有内容」的快照行),
// 避免两处口径漂移。
func RawTreeHasContent(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && s != "null" && s != "[]"
}

// rawTreeHasContent 是 RawTreeHasContent 的包内别名(写保护处调用)。
func rawTreeHasContent(s string) bool {
	return RawTreeHasContent(s)
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
	updates := map[string]any{
		"file_name": l.FileName, "size": l.Size, "direct_link": l.DirectLink,
		"server_link": l.ServerLink,
		"ua":          l.UA, "referer": l.Referer, "cookie_enc": l.CookieEnc,
		"bind_ip": l.BindIP, "expires_at": l.ExpiresAt,
	}
	// ★ ext 的写保护:ext 是 driver 私有上下文,对 mangodrive 而言 Ext["path"]
	//   是**唯一**能反推真实存储路径的地方(哈希 fid 不可逆,路径恢复只能从
	//   历史缓存行里捞)。本次解析若没拿到上下文(ext 为空 / 字面量 "null"),
	//   直接覆盖会把库里已有的 path 永久抹掉 → 该 fid 的裸哈希提链从此必然 404。
	//   故:ext 无有效内容时**不更新该列**(保留原值),有内容才覆盖。
	if extHasContent(l.Ext) {
		updates["ext"] = l.Ext
	}
	// ★ file_name/size 的写保护(与 ext 同型):「兜底名『文件』+0」是查不到
	//   元数据时的占位值,若本次解析元数据缺失而历史行里有真实名字/大小,
	//   无条件覆盖会把真实值永久抹成占位 —— 读侧还得靠它展示。
	//   故:占位元数据不覆盖已有真实值;真实值照常覆盖(含真实值互相覆盖)。
	if LinkMetaIsPlaceholder(l.FileName, l.Size) && !LinkMetaIsPlaceholder(exist.FileName, exist.Size) {
		updates["file_name"] = exist.FileName
		updates["size"] = exist.Size
	}
	return s.db.Model(&exist).Updates(updates).Error
}

// PlaceholderFileName 是「查不到元数据」时的兜底展示名。单独出现不一定是占位
// (真实文件可能恰好叫「文件」),与 size<=0 组合才是占位判据 —— 见
// LinkMetaIsPlaceholder。
const PlaceholderFileName = "文件"

// LinkMetaIsPlaceholder 判断一组文件名/大小是否为「查不到元数据」的占位值。
// 写侧(UpsertLink 的覆盖保护)与读侧(service 的 fileMeta/历史回退/缓存命中)
// 共用同一口径,防止占位值把真实元数据永久遮蔽或覆盖(防两处口径漂移)。
func LinkMetaIsPlaceholder(name string, size int64) bool {
	return name == "" || (name == PlaceholderFileName && size <= 0)
}

// extHasContent 判断 ext 是否携带有效上下文:非空、非 JSON null、非空对象。
// "null" 是 json.Marshal(nil map) 的产物——它作为「有列」存在,读回来时
// Unmarshal 得到 nil map,等于没有内容(线上历史数据里确有此形态)。
func extHasContent(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && s != "null" && s != "{}"
}

// GetLink 按 (share_key, fid) 读取直链缓存。
// 契约:不存在返回 (nil, nil)(cache miss 语义),DB 故障返回 error(调用方不得当 miss 吞掉)。
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

// ListLinksByFID 跨 share_key 按 fid 列出直链缓存历史行(最新的在前)。
//
// 用途:mangodrive 等驱动对长路径生成哈希 fid("p:<sha1>"),哈希不可逆——
// 调用方只拿到 fid 时(如客户端由 parentDirOf 推不出父目录、直接以 fid 提链),
// 只能从历史缓存行的 Ext["path"] 里恢复真实路径。
//
// ★ 为什么返回列表而不是只给一行:同一 fid 会有多行(每行对应一个缓存过它的
// 分享上下文),其中可能混有「有列无内容」的坏行(线上历史数据里 ext 被写成
// 字面量 "null" 即属此类)。只取最新一行会在坏行上翻车 —— 明明库里存在带
// path 的可用历史行,恢复却失败,裸哈希提链必然 404。
// 故这里只负责「按索引顺序把行摆出来」,挑哪一行由调用方决定。
//
// 排序口径:created_at DESC, id DESC(并列时取后写入的那条)。
// 不用 First():它会强制追加主键 tiebreaker 且方向为 id ASC,与索引
// (fid, created_at) 的降序扫描不匹配 → EXPLAIN 实测仍出现
// "USE TEMP B-TREE FOR RIGHT PART OF ORDER BY"。显式 Order 消除该临时排序。
func (s *Store) ListLinksByFID(fid string, limit int) ([]Link, error) {
	if fid == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1
	}
	var out []Link
	if err := s.linkByFIDQuery(s.db, fid).Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// linkByFIDQuery 构造 FindLinkByFID 的查询。单独抽出是为了让回归测试能用
// DryRun 会话取到**真实发出的 SQL**(而不是手抄一份近似 SQL 去 EXPLAIN——
// 手抄版无法约束 GORM 是否偷偷追加主键 tiebreaker,正是 P1 的漏网之处)。
func (s *Store) linkByFIDQuery(db *gorm.DB, fid string) *gorm.DB {
	return db.Where("fid = ?", fid).
		Order("created_at DESC, id DESC")
}

// GetShare 按 share_key 读取分享快照。
// 契约:不存在返回 (nil, nil),DB 故障返回 error。
func (s *Store) GetShare(shareKey string) (*Share, error) {
	var sh Share
	err := s.db.Where("share_key = ?", shareKey).First(&sh).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sh, nil
}

// ListSharesByPan 列出该网盘的全部分享快照(跨 share_key 的上下文自愈用)。
//
// ★ 为什么需要它:share_key = sha1(pan|url|pwd),**URL 形态不同即不同 key** ——
// 同一份夸克分享,「/s/xxx」与「/s/xxx#/list/share/yyy」是两个 key、两条快照。
// 于是「某条快照被写坏」不等于「该分享的上下文彻底丢失」:另一形态的快照里
// 往往还留着完整的 pwd_id/stoken。这是坏快照唯一的自愈来源。
//
// 只在同一 pan_type 内扫:不同网盘的 fid 空间互不相通,跨网盘取上下文必然串味。
// 快照行数量级很小(每个分享上下文一行),按 pan_type 取回即可。
// 固定按 id 升序返回;自愈侧逆序消费(最新快照的上下文最新,见 healShareNode)。
func (s *Store) ListSharesByPan(panType string) ([]Share, error) {
	if panType == "" {
		return nil, nil
	}
	var out []Share
	if err := s.db.Where("pan_type = ?", panType).Order("id").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ExpireLink 立即将某条直链标记为过期(上游否决时调用),使下次 GetFreshLink 触发重解析。
func (s *Store) ExpireLink(shareKey, fid string) error {
	return s.db.Model(&Link{}).
		Where("share_key = ? AND fid = ?", shareKey, fid).
		Update("expires_at", time.Now()).Error
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

func (s *Store) UpdateAccount(a *Account) error {
	return s.db.Model(&Account{}).Where("id = ?", a.ID).Updates(map[string]any{
		"pan_type":       a.PanType,
		"name":           a.Name,
		"cred_enc":       a.CredEnc,
		"cred_version":   a.CredVersion,
		"status":         a.Status,
		"cooldown_until": a.CooldownUntil,
		"last_check_at":  a.LastCheckAt,
	}).Error
}

func (s *Store) UpdateAccountCheck(id uint, status string, checkedAt time.Time, expectedStatus string) error {
	// cred_version 是凭据代际(§15.3):仅凭据内容变更时经 UpdateAccount 递增,
	// 健康检查只流转状态与 last_check_at,不得制造虚假版本变更。
	result := s.db.Model(&Account{}).Where("id = ? AND status = ?", id, expectedStatus).Updates(map[string]any{
		"status":        status,
		"last_check_at": checkedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *Store) DeleteAccount(id uint) error { return s.db.Delete(&Account{}, id).Error }

// MarkAccountStatus 只更新 status 列:调用方(resolver 标记 AuthExpired 等)
// 不应经手整行,避免用过期快照把并发改动的 cooldown_until/cred_version 一起冲掉。
func (s *Store) MarkAccountStatus(id uint, status string) error {
	return s.db.Model(&Account{}).Where("id = ?", id).Update("status", status).Error
}

// MarkAccountCooldown 只更新冷却涉及的列(状态 + 到期时间)。
func (s *Store) MarkAccountCooldown(id uint, until time.Time) error {
	return s.db.Model(&Account{}).Where("id = ?", id).Updates(map[string]any{
		"status": "cooling", "cooldown_until": until,
	}).Error
}

// PickAccount 选出一个可用账号:状态 ok,或 cooling 且冷却已过(选中时自动复位)。
// 状态机:ok →(风控)cooling →(冷却到期)ok;不存在其他恢复路径。
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

// UpdateDownload 只更新状态轮询涉及的列。
// 原 s.db.Save(d) 整行回写:轮询协程拿到的行快照与推送协程的写入并发时,
// 后写者会把前者的 dest_path 等列用**过期值**冲掉(丢更新)。
func (s *Store) UpdateDownload(d *Download) error {
	return s.db.Model(&Download{}).Where("gid = ?", d.GID).Updates(map[string]any{
		"status": d.Status, "err_msg": d.ErrMsg,
	}).Error
}

// ---- SyncItem ----

// UpsertSyncItem 同步条目落库:LWW 仲裁——库内 client_updated_at 更新则跳过(返回 applied=false);
// 否则事务内 DELETE+INSERT,保证每次生效的变更都产生新 rowid(=全局变更序号,pull 游标用)。
// applied 时 item.ID 被回填为落库后的新序号;跳过时回填为现有行 id,便于调用方对账。
func (s *Store) UpsertSyncItem(item *SyncItem) (applied bool, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var old SyncItem
		e := tx.Where("kind = ? AND uid = ?", item.Kind, item.UID).First(&old).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			item.ID = 0
			item.ServerUpdatedAt = time.Now()
			if e = tx.Create(item).Error; e != nil {
				return e
			}
			applied = true
			return nil
		}
		if e != nil {
			return e
		}
		if old.ClientUpdatedAt > item.ClientUpdatedAt {
			item.ID = old.ID // 旧记录胜出,丢弃来条(仲裁结果回填)
			return nil
		}
		item.ServerUpdatedAt = time.Now()
		if e = tx.Delete(&SyncItem{}, old.ID).Error; e != nil {
			return e
		}
		item.ID = 0
		if e = tx.Create(item).Error; e != nil {
			return e
		}
		applied = true
		return nil
	})
	return applied, err
}

// ListSyncItemsAfter 按 pull 游标取变更(id > since,升序)。
func (s *Store) ListSyncItemsAfter(since uint, limit int) ([]SyncItem, error) {
	var out []SyncItem
	err := s.db.Where("id > ?", since).Order("id ASC").Limit(limit).Find(&out).Error
	return out, err
}

// MaxSyncItemID 当前最大变更序号(客户端游标越过服务端时对齐用)。
func (s *Store) MaxSyncItemID() (uint, error) {
	var maxID uint
	err := s.db.Model(&SyncItem{}).Select("COALESCE(MAX(id),0)").Scan(&maxID).Error
	return maxID, err
}
