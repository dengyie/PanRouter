package api

// 端到端测试:真实 Router + SQLite + fake driver + mock 上游/aria2,
// 通过 HTTP 全栈验证鉴权、解析、路径决策、302 降级、中转、账号与下载任务。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/app"
	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/repo"
)

func nopLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

const (
	e2eShareURL = "https://fake.example/s/x"
	e2eUA       = "e2e-ua"
)

// e2eDriver:按 fid 返回不同约束的直链,驱动 §4.3 决策表的三条路径。
type e2eDriver struct{ upstream string }

func (d *e2eDriver) ID() string { return "fake" }
func (d *e2eDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{
		{FID: "f1", Name: "plain.bin", Size: 12, Ext: map[string]string{}},
		{FID: "fcookie", Name: "withcookie.bin", Size: 11, Ext: map[string]string{}},
		{FID: "fbind", Name: "bound.bin", Size: 11, Ext: map[string]string{}},
	}, nil
}

func (d *e2eDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	switch ref.FID {
	case "fcookie": // Cookie+UA+Referer 约束 → Can302=false → aria2/中转
		return driver.DirectLink{
			URL: d.upstream + "/file/c", UA: e2eUA,
			Referer: "https://pan.quark.cn/", Cookie: "session=abc",
			ExpiresAt: time.Now().Add(time.Hour),
		}, nil
	case "fbind": // 绑定解析 IP → cloud 画像或 aria2 异机时走中转
		return driver.DirectLink{URL: d.upstream + "/file/b", BindIP: true, ExpiresAt: time.Now().Add(time.Hour)}, nil
	default: // f1:裸链 → 浏览器 302
		return driver.DirectLink{URL: d.upstream + "/file/f1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
}
func (d *e2eDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true, Nickname: "e2e"}, nil
}

// e2eUpstream:直链目标(mock 网盘 CDN),支持 Range。
func e2eUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			w.Header().Set("Content-Range", "bytes 10-11/12")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("45"))
			return
		}
		switch r.URL.Path {
		case "/file/f1":
			_, _ = w.Write([]byte("UPSTREAM-f1"))
		case "/file/c":
			_, _ = w.Write([]byte("UPSTREAM-c"))
		case "/file/b":
			_, _ = w.Write([]byte("UPSTREAM-b"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// e2eAria2:mock aria2 JSON-RPC;捕获 addUri 请求体供断言。
func e2eAria2() (*httptest.Server, *string) {
	captured := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*captured = string(b)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "aria2.addUri":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"panrouter","result":"gid123"}`))
		case "aria2.tellStatus":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"panrouter","result":{"gid":"gid123","status":"complete","totalLength":"12","completedLength":"12","downloadSpeed":"0","errorMessage":""}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"panrouter","error":{"message":"unknown method"}}`))
		}
	}))
	return srv, captured
}

type e2eEnv struct {
	ts        *httptest.Server
	store     *repo.Store
	upstream  *httptest.Server
	aria2mock *httptest.Server
	aria2Body *string
	token     string
	client    *http.Client // 不跟随重定向,便于断言 302 Location
}

func newE2E(t *testing.T, sameHost bool) *e2eEnv {
	t.Helper()
	upstream := e2eUpstream()
	t.Cleanup(upstream.Close)
	aria2mock, aria2Body := e2eAria2()
	t.Cleanup(aria2mock.Close)

	cfg := config.Default()
	cfg.Server.DataDir = t.TempDir() // SQLite 落在临时目录
	cfg.Aria2.Endpoint = aria2mock.URL
	cfg.Aria2.SameHost = sameHost
	cfg.DomainRoutes = map[string][]string{"fake": {"fake.example"}}
	cfg.Drivers["fake"] = config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 10} // e2e 不测限频/并发闸,放开

	streamCl, err := httpx.New(httpx.Options{Timeout: 10 * time.Second, NoBodyTimeout: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}

	// 与生产 main 相同的装配路径(AllowPrivate 仅为 e2e 的 127.0.0.1 mock 放行)
	a, err := app.Build(app.Options{
		Cfg: cfg, Logger: nopLogger(), MasterKey: "e2e-master-key",
		Version: "e2e", AllowPrivateClients: true,
		ExtraDrivers: []app.ExtraDriver{{
			Driver:       &e2eDriver{upstream: upstream.URL},
			StreamClient: streamCl,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	deps := Deps{
		Version: "e2e", Cfg: a.Cfg, Resolver: a.Resolver, Relay: a.Relay, Aria2: a.Aria2,
		Accounts: a.Accounts, Store: a.Store, AES: a.AES, Signer: a.Signer,
		Log: a.Log, Met: a.Met, WebFS: nil,
	}
	ts := httptest.NewServer(Router(deps))
	t.Cleanup(ts.Close)

	// base_url 指向 e2e server 本身,下载链接才是可访问的(经 ApplyConfig 热生效)
	cfg.Server.BaseURL = ts.URL
	a.ApplyConfig(cfg)

	env := &e2eEnv{ts: ts, store: a.Store, upstream: upstream, aria2mock: aria2mock, aria2Body: aria2Body}
	env.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 登录拿 token
	st, body := env.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin123"}`)
	if st != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", st, body)
	}
	env.token = body["token"].(string)
	return env
}

// do 发送请求并解析 JSON;client 为 nil 时用不跟随重定向的默认客户端。
func (e *e2eEnv) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	client := e.client
	if client == nil {
		client = e.ts.Client()
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (e *e2eEnv) resolveFile(t *testing.T, fid string) map[string]any {
	t.Helper()
	// 模拟真实客户端流程:先列分享(落 share 快照),再解析单文件
	if st, body := e.do(t, "POST", "/api/v1/resolve", e.token, `{"url":"`+e2eShareURL+`"}`); st != http.StatusOK {
		t.Fatalf("resolve share: %d %v", st, body)
	}
	st, body := e.do(t, "POST", "/api/v1/resolve", e.token,
		`{"url":"`+e2eShareURL+`","fid":"`+fid+`"}`)
	if st != http.StatusOK {
		t.Fatalf("resolve %s: status=%d body=%v", fid, st, body)
	}
	return body
}

// ---- 鉴权 ----

func TestE2EAuthFlow(t *testing.T) {
	e := newE2E(t, true)

	if st, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"wrong"}`); st != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", st)
	}
	if st, _ := e.do(t, "GET", "/api/v1/accounts", "", ""); st != http.StatusUnauthorized {
		t.Fatalf("no token: %d", st)
	}
	// API Token 直连
	if st, _ := e.do(t, "GET", "/api/v1/accounts", config.DefaultAPIToken, ""); st != http.StatusOK {
		t.Fatalf("api token: %d", st)
	}
	// JWT
	if st, _ := e.do(t, "GET", "/api/v1/accounts", e.token, ""); st != http.StatusOK {
		t.Fatalf("jwt: %d", st)
	}
	// 伪造 token
	if st, _ := e.do(t, "GET", "/api/v1/accounts", e.token+"x", ""); st != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", st)
	}
	// /metrics 与 /healthz 公开
	if st, _ := e.do(t, "GET", "/metrics", "", ""); st != http.StatusOK {
		t.Fatalf("metrics: %d", st)
	}
}

// ---- 解析与三条下载路径 ----

func TestE2EResolveAndRoute302(t *testing.T) {
	e := newE2E(t, true)

	// 分享列表
	st, body := e.do(t, "POST", "/api/v1/resolve", e.token, `{"url":"`+e2eShareURL+`"}`)
	if st != http.StatusOK || len(body["files"].([]any)) != 3 {
		t.Fatalf("share: %d %v", st, body)
	}

	// f1:裸链 → route=302,/d 直接 302 到上游直链
	res := e.resolveFile(t, "f1")
	if res["route"] != "302" {
		t.Fatalf("f1 route: %v", res["route"])
	}
	req, _ := http.NewRequest("GET", res["download_url"].(string), nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("f1 /d status=%d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != res["direct_url"].(string) {
		t.Fatalf("f1 302 location=%s want %s", loc, res["direct_url"])
	}
	// 跟随后的最终内容
	final, err := http.Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Body.Close()
	b, _ := io.ReadAll(final.Body)
	if string(b) != "UPSTREAM-f1" {
		t.Fatalf("final body=%q", string(b))
	}

	// 第二次解析同一文件 → 缓存命中
	res2 := e.resolveFile(t, "f1")
	if res2["cache_hit"] != true {
		t.Fatalf("cache_hit=%v", res2["cache_hit"])
	}
}

func TestE2ECookieLinkDegradesToStream(t *testing.T) {
	e := newE2E(t, true)

	res := e.resolveFile(t, "fcookie")
	if res["route"] != "aria2" {
		t.Fatalf("fcookie route=%v (带 Cookie 时浏览器 302 不可用,应选 aria2)", res["route"])
	}
	if res["need_headers"] != true {
		t.Fatalf("need_headers=%v", res["need_headers"])
	}

	// /d 对 Can302=false 的直链应无感降级到 /stream
	req, _ := http.NewRequest("GET", res["download_url"].(string), nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("degrade status=%d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/stream/fake/") {
		t.Fatalf("degrade location=%s", loc)
	}

	// 中转流返回上游内容
	resp2, err := e.ts.Client().Get(e.ts.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	got, _ := io.ReadAll(resp2.Body)
	if string(got) != "UPSTREAM-c" {
		t.Fatalf("stream body=%q", string(got))
	}
}

func TestE2EStreamRange(t *testing.T) {
	e := newE2E(t, true)
	res := e.resolveFile(t, "f1")

	req, _ := http.NewRequest("GET", res["stream_url"].(string), nil)
	req.Header.Set("Range", "bytes=10-")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status=%d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Range") != "bytes 10-11/12" {
		t.Fatalf("content-range=%q", resp.Header.Get("Content-Range"))
	}
}

func TestE2EBadSignatureRejected(t *testing.T) {
	e := newE2E(t, true)
	if st, body := e.do(t, "GET", "/d/fake/k/f?sig=bad", "", ""); st != http.StatusNotFound {
		t.Fatalf("bad sig: %d %v", st, body)
	}
}

// ---- 账号 ----

func TestE2EAccountsCRUD(t *testing.T) {
	e := newE2E(t, true)

	// 空 Cookie 拒绝
	st, _ := e.do(t, "POST", "/api/v1/accounts", e.token, `{"pan_type":"fake","name":"a","cookie":"  "}`)
	if st != http.StatusNotFound { // KindNotFound → 404
		t.Fatalf("empty cookie: %d", st)
	}
	// 正常创建
	st, body := e.do(t, "POST", "/api/v1/accounts", e.token, `{"pan_type":"fake","name":"acc1","cookie":"CK=1"}`)
	if st != http.StatusOK {
		t.Fatalf("create: %d %v", st, body)
	}

	// 列表不得泄漏凭据
	st, body = e.do(t, "GET", "/api/v1/accounts", e.token, "")
	if st != http.StatusOK {
		t.Fatal(st)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "CK=1") {
		t.Fatal("credential leaked in account list")
	}

	// 刷新:fake driver 校验通过(CredStatus 未打 json tag,键为大写 Valid)
	st, body = e.do(t, "POST", "/api/v1/accounts/1/refresh", e.token, "")
	if st != http.StatusOK {
		t.Fatalf("refresh: %d %v", st, body)
	}
	if body["status"].(map[string]any)["Valid"] != true {
		t.Fatalf("refresh status: %v", body["status"])
	}

	// 删除
	if st, _ := e.do(t, "DELETE", "/api/v1/accounts/1", e.token, ""); st != http.StatusOK {
		t.Fatal("delete")
	}
	st, body = e.do(t, "GET", "/api/v1/accounts", e.token, "")
	if len(body["accounts"].([]any)) != 0 {
		t.Fatalf("after delete: %v", body)
	}
}

// ---- 下载任务 ----

func TestE2EDownloadsPushDirectHeaders(t *testing.T) {
	e := newE2E(t, true) // aria2 同机 → fcookie 走直链+headers
	res := e.resolveFile(t, "fcookie")

	st, body := e.do(t, "POST", "/api/v1/downloads", e.token,
		`{"pan":"fake","share_key":"`+res["share_key"].(string)+`","fid":"fcookie","dest":"/tmp/dl"}`)
	if st != http.StatusOK || body["gid"] != "gid123" {
		t.Fatalf("push: %d %v", st, body)
	}
	if body["route"] != "aria2" {
		t.Fatalf("push route=%v", body["route"])
	}
	// aria2 收到的 URI 必须是直链,且带 Cookie/UA header
	if !strings.Contains(*e.aria2Body, "/file/c") || !strings.Contains(*e.aria2Body, "Cookie: session=abc") {
		t.Fatalf("aria2 params: %s", *e.aria2Body)
	}

	// pan 不一致 → 404
	st, _ = e.do(t, "POST", "/api/v1/downloads", e.token,
		`{"pan":"other","share_key":"`+res["share_key"].(string)+`","fid":"fcookie"}`)
	if st != http.StatusNotFound {
		t.Fatalf("pan mismatch: %d", st)
	}

	// 状态查询
	st, body = e.do(t, "GET", "/api/v1/downloads/gid123", e.token, "")
	if st != http.StatusOK || body["status"] != "complete" || body["total"].(float64) != 12 {
		t.Fatalf("status: %d %v", st, body)
	}
}

func TestE2EDownloadsPushStreamFallback(t *testing.T) {
	e := newE2E(t, false) // aria2 异机 + BindIP → 必须走 /stream URL
	res := e.resolveFile(t, "fbind")
	if res["route"] != "stream" {
		t.Fatalf("fbind route=%v", res["route"])
	}
	st, body := e.do(t, "POST", "/api/v1/downloads", e.token,
		`{"pan":"fake","share_key":"`+res["share_key"].(string)+`","fid":"fbind"}`)
	if st != http.StatusOK || body["route"] != "stream" {
		t.Fatalf("push: %d %v", st, body)
	}
	if !strings.Contains(*e.aria2Body, e.ts.URL+"/stream/fake/") {
		t.Fatalf("aria2 should download PanRouter stream url: %s", *e.aria2Body)
	}
}

// ---- 批量 / 系统 ----

func TestE2EBatchResolve(t *testing.T) {
	e := newE2E(t, true)
	st, body := e.do(t, "POST", "/api/v1/resolve/batch", e.token,
		`{"items":[{"url":"`+e2eShareURL+`"},{"url":"::::"}]}`)
	if st != http.StatusOK {
		t.Fatal(st)
	}
	results := body["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results=%v", results)
	}
	first := results[0].(map[string]any)
	second := results[1].(map[string]any)
	if first["ok"] != true || second["ok"] != false {
		t.Fatalf("batch: %v %v", first, second)
	}
}

func TestE2ESystemEndpoints(t *testing.T) {
	e := newE2E(t, true)
	if st, body := e.do(t, "GET", "/healthz", "", ""); st != 200 || body["status"] != "ok" {
		t.Fatalf("healthz: %d %v", st, body)
	}
	if st, _ := e.do(t, "GET", "/readyz", "", ""); st != 200 {
		t.Fatal("readyz")
	}
	// 产生一次解析后,指标应出现 fake 网盘计数(label 按字母序输出)
	e.resolveFile(t, "f1")
	resp, err := e.ts.Client().Get(e.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "panrouter_resolve_total") || !strings.Contains(string(b), `pan="fake"`) {
		t.Fatalf("metrics missing resolve counter:\n%s", string(b))
	}
	// 未知路径 → 404 JSON
	if st, _ := e.do(t, "GET", "/nope", "", ""); st != http.StatusNotFound {
		t.Fatal("404 route")
	}
}
