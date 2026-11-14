package mangodrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
)

// parTreeFake 统计同层目录列表的并发峰值;每层列表人为加 40ms 延迟放大窗口。
type parTreeFake struct {
	entries               map[string]string
	mu                    sync.Mutex
	inflight, peak, calls int
}

func (f *parTreeFake) handler(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, `{"code":404,"message":"not found","data":null}`, http.StatusOK)
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	p, _ := in["path"].(string)
	f.mu.Lock()
	f.inflight++
	f.calls++
	if f.inflight > f.peak {
		f.peak = f.inflight
	}
	f.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"content":[` + f.entries[p] + `]}}`))
}

// 同层目录并发列表:峰值并发落在 [2, treeListConc],节点仍按 BFS 原顺序回填。
func TestListTreeParallelLevels(t *testing.T) {
	entries := map[string]string{}
	var root []string
	for i := 0; i < 8; i++ {
		root = append(root, fmt.Sprintf(`{"name":"d%d","is_dir":true,"size":0}`, i))
		entries[fmt.Sprintf("/Pool/d%d", i)] = fmt.Sprintf(`{"name":"f%d.bin","is_dir":false,"size":1}`, i)
	}
	entries["/Pool"] = strings.Join(root, ",")

	f := &parTreeFake{entries: entries}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	d := New(newClient(t))
	d.InternalBase = srv.URL
	nodes, err := d.ResolveShare(context.Background(),
		driver.ShareLink{URL: srv.URL + "/Pool"}, &driver.Credential{Cookie: "tok-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 16 {
		t.Fatalf("nodes=%d want 16", len(nodes))
	}
	for i := 0; i < 8; i++ {
		if want := fmt.Sprintf("d%d", i); nodes[i].Name != want {
			t.Fatalf("根层第 %d 节点=%q want %q", i, nodes[i].Name, want)
		}
		if want := fmt.Sprintf("d%d/f%d.bin", i, i); nodes[8+i].Name != want {
			t.Fatalf("子层第 %d 节点=%q want %q", i, nodes[8+i].Name, want)
		}
	}
	f.mu.Lock()
	peak, calls := f.peak, f.calls
	f.mu.Unlock()
	if calls != 9 { // 根 1 次 + 8 个子目录各 1 次
		t.Fatalf("calls=%d want 9", calls)
	}
	if peak < 2 {
		t.Fatalf("同层目录未并发, peak=%d", peak)
	}
	if peak > treeListConc {
		t.Fatalf("peak=%d 超出并发上限 %d", peak, treeListConc)
	}
}
