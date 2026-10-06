// 扫码登录:夸克官方 CAS 流程——服务端取 token 生成二维码,用户用夸克 App 扫码,
// 轮询拿 service_ticket 后经 pan.quark.cn/account/info?st=<ticket>&lw=scan 换取登录态,
// 从响应 Set-Cookie 提取完整 Cookie(与网页登录同源,含 __puus 等强校验项)。
// 端点与参数为社区通用流程(客户端 client_id=532),夸克改版时只需调整本文件。
package quark

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

const (
	// qrClientID 是夸克网页端登录的固定 client_id(社区通用值)。
	qrClientID = "532"
	// qrPollInterval 与 qrTimeout 约束轮询节奏:二维码有效期约 3 分钟。
	qrPollInterval = 2 * time.Second
	qrTimeout      = 180 * time.Second
)

// CAS 端点域名默认官方值;Driver 字段可注入(httptest mock),空回落常量。
const (
	defaultUopDomain = "https://uop.quark.cn"
	defaultQRInfoURL = "https://pan.quark.cn/account/info"
)

// QRCode 是一次扫码会话:Token 拼进二维码 URL,轮询用同一 token。
type QRCode struct {
	Token string `json:"token"`
	URL   string `json:"url"` // su.quark.cn 跳转链接,前端渲染成二维码
}

// qrStatus 是轮询中间态:pending(未扫)、confirmed(已出 ticket)、expired。
type qrStatus struct {
	State  string
	Ticket string
}

// QRToken 获取一次性扫码 token 与二维码 URL。
func (d *Driver) QRToken(ctx context.Context) (*QRCode, error) {
	q := url.Values{}
	q.Set("client_id", qrClientID)
	q.Set("v", "1.2")
	q.Set("request_id", newRequestID())
	u := d.uopEndpoint() + "/cas/ajax/getTokenForQrcodeLogin?" + q.Encode()

	var resp struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Members struct {
				Token string `json:"token"`
			} `json:"members"`
		} `json:"data"`
	}
	// CAS 端点不在 drive-pc 域,不走 classify/mustOK,直接判 status
	if _, err := d.callRaw(ctx, http.MethodGet, u, nil, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Status != 2000000 || resp.Data.Members.Token == "" {
		return nil, classify(fmt.Sprintf("获取扫码 token 失败(%d %s)", resp.Status, resp.Message))
	}
	token := resp.Data.Members.Token
	return &QRCode{Token: token, URL: buildQRURL(token)}, nil
}

// buildQRURL 构造夸克 App 可扫的跳转链接(ssb=weblogin 固定网页登录场景)。
func buildQRURL(token string) string {
	q := url.Values{}
	q.Set("token", token)
	q.Set("client_id", qrClientID)
	q.Set("ssb", "weblogin")
	q.Set("uc_param_str", "")
	q.Set("uc_biz_str", "S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0")
	return "https://su.quark.cn/4_eMHBJ?" + q.Encode()
}

// QRPoll 轮询一次扫码状态:pending=未扫,confirmed 返回已换好的 Cookie,
// expired=二维码失效(需重新取 token)。
func (d *Driver) QRPoll(ctx context.Context, token string) (cookie string, state string, err error) {
	q := url.Values{}
	q.Set("client_id", qrClientID)
	q.Set("v", "1.2")
	q.Set("token", token)
	q.Set("request_id", newRequestID())
	u := d.uopEndpoint() + "/cas/ajax/getServiceTicketByQrcodeToken?" + q.Encode()

	var resp struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Members struct {
				ServiceTicket string `json:"service_ticket"`
			} `json:"members"`
		} `json:"data"`
	}
	res, err := d.callRaw(ctx, http.MethodGet, u, nil, nil, &resp)
	if err != nil {
		return "", "", err
	}
	switch {
	case resp.Status == 2000000 && resp.Data.Members.ServiceTicket != "":
		cookie, err := d.exchangeTicket(ctx, resp.Data.Members.ServiceTicket)
		if err != nil {
			return "", "", err
		}
		return cookie, "confirmed", nil
	case resp.Status == 2000000:
		return "", "pending", nil
	case isQRWaitingStatus(resp.Status):
		return "", "pending", nil
	case isQRExpiredStatus(resp.Status):
		return "", "expired", nil
	default:
		dbg("QRPoll unexpected status=%d msg=%s body=%.200s", resp.Status, resp.Message, string(res.Body))
		return "", "", classify(resp.Message)
	}
}

// isQRWaitingStatus:官方 CAS 对"未扫码/已扫码未确认"返回的非成功业务码。
// 线上实测(2026-10-07):未扫码稳定返回 50004001 "Query result is empty";
// 80005000 系列为社区流传旧码,保留兼容。
func isQRWaitingStatus(code int) bool {
	switch code {
	case 50004001, // 未扫码/查询结果为空(实测)
		80005000, // 未扫码(社区)
		80005001, // 已扫码,等待手机确认
		80005002: // 确认中
		return true
	}
	return false
}

// isQRExpiredStatus:二维码过期、被作废或 token 已失效(50004002 实测)。
func isQRExpiredStatus(code int) bool {
	switch code {
	case 50004002, // Token Not Found(实测:token 被消耗或失效)
		80005003, // 已过期
		80005004: // 已作废
		return true
	}
	return false
}

// exchangeTicket 用 service_ticket 换登录态:访问 account/info?st=<ticket>&lw=scan,
// 上游会以 Set-Cookie 下发完整会话(__puus 等强校验项在内)。
func (d *Driver) exchangeTicket(ctx context.Context, ticket string) (string, error) {
	q := url.Values{}
	q.Set("st", ticket)
	q.Set("lw", "scan")
	u := d.infoEndpoint() + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "换取登录 Cookie 失败", err)
	}
	req.Header.Set("User-Agent", QuarkUA)
	req.Header.Set("Referer", referer)
	res, err := d.client.Do(req)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "换取登录 Cookie 失败", err)
	}
	cookie := joinCookies(res.Header.Values("Set-Cookie"))
	if !strings.Contains(cookie, "__puus") && !strings.Contains(cookie, "__pus") {
		return "", driver.NewErr(driver.KindUpstream, "登录响应缺少会话 Cookie,请重试", nil)
	}
	return cookie, nil
}

// uopDomain/qrInfoURL 返回扫码登录的端点(测试注入 httptest 地址)。
func (d *Driver) uopEndpoint() string {
	if d.QRUopDomain != "" {
		return d.QRUopDomain
	}
	return defaultUopDomain
}

func (d *Driver) infoEndpoint() string {
	if d.QRInfoURL != "" {
		return d.QRInfoURL
	}
	return defaultQRInfoURL
}

// newRequestID 生成 CAS 请求的 request_id(crypto/rand hex,非安全用途)。
func newRequestID() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// callRaw 是绕过 classify 的原始 JSON 调用(CAS 域错误码体系与 drive-pc 不同)。
func (d *Driver) callRaw(ctx context.Context, method, rawURL string, body any, cred *driver.Credential, out any) (*httpx.Result, error) {
	var h map[string]string
	if cred != nil && cred.Cookie != "" {
		h = map[string]string{"user-agent": QuarkUA, "referer": referer, "cookie": cred.Cookie}
	} else {
		h = map[string]string{"user-agent": QuarkUA, "referer": referer}
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
