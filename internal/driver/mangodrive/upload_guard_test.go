package mangodrive

// v2.11 回归:mangodrive 上传/提链的空响应与元数据。
//
// ① fs/put 返回 200 + 空 body:OpenList 正常必回 {"code":200,...},空 body
//    意味着代理截断/协议异常。修复前 resp.Code 恒为 0 → 按成功处理 →
//    转存报 done 而远端没有文件(静默丢文件)。
// ② GetDirectLink 顺带带回 fs/get 的 name/size:根 URL + 深路径 fid 提链时
//    缓存/快照/历史都查不到元数据,这是唯一来源。
// ③ 两个基址都未配置时必须显式报「未配置」,不得发出无 scheme 请求。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
)

func TestPutEmptyBodyIsFailure(t *testing.T) {
	// 200 + 空 body:正是「代理截断」的形态
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(newClient(t))
	d.InternalBase = srv.URL
	api := NewAPI(newClient(t), srv.URL, "tok-1")
	err := api.Put(context.Background(), "/Pool/out.bin", 10, strings.NewReader("0123456789"))
	if err == nil {
		t.Fatal("200+空 body 不得判为上传成功(静默丢文件)")
	}
	if !strings.Contains(err.Error(), "响应为空") {
		t.Fatalf("错误应说明响应为空: %v", err)
	}
}

func TestPutOKStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":null}`))
	}))
	defer srv.Close()

	api := NewAPI(newClient(t), srv.URL, "tok-1")
	if err := api.Put(context.Background(), "/Pool/out.bin", 3, strings.NewReader("abc")); err != nil {
		t.Fatalf("正常 200 响应不应失败: %v", err)
	}
}

func TestGetDirectLinkCarriesMetadata(t *testing.T) {
	f := newFake(t)
	d := New(newClient(t))
	d.InternalBase = f.srv.URL

	dl, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "tok-1"},
		driver.FileRef{FID: "/Pool/a.mkv", Ext: map[string]string{"path": "/Pool/a.mkv"}})
	if err != nil {
		t.Fatalf("direct link: %v", err)
	}
	if dl.Name != "a.mkv" || dl.Size != 1024 {
		t.Fatalf("提链应顺带带回元数据: name=%q size=%d want a.mkv/1024", dl.Name, dl.Size)
	}
	if !dl.ExpiresAt.After(time.Now()) {
		t.Fatalf("TTL missing: %+v", dl)
	}
}

func TestGetDirectLinkWithoutBaseErrors(t *testing.T) {
	d := New(newClient(t)) // InternalBase / PublicBase 均未配置
	_, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "tok-1"},
		driver.FileRef{FID: "/Pool/a.mkv"})
	if err == nil {
		t.Fatal("无基址必须显式报错,不得发出无 scheme 请求")
	}
	if !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("错误应说明配置缺失: %v", err)
	}

	if _, err := d.CheckCredential(context.Background(), driver.Credential{Cookie: "tok-1"}); err == nil {
		t.Fatal("CheckCredential 无基址同样必须显式报错")
	}
}
