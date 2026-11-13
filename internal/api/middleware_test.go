package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// ErrAbortHandler 是「响应已发出、主动断连接」的信号:recoverer 必须原样上抛,
// 让 net/http 真的把连接断掉。若吞掉并写 500,chunked 响应会被补上结束块,
// 客户端拿到的是一个「HTTP 完整」的截断文件。
func TestRecovererRepanicsAbortHandler(t *testing.T) {
	d := &Deps{Log: zap.NewNop().Sugar()}
	h := d.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler must propagate, got %v", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("handler should have panicked")
}

func TestRecovererWrites500ForOtherPanics(t *testing.T) {
	d := &Deps{Log: zap.NewNop().Sugar()}
	h := d.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"kind":"internal"`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}
