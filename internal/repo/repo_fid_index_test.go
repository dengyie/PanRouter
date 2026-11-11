package repo

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// FindLinkByFID 的查询计划护栏（P1 回归）。
//
// 背景:哈希 fid 的全局反查走 links 表。索引最初只有 (fid, created_at),
// 而 GORM First() 会强制追加主键 tiebreaker(ORDER BY created_at DESC, id),
// 该 tiebreaker 不在索引里 → SQLite 仍要 "USE TEMP B-TREE FOR RIGHT PART OF ORDER BY",
// 即索引只把排序范围从全表缩到同 fid 子集,并未消除临时 B 树。
//
// 修复:查询改用 Take + 显式 Order("created_at DESC, id DESC")。
// 索引维持 (fid, created_at) 两列——SQLite 索引隐含 rowid,已覆盖 id 取值,
// 决定成败的是 tiebreaker **方向**而非列数。本测试锁定该结论:
// ① 查询计划必须走 idx_fid_created 且不含 TEMP B-TREE;
// ② 返回行为:同 fid 取 created_at 最大者,created_at 并列取 id 最大者。
func TestFindLinkByFIDUsesIndexWithoutTempBTree(t *testing.T) {
	s := newTestStore(t)

	// 造同一 fid 的多行:created_at 递增,id 随之递增。
	fid := "p:0123456789abcdef0123456789abcdef01234567"
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		l := &Link{
			ShareKey:  fmt.Sprintf("k%d", i),
			FID:       fid,
			FileName:  fmt.Sprintf("f%d.bin", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Ext:       fmt.Sprintf(`{"path":"/p/f%d.bin"}`, i),
		}
		if err := s.db.Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 干扰行:其它 fid,行数更多,确保查询不会退化成全表扫。
	for i := 0; i < 50; i++ {
		l := &Link{
			ShareKey:  "noise",
			FID:       fmt.Sprintf("p:noise%04d", i),
			FileName:  "noise.bin",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := s.db.Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}

	// ① 查询计划断言:必须走 idx_fid_created 且无 TEMP B-TREE。
	//    EXPLAIN 的是**GORM 真实发出的 SQL**(DryRun 会话捕获),不是手抄近似——
	//    手抄版约束不到 GORM 是否偷偷追加主键 tiebreaker,正是 P1 的漏网之处。
	dry := s.db.Session(&gorm.Session{DryRun: true})
	stmt := s.linkByFIDQuery(dry, fid).Take(&Link{}).Statement
	realSQL := stmt.SQL.String()
	if realSQL == "" {
		t.Fatal("未能捕获 GORM 实际 SQL")
	}
	t.Logf("实际 SQL: %s", realSQL)

	// 关键断言:排序必须是 created_at DESC, **id DESC**。
	// GORM First() 追加的 tiebreaker 是「id」(**升序**),与索引的降序扫描方向相反
	// → SQLite 必须 "USE TEMP B-TREE FOR RIGHT PART OF ORDER BY"(SQLite 实测:
	// 同一索引下 ORDER BY created_at DESC, id DESC 无临时 B 树,而
	// ORDER BY created_at DESC, id(ASC) 有)。这才是 P1 的根因:
	// 方向不一致,不是缺列(SQLite 索引隐含 rowid,已覆盖 id 列)。
	if !strings.Contains(strings.ToLower(realSQL), "order by created_at desc, id desc") {
		t.Fatalf("排序必须是 created_at DESC, id DESC(与索引降序扫描同向,否则生临时 B 树),实际 SQL: %s", realSQL)
	}
	// 不得用 First():它会追加 id ASC 的主键 tiebreaker。
	if strings.Contains(strings.ToLower(realSQL), "order by created_at desc, id limit") {
		t.Fatalf("不得出现 First() 形态的 id 升序 tiebreaker: %s", realSQL)
	}

	// EXPLAIN 必须在「只有 (fid, created_at) 两列索引」的真实形态下评估:
	// SQLite 索引隐含 rowid,故 id 列天然可覆盖,三列索引与两列等价,
	// 决定成败的是 tiebreaker 方向。若库中存在多个候选索引,planner 可能
	// 挑走另一条而掩盖问题,故单独建一个只含该索引的库来 EXPLAIN。
	planDB, err := Open(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer planDB.Close()
	if err := planDB.db.Exec("DROP INDEX IF EXISTS idx_fid_created").Error; err != nil {
		t.Fatal(err)
	}
	if err := planDB.db.Exec("CREATE INDEX idx_fid_created ON links(fid, created_at)").Error; err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		ID     int    `gorm:"column:id"`
		Parent int    `gorm:"column:parent"`
		Detail string `gorm:"column:detail"`
	}
	if err := planDB.db.Raw("EXPLAIN QUERY PLAN "+realSQL, fid).Scan(&plans).Error; err != nil {
		t.Fatal(err)
	}
	if len(plans) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN 无输出")
	}
	var joined strings.Builder
	for _, p := range plans {
		joined.WriteString(p.Detail)
		joined.WriteString(" | ")
	}
	plan := joined.String()
	if !strings.Contains(plan, "idx_fid_created") {
		t.Fatalf("反查必须走 idx_fid_created,实际计划: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("反查不得出现 TEMP B-TREE(主键 tiebreaker 排序方向未被索引覆盖),实际计划: %s", plan)
	}

	// ② 返回行为:created_at 最大者;并列时 id 最大者。
	got, err := s.FindLinkByFID(fid)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("应命中历史行")
	}
	if got.FileName != "f4.bin" {
		t.Fatalf("应取 created_at 最大者 f4.bin,got %s", got.FileName)
	}

	// created_at 并列 → 取 id 最大者(后写入的那条)。
	// 注意 (share_key, fid) 是唯一键:同一 fid 必须配不同 share_key 才能多行。
	same := base.Add(10 * time.Minute)
	for i := 0; i < 3; i++ {
		l := &Link{
			ShareKey:  fmt.Sprintf("tie%d", i),
			FID:       fid,
			FileName:  fmt.Sprintf("tie%d.bin", i),
			CreatedAt: same,
			Ext:       fmt.Sprintf(`{"path":"/p/tie%d.bin"}`, i),
		}
		if err := s.db.Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.FindLinkByFID(fid)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FileName != "tie2.bin" {
		t.Fatalf("created_at 并列应取 id 最大者 tie2.bin,got %+v", got)
	}

	// 边界:空 fid 与不存在的 fid 都返回 (nil, nil),不报错。
	if l, err := s.FindLinkByFID(""); err != nil || l != nil {
		t.Fatalf("空 fid 应返回 (nil,nil),got %v %v", l, err)
	}
	if l, err := s.FindLinkByFID("p:doesnotexist"); err != nil || l != nil {
		t.Fatalf("不存在的 fid 应返回 (nil,nil),got %v %v", l, err)
	}
}
