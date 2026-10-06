package api

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"

	"github.com/dengyie/panrouter/internal/pkg/metrics"
)

func webRouter(web fs.FS) http.Handler {
	return Router(Deps{
		Version: "webtest",
		Log:     zap.NewNop().Sugar(),
		Met:     metrics.New(),
		WebFS:   web,
	})
}

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":     {Data: []byte("<!doctype html>INDEX")},
		"assets/app.js":  {Data: []byte("console.log(1)")},
		"assets/app.css": {Data: []byte("body{}")},
	}
}

func doWeb(t *testing.T, h http.Handler, method, path string, accept string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWebServesIndex(t *testing.T) {
	h := webRouter(spaFS())
	res := doWeb(t, h, http.MethodGet, "/", "")
	body := readBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status=%d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / content-type=%s", res.Header.Get("Content-Type"))
	}
	if body != "<!doctype html>INDEX" {
		t.Fatalf("GET / body=%q", body)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("GET / cache-control=%q", cc)
	}
}

func TestWebServesHashedAsset(t *testing.T) {
	h := webRouter(spaFS())
	res := doWeb(t, h, http.MethodGet, "/assets/app.js", "")
	body := readBody(t, res)
	if res.StatusCode != http.StatusOK || body != "console.log(1)" {
		t.Fatalf("asset status=%d body=%q", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("asset content-type=%s", ct)
	}
}

func TestWebSPAFallback(t *testing.T) {
	h := webRouter(spaFS())
	res := doWeb(t, h, http.MethodGet, "/some/spa/route", "text/html")
	body := readBody(t, res)
	if res.StatusCode != http.StatusOK || body != "<!doctype html>INDEX" {
		t.Fatalf("fallback status=%d body=%q", res.StatusCode, body)
	}
}

func TestWebMissingAssetJSON404(t *testing.T) {
	h := webRouter(spaFS())
	res := doWeb(t, h, http.MethodGet, "/missing.png", "")
	body := readBody(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing.png status=%d body=%s", res.StatusCode, body)
	}
	if strings.Contains(body, "INDEX") {
		t.Fatalf("missing.png fell back to SPA: %s", body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil || m["code"] != float64(404) {
		t.Fatalf("missing.png want JSON 404, got %s", body)
	}
}

func TestWebDoesNotSwallowAPIOrHealthz(t *testing.T) {
	h := webRouter(spaFS())

	res := doWeb(t, h, http.MethodGet, "/api/v1/nope", "text/html")
	body := readBody(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/api/v1/nope status=%d body=%s", res.StatusCode, body)
	}
	if strings.Contains(body, "INDEX") {
		t.Fatalf("/api/v1/nope swallowed by SPA: %s", body)
	}

	res = doWeb(t, h, http.MethodGet, "/healthz", "text/html")
	body = readBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status=%d body=%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"ok"`) || strings.Contains(body, "INDEX") {
		t.Fatalf("/healthz body=%s", body)
	}

	res = doWeb(t, h, http.MethodGet, "/d/x", "text/html")
	body = readBody(t, res)
	if res.StatusCode != http.StatusNotFound || strings.Contains(body, "INDEX") {
		t.Fatalf("/d swallowed by SPA: %d %s", res.StatusCode, body)
	}
}

func TestWebReservedPathsJSON404(t *testing.T) {
	h := webRouter(spaFS())
	for _, p := range []string{
		"/api", "/api/nope", "/api/v1/nope", "/api/v1/../../settings",
		"/d", "/d/nope", "/stream", "/stream/nope",
		"/healthz/nope", "/readyz/nope", "/metrics/nope",
	} {
		t.Run(p, func(t *testing.T) {
			res := doWeb(t, h, http.MethodGet, p, "text/html")
			body := readBody(t, res)
			var m map[string]any
			if res.StatusCode != http.StatusNotFound || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || json.Unmarshal([]byte(body), &m) != nil || m["code"] != float64(404) {
				t.Fatalf("want JSON 404, status=%d content-type=%s body=%s", res.StatusCode, res.Header.Get("Content-Type"), body)
			}
		})
	}
}

func TestWebNavigationPolicy(t *testing.T) {
	h := webRouter(spaFS())
	cases := []struct {
		path   string
		accept string
		status int
	}{
		{"/releases/v1.2.3", "text/html", http.StatusOK},
		{"/users/alice@example.com", "text/html,application/xhtml+xml", http.StatusOK},
		{"/releases/v1.2.3", "text/html; charset=utf-8; q=0.8", http.StatusOK},
		{"/releases/v1.2.3", "", http.StatusNotFound},
		{"/releases/v1.2.3", "application/json", http.StatusNotFound},
		{"/releases/v1.2.3", "text/html;q=0", http.StatusNotFound},
		{"/releases/v1.2.3", "application/json; note=\"text/html\"", http.StatusNotFound},
		{"/missing.js", "*/*", http.StatusNotFound},
		{"/missing.css", "text/css,*/*;q=0.1", http.StatusNotFound},
		{"/missing.png", "image/*,*/*;q=0.8", http.StatusNotFound},
		{"/healthz-dashboard", "text/html", http.StatusOK},
		{"/metrics-dashboard", "text/html", http.StatusOK},
		{"/settings", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.path+"/"+tc.accept, func(t *testing.T) {
			res := doWeb(t, h, http.MethodGet, tc.path, tc.accept)
			body := readBody(t, res)
			if res.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.status, body)
			}
			if tc.status == http.StatusOK && (body != "<!doctype html>INDEX" || res.Header.Get("Cache-Control") != "no-cache") {
				t.Fatalf("want no-cache index, headers=%v body=%s", res.Header, body)
			}
		})
	}

	res := doWeb(t, h, http.MethodGet, "/assets/app.js", "text/html")
	if body := readBody(t, res); res.StatusCode != http.StatusOK || body != "console.log(1)" {
		t.Fatalf("existing asset lost precedence: %d %s", res.StatusCode, body)
	}

	res = doWeb(t, h, http.MethodHead, "/releases/v1.2.3", "text/html")
	if body := readBody(t, res); res.StatusCode != http.StatusOK || body != "" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("HEAD navigation: %d %v %s", res.StatusCode, res.Header, body)
	}

	res = doWeb(t, h, http.MethodPost, "/releases/v1.2.3", "text/html")
	if body := readBody(t, res); res.StatusCode != http.StatusNotFound || strings.Contains(body, "INDEX") {
		t.Fatalf("POST must not fallback: %d %s", res.StatusCode, body)
	}
}

func TestWebSystemEndpointsKeepHandlers(t *testing.T) {
	e := newE2E(t, true)
	h := Router(Deps{Version: "webtest", Store: e.store, Log: zap.NewNop().Sugar(), Met: metrics.New(), WebFS: spaFS()})
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		t.Run(p, func(t *testing.T) {
			res := doWeb(t, h, http.MethodGet, p, "text/html")
			body := readBody(t, res)
			if res.StatusCode != http.StatusOK || strings.Contains(body, "INDEX") {
				t.Fatalf("system handler lost: %d %s", res.StatusCode, body)
			}
			if p != "/metrics" && !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("system content-type=%s", res.Header.Get("Content-Type"))
			}
		})
	}
}

func TestWebNilFSJSON404(t *testing.T) {
	h := webRouter(nil)
	res := doWeb(t, h, http.MethodGet, "/", "text/html")
	body := readBody(t, res)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("nil WebFS GET / status=%d body=%s", res.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil || m["code"] != float64(404) {
		t.Fatalf("nil WebFS want JSON 404, got %s", body)
	}
}
