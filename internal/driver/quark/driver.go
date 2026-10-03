// Package quark 实现夸克网盘 driver。
// 接口依据 quark-auto-save 等开源实现的公开调用方式,风控形态变化时只需调整本包。
package quark

import (
	"context"
	"encoding/json"
	"errors"
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
	infoURL  = "https://pan.quark.cn/account/info"
	referer  = "https://pan.quark.cn/"
	QuarkUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"
	linkTTL  = 2 * time.Hour
)

var pwdIDRe = regexp.MustCompile(`/s/([0-9a-zA-Z]+)`)

type Driver struct {
	client       *httpx.Client
	base         string        // token/detail/save/task/file 全链路(drive-pc);测试注入 httptest 地址
	taskInterval time.Duration // 任务轮询间隔(测试可缩短)
}

func New(client *httpx.Client, base, shareBase string) *Driver {
	if base == "" {
		base = baseURL
	}
	// shareBase 参数保留占位以兼容既有调用方;转存链已全部收敛到 drive-pc 单域。
	_ = shareBase
	return &Driver{client: client, base: base, taskInterval: time.Second}
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
	case containsAny(m, "stoken"):
		// 缓存的 stoken 有时效,过期可重新获取;误判为 share_gone 会误导缓存失效策略
		return driver.NewErr(driver.KindUpstream, "夸克 stoken 过期,请重试", nil)
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
		return d.linkFromList(cred, res, &resp)
	}

	// 分享模式:夸克已下线 sharepage/download 直链端点(实测 404),现行流程为网页同款
	// 转存链:转存到自己网盘 → 轮询任务拿新 fid → file/download 取直链 → 尽力清理转存副本
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
	savedFid, err := d.saveToDrive(ctx, cred, pwdID, st, ref.FID)
	if err != nil && stokenExpired(err) {
		// resolve 时缓存的 stoken 有时效:重新获取后重试一次
		var st2 string
		if st2, err = d.stoken(ctx, pwdID, ref.Ext["pwd"], cred); err != nil {
			return driver.DirectLink{}, err
		}
		savedFid, err = d.saveToDrive(ctx, cred, pwdID, st2, ref.FID)
	}
	if err != nil {
		return driver.DirectLink{}, err
	}
	u := d.base + "/1/clouddrive/file/download?pr=ucpro&fr=pc&uc_param_str="
	var resp apiResp
	res, err := d.call(ctx, "POST", u, map[string]any{"fids": []string{savedFid}}, cred, &resp)
	if err != nil {
		return driver.DirectLink{}, err
	}
	dl, err := d.linkFromList(cred, res, &resp)
	if err != nil {
		return driver.DirectLink{}, err
	}
	d.cleanupSaved(ctx, cred, savedFid)
	return dl, nil
}

// saveToDrive 转存分享文件到自己网盘根目录,轮询任务拿到转存后的新 fid。
// 请求体为线上实测:fid_list 精确选择单文件(pdir_save_all=true 会转存整个分享,
// save_as_select_top_fids 字段实测不生效,一律 41013)。
func (d *Driver) saveToDrive(ctx context.Context, cred *driver.Credential, pwdID, stoken, fid string) (string, error) {
	body := map[string]any{
		"fid_list": []string{fid}, "to_pdir_fid": "0", "pwd_id": pwdID, "stoken": stoken,
	}
	var resp apiResp
	if _, err := d.call(ctx, "POST", d.base+"/1/clouddrive/share/sharepage/save?pr=ucpro&fr=pc", body, cred, &resp); err != nil {
		return "", err
	}
	var data struct {
		TaskID string `json:"task_id"`
	}
	if err := mustOK(&resp); err != nil {
		return "", err
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克转存响应结构变化,解析失败", err)
	}
	if data.TaskID == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克转存响应缺少 task_id", nil)
	}
	return d.pollSaveTask(ctx, cred, data.TaskID)
}

// pollSaveTask 轮询转存任务直至 status==2(任务轮询端点实测在 drive-pc),返回转存后的新 fid。
func (d *Driver) pollSaveTask(ctx context.Context, cred *driver.Credential, taskID string) (string, error) {
	const attempts = 30
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			return "", driver.NewErr(driver.KindUpstream, "等待夸克转存任务时请求已取消", ctx.Err())
		case <-time.After(d.taskInterval):
		}
		u := fmt.Sprintf("%s/1/clouddrive/task?pr=ucpro&fr=pc&task_id=%s&retry_index=%d", d.base, url.QueryEscape(taskID), i)
		var resp apiResp
		if _, err := d.call(ctx, "GET", u, nil, cred, &resp); err != nil {
			return "", err
		}
		if err := mustOK(&resp); err != nil {
			return "", err
		}
		var data struct {
			Status int `json:"status"`
			SaveAs struct {
				SaveAsSelectTopFids []string `json:"save_as_select_top_fids"`
				SaveAsTopFids       []string `json:"save_as_top_fids"`
			} `json:"save_as"`
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return "", driver.NewErr(driver.KindInterfaceChanged, "夸克转存任务响应结构变化,解析失败", err)
		}
		if data.Status == 2 {
			if len(data.SaveAs.SaveAsSelectTopFids) > 0 && data.SaveAs.SaveAsSelectTopFids[0] != "" {
				return data.SaveAs.SaveAsSelectTopFids[0], nil
			}
			if len(data.SaveAs.SaveAsTopFids) > 0 && data.SaveAs.SaveAsTopFids[0] != "" {
				return data.SaveAs.SaveAsTopFids[0], nil
			}
			return "", driver.NewErr(driver.KindInterfaceChanged, "夸克转存任务完成但缺少新文件 fid", nil)
		}
	}
	return "", driver.NewErr(driver.KindUpstream, "夸克转存任务轮询超时,请稍后重试", nil)
}

// stokenExpired 识别 stoken 过期类错误(重试前提)。
func stokenExpired(err error) bool {
	var de *driver.Error
	if !errors.As(err, &de) {
		return false
	}
	return de.Kind == driver.KindUpstream && strings.Contains(de.UserHint, "stoken")
}

// cleanupSaved 尽力删除转存副本,避免每次解析都在网盘堆积重复文件。
// 直链已取回,删除失败仅忽略(副本残留不影响本次下载)。
func (d *Driver) cleanupSaved(ctx context.Context, cred *driver.Credential, fid string) {
	u := d.base + "/1/clouddrive/file/delete?pr=ucpro&fr=pc"
	body := map[string]any{"action_type": 2, "filelist": []string{fid}, "exclude_fids": []string{}}
	var resp apiResp
	_, _ = d.call(ctx, "POST", u, body, cred, &resp)
}

func (d *Driver) linkFromList(cred *driver.Credential, res *httpx.Result, resp *apiResp) (driver.DirectLink, error) {
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
	return d.buildLink(cred, res, data[0].DownloadURL), nil
}

// buildLink 组装 DirectLink:直链要求夸克客户端 UA + Referer。
// Cookie 以登录态为基底(线上实测 CDN 强校验完整 Cookie,缺任一项 403),
// 下载接口下发的 Set-Cookie(如轮换的 __puus)同名覆盖。
func (d *Driver) buildLink(cred *driver.Credential, res *httpx.Result, downloadURL string) driver.DirectLink {
	cookie := ""
	if cred != nil {
		cookie = cred.Cookie
	}
	if res != nil {
		cookie = mergeCookies(cookie, joinCookies(res.Header.Values("Set-Cookie")))
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

// mergeCookies 以 base 为基底,patch 中同名项覆盖(base 顺序在前保证可读性)。
func mergeCookies(base, patch string) string {
	if patch == "" {
		return base
	}
	if base == "" {
		return patch
	}
	result := []string{}
	seen := map[string]string{}
	for _, part := range strings.Split(base, "; ") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		seen[kv[0]] = kv[1]
		result = append(result, part)
	}
	for _, part := range strings.Split(patch, "; ") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		if _, ok := seen[kv[0]]; ok {
			for i, p := range result {
				if strings.SplitN(p, "=", 2)[0] == kv[0] {
					result[i] = part
					break
				}
			}
		} else {
			seen[kv[0]] = kv[1]
			result = append(result, part)
		}
	}
	return strings.Join(result, "; ")
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
