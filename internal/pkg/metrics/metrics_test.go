package metrics

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	header  http.Header
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestHandlerDoesNotHoldRegistryLockDuringWrite(t *testing.T) {
	r := New()
	r.Inc("panrouter_resolve_total", map[string]string{"pan": "quark"})
	w := &blockingWriter{started: make(chan struct{}), release: make(chan struct{}), header: make(http.Header)}
	done := make(chan struct{})
	go func() {
		r.Handler().ServeHTTP(w, nil)
		close(done)
	}()
	<-w.started

	incDone := make(chan struct{})
	go func() {
		r.Inc("panrouter_resolve_total", map[string]string{"pan": "quark"})
		close(incDone)
	}()
	select {
	case <-incDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("metric update blocked while response writer was blocked")
	}
	close(w.release)
	<-done
}

// 回归:Inc/Set 过的指标必须自动出现在 /metrics,不依赖手工维护的白名单
// (v2.12.3 的新计数器曾因白名单未登记而「计数在跑、导出看不到」)。
func TestSnapshotExportsEveryRegisteredMetric(t *testing.T) {
	r := New()
	r.Inc("panrouter_new_counter_total", map[string]string{"k": "v"})
	r.Set("panrouter_new_gauge", map[string]string{"k": "v"}, 7)

	body := string(r.snapshot())
	for _, want := range []string{
		"# TYPE panrouter_new_counter_total counter",
		`panrouter_new_counter_total{k="v"} 1`,
		"# TYPE panrouter_new_gauge gauge",
		`panrouter_new_gauge{k="v"} 7`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}
