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

var strings300 = strings.Repeat("p", 300)

// 同目标并发写守卫(P1, 生产矩阵实测:stream PUT 与 offline 上传互踩导致
// OpenList 离线任务整单重试白拉 1GB):同路径同时只允许一个任务在写。
func TestDestConflictStreamVsStream(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "300")
			w.WriteHeader(http.StatusOK)
			return
		}
		<-release // 挂住第一个 stream 任务
		w.Header().Set("Content-Length", "300")
		_, _ = w.Write([]byte(strings300))
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
	})
	sub := func() *TransferTask {
		task, err := tr.Submit(TransferRequest{
			SourceURL: up.URL + "/same.bin", FID: FIDDirect,
			DestDir: "/Pool", Channel: ChannelStream, UA: "UA/1",
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return task
	}

	t1 := sub()
	deadline := time.Now().Add(3 * time.Second)
	for tr.Get(t1.ID).State != "running" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	t2 := sub()
	waitDone(t, tr, t2.ID)
	snap2 := tr.Get(t2.ID)
	if snap2.State != "failed" || snap2.Hint != "conflict" {
		t.Fatalf("同路径并发任务应 conflict 拒收, got state=%s hint=%s err=%s",
			snap2.State, snap2.Hint, snap2.Error)
	}

	close(release)
	waitDone(t, tr, t1.ID)
	if tr.Get(t1.ID).State != "done" {
		t.Fatalf("t1 应完成, got %s", tr.Get(t1.ID).State)
	}

	t3 := sub()
	waitDone(t, tr, t3.ID)
	if tr.Get(t3.ID).State != "done" {
		t.Fatalf("释放后同路径应可再次转存, got %s err=%s", tr.Get(t3.ID).State, tr.Get(t3.ID).Error)
	}
}

// offline 产物落池前任务必须停在 pending(不能乐观 done):
// pending 期间同路径的新任务要被拒收——这正是生产上「同路径二次提交抢在
// 首次上传落盘前、却又看到 done 于是放心提交」的根因。确认器观测到产物后
// 收口 done 并带上真实落盘大小,pending 期间的拒绝也不得堆成新记录。
func TestDestConflictOfflineHoldsUntilConfirm(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "300")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", "300")
		_, _ = w.Write(make([]byte, 300))
	}))
	defer up.Close()

	var fsSize int64
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/add_offline_download":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"tasks":[{"id":"1"}]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/get":
			var body struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"` +
				body.Path + `","size":` + strconv.FormatInt(atomic.LoadInt64(&fsSize), 10) + `,"is_dir":false}}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmPoll = 50 * time.Millisecond
	})
	sub := func(ch TransferChannel) *TransferTask {
		task, err := tr.Submit(TransferRequest{
			SourceURL: up.URL + "/same.bin", FID: FIDDirect,
			DestDir: "/Pool", Channel: ch, UA: "UA/1",
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		return task
	}

	t1 := sub(ChannelOffline)
	// 新契约:offline 受理后停在 pending,确认器仍占用 /Pool/same.bin。
	waitState(t, tr, t1.ID, "pending")
	if snap := tr.Get(t1.ID); snap.State != "pending" {
		t.Fatalf("产物未落池前不得 done, got state=%s", snap.State)
	}

	t2 := sub(ChannelStream)
	waitDone(t, tr, t2.ID)
	if snap := tr.Get(t2.ID); snap.State != "failed" || snap.Hint != "conflict" {
		t.Fatalf("offline 确认前同路径应拒收, got state=%s hint=%s err=%s",
			snap.State, snap.Hint, snap.Error)
	}

	atomic.StoreInt64(&fsSize, 300) // 产物落池 → 确认器收口 done 并放行
	waitDone(t, tr, t1.ID)
	if snap := tr.Get(t1.ID); snap.State != "done" || snap.Size != 300 {
		t.Fatalf("确认后应收口 done/真实大小, got state=%s size=%d err=%s",
			snap.State, snap.Size, snap.Error)
	}

	t3 := sub(ChannelStream)
	waitDone(t, tr, t3.ID)
	if snap := tr.Get(t3.ID); snap.State != "done" {
		t.Fatalf("确认后同路径应放行, got state=%s err=%s", snap.State, snap.Error)
	}
}

// 确认窗口超时:产物迟迟未现也收口 failed(不能再假装 done),
// 同时释放同路径占用,避免路径被永久锁死。
func TestDestConflictOfflineTimeoutReleases(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "300")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", "300")
		_, _ = w.Write(make([]byte, 300))
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/add_offline_download":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"tasks":[{"id":"1"}]}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/get":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"size":0,"is_dir":false}}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
		c.Transfer.OfflineConfirmAfter = 200 * time.Millisecond
		c.Transfer.OfflineConfirmPoll = 40 * time.Millisecond
	})
	t1, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/same.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelOffline, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitState(t, tr, t1.ID, "pending", "failed") // 确认窗口(200ms)超时 → failed
	time.Sleep(500 * time.Millisecond)

	if snap := tr.Get(t1.ID); snap.State != "failed" || snap.Hint != "upstream_error" {
		t.Fatalf("产物未落池应收口 failed(upstream_error), got state=%s hint=%s err=%s",
			snap.State, snap.Hint, snap.Error)
	}

	t2, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/same.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelStream, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, t2.ID)
	if snap := tr.Get(t2.ID); snap.State != "done" {
		t.Fatalf("超时释放后同路径应放行, got state=%s err=%s", snap.State, snap.Error)
	}
}
