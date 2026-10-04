// Package lanzou 实现蓝奏云 driver(免登录,分享页解析)。
// 蓝奏云无开放 API,依赖分享页 HTML/JS 结构,结构变化时只需调整本包。
package lanzou

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

const BrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

type Driver struct {
	client *httpx.Client
}

func New(client *httpx.Client) *Driver { return &Driver{client: client} }

func (d *Driver) ID() string { return "lanzou" }

var (
	iframeRe = regexp.MustCompile(`(?i)<iframe[^>]+src=["']([^"']*(?:fn\?)[^"']*)["']`)
	// 词边界硬化:避免误配页面里的 wp_sign 等同名变量(RE2 无 lookbehind,用前置字符排除)
	signRe     = regexp.MustCompile(`(?i)(?:^|[^A-Za-z_])sign\s*=\s*['"]([^'"]{10,})['"]`)
	isngisRe   = regexp.MustCompile(`(?i)(?:^|[^A-Za-z_])isngis\s*=\s*['"]([^'"]{10,})['"]`)
	ajaxFileRe = regexp.MustCompile(`(?i)url\s*:\s*['"](https?://[^'"]*ajaxfile\.php\?file=\d+)['"]`)
	titleRe    = regexp.MustCompile(`(?i)<title>([^<]+)</title>`)
	sizeRe     = regexp.MustCompile(`大小[：:]\s*([\d.]+)\s*([KMG]?B)`)
)

// isPasswordPage 识别蓝奏云密码门页面(带密码分享的入口页)。
func isPasswordPage(body string) bool {
	return containsAny(body, "passwddiv", `id="password"`, `name="pwd"`)
}

func goneOrPassword(body string) error {
	if containsAny(body, "文件取消分享了", "文件不存在", "来晚一步") {
		return driver.NewErr(driver.KindShareGone, "蓝奏云分享已失效", nil)
	}
	if isPasswordPage(body) {
		return driver.NewErr(driver.KindUnsupported, "该蓝奏云链接带密码,请在解析时提供提取码", nil)
	}
	return nil
}

func containsAny(s string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func (d *Driver) get(ctx context.Context, rawURL, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "构造请求失败", err)
	}
	req.Header.Set("User-Agent", BrowserUA)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	res, err := d.client.Do(req)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "蓝奏云网络请求失败", err)
	}
	if res.StatusCode >= 400 {
		return "", driver.NewErr(driver.KindUpstream, fmt.Sprintf("蓝奏云响应异常:%d", res.StatusCode), nil)
	}
	return string(res.Body), nil
}

// resolvePage 执行 分享页 →(iframe)→ sign → ajaxm.php 的完整解析链;
// 带密码分享则走 密码页 → isngis+fileid → ajaxfile.php 路径(提交码由调用方传入)。
func (d *Driver) resolvePage(ctx context.Context, shareURL, pwd string) (finalURL, fileName string, fileSize int64, err error) {
	page, err := d.get(ctx, shareURL, "")
	if err != nil {
		return "", "", 0, err
	}
	if containsAny(page, "文件取消分享了", "文件不存在", "来晚一步") {
		return "", "", 0, driver.NewErr(driver.KindShareGone, "蓝奏云分享已失效", nil)
	}
	if isPasswordPage(page) {
		if pwd == "" {
			return "", "", 0, driver.NewErr(driver.KindUnsupported, "该蓝奏云链接带密码,请在解析时提供提取码", nil)
		}
		return d.resolvePasswordPage(ctx, page, shareURL, pwd)
	}
	sign, pageRef := extractSign(page, shareURL)
	if sign == "" {
		m := iframeRe.FindStringSubmatch(page)
		if len(m) < 2 {
			return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云页面结构变化:未找到 sign/iframe", nil)
		}
		iframeURL := absolutize(m[1], shareURL)
		body2, err := d.get(ctx, iframeURL, shareURL)
		if err != nil {
			return "", "", 0, err
		}
		if e := goneOrPassword(body2); e != nil {
			return "", "", 0, e
		}
		sign, pageRef = extractSign(body2, iframeURL)
	}
	if sign == "" {
		return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云页面结构变化:未提取到 sign", nil)
	}
	finalURL, err = d.ajaxm(ctx, shareURL, pageRef, sign)
	if err != nil {
		return "", "", 0, err
	}
	fileName = "蓝奏云文件"
	if m := titleRe.FindStringSubmatch(page); len(m) > 1 {
		fileName = strings.TrimSpace(m[1])
	}
	fileSize = parseSize(page)
	return finalURL, fileName, fileSize, nil
}

func extractSign(body, fallbackRef string) (sign, ref string) {
	if m := signRe.FindStringSubmatch(body); len(m) > 1 {
		return m[1], fallbackRef
	}
	return "", fallbackRef
}

// resolvePasswordPage 走 密码页 → 提取 isngis+fileid → ajaxfile.php 提交提取码 的链路。
// 页面内嵌脚本形如: url : 'https://apifile.woozooo.com/ajaxfile.php?file=321571261'
func (d *Driver) resolvePasswordPage(ctx context.Context, page, shareURL, pwd string) (finalURL, fileName string, fileSize int64, err error) {
	isngis := ""
	if m := isngisRe.FindStringSubmatch(page); len(m) > 1 {
		isngis = m[1]
	}
	if isngis == "" {
		return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云密码页缺少 isngis 令牌", nil)
	}
	ajaxURL := ""
	if m := ajaxFileRe.FindStringSubmatch(page); len(m) > 1 {
		ajaxURL = m[1]
	}
	if ajaxURL == "" {
		return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云密码页缺少 ajaxfile 端点", nil)
	}
	form := url.Values{"action": {"downprocess"}, "sign": {isngis}, "kd": {"1"}, "p": {pwd}}
	req, err := http.NewRequestWithContext(ctx, "POST", ajaxURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", 0, driver.NewErr(driver.KindUpstream, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", BrowserUA)
	req.Header.Set("Referer", shareURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	res, err := d.client.Do(req)
	if err != nil {
		return "", "", 0, driver.NewErr(driver.KindUpstream, "蓝奏云网络请求失败", err)
	}
	var aj struct {
		ZT  int    `json:"zt"`
		Dom string `json:"dom"`
		URL string `json:"url"`
		Inf string `json:"inf"`
	}
	if err := json.Unmarshal(res.Body, &aj); err != nil {
		return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云 ajaxfile 响应结构变化", err)
	}
	if aj.ZT != 1 || aj.Dom == "" || aj.URL == "" {
		if containsAny(aj.Inf, "无法识别", "密码") {
			return "", "", 0, driver.NewErr(driver.KindNotFound, "提取码错误或文件无法识别", nil)
		}
		if containsAny(aj.Inf, "频繁", "稍后") {
			return "", "", 0, driver.NewErr(driver.KindRiskControl, "蓝奏云触发限流,请稍后重试", nil)
		}
		return "", "", 0, driver.NewErr(driver.KindInterfaceChanged, "蓝奏云密码流程失败:"+aj.Inf, nil)
	}
	name := aj.Inf
	if name == "" {
		name = "蓝奏云文件"
	}
	return joinFinalURL(aj.Dom, aj.URL), name, 0, nil
}

func (d *Driver) ajaxm(ctx context.Context, shareURL, pageRef, sign string) (string, error) {
	u, err := url.Parse(shareURL)
	if err != nil {
		return "", driver.NewErr(driver.KindNotFound, "无效的蓝奏云链接", err)
	}
	ajaxURL := u.Scheme + "://" + u.Host + "/ajaxm.php"
	form := url.Values{"action": {"downprocess"}, "signs": {sign}, "ves": {"1"}}
	req, err := http.NewRequestWithContext(ctx, "POST", ajaxURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", BrowserUA)
	req.Header.Set("Referer", pageRef)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	res, err := d.client.Do(req)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "蓝奏云网络请求失败", err)
	}
	var aj struct {
		ZT  int    `json:"zt"`
		Dom string `json:"dom"`
		URL string `json:"url"`
		Inf string `json:"inf"`
	}
	if err := json.Unmarshal(res.Body, &aj); err != nil {
		return "", driver.NewErr(driver.KindInterfaceChanged, "蓝奏云 ajaxm 响应结构变化", err)
	}
	if aj.ZT != 1 || aj.Dom == "" || aj.URL == "" {
		if containsAny(strings.ToLower(aj.Inf), "频繁", "稍后") {
			return "", driver.NewErr(driver.KindRiskControl, "蓝奏云触发限流,请稍后重试", nil)
		}
		return "", driver.NewErr(driver.KindInterfaceChanged, "蓝奏云解析失败:"+aj.Inf, nil)
	}
	return joinFinalURL(aj.Dom, aj.URL), nil
}

// joinFinalURL 拼接最终直链:不同接口的 url 字段可能带或不带 /file/ 前缀,避免重复拼接。
func joinFinalURL(dom, u string) string {
	dom = strings.TrimRight(dom, "/")
	if strings.Contains(u, "/file/") {
		return dom + u
	}
	return dom + "/file/" + u
}

func absolutize(href, base string) string {
	h := strings.TrimSpace(href)
	if strings.HasPrefix(h, "//") {
		return "https:" + h
	}
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		return h
	}
	b, err := url.Parse(base)
	if err != nil {
		return h
	}
	r, err := b.Parse(h)
	if err != nil {
		return h
	}
	return r.String()
}

func parseSize(page string) int64 {
	m := sizeRe.FindStringSubmatch(page)
	if len(m) < 3 {
		return 0
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	mult := map[string]int64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30}
	return int64(f * float64(mult[strings.ToUpper(m[2])]))
}

// ---- Driver 接口 ----

func (d *Driver) ResolveShare(ctx context.Context, share driver.ShareLink, cred *driver.Credential) ([]driver.FileNode, error) {
	if cred != nil {
		_ = cred // 免登录,忽略
	}
	u, err := url.Parse(share.URL)
	if err != nil || u.Host == "" {
		return nil, driver.NewErr(driver.KindNotFound, "无效的蓝奏云链接", err)
	}
	if strings.Contains(u.Path, "/s/") || strings.Contains(u.Path, "/s.html") {
		return nil, driver.NewErr(driver.KindUnsupported, "暂不支持蓝奏云文件夹分享(M1)", nil)
	}
	final, name, size, err := d.resolvePage(ctx, share.URL, share.Pwd)
	if err != nil {
		return nil, err
	}
	_ = final // 列表场景不需要直链;GetDirectLink 会用 Ext.share_url 重新解析取新链
	return []driver.FileNode{{
		FID: "file", Name: name, Size: size, IsDir: false,
		Ext: map[string]string{"share_url": share.URL, "pwd": share.Pwd},
	}}, nil
}

func (d *Driver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if ref.Own {
		return driver.DirectLink{}, driver.NewErr(driver.KindUnsupported, "蓝奏云暂不支持网盘内文件提链(M1)", nil)
	}
	shareURL := ref.Ext["share_url"]
	if shareURL == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "缺少蓝奏云分享上下文,请重新解析", nil)
	}
	final, _, _, err := d.resolvePage(ctx, shareURL, ref.Ext["pwd"])
	if err != nil {
		return driver.DirectLink{}, err
	}
	// 浏览器 302 由浏览器自身过 CDN 挑战;非浏览器(/stream、aria2)由 httpx acw_sc__v2 中间件解题重放。
	return driver.DirectLink{URL: final, ExpiresAt: time.Now().Add(30 * time.Minute)}, nil
}

func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true, Nickname: "lanzou", Message: "免登录盘,无需凭据"}, nil
}
