// Package baidu 实现百度网盘 driver。
//
// 接口形态来自两处实证,不用推测:
//   - 本仓姊妹项目 YunX-Desktop 的 BaiduApi(生产在用的完整取链链);
//   - 上游开源实现的公开调用方式(qjfoidnh/BaiduPCS-Go、AlistGo/alist baidu_share)。
//
// 与夸克同型:Cookie 登录态 + 分享链接 + 风控限速。差异在于百度必须经「转存到
// 自己网盘」才能取直链(转发链),且 bdstoken 随会话漂移需要现取现用。
package baidu

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

const (
	baseURL = "https://pan.baidu.com"
	pcsURL  = "https://d.pcs.baidu.com"

	// webUA 用于分享页/网盘内部 API(share/verify、xpan/share、filemetas 等)。
	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	// NetdiskUA 用于客户端协议端点(yun/api/list、filemanager、locatedownload);
	// 直链下载同样要求该 UA,由 DirectLink.UA 下发。
	NetdiskUA = "netdisk;12.24.6;piano;android-android;16;JSbridge4.4.0;jointBridge;1.1.0"

	appID   = "250528"
	referer = "https://pan.baidu.com/"

	// tmpDirName 是转存专用暂存目录(网盘根目录下);清理只删该目录内的副本,
	// 避免误删用户自己的文件。
	tmpDirName = "panrouter_tmp"
	// linkTTL 保守取值:locatedownload/filemetas 的直链其实按小时计,但我们按
	// 45 分钟缓存,过期由 Resolver 走 singleflight 续签。
	linkTTL = 45 * time.Minute

	// 分享目录遍历上界:避免异常响应或超大分享把解析拖死。
	maxShareDepth = 8
	maxShareFiles = 200
	maxShareDirs  = 64
	// maxListPages 单目录翻页上界(num=100 → 单目录 2000 条)。
	maxListPages = 20

	// DailyTransferLimit 是每个账号每日转存次数硬闸(pilot 期防风控)。
	// 百度对高频转存的风控是账号级甚至 IP 级的,超限直接拒绝而不是硬闯。
	DailyTransferLimit = 50
)

// listPageSize 百度分享列目录单页上限(xpan/share num 最大 100)。
const listPageSize = 100

// 分享链接里的 surl:新格式已从 base62 迁到 URL-safe base64,字母表含 `_`/`-`,
// 旧实现只认 [0-9a-zA-Z] 会把新链接判成"STOKEN 缺失"(qjfoidnh/BaiduPCS-Go#534)。
var (
	surlPathRe  = regexp.MustCompile(`/s/([0-9A-Za-z_-]{5,40})`)
	surlQueryRe = regexp.MustCompile(`surl=([0-9A-Za-z_-]{5,40})`)
	pwdRe       = regexp.MustCompile(`(?:pwd|password)=([0-9A-Za-z]{4})`)
)

// errBdstoken 是 bdstoken 失效的哨兵错误:获取 bdstoken 的调用点据此刷新重试,
// 不依赖上游文案(百度常把这类错误写成风控话术)。
var errBdstoken = errors.New("baidu bdstoken invalid")

type Driver struct {
	client *httpx.Client
	base   string // pan.baidu.com;测试注入 httptest 地址
	pcs    string // d.pcs.baidu.com;测试注入 httptest 地址

	tmpMu   sync.Mutex
	tmpDirs map[string]string // 登录态指纹 → 暂存目录路径

	tokenMu  sync.Mutex
	bdstoken map[string]string // 登录态指纹 → bdstoken

	quotaMu  sync.Mutex
	quotaDay map[string]string // 登录态指纹 → 记数日("2006-01-02")
	quotaN   map[string]int    // 登录态指纹 → 当日已转存次数
}

func New(client *httpx.Client, base string) *Driver {
	if base == "" {
		base = baseURL
	}
	return &Driver{
		client:   client,
		base:     strings.TrimRight(base, "/"),
		pcs:      pcsURL,
		tmpDirs:  map[string]string{},
		bdstoken: map[string]string{},
		quotaDay: map[string]string{},
		quotaN:   map[string]int{},
	}
}

// WithPCSBase 注入 PCS(locatedownload)基址,仅测试使用。
func (d *Driver) WithPCSBase(pcs string) *Driver {
	if pcs != "" {
		d.pcs = strings.TrimRight(pcs, "/")
	}
	return d
}

func (d *Driver) ID() string { return "baidu" }

// ---- 错误分类 ----

// classifyErrno 把百度 errno + errmsg 映射到契约错误类别。
// errno 是百度的机器判据(0 = 成功),文案只作兜底——同一 errno 的 errmsg 会变。
func classifyErrno(errno int, msg string) *driver.Error {
	switch errno {
	case 0:
		return nil
	case -6, -20:
		// -6: 身份验证失败/未登录; -20: cookies 失效
		return driver.NewErr(driver.KindAuthExpired, "百度 Cookie 已失效,请到账号页更新", nil)
	case -9:
		return driver.NewErr(driver.KindNotFound, "百度文件不存在或已删除", nil)
	case 2:
		// 参数错误:分享场景下多为「缺少 BDCLND / 链接无效」。
		if strings.Contains(msg, "提取码") || strings.Contains(msg, "密码") {
			return driver.NewErr(driver.KindNotFound, "提取码错误或分享需要提取码", nil)
		}
		return driver.NewErr(driver.KindUpstream, "百度接口返回参数错误(errno=2):"+msg, nil)
	case 5, 12, 118:
		// 分享已取消/已失效/违规
		return driver.NewErr(driver.KindShareGone, "分享已失效:"+msg, nil)
	case 105:
		// bdstoken 失效:调用方刷新后重试。
		return driver.NewErr(driver.KindUpstream, "百度 bdstoken 已失效,请重试", errBdstoken)
	case 110, 9019:
		// 110: 分享不存在/已删除; 9019: 需要安全验证
		if errno == 9019 {
			return driver.NewErr(driver.KindRiskControl, "百度要求安全验证,请稍后重试:"+msg, nil)
		}
		return driver.NewErr(driver.KindShareGone, "分享不存在或已被删除:"+msg, nil)
	case 111:
		// 空间不足 / 文件数超限
		return driver.NewErr(driver.KindUpstream, "百度网盘空间不足或转存受限:"+msg, nil)
	case 112, 113, 115, 190:
		return driver.NewErr(driver.KindShareGone, "分享已失效:"+msg, nil)
	case 31066:
		return driver.NewErr(driver.KindRiskControl, "百度转存文件数超限:"+msg, nil)
	case 31061:
		// 同名文件:转存用 ondup=newcopy 规避,真出现按冲突处理。
		return driver.NewErr(driver.KindConflict, "百度网盘已存在同名文件:"+msg, nil)
	case 31064:
		return driver.NewErr(driver.KindUpstream, "百度转存失败:"+msg, nil)
	case 31024, 31034:
		return driver.NewErr(driver.KindUpstream, "百度网盘空间不足:"+msg, nil)
	}
	// 文案兜底(未知 errno 时不丢信息,按可重试上游错误处理最稳)。
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "验证", "频繁", "稍后", "安全"):
		return driver.NewErr(driver.KindRiskControl, "百度触发风控:"+msg, nil)
	case containsAny(m, "登录", "login", "鉴权", "未授权"):
		return driver.NewErr(driver.KindAuthExpired, "百度 Cookie 已失效,请到账号页更新", nil)
	case containsAny(m, "提取码", "密码"):
		return driver.NewErr(driver.KindNotFound, "提取码错误:"+msg, nil)
	case containsAny(m, "取消", "失效", "删除", "不存在", "违规", "封禁"):
		return driver.NewErr(driver.KindShareGone, "分享已失效:"+msg, nil)
	case containsAny(m, "空间", "容量", "超限", "限制"):
		return driver.NewErr(driver.KindUpstream, "百度网盘受限:"+msg, nil)
	default:
		return driver.NewErr(driver.KindUpstream, fmt.Sprintf("百度接口异常(errno=%d):%s", errno, msg), nil)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func containsAny(s string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// ---- 内部请求 ----

type apiResp struct {
	Errno  int    `json:"errno"`
	ErrMsg string `json:"errmsg"`
}

// call 发送请求并解码响应到 out(out 需自带 errno 字段),返回原始响应。
// form 非空时以 application/x-www-form-urlencoded 提交。
func (d *Driver) call(ctx context.Context, method, rawURL string, form url.Values, headers map[string]string, out any) (*httpx.Result, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "构造百度请求失败", err)
	}
	req.Header.Set("User-Agent", webUA)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "百度网络请求失败", err)
	}
	if out != nil && len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return res, driver.NewErr(driver.KindInterfaceChanged, "百度响应不是合法 JSON,疑似接口改版", err)
		}
	}
	return res, nil
}

// mustOK 校验 errno。★ errno==0 必须显式返回 nil:classifyErrno 返回的是
// *driver.Error,直接 `return classifyErrno(...)` 会把 nil 指针装进 error 接口,
// 得到「非 nil 的 nil 错误」——调用方 err != nil 判真,成功被当失败。
func mustOK(errno int, msg string) error {
	if errno == 0 {
		return nil
	}
	return classifyErrno(errno, msg)
}

// cookieHeader 组装请求 Cookie:Cookie 主体 + 可选的 BDCLND(分享验证标识,
// 值就是 verify 返回的 randsk)。Cookie 里已有 BNCLND 则不重复添加。
func cookieHeader(cookie, sekey string) string {
	if sekey == "" || strings.Contains(cookie, "BDCLND=") {
		return cookie
	}
	if cookie == "" {
		return "BDCLND=" + sekey
	}
	return cookie + "; BDCLND=" + sekey
}

func fingerprint(cookie string) string {
	h := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(h[:])[:16]
}

// ---- bdstoken ----

// getBdstoken 取 bdstoken(bdstoken 随会话漂移,缓存到进程内按账号区分)。
// force=true 时忽略缓存重取(errno=105/-6 后的重试路径)。
func (d *Driver) getBdstoken(ctx context.Context, cred *driver.Credential, force bool) (string, error) {
	if cred == nil || cred.Cookie == "" {
		return "", driver.NewErr(driver.KindAuthExpired, "百度需要登录态,请先在「账号管理」添加百度账号 Cookie", nil)
	}
	key := fingerprint(cred.Cookie)
	if !force {
		d.tokenMu.Lock()
		tok := d.bdstoken[key]
		d.tokenMu.Unlock()
		if tok != "" {
			return tok, nil
		}
	}
	u := d.base + "/api/gettemplatevariable?clienttype=0&app_id=" + appID + "&web=1&fields=" +
		url.QueryEscape(`["bdstoken"]`)
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Result struct {
			Bdstoken string `json:"bdstoken"`
		} `json:"result"`
	}
	if _, err := d.call(ctx, "GET", u, nil, map[string]string{"cookie": cred.Cookie, "referer": referer}, &out); err != nil {
		return "", err
	}
	if err := mustOK(out.Errno, out.ErrMsg); err != nil {
		return "", err
	}
	if out.Result.Bdstoken == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "百度 bdstoken 响应缺少字段", nil)
	}
	d.tokenMu.Lock()
	d.bdstoken[key] = out.Result.Bdstoken
	d.tokenMu.Unlock()
	return out.Result.Bdstoken, nil
}

// forgetBdstoken 丢弃缓存(errno=105/-6 后强制重取)。
func (d *Driver) forgetBdstoken(cookie string) {
	d.tokenMu.Lock()
	delete(d.bdstoken, fingerprint(cookie))
	d.tokenMu.Unlock()
}

// ---- 转存日配额硬闸 ----

// takeTransferQuota 消耗一次转存配额;超限返回风控类错误(pilot 期保护账号)。
func (d *Driver) takeTransferQuota(cookie string) error {
	key := fingerprint(cookie)
	day := time.Now().Format("2006-01-02")
	d.quotaMu.Lock()
	defer d.quotaMu.Unlock()
	if d.quotaDay[key] != day {
		d.quotaDay[key] = day
		d.quotaN[key] = 0
	}
	if d.quotaN[key] >= DailyTransferLimit {
		return driver.NewErr(driver.KindRiskControl,
			fmt.Sprintf("百度账号今日转存已达上限(%d 次/天),请明日再试或改用其它账号", DailyTransferLimit), nil)
	}
	d.quotaN[key]++
	return nil
}

// ---- Credential ----

func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	if cred.Cookie == "" {
		return driver.CredStatus{Valid: false, Message: "Cookie 为空"}, nil
	}
	u := d.base + "/api/gettemplatevariable?clienttype=0&app_id=" + appID + "&web=1&fields=" +
		url.QueryEscape(`["username","bdstoken"]`)
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Result struct {
			Username string `json:"username"`
			Bdstoken string `json:"bdstoken"`
		} `json:"result"`
	}
	if _, err := d.call(ctx, "GET", u, nil, map[string]string{"cookie": cred.Cookie, "referer": referer}, &out); err != nil {
		return driver.CredStatus{}, err
	}
	// -6/-20 说明 Cookie 失效;其它 errno 也要区分,不能把有效 Cookie 静默记为 expired。
	if out.Errno != 0 {
		if de := classifyErrno(out.Errno, out.ErrMsg); de != nil {
			if de.Kind == driver.KindAuthExpired {
				return driver.CredStatus{Valid: false, Message: "Cookie 已失效"}, nil
			}
			return driver.CredStatus{}, de
		}
	}
	if out.Result.Bdstoken == "" && out.Result.Username == "" {
		return driver.CredStatus{}, driver.NewErr(driver.KindInterfaceChanged, "百度账号信息响应缺少字段,疑似接口改版", nil)
	}
	// 复用本次结果,省一次 gettemplatevariable。
	if out.Result.Bdstoken != "" {
		d.tokenMu.Lock()
		d.bdstoken[fingerprint(cred.Cookie)] = out.Result.Bdstoken
		d.tokenMu.Unlock()
	}
	return driver.CredStatus{Valid: true, Nickname: out.Result.Username, Message: "ok"}, nil
}
