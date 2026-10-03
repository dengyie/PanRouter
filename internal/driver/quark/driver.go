// Package quark 实现夸克网盘 driver。
// 接口依据 quark-auto-save 等开源实现的公开调用方式,风控形态变化时只需调整本包。
package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

const (
	baseURL  = "https://drive-pc.quark.cn"
	shareURL = "https://drive.quark.cn"
	infoURL  = "https://pan.quark.cn/account/info"
	referer  = "https://pan.quark.cn/"
	QuarkUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"
	linkTTL  = 2 * time.Hour
)

var pwdIDRe = regexp.MustCompile(`/s/([0-9a-zA-Z]+)`)

type Driver struct {
	client       *httpx.Client
	base         string        // token/detail/file 下载(drive-pc);测试注入 httptest 地址
	shareBase    string        // 分享直链端点(drive.quark.cn);测试注入 httptest 地址
	taskInterval time.Duration // 任务轮询间隔(测试可缩短)
}

func New(client *httpx.Client, base, shareBase string) *Driver {
	if base == "" {
		base = baseURL
	}
	if shareBase == "" {
		shareBase = shareURL
	}
	return &Driver{client: client, base: base, shareBase: shareBase, taskInterval: time.Second}
}

func (d *Driver) ID() string { return "quark" }

// ---- API 响应结构 ----

type apiResp struct {
	Status  int             `json:"status"`
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
	case containsAny(m, "验证", "captcha", "频繁", "稍后", "请重试", "安全"):
		return driver.NewErr(driver.KindRiskControl, "夸克触发风控:"+msg, nil)
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

func (d *Driver) ResolveShare(ctx context.Context, share driver.ShareLink, cred *driver.Credential) ([]driver.FileNode, error) {
	m := pwdIDRe.FindStringSubmatch(share.URL)
	if m == nil {
		return nil, driver.NewErr(driver.KindNotFound, "无法识别的夸克分享链接", nil)
	}
	pwdID := m[1]
	st, err := d.stoken(ctx, pwdID, share.Pwd, cred)
	if err != nil {
		return nil, err
	}
	var nodes []driver.FileNode
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
		q.Set("pwd_id", pwdID)
		q.Set("stoken", st)
		q.Set("pdir_fid", "0")
		q.Set("_page", fmt.Sprint(page))
		q.Set("_size", "50")
		q.Set("_fetch_banner", "0")
		q.Set("_fetch_share", "0")
		q.Set("_fetch_total", "1")
		q.Set("_sort", "file_type:asc,updated_at:desc")
		q.Set("ver", "2")
		var resp apiResp
		if _, err := d.call(ctx, "GET", d.base+"/1/clouddrive/share/sharepage/detail?"+q.Encode(), nil, cred, &resp); err != nil {
			return nil, err
		}
		var data struct {
			List     []quarkFile `json:"list"`
			Metadata struct {
				Total int `json:"_total"`
			} `json:"metadata"`
		}
		if err := mustOK(&resp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return nil, driver.NewErr(driver.KindInterfaceChanged, "夸克响应结构变化,解析失败", err)
		}
		for _, f := range data.List {
			name := f.FileName
			if name == "" {
				name = f.ShareName
			}
			nodes = append(nodes, driver.FileNode{
				FID: f.FID, Name: name, Size: f.Size, IsDir: f.Dir,
				Ext: map[string]string{"pwd_id": pwdID, "stoken": st, "pwd": share.Pwd},
			})
		}
		if len(data.List) == 0 || (data.Metadata.Total > 0 && len(nodes) >= data.Metadata.Total) {
			break
		}
	}
	return nodes, nil
}

func (d *Driver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if ref.Own {
		if cred == nil || cred.Cookie == "" {
			return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "夸克网盘内文件提链需要登录态,请添加夸克账号 Cookie", nil)
		}
		u := d.base + "/1/clouddrive/file/download?pr=ucpro&fr=pc&uc_param_str="
		var resp apiResp
		res, err := d.call(ctx, "POST", u, map[string]any{"fids": []string{ref.FID}}, cred, &resp)
		if err != nil {
			return driver.DirectLink{}, err
		}
		return d.linkFromList(res, &resp)
	}

	// 分享模式:端点在 drive.quark.cn,h5 网关对未登录请求隐藏(实测 404),必须带 Cookie
	pwdID := ref.Ext["pwd_id"]
	if pwdID == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "缺少夸克分享上下文,请重新解析", nil)
	}
	if cred == nil || cred.Cookie == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "夸克分享直链需要登录态,请先在「账号管理」添加夸克账号 Cookie", nil)
	}
	st := ref.Ext["stoken"]
	if st == "" {
		var err error
		if st, err = d.stoken(ctx, pwdID, ref.Ext["pwd"], cred); err != nil {
			return driver.DirectLink{}, err
		}
	}
	u := d.shareBase + "/1/clouddrive/share/sharepage/download?pr=ucpro&fr=pc"
	var resp apiResp
	res, err := d.call(ctx, "POST", u,
		map[string]any{"fid": ref.FID, "pwd_id": pwdID, "stoken": st}, cred, &resp)
	if err != nil {
		return driver.DirectLink{}, err
	}
	if err := mustOK(&resp); err != nil {
		return driver.DirectLink{}, err
	}
	return d.extractShareLink(ctx, cred, &resp, res)
}

// extractShareLink 解析分享下载接口的三种响应形态:
// 1) data 为数组,元素带 download_url;2) data.task_id → 轮询任务;3) data.data[0].download_url。
func (d *Driver) extractShareLink(ctx context.Context, cred *driver.Credential, resp *apiResp, res *httpx.Result) (driver.DirectLink, error) {
	// 形态 1:同步数组
	var list []struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(resp.Data, &list); err == nil && len(list) > 0 && list[0].DownloadURL != "" {
		return d.buildLink(res, list[0].DownloadURL), nil
	}
	// 形态 3:data.data[0].download_url
	var nested struct {
		Data []struct {
			DownloadURL string `json:"download_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Data, &nested); err == nil && len(nested.Data) > 0 && nested.Data[0].DownloadURL != "" {
		return d.buildLink(res, nested.Data[0].DownloadURL), nil
	}
	// 形态 2:异步任务
	var task struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(resp.Data, &task); err == nil && task.TaskID != "" {
		return d.pollTask(ctx, cred, task.TaskID, res)
	}
	return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged,
		fmt.Sprintf("夸克分享下载响应结构未识别: %.200s", string(resp.Data)), nil)
}

// pollTask 轮询夸克异步任务,直至 status==2(完成)拿 download_url。
func (d *Driver) pollTask(ctx context.Context, cred *driver.Credential, taskID string, dlRes *httpx.Result) (driver.DirectLink, error) {
	const attempts = 30
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return driver.DirectLink{}, driver.NewErr(driver.KindUpstream, "等待夸克任务时请求已取消", ctx.Err())
		case <-time.After(d.taskInterval):
		}
		u := fmt.Sprintf("%s/1/clouddrive/task?pr=ucpro&fr=pc&task_id=%s&retry_index=%d", d.shareBase, url.QueryEscape(taskID), i)
		var resp apiResp
		if _, err := d.call(ctx, "GET", u, nil, cred, &resp); err != nil {
			return driver.DirectLink{}, err
		}
		if err := mustOK(&resp); err != nil {
			return driver.DirectLink{}, err
		}
		var data struct {
			Status int `json:"status"`
			Data   []struct {
				DownloadURL string `json:"download_url"`
			} `json:"data"`
			DownloadURL string `json:"download_url"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged, "夸克任务响应结构变化,解析失败", err)
		}
		if data.Status == 2 {
			if u := data.DownloadURL; u != "" {
				return d.buildLink(dlRes, u), nil
			}
			if len(data.Data) > 0 && data.Data[0].DownloadURL != "" {
				return d.buildLink(dlRes, data.Data[0].DownloadURL), nil
			}
			return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged, "夸克任务已完成但缺少下载直链", nil)
		}
	}
	return driver.DirectLink{}, driver.NewErr(driver.KindUpstream, "夸克任务轮询超时,请稍后重试", nil)
}

func (d *Driver) linkFromList(res *httpx.Result, resp *apiResp) (driver.DirectLink, error) {
	var data []struct {
		DownloadURL string `json:"download_url"`
	}
	if err := mustOK(resp); err != nil {
		return driver.DirectLink{}, err
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged, "夸克响应结构变化,解析失败", err)
	}
	if len(data) == 0 || data[0].DownloadURL == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindInterfaceChanged, "夸克响应缺少下载直链", nil)
	}
	return d.buildLink(res, data[0].DownloadURL), nil
}

// buildLink 组装 DirectLink:直链要求夸克客户端 UA + Referer,并携带下载接口下发的 Cookie(如 __puus)。
func (d *Driver) buildLink(res *httpx.Result, downloadURL string) driver.DirectLink {
	var cookie string
	if res != nil {
		cookie = joinCookies(res.Header.Values("Set-Cookie"))
	}
	return driver.DirectLink{
		URL:       downloadURL,
		UA:        QuarkUA,
		Referer:   referer,
		Cookie:    cookie,
		BindIP:    false,
		ExpiresAt: time.Now().Add(linkTTL),
	}
}

func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	if cred.Cookie == "" {
		return driver.CredStatus{Valid: false, Message: "Cookie 为空"}, nil
	}
	u := infoURL + "?fr=pc&platform=pc"
	var resp apiResp
	if _, err := d.call(ctx, "GET", u, nil, &cred, &resp); err != nil {
		return driver.CredStatus{}, err
	}
	if resp.Code != 0 {
		return driver.CredStatus{Valid: false, Message: resp.Message}, nil
	}
	var data struct {
		Nickname string `json:"nickname"`
	}
	_ = json.Unmarshal(resp.Data, &data)
	return driver.CredStatus{Valid: true, Nickname: data.Nickname, Message: "ok"}, nil
}

// joinCookies 把下载接口返回的 Set-Cookie 合并为请求用 Cookie 串(如 __puus)。
func joinCookies(setCookies []string) string {
	var parts []string
	seen := map[string]bool{}
	for _, sc := range setCookies {
		kv := strings.SplitN(sc, ";", 2)[0]
		name := strings.TrimSpace(strings.SplitN(kv, "=", 2)[0])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, kv)
	}
	return strings.Join(parts, "; ")
}
