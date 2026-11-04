package api

// 端到端测试:真实 Router + SQLite + fake driver + mock 上游/aria2,
// 通过 HTTP 全栈验证鉴权、解析、路径决策、302 降级、中转、账号与下载任务。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/app"
	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/driver/quark"
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

	// 扫码登录上游(mock CAS + account/info):真 quark driver 注入,端点指向 mock。
	qrMockMux := http.NewServeMux()
	qrMockMux.HandleFunc("/cas/ajax/getTokenForQrcodeLogin", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 2000000, "message": "ok",
			"data": map[string]any{"members": map[string]any{"token": "e2e-tok"}},
		})
	})
	qrMockMux.HandleFunc("/cas/ajax/getServiceTicketByQrcodeToken", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 2000000, "message": "ok",
			"data": map[string]any{"members": map[string]any{"service_ticket": "e2e-st"}},
		})
	})
	qrMockMux.HandleFunc("/account/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "__pus=e2e; domain=.quark.cn; path=/")
		w.Header().Add("Set-Cookie", "__puus=e2e2; domain=.quark.cn; path=/")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 2000000})
	})
	qrUpstream := httptest.NewServer(qrMockMux)
	t.Cleanup(qrUpstream.Close)
	apiCl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	qrDriver := quark.New(apiCl, "https://drive-pc.quark.cn")
	qrDriver.QRUopDomain = qrUpstream.URL
	qrDriver.QRInfoURL = qrUpstream.URL + "/account/info"

	// 与生产 main 相同的装配路径(AllowPrivate 仅为 e2e 的 127.0.0.1 mock 放行)
	a, err := app.Build(app.Options{
		Cfg: cfg, Logger: nopLogger(), MasterKey: "e2e-master-key",
		AllowPrivateClients: true,
		ExtraDrivers: []app.ExtraDriver{{
			Driver:       &e2eDriver{upstream: upstream.URL},
			StreamClient: streamCl,
		}, {
			Driver:    qrDriver,
			APIClient: apiCl,
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

	// 游客可解析免登录盘;账号与 aria2 仍需登录
	st, body := e.do(t, "POST", "/api/v1/resolve", "", `{"url":"`+e2eShareURL+`"}`)
	if st != http.StatusOK {
		t.Fatalf("guest resolve: %d %v", st, body)
	}
	files, _ := body["files"].([]any)
	if len(files) == 0 {
		t.Fatalf("guest resolve empty: %v", body)
	}
	first := files[0].(map[string]any)
	if first["download_url"] == nil || first["download_url"] == "" {
		t.Fatalf("guest auto-link missing download_url: %v", first)
	}
	if st, _ := e.do(t, "POST", "/api/v1/downloads", "", `{"pan":"fake","share_key":"x","fid":"f1"}`); st != http.StatusUnauthorized {
		t.Fatalf("guest downloads: %d", st)
	}
	if st, _ := e.do(t, "POST", "/api/v1/resolve", e.token+"x", `{"url":"`+e2eShareURL+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("forged token resolve: %d", st)
	}
}

// ---- 解析与三条下载路径 ----

func TestE2EResolveAndRoute302(t *testing.T) {
	e := newE2E(t, true)

	// 分享列表:一次解析即自动提链,文件行应带 download_url
	st, body := e.do(t, "POST", "/api/v1/resolve", e.token, `{"url":"`+e2eShareURL+`"}`)
	if st != http.StatusOK || len(body["files"].([]any)) != 3 {
		t.Fatalf("share: %d %v", st, body)
	}
	first := body["files"].([]any)[0].(map[string]any)
	if first["download_url"] == nil || first["download_url"] == "" {
		t.Fatalf("share auto-link missing download_url: %v", first)
	}

	// f1:裸链 → route=302,/d 直接 302 到上游直链(分享解析已提过,此处为缓存命中)
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

// 批量预算契约(§6.4):超限 400 + kind=unsupported;上限值与常量一致。
func TestE2EBatchResolveLimit(t *testing.T) {
	e := newE2E(t, true)
	items := `{"items":[` + strings.Repeat(`{"url":"::::"},`, ResolveBatchMaxItems) + `{"url":"::::"}]}`
	st, body := e.do(t, "POST", "/api/v1/resolve/batch", e.token, items)
	if st != http.StatusBadRequest || body["kind"] != string(driver.KindUnsupported) {
		t.Fatalf("over-limit must be 400/unsupported: %d %v", st, body["kind"])
	}
	if st, _ := e.do(t, "POST", "/api/v1/resolve/batch", e.token, `{"items":[]}`); st != http.StatusBadRequest {
		t.Fatalf("empty items must be 400: %d", st)
	}
}

// 扫码登录全链路(e2e):取 token → 轮询 confirmed → 账号自动入库且可查询。
func TestE2EQuarkQRLogin(t *testing.T) {
	e := newE2E(t, true)
	st, body := e.do(t, "GET", "/api/v1/accounts/quark/qr/token", e.token, "")
	if st != 200 || body["token"] != "e2e-tok" {
		t.Fatalf("qr token: %d %v", st, body)
	}
	if u, _ := body["url"].(string); !strings.HasPrefix(u, "https://su.quark.cn/") {
		t.Fatalf("qr url: %v", body["url"])
	}
	// png 字段:后端直出 data URI,前端 <img src> 零依赖渲染;必须是合法 PNG。
	pngURI, _ := body["png"].(string)
	const pngPrefix = "data:image/png;base64,"
	if !strings.HasPrefix(pngURI, pngPrefix) {
		t.Fatalf("qr png data URI missing: %q", pngURI)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pngURI, pngPrefix))
	if err != nil || len(raw) < 8 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("qr png invalid: err=%v magic=%x", err, raw[:min(8, len(raw))])
	}

	st, body = e.do(t, "POST", "/api/v1/accounts/quark/qr/poll", e.token,
		`{"token":"e2e-tok","name":"扫码号"}`)
	if st != 200 || body["status"] != "confirmed" {
		t.Fatalf("poll confirmed: %d %v", st, body)
	}
	acc, _ := body["account"].(map[string]any)
	if acc == nil || acc["id"] == nil || acc["pan_type"] != "quark" || acc["status"] != "ok" {
		t.Fatalf("account: %v", acc)
	}

	// 账号列表可见;refresh 走真 quark driver(凭据无效是预期,断言只到端点可达)
	st, body = e.do(t, "GET", "/api/v1/accounts", e.token, "")
	if st != 200 {
		t.Fatalf("list accounts: %d", st)
	}
	list, _ := body["accounts"].([]any)
	found := false
	for _, it := range list {
		if a, ok := it.(map[string]any); ok && a["pan_type"] == "quark" {
			found = true
		}
	}
	if !found {
		t.Fatalf("quark account not listed: %v", body)
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

// ---- 设备历史同步(M3) ----

// syncPush 构造一条推送条目的 JSON。
func syncPush(deviceID, kind, uid string, deleted bool, updatedAt int64, payload string) string {
	return `{"device_id":"` + deviceID + `","items":[{"kind":"` + kind + `","uid":"` + uid +
		`","deleted":` + boolJSON(deleted) + `,"updated_at":` + strconvI64(updatedAt) +
		`,"payload":` + payload + `}]}`
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func strconvI64(v int64) string { return strconv.FormatInt(v, 10) }

func TestE2ESyncPushPullRoundtrip(t *testing.T) {
	e := newE2E(t, true)

	// 推三类条目
	push := func(body string) (int, map[string]any) {
		return e.do(t, "POST", "/api/v1/sync", e.token, body)
	}
	if st, b := push(syncPush("pc-1", "bookmark", "u-bm", false, 1000, `{"link":"https://pan.quark.cn/s/abc","title":"资源","platform":"quark"}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("push bookmark: status=%d body=%v", st, b)
	}
	if st, b := push(syncPush("pc-1", "link_history", "u-lh", false, 1100, `{"url":"https://pan.quark.cn/s/def","title":"历史","platform":"quark"}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("push link_history: status=%d body=%v", st, b)
	}
	if st, b := push(syncPush("pc-2", "download_record", "u-dl", false, 1200, `{"share_url":"https://pan.quark.cn/s/ghi","file_name":"a.zip","platform":"quark","total_size":123}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("push download_record: status=%d body=%v", st, b)
	}

	// 全量拉回:按 id 升序,payload 原样保留
	st, b := e.do(t, "GET", "/api/v1/sync?since=0", e.token, "")
	if st != http.StatusOK {
		t.Fatalf("pull: status=%d body=%v", st, b)
	}
	items := b["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d: %v", len(items), b)
	}
	lastSeq := b["last_seq"].(float64)
	if lastSeq < 3 {
		t.Fatalf("last_seq should be >=3, got %v", lastSeq)
	}
	first := items[0].(map[string]any)
	if first["kind"].(string) != "bookmark" || first["uid"].(string) != "u-bm" ||
		first["device_id"].(string) != "pc-1" || first["deleted"].(bool) {
		t.Fatalf("first item mismatch: %v", first)
	}
	if first["client_updated_at"].(float64) != 1000 {
		t.Fatalf("client_updated_at mismatch: %v", first)
	}
	var pl map[string]any
	if err := json.Unmarshal([]byte(first["payload"].(string)), &pl); err != nil || pl["title"] != "资源" {
		t.Fatalf("payload roundtrip broken: %v err=%v", first["payload"], err)
	}

	// 游标之后无新数据
	if st, b2 := e.do(t, "GET", "/api/v1/sync?since="+strconvI64(int64(lastSeq)), e.token, ""); st != http.StatusOK || len(b2["items"].([]any)) != 0 {
		t.Fatalf("pull since=last_seq should be empty: %d %v", st, b2)
	}

	// 墓碑传播
	if st, b := push(syncPush("pc-1", "bookmark", "u-bm", true, 2000, `{}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("tombstone push: %d %v", st, b)
	}
	_, b = e.do(t, "GET", "/api/v1/sync?since="+strconvI64(int64(lastSeq)), e.token, "")
	items = b["items"].([]any)
	if len(items) != 1 || !items[0].(map[string]any)["deleted"].(bool) {
		t.Fatalf("tombstone not propagated: %v", b)
	}
}

func TestE2ESyncLWWArbitration(t *testing.T) {
	e := newE2E(t, true)
	push := func(body string) (int, map[string]any) {
		return e.do(t, "POST", "/api/v1/sync", e.token, body)
	}
	// 先推 v2(较新)
	if st, b := push(syncPush("pc-1", "bookmark", "u-1", false, 2000, `{"title":"v2"}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("push v2: %d %v", st, b)
	}
	// 再推 v1(较旧)→ 被 LWW 拒绝
	st, b := push(syncPush("pc-1", "bookmark", "u-1", false, 1000, `{"title":"v1"}`))
	if st != http.StatusOK || b["skipped"].(float64) != 1 {
		t.Fatalf("stale push should be skipped: %d %v", st, b)
	}
	// 服务端仍是 v2
	_, b = e.do(t, "GET", "/api/v1/sync?since=0", e.token, "")
	item := b["items"].([]any)[0].(map[string]any)
	if item["client_updated_at"].(float64) != 2000 || !strings.Contains(item["payload"].(string), "v2") {
		t.Fatalf("LWW failed, server kept wrong version: %v", item)
	}
	// 相同 uid 的新版本被接受且拿到新序号(游标推进)
	oldSeq := item["id"].(float64)
	if st, b := push(syncPush("pc-1", "bookmark", "u-1", false, 3000, `{"title":"v3"}`)); st != http.StatusOK || b["accepted"].(float64) != 1 {
		t.Fatalf("push v3: %d %v", st, b)
	}
	_, b = e.do(t, "GET", "/api/v1/sync?since="+strconvI64(int64(oldSeq)), e.token, "")
	items := b["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"].(float64) <= oldSeq {
		t.Fatalf("updated item must get a new larger seq: %v", b)
	}
}

func TestE2ESyncValidation(t *testing.T) {
	e := newE2E(t, true)
	cases := []struct {
		name string
		body string
	}{
		{"bad kind", syncPush("pc-1", "bookmark2", "u-1", false, 1, `{}`)},
		{"empty uid", `{"device_id":"pc-1","items":[{"kind":"bookmark","uid":"","deleted":false,"updated_at":1,"payload":{}}]}`},
		{"empty device", `{"device_id":"","items":[{"kind":"bookmark","uid":"u","deleted":false,"updated_at":1,"payload":{}}]}`},
		{"empty items", `{"device_id":"pc-1","items":[]}`},
	}
	for _, c := range cases {
		if st, b := e.do(t, "POST", "/api/v1/sync", e.token, c.body); st != http.StatusBadRequest {
			t.Fatalf("%s: want 400 got %d body=%v", c.name, st, b)
		}
	}
	// payload 超限
	big := `{"pad":"` + strings.Repeat("x", 17*1024) + `"}`
	if st, _ := e.do(t, "POST", "/api/v1/sync", e.token, syncPush("pc-1", "bookmark", "u-big", false, 1, big)); st != http.StatusBadRequest {
		t.Fatal("oversized payload should 400")
	}
	// 非法 since
	if st, _ := e.do(t, "GET", "/api/v1/sync?since=abc", e.token, ""); st != http.StatusBadRequest {
		t.Fatal("bad since should 400")
	}
	// 非法 limit
	if st, _ := e.do(t, "GET", "/api/v1/sync?since=0&limit=99999", e.token, ""); st != http.StatusBadRequest {
		t.Fatal("bad limit should 400")
	}
}

// TestE2ESyncPullCursorRewind 覆盖「服务端库回档后客户端游标超前」的收敛路径:
// 空页必须回报库内真实最大序号,否则客户端会认为已追平而永久脱离同步。
func TestE2ESyncPullCursorRewind(t *testing.T) {
	e := newE2E(t, true)
	if st, b := e.do(t, "POST", "/api/v1/sync", e.token, syncPush("pc-1", "bookmark", "u-1", false, 1000, `{"title":"v1"}`)); st != http.StatusOK {
		t.Fatalf("push: %d %v", st, b)
	}
	_, b := e.do(t, "GET", "/api/v1/sync?since=0", e.token, "")
	maxID := b["last_seq"].(float64)
	if maxID <= 0 {
		t.Fatalf("last_seq should be > 0, got %v", b)
	}
	// 客户端游标远超前(模拟库回档/换机迁移误带游标):必须回落真实最大值
	st, b2 := e.do(t, "GET", "/api/v1/sync?since=999999", e.token, "")
	if st != http.StatusOK || len(b2["items"].([]any)) != 0 {
		t.Fatalf("pull beyond max should be empty: %d %v", st, b2)
	}
	if got := b2["last_seq"].(float64); got != maxID {
		t.Fatalf("empty page must report real max seq %v (rewind), got %v", maxID, got)
	}
	// 游标收敛后再拉:仍为空且保持稳定(不振荡)
	_, b3 := e.do(t, "GET", "/api/v1/sync?since="+strconvI64(int64(maxID)), e.token, "")
	if got := b3["last_seq"].(float64); got != maxID {
		t.Fatalf("cursor should stay at max seq, got %v", got)
	}
}

// TestE2ESyncPushBodyCap 请求体上限:解码是全量缓冲,失控/恶意客户端灌大 body 必须
// 被 4xx 按内容拒绝(而非缓冲到内存或按 404 让客户端无限重试)。
func TestE2ESyncPushBodyCap(t *testing.T) {
	e := newE2E(t, true)
	item := `{"kind":"bookmark","uid":"u","deleted":false,"updated_at":1,"payload":{"a":"` + strings.Repeat("x", 1024) + `"}},`
	big := `{"device_id":"pc-1","items":[` + strings.Repeat(item, 11000) + `]}`
	st, b := e.do(t, "POST", "/api/v1/sync", e.token, big)
	if st != http.StatusBadRequest {
		t.Fatalf("oversize body should be 400, got %d %v", st, b)
	}
	if k, _ := b["kind"].(string); k != "unsupported" {
		t.Fatalf("oversize body kind should be unsupported, got %v", b)
	}
	// 正常体不受影响
	if st, b := e.do(t, "POST", "/api/v1/sync", e.token, syncPush("pc-1", "bookmark", "u-1", false, 1, `{"title":"v1"}`)); st != http.StatusOK {
		t.Fatalf("normal push after cap: %d %v", st, b)
	}
}

func TestE2ESyncAuthRequired(t *testing.T) {
	e := newE2E(t, true)
	st, b := e.do(t, "POST", "/api/v1/sync", "", syncPush("pc-1", "bookmark", "u-1", false, 1, `{}`))
	if st != http.StatusUnauthorized || b["kind"] != "session_expired" {
		t.Fatalf("unauthenticated push: %d %v", st, b)
	}
	st, b = e.do(t, "GET", "/api/v1/sync?since=0", "", "")
	if st != http.StatusUnauthorized || b["kind"] != "session_expired" {
		t.Fatalf("unauthenticated pull: %d %v", st, b)
	}
}
