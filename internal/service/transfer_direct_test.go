package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// 直链直传(fid=direct)基础契约:HEAD 探大小 → stream 全量搬运 →
// 快照落 URL 末段名,产物完整送达 OpenList。
func TestDirectStreamTransfer(t *testing.T) {
	payload := strings.Repeat("d", 4096)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("上游应收到 UA")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	var gotPath string
	var gotLen int
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			gotPath = r.Header.Get("File-Path")
			b, _ := io.ReadAll(r.Body)
			gotLen = len(b)
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case r.URL.Path == "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
	})
	task, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/files/test-data.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelStream, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" {
		t.Fatalf("state=%s err=%s", snap.State, snap.Error)
	}
	if snap.FileName != "test-data.bin" || snap.DestPath != "/Pool/test-data.bin" {
		t.Fatalf("快照命名: file=%s path=%s", snap.FileName, snap.DestPath)
	}
	if snap.Size != int64(len(payload)) || snap.Done != int64(len(payload)) {
		t.Fatalf("size/done=%d/%d want %d", snap.Size, snap.Done, len(payload))
	}
	if gotLen != len(payload) {
		t.Fatalf("OpenList 收到 len=%d", gotLen)
	}
	if un, e := url.PathUnescape(gotPath); e != nil || un != "/Pool/test-data.bin" {
		t.Fatalf("File-Path=%q want /Pool/test-data.bin", gotPath)
	}
}

// 直链 offline:aria2 拉公网 URL,落盘名 = URL 末段(与快照一致)。
func TestDirectOfflineTransfer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "300")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	var offlinePath string
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case "/api/fs/add_offline_download":
			var body struct {
				Path string `json:"path"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			offlinePath = body.Path
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"tasks":[{"id":"1"}]}}`))
		case "/api/fs/get": // 落池确认器轮询:始终未落池,保持静默轮询即可
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"size":0,"is_dir":false}}`))
		default:
			t.Errorf("unexpected openlist call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
	})
	task, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/pub/archive.tar.gz", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelOffline, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// offline 受理后停在 pending(产物未落池):命名/调用参数在受理瞬间即已确定。
	waitState(t, tr, task.ID, "pending")
	snap := tr.Get(task.ID)
	if snap.FileName != "archive.tar.gz" || snap.DestPath != "/Pool/archive.tar.gz" {
		t.Fatalf("快照命名: file=%s path=%s (URL 末段)", snap.FileName, snap.DestPath)
	}
	if offlinePath != "/Pool" {
		t.Fatalf("add_offline path=%q want /Pool", offlinePath)
	}
}

// SSRF 闸门:私网直链在生产语义(AllowPrivate=false)下必须在受理前拒绝,
// 绝不能交给远端 aria2 去拉内网地址。
func TestDirectPrivateRejected(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("私网直链不应触达 OpenList")
	}))
	defer ol.Close()

	// 不开 DirectAllowPrivate:默认生产语义。
	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, nil)
	task, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/secret.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelOffline, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "failed" {
		t.Fatalf("私网直链应失败, got state=%s", snap.State)
	}
	if snap.Hint != string(driver.KindUnsupported) {
		t.Fatalf("hint=%q want %q (err=%s)", snap.Hint, driver.KindUnsupported, snap.Error)
	}
	if !strings.Contains(snap.Error, "私网") {
		t.Fatalf("错误信息应说明私网拒绝, got: %s", snap.Error)
	}
}

// 未知大小(HEAD/Range 都拿不到长度)的直链:auto 必须走 stream(由实收字节兜底),
// 显式 offline 在默认上限(64GiB)下按契约拒绝。
func TestDirectUnknownSizeChannelPolicy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 显式 chunked:HEAD 无 Content-Length,GET 也不给长度(强制未知大小)。
		w.Header().Set("Transfer-Encoding", "chunked")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("chunked-data"))
		fl.Flush()
	}))
	defer up.Close()

	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fs/mkdir":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		case "/api/fs/put":
			_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ol.Close()

	tr, _ := newTransferFixtureMut(t, up.URL, ol.URL, func(c *config.Config) {
		c.Transfer.DirectAllowPrivate = true
	})
	task, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/blob/no-length.bin", FID: FIDDirect,
		DestDir: "/Pool", UA: "UA/1", // auto
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" || snap.Channel != string(ChannelStream) {
		t.Fatalf("未知大小 auto 应回退 stream, got state=%s channel=%s err=%s",
			snap.State, snap.Channel, snap.Error)
	}

	task2, err := tr.Submit(TransferRequest{
		SourceURL: up.URL + "/blob/no-length.bin", FID: FIDDirect,
		DestDir: "/Pool", Channel: ChannelOffline, UA: "UA/1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task2.ID)
	snap2 := tr.Get(task2.ID)
	if snap2.State != "failed" || snap2.Hint != string(driver.KindUnsupported) {
		t.Fatalf("未知大小显式 offline 应拒绝, got state=%s hint=%s", snap2.State, snap2.Hint)
	}
}
