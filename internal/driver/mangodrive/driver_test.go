package mangodrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// fakeOpenList 模拟 OpenList REST v3 的最小行为面:鉴权 + fs/get + fs/list。
type fakeOpenList struct {
	t        *testing.T
	token    string
	srv      *httptest.Server
	authed   map[string]bool
	listPage int
	listCall int
}

func newFake(t *testing.T) *fakeOpenList {
	t.Helper()
	f := &fakeOpenList{t: t, token: "tok-1", authed: map[string]bool{}, listPage: 3}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenList) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/auth/login" && r.Header.Get("Authorization") != f.token {
		http.Error(w, `{"code":401,"message":"token is invalidated","data":null}`, http.StatusOK)
		return
	}
	switch r.URL.Path {
	case "/api/auth/login":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "admin" || in["password"] != "pw" {
			_, _ = w.Write([]byte(`{"code":400,"message":"wrong password","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"token":"tok-1"}}`))
	case "/api/me":
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"id":1,"username":"admin"}}`))
	case "/api/fs/get":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		p, _ := in["path"].(string)
		switch p {
		case "/Pool":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"Pool","is_dir":true,"size":0}}`))
		case "/Pool/a.mkv":
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"a.mkv","is_dir":false,"size":1024,"raw_url":"/d/Pool/a.mkv?sign=s1"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":500,"message":"object not found","data":null}`))
		}
	case "/api/fs/list":
		f.listCall++
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` +
			`{"name":"a.mkv","is_dir":false,"size":1024},{"name":"sub","is_dir":true,"size":0}],"total":2}}`))
	default:
		f.t.Fatalf("unexpected path %s", r.URL.Path)
	}
}

func newTestDriver(base string) *Driver {
	d := New(nil)
	d.InternalBase = base
	return d
}

func newClient(t *testing.T) *httpx.Client {
	t.Helper()
	c, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDriverResolveAndLink(t *testing.T) {
	f := newFake(t)
	d := New(newClient(t))
	d.InternalBase = f.srv.URL
	cred := &driver.Credential{Cookie: "tok-1"}
	ctx := context.Background()

	// 目录:两个条目,子目录带 path 上下文。
	nodes, err := d.ResolveShare(ctx, driver.ShareLink{URL: f.srv.URL + "/Pool"}, cred)
	if err != nil {
		t.Fatalf("resolve dir: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Name != "a.mkv" || !nodes[1].IsDir {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if nodes[0].Ext["path"] != "/Pool/a.mkv" {
		t.Fatalf("path context lost: %+v", nodes[0].Ext)
	}

	// 文件:单元素,直链带 UA 与 TTL。
	one, err := d.ResolveShare(ctx, driver.ShareLink{URL: f.srv.URL + "/Pool/a.mkv"}, cred)
	if err != nil || len(one) != 1 || one[0].IsDir {
		t.Fatalf("resolve file: %+v %v", one, err)
	}
	dl, err := d.GetDirectLink(ctx, cred, driver.FileRef{FID: one[0].FID, Ext: one[0].Ext})
	if err != nil {
		t.Fatalf("direct link: %v", err)
	}
	if !strings.HasSuffix(dl.URL, "/d/Pool/a.mkv?sign=s1") {
		t.Fatalf("raw_url not absolutized: %s", dl.URL)
	}
	if dl.UA == "" || !dl.ExpiresAt.After(time.Now()) {
		t.Fatalf("link constraints missing: %+v", dl)
	}
}

func TestDriverCheckCredential(t *testing.T) {
	f := newFake(t)
	d := New(newClient(t))
	d.InternalBase = f.srv.URL
	ctx := context.Background()

	st, err := d.CheckCredential(ctx, driver.Credential{Cookie: "tok-1"})
	if err != nil || !st.Valid || st.Nickname != "admin" {
		t.Fatalf("valid token: %+v %v", st, err)
	}
	st, err = d.CheckCredential(ctx, driver.Credential{Cookie: "bad-token"})
	if err == nil {
		t.Fatalf("bad token must fail")
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.KindAuthExpired {
		t.Fatalf("bad token must be auth_expired, got %v", err)
	}
}
