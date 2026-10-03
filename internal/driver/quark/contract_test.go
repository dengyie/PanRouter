package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// 合约测试:用 httptest 回放夸克响应样本,不打真实网盘(设计文档 §9)。

func testServer(t *testing.T, failToken bool, tokenResp, detailResp, downloadResp func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/token"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["pwd_id"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(tokenRespBody))
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/detail"):
			_, _ = w.Write([]byte(detailRespBody))
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/download"):
			w.Header().Add("Set-Cookie", "__puus=puusvalue; Path=/; Domain=quark.cn")
			_, _ = w.Write([]byte(downloadRespBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

const (
	tokenRespBody    = `{"code":0,"message":"ok","data":{"stoken":"STOKEN"}}`
	detailRespBody   = `{"code":0,"message":"ok","data":{"list":[{"fid":"F1","file_name":"a.mp4","size":123,"dir":false}],"metadata":{"_total":1}}}`
	downloadRespBody = `{"code":0,"message":"ok","data":[{"download_url":"https://dl.example.com/file?f=1"}]}`
)

func newTestDriver(t *testing.T, srv *httptest.Server) *Driver {
	t.Helper()
	cl, err := httpx.New(httpx.Options{AllowPrivate: true, RedirectAllow: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return New(cl, srv.URL, srv.URL)
}

func TestResolveShareAndDirectLink(t *testing.T) {
	srv := testServer(t, false, nil, nil, nil)
	defer srv.Close()
	d := newTestDriver(t, srv)

	nodes, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/s/abc123"}, nil)
	if err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if len(nodes) != 1 || nodes[0].FID != "F1" || nodes[0].Name != "a.mp4" || nodes[0].Size != 123 {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if nodes[0].Ext["pwd_id"] != "abc123" || nodes[0].Ext["stoken"] != "STOKEN" {
		t.Fatalf("ext lost: %+v", nodes[0].Ext)
	}

	// 分享直链必须携带登录态
	if _, err := d.GetDirectLink(context.Background(), nil, driver.FileRef{FID: "F1", Ext: nodes[0].Ext}); err == nil {
		t.Fatal("无 Cookie 时应返回 AuthExpired")
	} else {
		var de *driver.Error
		if !errors.As(err, &de) || de.Kind != driver.KindAuthExpired {
			t.Fatalf("expect auth_expired, got %v", err)
		}
	}

	cred := &driver.Credential{Cookie: "session=1"}
	dl, err := d.GetDirectLink(context.Background(), cred, driver.FileRef{FID: "F1", Ext: nodes[0].Ext})
	if err != nil {
		t.Fatalf("GetDirectLink: %v", err)
	}
	if dl.URL != "https://dl.example.com/file?f=1" {
		t.Errorf("direct url: %s", dl.URL)
	}
	if !strings.Contains(dl.Cookie, "__puus=puusvalue") {
		t.Errorf("下载接口的 Set-Cookie 应并入直链 Cookie(aria2/中转需携带):%q", dl.Cookie)
	}
	if dl.UA != QuarkUA {
		t.Errorf("直链 UA 应为夸克客户端 UA: %s", dl.UA)
	}
}

// 异步任务形态:data.task_id → 轮询 /1/clouddrive/task 至 status==2。
func TestDirectLinkViaAsyncTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/token"):
			_, _ = w.Write([]byte(tokenRespBody))
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/detail"):
			_, _ = w.Write([]byte(detailRespBody))
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/download"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"T123"}}`))
		case strings.HasSuffix(r.URL.Path, "/clouddrive/task"):
			if r.URL.Query().Get("task_id") != "T123" || r.URL.Query().Get("retry_index") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":2,"data":[{"download_url":"https://dl.example.com/task-file"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := newTestDriver(t, srv)

	dl, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	if err != nil {
		t.Fatalf("GetDirectLink(async): %v", err)
	}
	if dl.URL != "https://dl.example.com/task-file" {
		t.Fatalf("task download url: %s", dl.URL)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		body string
		want driver.Kind
	}{
		{`{"code":32003,"message":"分享已经取消了"}`, driver.KindShareGone},
		{`{"code":41008,"message":"操作太频繁,请稍后再试"}`, driver.KindRiskControl},
		{`{"code":31001,"message":"请登录后操作"}`, driver.KindAuthExpired},
		{`{"code":70016,"message":"提取码错误"}`, driver.KindNotFound},
		{`<html>unexpected</html>`, driver.KindUpstream}, // 非 JSON → 上游异常
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(c.body))
		}))
		d := newTestDriver(t, srv)
		_, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/s/abc123"}, nil)
		srv.Close()
		var de *driver.Error
		if !errors.As(err, &de) {
			t.Fatalf("%s: expect driver.Error, got %v", c.want, err)
		}
		if de.Kind != c.want {
			t.Errorf("body %s: kind=%s want=%s", c.body, de.Kind, c.want)
		}
	}
}
