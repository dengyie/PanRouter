package metrics

import (
	"net/http"
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
