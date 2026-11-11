package repo

import (
	"encoding/json"
	"testing"
	"time"
)

// shares.raw_tree 的写保护回归(线上缺陷,与 links.ext 同型)。
//
// 缺陷:raw_tree 是分享快照 —— driver 解析出的节点列表 JSON,里面带着
// quark 的 pwd_id/stoken、lanzou 的 share_url/pwd 等**唯一**的分享上下文。
// 一次「没拿到节点」的解析(json.Marshal(nil slice) → 字面量 "null")若
// 无条件覆盖,就把该分享的快照清成空壳:读回来 Unmarshal 得到 nil 节点列表,
// 于是该分享下**所有**文件都提不了链(driver 报「缺少夸克分享上下文」)。
//
// 与 links.ext 的差别在于后果不可恢复:links.ext 坏了还能从同 fid 的历史行
// 反查(只要还有别的行),而快照是该分享上下文的唯一来源(quark auto-link
// 配额只有 2,第 3 个及以后的文件只可能从快照拿上下文)。
//
// 修复:① repo.UpsertShare:raw_tree 无有效内容时**不更新该列**;
// ② service 层:nil 节点列表规范成 "[]" 而不是 "null"(见 resolver.go)。
func TestUpsertShareDoesNotClobberRawTreeWithNull(t *testing.T) {
	s := newTestStore(t)

	const want = `[{"fid":"f1","name":"a.bin","size":10,"ext":{"pwd_id":"pid-1"}}]`
	if err := s.db.Create(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: want, ResolvedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}

	// 前提:nil slice 序列化出的确是字面量 "null"。
	var nilNodes []struct{}
	raw, err := json.Marshal(nilNodes)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("前提不成立:nil slice 应序列化为 null,got %q", raw)
	}

	// 第二次解析没拿到节点 → raw_tree = "null"。
	if err := s.UpsertShare(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: string(raw), ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetShare("k")
	if err != nil || got == nil {
		t.Fatalf("get share: %v %v", got, err)
	}
	if got.RawTree != want {
		t.Fatalf("真实快照被字面量 null 覆盖: got %q want %q", got.RawTree, want)
	}
}

// 空数组 "[]" 同样不得覆盖原值("[]" 与 "null" 一样是「有列无内容」:
// 分享真空了与「没解析出来」无法区分,一律视为无内容)。
func TestUpsertShareDoesNotClobberRawTreeWithEmptyArray(t *testing.T) {
	s := newTestStore(t)
	const want = `[{"fid":"f1","name":"a.bin","size":10,"ext":{"pwd_id":"pid-1"}}]`
	if err := s.db.Create(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: want, ResolvedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertShare(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: "[]", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetShare("k")
	if err != nil || got == nil {
		t.Fatalf("get share: %v %v", got, err)
	}
	if got.RawTree != want {
		t.Fatalf("真实快照被空数组覆盖: got %q want %q", got.RawTree, want)
	}
}

// 空串同样不得覆盖原值。
func TestUpsertShareDoesNotClobberRawTreeWithEmpty(t *testing.T) {
	s := newTestStore(t)
	const want = `[{"fid":"f1","name":"a.bin","size":10,"ext":{"pwd_id":"pid-1"}}]`
	if err := s.db.Create(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: want, ResolvedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertShare(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: "", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetShare("k")
	if err != nil || got == nil {
		t.Fatalf("get share: %v %v", got, err)
	}
	if got.RawTree != want {
		t.Fatalf("真实快照被空串覆盖: got %q want %q", got.RawTree, want)
	}
}

// 有有效内容时必须正常覆盖(快照是「每次解析刷新」的,写保护不能变成「只增不减」)。
func TestUpsertShareStillUpdatesValidRawTree(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: `[{"fid":"f1","ext":{"pwd_id":"old"}}]`,
		ResolvedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	const want = `[{"fid":"f1","ext":{"pwd_id":"new"}}]`
	if err := s.UpsertShare(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k", RawTree: want, ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetShare("k")
	if err != nil || got == nil {
		t.Fatalf("get share: %v %v", got, err)
	}
	if got.RawTree != want {
		t.Fatalf("有效快照未被更新: got %q want %q", got.RawTree, want)
	}
}

// 首次落库(不存在旧行)时按原样写入 —— 写保护只针对「覆盖已有好快照」。
// 这一条是线上历史坏行的来源形态:库里确实存在 raw_tree='null'/'[]' 的行。
func TestUpsertShareCreatesRowWithEmptyShellWhenNew(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertShare(&Share{
		PanType: "quark", ShareURL: "https://pan.quark.cn/s/x",
		ShareKey: "k-new", RawTree: "null", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetShare("k-new")
	if err != nil || got == nil {
		t.Fatalf("get share: %v %v", got, err)
	}
	if got.RawTree != "null" {
		t.Fatalf("新行应原样写入(不得被写保护静默丢弃),got %q", got.RawTree)
	}
}

// RawTreeHasContent 的边界判定。
func TestRawTreeHasContent(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"null", false},
		{"[]", false},
		{`[{"fid":"f1"}]`, true},
		{`[{"fid":"f1","ext":{"pwd_id":"p"}}]`, true},
	}
	for _, c := range cases {
		if got := RawTreeHasContent(c.raw); got != c.want {
			t.Fatalf("RawTreeHasContent(%q) = %v want %v", c.raw, got, c.want)
		}
	}
}

// ListSharesByPan 必须只返回同一网盘的快照行。
//
// 读侧坏快照自愈靠它跨 share_key 找上下文,而 share_key = sha1(pan|url|pwd),
// 同一份分享的不同 URL 形态(根链 vs 带 #/list/share/yyy 的深链)是两个 key。
// 不同网盘的 fid 空间互不相通,跨网盘取上下文必然串味(quark 的 pwd_id 会被
// 塞进 lanzou 的请求里),故 pan_type 必须严格隔离。
func TestListSharesByPanIsolatesPanTypes(t *testing.T) {
	s := newTestStore(t)
	rows := []struct {
		pan string
		key string
		raw string
	}{
		{"quark", "q1", `[{"fid":"f1","ext":{"pwd_id":"pid"}}]`},
		{"quark", "q2", "null"},
		{"lanzou", "l1", `[{"fid":"f1","ext":{"share_url":"u"}}]`},
	}
	for _, r := range rows {
		if err := s.db.Create(&Share{
			PanType: r.pan, ShareURL: "https://x/" + r.key,
			ShareKey: r.key, RawTree: r.raw, ResolvedAt: time.Now(),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListSharesByPan("quark")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("quark 应有 2 行,got %d", len(got))
	}
	for _, g := range got {
		if g.PanType != "quark" {
			t.Fatalf("串味:返回了非 quark 行 %+v", g)
		}
	}
	// 空 pan_type 不应返回全表。
	if got, err := s.ListSharesByPan(""); err != nil || len(got) != 0 {
		t.Fatalf("空 pan_type 应返回空集,got %v %v", got, err)
	}
	// 不存在的网盘返回空集,不报错。
	if got, err := s.ListSharesByPan("nope"); err != nil || len(got) != 0 {
		t.Fatalf("不存在的网盘应返回空集,got %v %v", got, err)
	}
}
