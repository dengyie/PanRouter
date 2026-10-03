package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 私网地址必须在拨号层被拦截(SSRF 防护的最后一道闸)。
func TestPrivateAddressBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	cl, err := New(Options{AllowPrivate: false, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	_, err = cl.Do(req)
	if err == nil || !strings.Contains(err.Error(), "blocked private") {
		t.Fatalf("private address must be blocked: %v", err)
	}
}

// 重定向到白名单外的 host 必须被拒(在 DNS 解析之前按后缀拒绝,不产生真实请求)。
func TestRedirectOutsideAllowlistBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://blocked.example/x", http.StatusFound)
	}))
	defer srv.Close()
	cl, err := New(Options{AllowPrivate: true}) // 白名单为空 = 仅允许同 host
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	_, err = cl.Do(req)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("redirect outside allowlist must be blocked: %v", err)
	}
}

// P1 回归:NoBodyTimeout=true 时,总时长超过 Timeout 的响应体不得被截断
// (响应头仍须在 Timeout 内到达)。
func TestNoBodyTimeoutAllowsSlowBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 128)))
			fl.Flush()
			time.Sleep(150 * time.Millisecond) // 总时长 ~450ms > Timeout 200ms
		}
	}))
	defer srv.Close()
	cl, err := New(Options{Timeout: 200 * time.Millisecond, NoBodyTimeout: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("slow body must not be truncated: %v", err)
	}
	if len(res.Body) != 384 {
		t.Fatalf("body truncated: got %d bytes, want 384", len(res.Body))
	}
}
