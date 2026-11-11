package api

// v2.11 回归:fid 进 URL 路径段必须按段转义。
//
// 根因:mangodrive 的 path 形态 fid 就是存储路径("/a/b/c.txt",含 "/"),
// chi 的 {fid} 参数只匹配单个路径段(tree.go 对含 "/" 的段拒绝匹配),而
// 签发端(resolver.buildResult 等)把 fid 原样拼进路径 → 这类文件的下载入口
// 必然 404。生产实证:mangodrive welcome.txt 提链 200,其 download_url 请求
// 404,而同一响应里的 quark fid(无斜杠)一切正常。
//
// 约定:签名 payload 恒为原始 fid(编码前),URL 段为 PathEscape 形态,
// 验签端先解回原始 fid 再验签(见 service/dlurl.go)。本测试走真实 Router,
// 任何一端(签发/验签/路由)回退都会失败。

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const e2eDeepFID = "/deep/dir/f.bin"

func TestE2EDownloadSlashFidFullPathEscaped(t *testing.T) {
	e := newE2E(t, true)
	res := e.resolveFile(t, e2eDeepFID)

	du, _ := res["download_url"].(string)
	su, _ := res["stream_url"].(string)
	if du == "" || su == "" {
		t.Fatalf("download_url/stream_url missing: %v", res)
	}
	// 签发端必须编码 fid 段;若回退成原样拼接,这里立刻暴露
	if !strings.Contains(du, "%2F") || !strings.Contains(su, "%2F") {
		t.Fatalf("fid 段未转义: du=%s su=%s", du, su)
	}

	// 签名 payload 是原始 fid:URL 里必须能带编码段通过验签并 302 到上游直链
	resp, err := e.client.Get(du)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /d(路径 fid) = %d, want 302(修复前这里 404)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/file/deep") {
		t.Fatalf("302 Location=%q, want 上游直链 /file/deep", loc)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("302 应来自 PanRouter 路由而非上游")
	}

	// /stream 中转同样要吃下编码段并回传上游字节
	sresp, err := e.client.Get(su)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	body, _ := io.ReadAll(sresp.Body)
	if sresp.StatusCode != http.StatusOK || string(body) != "UPSTREAM-deep" {
		t.Fatalf("GET /stream(路径 fid) = %d body=%q, want 200/UPSTREAM-deep", sresp.StatusCode, body)
	}
}

// 对照组:未编码的斜杠 fid 本就无法通过路由(chi 参数不跨 "/"),必须仍是 404
// —— 这断言「编码是签发端职责」,防止有人改成「路由放宽」式的错误修法。
func TestE2EDownloadRawSlashFidStill404(t *testing.T) {
	e := newE2E(t, true)
	e.resolveFile(t, e2eDeepFID)
	key := ""
	if st, body := e.do(t, "POST", "/api/v1/resolve", e.token,
		`{"url":"`+e2eShareURL+`","fid":"`+e2eDeepFID+`"}`); st == http.StatusOK {
		key, _ = body["share_key"].(string)
	}
	if key == "" {
		t.Fatal("resolve failed")
	}
	st, _ := e.do(t, "GET", "/d/fake/"+key+"/deep/dir/f.bin?sig=whatever", "", "")
	if st != http.StatusNotFound {
		t.Fatalf("未编码斜杠 fid 应被路由拒绝(404), got %d", st)
	}
}

// v2.11 回归:账号 id 非数字不得吞错(修复前 id=0 继续执行,写出误导性审计)。
func TestE2EAccountInvalidIDRejected(t *testing.T) {
	e := newE2E(t, true)
	for _, path := range []string{"/api/v1/accounts/abc", "/api/v1/accounts/1e3"} {
		if st, body := e.do(t, "DELETE", path, e.token, ""); st != http.StatusBadRequest {
			t.Fatalf("DELETE %s = %d (%v), want 400", path, st, body)
		}
		if st, body := e.do(t, "POST", path+"/refresh", e.token, ""); st != http.StatusBadRequest {
			t.Fatalf("refresh %s = %d (%v), want 400", path, st, body)
		}
	}
}
