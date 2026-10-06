// Package quark 实现夸克网盘 driver。
// 接口依据 quark-auto-save 等开源实现的公开调用方式,风控形态变化时只需调整本包。
package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// dbg 临时调试:QUARK_DEBUG=1 时打印转存链每步的 fid/响应。
func dbg(format string, args ...any) {
	if os.Getenv("QUARK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[quark-dbg] "+format+"\n", args...)
	}
}

const (
	baseURL    = "https://drive-pc.quark.cn"
	infoURL    = "https://pan.quark.cn/account/info"
	referer    = "https://pan.quark.cn/"
	QuarkUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"
	linkTTL    = 2 * time.Hour
	tmpDirName = "panrouter_tmp" // 转存专用暂存目录;cleanup 只删该目录内副本,避免去重 fid 误删用户文件
	// 分享目录遍历上界:避免异常响应或超大分享把解析拖死。
	maxShareDepth = 8
	maxShareFiles = 200
	maxShareDirs  = 64
)

var pwdIDRe = regexp.MustCompile(`/s/([0-9a-zA-Z]+)`)

// errStokenExpired 是 stoken 过期的哨兵错误:classify 用它标注类别,
// stokenExpired 通过 errors.Is 识别,避免用人类可读文案当机器判据。
var errStokenExpired = errors.New("quark stoken expired")

type Driver struct {
	client       *httpx.Client
	base         string        // token/detail/save/task/file 全链路(drive-pc);测试注入 httptest 地址
	taskInterval time.Duration // 任务轮询间隔(测试可缩短)
	tmpMu        sync.Mutex
	tmpDirs      map[string]string // 登录态指纹 → 转存暂存目录 fid
	QRUopDomain  string            // 扫码登录 CAS 域(测试注入 httptest;空=官方)
	QRInfoURL    string            // 扫码登录 ticket 换 Cookie 端点(同上)
}

func New(client *httpx.Client, base string) *Driver {
	if base == "" {
		base = baseURL
	}
	return &Driver{client: client, base: base, taskInterval: time.Second, tmpDirs: map[string]string{}}
}

func (d *Driver) ID() string { return "quark" }

// ---- API 响应结构 ----

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type quarkFile struct {
	FID       string `json:"fid"`
	FileName  string `json:"file_name"`
	ShareName string `json:"share_name"`
	Size      int64  `json:"size"`
	Dir       bool   `json:"dir"`
}

// ---- 错误分类 ----

func containsAny(s string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func classify(msg string) *driver.Error {
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "stoken"):
		// 缓存 stoken 有过期语义,可重取重试;须先于风控分支匹配
		// (上游文案常含"请重试",会被风控分支截走),且不得误判为 share_gone。
		return driver.NewErr(driver.KindUpstream, "夸克 stoken 过期,请重试", errStokenExpired)
	case containsAny(m, "验证", "captcha", "频繁", "稍后", "请重试", "安全"):
		return driver.NewErr(driver.KindRiskControl, "夸克触发风控:"+msg, nil)
	case containsAny(m, "capacity", "容量", "转存"):
		// 线上实测:容量超限为 "capacity limit[{0}]"(任务轮询)或 "转存失败"(41013)
		return driver.NewErr(driver.KindRiskControl, "夸克转存受限(网盘容量不足或次数超限):"+msg, nil)
	case containsAny(m, "登录", "login", "鉴权", "身份", "未授权"):
		return driver.NewErr(driver.KindAuthExpired, "夸克 Cookie 已失效,请到账号页更新", nil)
	case containsAny(m, "提取码", "密码错误"):
		return driver.NewErr(driver.KindNotFound, "提取码错误", nil)
	case containsAny(m, "取消", "失效", "删除", "不存在", "封禁", "违规"):
		return driver.NewErr(driver.KindShareGone, "分享已失效:"+msg, nil)
	default:
		return driver.NewErr(driver.KindUpstream, "夸克接口异常:"+msg, nil)
	}
}

// ---- 内部请求 ----

func (d *Driver) call(ctx context.Context, method, rawURL string, body any, cred *driver.Credential, out *apiResp) (*httpx.Result, error) {
	h := map[string]string{"user-agent": QuarkUA, "referer": referer}
	if cred != nil && cred.Cookie != "" {
		h["cookie"] = cred.Cookie
	}
	res, err := d.client.DoJSON(ctx, method, rawURL, h, body, out)
	if err != nil {
		if errors.Is(err, httpx.ErrBadPayload) {
			return res, driver.NewErr(driver.KindInterfaceChanged, "夸克响应不是合法 JSON,疑似接口改版", err)
		}
		return res, driver.NewErr(driver.KindUpstream, "夸克网络请求失败", err)
	}
	return res, nil
}

// mustOK 校验业务码;Data 的反序列化由调用方按实际结构处理。
func mustOK(resp *apiResp) error {
	if resp.Code != 0 {
		return classify(resp.Message)
	}
	return nil
}

func (d *Driver) stoken(ctx context.Context, pwdID, passcode string, cred *driver.Credential) (string, error) {
	u := d.base + "/1/clouddrive/share/sharepage/token?pr=ucpro&fr=pc"
	var resp apiResp
	if _, err := d.call(ctx, "POST", u, map[string]string{"pwd_id": pwdID, "passcode": passcode}, cred, &resp); err != nil {
		return "", err
	}
	var data struct {
		Stoken string `json:"stoken"`
	}
	if err := mustOK(&resp); err != nil {
		return "", err
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克响应结构变化,解析失败", err)
	}
	if data.Stoken == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克响应缺少 stoken", nil)
	}
	return data.Stoken, nil
}

// ---- Driver 接口 ----
