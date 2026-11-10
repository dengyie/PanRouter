package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
)

// offlineStack 确认器测试的双端 fixture:up = 上游直链,ol = OpenList REST 线协议。
type offlineStack struct {
	up, ol  *httptest.Server
	size    int64                  // /api/fs/get|list 返回的产物大小
	mtime   atomic.Pointer[string] // modified 字段(RFC3339Nano);测试与 HTTP handler 并发读写
	name    string                 // 只在目录列表里出现的真实文件名(对账场景)
	undone  string                 // /api/task/offline_download/undone 的 data
	lists   int64                  // /api/fs/list 调用次数
	headLen bool                   // HEAD 是否带 Content-Length(决定 expect>0 或 <=0)
	getMiss bool                   // /api/fs/get 固定报空(只能靠目录对账)
}

// setMtime 在测试侧更新产物 mtime(必须走原子,handler 可能正在读)。
func (s *offlineStack) setMtime(v string) {
	s.mtime.Store(&v)
}

func (s *offlineStack) getMtime() string {
	if p := s.mtime.Load(); p != nil {
		return *p
	}
	return ""
}

func newOfflineStack(t *testing.T) *offlineStack {
	t.Helper()
	s := &offlineStack{undone: `[]`, headLen: true}
	s.setMtime("2030-01-01T00:00:00Z")
	s.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if s.headLen {
				w.Header().Set("Content-Length", "300")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if s.headLen {
			w.Header().Set("Content-Length", "300")
			_, _ = w.Write(make([]byte, 300))
			return
		}
		// 无 Content-Length + 先 Flush:chunked 响应,让 Range 探测也拿不到大小(expect<=0 路径)。
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write(make([]byte, 300))
	}))
	s.ol = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/add_offline_download":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"tasks":[{"id":"rmt-1","state":1}]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/get":
			if s.getMiss {
				_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"size":0,"is_dir":false}}`))
				return
			}
			var body struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			name := body.Path
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"` + name +
				`","size":` + strconv.FormatInt(atomic.LoadInt64(&s.size), 10) +
				`,"is_dir":false,"modified":"` + s.getMtime() + `"}}`))
		case r.URL.Path == "/api/fs/list":
			atomic.AddInt64(&s.lists, 1)
			content := `[]`
			if s.name != "" {
				content = `[{"name":"` + s.name + `","size":` + strconv.FormatInt(atomic.LoadInt64(&s.size), 10) +
					`,"is_dir":false,"modified":"` + s.getMtime() + `"}]`
			}
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":` + content + `,"total":0}}`))
		case r.URL.Path == "/api/task/offline_download/undone":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":` + s.undone + `}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(func() { s.up.Close(); s.ol.Close() })
	return s
}

func (s *offlineStack) submitOffline(t *testing.T, tr *Transfer) (string, string) {
	t.Helper()
	task, err := tr.Submit(TransferRequest{
		SourceURL: s.up.URL + "/pool/same.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelOffline, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return task.ID, "/Pool/same.bin"
}

// 预测路径已命中且尺寸已足:done 并回填真实大小。
func TestOfflineConfirmLandsOnPredictedPath(t *testing.T) {
	s := newOfflineStack(t)
	atomic.StoreInt64(&s.size, 300)
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
	})
	id, _ := s.submitOffline(t, tr)
	waitState(t, tr, id, "pending", "done", "failed")
	if snap := tr.Get(id); snap.State != "done" || snap.Size != 300 {
		t.Fatalf("应 done/真实大小, got state=%s size=%d err=%s", snap.State, snap.Size, snap.Error)
	}
}

// 受理即 done:after<0 时不得再等确认窗口,也不得停在 pending。
func TestOfflineConfirmDisabledAcceptsOptimistically(t *testing.T) {
	s := newOfflineStack(t)
	atomic.StoreInt64(&s.size, 300)
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmAfter = -1
	})
	id, _ := s.submitOffline(t, tr)
	waitDone(t, tr, id)
	if snap := tr.Get(id); snap.State != "done" {
		t.Fatalf("after<0 应受理即 done, got state=%s err=%s", snap.State, snap.Error)
	}
}

// 源大小未知(expect<=0)时,同名旧残留不能当成产物。产物 mtime 早于任务起始，应继续等待而不是误判 done；mtime 刷新后才收口。
func TestOfflineConfirmUnknownSizeRequiresFreshArtifact(t *testing.T) {
	s := newOfflineStack(t)
	s.headLen = false                  // 源大小未知 -> expect<=0
	s.setMtime("2020-01-01T00:00:00Z") // 同名旧残留
	atomic.StoreInt64(&s.size, 300)
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.MaxBytes = -1 // 关闭硬上限:允许大小未知的显式 offline
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
	})
	id, _ := s.submitOffline(t, tr)
	waitState(t, tr, id, "pending")
	time.Sleep(300 * time.Millisecond)
	if snap := tr.Get(id); snap.State != "pending" {
		t.Fatalf("mtime 未刷新前不得 done, got state=%s size=%d", snap.State, snap.Size)
	}
	s.setMtime("2030-01-01T00:00:00Z")
	waitDone(t, tr, id)
	if snap := tr.Get(id); snap.State != "done" || snap.Size != 300 {
		t.Fatalf("mtime 刷新后应 done, got state=%s size=%d err=%s", snap.State, snap.Size, snap.Error)
	}
}

// 远端离线任务已失败时必须快速收口(failed/upstream_error)，不能空占 pending 直到确认窗口超时(生产默认 2h)。
func TestOfflineRemoteFailureFailsFast(t *testing.T) {
	s := newOfflineStack(t)
	s.undone = `[{"id":"rmt-1","state":4,"error":"404 not found"}]`
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
	})
	start := time.Now()
	id, _ := s.submitOffline(t, tr)
	waitDone(t, tr, id)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("远端失败应快速收口, 耗时 %s", d)
	}
	snap := tr.Get(id)
	if snap.State != "failed" || snap.Hint != "upstream_error" {
		t.Fatalf("got state=%s hint=%s err=%s", snap.State, snap.Hint, snap.Error)
	}
	if !strings.Contains(snap.Error, "404 not found") {
		t.Fatalf("失败原因应透出远端文案, got %q", snap.Error)
	}
}

// 预测落盘名未命中时要靠目录对账找回真实产物(Content-Disposition 改名)，并把快照指向真实文件名，而不是报 failed。
func TestOfflineTimeoutReconcilesRenamedArtifact(t *testing.T) {
	s := newOfflineStack(t)
	s.getMiss = true                              // 预测路径永远命中不到
	s.name = "renamed-by-content-disposition.bin" // 真实落盘名
	atomic.StoreInt64(&s.size, 300)
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmAfter = 400 * time.Millisecond
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
	})
	id, _ := s.submitOffline(t, tr)
	waitDone(t, tr, id)
	snap := tr.Get(id)
	if snap.State != "done" || snap.Size != 300 {
		t.Fatalf("对账应 done/300, got state=%s size=%d err=%s", snap.State, snap.Size, snap.Error)
	}
	if !strings.HasSuffix(snap.DestPath, s.name) {
		t.Fatalf("快照应指向真实文件名, got dest=%s", snap.DestPath)
	}
	if atomic.LoadInt64(&s.lists) == 0 {
		t.Fatalf("应走过目录对账")
	}
}

// pending 不是终结态:缓存超限剪枝不能把它删掉，否则确认器失去观测对象。
func TestOfflinePendingNotPruned(t *testing.T) {
	s := newOfflineStack(t)
	tr, _ := newTransferFixtureMut(t, s.up.URL, s.ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.KeepTasks = 1
		c.Transfer.OfflineConfirmPoll = 30 * time.Millisecond
	})
	pend, _ := s.submitOffline(t, tr)
	waitState(t, tr, pend, "pending")
	for i := 0; i < 3; i++ {
		task, err := tr.Submit(TransferRequest{
			SourceURL: s.up.URL + "/tmp/d" + strconv.Itoa(i) + "/f.bin", FID: FIDDirect,
			DestDir: "/Pool/d" + strconv.Itoa(i), Channel: ChannelStream, UA: "UA/1",
		})
		if err != nil {
			t.Fatalf("submit stream: %v", err)
		}
		waitDone(t, tr, task.ID)
	}
	if snap := tr.Get(pend); snap == nil || snap.State != "pending" {
		t.Fatalf("pending 任务不应被剪枝, got %v", snap)
	}
}
