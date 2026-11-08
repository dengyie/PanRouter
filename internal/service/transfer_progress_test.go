package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 复现线上形态: OpenList 收完 body 后迟迟不回包(落盘到 union/crypt 很慢),
// 此时 done 必须已经涨到全量。若这里是 0, 说明进度根本没被计数。
func TestTransferProgressVisibleWhilePutPending(t *testing.T) {
	payload := strings.Repeat("p", 100000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	release := make(chan struct{})
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
			<-release // 模拟 OpenList 落盘慢, 回包前卡住
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

	deadline := time.Now().Add(5 * time.Second)
	var peak int64
	for time.Now().Before(deadline) {
		s := tr.Get(task.ID)
		if s != nil && s.Done > peak {
			peak = s.Done
		}
		if peak >= 100000 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	waitDone(t, tr, task.ID)

	if peak != 100000 {
		t.Fatalf("PUT 挂起期间进度未涨到全量: peak=%d want 100000", peak)
	}
	if snap := tr.Get(task.ID); snap.Done != 100000 {
		t.Fatalf("终态 done=%d want 100000", snap.Done)
	}
}

// 回归:Get 返回的是快照副本,API handler 会再对它调一次 Snapshot。
// 二次快照必须保留进度,否则线上轮询永远读到 done=0(即使 size 正确)。
func TestTransferSnapshotIdempotent(t *testing.T) {
	payload := strings.Repeat("z", 4096)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			_, _ = io.Copy(io.Discard, r.Body)
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
	if snap.Done != 4096 || snap.Size != 4096 {
		t.Fatalf("首次快照 size/done=%d/%d want 4096", snap.Size, snap.Done)
	}
	again := snap.Snapshot()
	if again.Done != 4096 || again.Size != 4096 {
		t.Fatalf("二次快照 size/done=%d/%d want 4096", again.Size, again.Done)
	}
}
