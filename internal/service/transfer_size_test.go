package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P1 契约:解析阶段拿不到文件大小时(link.Size=0),任务完成后
// Size/Done 必须回填为真实搬运字节数,否则轮询端永远停在 0%。
func TestTransferUnknownSourceSizeBackfilled(t *testing.T) {
	payload := strings.Repeat("z", 4321)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4321")
		_, _ = w.Write([]byte(payload))
	}))
	defer up.Close()

	var gotPath string
	ol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/fs/put":
			gotPath = r.Header.Get("File-Path")
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
	// fakeDriver 解析出的 FileNode.Size 与直链条目大小无关, link.Size 为 0,
	// 正是线上「解析期不知道大小」的形态。

	task, err := tr.Submit(TransferRequest{SourceURL: share, Channel: ChannelStream, DestDir: "/Pool", UA: "UA/1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitDone(t, tr, task.ID)
	snap := tr.Get(task.ID)
	if snap.State != "done" {
		t.Fatalf("state=%s err=%s hint=%s", snap.State, snap.Error, snap.Hint)
	}
	if snap.Size != 4321 || snap.Done != 4321 {
		t.Fatalf("size/done 未回填: size=%d done=%d want 4321", snap.Size, snap.Done)
	}
	if gotPath == "" {
		t.Fatalf("File-Path header missing")
	}
}
