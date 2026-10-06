package api

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// reservedWebPath 这些前缀永远不回退到 index.html,避免 SPA 吞掉 API/下载/探活。
func reservedWebPath(p string) bool {
	p = path.Clean("/" + p)
	for _, prefix := range []string{"/api", "/d", "/stream", "/healthz", "/readyz", "/metrics"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func spaFallback(name string, r *http.Request) bool {
	if path.Ext(name) == "" {
		return true
	}
	for _, value := range r.Header.Values("Accept") {
		for _, item := range strings.Split(value, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || mediaType != "text/html" {
				continue
			}
			if raw, ok := params["q"]; ok {
				q, err := strconv.ParseFloat(raw, 64)
				if err != nil || !(q > 0 && q <= 1) {
					continue
				}
			}
			return true
		}
	}
	return false
}

// handleWeb 托管 web/dist:命中文件则原样返回;页面导航未命中则回退 index.html。
func (d *Deps) handleWeb(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		d.serveIndex(w, r)
		return
	}
	if d.serveStatic(w, r, name) {
		return
	}
	if spaFallback(name, r) {
		d.serveIndex(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
}

func (d *Deps) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	if !d.serveStatic(w, r, "index.html") {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "index.html missing"})
	}
}

// serveStatic 打开并写出 WebFS 中的普通文件。目录不列出。命中返回 true。
func (d *Deps) serveStatic(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := d.WebFS.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	if name == "index.html" && w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
			return true
		}
		http.ServeContent(w, r, path.Base(name), st.ModTime(), bytes.NewReader(data))
		return true
	}
	http.ServeContent(w, r, path.Base(name), st.ModTime(), rs)
	return true
}
