package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
