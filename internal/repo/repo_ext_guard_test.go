package repo

import (
	"encoding/json"
	"testing"
	"time"
)

// links.ext 的写保护回归(线上缺陷)。
//
// 缺陷:mangodrive 对超长路径生成不可逆哈希 fid("p:<sha1>"),Ext["path"] 是
// **唯一**能反推真实存储路径的地方。若某次解析没拿到上下文却把 ext 写成
// json.Marshal(nil map) 的字面量 "null",该行就退化成「有列无内容」的坏行:
//   - 写侧:覆盖掉了同 (share_key,fid) 行里已有的真实 path;
//   - 读侧:历史反查若命中这行,Unmarshal 得到 nil map,等于没恢复 →
//     裸哈希提链必然 404,而库里明明存在带 path 的可用历史行。
//
// 修复两处:① repo.UpsertLink:ext 无有效内容时**不更新该列**(保留原值);
//
//	② service 层:不再把 nil map 序列化成 "null" 落库(见 resolver.go)。
func TestUpsertLinkDoesNotClobberExtWithNull(t *testing.T) {
	s := newTestStore(t)

	const want = `{"api":"","path":"/Pool/real/a.bin"}`
	seed := &Link{ShareKey: "k", FID: "p:hash", FileName: "a.bin", Ext: want}
	if err := s.db.Create(seed).Error; err != nil {
		t.Fatal(err)
	}

	// 复现线上形态:ext 为 nil map → json.Marshal 产出字面量 "null"。
	var nilExt map[string]string
	raw, err := json.Marshal(nilExt)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("前提不成立:nil map 应序列化为 null,got %q", raw)
	}

	// 第二次解析没拿到上下文 → ext = "null"。
	if err := s.UpsertLink(&Link{
		ShareKey: "k", FID: "p:hash", FileName: "a.bin", Ext: string(raw),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetLink("k", "p:hash")
	if err != nil || got == nil {
		t.Fatalf("get link: %v %v", got, err)
	}
	if got.Ext != want {
		t.Fatalf("真实 path 被字面量 null 覆盖: got %q want %q", got.Ext, want)
	}
}

// 空串 ext 同样不得覆盖原值(空上下文是「没有信息」,不是「清空信息」)。
func TestUpsertLinkDoesNotClobberExtWithEmpty(t *testing.T) {
	s := newTestStore(t)
	const want = `{"api":"","path":"/Pool/real/a.bin"}`
	if err := s.db.Create(&Link{ShareKey: "k", FID: "p:hash", FileName: "a.bin", Ext: want}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertLink(&Link{ShareKey: "k", FID: "p:hash", FileName: "a.bin", Ext: ""}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLink("k", "p:hash")
	if err != nil || got == nil {
		t.Fatalf("get link: %v %v", got, err)
	}
	if got.Ext != want {
		t.Fatalf("真实 path 被空串覆盖: got %q want %q", got.Ext, want)
	}
}

// 有有效内容时必须正常覆盖(ext 是可更新的,不能被写保护变成「只增不减」)。
func TestUpsertLinkStillUpdatesValidExt(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Link{ShareKey: "k", FID: "p:hash", FileName: "a.bin",
		Ext: `{"api":"","path":"/Pool/old/a.bin"}`}).Error; err != nil {
		t.Fatal(err)
	}
	const want = `{"api":"","path":"/Pool/new/a.bin"}`
	if err := s.UpsertLink(&Link{ShareKey: "k", FID: "p:hash", FileName: "a.bin", Ext: want}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLink("k", "p:hash")
	if err != nil || got == nil {
		t.Fatalf("get link: %v %v", got, err)
	}
	if got.Ext != want {
		t.Fatalf("有效 ext 未被更新: got %q want %q", got.Ext, want)
	}
}

// extHasContent 的边界判定。
func TestExtHasContent(t *testing.T) {
	cases := []struct {
		ext  string
		want bool
	}{
		{"", false},
		{"null", false},
		{"{}", false},
		{"   ", false},
		{`{"api":"","path":"/a.bin"}`, true},
		{`{"pwd":"x"}`, true},
	}
	for _, c := range cases {
		if got := extHasContent(c.ext); got != c.want {
			t.Fatalf("extHasContent(%q) = %v want %v", c.ext, got, c.want)
		}
	}
}

// 历史反查必须能越过「坏行」拿到下面那条带 path 的行。
// 这是 ListLinksByFID 存在的理由:只取最新一行会在坏行上翻车。
func TestListLinksByFIDReturnsBadAndGoodRowsOrdered(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	rows := []struct {
		share string
		ext   string
	}{
		{"old-good", `{"api":"","path":"/Pool/real/a.bin"}`},
		{"bad", "null"},
	}
	for i, r := range rows {
		l := &Link{
			ShareKey: r.share, FID: "p:hash", FileName: "a.bin",
			Ext: r.ext, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := s.db.Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListLinksByFID("p:hash", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("应返回 2 行,got %d", len(all))
	}
	// 新的在前:坏行在前,可用行在后。
	if all[0].Ext != "null" {
		t.Fatalf("第 1 行应为最新的坏行(ext=null),got %q", all[0].Ext)
	}
	if all[1].Ext != `{"api":"","path":"/Pool/real/a.bin"}` {
		t.Fatalf("第 2 行应为带 path 的可用行,got %q", all[1].Ext)
	}
}
