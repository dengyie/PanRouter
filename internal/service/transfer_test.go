package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// newTransferFixture 装配 Resolver(假网盘直链指向 upstream)+ Transfer(OpenList 指向 openlist)。
func newTransferFixture(t *testing.T, upstreamURL, openlistURL string) (*Transfer, string) {
	return newTransferFixtureMut(t, upstreamURL, openlistURL, nil)
}

// newTransferFixtureMut 在标准 fixture 上允许测试覆写配置(单文件上限/auto 阈值等)。
func newTransferFixtureMut(t *testing.T, upstreamURL, openlistURL string, mut func(*config.Config)) (*Transfer, string) {
	t.Helper()
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	cfg.Transfer.Endpoint = openlistURL
	cfg.Transfer.Token = "tok"
	if mut != nil {
		mut(cfg)
	}
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

	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, NoBodyTimeout: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := NewTransfer(cfgp, resolver, map[string]*httpx.Client{"fake": cl}, store, aes, met, log)
	if err != nil {
		t.Fatal(err)
	}
	return tr, "https://fake.example/s/x"
}

// P1 契约:stream 通道把上游字节完整写进 OpenList,进度回填到任务快照。
func TestTransferStreamPushesBytes(t *testing.T) {
	payload := strings.Repeat("abc", 1000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("上游应收到 UA")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	var got []byte
	var gotPath string
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "tok" {
			t.Errorf("missing auth: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			gotPath = r.Header.Get("File-Path")
			got, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, share := newTransferFixture(t, up.URL, ol.URL)
	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" {
		t.Fatalf("state=%s err=%s", snap.State, snap.Error)
	}
	if string(got) != payload {
		t.Fatalf("body mismatch: got %d want %d bytes", len(got), len(payload))
	}
	if gotPath == "" {
		t.Fatalf("File-Path header missing")
	}
	if snap.Done != int64(len(payload)) || snap.DestPath != "/Pool/n.bin" {
		t.Fatalf("snapshot: done=%d path=%s", snap.Done, snap.DestPath)
	}
}

// P2 契约:auto 通道在无提取链约束时选离线(远端直拉),有约束时退回 stream。
func TestTransferAutoChannelSelection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer up.Close()

	var offline int
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case "/api/fs/add_offline_download":
			offline++
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"tasks":[{"id":"1"}]}}`))
		default:
			t.Errorf("unexpected: %s", r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, share := newTransferFixture(t, up.URL, ol.URL)
	task, err := tr.Submit(TransferRequest{SourceURL: share, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	if tr.Get(task.ID).Channel != string(ChannelOffline) {
		t.Fatalf("auto should pick offline for constraint-free link, got %s", tr.Get(task.ID).Channel)
	}
	if offline != 1 {
		t.Fatalf("offline called %d times", offline)
	}
}

// P2 回归:上游不带 Content-Length(chunked CDN)时不得伪造长度,否则 PUT 会断流。
func TestTransferStreamUnknownLengthFallsBackChunked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("no flush")
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 5; i++ {
			_, _ = w.Write([]byte(strings.Repeat("z", 100)))
			flusher.Flush()
		}
	}))
	defer up.Close()

	var declared int64
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case "/api/fs/put":
			declared = r.ContentLength
			_, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		}
	}))
	defer ol.Close()

	tr, share := newTransferFixture(t, up.URL, ol.URL)
	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	if snap := tr.Get(task.ID); snap.State != "done" {
		t.Fatalf("state=%s err=%s", snap.State, snap.Error)
	}
	if declared != -1 {
		t.Fatalf("unknown-length source must upload chunked, declared=%d", declared)
	}
}

func waitDone(t *testing.T, tr *Transfer, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := tr.Get(id); s != nil && (s.State == "done" || s.State == "failed") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
}
