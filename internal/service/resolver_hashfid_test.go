package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// hashFidDriver 模拟 mangodrive 的长路径形态:fid 是哈希("p:<sha1>"),
// GetDirectLink 只认 ref.Ext["path"](真实存储路径),把哈希当路径会失败。
type hashFidDriver struct {
	fakeDriver
	realPath string
	fid      string
}

func newHashFidDriver(realPath string) *hashFidDriver {
	h := sha1.Sum([]byte(realPath))
	return &hashFidDriver{
		realPath: realPath,
		fid:      "p:" + hex.EncodeToString(h[:]),
	}
}

func (d *hashFidDriver) ResolveShare(_ context.Context, share driver.ShareLink, _ *driver.Credential) ([]driver.FileNode, error) {
	// 只有列到该文件所在目录时才能看到它(带 Ext path)。
	if !strings.HasSuffix(share.URL, "/deep") {
		return nil, driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在", nil)
	}
	return []driver.FileNode{{
		FID:   d.fid,
		Name:  "long.bin",
		Size:  42,
		IsDir: false,
		Ext:   map[string]string{"path": d.realPath},
	}}, nil
}

func (d *hashFidDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	p := ""
	if ref.Ext != nil {
		p = ref.Ext["path"]
	}
	if p == "" {
		// 服务端 mangodrive 把裸 fid 当路径去 OpenList Get → 404 的等价物。
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在:rawPath: "+ref.FID, nil)
	}
	if p != d.realPath {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "wrong path: "+p, nil)
	}
	return driver.DirectLink{URL: "https://up.invalid/long.bin", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// 回归(线上 bug):哈希 fid("p:<sha1>")不可逆,客户端 parentDirOf 推不出父目录,
// 只能以「任意 url(如根目录)+ 裸 fid」提链。此时本分享缓存/快照都没有该 fid,
// 必须从 links 表的历史缓存行(当初 auto-link 它的分享把真实路径写进 Ext["path"])
// 反查恢复——否则 driver 拿哈希当路径去 Get,404。
//
// 场景还原:
//  1. 先列真实目录(客户端浏览行为)→ auto-link 落库,Ext 带 path;
//  2. 换一个全新 url(模拟 parentDirOf 兜底返回的 "/")再以裸 fid 提链;
//  3. 修复前:driver 收到 "p:hash" 当路径 → 404;修复后:历史 Ext 反查 → 成功。
func TestResolveFileHashFidRecoversFromHistoryLink(t *testing.T) {
	d := newHashFidDriver("/Pool/这是一个相当长的中文目录名称用于测试fid哈希边界场景的目录名这里继续填充更多字符到超过一百个字节的长度限制值/deep/long.bin")
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// 1) 列真实目录 → auto-link 把 Ext["path"] 写进 links 表
	deepURL := testShareURL + "/deep"
	if _, err := resolver.ResolveShare(ctx, deepURL, "", ""); err != nil {
		t.Fatalf("list deep dir: %v", err)
	}

	// 2) 根目录 + 裸哈希 fid 提链(修复前在此 404)
	res, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, "")
	if err != nil {
		t.Fatalf("hash-fid link with bare url: %v", err)
	}
	if res.FileName != "long.bin" {
		t.Fatalf("file name not recovered: %q", res.FileName)
	}
	if res.DirectURL != "https://up.invalid/long.bin" {
		t.Fatalf("direct url wrong: %q", res.DirectURL)
	}
}

// 无历史缓存时(从未列过目录、直接哈希 fid 提链):错误要向上传播,不得伪装成功。
func TestResolveFileHashFidNoHistoryFails(t *testing.T) {
	d := newHashFidDriver("/Pool/never-listed/deep/long.bin")
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	if _, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, ""); err == nil {
		t.Fatal("expected failure without any history, got success")
	}
}

// DB 故障不得降级成误导性的驱动 404:store 关闭后,提链必须报「查询」类错误,
// 而不是把哈希当路径去 Get 得到「Mango Drive 路径不存在」(评审 P2 的契约回归)。
func TestResolveFileHashFidDBErrorPropagates(t *testing.T) {
	d := newHashFidDriver("/Pool/never-listed/deep/long.bin")
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)
	_ = store.Close() // 注入 DB 故障

	_, err := resolver.ResolveFile(ctx, testShareURL+"/", "", d.fid, false, "")
	if err == nil {
		t.Fatal("expected DB error to propagate, got success")
	}
	if strings.Contains(err.Error(), "Mango Drive 路径不存在") {
		t.Fatalf("DB failure degraded to driver 404: %v", err)
	}
	if !strings.Contains(err.Error(), "query") {
		t.Fatalf("error should mention the failed query, got: %v", err)
	}
}

// path 形态 fid 未命中时不得触发历史反查(门控回归):错误目录 + path fid 必须走
// driver 的空 Ext 404,错误信息里不得出现历史反查的报错前缀。
func TestResolveFilePathFidSkipsHistoryLookup(t *testing.T) {
	d := newHashFidDriver("/Pool/never-listed/deep/long.bin")
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)

	// path 形态 fid + 错误目录(根 url):门控生效 → 不反查 → driver 空 Ext 404
	_, err := resolver.ResolveFile(ctx, testShareURL+"/", "", "/Pool/some/other/path.bin", false, "")
	if err == nil {
		t.Fatal("expected failure for path fid with wrong dir, got success")
	}
	if strings.Contains(err.Error(), "query link history") {
		t.Fatalf("path fid must not trigger history lookup: %v", err)
	}
}
