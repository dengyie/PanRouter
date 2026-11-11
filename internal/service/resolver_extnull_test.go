package service

import (
	"context"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/repo"
)

// 回归(线上缺陷):历史反查撞上「坏行」时必须继续往下找可用行。
//
// 缺陷形态:同一哈希 fid 的多行 links 缓存里,较新的那行 ext 是字面量 "null"
// (json.Marshal(nil map) 的产物,代表「这次解析没拿到上下文」),较旧的那行
// 才有真实 path。只取最新一行 → Unmarshal 得到 nil map → 路径没恢复 →
// driver 把哈希当路径去 Get → 404。库里明明存在带 path 的可用历史行,
// 裸哈希提链却必然失败。
//
// 修复:历史反查改为按新→旧遍历,取第一条能解出非空 path 的行。
func TestResolveFileHashFidSkipsBadHistoryRow(t *testing.T) {
	d := newHashFidDriver("/Pool/real/deep/long.bin")
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// 1) 先列真实目录 → auto-link 落库,Ext 带 path(可用的历史行)。
	deepURL := testShareURL + "/deep"
	if _, err := resolver.ResolveShare(ctx, deepURL, "", ""); err != nil {
		t.Fatalf("list deep dir: %v", err)
	}
	key := resolver.shareKey("fake", deepURL, "")

	_ = key
	// 2) 注入一行「坏行」:另一条 share_key(故是独立的一行,不是对上面那行的
	//    覆盖)、更新的时间、更高的 id,但 ext 是字面量 "null"。
	//    这正是线上被污染的历史数据形态(同一 fid 在不同分享上下文下各留一行,
	//    其中一行 ext 被写成 "null")。
	bad := &repo.Link{
		ShareKey:  key + "-other",
		FID:       d.fid,
		FileName:  "long.bin",
		Ext:       "null",
		CreatedAt: time.Now().Add(time.Hour), // 比可用行更新
	}
	if err := store.UpsertLink(bad); err != nil {
		t.Fatal(err)
	}
	// 坏行必须是**独立**的一行(否则测试会退化成「写侧保护」而非「读侧挑行」)。
	all, err := store.ListLinksByFID(d.fid, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("同 fid 应有 2 行(可用行 + 坏行),got %d", len(all))
	}
	if all[0].Ext != "null" {
		t.Fatalf("最新一行应是坏行(ext=null),got %q", all[0].Ext)
	}

	// 3) 裸哈希 fid + 根 url 提链:必须跳过坏行,取到下面的可用行 → 成功。
	res, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, "")
	if err != nil {
		t.Fatalf("hash-fid link with bad history row present: %v", err)
	}
	if res.DirectURL != "https://up.invalid/long.bin" {
		t.Fatalf("direct url wrong: %q", res.DirectURL)
	}
	if res.FileName != "long.bin" {
		t.Fatalf("file name not recovered: %q", res.FileName)
	}
}

// 反向护栏:库里**只有**坏行(没有任何带 path 的历史)时,必须失败,
// 而不是把 nil map 当上下文蒙混过关。
func TestResolveFileHashFidOnlyBadHistoryFails(t *testing.T) {
	d := newHashFidDriver("/Pool/real/deep/long.bin")
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	if _, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, ""); err == nil {
		t.Fatal("无任何历史时应失败,got success")
	}

	// 注入唯一的坏行(仍无 path)→ 依然必须失败。
	key := resolver.shareKey("fake", testShareURL+"/", "") + "-other"
	if err := store.UpsertLink(&repo.Link{
		ShareKey: key, FID: d.fid, FileName: "long.bin", Ext: "null",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, ""); err == nil {
		t.Fatal("只有坏行时仍应失败,got success")
	}
}

// 写侧护栏:没拿到上下文时落库不得产生字面量 "null" 的 ext。
// (service 层已改为空上下文落空串;repo 层另有写保护兜底。)
func TestResolveFileDoesNotPersistNullExt(t *testing.T) {
	d := &fakeDriver{linkURL: "https://up.invalid/x"}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	if _, err := resolver.ResolveFile(ctx, testShareURL, "", "f-nocontext", false, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	l, err := store.GetLink(key, "f-nocontext")
	if err != nil || l == nil {
		t.Fatalf("get link: %v %v", l, err)
	}
	if l.Ext == "null" {
		t.Fatalf("落库 ext 不得是字面量 null:%q", l.Ext)
	}
}
