package quark

// v2.11 回归:tmpDir 的 mkdir 冲突退化链路必须把**查找失败**的真实错误带回去。
//
// 缺陷:mkdir 报「目录已存在」(业务冲突)后退化 findTmpDir;findTmpDir 失败时
// 原代码 `return "", err` 把 mkdir 的 err 冒充查找结果 —— 错误分类与文案失真
// (调用方按 mkdir 冲突重试,永远不会成功)。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/driver"
)

func TestTmpDirFindFailureReturnsFindError(t *testing.T) {
	var mkdirCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clouddrive/file/sort"):
			// findTmpDir 的目录查询失败(404 → 结构变化错误)
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/1/clouddrive/file"):
			mkdirCalls++
			// mkdir 冲突(目录已存在)→ 退化 findTmpDir
			writeJSONLine(w, `{"code":41009,"message":"目录已存在","data":null}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv)
	_, err := d.tmpDir(context.Background(), &driver.Credential{Cookie: "c=1"})
	if err == nil {
		t.Fatal("mkdir 冲突 + 查找失败必须报错")
	}
	if strings.Contains(err.Error(), "目录已存在") {
		t.Fatalf("必须返回 findTmpDir 的错误而非 mkdir 的冲突错误: %v", err)
	}
}

// 对照:mkdir 冲突 + 查找成功 → 返回找到的 fid(退化链路正常工作)。
func TestTmpDirFallsBackToFind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/clouddrive/file/sort"):
			writeJSONLine(w, `{"code":0,"message":"ok","data":{"list":[{"fid":"FDIR","file_name":"`+tmpDirName+`","dir":true}],"metadata":{"_total":1}}}`)
		case strings.HasSuffix(r.URL.Path, "/1/clouddrive/file"):
			writeJSONLine(w, `{"code":41009,"message":"目录已存在","data":null}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := newTestDriver(t, srv)
	fid, err := d.tmpDir(context.Background(), &driver.Credential{Cookie: "c=1"})
	if err != nil {
		t.Fatalf("退化查找应成功: %v", err)
	}
	if fid != "FDIR" {
		t.Fatalf("应返回按名查到的 fid, got %q", fid)
	}
}
