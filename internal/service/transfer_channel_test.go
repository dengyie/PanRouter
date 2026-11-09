package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// chooseChannel 是 auto/显式通道的判定中心:约束、阈值、上限三者
// 任何组合都不允许把超过 max_bytes 的源静默交给远端 aria2。
func TestChooseChannel(t *testing.T) {
	const (
		giB = int64(1) << 30
	)
	cases := []struct {
		name      string
		req       TransferChannel
		needHdr   bool
		bindIP    bool
		size      int64
		threshold int64
		maxBytes  int64
		want      TransferChannel
		wantErr   bool
		wantKind  driver.Kind
	}{
		{name: "auto 小文件走 offline", req: ChannelAuto, size: 100, threshold: 8 * giB, want: ChannelOffline},
		{name: "auto 恰好达阈值走 stream", req: ChannelAuto, size: 8 * giB, threshold: 8 * giB, want: ChannelStream},
		{name: "auto 超阈值走 stream", req: ChannelAuto, size: 9 * giB, threshold: 8 * giB, want: ChannelStream},
		{name: "auto 需请求头走 stream", req: ChannelAuto, needHdr: true, size: 100, threshold: 8 * giB, want: ChannelStream},
		{name: "auto 绑定 IP 走 stream", req: ChannelAuto, bindIP: true, size: 100, threshold: 8 * giB, want: ChannelStream},
		{name: "auto 大小未知有上限走 stream", req: ChannelAuto, size: 0, threshold: 8 * giB, maxBytes: 64 * giB, want: ChannelStream},
		{name: "auto 大小未知无上限走 stream", req: ChannelAuto, size: 0, threshold: 8 * giB, want: ChannelStream},
		{name: "auto 全离线且大小未知无上限走 offline", req: ChannelAuto, size: 0, threshold: -1, want: ChannelOffline},
		{name: "auto 全离线但大小未知有上限改走 stream", req: ChannelAuto, size: 0, threshold: -1, maxBytes: 64 * giB, want: ChannelStream},
		{name: "auto 小文件但超上限改走 stream", req: ChannelAuto, size: 2 * giB, threshold: 8 * giB, maxBytes: 1 * giB, want: ChannelStream},
		{name: "显式 offline 小文件", req: ChannelOffline, size: 100, want: ChannelOffline},
		{name: "显式 offline 大小未知无上限", req: ChannelOffline, size: 0, want: ChannelOffline},
		{name: "显式 offline 大小未知有上限拒绝", req: ChannelOffline, size: 0, maxBytes: 1 * giB, wantErr: true, wantKind: driver.KindUnsupported},
		{name: "显式 offline 超上限拒绝", req: ChannelOffline, size: 2 * giB, maxBytes: 1 * giB, wantErr: true, wantKind: driver.KindUnsupported},
		{name: "显式 stream 不预检上限", req: ChannelStream, size: 2 * giB, maxBytes: 1 * giB, want: ChannelStream},
		{name: "空通道等同 auto", req: "", size: 100, threshold: 8 * giB, want: ChannelOffline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseChannel(tc.req, tc.needHdr, tc.bindIP, tc.size, tc.threshold, tc.maxBytes)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got channel=%s", got)
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Kind != tc.wantKind {
					t.Fatalf("want kind=%s, got %v", tc.wantKind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("channel=%s want %s", got, tc.want)
			}
		})
	}
}

// 显式 offline 的源超过 transfer.max_bytes 时必须在提交远端任务前拒绝:
// 远端 aria2 不受本机实收字节校验约束,超限文件一旦开始拉取就无法回滚。
func TestOfflineRejectsOversizeBeforeRemote(t *testing.T) {
	payload := strings.Repeat("x", 300)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "300")
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	var remoteCalls int
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls++
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer ol.Close()

	tr, share := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.MaxBytes = 100
	})
	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelOffline, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "failed" {
		t.Fatalf("超限 offline 应失败, got state=%s size=%d", snap.State, snap.Size)
	}
	if snap.Hint != string(driver.KindUnsupported) {
		t.Fatalf("hint=%q want %q (err=%s)", snap.Hint, driver.KindUnsupported, snap.Error)
	}
	if remoteCalls != 0 {
		t.Fatalf("超限时不应调用 OpenList, got %d calls", remoteCalls)
	}
}

// 上游 chunked(无 Content-Length)时无法预检:必须在搬运途中按实收字节截断,
// 否则 max_bytes 会被 "未知长度" 的上游整段绕过。
func TestStreamAbortsChunkedOversizeMidTransfer(t *testing.T) {
	payload := strings.Repeat("c", 64<<10)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 不设 Content-Length 且显式 Close:强制 chunked 传输。
		w.Header().Set("Transfer-Encoding", "chunked")
		fl := w.(http.Flusher)
		for i := 0; i < 2; i++ {
			_, _ = w.Write([]byte(payload))
			fl.Flush()
		}
	}))
	defer up.Close()

	var received int64
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			n, _ := io.Copy(io.Discard, r.Body)
			atomic.AddInt64(&received, n)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		}
	}))
	defer ol.Close()

	// fakeDriver 登记的 Size=300 用于解析元数据;真实上游声明 128KiB。
	tr, share := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.MaxBytes = 1000
	})
	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	// 上游先声明 128KiB > 1000,预检即拒绝(不进入搬运)。
	if snap.State != "failed" || snap.Hint != string(driver.KindUnsupported) {
		t.Fatalf("state=%s hint=%s err=%s", snap.State, snap.Hint, snap.Error)
	}
	// 截断允许一个读缓冲的越过(io.Copy 先写后判错),但不能把 128KiB 全部搬完。
	if got := atomic.LoadInt64(&received); got >= int64(len(payload)*2) {
		t.Fatalf("超限文件被完整转发, OpenList 收到 %d 字节", got)
	}
}
