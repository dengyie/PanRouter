package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// 合约测试:用 httptest 回放夸克响应样本,不打真实网盘(设计文档 §9)。
// 分享直链为转存链:建暂存目录 → save → 轮询任务拿新 fid → file/download → cleanup delete(线上实测形态)。

const (
	tokenRespBody  = `{"code":0,"message":"ok","data":{"stoken":"STOKEN"}}`
	detailRespBody = `{"code":0,"message":"ok","data":{"list":[{"fid":"F1","file_name":"a.mp4","size":123,"dir":false}],"metadata":{"_total":1}}}`
	saveRespBody   = `{"code":0,"message":"ok","data":{"task_id":"T1"}}`
	mkdirRespBody  = `{"code":0,"message":"ok","data":{"fid":"TDIR"}}`
	// 转存任务完成:新 fid 为 F2(与分享内 F1 不同)
	saveDoneRespBody = `{"code":0,"message":"ok","data":{"status":2,"save_as":{"save_as_select_top_fids":["F2"],"save_as_top_fids":["F2"]}}}`
	downloadRespBody = `{"code":0,"message":"ok","data":[{"download_url":"https://dl.example.com/file?f=1"}]}`
	deleteRespBody   = `{"code":0,"message":"ok","data":{}}`
	tmpDirFID        = "TDIR"
)

func newTestDriver(t *testing.T, srv *httptest.Server) *Driver {
	t.Helper()
	cl, err := httpx.New(httpx.Options{AllowPrivate: true, RedirectAllow: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return New(cl, srv.URL)
}

func writeJSONLine(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// chainServer 可配置的转存链 fixture:token/detail/mkdir/save/task/file download/delete。
// 各回调可覆盖默认(成功)行为,用于构造轮询、容量、stoken 过期、清理等场景。
type chainServer struct {
	taskResp     func(idx int) string
	saveResp     func(call int) string // call: 第几次 save 调用(0 起)
	downloadResp func() (int, string)
	detailResp   func(pdir string) string
	deletes      []string // 记录 cleanup 删除的 fid
	mkdirCalls   int
	saveCall     int
	mu           sync.Mutex
}

func (c *chainServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/token"):
			writeJSONLine(w, tokenRespBody)
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/detail"):
			if c.detailResp != nil {
				writeJSONLine(w, c.detailResp(r.URL.Query().Get("pdir_fid")))
				return
			}
			writeJSONLine(w, detailRespBody)
		case strings.HasSuffix(r.URL.Path, "/share/sharepage/save"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.mu.Lock()
			call := c.saveCall
			c.saveCall++
			fn := c.saveResp
			c.mu.Unlock()
			if fn != nil {
				writeJSONLine(w, fn(call))
				return
			}
			// 请求体为线上实测:fid_list 精确选择单个文件,转存到暂存目录 TDIR
			sel, _ := body["fid_list"].([]any)
			if len(sel) != 1 || sel[0] != "F1" || body["pwd_id"] == "" ||
				body["stoken"] == "" || body["to_pdir_fid"] != tmpDirFID {
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
			writeJSONLine(w, c.taskResp(idx))
		case strings.HasSuffix(r.URL.Path, "/file/download"):
			if c.downloadResp != nil {
				code, body := c.downloadResp()
				w.WriteHeader(code)
				writeJSONLine(w, body)
				return
			}
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
			c.mu.Lock()
			if len(fids) == 1 {
				c.deletes = append(c.deletes, fmt.Sprint(fids[0]))
			}
			c.mu.Unlock()
			writeJSONLine(w, deleteRespBody)
		case strings.HasSuffix(r.URL.Path, "/1/clouddrive/file"):
			c.mu.Lock()
			c.mkdirCalls++
			c.mu.Unlock()
			writeJSONLine(w, mkdirRespBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// saveCall 计数器(放在链上,避免 handler 闭包捕获多次)。
func (c *chainServer) saveCallCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.saveCall }

func newChainServer(t *testing.T, cs *chainServer) *httptest.Server {
	t.Helper()
	if cs.taskResp == nil {
		cs.taskResp = func(int) string { return saveDoneRespBody }
	}
	return httptest.NewServer(cs.handler())
}

func TestResolveShare(t *testing.T) {
	srv := newChainServer(t, &chainServer{})
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

func TestResolveShareWalksDirectory(t *testing.T) {
	cs := &chainServer{detailResp: func(pdir string) string {
		if pdir == "DIR1" {
			return `{"code":0,"message":"ok","data":{"list":[{"fid":"F1","file_name":"a.mp4","size":123,"dir":false}],"metadata":{"_total":1}}}`
		}
		return `{"code":0,"message":"ok","data":{"list":[{"fid":"DIR1","file_name":"MuMu模拟器","size":0,"dir":true}],"metadata":{"_total":1}}}`
	}}
	srv := newChainServer(t, cs)
	defer srv.Close()
	d := newTestDriver(t, srv)

	nodes, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/s/abc123"}, nil)
	if err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 file after walking dir, got %+v", nodes)
	}
	if nodes[0].FID != "F1" || nodes[0].IsDir || nodes[0].Name != "MuMu模拟器/a.mp4" {
		t.Fatalf("unexpected flattened node: %+v", nodes[0])
	}
}

func TestGetDirectLinkSaveChain(t *testing.T) {
	cs := &chainServer{}
	srv := newChainServer(t, cs)
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
	// 转存副本必须被清理
	if len(cs.deletes) != 1 || cs.deletes[0] != "F2" {
		t.Errorf("转存副本应被删除,实际删除:%v", cs.deletes)
	}
}

// 暂存目录按登录态缓存:同账号多次提链只建一次目录。
func TestTmpDirCachedPerCredential(t *testing.T) {
	cs := &chainServer{}
	srv := newChainServer(t, cs)
	defer srv.Close()
	d := newTestDriver(t, srv)
	cred := &driver.Credential{Cookie: "session=1"}
	ref := driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}}

	for i := 0; i < 2; i++ {
		if _, err := d.GetDirectLink(context.Background(), cred, ref); err != nil {
			t.Fatalf("GetDirectLink #%d: %v", i, err)
		}
	}
	if cs.mkdirCalls != 1 {
		t.Errorf("暂存目录应按登录态缓存,建目录调用 %d 次,期望 1", cs.mkdirCalls)
	}
}

// 转存任务轮询:status 未完成时按 retry_index 递增重试,完成后取 save_as 新 fid。
func TestSaveTaskPolling(t *testing.T) {
	cs := &chainServer{taskResp: func(idx int) string {
		if idx == 0 {
			return `{"code":0,"data":{"status":0}}`
		}
		return saveDoneRespBody
	}}
	srv := newChainServer(t, cs)
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

// stoken 过期是常态路径:首次 save 返回 stoken 过期 → 重取 stoken 后重试成功。
func TestStokenExpiredRetry(t *testing.T) {
	cs := &chainServer{saveResp: func(call int) string {
		if call == 0 {
			return `{"code":41010,"message":"分享的stoken过期"}`
		}
		return saveRespBody
	}}
	srv := newChainServer(t, cs)
	defer srv.Close()
	d := newTestDriver(t, srv)
	d.taskInterval = time.Millisecond

	dl, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "EXPIRED"}})
	if err != nil {
		t.Fatalf("stoken 过期后应重取并重试成功, got %v", err)
	}
	if dl.URL != "https://dl.example.com/file?f=1" {
		t.Fatalf("download url: %s", dl.URL)
	}
	if cs.saveCallCount() != 2 {
		t.Errorf("应重试一次 save(共 2 次),实际 %d 次", cs.saveCallCount())
	}
}

// 取直链失败时,转存副本仍须清理(避免网盘空间静默泄漏)。
func TestCleanupOnDownloadFailure(t *testing.T) {
	cs := &chainServer{downloadResp: func() (int, string) {
		return http.StatusOK, `{"code":31001,"message":"请登录后操作"}`
	}}
	srv := newChainServer(t, cs)
	defer srv.Close()
	d := newTestDriver(t, srv)
	d.taskInterval = time.Millisecond

	_, err := d.GetDirectLink(context.Background(), &driver.Credential{Cookie: "session=1"},
		driver.FileRef{FID: "F1", Ext: map[string]string{"pwd_id": "abc123", "stoken": "STOKEN"}})
	if err == nil {
		t.Fatal("取链失败应返回错误")
	}
	if len(cs.deletes) != 1 || cs.deletes[0] != "F2" {
		t.Errorf("取链失败也必须清理转存副本,实际删除:%v", cs.deletes)
	}
}

// 容量超限分类(线上实测形态):任务轮询「capacity limit」→ risk_control。
func TestSaveCapacityClassification(t *testing.T) {
	cs := &chainServer{taskResp: func(int) string {
		return `{"status":400,"code":32003,"message":"capacity limit[{0}]"}`
	}}
	srv := newChainServer(t, cs)
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

// stoken 过期必须分类为 upstream(可重试),不得误判为 share_gone。
func TestStokenClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONLine(w, `{"code":41010,"message":"分享的stoken过期"}`)
	}))
	defer srv.Close()
	d := newTestDriver(t, srv)

	_, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: srv.URL + "/s/abc123"}, nil)
	var de *driver.Error
	if !errors.As(err, &de) {
		t.Fatalf("expect driver.Error, got %v", err)
	}
	if de.Kind != driver.KindUpstream {
		t.Errorf("stoken 过期应为 upstream, got %s", de.Kind)
	}
	if !stokenExpired(err) {
		t.Errorf("stokenExpired 应识别该错误")
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
