package service

import (
	"context"
	"fmt"
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

// quotaFidDriver 多文件版:一个 12 文件的深层目录,锁定「每目录 auto-link 前 8 个
// (defaultAutoLinkMax)」的配额语义——这是 v1.22 客户端「恒携带目录」契约的服务端
// 事实源,配额或 auto-link 循环被改动时必须保持这三条行为。
type quotaFidDriver struct {
	fakeDriver
	dirSuffix string             // 目录 URL 以此结尾才可见
	files     map[string]string  // fid(哈希) → 真实存储路径
	names     []string           // 列表返回顺序(即 auto-link 顺序)
}

func newQuotaFidDriver(dirSuffix string, n int) *quotaFidDriver {
	d := &quotaFidDriver{dirSuffix: dirSuffix, files: map[string]string{}}
	for i := 0; i < n; i++ {
		p := "/Pool/这是一个相当长的中文目录名称用于测试自动提链配额边界场景的目录名继续填充字符确保超过一百个字节触发哈希/quota"
		p += fmt.Sprintf("/quota%02d.bin", i)
		fid := "p:" + hex.EncodeToString(func() []byte { h := sha1.Sum([]byte(p)); return h[:] }())
		d.files[fid] = p
		d.names = append(d.names, fmt.Sprintf("quota%02d.bin", i))
	}
	return d
}

func (d *quotaFidDriver) ResolveShare(_ context.Context, share driver.ShareLink, _ *driver.Credential) ([]driver.FileNode, error) {
	if !strings.HasSuffix(share.URL, d.dirSuffix) {
		return nil, driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在", nil)
	}
	nodes := make([]driver.FileNode, 0, len(d.names))
	for _, name := range d.names {
		for fid, p := range d.files {
			if strings.HasSuffix(p, "/"+name) {
				nodes = append(nodes, driver.FileNode{FID: fid, Name: name, Size: 42, IsDir: false,
					Ext: map[string]string{"path": p}})
			}
		}
	}
	return nodes, nil
}

func (d *quotaFidDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	p := d.files[ref.FID]
	if p == "" || (ref.Ext != nil && ref.Ext["path"] != "" && ref.Ext["path"] != p) {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在", nil)
	}
	if ref.Ext == nil || ref.Ext["path"] == "" {
		// 裸哈希:模拟 OpenList 把哈希当路径 → 404
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在:rawPath: "+ref.FID, nil)
	}
	return driver.DirectLink{URL: "https://up.invalid/" + ref.FID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// 锁定配额语义:12 文件目录浏览后——
//  1. 前 8 个(auto-link 配额内)有 links 历史行,裸哈希+根 url 反查成功;
//  2. 第 9+ 个无历史行,裸哈希+根 url 必须失败(契约:客户端不得依赖反查);
//  3. 第 9+ 个携带真实目录提链成功(快照直查,配额无关)。
func TestResolveShareAutoLinkQuotaSemantics(t *testing.T) {
	const n = 12
	d := newQuotaFidDriver("/quota-deep", n)
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	ctx := WithAuthed(context.Background(), true)
	dirURL := testShareURL + "/quota-deep"

	// 浏览:触发前 8 个 auto-link
	res, err := resolver.ResolveShare(ctx, dirURL, "", "")
	if err != nil {
		t.Fatalf("list dir: %v", err)
	}
	if len(res.Files) != n {
		t.Fatalf("应列出 %d 个文件, got %d", n, len(res.Files))
	}
	// files 顺序 = nodes 顺序(auto-link 顺序)
	fids := make([]string, 0, n)
	for _, it := range res.Files {
		fids = append(fids, it.FID)
	}

	// 1) 配额内:裸哈希+根 url 反查成功
	for i := 0; i < 8; i++ {
		if _, err := resolver.ResolveFile(ctx, testShareURL+"/", "", fids[i], false, ""); err != nil {
			t.Fatalf("配额内文件 %d 反查应成功: %v", i, err)
		}
	}
	// 2) 配额外:裸哈希+根 url 必须失败(反查无历史行;锁定该契约)
	for i := 8; i < n; i++ {
		if _, err := resolver.ResolveFile(ctx, testShareURL+"/", "", fids[i], false, ""); err == nil {
			t.Fatalf("配额外文件 %d 裸哈希反查必须失败(无历史行)", i)
		}
	}
	// 3) 配额外:携带真实目录 → 快照直查成功(客户端 v1.22 契约)
	for i := 8; i < n; i++ {
		res, err := resolver.ResolveFile(ctx, dirURL, "", fids[i], false, "")
		if err != nil {
			t.Fatalf("配额外文件 %d 带目录提链应成功: %v", i, err)
		}
		if res.FileName == "" {
			t.Fatalf("配额外文件 %d 应恢复文件名", i)
		}
	}
}
