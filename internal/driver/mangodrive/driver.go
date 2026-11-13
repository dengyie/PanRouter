// Package mangodrive 实现私有云盘(Mango Drive / OpenList 后端)driver。

package mangodrive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/url"
	"strings"
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
// listingTruncated 会把它提升为 ResolveShareResult.Truncated),预算耗尽
// 同样收窄返回,绝不 panic 也绝不静默丢整棵。
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
	nodes, truncated := listTree(ctx, api, p)
	if truncated && len(nodes) > 0 {
		nodes[len(nodes)-1].Ext["truncated"] = "1"
	}
	return nodes, nil
}

// 树遍历上限:与 ingest_tree 客户端展开(8 层/200 目录)同量级,服务端略宽
// (节点口径而非目录口径,一个千文件平铺目录也要能整树返回)。
const (
	treeMaxDepth = 8
	treeMaxNodes = 1000
)

// listTree 从根目录 BFS 整棵子树;返回节点(相对名)与是否触限截断。
func listTree(ctx context.Context, api *API, root string) ([]driver.FileNode, bool) {
	type queued struct {
		path string // 存储内绝对路径
		rel  string // 相对 root 的路径("" = 根层)
	}
	nodes := make([]driver.FileNode, 0, 16)
	truncated := false
	queue := []queued{{path: root, rel: ""}}
	seen := map[string]bool{root: true} // 目录去重(防御性:软链/异常上游可能成环)
	for depth := 0; depth < treeMaxDepth && len(queue) > 0; depth++ {
		var next []queued
		for _, q := range queue {
			if len(nodes) >= treeMaxNodes || ctx.Err() != nil {
				return nodes, true
			}
			list, err := api.List(ctx, q.path)
			if err != nil {
				// 单层列表失败:该子目录弃置,其余继续(上游瞬时抖动不毁整树)。
				continue
			}
			for i := range list {
				if len(nodes) >= treeMaxNodes || ctx.Err() != nil {
					return nodes, true
				}
				child := list[i]
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
					next = append(next, queued{path: full, rel: rel})
				}
			}
		}
		queue = next
	}
	if len(queue) > 0 { // 深度触限仍有未下探目录
		truncated = true
	}
	return nodes, truncated
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
