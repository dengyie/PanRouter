package service

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/breaker"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/limiter"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
)

type fakeDriver struct {
	linkURL   string
	serverURL string // 可选:服务端自取直链;空 = 复用 linkURL
}

func (f *fakeDriver) ID() string { return "fake" }
func (f *fakeDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{{FID: "f1", Name: "n.bin", Size: 300, Ext: map[string]string{}}}, nil
}
func (f *fakeDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{URL: f.linkURL, ServerURL: f.serverURL, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (f *fakeDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true}, nil
}

// fixture:完整装配 Resolver + Relay,直链指向 mock 上游;返回 shareKey 供 StreamInput 使用。
func newRelayFixture(t *testing.T, upstreamURL string) (*Relay, string) {
	t.Helper()
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	cfgp := &config.Provider{}
	cfgp.Set(cfg)
	aes := crypto.New("test-key")
	log := zap.NewNop().Sugar()

	fd := &fakeDriver{linkURL: upstreamURL + "/file"}
	reg := driver.NewRegistry([]driver.Driver{fd}, map[string][]string{"fake": {"fake.example"}})
	met := metrics.New()
	acc := NewAccountService(store, aes, met, log)
	resolver := NewResolver(cfgp, reg, store, aes, acc, limiter.New(),
		breaker.NewRegistry(5, time.Minute), sign.New("k"), met, log)

	const shareURL = "https://fake.example/s/x"
	if _, err := resolver.ResolveShare(context.Background(), shareURL, "", ""); err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if _, err := resolver.ResolveFile(context.Background(), shareURL, "", "f1", false, ""); err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}

	streamCl, err := httpx.New(httpx.Options{
		Timeout: 300 * time.Millisecond, NoBodyTimeout: true, AllowPrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	relay := NewRelay(resolver, map[string]*httpx.Client{"fake": streamCl}, store, aes, met, log)
	return relay, resolver.shareKey("fake", shareURL, "")
}

// P1 回归:响应头即时返回、体传输持续超过 Timeout 的慢流必须完整送达,不得截断。
func TestRelaySlowStreamNotTruncated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 128)))
			fl.Flush()
			time.Sleep(200 * time.Millisecond) // 总时长 ~600ms > stream client Timeout 300ms
		}
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "")
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if rec.Body.Len() != 384 {
		t.Fatalf("body truncated: got %d bytes, want 384", rec.Body.Len())
	}
}

func TestRelayRangePassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=100-" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", "bytes 100-299/300")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("y", 200)))
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	err := relay.Serve(context.Background(), rec,
		StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "bytes=100-")
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", rec.Code)
	}
	if rec.Header().Get("Content-Range") != "bytes 100-299/300" {
		t.Fatalf("content-range not passed through: %q", rec.Header().Get("Content-Range"))
	}
}

// 中转上游把请求 302 到白名单外域名时必须报错,且不得写出任何响应体。
func TestRelayRedirectOutsideAllowlistBlocked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://blocked.example/x")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "")
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("redirect outside allowlist must be blocked: %v", err)
	}
}

// 上游 403(直链提前失效)时必须失效本地缓存行,否则重新解析仍命中同一死链。
func TestRelayUpstreamForbiddenInvalidatesCache(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err == nil {
		t.Fatal("upstream 403 should surface an error")
	}
	got, err := relay.store.GetLink(key, "f1")
	if err != nil || got == nil {
		t.Fatalf("link row missing: %v", err)
	}
	if got.ExpiresAt.After(time.Now()) {
		t.Fatalf("upstream 403 must expire the cached link, expires_at=%v", got.ExpiresAt)
	}
}

// 非浏览器 UA 经 /stream 遇 acw_sc__v2 挑战页须自动解题重放,拿到真实文件。
func TestRelaySolvesLanzouACWChallenge(t *testing.T) {
	const (
		arg1    = "6a9c4c0e1f2b3d4a5e6f708192a3b4c5d6e7f809"
		token   = "7eb1cd08f1e9fa0e95d70fade751cd0c73136cc2"
		payload = "LANZOU-FILE-BYTES"
	)
	challenge := `<html><script>var arg1='` + arg1 + `';document.cookie='acw_sc__v2='+arg1;</script></html>`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("acw_sc__v2")
		if c == nil || c.Value != token {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(challenge))
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != payload {
		t.Fatalf("body=%q want %q", rec.Body.String(), payload)
	}
}

// fakeUpstream 模拟 quark 直链:按 Range 返回片段,并记录每个请求的 Range 头与
// 新建 TCP 连接数。acceptRanges=false 时不下发 Accept-Ranges
// (用于验证「上游不支持 Range 就不轮换」)。
type fakeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	ranges []string
	conns  int64
}

func newFakeUpstream(t *testing.T, payload []byte, acceptRanges bool) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh := r.Header.Get("Range")
		u.mu.Lock()
		u.ranges = append(u.ranges, rh)
		u.mu.Unlock()

		start, end := 0, len(payload)-1
		partial := false
		if rh != "" {
			if s, e, ok := parseTestRange(rh, len(payload)); ok {
				start, end, partial = s, e, true
			}
		}
		if acceptRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		if partial {
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(payload[start : end+1])
	}))
	u.srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			atomic.AddInt64(&u.conns, 1)
		}
	}
	u.srv.Start()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) URL() string { return u.srv.URL }

func (u *fakeUpstream) reqRanges() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.ranges...)
}

func (u *fakeUpstream) connections() int64 { return atomic.LoadInt64(&u.conns) }

func parseTestRange(h string, total int) (int, int, bool) {
	h = strings.TrimPrefix(h, "bytes=")
	sp, ep, ok := strings.Cut(h, "-")
	if !ok {
		return 0, 0, false
	}
	s, err := strconv.Atoi(sp)
	if err != nil || s >= total {
		return 0, 0, false
	}
	e := total - 1
	if ep != "" {
		if v, err := strconv.Atoi(ep); err == nil {
			e = v
		}
	}
	if e >= total {
		e = total - 1
	}
	if e < s {
		return 0, 0, false
	}
	return s, e, true
}

// withRotateBytes 临时把轮换阈值调小,让单测用 KB 级数据就能触发多条上游连接。
func withRotateBytes(t *testing.T, v int64) {
	t.Helper()
	old := relayRotateBytes
	relayRotateBytes = v
	t.Cleanup(func() { relayRotateBytes = old })
}

// P0 回归:单条上游连接搬够 relayRotateBytes 后必须重开连接续传,
// 而客户端收到的字节流仍完整且顺序正确(quark 长连接降速的根治手段)。
func TestRelayRotatesUpstreamConnection(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 40<<10) // 320 KB
	up := newFakeUpstream(t, payload, true)
	// 阈值取 32 KB:即便每次 Read 都取满 128 KB,一条连接最多也只能搬
	// 32K+128K=160K,320 KB 必然需要至少 2 次轮换(Read 返回得更碎只会更多)。
	withRotateBytes(t, 32<<10)

	relay, key := newRelayFixture(t, up.URL())
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body corrupted after rotation: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}

	got := up.reqRanges()
	if len(got) < 3 {
		t.Fatalf("want >=3 upstream requests (rotation), got %d: %v", len(got), got)
	}
	if got[0] != "" {
		t.Fatalf("first upstream request must be unconditional, got Range=%q", got[0])
	}
	prev := 0
	for i, rh := range got[1:] {
		s, _, ok := parseTestRange(rh, len(payload))
		if !ok || s <= prev {
			t.Fatalf("rotation #%d Range=%q must continue forward from %d (all=%v)", i+1, rh, prev, got)
		}
		prev = s
	}
	// 轮换必须真的换 TCP 连接:否则复用一条已被限速的旧连接等于没轮换
	if n := up.connections(); n < int64(len(got)) {
		t.Fatalf("want a fresh TCP connection per upstream request, got %d conns for %d requests", n, len(got))
	}

	// 指标必须真的记上:没有指标就等于线上无法判断轮换是否生效
	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if body := rr.Body.String(); !strings.Contains(body, `panrouter_relay_bytes_total{pan="fake"} 327680`) {
		t.Fatalf("relay bytes metric missing/wrong:\n%s", body)
	}
	if body := rr.Body.String(); !strings.Contains(body, fmt.Sprintf(`panrouter_relay_rotate_total{pan="fake"} %d`, len(got)-1)) {
		t.Fatalf("relay rotate metric missing/wrong (want %d rotations):\n%s", len(got)-1, body)
	}
}

// 上游不声明 Accept-Ranges 时不得轮换:否则会对着同一个「忽略 Range 的 200」
// 反复请求全量数据,把带宽放大成 N 倍。
func TestRelayNoRotateWhenUpstreamLacksRange(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 320<<10)
	up := newFakeUpstream(t, payload, false)
	withRotateBytes(t, 100<<10)

	relay, key := newRelayFixture(t, up.URL())
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body mismatch: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	if got := up.reqRanges(); len(got) != 1 {
		t.Fatalf("upstream without range support must not rotate, got %d requests: %v", len(got), got)
	}
}
