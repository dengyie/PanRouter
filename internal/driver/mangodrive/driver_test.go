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
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		p, _ := in["path"].(string)
		// 路径感知:根层 {文件,子目录},子目录 {孙文件},其余为空——否则
		// 递归 ResolveShare 会对同一内容无限下探(触上限截断)。
		var content string
		switch p {
		case "/Pool":
			content = `{"name":"a.mkv","is_dir":false,"size":1024},{"name":"sub","is_dir":true,"size":0}`
		case "/Pool/sub":
			content = `{"name":"b.bin","is_dir":false,"size":2048}`
		default:
			content = ""
		}
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` +
			content + `],"total":2}}`))
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

	// 目录:整棵子树(根层 2 条 + sub 内 1 条),Name 为相对分享根路径。
	nodes, err := d.ResolveShare(ctx, driver.ShareLink{URL: f.srv.URL + "/Pool"}, cred)
	if err != nil {
		t.Fatalf("resolve dir: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("整树应有 3 节点(根2 + sub 内 1), got %+v", nodes)
	}
	if nodes[0].Name != "a.mkv" || nodes[1].Name != "sub" || !nodes[1].IsDir {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if nodes[2].Name != "sub/b.bin" || nodes[2].IsDir || nodes[2].Size != 2048 {
		t.Fatalf("子目录未递归或相对名错误: %+v", nodes[2])
	}
	if nodes[0].Ext["path"] != "/Pool/a.mkv" {
		t.Fatalf("path context lost: %+v", nodes[0].Ext)
	}
	if nodes[2].Ext["path"] != "/Pool/sub/b.bin" {
		t.Fatalf("子节点 path 上下文丢失: %+v", nodes[2].Ext)
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

// ---------------------------------------------------------------------------
// ResolveShare 整树递归回归
//
// mangodrive 是存储路径视图,旧实现只 List 根层:嵌套分享的子目录文件对
// 客户端完全不可见(auto-link 也只挂顶层)。现服务端 BFS 整树;上限触发时
// 必须诚实打 truncated 标,而不是静默返回半棵树装作完整。
// ---------------------------------------------------------------------------

// fakeTree 按目录前缀返回不同层;dirs 时序固定,便于断言调用次数。
type fakeTree struct {
	t        *testing.T
	srv      *httptest.Server
	entries  map[string]string // path -> content JSON 片段
	listed   []string          // 收到的 path 序列
	listCall int
}

func newFakeTree(t *testing.T, entries map[string]string) *fakeTree {
	t.Helper()
	f := &fakeTree{t: t, entries: entries}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/get" {
			// ResolveShare 先 Get 确认目录存在;凡在 entries 里的路径都算目录。
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			p, _ := in["path"].(string)
			if _, ok := f.entries[p]; ok {
				_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"` +
					strings.TrimPrefix(p, "/") + `","is_dir":true,"size":0}}`))
			} else {
				_, _ = w.Write([]byte(`{"code":500,"message":"object not found","data":null}`))
			}
			return
		}
		if r.URL.Path != "/api/fs/list" {
			http.Error(w, `{"code":404,"message":"not found","data":null}`, http.StatusOK)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		p, _ := in["path"].(string)
		f.listed = append(f.listed, p)
		f.listCall++
		_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` +
			f.entries[p] + `]}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestResolveShareTruncatesHonestyOnNodeCap(t *testing.T) {
	// 平铺 1200 个文件目录 > treeMaxNodes(1000):必须触限并打标。
	var sb strings.Builder
	for i := 0; i < 1200; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"name":"f`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(`","is_dir":false,"size":1}`)
	}
	f := newFakeTree(t, map[string]string{"/Pool": sb.String()})
	d := New(newClient(t))
	d.InternalBase = f.srv.URL

	nodes, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: f.srv.URL + "/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != treeMaxNodes {
		t.Fatalf("节点应恰好截在 %d, got %d", treeMaxNodes, len(nodes))
	}
	last := nodes[len(nodes)-1].Ext["truncated"]
	if last != "1" {
		t.Fatalf("触限后最后一个节点必须带 truncated=1, got Ext %+v", nodes[len(nodes)-1].Ext)
	}
}

func TestResolveShareDepthCapMarksTruncated(t *testing.T) {
	// 自指目录:每个目录都含同名子目录 → 深度触限(treeMaxDepth)截断。
	deep := map[string]string{}
	var build func(prefix string, level int)
	build = func(prefix string, level int) {
		if level > treeMaxDepth+3 {
			return
		}
		deep[prefix] = `{"name":"sub","is_dir":true,"size":0}`
		build(prefix+"/sub", level+1)
	}
	build("/Pool", 0)
	f := newFakeTree(t, deep)
	d := New(newClient(t))
	d.InternalBase = f.srv.URL

	nodes, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: f.srv.URL + "/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	// 深度 8 → 应收到 sub, sub/sub, ..., 8 层共 8 个节点,带 truncated 标。
	if len(nodes) != treeMaxDepth {
		t.Fatalf("深度触限应返回 %d 层, got %d", treeMaxDepth, len(nodes))
	}
	if nodes[len(nodes)-1].Name != "sub/sub/sub/sub/sub/sub/sub/sub" {
		t.Fatalf("最深节点相对名错误: %q", nodes[len(nodes)-1].Name)
	}
	if nodes[len(nodes)-1].Ext["truncated"] != "1" {
		t.Fatalf("深度触限必须打 truncated 标: %+v", nodes[len(nodes)-1].Ext)
	}
	// 列表调用数 == 深度(每层一次),不随环爆炸。
	if f.listCall != treeMaxDepth {
		t.Fatalf("BFS 应恰 List %d 次, got %d (%v)", treeMaxDepth, f.listCall, f.listed)
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
