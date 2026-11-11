package repo

import (
	"testing"
	"time"
)

func futureTime() time.Time { return time.Now().Add(time.Hour) }

// v2.11 回归:file_name/size 的占位值写保护(与 v2.9 ext 写保护同型)。
//
// 缺陷:「兜底名『文件』+0」是查不到元数据时的占位值,UpsertLink 无条件覆盖
// file_name/size —— 一次元数据缺失的解析就把库里已有的真实名字/大小永久抹成
// 占位,读侧(fileMeta)还得靠它给客户端命名与预分配。
func TestUpsertLinkDoesNotClobberMetaWithPlaceholder(t *testing.T) {
	s := newTestStore(t)

	seed := &Link{ShareKey: "k", FID: "f1", FileName: "real.bin", Size: 4096}
	if err := s.db.Create(seed).Error; err != nil {
		t.Fatal(err)
	}

	// 复现:第二次解析元数据缺失 → 兜底名 + 0
	if err := s.UpsertLink(&Link{
		ShareKey: "k", FID: "f1", FileName: PlaceholderFileName, Size: 0,
		DirectLink: "https://up.invalid/x", ExpiresAt: futureTime(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLink("k", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.FileName != "real.bin" || got.Size != 4096 {
		t.Fatalf("占位元数据覆盖了真实值: name=%q size=%d", got.FileName, got.Size)
	}
	// 直链等其余列照常更新(占位保护只针对元数据两列)
	if got.DirectLink != "https://up.invalid/x" {
		t.Fatalf("直链列应照常更新, got %q", got.DirectLink)
	}
}

// 真实元数据必须能覆盖(包括覆盖占位行与真实值互相覆盖)。
func TestUpsertLinkStillUpdatesRealMeta(t *testing.T) {
	s := newTestStore(t)

	if err := s.db.Create(&Link{ShareKey: "k", FID: "f1", FileName: PlaceholderFileName, Size: 0}).Error; err != nil {
		t.Fatal(err)
	}
	// 占位行 → 真实值:必须写入
	if err := s.UpsertLink(&Link{
		ShareKey: "k", FID: "f1", FileName: "now-real.bin", Size: 7,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetLink("k", "f1")
	if got.FileName != "now-real.bin" || got.Size != 7 {
		t.Fatalf("真实值应覆盖占位行: name=%q size=%d", got.FileName, got.Size)
	}
	// 真实值 → 另一个真实值:必须写入(改名/改大小是正常链路)
	if err := s.UpsertLink(&Link{
		ShareKey: "k", FID: "f1", FileName: "renamed.bin", Size: 9,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetLink("k", "f1")
	if got.FileName != "renamed.bin" || got.Size != 9 {
		t.Fatalf("真实值间应正常覆盖: name=%q size=%d", got.FileName, got.Size)
	}
}

// 占位口径:空名 / 兜底名+非正大小 才算占位;真实文件叫「文件」但有大小,不算。
func TestLinkMetaIsPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want bool
	}{
		{"", 0, true},
		{"", 100, true},
		{PlaceholderFileName, 0, true},
		{PlaceholderFileName, -1, true},
		{PlaceholderFileName, 5, false},  // 有大小:不是「查不到」的占位
		{"real.bin", 0, false},           // 真实 0 字节文件
		{"real.bin", 4096, false},
	}
	for _, c := range cases {
		if got := LinkMetaIsPlaceholder(c.name, c.size); got != c.want {
			t.Fatalf("LinkMetaIsPlaceholder(%q,%d)=%v want %v", c.name, c.size, got, c.want)
		}
	}
}
