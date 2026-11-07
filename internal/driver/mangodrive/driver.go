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
	maxListPages = 20
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

// fidFor 生成稳定 FID:短路径直接用路径(便于人工排查),超长路径取哈希。
func fidFor(p string) string {
	if len(p) <= 100 {
		return p
	}
	h := sha1.Sum([]byte(p))
	return "p:" + hex.EncodeToString(h[:])
}

// ResolveShare 解析一个存储路径(目录 → 列表,文件 → 单元素)。
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
	list, err := api.List(ctx, p)
	if err != nil {
		return nil, err
	}
	nodes := make([]driver.FileNode, 0, len(list))
	for i := range list {
		nodes = append(nodes, nodeOf(join(p, list[i].Name), list[i]))
	}
	return nodes, nil
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
	api := NewAPI(d.client, d.pickBase(d.PublicBase), cred.Cookie)
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
	return driver.DirectLink{
		URL:       absolute(obj.RawURL, d.PublicBase),
		UA:        BrowserUA,
		ExpiresAt: time.Now().Add(rawLinkTTL),
	}, nil
}

// CheckCredential 用 /api/me 探测 token 是否有效。
func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	if cred.Cookie == "" {
		return driver.CredStatus{}, driver.NewErr(driver.KindAuthInvalid, "Mango Drive token 为空", nil)
	}
	name, err := NewAPI(d.client, d.pickBase(d.PublicBase), cred.Cookie).Me(ctx)
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
