package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// ctxDriver 模拟 quark/蓝奏云这类**依赖分享上下文**的驱动:
// 拿不到 Ext["pwd_id"] 就直接失败(driver 原文:「缺少夸克分享上下文,请重新解析」)。
// 这正是坏快照(raw_tree='null')造成的后果形态:快照是上下文的唯一来源,
// 快照一坏,该分享下所有文件都提不了链。
type ctxDriver struct {
	fakeDriver
	nodes []driver.FileNode // ResolveShare 返回的节点;nil = 空解析
}

func (d *ctxDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return d.nodes, nil
}

func (d *ctxDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if ref.Ext == nil || ref.Ext["pwd_id"] == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "缺少夸克分享上下文,请重新解析", nil)
	}
	return driver.DirectLink{URL: "https://up.invalid/" + ref.FID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// 快照节点(fid f1 + pwd_id 上下文)。
func ctxNode() driver.FileNode {
	return driver.FileNode{FID: "f1", Name: "a.bin", Size: 10, Ext: map[string]string{"pwd_id": "pid-1"}}
}

// 读侧坏快照自愈(线上缺陷)。
//
// 缺陷形态(生产实测):share_key = sha1(pan|url|pwd),同一份分享的两种 URL 形态
// 是**两个 key、两条快照** —— 「/s/xxx」与「/s/xxx#/list/share/yyy」。其中一条
// 的 raw_tree 被写成字面量 "null"(json.Marshal(nil slice) 的产物),另一条完好。
// 于是「用 /s/xxx 形态提链」时快照里读不到任何节点 → ref.Ext 为空 →
// driver 报「缺少夸克分享上下文」→ 该分享下**所有**文件都提不了链,
// 而库里明明有完好的兄弟快照。
//
// 修复:buildRef/fileMeta 在本分享快照是空壳时,从同一网盘**别的 share_key**
// 快照里找回该 fid 的节点(名字/大小/上下文)。
//
// 回退判别:去掉 healShareNode 后,本用例必然失败(driver 拿不到 pwd_id)。
func TestResolveFileHealsContextFromSiblingShareSnapshot(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	const urlA = "https://fake.example/s/x"                 // 坏快照所在的形态
	const urlB = "https://fake.example/s/x#/list/share/yyy" // 完好的兄弟形态
	keyA := resolver.shareKey("fake", urlA, "")
	keyB := resolver.shareKey("fake", urlB, "")
	if keyA == keyB {
		t.Fatalf("前提不成立:两种 URL 形态必须是不同 share_key(都=%s)", keyA)
	}

	// 兄弟快照:完好,带着 pwd_id 上下文。
	good, err := json.Marshal([]driver.FileNode{ctxNode()})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlB, ShareKey: keyB,
		RawTree: string(good), ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// 本分享的快照:空壳("null" —— 生产历史数据的真实形态)。
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlA, ShareKey: keyA,
		RawTree: "null", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 用坏快照所在的形态提链:必须自愈成功。
	res, err := resolver.ResolveFile(ctx, urlA, "", "f1", false, "")
	if err != nil {
		t.Fatalf("坏快照下应自愈成功: %v", err)
	}
	if res.DirectURL != "https://up.invalid/f1" {
		t.Fatalf("直链不对: %q", res.DirectURL)
	}
	if res.FileName != "a.bin" {
		t.Fatalf("文件名未从兄弟快照恢复: %q", res.FileName)
	}
	if res.Size != 10 {
		t.Fatalf("大小未从兄弟快照恢复: %d", res.Size)
	}
}

// 跨网盘隔离护栏:兄弟快照若在**别的网盘**下,不得拿来当上下文 ——
// 不同网盘的 fid 空间互不相通,夸克的 pwd_id 塞进蓝奏云的请求必然串味。
// 回退判别:若去掉 pan_type 过滤,本用例会失败(不该成功)。
func TestResolveFileDoesNotHealAcrossPanTypes(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	const urlA = "https://fake.example/s/x"
	const urlB = "https://fake.example/s/x#/list/share/yyy"
	keyA := resolver.shareKey("fake", urlA, "")
	keyB := resolver.shareKey("fake", urlB, "")

	good, err := json.Marshal([]driver.FileNode{ctxNode()})
	if err != nil {
		t.Fatal(err)
	}
	// 兄弟快照挂在**别的网盘**下(生产上 quark 与 mangodrive 混在同一 shares 表)。
	if err := store.UpsertShare(&repo.Share{
		PanType: "mangodrive", ShareURL: urlB, ShareKey: keyB,
		RawTree: string(good), ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlA, ShareKey: keyA,
		RawTree: "null", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := resolver.ResolveFile(ctx, urlA, "", "f1", false, ""); err == nil {
		t.Fatal("跨网盘的快照不得用作上下文,却成功了")
	} else if !strings.Contains(err.Error(), "缺少夸克分享上下文") {
		t.Fatalf("应因缺少上下文而失败,实际: %v", err)
	}
}

// 写侧护栏:解析不到节点时落库不得产生字面量 "null" 的 raw_tree。
// 空数组 "[]" 才是诚实表达(「解析成功但没有节点」),能与「null / 解析失败」区分。
//
// 回退判别:去掉 resolver.go 里的 nil→"[]" 规范化后,本用例必然失败。
func TestResolveShareDoesNotPersistNullRawTree(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"}} // nodes 为 nil = 空解析
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	if _, err := resolver.ResolveShare(ctx, testShareURL, "", ""); err != nil {
		t.Fatalf("resolve share: %v", err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	sh, err := store.GetShare(key)
	if err != nil || sh == nil {
		t.Fatalf("get share: %v %v", sh, err)
	}
	if sh.RawTree == "null" {
		t.Fatalf("落库 raw_tree 不得是字面量 null: %q", sh.RawTree)
	}
	if sh.RawTree != "[]" {
		t.Fatalf("空解析应落空数组 [],got %q", sh.RawTree)
	}
}

// 写侧护栏(截断形态):驱动只回一个「truncated」标记节点时,resolver 会把节点
// 规范成 nil —— 落库同样不得是 "null"。
func TestResolveShareTruncatedDoesNotPersistNullRawTree(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"},
		nodes: []driver.FileNode{{Ext: map[string]string{"truncated": "1"}}}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	if _, err := resolver.ResolveShare(ctx, testShareURL, "", ""); err != nil {
		t.Fatalf("resolve share: %v", err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	sh, err := store.GetShare(key)
	if err != nil || sh == nil {
		t.Fatalf("get share: %v %v", sh, err)
	}
	if sh.RawTree == "null" {
		t.Fatalf("截断解析落库 raw_tree 不得是字面量 null: %q", sh.RawTree)
	}
}

// 写保护护栏(服务层落地):已有好快照时,一次空解析不得把它覆盖成空壳。
//
// 回退判别:去掉 repo.UpsertShare 的写保护后,本用例必然失败。
func TestResolveShareDoesNotClobberGoodSnapshotWithEmptyParse(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// ① 先用能解析出节点的驱动写一条好快照。
	if _, err := resolver.ResolveShare(ctx, testShareURL, "", ""); err != nil {
		t.Fatalf("resolve share: %v", err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	before, err := store.GetShare(key)
	if err != nil || before == nil {
		t.Fatalf("get share: %v %v", before, err)
	}
	if !repo.RawTreeHasContent(before.RawTree) {
		t.Fatalf("前提不成立:快照应带内容,got %q", before.RawTree)
	}

	// ② 同一分享再来一次空解析(驱动此时解析不出节点)。
	d.nodes = nil
	if _, err := resolver.ResolveShare(ctx, testShareURL, "", ""); err != nil {
		t.Fatalf("resolve share (empty): %v", err)
	}
	after, err := store.GetShare(key)
	if err != nil || after == nil {
		t.Fatalf("get share: %v %v", after, err)
	}
	if after.RawTree != before.RawTree {
		t.Fatalf("好快照被空解析覆盖: got %q want %q", after.RawTree, before.RawTree)
	}
}
