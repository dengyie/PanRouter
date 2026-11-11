package service

// v2.11 回归:「有列无内容」家族的元数据面 —— 占位名遮蔽真实元数据、
// 「有键无值」的 ext 冒充有效上下文、自愈取最旧快照。

import (
	"context"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// capDriver 捕获 buildRef 交给 driver 的 FileRef,用于断言恢复链的产物。
type capDriver struct {
	fakeDriver
	mu    chan struct{}
	refs  chan driver.FileRef
	block bool
}

func newCapDriver() *capDriver {
	return &capDriver{mu: make(chan struct{}, 8), refs: make(chan driver.FileRef, 8)}
}

func (d *capDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	select {
	case d.refs <- ref:
	default:
	}
	return driver.DirectLink{URL: "https://up.invalid/f", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// P1:fileMeta 不得把占位链接行(「文件」+0)当命中 —— 它会短路后面的快照/
// 自愈/历史三条恢复链,而快照里明明有真实名字与大小。缓存命中路径同理。
func TestFileMetaSkipsPlaceholderLinkRow(t *testing.T) {
	d := &fakeDriver{linkURL: "https://up.invalid/x"}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	key := resolver.shareKey("fake", testShareURL, "")
	// 快照里有真实元数据;链接行是历史占位值
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: testShareURL, ShareKey: key,
		RawTree: `[{"FID":"f1","Name":"real.bin","Size":321,"IsDir":false,"Ext":{"pwd_id":"x"}}]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertLink(&repo.Link{
		ShareKey: key, FID: "f1", FileName: placeholderFileName, Size: 0,
		DirectLink: "https://up.invalid/f", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	res, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "f1", false, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.CacheHit != true {
		t.Fatalf("新鲜缓存应命中, cache_hit=%v", res.CacheHit)
	}
	if res.FileName != "real.bin" || res.Size != 321 {
		t.Fatalf("占位缓存行遮蔽了快照元数据: name=%q size=%d want real.bin/321", res.FileName, res.Size)
	}
}

// P1:driver 提链途中带回的元数据(mangodrive fs/get)必须被采用 ——
// 根 URL + 深路径 fid 场景下这是**唯一**的元数据来源。
func TestResolveFileUsesDirectLinkMetadata(t *testing.T) {
	d := &metaDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/x"}}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "/deep/dir/f.bin", false, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.FileName != "deep.bin" || res.Size != 5 {
		t.Fatalf("driver 元数据未被采用: name=%q size=%d want deep.bin/5", res.FileName, res.Size)
	}
}

type metaDriver struct{ fakeDriver }

func (d *metaDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return nil, nil // 根 URL 列目录查不到深路径文件 → fileMeta 全部落空
}

func (d *metaDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	// 模拟 mangodrive:提链时 fs/get 顺带拿到真实元数据
	return driver.DirectLink{
		URL: "https://up.invalid/f", ExpiresAt: time.Now().Add(time.Hour),
		Name: "deep.bin", Size: 5,
	}, nil
}

// P2:「有键无值」的 ext({"api":""})不得冒充有效上下文 —— 否则它会短路
// 快照/自愈/历史三条恢复链,mangodrive 哈希 fid 提链必然 404。
func TestBuildRefSkipsValuelessExt(t *testing.T) {
	d := newCapDriver()
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	key := resolver.shareKey("fake", testShareURL, "")
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: testShareURL, ShareKey: key,
		RawTree: `[{"FID":"p:hash","Name":"h.bin","Size":1,"IsDir":false,"Ext":{"path":"/Pool/real/h.bin","api":""}}]`,
	}); err != nil {
		t.Fatal(err)
	}
	// 历史缓存行:有键无值(正是 v2.9 之前「有列无内容」的相邻形态)
	if err := store.UpsertLink(&repo.Link{
		ShareKey: key, FID: "p:hash", FileName: "h.bin", Ext: `{"api":""}`,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "p:hash", false, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	select {
	case ref := <-d.refs:
		if ref.Ext["path"] != "/Pool/real/h.bin" {
			t.Fatalf("无效 ext 短路了恢复链: ref.Ext=%v", ref.Ext)
		}
	default:
		t.Fatal("driver 未被调用")
	}
}

// P3:自愈必须取**最新**的命中快照(id DESC),不能取最旧的 —— 旧快照的
// 上下文(stoken 等)可能已过期。
func TestHealPrefersNewestSiblingSnapshot(t *testing.T) {
	cd := newCapDriver()
	resolver, store := newTestResolver(t, cd, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	// 自分享:空壳(坏快照)
	selfKey := resolver.shareKey("fake", "https://fake.example/s/abc123", "")
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: "https://fake.example/s/abc123", ShareKey: selfKey, RawTree: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	// 同一分享的两个 URL 形态:旧(id 小)带过期 stoken,新(id 大)带新 stoken
	mk := func(url, stoken string) {
		k := resolver.shareKey("fake", url, "")
		if err := store.UpsertShare(&repo.Share{
			PanType: "fake", ShareURL: url, ShareKey: k,
			RawTree: `[{"FID":"f1","Name":"n.bin","Size":1,"IsDir":false,"Ext":{"pwd_id":"abc123","stoken":"` + stoken + `"}}]`,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("https://fake.example/s/abc123?form=old", "OLD-STOKEN")
	mk("https://fake.example/s/abc123?form=new", "NEW-STOKEN")

	if _, err := resolver.ResolveFile(WithAuthed(context.Background(), true), "https://fake.example/s/abc123", "", "f1", false, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	select {
	case ref := <-cd.refs:
		if ref.Ext["stoken"] != "NEW-STOKEN" {
			t.Fatalf("自愈取了旧快照: stoken=%q want NEW-STOKEN", ref.Ext["stoken"])
		}
	default:
		t.Fatal("driver 未被调用")
	}
}
