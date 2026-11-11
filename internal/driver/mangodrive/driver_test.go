package mangodrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	// 只配内网基址:服务端自取直链要补成内网绝对地址。
	if want := f.srv.URL + "/d/Pool/a.mkv?sign=s1"; dl.ServerURL != want {
		t.Fatalf("server link: got %q want %q", dl.ServerURL, want)
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

func TestSwapBaseRewritesInternalLink(t *testing.T) {
	got := swapBase("http://127.0.0.1:15244/p/Pool/a.mkv?sign=s1",
		"http://127.0.0.1:15244", "https://drive.example.com")
	want := "https://drive.example.com/p/Pool/a.mkv?sign=s1"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if same := swapBase(want, "http://127.0.0.1:15244", "https://drive.example.com"); same != want {
		t.Fatalf("public link must stay untouched, got %q", same)
	}
}

// ---------------------------------------------------------------------------
// List 翻页回归:上游不返回 total 时不得静默截断
//
// 背景:原实现 `page*listPageSize >= out.Total` 作为终止条件。某些 OpenList
// 版本/接口形态**不返回 total**(字段缺省 → 0),此时 200 >= 0 恒真 →
// 恰好取满一页(200 条)就 break → 目录超过 200 条时后面的文件对客户端
// 完全不可见,且无任何报错(表现为「文件夹里少了文件」)。
//
// 本用例让上游返回 250 条、total 字段缺省,断言必须翻到第 2 页并取满 250 条。
// ---------------------------------------------------------------------------

// fakeListNoTotal 模拟「返回 total 缺失」的 fs/list 端点。
type fakeListNoTotal struct {
	t      *testing.T
	srv    *httptest.Server
	called int
}

func newFakeListNoTotal(t *testing.T, totalFiles int) *fakeListNoTotal {
	t.Helper()
	f := &fakeListNoTotal{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fs/list" {
			http.Error(w, `{"code":404,"message":"not found","data":null}`, http.StatusOK)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		page, _ := in["page"].(float64)
		perPage, _ := in["per_page"].(float64)
		if perPage <= 0 {
			perPage = listPageSize
		}
		f.called++
		start := int(page-1) * int(perPage)
		end := start + int(perPage)
		if end > totalFiles {
			end = totalFiles
		}
		var items []string
		if start < totalFiles {
			for i := start; i < end; i++ {
				items = append(items, `{"name":"f`+strconv.Itoa(i)+`","is_dir":false,"size":1}`)
			}
		}
		// ★ 关键:响应里**不含** total 字段(上游形态不返回)
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` +
			strings.Join(items, ",") + `]}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestListDoesNotTruncateWhenTotalMissing(t *testing.T) {
	const totalFiles = 250 // > per_page(200):必须翻页
	f := newFakeListNoTotal(t, totalFiles)
	api := NewAPI(newClient(t), f.srv.URL, "tok-1")

	objs, err := api.List(context.Background(), "/Pool")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != totalFiles {
		t.Fatalf("total 缺失时不得截断: got %d 条, want %d 条(翻页次数=%d)", len(objs), totalFiles, f.called)
	}
	if f.called < 2 {
		t.Fatalf("应至少翻 2 页,实际调用 %d 次", f.called)
	}
}

// 上游正常返回 total 时仍按 total 提前终止(不多翻页)。
func TestListStopsWhenTotalReached(t *testing.T) {
	var servedPages []int
	const totalFiles = 250
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		page, _ := in["page"].(float64)
		servedPages = append(servedPages, int(page))
		start := (int(page) - 1) * listPageSize
		end := start + listPageSize
		if end > totalFiles {
			end = totalFiles
		}
		var items []string
		for i := start; i < end; i++ {
			items = append(items, `{"name":"f`+strconv.Itoa(i)+`","is_dir":false,"size":1}`)
		}
		// total 正常返回 → 翻到第 2 页取满 250 后必须停止(不翻第 3 页)
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` +
			strings.Join(items, ",") + `],"total":250}}`))
	}))
	defer srv.Close()
	api := NewAPI(newClient(t), srv.URL, "tok-1")
	objs, err := api.List(context.Background(), "/Pool")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != totalFiles {
		t.Fatalf("got %d 条 want %d", len(objs), totalFiles)
	}
	if len(servedPages) != 2 {
		t.Fatalf("total=250 时应翻到第 2 页后停止,实际请求页=%v", servedPages)
	}
}
