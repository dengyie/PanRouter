package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/breaker"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/limiter"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/pkg/sign"
	"github.com/dengyie/panrouter/internal/repo"
	"go.uber.org/zap"
)

// P0 契约:私有盘公网直链不可回源(Cloudflare 隧道),转存必须走 driver 给的
// 服务端直链;走公网一定会失败。同时验证完成后 size/done 回填。
func TestTransferPrefersServerLink(t *testing.T) {
	payload := strings.Repeat("q", 5555)

	// 内网直链:可达,返回真实字节。
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5555")
		_, _ = w.Write([]byte(payload))
	}))
	defer internal.Close()

	// 公网直链:不可回源(403),一旦被请求即失败。
	var publicHits int64
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&publicHits, 1)
		http.Error(w, "blocked by edge", http.StatusForbidden)
	}))
	defer public.Close()

	var got []byte
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			got, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	cfg := config.Default()
	cfg.Transfer.Endpoint = ol.URL
	cfg.Transfer.Token = "tok"
	cfgp := &config.Provider{}
	cfgp.Set(cfg)

	aes := crypto.New("test-key")
	log := zap.NewNop().Sugar()
	fd := &fakeDriver{linkURL: public.URL + "/file", serverURL: internal.URL + "/file"}
	reg := driver.NewRegistry([]driver.Driver{fd}, map[string][]string{"fake": {"fake.example"}})
	met := metrics.New()
	acc := NewAccountService(store, aes, met, log)
	resolver := NewResolver(cfgp, reg, store, aes, acc, limiter.New(),
		breaker.NewRegistry(5, time.Minute), sign.New("k"), met, log)
	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, NoBodyTimeout: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := NewTransfer(cfgp, resolver, map[string]*httpx.Client{"fake": cl}, store, aes, met, log)
	if err != nil {
		t.Fatal(err)
	}

	task, err := tr.Submit(TransferRequest{SourceURL: "https://fake.example/s/x", Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" {
		t.Fatalf("state=%s err=%s hint=%s", snap.State, snap.Error, snap.Hint)
	}
	if n := atomic.LoadInt64(&publicHits); n != 0 {
		t.Fatalf("不应访问公网直链, 实际命中 %d 次", n)
	}
	if string(got) != payload {
		t.Fatalf("落盘字节不符: got %d want %d", len(got), len(payload))
	}
	if snap.Size != 5555 || snap.Done != 5555 {
		t.Fatalf("size/done 未回填: size=%d done=%d want 5555", snap.Size, snap.Done)
	}
}
