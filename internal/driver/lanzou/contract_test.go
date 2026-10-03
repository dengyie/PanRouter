package lanzou

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// 合约测试:回放蓝奏云分享页/iframe/ajaxm 各形态样本,不打真实网盘。

const (
	pageWithIframe = `<html><title>测试文件.zip - 蓝奏云</title>
	<div>大小：1.50 MB</div>
	<iframe class="n_post" src="/fn?abc123" ></iframe></html>`
	iframePage = `<html><script>var sign = 'SIGNVALUE123456';</script></html>`
	ajaxOK     = `{"zt":1,"dom":"https://dev.example.com","url":"/file/xyz789"}`
	pageModern = `<html><title>新页面.bin - 蓝奏云</title><script>var sign='MODERNSIGN99999';</script></html>`
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/page1":
			_, _ = w.Write([]byte(pageWithIframe))
		case "/fn":
			if !strings.Contains(r.URL.RawQuery, "abc123") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(iframePage))
		case "/modern":
			_, _ = w.Write([]byte(pageModern))
		case "/gone":
			_, _ = w.Write([]byte(`<html>文件取消分享了</html>`))
		case "/pwd":
			_, _ = w.Write([]byte(`<html><div id="passwddiv">请输入密码</div></html>`))
		case "/broken":
			_, _ = w.Write([]byte(`<html>页面改版了,啥都没有</html>`))
		case "/ajaxm.php":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("signs") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(ajaxOK))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestDriver(t *testing.T, srv *httptest.Server) *Driver {
	t.Helper()
	cl, err := httpx.New(httpx.Options{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	return New(cl)
}

func TestResolveShareIframeFlow(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	d := newTestDriver(t, srv)

	nodes, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/page1"}, nil)
	if err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "测试文件.zip - 蓝奏云" || nodes[0].Size != 1572864 {
		t.Fatalf("nodes: %+v", nodes)
	}
	if nodes[0].Ext["share_url"] != srv.URL+"/page1" {
		t.Fatalf("share_url 上下文丢失: %+v", nodes[0].Ext)
	}
	dl, err := d.GetDirectLink(context.Background(), nil, driver.FileRef{FID: "file", Ext: nodes[0].Ext})
	if err != nil {
		t.Fatalf("GetDirectLink: %v", err)
	}
	if dl.URL != "https://dev.example.com/file/xyz789" {
		t.Errorf("final url: %s", dl.URL)
	}
	if dl.UA != "" || dl.Cookie != "" {
		t.Errorf("蓝奏云最终直链应允许浏览器 302(无 UA/Cookie 约束): %+v", dl)
	}
}

func TestResolveShareModernFlow(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	d := newTestDriver(t, srv)
	nodes, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/modern"}, nil)
	if err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "新页面.bin - 蓝奏云" {
		t.Fatalf("nodes: %+v", nodes)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		path string
		want string // driver.Kind 字符串
	}{
		{"/gone", string(driver.KindShareGone)},
		{"/pwd", string(driver.KindUnsupported)},
		{"/broken", string(driver.KindInterfaceChanged)},
	}
	for _, c := range cases {
		srv := newTestServer(t)
		d := newTestDriver(t, srv)
		_, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + c.path}, nil)
		srv.Close()
		if err == nil {
			t.Errorf("%s: expect error", c.path)
			continue
		}
		if !isKind(err, c.want) {
			t.Errorf("%s: got %v want kind %s", c.path, err, c.want)
		}
	}
}

func isKind(err error, kind string) bool {
	de, ok := err.(*driver.Error)
	if !ok {
		return false
	}
	return string(de.Kind) == kind
}
