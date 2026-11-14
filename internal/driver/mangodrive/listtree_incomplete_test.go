package mangodrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
)

// incompleteFake 是可注入失败的 OpenList:按路径统计调用次数,支持「始终失败」
// 与「前 N 次失败」,并记录全进程视角的在途并发峰值(验证进程级闸)。
type incompleteFake struct {
	entries   map[string]string // path → content JSON 片段
	alwaysBad map[string]bool
	badFirst  map[string]int
	delay     time.Duration

	mu             sync.Mutex
	calls          map[string]int
	inflight, peak int
}

func newIncompleteFake() *incompleteFake {
	return &incompleteFake{
		entries:   map[string]string{},
		alwaysBad: map[string]bool{},
		badFirst:  map[string]int{},
		calls:     map[string]int{},
	}
}

func (f *incompleteFake) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/fs/get" {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		p, _ := in["path"].(string)
		if _, ok := f.entries[p]; ok {
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"name":"d","is_dir":true,"size":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":500,"message":"object not found","data":null}`))
		return
	}
	if r.URL.Path != "/api/fs/list" {
		_, _ = w.Write([]byte(`{"code":404,"message":"not found","data":null}`))
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	p, _ := in["path"].(string)

	f.mu.Lock()
	f.calls[p]++
	n := f.calls[p]
	f.inflight++
	if f.inflight > f.peak {
		f.peak = f.inflight
	}
	bad := f.alwaysBad[p] || n <= f.badFirst[p]
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()

	if bad {
		_, _ = w.Write([]byte(`{"code":500,"message":"internal error","data":null}`))
		return
	}
	_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` + f.entries[p] + `]}}`))
}

func (f *incompleteFake) callCount(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[p]
}

func (f *incompleteFake) stats() (peak int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func newIncompleteDriver(t *testing.T, f *incompleteFake) *Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	d := New(newClient(t))
	d.InternalBase = srv.URL
	return d
}

// 根目录读不到 = 这次解析没有任何有效内容:必须报错,不能返回空列表
// (否则上层会把「上游挂了」当成「空文件夹」写进快照)。
func TestListTreeRootFailureReturnsError(t *testing.T) {
	f := newIncompleteFake()
	f.entries["/Pool"] = ""
	f.alwaysBad["/Pool"] = true
	d := newIncompleteDriver(t, f)

	_, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: "https://drive.example.com/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err == nil {
		t.Fatal("根目录列表失败必须返回错误")
	}
	if got := f.callCount("/Pool"); got != 2 {
		t.Fatalf("根目录应重试一次(共 2 次调用), got %d", got)
	}
}

// 子目录重试后仍失败:其余内容照常返回,但必须在末节点打 incomplete 标记
// —— 缺子树的快照不能让调用方当成完整结果。
func TestListTreeSubdirFailureMarksIncomplete(t *testing.T) {
	f := newIncompleteFake()
	f.entries["/Pool"] = `{"name":"good","is_dir":true,"size":0},{"name":"bad","is_dir":true,"size":0}`
	f.entries["/Pool/good"] = `{"name":"f.bin","is_dir":false,"size":1}`
	f.entries["/Pool/bad"] = ""
	f.alwaysBad["/Pool/bad"] = true
	d := newIncompleteDriver(t, f)

	nodes, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: "https://drive.example.com/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err != nil {
		t.Fatalf("单个子目录失败不应让整次解析失败: %v", err)
	}
	if len(nodes) != 3 { // good / bad / good/f.bin
		t.Fatalf("nodes=%d want 3: %+v", len(nodes), nodes)
	}
	last := nodes[len(nodes)-1]
	if last.Ext["incomplete"] != "1" {
		t.Fatalf("末节点应带 incomplete 标记: %+v", last)
	}
	if last.Ext["truncated"] != "" {
		t.Fatalf("读取失败不应伪装成「分享过大」截断: %+v", last)
	}
	if got := f.callCount("/Pool/bad"); got != 2 {
		t.Fatalf("失败子目录应重试一次(共 2 次), got %d", got)
	}
}

// 瞬时失败重试后成功:整树完整,不得留下 incomplete 标记。
func TestListTreeTransientSubdirFailureRetried(t *testing.T) {
	f := newIncompleteFake()
	f.entries["/Pool"] = `{"name":"d0","is_dir":true,"size":0},{"name":"d1","is_dir":true,"size":0}`
	f.entries["/Pool/d0"] = `{"name":"a.bin","is_dir":false,"size":1}`
	f.entries["/Pool/d1"] = `{"name":"b.bin","is_dir":false,"size":1}`
	f.badFirst["/Pool/d1"] = 1 // 首次 500,重试成功
	d := newIncompleteDriver(t, f)

	nodes, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: "https://drive.example.com/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 { // d0 / d1 / d0/a.bin / d1/b.bin
		t.Fatalf("nodes=%d want 4: %+v", len(nodes), nodes)
	}
	for _, n := range nodes {
		if n.Ext["incomplete"] == "1" {
			t.Fatalf("重试已成功,不应标 incomplete: %+v", n)
		}
	}
	if got := f.callCount("/Pool/d1"); got != 2 {
		t.Fatalf("瞬时失败应重试(共 2 次), got %d", got)
	}
}

// 进程级并发闸:两次解析同时进行时,上游 fs/list 总并发仍不得超过 treeListConc
// (否则批量解析会按「请求数 × 4」放大,把 OpenList 打出限流)。
func TestListTreeGlobalConcurrencyCap(t *testing.T) {
	f := newIncompleteFake()
	f.delay = 40 * time.Millisecond
	for _, root := range []string{"/A", "/B"} {
		var content string
		for i := 0; i < 4; i++ {
			dir := fmt.Sprintf("%s/d%d", root, i)
			if content != "" {
				content += ","
			}
			content += fmt.Sprintf(`{"name":"d%d","is_dir":true,"size":0}`, i)
			f.entries[dir] = fmt.Sprintf(`{"name":"f%d.bin","is_dir":false,"size":1}`, i)
		}
		f.entries[root] = content
	}
	d := newIncompleteDriver(t, f)

	var wg sync.WaitGroup
	for _, root := range []string{"/A", "/B"} {
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			nodes, err := d.ResolveShare(context.Background(),
				driver.ShareLink{URL: "https://drive.example.com" + root}, &driver.Credential{Cookie: "tok-1"})
			if err != nil {
				t.Errorf("resolve %s: %v", root, err)
				return
			}
			if len(nodes) != 8 {
				t.Errorf("%s nodes=%d want 8", root, len(nodes))
			}
		}(root)
	}
	wg.Wait()

	if peak := f.stats(); peak > treeListConc {
		t.Fatalf("上游 fs/list 总并发 %d 超出进程级上限 %d", peak, treeListConc)
	} else if peak < 2 {
		t.Fatalf("两次解析未产生并发, peak=%d", peak)
	}
}
