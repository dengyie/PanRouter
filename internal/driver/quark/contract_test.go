package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// 合约测试:用 httptest 回放夸克响应样本,不打真实网盘(设计文档 §9)。
// 分享直链为转存链:save → 轮询任务拿新 fid → file/download → cleanup delete(线上实测形态)。

const (
	tokenRespBody = `{"code":0,"message":"ok","data":{"stoken":"STOKEN"}}`
	detailRespBody = `{"code":0,"message":"ok","data":{"list":[{"fid":"F1","file_name":"a.mp4","size":123,"dir":false}],"metadata":{"_total":1}}}`
	saveRespBody   = `{"code":0,"message":"ok","data":{"task_id":"T1"}}`
	// 转存任务完成:新 fid 为 F2(与分享内 F1 不同)
	saveDoneRespBody = `{"code":0,"message":"ok","data":{"status":2,"save_as":{"save_as_select_top_fids":["F2"],"save_as_top_fids":["F2"]}}}`
	downloadRespBody = `{"code":0,"message":"ok","data":[{"download_url":"https://dl.example.com/file?f=1"}]}`
	deleteRespBody   = `{"code":0,"message":"ok","data":{}}`
)

func newTestDriver(t *testing.T, srv *httptest.Server) *Driver {
	t.Helper()
	cl, err := httpx.New(httpx.Options{AllowPrivate: true, RedirectAllow: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return New(cl, srv.URL, srv.URL)
}

func writeJSONLine(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// saveChainServer 标准转存链 fixture:token/detail/save/task/file download/delete。
func saveChainServer(t *testing.T, taskResp func(retryIndex int) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/token"):
			writeJSONLine(w, tokenRespBody)
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/detail"):
			writeJSONLine(w, detailRespBody)
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/save"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			// 请求体为线上实测:fid_list 精确选择单个文件
			sel, _ := body["fid_list"].([]any)
			if len(sel) != 1 || sel[0] != "F1" || body["pwd_id"] == "" || body["stoken"] == "" {
				writeJSONLine(w, `{"code":41013,"message":"转存失败"}`)
				return
			}
			writeJSONLine(w, saveRespBody)
		case strings.HasSuffix(r.URL.Path, "/clouddrive/task"):
			if r.URL.Query().Get("task_id") != "T1" || r.URL.Query().Get("retry_index") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			idx := 0
			_, _ = fmt.Sscanf(r.URL.Query().Get("retry_index"), "%d", &idx)
			writeJSONLine(w, taskResp(idx))
		case strings.HasSuffix(r.URL.Path, "/file/download"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fids, _ := body["fids"].([]any)
			if len(fids) != 1 || fids[0] != "F2" {
				writeJSONLine(w, `{"code":31001,"message":"请登录后操作"}`)
				return
			}
			w.Header().Add("Set-Cookie", "__puus=puusvalue; Path=/; Domain=quark.cn")
			writeJSONLine(w, downloadRespBody)
		case strings.HasSuffix(r.URL.Path, "/file/delete"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fids, _ := body["filelist"].([]any)
			if len(fids) != 1 || fids[0] != "F2" {
				writeJSONLine(w, `{"code":32003,"message":"删除失败"}`)
				return
			}
			writeJSONLine(w, deleteRespBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestResolveShare(t *testing.T) {
	srv := saveChainServer(t, func(int) string { return saveDoneRespBody })
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
}

func TestGetDirectLinkSaveChain(t *testing.T) {
	srv := saveChainServer(t, func(int) string { return saveDoneRespBody })
	defer srv.Close()
	d := newTestDriver(t, srv)

	// 分享直链必须携带登录态(转存依赖账号)
	_, err := d.GetDirectLink(context.Background(), nil, driver.FileRef{FID: "F1",
		Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	if err == nil {
		t.Fatal("无 Cookie 时应返回 AuthExpired")
	} else {
		var de *driver.Error
		if !errors.As(err, &de) || de.Kind != driver.KindAuthExpired {
			t.Fatalf("expect auth_expired, got %v", err)
		}
	}

	dl, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	if err != nil {
		t.Fatalf("GetDirectLink: %v", err)
	}
	if dl.URL != "https://dl.example.com/file?f=1" {
		t.Errorf("direct url: %s", dl.URL)
	}
	if !strings.Contains(dl.Cookie, "__puus=puusvalue") {
		t.Errorf("下载接口的 Set-Cookie 应并入直链 Cookie(aria2/中转需携带):%q", dl.Cookie)
	}
	// 线上实测:CDN 强校验完整登录 Cookie,直链 Cookie 必须以登录态为基底
	if !strings.Contains(dl.Cookie, "session=1") {
		t.Errorf("直链 Cookie 应以登录态为基底:%q", dl.Cookie)
	}
	if dl.UA != QuarkUA {
		t.Errorf("直链 UA 应为夸克客户端 UA: %s", dl.UA)
	}
}

// 转存任务轮询:status 未完成时按 retry_index 递增重试,完成后取 save_as 新 fid。
func TestSaveTaskPolling(t *testing.T) {
	srv := saveChainServer(t, func(idx int) string {
		if idx == 0 {
			return `{"code":0,"data":{"status":0}}`
		}
		return saveDoneRespBody
	})
	defer srv.Close()
	d := newTestDriver(t, srv)
	d.taskInterval = time.Millisecond

	dl, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	if err != nil {
		t.Fatalf("GetDirectLink(polling): %v", err)
	}
	if dl.URL != "https://dl.example.com/file?f=1" {
		t.Fatalf("task download url: %s", dl.URL)
	}
}

// 容量超限分类(线上实测形态):save 41013「转存失败」/ 任务轮询「capacity limit」→ risk_control。
func TestSaveCapacityClassification(t *testing.T) {
	srv := saveChainServer(t, func(int) string {
		return `{"status":400,"code":32003,"message":"capacity limit[{0}]"}`
	})
	defer srv.Close()
	d := newTestDriver(t, srv)
	d.taskInterval = time.Millisecond

	_, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	var de *driver.Error
	if !errors.As(err, &de) {
		t.Fatalf("expect driver.Error, got %v", err)
	}
	if de.Kind != driver.KindRiskControl {
		t.Errorf("capacity limit 应为 risk_control, got %s (%v)", de.Kind, de.UserHint)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		body string
		want driver.Kind
	}{
		{`{"code":32003,"message":"分享已经取消了"}`, driver.KindShareGone},
		{`{"code":41008,"message":"操作太频繁,请稍后再试"}`, driver.KindRiskControl},
		{`{"code":41013,"message":"转存失败"}`, driver.KindRiskControl},
		{`{"code":31001,"message":"请登录后操作"}`, driver.KindAuthExpired},
		{`{"code":70016,"message":"提取码错误"}`, driver.KindNotFound},
		{`<html>unexpected</html>`, driver.KindUpstream}, // 非 JSON → 上游异常
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSONLine(w, c.body)
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
