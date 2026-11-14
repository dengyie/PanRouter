// Package mangodrive 实现私有云盘(Mango Drive / OpenList 后端)driver。

package mangodrive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

const (
	BrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// rawLinkTTL:OpenList 直链默认有效期 30 分钟;保守按 20 分钟缓存,
	// 过期由 Resolver.GetFreshLink 走 singleflight 续签。
	rawLinkTTL = 20 * time.Minute
	// maxListPages 单目录翻页上界,防异常响应把解析拖死。
	// 50 × 200 = 单目录 1 万条上界(实测 250 文件双页正常);命中上界说明目录异常,宁截断不拖死。
	maxListPages = 50
	// listPageSize 单页条数(OpenList 上限 500)。
	listPageSize = 200
)

type Driver struct {
	client *httpx.Client
	// InternalBase 可选:内网(EasyTier)API 基址,如 http://10.0.0.3:5244。
	// 与 URL 主域一致时走该基址,绕开 Cloudflare 隧道的请求体上限与额外一跳。
	InternalBase string
	// PublicBase 可选:对外域名基址,用于把相对 raw_url 还原成可公网下载的直链。
	PublicBase string
}

func New(client *httpx.Client) *Driver { return &Driver{client: client} }

func (d *Driver) ID() string { return "mangodrive" }

// ---- URL → (API 基址, 存储路径) ----

// resolve 返回该链接应使用的 API 基址与存储内路径。
func (d *Driver) resolve(rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return "", "", driver.NewErr(driver.KindNotFound, "Mango Drive 链接无效", err)
	}
	public := u.Scheme + "://" + u.Host
	p := u.Path
	if un, uerr := url.PathUnescape(u.EscapedPath()); uerr == nil {
		p = un
	}
	return d.pickBase(public), cleanPath(p), nil
}

// pickBase 返回 API 基址:配置了内网基址则走内网(同一存储、同一 token),
// 否则用链接自身的 scheme://host。raw_url 仍由上游按公网域名签发。
func (d *Driver) pickBase(public string) string {
	if d.InternalBase != "" {
		return strings.TrimRight(d.InternalBase, "/")
	}
	return public
}

// cleanPath 归一化:补前导斜杠、折叠重复斜杠、去尾斜杠(根目录除外)。
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// FidHashPrefix 哈希 fid 的前缀:哈希不可逆,客户端由它推不出父目录,
// 服务端直链提链依赖分享快照回放(ref.Ext["path"])或历史缓存反查。
// 导出供 resolver 做历史反查的形态门控(仅哈希 fid 反查,防跨驱动撞名)。
const FidHashPrefix = "p:"

// fidFor 生成稳定 FID:短路径直接用路径(便于人工排查),超长路径取哈希。
func fidFor(p string) string {
	if len(p) <= 100 {
		return p
	}
	h := sha1.Sum([]byte(p))
	return FidHashPrefix + hex.EncodeToString(h[:])
}

// ResolveShare 解析一个存储路径(目录 → 整棵子树,文件 → 单元素)。
//
// mangodrive 是存储路径视图(URL path == 存储 path),没有分享快照可回放,
// 所以这里自己把树走完:根层 List 一次,再对每个目录 BFS 下探,直到没有新
// 目录或触达上限。Name 一律重写为**相对分享根的路径**(上游只给叶子名,而
// 客户端 build_plan/快照回放都要相对路径;根层叶子自然等于自身)。
//
// 上限(防循环引用/病态大树把 90s resolve 预算打爆):深度 8 层、总节点
// 1000 个。触限时对最后一个节点打 Ext["truncated"]="1"(resolver 的
// listingFlags 会把它提升为 ResolveShareResult.Truncated);目录读取失败
// 重试一次仍失败则打 Ext["incomplete"]="1"(子树缺失,快照不完整);预算
// 耗尽同样收窄返回,绝不 panic 也绝不静默丢整棵。
func (d *Driver) ResolveShare(ctx context.Context, share driver.ShareLink, cred *driver.Credential) ([]driver.FileNode, error) {
	api, p, err := d.target(share.URL, cred)
	if err != nil {
		return nil, err
	}
	obj, err := api.Get(ctx, p)
	if err != nil {
		return nil, err
	}
	if !obj.IsDir {
		return []driver.FileNode{nodeOf(p, *obj)}, nil
	}
	nodes, truncated, incomplete, err := listTree(ctx, api, p)
	if err != nil {
		return nil, err
	}
	if len(nodes) > 0 {
		last := &nodes[len(nodes)-1]
		if truncated {
			last.Ext["truncated"] = "1"
		}
		if incomplete {
			// 子树缺失(重试后仍读不到):让 resolver 把「列表不完整」如实
			// 告诉客户端,而不是把缺节点的快照当成完整结果。
			last.Ext["incomplete"] = "1"
		}
	}
	return nodes, nil
}

// 树遍历上限:与 ingest_tree 客户端展开(8 层/200 目录)同量级,服务端略宽
// (节点口径而非目录口径,一个千文件平铺目录也要能整树返回)。
const (
	treeMaxDepth = 8
	treeMaxNodes = 1000
	// treeListConc 同一层目录的并发列表上限。生产实证:66 节点整树串行
	// fs/list 耗时 9.3s(每目录约 200-300ms),而提链阶段并发后只有 0.45s;
	// 4 路并发把目录层压到 1-2 波,同时远低于 OpenList 的承受上限。
	treeListConc = 4
)

type treeDir struct {
	path string // 存储内绝对路径
	rel  string // 相对 root 的路径("" = 根层)
}

type treeListed struct {
	list []Obj
	err  error
}

// listSem 是全进程共享的目录列表并发闸。
//
// treeListConc 只约束**单次解析**内部的并发;而桌面端可以并发发起多次解析
// (批量解析上限 50),没有进程级闸时上游瞬时并发会按「请求数 × 4」放大,
// OpenList 被自家客户端打出限流,表现就是部分目录读取失败、整树缺节点。
// 故把并发额度做成进程级共享:单请求空闲时仍能拿满 4 路,多请求并发时
// 排队而不是叠加。
var listSem = make(chan struct{}, treeListConc)

// listOne 列一个目录:先拿进程级并发额度,再把 panic 收口成错误。
//
// ★ listDirsBounded 在**独立 goroutine** 里调用它:driver 的 panic 不会经过
//   chi 的 recoverer(net/http 只 recover 处理请求的那个 goroutine),逃出去
//   就是整个进程崩溃。单个目录读坏没有理由毁掉服务,故在此收口。
func listOne(ctx context.Context, api *API, p string) (list []Obj, err error) {
	defer func() {
		if r := recover(); r != nil {
			list, err = nil, driver.NewErr(driver.KindUpstream,
				"Mango Drive 列目录时发生内部错误", fmt.Errorf("panic: %v", r))
		}
	}()
	select {
	case listSem <- struct{}{}:
		defer func() { <-listSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return api.List(ctx, p)
}

// listDirsBounded 并发列一组目录,结果与入参同序(下标对应),便于上层按
// 原 BFS 顺序回填节点;ctx 取消后不再派发新目录。
func listDirsBounded(ctx context.Context, api *API, dirs []treeDir) []treeListed {
	out := make([]treeListed, len(dirs))
	conc := treeListConc
	if conc > len(dirs) {
		conc = len(dirs)
	}
	if conc <= 1 {
		for i := range dirs {
			out[i].list, out[i].err = listOne(ctx, api, dirs[i].path)
		}
		return out
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				out[i].list, out[i].err = listOne(ctx, api, dirs[i].path)
			}
		}()
	}
	for i := range dirs {
		if ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out
}

// listTree 从根目录 BFS 整棵子树。
//
// 返回值:
//   - nodes: 相对名节点,按 BFS 原顺序;
//   - truncated: 触达深度/节点上限(有内容未展开);
//   - incomplete: 有目录读取失败(重试一次后仍失败),该子树缺失;
//   - err: 根目录列表失败 —— 此时没有任何有效内容,必须如实报错,
//     不能返回空列表让上层把分享当成「空文件夹」。
//
// 子目录失败不再静默跳过:先原地重试一次(上游限流/抖动多为瞬时),仍失败
// 才记 incomplete。缺了子树的快照必须让调用方知道,否则「静默少文件」比
// 报错危险得多。
func listTree(ctx context.Context, api *API, root string) (nodes []driver.FileNode, truncated, incomplete bool, err error) {
	nodes = make([]driver.FileNode, 0, 16)
	queue := []treeDir{{path: root}}
	seen := map[string]bool{root: true} // 目录去重(防御性:软链/异常上游可能成环)
	for depth := 0; depth < treeMaxDepth && len(queue) > 0; depth++ {
		if len(nodes) >= treeMaxNodes || ctx.Err() != nil {
			return nodes, true, incomplete, nil
		}
		listed := listDirsBounded(ctx, api, queue)
		var next []treeDir
		for i, q := range queue {
			if len(nodes) >= treeMaxNodes || ctx.Err() != nil {
				return nodes, true, incomplete, nil
			}
			if listed[i].err != nil && ctx.Err() == nil {
				// 瞬时失败(限流/网络抖动)重试一次;串行重试,不再叠加并发。
				listed[i].list, listed[i].err = listOne(ctx, api, q.path)
			}
			if listed[i].err != nil {
				if depth == 0 {
					return nil, false, false, listed[i].err
				}
				incomplete = true
				continue
			}
			list := listed[i].list
			for j := range list {
				if len(nodes) >= treeMaxNodes || ctx.Err() != nil {
					return nodes, true, incomplete, nil
				}
				child := list[j]
				full := join(q.path, child.Name)
				rel := child.Name
				if q.rel != "" {
					rel = q.rel + "/" + child.Name
				}
				n := nodeOf(full, child)
				n.Name = rel
				nodes = append(nodes, n)
				if child.IsDir && !seen[full] {
					seen[full] = true
					next = append(next, treeDir{path: full, rel: rel})
				}
			}
		}
		queue = next
	}
	return nodes, len(queue) > 0, incomplete, nil // 深度触限仍有未下探目录
}

// target 解析链接并返回已鉴权的 API。
func (d *Driver) target(rawURL string, cred *driver.Credential) (*API, string, error) {
	base, p, err := d.resolve(rawURL)
	if err != nil {
		return nil, "", err
	}
	if cred == nil || cred.Cookie == "" {
		return nil, "", driver.NewErr(driver.KindAuthExpired, "Mango Drive 需要登录态,请先在「账号管理」添加账号", nil)
	}
	return NewAPI(d.client, base, cred.Cookie), p, nil
}

// GetDirectLink 获取文件直链。ref.Ext 里带 path 时随之(分享快照回放)。
func (d *Driver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if cred == nil || cred.Cookie == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "Mango Drive 需要登录态,请先在「账号管理」添加账号", nil)
	}
	p := ""
	if ref.Ext != nil {
		p = ref.Ext["path"]
	}
	if p == "" {
		p = ref.FID
	}
	if p == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "Mango Drive 缺少文件路径", nil)
	}
	base := d.pickBase(d.PublicBase)
	if base == "" {
		// 未配内网基址且提链时拿不到链接 host(公开分享 URL 才有 host):
		// 与其发出无 scheme 的请求得到费解的连接错误,不如显式报配置缺失。
		return driver.DirectLink{}, driver.NewErr(driver.KindUnsupported, "Mango Drive 未配置 API 基址(public_base/internal_base)", nil)
	}
	api := NewAPI(d.client, base, cred.Cookie)
	obj, err := api.Get(ctx, p)
	if err != nil {
		return driver.DirectLink{}, err
	}
	if obj.IsDir {
		return driver.DirectLink{}, driver.NewErr(driver.KindUnsupported, "Mango Drive 目录不支持直链下载", nil)
	}
	if obj.RawURL == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged, "Mango Drive 未返回 raw_url", nil)
	}
	// 内网基址取到的 raw_url 指向 mesh/回环地址,客户端无法访问;
	// 换回对外域名(sign 只签路径,换 host 不影响校验)。
	link := absolute(obj.RawURL, d.PublicBase)
	internal := ""
	switch {
	case d.InternalBase != "" && d.PublicBase != "":
		link = swapBase(link, d.InternalBase, d.PublicBase)
		// 转存由服务端自取:公网域名经 Cloudflare 隧道有 100MB 请求体限制且
		// 不可回源,拉取会 TLS 超时;内网直连才是这台 OpenList 的可靠出口。
		internal = swapBase(link, d.PublicBase, d.InternalBase)
	case d.InternalBase != "":
		// 只配内网基址:raw_url 可能是相对路径,补成内网绝对地址。
		internal = absolute(link, d.InternalBase)
	}
	// fs/get 响应本就带着 name/size:提链路径(根 URL + 深路径 fid)下缓存与
	// 快照都查不到元数据,这里不带走的话 service 层只能落「文件」+0。
	return driver.DirectLink{
		URL:       link,
		ServerURL: internal,
		UA:        BrowserUA,
		ExpiresAt: time.Now().Add(rawLinkTTL),
		Name:      obj.Name,
		Size:      obj.Size,
	}, nil
}

// MaxParallelLinks 声明可并发提链数。fs/get 是无状态只读请求,生产配置
// limit_qps=10 / download_concurrency=8;4 路并发把「分享内 8 个文件自动
// 提链」从 8 个串行往返压成 2 波(实测单次 fs/get 约 2.5-3s),同时给其它
// 请求留足令牌。
func (d *Driver) MaxParallelLinks() int { return 4 }

// CheckCredential 用 /api/me 探测 token 是否有效。
func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	if cred.Cookie == "" {
		return driver.CredStatus{}, driver.NewErr(driver.KindAuthInvalid, "Mango Drive token 为空", nil)
	}
	base := d.pickBase(d.PublicBase)
	if base == "" {
		return driver.CredStatus{}, driver.NewErr(driver.KindUnsupported, "Mango Drive 未配置 API 基址(public_base/internal_base)", nil)
	}
	name, err := NewAPI(d.client, base, cred.Cookie).Me(ctx)
	if err != nil {
		return driver.CredStatus{}, err
	}
	return driver.CredStatus{Valid: true, Nickname: name}, nil
}

// nodeOf 把 OpenList 条目转成 FileNode,FID 里埋存储路径(短路径直接可见)。
func nodeOf(p string, o Obj) driver.FileNode {
	return driver.FileNode{
		FID:   fidFor(p),
		Name:  o.Name,
		Size:  o.Size,
		IsDir: o.IsDir,
		Ext:   map[string]string{"path": p, "api": ""},
	}
}

// join 拼接父子路径。
func join(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// absolute 把 raw_url 补成完整地址:已是绝对地址直接用;否则挂 PublicBase(兜底)。
func absolute(raw, base string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	b := strings.TrimRight(base, "/")
	if b == "" {
		return raw
	}
	return b + raw
}

// swapBase 把 raw 中 oldBase(scheme://host) 换成 newBase,仅当 host 一致时生效。
func swapBase(raw, oldBase, newBase string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	o, oerr := url.Parse(oldBase)
	n, nerr := url.Parse(newBase)
	if oerr != nil || nerr != nil {
		return raw
	}
	if u.IsAbs() && u.Scheme == o.Scheme && u.Host == o.Host {
		u.Scheme = n.Scheme
		u.Host = n.Host
		return u.String()
	}
	return raw
}
