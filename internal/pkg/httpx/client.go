// Package httpx 提供带 SSRF 防护的 HTTP 客户端:
// 全局禁止私网直连,重定向按 per-driver 域名白名单逐跳校验。
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

type Options struct {
	Timeout       time.Duration
	Proxy         string   // http:// 或 socks5://(需 transport 支持);空为直连
	RedirectAllow []string // 重定向目标 host 后缀白名单;空 = 仅允许同 host 回环跳转
	MaxRedirects  int
	AllowPrivate  bool // 仅供测试 fixture 使用
	// NoBodyTimeout: 中转流场景去掉 client 级总超时(http.Client.Timeout 包含读体全程,
	// 会截断长下载);响应头到达仍受 ResponseHeaderTimeout=Timeout 约束。
	NoBodyTimeout bool
}

// Result 是已读完响应体的结果(用于 JSON API 调用)。
type Result struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type Client struct {
	hc   *http.Client
	opts Options
	acw  acwCache // host → acw_sc__v2;O(1) 命中,避免每次 Range 再解一次
}

const maxAPIBody = 8 << 20 // 8MB,防上游异常返回超大响应

func New(opts Options) (*Client, error) {
	if opts.MaxRedirects == 0 {
		opts.MaxRedirects = 5
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: controlFn(opts.AllowPrivate)}
		tr := &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: opts.Timeout,
			// 零值 Proxy 已是直连;显式 nil 防止以后改成 DefaultTransport 时吃到
			// 宿主 HTTP_PROXY(pxed 全局 Privoxy 会把网盘上游拐走)。
			Proxy: nil,
		}
		if opts.Proxy != "" {
			u, err := url.Parse(opts.Proxy)
			if err != nil {
				return nil, fmt.Errorf("bad proxy url: %w", err)
			}
			tr.Proxy = http.ProxyURL(u)
		}
	c := &Client{opts: opts}
	clientTimeout := opts.Timeout
	if opts.NoBodyTimeout {
		clientTimeout = 0
	}
	c.hc = &http.Client{
		Transport:     tr,
		Timeout:       clientTimeout,
		CheckRedirect: c.checkRedirect,
	}
	return c, nil
}

func controlFn(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(_ string, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil
		}
		if isPrivate(ip) {
			return fmt.Errorf("httpx: blocked private address %s", address)
		}
		return nil
	}
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= c.opts.MaxRedirects {
		return fmt.Errorf("httpx: too many redirects (>=%d)", c.opts.MaxRedirects)
	}
	if len(via) == 0 {
		return nil
	}
	dstHost := req.URL.Hostname()
	if dstHost == via[0].URL.Hostname() { // 同 host 跳转放行
		return c.privateGuard(req, dstHost)
	}
	if !SuffixMatch(dstHost, c.opts.RedirectAllow) {
		return fmt.Errorf("httpx: redirect to %q blocked by allowlist", req.URL.Host)
	}
	// 跨 host 跳转时 Go 会剥离 Cookie;把已解的 acw_sc__v2 贴到目标 host,避免每次 302 后再吃一轮挑战。
	if token := c.acw.get(req.URL.Host); token != "" {
		setCookieKV(req, acwCookieName, token)
	}
	return c.privateGuard(req, dstHost)
}

func (c *Client) privateGuard(req *http.Request, host string) error {
	if c.opts.AllowPrivate {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(req.Context(), host)
	if err != nil {
		return fmt.Errorf("httpx: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivate(ip.IP) {
			return fmt.Errorf("httpx: %s resolves to private address", host)
		}
	}
	return nil
}

// SuffixMatch 判断 host 是否命中后缀白名单(大小写不敏感)。
func SuffixMatch(host string, allow []string) bool {
	host = strings.ToLower(host)
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		a = strings.TrimPrefix(a, ".")
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// Do 执行请求并读完全部响应体(API 调用用)。
func (c *Client) Do(req *http.Request) (*Result, error) {
	resp, err := c.doWithACW(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &Result{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// DoStream 执行请求并保留响应体(中转流用)。遇 acw_sc__v2 挑战页自动解题重放(设计文档 §10 S2)。
func (c *Client) DoStream(req *http.Request) (*http.Response, error) { return c.doWithACW(req) }

func (c *Client) doWithACW(req *http.Request) (*http.Response, error) {
	cur := req
	origCtx := req.Context()
	for attempt := 0; attempt <= maxACWReplay; attempt++ {
		if token := c.acw.get(cur.URL.Host); token != "" {
			cloned, err := cloneRequest(cur, origCtx)
			if err != nil {
				return nil, err
			}
			cur = cloned
			setCookieKV(cur, acwCookieName, token)
		}
		resp, err := c.hc.Do(cur)
		if err != nil {
			return nil, err
		}
		page, challenge, err := inspectChallenge(resp)
		if err != nil {
			return nil, err
		}
		if !challenge {
			return resp, nil
		}
		if attempt == maxACWReplay {
			return nil, fmt.Errorf("httpx: acw_sc__v2 challenge persisted after retry")
		}
		token, err := SolveACW(page)
		if err != nil {
			return nil, fmt.Errorf("httpx: solve acw_sc__v2: %w", err)
		}
		// 重放必须沿用调用方 context:关闭挑战页 Body 会取消 resp.Request.Context()。
		final := cur.URL
		if resp.Request != nil && resp.Request.URL != nil {
			final = resp.Request.URL
		}
		c.acw.set(final.Host, token)
		replay, err := cloneRequest(cur, origCtx)
		if err != nil {
			return nil, err
		}
		u := *final
		replay.URL = &u
		replay.Host = u.Host
		setCookieKV(replay, acwCookieName, token)
		cur = replay
	}
	return nil, fmt.Errorf("httpx: acw_sc__v2 exhausted")
}

func cloneRequest(r *http.Request, ctx context.Context) (*http.Request, error) {
	nr := r.Clone(ctx)
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		nr.Body = body
	}
	return nr, nil
}

// DoJSON 发送 JSON 请求并反序列化响应。
func (c *Client) DoJSON(ctx context.Context, method, url string, headers map[string]string, body, out any) (*Result, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if out != nil && len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return res, fmt.Errorf("decode response: %w (body head: %.200s)", err, string(res.Body))
		}
	}
	return res, nil
}
