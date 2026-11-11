package service

// v2.11 回归:转存链路的「空/失败被当成功」三连。
//
// ① 目录转存的源选择必须跳过 LinkError 文件:修复后第一个可提链的文件被选中;
//    修复前选中首个文件(哪怕它提链失败)→ 整次转存退出,后面的可用文件永无机会。
// ② stream 通道:解析时登记了非零大小、却收到 200 空 body(直链失效/风控的
//    常见形态)必须报错 —— 修复前 size=0 跳过字节数校验,0 字节产物被标 done。
// ③ offline 产物确认:expect==0(源文件 0 字节)是合法形态,必须能确认;
//    修复前「非空才确认」让合法产物永远确认不了,确认窗口超时误报 failed。

import (
	"context"
	"net/http"
	"net/url"
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

// pickDriver:第一个文件提链必败(LinkError 形态),第二个可用。
type pickDriver struct{ upstream string }

func (d *pickDriver) ID() string { return "fake" }

func (d *pickDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{
		{FID: "bad", Name: "bad.bin", Size: 1, Ext: map[string]string{}},
		{FID: "good", Name: "good.bin", Size: 4, Ext: map[string]string{}},
	}, nil
}

func (d *pickDriver) GetDirectLink(_ context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if ref.FID == "bad" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "源文件已失效", nil)
	}
	return driver.DirectLink{URL: d.upstream + "/good", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (d *pickDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true}, nil
}

// allFailDriver:分享里所有文件的提链都失败。
type allFailDriver struct {
	fakeDriver
	err driver.Error
}

func (d *allFailDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{
		{FID: "b1", Name: "b1.bin", Size: 1, Ext: map[string]string{}},
		{FID: "b2", Name: "b2.bin", Size: 1, Ext: map[string]string{}},
	}, nil
}

func (d *allFailDriver) GetDirectLink(context.Context, *driver.Credential, driver.FileRef) (driver.DirectLink, error) {
	return driver.DirectLink{}, driver.NewErr(d.err.Kind, "上游拒了", nil)
}

// newTransferFixtureWithDriver 与 transfer_test.go 的标准 fixture 相同,
// 只是允许注入自定义 driver(resolveSource 的源选择必须走完整解析链)。
func newTransferFixtureWithDriver(t *testing.T, drv driver.Driver, upstreamURL, openlistURL string) (*Transfer, string) {
	t.Helper()
	store, err := repo.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	cfg.Transfer.Endpoint = openlistURL
	cfg.Transfer.Token = "tok"
	// 与 transfer_test.go 的标准 fixture 对齐:e2e 不测限频/并发闸,放开
	cfg.Drivers["fake"] = config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 10}
	cfgp := &config.Provider{}
	cfgp.Set(cfg)

	aes := crypto.New("test-key")
	log := zap.NewNop().Sugar()
	reg := driver.NewRegistry([]driver.Driver{drv}, map[string][]string{"fake": {"fake.example"}})
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

func TestTransferSourceSkipsLinkErrorFiles(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("DATA"))
	}))
	defer up.Close()

	var gotPath string
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			gotPath = r.Header.Get("File-Path")
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, share := newTransferFixtureWithDriver(t, &pickDriver{upstream: up.URL}, up.URL, ol.URL)
	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" {
		t.Fatalf("应跳过 LinkError 文件转存 good.bin, got state=%s err=%s", snap.State, snap.Error)
	}
	// File-Path 头是 PathEscape 形态(mangodrive Put 的约定),解回真实路径再断言
	if plain, uerr := url.PathUnescape(gotPath); uerr != nil || !strings.HasSuffix(plain, "/good.bin") {
		t.Fatalf("落盘名应为 good.bin, got %q", gotPath)
	}
}

// 全部提链失败时,不得误报「分享中没有可转存的文件」—— 应给出真实上游错误。
func TestTransferSourceAllLinkErrorsReportsRealError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer up.Close()
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/mkdir" {
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		}
	}))
	defer ol.Close()

	tr, share := newTransferFixtureWithDriver(t, &allFailDriver{err: driver.Error{Kind: driver.KindNotFound}},
		up.URL, ol.URL)
	_, err := tr.resolveSource(context.Background(), TransferRequest{SourceURL: share, Channel: ChannelStream, UA: "UA/1"})
	if err == nil {
		t.Fatal("全失败源必须报错")
	}
	if strings.Contains(err.Error(), "没有可转存的文件") {
		t.Fatalf("应给出真实上游错误而非误报: %v", err)
	}
}

// stream 通道:登记非零大小 + 200 空 body → 必须报错,不得标 done。
func TestTransferStreamEmptyBodyFails(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 200、无 body、无 Content-Length —— 直链失效的典型形态
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
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
	if snap.State != "failed" {
		t.Fatalf("空 body 200 必须失败(0 字节产物不得标 done), got state=%s", snap.State)
	}
	if !strings.Contains(snap.Error, "空内容") {
		t.Fatalf("错误应说明空内容: %q", snap.Error)
	}
}

// offline:源大小未知(expect<=0)且产物是合法的 0 字节文件 → done,不得超窗误报 failed。
func TestOfflineConfirmZeroByteArtifact(t *testing.T) {
	s := newOfflineStack(t)
	s.headLen = false                 // 源大小未知(chunked)→ expect<=0
	atomic.StoreInt64(&s.size, 0)     // 落池产物本身 0 字节(合法)
	s.setMtime("2030-01-01T00:00:00Z") // 新鲜

	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.MaxBytes = -1
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
		c.Transfer.OfflineConfirmAfter = 800 * time.Millisecond // 修复前会一直等到超窗失败
	})
	id, _ := s.submitOffline(t, tr)
	waitState(t, tr, id, "done", "failed")
	snap := tr.Get(id)
	if snap.State != "done" {
		t.Fatalf("0 字节产物应被确认 done, got state=%s err=%s", snap.State, snap.Error)
	}
}

// 对照:expect>0 时产物大小不足仍不得确认(不放宽既有严格判据)。
func TestOfflineConfirmStillRequiresSize(t *testing.T) {
	s := newOfflineStack(t)
	s.headLen = true               // 源大小已知 300 → expect=300
	atomic.StoreInt64(&s.size, 100) // 落池产物只有 100 字节

	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
		c.Transfer.OfflineConfirmAfter = 300 * time.Millisecond
	})
	id, _ := s.submitOffline(t, tr)
	waitState(t, tr, id, "done", "failed")
	if snap := tr.Get(id); snap.State != "failed" {
		t.Fatalf("产物不足期望大小必须 failed, got state=%s size=%d", snap.State, snap.Size)
	}
}
