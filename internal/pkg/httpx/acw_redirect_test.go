package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// P2-10 回归:POST 遇 302 跳转后吃挑战页,重放必须以最终生效请求(Go 已按 302
// 把 POST 改写为 GET、剥离 body)为模板;以原始请求为模板会把 POST+body 重放到跳转目标。
func TestACWReplayAfter302DropsStaleBody(t *testing.T) {
	var cdnMethod atomic.Value
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnMethod.Store(r.Method)
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
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, cdn.URL+"/file", http.StatusFound)
	}))
	defer front.Close()

	cl, err := New(Options{AllowPrivate: true, Timeout: 2 * time.Second, RedirectAllow: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, front.URL+"/submit", strings.NewReader("payload=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.DoStream(req)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != filePayload {
		t.Fatalf("body=%q want %q", body, filePayload)
	}
	if got := cdnMethod.Load(); got != http.MethodGet {
		t.Fatalf("replay after 302 must be GET, got %v", got)
	}
}

// P2-10 回归:POST 遇 307 跳转后吃挑战页,重放必须保留原 method 与 body。
func TestACWReplayAfter307KeepsBody(t *testing.T) {
	var (
		mu       sync.Mutex
		method   string
		bodyHead string
	)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie(acwCookieName)
		if c == nil || c.Value != testACWTok {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(challengeHTML))
			return
		}
		mu.Lock()
		method = r.Method
		b, _ := io.ReadAll(io.LimitReader(r.Body, 64))
		bodyHead = string(b)
		mu.Unlock()
		_, _ = w.Write([]byte(filePayload))
	}))
	defer cdn.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/file", http.StatusTemporaryRedirect)
	}))
	defer front.Close()

	cl, err := New(Options{AllowPrivate: true, Timeout: 2 * time.Second, RedirectAllow: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, front.URL+"/submit", strings.NewReader("payload=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.DoStream(req)
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if string(out) != filePayload {
		t.Fatalf("body=%q want %q", out, filePayload)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPost || bodyHead != "payload=1" {
		t.Fatalf("replay after 307 must keep POST+body, got method=%s body=%q", method, bodyHead)
	}
}
