package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

type fakeDriver struct{ linkURL string }

func (f *fakeDriver) ID() string { return "fake" }
func (f *fakeDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{{FID: "f1", Name: "n.bin", Size: 300, Ext: map[string]string{}}}, nil
}
func (f *fakeDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{URL: f.linkURL, ExpiresAt: time.Now().Add(time.Hour)}, nil
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
	acc := NewAccountService(store, aes, log)
	resolver := NewResolver(cfgp, reg, store, aes, acc, limiter.New(),
		breaker.NewRegistry(5, time.Minute), sign.New("k"), metrics.New(), log)

	const shareURL = "https://fake.example/s/x"
	if _, err := resolver.ResolveShare(context.Background(), shareURL, ""); err != nil {
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
	relay := NewRelay(resolver, map[string]*httpx.Client{"fake": streamCl}, store, aes, log)
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
