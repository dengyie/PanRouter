package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testArg1      = "6a9c4c0e1f2b3d4a5e6f708192a3b4c5d6e7f809"
	testACWTok    = "7eb1cd08f1e9fa0e95d70fade751cd0c73136cc2"
	challengeHTML = `<html><script>var arg1='` + testArg1 + `';document.cookie='acw_sc__v2='+arg1;</script></html>`
	filePayload   = "LANZOU-FILE-BYTES"
)

func TestSolveACW(t *testing.T) {
	got, err := SolveACW([]byte(challengeHTML))
	if err != nil {
		t.Fatal(err)
	}
	if got != testACWTok {
		t.Fatalf("token=%s want %s", got, testACWTok)
	}
	if _, err := SolveACW([]byte(`<html>no challenge</html>`)); err == nil {
		t.Fatal("missing arg1 must fail")
	}
}

func TestDoStreamSolvesACWChallenge(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		c, _ := r.Cookie(acwCookieName)
		if c == nil || c.Value != testACWTok {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(challengeHTML))
			return
		}
		if r.Header.Get("Range") == "bytes=0-4" {
			w.Header().Set("Content-Range", "bytes 0-4/17")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(filePayload[:5]))
			return
		}
		_, _ = w.Write([]byte(filePayload))
	}))
	defer srv.Close()

	cl, err := New(Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/file", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	resp, err := cl.DoStream(req)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != filePayload {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if hits.Load() < 2 {
		t.Fatalf("challenge must trigger a replay, hits=%d", hits.Load())
	}

	// 缓存命中:同一 host 的 Range 续传不再吃挑战页
	before := hits.Load()
	req2, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/file", nil)
	req2.Header.Set("Range", "bytes=0-4")
	resp2, err := cl.DoStream(req2)
	if err != nil {
		t.Fatalf("cached Range: %v", err)
	}
	defer resp2.Body.Close()
	got, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusPartialContent || string(got) != filePayload[:5] {
		t.Fatalf("range status=%d body=%q", resp2.StatusCode, got)
	}
	if hits.Load() != before+1 {
		t.Fatalf("cached cookie should skip challenge, hits %d → %d", before, hits.Load())
	}
}

func TestDoStreamACWReplayOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(challengeHTML))
	}))
	defer srv.Close()
	cl, err := New(Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	_, err = cl.DoStream(req)
	if err == nil || !strings.Contains(err.Error(), "persisted") {
		t.Fatalf("persistent challenge must fail: %v", err)
	}
}

func TestDoStreamDoesNotMisdetectBinary(t *testing.T) {
	payload := strings.Repeat("x", 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	cl, err := New(Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	resp, err := cl.DoStream(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != payload {
		t.Fatalf("binary body mutated: %q", body)
	}
}

func TestRedirectInjectsCachedACWCookie(t *testing.T) {
	var cdn *httptest.Server
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie(acwCookieName)
		if c == nil || c.Value != testACWTok {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(challengeHTML))
			return
		}
		_, _ = w.Write([]byte(filePayload))
	}))
	defer cdn.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/file", http.StatusFound)
	}))
	defer front.Close()

	cl, err := New(Options{
		AllowPrivate: true, Timeout: 2 * time.Second,
		RedirectAllow: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", front.URL+"/share", nil)
	resp, err := cl.DoStream(req)
	if err != nil {
		t.Fatalf("first (solve): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != filePayload {
		t.Fatalf("first body=%q", body)
	}

	req2, _ := http.NewRequestWithContext(context.Background(), "GET", front.URL+"/share", nil)
	resp2, err := cl.DoStream(req2)
	if err != nil {
		t.Fatalf("second (cached via redirect): %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != filePayload {
		t.Fatalf("second body=%q", body2)
	}
}

// blockingBody 模拟「4xx 响应体挂在半路」:Read 阻塞到 Close。
type blockingBody struct {
	mu     sync.Mutex
	prefix []byte
	closed bool
	done   chan struct{}
	once   sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()
	<-b.done // Close 之前永远等不到
	return 0, errors.New("body closed")
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}

// 挑战页窥探必须自己兜住「响应体挂住」:中转 client 是 NoBodyTimeout=true,
// 这里的读发生在任何超时机制接管之前,不设界就是一次永久阻塞(连 *http.Response
// 都还没交到调用方,停流看门狗无从介入)。
func TestInspectChallengeProbeIsBounded(t *testing.T) {
	body := &blockingBody{prefix: []byte("denied-but-not-a-challenge"), done: make(chan struct{})}
	resp := &http.Response{
		StatusCode:    http.StatusForbidden,
		Header:        http.Header{"Content-Type": []string{"text/html"}},
		Body:          body,
		ContentLength: -1,
	}

	start := time.Now()
	peek, challenge, err := inspectChallenge(resp, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("probe timeout must surface as an error (peek is not a complete body): err=%v", err)
	}
	if challenge {
		t.Fatal("a stalled body is not an acw challenge")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe blocked for %v, want bounded by the probe budget", elapsed)
	}
	// 超时即失败:调用方(relay)会干净报错,而不是把 peek 伪装成完整响应——
	// 那会让 chunked 下客户端拿到「HTTP 完整」的截断文件且无任何错误。
	_ = peek
	_ = resp.Body.Close()
}

// 二进制流(即使 chunked/总长未知)不该被窥探:挑战页一定是文本/412,对
// application/octet-stream 做探测只有延迟和误杀,没有任何收益。
func TestInspectChallengeSkipsBinaryChunkedStream(t *testing.T) {
	const payload = "some-binary-payload"
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:          io.NopCloser(strings.NewReader(payload)),
		ContentLength: -1, // chunked
	}
	peek, challenge, err := inspectChallenge(resp, time.Millisecond)
	if err != nil {
		t.Fatalf("binary stream must not be probed: %v", err)
	}
	if challenge || peek != nil {
		t.Fatalf("binary stream is never a challenge: challenge=%v peek=%q", challenge, peek)
	}
	rest, err := io.ReadAll(resp.Body) // 原 Body 必须原封不动
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(rest) != payload {
		t.Fatalf("binary body was consumed by probe: %q", rest)
	}
}

// 没有 GetBody 的流式请求体(io.Pipe 等)在挑战/307 重放时已不可再读,必须显式
// 拒绝,否则重放发出的是空体,上游 4xx 还会被误归因成「挑战持续」。
func TestRewindableRejectsStreamBody(t *testing.T) {
	pr, _ := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/", pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := rewindable(req); err == nil {
		t.Fatal("streaming body must be rejected as non-rewindable")
	}
	bufReq, err := http.NewRequest(http.MethodPost, "http://example.invalid/", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := rewindable(bufReq); err != nil {
		t.Fatalf("rewindable body rejected: %v", err)
	}
	nilReq, err := http.NewRequest(http.MethodPost, "http://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rewindable(nilReq); err != nil {
		t.Fatalf("nil body rejected: %v", err)
	}
}
