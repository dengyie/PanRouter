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
	relay := NewRelay(resolver, map[string]*httpx.Client{"fake": streamCl}, store, aes, met, log, cfgp)
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

// 多段 Range(multipart/byteranges,如 bytes=0-3,10-13)端到端。多段响应只能整体透传:
// ①整个 multipart 体必须原样到达,一个字节都不能被当成「越界」钳掉;②轮换必须禁用
// ——续传/轮换用的是单条 bytes=<pos>-<limit> Range,会把分段边界和跨段字节重写坏。
func TestRelayMultipartRangePassesThroughVerbatim(t *testing.T) {
	const boundary = "multi-9a2b7c"
	partA := []byte("\r\n--" + boundary + "\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 0-3/300\r\n\r\n0123")
	partB := []byte("\r\n--" + boundary + "\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes 10-13/300\r\n\r\nabcd")
	tail := []byte("\r\n--" + boundary + "--\r\n")
	body := bytes.Join([][]byte{partA, partB, tail}, nil)

	var mu sync.Mutex
	var ranges []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	// 阈值压到 32 KB:一条 128 KB 的 Read 就足以触发轮换——若多段误开了轮换,这里必现。
	withRotateBytes(t, relay, 32<<10)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec,
		StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "bytes=0-3,10-13"); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "multipart/byteranges; boundary="+boundary {
		t.Fatalf("content-type not passed through: %q", rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("multipart body corrupted: got %d bytes, want %d", rec.Body.Len(), len(body))
	}
	// 多段不能续传/轮换:必须恰好 1 次上游请求,且携带的就是原始多段 Range。
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "bytes=0-3,10-13" {
		t.Fatalf("want exactly 1 upstream request with the multi-range header, got %v", got)
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
// 阈值挂在 Relay 实例上而不是包级变量:包级变量被测试改写时,已经在跑的
// 看门狗/转发 goroutine 会与之形成 data race(v2.12.8 的 CI 就抓到过一次)。
func withRotateBytes(t *testing.T, relay *Relay, v int64) {
	t.Helper()
	old := relay.rotateBytes
	relay.rotateBytes = v
	t.Cleanup(func() { relay.rotateBytes = old })
}

// P0 回归:单条上游连接搬够 relayRotateBytes 后必须重开连接续传,
// 而客户端收到的字节流仍完整且顺序正确(quark 长连接降速的根治手段)。
func TestRelayRotatesUpstreamConnection(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 40<<10) // 320 KB
	up := newFakeUpstream(t, payload, true)

	relay, key := newRelayFixture(t, up.URL())
	// 阈值取 32 KB:即便每次 Read 都取满 128 KB,一条连接最多也只能搬
	// 32K+128K=160K,320 KB 必然需要至少 2 次轮换(Read 返回得更碎只会更多)。
	withRotateBytes(t, relay, 32<<10)
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

	relay, key := newRelayFixture(t, up.URL())
	withRotateBytes(t, relay, 100<<10)
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

// 客户端中途断开(取消 ctx)必须记成 client_gone:上游 Read 会先返回
// context canceled,若按 upstream_interrupt 记,线上就分不清「用户关页面」
// 和「上游塌了」,报警也会失真。
func TestRelayClientCancelCountedAsClientGone(t *testing.T) {
	payload := bytes.Repeat([]byte("q"), 64<<20) // 足够大,不会在取消前读完
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:1<<20])
		w.(http.Flusher).Flush()
		<-release // 挂住,模拟慢上游;测试结束时由 defer 释放
	}))
	defer up.Close()
	defer close(release)

	relay, key := newRelayFixture(t, up.URL)
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() {
		done <- relay.Serve(ctx, rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "")
	}()

	time.Sleep(150 * time.Millisecond) // 让第一块到客户端
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after client cancel")
	}

	rrm := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rrm, httptest.NewRequest("GET", "/metrics", nil))
	body := rrm.Body.String()
	if !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="client_gone"} 1`) {
		t.Fatalf("client cancel must be counted as client_gone:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="upstream_interrupt"} 0`) {
		t.Fatalf("client cancel must not be counted as upstream_interrupt:\n%s", body)
	}
}

// 中转请求必须显式要 identity 编码,且上游即便仍带 Content-Encoding 也要原样透传:
// 否则客户端会拿到「被 Go 透明解压过的字节 + 缺失的 Content-Encoding」,
// 或被解压抹掉 Content-Length 导致 Range/续传边界错位。
func TestRelayAsksIdentityAndPassesContentEncoding(t *testing.T) {
	payload := []byte("NOT-REALLY-GZIP-BUT-DECLARED-AS-IT")
	var gotAE string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if gotAE != "identity" {
		t.Fatalf("relay must ask for identity encoding, upstream saw %q", gotAE)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding not passed through: %q", rec.Header().Get("Content-Encoding"))
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("content-length must stay %d, got %q", len(payload), got)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body must be raw bytes, got %q", rec.Body.String())
	}
}

// rangeHandler 造一个「先快后断」的上游:首次请求(无 Range)吐 chunk 字节后掐断连接,
// 之后按 Range 返回剩余部分。返回已服务的请求数(供断言续传次数)。
func abortingUpstream(t *testing.T, payload []byte, chunk int) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh := r.Header.Get("Range")
		u.mu.Lock()
		u.ranges = append(u.ranges, rh)
		u.mu.Unlock()

		w.Header().Set("Accept-Ranges", "bytes")
		start, end := 0, len(payload)-1
		if rh != "" {
			s, e, ok := parseTestRange(rh, len(payload))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		}
		stop := start + chunk
		if stop > end+1 {
			stop = end + 1
		}
		_, _ = w.Write(payload[start:stop])
		w.(http.Flusher).Flush()
		if stop <= end {
			// 声明了 Content-Length 却提前掐连接:客户端读到 unexpected EOF
			panic(http.ErrAbortHandler)
		}
	}))
	u.srv.Start()
	t.Cleanup(u.srv.Close)
	return u
}

// 上游中途掐断连接时,已发出的 Content-Length 不可能收回;能 Range 续传就必须续,
// 否则客户端只能拿到半截文件(表现为「传输中断」)。
func TestRelayResumesAfterUpstreamAbort(t *testing.T) {
	payload := bytes.Repeat([]byte("r"), 200<<10)
	up := abortingUpstream(t, payload, 100<<10)

	relay, key := newRelayFixture(t, up.URL())
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body incomplete after abort: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	got := up.reqRanges()
	if len(got) != 2 {
		t.Fatalf("want 2 upstream requests (initial+resume), got %d: %v", len(got), got)
	}
	// 续传 Range 必须收窄到客户端允许的终点(整段请求时就是文件尾),
	// 否则上游多吐的字节会撞上已发出的 Content-Length。
	if want := fmt.Sprintf("bytes=%d-%d", 100<<10, len(payload)-1); got[1] != want {
		t.Fatalf("resume must continue from pos 102400 bounded at %d, got Range=%q want %q", len(payload)-1, got[1], want)
	}

	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `panrouter_relay_resume_total{pan="fake"} 1`) {
		t.Fatalf("resume metric missing:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="upstream_interrupt"} 0`) {
		t.Fatalf("successful resume must not count as abort:\n%s", body)
	}
}

// 上游「一个字节都不再吐」时(黑洞/半开连接),NoBodyTimeout 会让我们永远等下去;
// 停流看门狗必须打断它并续传,而不是把客户端吊死在 0 B/s。
func TestRelayResumesAfterStall(t *testing.T) {
	payload := bytes.Repeat([]byte("s"), 200<<10)
	const cut = 64 << 10

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if rh := r.Header.Get("Range"); rh != "" {
			s, e, ok := parseTestRange(rh, len(payload))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(e-s+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[s : e+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:cut])
		w.(http.Flusher).Flush()
		// 挂住不再吐字节,直到中转取消这条连接(模拟上游黑洞)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			t.Error("relay never canceled the stalled upstream connection")
		}
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	relay.stallTimeout = 200 * time.Millisecond // 挂死判定缩到 200ms,单测不必等 60s
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body incomplete after stall: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	// 判挂死必须单独计数:否则运维只能从 resume_total 倒推,分不清
	// 「上游偶发断流但续上了」和「上游黑洞、看门狗掐了一次」。
	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if body := rr.Body.String(); !strings.Contains(body, `panrouter_relay_stall_total{pan="fake"} 1`) {
		t.Fatalf("stall watchdog firing not counted:\n%s", body)
	}
}

// 上游每次都断时不能无限重连:续满 relayResumeMax 次后按 upstream_interrupt 收尾。
func TestRelayResumeExhaustedCountsUpstreamInterrupt(t *testing.T) {
	payload := bytes.Repeat([]byte("e"), 400<<10)
	up := abortingUpstream(t, payload, 50<<10)

	relay, key := newRelayFixture(t, up.URL())
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Body.Len() >= len(payload) {
		t.Fatalf("payload should stay truncated, got %d bytes", rec.Body.Len())
	}
	if n := len(up.reqRanges()); n != 1+relayResumeMax {
		t.Fatalf("want %d upstream requests (1 + %d resumes), got %d", 1+relayResumeMax, relayResumeMax, n)
	}

	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, fmt.Sprintf(`panrouter_relay_resume_total{pan="fake"} %d`, relayResumeMax)) {
		t.Fatalf("resume metric wrong:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="upstream_interrupt"} 1`) {
		t.Fatalf("exhausted resume must count upstream_interrupt:\n%s", body)
	}
}

// 客户端要的是有界范围(bytes=A-B)时,轮换/续传只能写到 B:Range 必须收窄成
// bytes=<pos>-B。旧实现一律发 bytes=<pos>-,上游会继续吐到文件尾,而响应头里的
// Content-Length 早已按 B 发出,net/http 直接拒收整块("wrote more than the
// declared Content-Length"),客户端拿到的是意外 EOF。
func TestRelayBoundedRangeRotationStaysWithinRequest(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 40<<10) // 640 KB
	const from, to = 1000, 399999                               // 窗口 ~399 KB,32 KB 阈值下必然多次轮换
	up := newFakeUpstream(t, payload, true)

	relay, key := newRelayFixture(t, up.URL())
	withRotateBytes(t, relay, 32<<10)

	rec := httptest.NewRecorder()
	err := relay.Serve(context.Background(), rec,
		StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, fmt.Sprintf("bytes=%d-%d", from, to))
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", rec.Code)
	}
	want := payload[from : to+1]
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("bounded range corrupted after rotation: got %d bytes, want %d", rec.Body.Len(), len(want))
	}

	got := up.reqRanges()
	if len(got) < 3 {
		t.Fatalf("want >=3 upstream requests (rotation), got %d: %v", len(got), got)
	}
	if got[0] != fmt.Sprintf("bytes=%d-%d", from, to) {
		t.Fatalf("client range must be passed through unchanged, got %q", got[0])
	}
	for i, rh := range got[1:] {
		s, e, ok := parseTestRange(rh, len(payload))
		if !ok || s <= from || e != to {
			t.Fatalf("rotation #%d Range=%q must be bytes=<pos>-%d inside the requested window", i+1, rh, to)
		}
	}
}

// 上游声明 Accept-Ranges: bytes 却对 Range 回 200(整段)时,只能续一次:
// 之后每条连接都得从头丢弃 pos 字节,再轮换就是纯粹的带宽放大(256 MB 起跳)。
func TestRelayAdvertisedRangeIgnoredStopsRotating(t *testing.T) {
	payload := bytes.Repeat([]byte("k"), 320<<10)
	var mu sync.Mutex
	var ranges []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		// 声明支持 Range,但无论有没有 Range 都回 200 整段
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	withRotateBytes(t, relay, 100<<10)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body mismatch: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	mu.Lock()
	n := len(ranges)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("upstream ignoring range must cost exactly one fallback request, got %d: %v", n, ranges)
	}

	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if body := rr.Body.String(); !strings.Contains(body, `panrouter_relay_rotate_total{pan="fake"} 1`) {
		t.Fatalf("expected exactly 1 rotation before fallback:\n%s", body)
	}
}

// blockingWriter 模拟「客户端读得很慢」:Write 阻塞到 release 关闭。
type blockingWriter struct {
	rec     *httptest.ResponseRecorder
	release chan struct{}
	blocked chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Header() http.Header  { return b.rec.Header() }
func (b *blockingWriter) WriteHeader(code int) { b.rec.WriteHeader(code) }
func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.blocked) })
	<-b.release
	return b.rec.Write(p)
}

// 客户端自己读不动(w.Write 阻塞)不是上游挂死:看门狗只该在「正阻塞在读上游」
// 时生效。否则一个慢客户端会被判成停流 → 取消重连 → 续传 3 次 → 记
// upstream_interrupt 并把一次正常的下载掐掉。
func TestRelaySlowClientDoesNotTripStallWatchdog(t *testing.T) {
	payload := bytes.Repeat([]byte("w"), 256<<10)
	up := newFakeUpstream(t, payload, true)

	relay, key := newRelayFixture(t, up.URL())
	relay.stallTimeout = 100 * time.Millisecond

	bw := &blockingWriter{
		rec:     httptest.NewRecorder(),
		release: make(chan struct{}),
		blocked: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- relay.Serve(context.Background(), bw, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, "")
	}()

	select {
	case <-bw.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("relay never wrote to the client")
	}
	// 阻塞时长是挂死阈值的 6 倍、看门狗 tick 的 24 倍:误判的话必然已经重连过。
	time.Sleep(600 * time.Millisecond)
	if n := len(up.reqRanges()); n != 1 {
		t.Fatalf("slow client must not be treated as stalled upstream, got %d upstream requests", n)
	}

	close(bw.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not finish after the client resumed reading")
	}
	if !bytes.Equal(bw.rec.Body.Bytes(), payload) {
		t.Fatalf("body corrupted: got %d bytes, want %d", bw.rec.Body.Len(), len(payload))
	}

	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `panrouter_relay_resume_total{pan="fake"} 0`) {
		t.Fatalf("slow client must not trigger resume:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="upstream_interrupt"} 0`) {
		t.Fatalf("slow client must not be counted as upstream_interrupt:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_stall_total{pan="fake"} 0`) {
		t.Fatalf("slow client must not trip the stall watchdog:\n%s", body)
	}
	if !strings.Contains(body, `panrouter_relay_streams_active{pan="fake"} 0`) {
		t.Fatalf("streams_active must return to 0 after Serve returns:\n%s", body)
	}
}

// 轮换/续传必须校验 ETag/Last-Modified:上游在该位置已经换成另一个对象时,
// 继续写就是把两个版本的首尾拼成「长度正确、内容错误」的文件——比直接报错更糟。
func TestRelayAbortsOnValidatorMismatch(t *testing.T) {
	payload := bytes.Repeat([]byte("v"), 320<<10)
	var mu sync.Mutex
	var reqs int
	var ranges []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		idx := reqs
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("ver-%d", idx))) // 每次请求都是新版本
		start, end := 0, len(payload)-1
		if rh := r.Header.Get("Range"); rh != "" {
			s, e, ok := parseTestRange(rh, len(payload))
			if !ok {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		_, _ = w.Write(payload[start : end+1])
	}))
	defer up.Close()

	relay, key := newRelayFixture(t, up.URL)
	withRotateBytes(t, relay, 32<<10)
	rec := httptest.NewRecorder()
	if err := relay.Serve(context.Background(), rec, StreamInput{Pan: "fake", ShareKey: key, FID: "f1"}, ""); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if rec.Body.Len() >= len(payload) {
		t.Fatalf("body must be cut at the version boundary, got %d bytes", rec.Body.Len())
	}
	// 只写进第一块数据,不得把第二个版本的字节续在后面
	if !bytes.HasPrefix(payload, rec.Body.Bytes()) {
		t.Fatal("prefix bytes must come from the original object")
	}
	mu.Lock()
	n := len(ranges)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("mismatched validator must stop at once, got %d upstream requests: %v", n, ranges)
	}

	rr := httptest.NewRecorder()
	relay.met.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if body := rr.Body.String(); !strings.Contains(body, `panrouter_relay_abort_total{pan="fake",reason="rotate_failed"} 1`) {
		t.Fatalf("validator mismatch must be counted as rotate_failed:\n%s", body)
	}
}

func TestRangeEndLimit(t *testing.T) {
	const total = 1000
	cases := []struct {
		name   string
		hdr    string
		status int
		want   int64
		ok     bool
	}{
		{"整段请求", "", http.StatusOK, total - 1, true},
		{"206 无 Range 头", "", http.StatusPartialContent, total - 1, true},
		{"明确终点", "bytes=100-299", http.StatusPartialContent, 299, true},
		{"终点超出文件尾", "bytes=900-5000", http.StatusPartialContent, total - 1, true},
		{"开放终点", "bytes=100-", http.StatusPartialContent, total - 1, true},
		{"后缀 Range", "bytes=-100", http.StatusPartialContent, total - 1, true},
		{"多段 Range", "bytes=0-1,5-6", http.StatusPartialContent, 0, false},
		{"非 bytes 单位", "items=0-1", http.StatusPartialContent, 0, false},
		{"终点不可解析", "bytes=100-abc", http.StatusPartialContent, 0, false},
		{"起点不可解析", "bytes=abc-200", http.StatusPartialContent, 0, false},
		{"不是 Range 语法", "100-200", http.StatusPartialContent, 0, false},
		{"上游回 200 忽略 Range", "bytes=100-299", http.StatusOK, total - 1, true},
	}
	for _, c := range cases {
		got, ok := rangeEndLimit(c.hdr, total, c.status)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("%s: rangeEndLimit(%q,%d,%d) = (%d,%v), want (%d,%v)",
				c.name, c.hdr, total, c.status, got, ok, c.want, c.ok)
		}
	}
	if _, ok := rangeEndLimit("bytes=0-1", 0, http.StatusPartialContent); ok {
		t.Fatal("unknown total must disable rotation")
	}
}

// rotateLimit/stallLimit 三级求值:显式字段(单测/排障注入) > 配置 > 内置默认;
// 负值原样透传(= 关闭该特性),0 一律回落默认。见 config.Relay 的契约注释。
func TestRelayTuningPrecedence(t *testing.T) {
	cfg := config.Default()
	cfgp := &config.Provider{}
	cfgp.Set(cfg)
	r := NewRelay(nil, nil, nil, nil, nil, zap.NewNop().Sugar(), cfgp)

	if got := r.rotateLimit(); got != defaultRelayRotateBytes {
		t.Fatalf("default rotateLimit=%d, want %d", got, defaultRelayRotateBytes)
	}
	if got := r.stallLimit(); got != defaultRelayStallTimeout {
		t.Fatalf("default stallLimit=%v, want %v", got, defaultRelayStallTimeout)
	}

	// 配置负值 = 关闭,必须原样透传(调用方按「不轮换 / 不看门狗」处理)
	cfg.Relay.RotateBytes, cfg.Relay.StallTimeout = -1, -1
	if got := r.rotateLimit(); got != -1 {
		t.Fatalf("negative rotate must pass through, got %d", got)
	}
	if got := r.stallLimit(); got != -1 {
		t.Fatalf("negative stall must pass through, got %v", got)
	}

	// 配置 0 = 回落内置默认(手写 Config 或显式写 0)
	cfg.Relay.RotateBytes, cfg.Relay.StallTimeout = 0, 0
	if got := r.rotateLimit(); got != defaultRelayRotateBytes {
		t.Fatalf("zero rotate must fall back to %d, got %d", defaultRelayRotateBytes, got)
	}
	if got := r.stallLimit(); got != defaultRelayStallTimeout {
		t.Fatalf("zero stall must fall back to %v, got %v", defaultRelayStallTimeout, got)
	}

	// 显式字段优先于配置
	r.rotateBytes, r.stallTimeout = 4096, 7*time.Second
	if got := r.rotateLimit(); got != 4096 {
		t.Fatalf("field override must win for rotate, got %d", got)
	}
	if got := r.stallLimit(); got != 7*time.Second {
		t.Fatalf("field override must win for stall, got %v", got)
	}

	// 未接配置时也必须回落默认,不能把阈值求成 0(那等于静默关闭两项能力)
	plain := NewRelay(nil, nil, nil, nil, nil, zap.NewNop().Sugar(), nil)
	if got := plain.rotateLimit(); got != defaultRelayRotateBytes {
		t.Fatalf("nil cfgp must fall back to %d, got %d", defaultRelayRotateBytes, got)
	}
	if got := plain.stallLimit(); got != defaultRelayStallTimeout {
		t.Fatalf("nil cfgp must fall back to %v, got %v", defaultRelayStallTimeout, got)
	}
}
