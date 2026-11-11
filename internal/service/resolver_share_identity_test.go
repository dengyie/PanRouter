package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// 污染护栏:同一网盘下**另一份分享**的快照不得被拿来当上下文。
//
// ★ 这不是假想风险:蓝奏云每个分享的单文件节点 fid 恒为 "file",
//
//	所以「fid 相同」根本不代表「同一个文件」。若自愈不做分享身份校验,
//	蓝奏云就会拿**别的分享**的 share_url/pwd 去提链 ——
//	提链成功、直链却指向另一个分享的文件,用户毫不知情。
//	静默给错文件比提链失败危险得多。
//
// 回退判别:去掉 sameShareIdentity 校验后,本用例必然失败(不该成功却成功)。
func TestResolveFileDoesNotHealFromDifferentShare(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/file"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// 本分享(fid "f1" 的上下文在坏快照里丢失)。
	const urlA = "https://fake.example/s/aaaaaaaaaaaa"
	keyA := resolver.shareKey("fake", urlA, "")
	// 另一份**不同的**分享,fid 恰好同名(蓝奏云就是这种情况)。
	const urlB = "https://fake.example/s/bbbbbbbbbbbb"
	keyB := resolver.shareKey("fake", urlB, "")

	good, err := json.Marshal([]driver.FileNode{{FID: "f1", Name: "other.bin", Size: 999,
		Ext: map[string]string{"pwd_id": "bbbbbbbbbbbb"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlB, ShareKey: keyB,
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

	// 必须失败(而不是拿别的分享的上下文提链成功)。
	if _, err := resolver.ResolveFile(ctx, urlA, "", "f1", false, ""); err == nil {
		t.Fatal("另一份分享的上下文不得被拿来提链,却成功了")
	}
}

// 蓝奏云形态的污染护栏:fid 恒为 "file",两个不同分享的快照里都是 "file"。
// 必须按分享身份(share_url)区分,不得串味。
func TestResolveFileDoesNotHealFromDifferentLanzouShare(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/file"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	const urlA = "https://fake.example/iAAAAAAAAAA"
	const urlB = "https://fake.example/iBBBBBBBBBB"
	keyA := resolver.shareKey("fake", urlA, "")
	keyB := resolver.shareKey("fake", urlB, "")

	// 蓝奏云形态的节点:fid 恒为 "file",Ext 带 share_url/pwd。
	good, err := json.Marshal([]driver.FileNode{{FID: "file", Name: "other.zip", Size: 999,
		Ext: map[string]string{"share_url": urlB, "pwd": "9999"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlB, ShareKey: keyB,
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

	if _, err := resolver.ResolveFile(ctx, urlA, "", "file", false, ""); err == nil {
		t.Fatal("另一份蓝奏云分享的上下文不得被拿来提链,却成功了")
	}
}

// 反向护栏:同一份分享的两种 URL 形态(根链 vs 深链)必须仍能自愈 ——
// 身份校验不能把「同份不同形态」也挡掉(那正是自愈要救的场景)。
//
// quark 的形态:根链 "/s/<id>" 与深链 "/s/<id>#/list/share/<fid>",
// 节点的 Ext["pwd_id"] 就是那个 id。
func TestResolveFileHealsSameShareDifferentUrlForm(t *testing.T) {
	d := &ctxDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f1"}, nodes: []driver.FileNode{ctxNode()}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	const id = "AbCdEf123456"
	const urlA = "https://fake.example/s/" + id + "#/list/share/ZzYyXx987654"
	const urlB = "https://fake.example/s/" + id
	keyA := resolver.shareKey("fake", urlA, "")
	keyB := resolver.shareKey("fake", urlB, "")

	good, err := json.Marshal([]driver.FileNode{{FID: "f1", Name: "a.bin", Size: 10,
		Ext: map[string]string{"pwd_id": id, "stoken": "ST"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: urlB, ShareKey: keyB,
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

	res, err := resolver.ResolveFile(ctx, urlA, "", "f1", false, "")
	if err != nil {
		t.Fatalf("同一份分享的不同 URL 形态应自愈成功: %v", err)
	}
	if res.FileName != "a.bin" {
		t.Fatalf("文件名未恢复: %q", res.FileName)
	}
}

// host 不同即不是同一份分享(蓝奏云不同子域名下同名 id 也可能是不同分享)。
func TestSameShareIdentityRejectsDifferentHost(t *testing.T) {
	self := &repo.Share{PanType: "fake", ShareURL: "https://a.example/s/abcdef123456"}
	cand := &repo.Share{PanType: "fake", ShareURL: "https://b.example/s/abcdef123456"}
	node := &driver.FileNode{FID: "f1", Ext: map[string]string{"pwd_id": "abcdef123456"}}
	if sameShareIdentity(self, cand, node) {
		t.Fatal("host 不同不得判为同一份分享")
	}
	// 同 host 同 id → 是。
	cand2 := &repo.Share{PanType: "fake", ShareURL: "https://a.example/s/abcdef123456"}
	if !sameShareIdentity(self, cand2, node) {
		t.Fatal("同 host 同 id 应判为同一份分享")
	}
}

// 身份判定的边界表。
//
// 注意:节点的 pwd_id 是**权威**的分享身份(quark 解析出的 pwd_id 就是分享 id),
// 若节点的 pwd_id 与本分享 id 相同,即便候选快照的 URL 指向别的 id,
// 该节点仍属本分享(现实中不会不一致,但判据要稳健)。
func TestSameShareIdentity(t *testing.T) {
	cases := []struct {
		name string
		self *repo.Share
		cand *repo.Share
		node *driver.FileNode
		want bool
	}{
		{
			name: "同 host 同 id",
			self: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456"},
			cand: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456"},
			node: &driver.FileNode{FID: "f1", Ext: map[string]string{"pwd_id": "AbCdEf123456"}},
			want: true,
		},
		{
			name: "同 host 不同 id 且节点身份也不同",
			self: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456"},
			cand: &repo.Share{ShareURL: "https://pan.quark.cn/s/000000000000"},
			node: &driver.FileNode{FID: "f1", Ext: map[string]string{"pwd_id": "000000000000"}},
			want: false,
		},
		{
			name: "不同 host",
			self: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456"},
			cand: &repo.Share{ShareURL: "https://other.example/s/AbCdEf123456"},
			node: &driver.FileNode{FID: "f1", Ext: map[string]string{"pwd_id": "AbCdEf123456"}},
			want: false,
		},
		{
			name: "同 host 同 path 不同形态(深链 vs 根链)",
			self: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456#/list/share/x"},
			cand: &repo.Share{ShareURL: "https://pan.quark.cn/s/AbCdEf123456"},
			node: &driver.FileNode{FID: "f1", Ext: map[string]string{}},
			want: true,
		},
	}
	for _, c := range cases {
		if got := sameShareIdentity(c.self, c.cand, c.node); got != c.want {
			t.Fatalf("%s: sameShareIdentity = %v want %v", c.name, got, c.want)
		}
	}
}

// 分享 id 提取的边界:目录名不得被误当分享 id。
func TestShareIDOf(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://pan.quark.cn/s/AbCdEf123456", "AbCdEf123456"},
		{"https://pan.quark.cn/s/AbCdEf123456#/list/share/ZzYyXx987654", "AbCdEf123456"},
		{"https://wwbfg.lanzouu.com/iNrjO4afcy0b", "iNrjO4afcy0b"},
		{"https://drive.example.com/Pool-Secure", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := shareIDOf(c.raw); got != c.want {
			t.Fatalf("shareIDOf(%q) = %q want %q", c.raw, got, c.want)
		}
	}
}

// 目录段前缀判定:段边界必须对齐("/ab" 不是 "/a" 的前缀)。
func TestPathHasPrefix(t *testing.T) {
	cases := []struct {
		p, prefix string
		want      bool
	}{
		{"/Pool-Secure/PanRouter/out", "/Pool-Secure/PanRouter", true},
		{"/Pool-Secure/PanRouter", "/Pool-Secure/PanRouter", true},
		{"/Pool-Secure/PanRouterX/out", "/Pool-Secure/PanRouter", false},
		{"/other/file", "/Pool-Secure", false},
		{"/x", "", false},
	}
	for _, c := range cases {
		if got := pathHasPrefix(c.p, c.prefix); got != c.want {
			t.Fatalf("pathHasPrefix(%q,%q) = %v want %v", c.p, c.prefix, got, c.want)
		}
	}
}
