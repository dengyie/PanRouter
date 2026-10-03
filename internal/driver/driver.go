// Package driver 是所有网盘差异的防腐层。
// service/api 层只依赖本文件的契约;网盘接口改版只应修改具体 driver 包。
package driver

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- 错误分类(契约,不允许 driver 私自定义) ----

type Kind string

const (
	KindAuthExpired      Kind = "auth_expired"      // 凭据失效
	KindRiskControl      Kind = "risk_control"      // 风控/限流/验证码
	KindShareGone        Kind = "share_gone"        // 分享失效/被取消
	KindNotFound         Kind = "not_found"         // 链接/文件不存在
	KindInterfaceChanged Kind = "interface_changed" // 返回结构解析失败(接口改版)
	KindUpstream         Kind = "upstream_error"    // 上游 5xx/网络错误
	KindUnsupported      Kind = "unsupported"       // 功能暂不支持(如带密码蓝奏云)
)

// Error 是 driver 返回的唯一错误类型,Kind 驱动 service 层的重试/熔断/提示决策。
type Error struct {
	Kind      Kind
	Retriable bool
	UserHint  string
	Err       error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.UserHint, e.Err)
	}
	return e.UserHint
}
func (e *Error) Unwrap() error { return e.Err }

// NewErr 构造分类错误;默认 RiskControl / Upstream 可重试,其余不可。
func NewErr(kind Kind, hint string, err error) *Error {
	return &Error{Kind: kind, Retriable: kind == KindRiskControl || kind == KindUpstream, UserHint: hint, Err: err}
}

// ---- 数据结构 ----

type ShareLink struct {
	URL string
	Pwd string // 提取码,可为空
}

type FileNode struct {
	FID   string
	Name  string
	Size  int64
	IsDir bool
	Ext   map[string]string // driver 私有上下文(写入 links.ext)
}

// FileRef 指向一次直链获取请求的目标。
type FileRef struct {
	Own      bool // true: 网盘内自己的文件;false: 分享内文件
	FID      string
	ShareKey string
	Ext      map[string]string // 来自 FileNode.Ext 的上下文
}

type Credential struct {
	Cookie string
	Extra  map[string]string
}

type CredStatus struct {
	Valid    bool
	Nickname string
	Message  string
}

// DirectLink 描述直链及其校验约束,是下载路径决策(§4.3)的唯一输入。
type DirectLink struct {
	URL       string
	UA        string // 直链要求的 UA;空 = 不校验
	Referer   string // 直链要求的 Referer;空 = 不校验
	Cookie    string // 直链需携带的 Cookie;非空则浏览器 302 不可用
	BindIP    bool   // 直链绑定解析出口 IP(cloud 画像下浏览器 302 不可用)
	ExpiresAt time.Time
}

// ---- 接口 ----

type Driver interface {
	ID() string
	// ResolveShare 解析分享链接,返回文件树(免登录盘 cred 可为 nil)。
	ResolveShare(ctx context.Context, share ShareLink, cred *Credential) ([]FileNode, error)
	// GetDirectLink 获取单个文件的直链。
	GetDirectLink(ctx context.Context, cred *Credential, ref FileRef) (DirectLink, error)
	// CheckCredential 供凭据看门狗定时调用。
	CheckCredential(ctx context.Context, cred Credential) (CredStatus, error)
}

// ---- 注册表与域名路由 ----

type route struct {
	suffix   string
	driverID string
}

type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
	routes  []route // 按 suffix 长度降序,最长匹配优先;可经 UpdateRoutes 热更新
}

func NewRegistry(drivers []Driver, routes map[string][]string) *Registry {
	r := &Registry{drivers: map[string]Driver{}}
	for _, d := range drivers {
		r.drivers[d.ID()] = d
	}
	r.routes = buildRoutes(routes, r.drivers)
	return r
}

// UpdateRoutes 热更新域名路由表(配置热加载时调用);只保留已启用 driver 的路由。
func (r *Registry) UpdateRoutes(routes map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = buildRoutes(routes, r.drivers)
}

func buildRoutes(routes map[string][]string, drivers map[string]Driver) []route {
	var out []route
	for driverID, suffixes := range routes {
		if _, ok := drivers[driverID]; !ok {
			continue // driver 未启用,路由跳过
		}
		for _, s := range suffixes {
			s = strings.ToLower(strings.TrimSpace(s))
			if s != "" {
				out = append(out, route{suffix: s, driverID: driverID})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].suffix) > len(out[j].suffix) })
	return out
}

func (r *Registry) Get(id string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[id]
	return d, ok
}

func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.drivers))
	for id := range r.drivers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Detect 按域名后缀最长匹配识别分享链接所属网盘。
func (r *Registry) Detect(rawURL string) (Driver, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return nil, NewErr(KindNotFound, "无法识别的链接", err)
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimSuffix(host, ".")
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.routes {
		if host == rt.suffix || strings.HasSuffix(host, "."+rt.suffix) {
			return r.drivers[rt.driverID], nil
		}
	}
	return nil, NewErr(KindNotFound, fmt.Sprintf("暂不支持该网盘域名:%s", host), nil)
}
