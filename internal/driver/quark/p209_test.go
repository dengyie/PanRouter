package quark

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// P2-9 回归:转存任务被上游接受后,若轮询因请求取消而中断(此后任务仍可能完成),
// 必须用独立有界 ctx 重新轮询拿到新 fid 并删除暂存副本,不得泄漏。
func TestAbandonedSaveTaskCleanup(t *testing.T) {
	var (
		deleteHits  atomic.Int32
		deleteBody  atomic.Value // string
		pollStarted = make(chan struct{})
		startOnce   sync.Once
		resumePoll  = make(chan struct{})
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/file/delete"):
			deleteHits.Add(1)
			b := make([]byte, 512)
			n, _ := r.Body.Read(b)
			deleteBody.Store(string(b[:n]))
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/task"):
			startOnce.Do(func() { close(pollStarted) })
			<-resumePoll
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"status":2,"save_as":{"save_as_top_fids":["newfid"]}}}`))
		case strings.HasSuffix(r.URL.Path, "/sharepage/save"):
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"task_id":"t1"}}`))
		case r.URL.Path == "/1/clouddrive/file":
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"fid":"tmp1"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
		}
	}))
	defer srv.Close()

	cl, err := httpx.New(httpx.Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, srv.URL)
	d.taskInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := d.GetDirectLink(ctx, &driver.Credential{Cookie: "c=1"}, driver.FileRef{
			Own: false, FID: "srcfid", ShareKey: "sk",
			Ext: map[string]string{"pwd_id": "pwd1", "stoken": "st", "pwd": "1234"},
		})
		done <- err
	}()

	<-pollStarted     // 首次任务轮询已被上游受理
	cancel()          // 请求方放弃
	close(resumePoll) // 放行轮询端点:原调用吃到 ctx 取消,清理重试拿到 status=2
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetDirectLink did not return")
	}
	if deleteHits.Load() != 1 {
		t.Fatalf("abandoned save copy must be cleaned, delete hits=%d", deleteHits.Load())
	}
	if body, _ := deleteBody.Load().(string); !strings.Contains(body, "newfid") {
		t.Fatalf("delete must target saved fid, body=%q", body)
	}
}
