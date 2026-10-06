// 直链与凭据:GetDirectLink(分享模式编排转存链)、DirectLink 组装、Cookie 合并、CheckCredential。
package quark

import (
	"context"
	"encoding/json"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"strings"
	"time"
)

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
	// 转存链:转存到专用暂存目录 → 轮询任务拿新 fid → file/download 取直链 → 尽力清理转存副本
	pwdID := ref.Ext["pwd_id"]
	if pwdID == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "缺少夸克分享上下文,请重新解析", nil)
	}
	if cred == nil || cred.Cookie == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "夸克分享直链需要登录态,请先在「账号管理」添加夸克账号 Cookie", nil)
	}
	tmpFID, err := d.tmpDir(ctx, cred)
	if err != nil {
		return driver.DirectLink{}, err
	}
	savedFid, err := d.saveWithStokenRetry(ctx, cred, pwdID, ref, tmpFID)
	if err != nil {
		// 转存失败可能是暂存目录已被外部删除:丢弃缓存,下次 find-or-create 重建
		d.forgetTmpDir(cred)
		return driver.DirectLink{}, err
	}
	dbg("GetDirectLink tmpFID=%s savedFid=%s srcFid=%s", tmpFID, savedFid, ref.FID)
	// 转存副本已在暂存目录内:无论后续取链成功与否都清理,避免网盘堆积重复文件。
	// 用 WithoutCancel 保证请求被取消时清理仍能完成。
	defer d.cleanupSaved(context.WithoutCancel(ctx), cred, savedFid)
	u := d.base + "/1/clouddrive/file/download?pr=ucpro&fr=pc&uc_param_str="
	var resp apiResp
	res, err := d.call(ctx, "POST", u, map[string]any{"fids": []string{savedFid}}, cred, &resp)
	if err != nil {
		return driver.DirectLink{}, err
	}
	dbg("GetDirectLink download resp code=%d msg=%s data=%s", resp.Code, resp.Message, string(resp.Data))
	return d.linkFromList(cred, res, &resp)
}

// saveWithStokenRetry 执行转存;stoken 过期时重取一次。resolve 快照里的 stoken 有时效,
// 过期必然发生,故重试是常态路径而非异常兜底。

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
// 按 ";" 分割并 trim,容忍紧凑串与混合空白(来源拼接格式不一),畸形项丢弃。
func mergeCookies(base, patch string) string {
	if patch == "" {
		return base
	}
	if base == "" {
		return strings.TrimSpace(patch)
	}
	result := []string{}
	seen := map[string]string{}
	for _, part := range strings.Split(base, ";") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			continue
		}
		seen[strings.TrimSpace(kv[0])] = kv[1]
		result = append(result, part)
	}
	for _, part := range strings.Split(patch, ";") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			continue
		}
		name := strings.TrimSpace(kv[0])
		if _, ok := seen[name]; ok {
			for i, p := range result {
				if strings.TrimSpace(strings.SplitN(p, "=", 2)[0]) == name {
					result[i] = part
					break
				}
			}
		} else {
			result = append(result, part)
		}
		seen[name] = kv[1]
	}
	return strings.Join(result, "; ")
}

func (d *Driver) CheckCredential(ctx context.Context, cred driver.Credential) (driver.CredStatus, error) {
	if cred.Cookie == "" {
		return driver.CredStatus{Valid: false, Message: "Cookie 为空"}, nil
	}
	u := d.infoEndpoint() + "?fr=pc&platform=pc"
	// pan.quark.cn/account/info 的业务码是字符串("OK"),与 drive-pc 的 int 0 不同,
	// 不能走 apiResp(int)/mustOK,用宽松解码单独处理。
	var resp struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if _, err := d.callRaw(ctx, "GET", u, nil, &cred, &resp); err != nil {
		return driver.CredStatus{}, err
	}
	if code := string(resp.Code); code != `"OK"` && code != `0` {
		return driver.CredStatus{Valid: false, Message: resp.Message}, nil
	}
	var data struct {
		Nickname string `json:"nickname"`
	}
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return driver.CredStatus{}, driver.NewErr(driver.KindInterfaceChanged, "夸克账号信息响应结构变化,解析失败", err)
		}
	}
	return driver.CredStatus{Valid: true, Nickname: data.Nickname, Message: "ok"}, nil
}

// joinCookies 把下载接口返回的 Set-Cookie 合并为请求用 Cookie 串(如 __puus)。
func joinCookies(setCookies []string) string {
	var parts []string
	seen := map[string]bool{}
	for _, sc := range setCookies {
		kv := strings.TrimSpace(strings.SplitN(sc, ";", 2)[0])
		name := strings.TrimSpace(strings.SplitN(kv, "=", 2)[0])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		parts = append(parts, kv)
	}
	return strings.Join(parts, "; ")
}
