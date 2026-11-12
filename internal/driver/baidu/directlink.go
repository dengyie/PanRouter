// 直链链:暂存目录 → 转存 → locatedownload/filemetas 取直链 → 副本清理(bdstoken 刷新重试为常态路径)。
package baidu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
)

func (d *Driver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	if cred == nil || cred.Cookie == "" {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired,
			"百度提链需要登录态,请先在「账号管理」添加百度账号 Cookie", nil)
	}
	if ref.Own {
		token, err := d.getBdstoken(ctx, cred, false)
		if err != nil {
			return driver.DirectLink{}, err
		}
		u, err := d.filemetasDlink(ctx, cred.Cookie, token, ref.FID)
		if err != nil {
			return driver.DirectLink{}, err
		}
		return d.linkFrom(cookieHeader(cred.Cookie, ""), u), nil
	}
	if ref.Ext == nil {
		return driver.DirectLink{}, driver.NewErr(driver.KindNotFound, "缺少百度分享上下文,请重新解析", nil)
	}
	shareID, uk, sekey := ref.Ext["share_id"], ref.Ext["uk"], ref.Ext["sekey"]

	tmp := d.ensureTmpDir(ctx, cred)
	if tmp != "" {
		if err := d.takeTransferQuota(cred.Cookie); err != nil {
			return driver.DirectLink{}, err
		}
		newFsid, newPath, err := d.transferWithTokenRetry(ctx, cred, shareID, uk, sekey, ref.FID, tmp)
		if err != nil {
			// 转存目录可能被外部删除:丢弃缓存,下次 find-or-create 重建。
			d.forgetTmpDir(cred.Cookie)
			return driver.DirectLink{}, err
		}
		defer d.cleanupSaved(context.WithoutCancel(ctx), cred, shareID, uk, sekey, newPath)
		if u, err := d.locateDownload(ctx, cred.Cookie, newPath); err == nil {
			return d.linkFrom(cookieHeader(cred.Cookie, sekey), u), nil
		} else {
			// locatedownload 抓包常量(psign 等)若被百度轮换,退回 filemetas 按 fs_id 取链。
			_ = err
		}
		token, err := d.getBdstoken(ctx, cred, false)
		if err != nil {
			return driver.DirectLink{}, err
		}
		u, err := d.filemetasDlink(ctx, cred.Cookie, token, newFsid)
		if err != nil {
			return driver.DirectLink{}, err
		}
		return d.linkFrom(cookieHeader(cred.Cookie, sekey), u), nil
	}
	return driver.DirectLink{}, driver.NewErr(driver.KindUpstream, "百度暂存目录不可用", nil)
}

// linkFrom 组装 DirectLink:百度直链要求 netdisk UA + 登录 Cookie(实测缺任一项 403)。
func (d *Driver) linkFrom(cookie, u string) driver.DirectLink {
	return driver.DirectLink{
		URL:       u,
		UA:        NetdiskUA,
		Referer:   referer,
		Cookie:    cookie,
		BindIP:    false,
		ExpiresAt: time.Now().Add(linkTTL),
	}
}

// ---- 转存 ----

// ensureTmpDir 按登录态指纹缓存暂存目录路径,不存在则创建 /panrouter_tmp。
func (d *Driver) ensureTmpDir(ctx context.Context, cred *driver.Credential) string {
	key := fingerprint(cred.Cookie)
	d.tmpMu.Lock()
	p := d.tmpDirs[key]
	d.tmpMu.Unlock()
	if p != "" {
		return p
	}
	token, err := d.getBdstoken(ctx, cred, false)
	if err != nil {
		return ""
	}
	if err := d.createDir(ctx, cred.Cookie, token, "/"+tmpDirName); err == nil {
		d.tmpMu.Lock()
		d.tmpDirs[key] = "/" + tmpDirName
		d.tmpMu.Unlock()
		return "/" + tmpDirName
	}
	// 创建失败大概率是「目录已存在」:按名探测确认后再复用。
	for _, rootPath := range d.listRootPaths(ctx, cred.Cookie, token) {
		if rootPath == "/"+tmpDirName {
			d.tmpMu.Lock()
			d.tmpDirs[key] = "/" + tmpDirName
			d.tmpMu.Unlock()
			return "/" + tmpDirName
		}
	}
	return ""
}

func (d *Driver) forgetTmpDir(cookie string) {
	d.tmpMu.Lock()
	delete(d.tmpDirs, fingerprint(cookie))
	d.tmpMu.Unlock()
}

// createDir 在个人网盘根目录下建目录(已存在时接口报错,由调用方按 exist 兜底)。
func (d *Driver) createDir(ctx context.Context, cookie, token, path string) error {
	form := url.Values{}
	form.Set("path", path)
	form.Set("isdir", "1")
	form.Set("size", "")
	form.Set("block_list", "[]")
	form.Set("method", "post")
	form.Set("dataType", "json")
	u := d.base + "/api/create?a=commit&channel=chunlei&web=1&app_id=" + appID +
		"&clienttype=0&bdstoken=" + url.QueryEscape(token)
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
	}
	_, err := d.call(ctx, "POST", u, form, map[string]string{"cookie": cookie, "referer": "https://pan.baidu.com/disk/main"}, &out)
	if err != nil {
		return err
	}
	if err := mustOK(out.Errno, out.ErrMsg); err != nil {
		return err
	}
	return nil
}

// listRootPaths 返回个人网盘根目录下的路径列表(供 tmpdir 存在性探测)。
func (d *Driver) listRootPaths(ctx context.Context, cookie, token string) []string {
	u := d.base + "/api/list?clienttype=0&app_id=" + appID +
		"&web=1&order=time&desc=1&dir=" + url.QueryEscape("/") + "&num=100&page=1"
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		List   []struct {
			Path string `json:"path"`
		} `json:"list"`
	}
	if _, err := d.call(ctx, "GET", u, nil, map[string]string{"cookie": cookie}, &out); err != nil {
		return nil
	}
	paths := make([]string, 0, len(out.List))
	for _, it := range out.List {
		paths = append(paths, it.Path)
	}
	return paths
}

// transferWithTokenRetry 执行转存;bdstoken 失效(errno=105/-6)时重取一次。
func (d *Driver) transferWithTokenRetry(ctx context.Context, cred *driver.Credential, shareID, uk, sekey, fsid, toDir string) (string, string, error) {
	token, err := d.getBdstoken(ctx, cred, false)
	if err != nil {
		return "", "", err
	}
	newFsid, newPath, err := d.transfer(ctx, cred.Cookie, shareID, uk, sekey, fsid, toDir, token)
	if err != nil && isBdstoken(err) {
		d.forgetBdstoken(cred.Cookie)
		if token, err = d.getBdstoken(ctx, cred, true); err != nil {
			return "", "", err
		}
		newFsid, newPath, err = d.transfer(ctx, cred.Cookie, shareID, uk, sekey, fsid, toDir, token)
	}
	return newFsid, newPath, err
}

func isBdstoken(err error) bool {
	var de *driver.Error
	if !errors.As(err, &de) {
		return false
	}
	return errors.Is(de, errBdstoken)
}

// transfer 转存分享中的单个文件到 toDir,返回转存后的新 fs_id 与新路径。
// ondup=newcopy:同名文件一律复制新副本,不会覆盖旧文件。
func (d *Driver) transfer(ctx context.Context, cookie, shareID, uk, sekey, fsid, toDir, token string) (string, string, error) {
	form := url.Values{}
	form.Set("fsidlist", `["`+fsid+`"]`)
	form.Set("path", toDir)
	u := d.base + "/share/transfer?shareid=" + url.QueryEscape(shareID) +
		"&from=" + url.QueryEscape(uk) +
		"&channel=chunlei&sekey=" + url.QueryEscape(sekey) +
		"&ondup=newcopy&web=1&app_id=" + appID +
		"&bdstoken=" + url.QueryEscape(token) + "&clienttype=0"
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Extra  struct {
			List []struct {
				ToFsID string `json:"to_fs_id"`
				To     string `json:"to"`
			} `json:"list"`
		} `json:"extra"`
	}
	_, err := d.call(ctx, "POST", u, form, map[string]string{
		"cookie":  cookieHeader(cookie, sekey),
		"origin":  "https://pan.baidu.com",
		"referer": "https://pan.baidu.com/s/",
	}, &out)
	if err != nil {
		return "", "", err
	}
	if err := mustOK(out.Errno, out.ErrMsg); err != nil {
		return "", "", err
	}
	if len(out.Extra.List) == 0 || out.Extra.List[0].ToFsID == "" {
		return "", "", driver.NewErr(driver.KindInterfaceChanged, "百度转存响应缺少新文件 fs_id", nil)
	}
	first := out.Extra.List[0]
	to := first.To
	if to == "" {
		to = strings.TrimRight(toDir, "/") + "/" + fsid
	}
	return first.ToFsID, to, nil
}

// ---- 取直链 ----

// locateDownload 走客户端协议的高带宽直链(自带 sign,无需计算)。
// 抓包常量(psign/rand/devuid 等)有登录态即可复用;百度轮换时返回错误,由调用方退 filemetas。
func (d *Driver) locateDownload(ctx context.Context, cookie, path string) (string, error) {
	now := time.Now().Unix()
	u := d.pcs + "/rest/2.0/pcs/file" +
		"?method=locatedownload" +
		"&app_id=" + appID +
		"&clienttype=17&ver=4.0" +
		"&ant=1&check_blue=1&es=1&esl=1&apn_id=1_-1" +
		"&freeisp=0&queryfree=0&use=1&dtype=1&eck=1&ehps=1" +
		"&err_ver=1.0&network_type=WIFI&channel=0" +
		"&path=" + url.QueryEscape(path) +
		"&time=" + itoa(int(now)) +
		"&rand=5ed606e9da222cde0474cdf70eda884b" +
		"&devuid=0F1E9FC2E084472DA5A61C4CF4C759AF" +
		"&cuid=0F1E9FC2E084472DA5A61C4CF4C759AF" +
		"&deviceid=348642637967375013" +
		"&psign=860a071f77c860e8cea06e4e54c518f3" +
		"&version=2.2.111.34&version_app=12.24.6&vip=0"
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader("0"))
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "构造百度直链请求失败", err)
	}
	req.Header.Set("User-Agent", NetdiskUA)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := d.client.Do(req)
	if err != nil {
		return "", driver.NewErr(driver.KindUpstream, "百度 locatedownload 网络请求失败", err)
	}
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Urls   []struct {
			URL     string `json:"url"`
			Encrypt int    `json:"encrypt"`
		} `json:"urls"`
	}
	if len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, &out); err != nil {
			return "", driver.NewErr(driver.KindInterfaceChanged, "百度 locatedownload 响应不是合法 JSON", err)
		}
	}
	if out.Errno != 0 {
		return "", mustOK(out.Errno, out.ErrMsg)
	}
	var candidate string
	for _, it := range out.Urls {
		if it.URL == "" {
			continue
		}
		// encrypt=0 明文 CDN 最稳;没有则退回第一个 https 候选。
		if it.Encrypt == 0 && strings.HasPrefix(it.URL, "https") {
			return it.URL, nil
		}
		if candidate == "" || strings.HasPrefix(it.URL, "https") {
			candidate = it.URL
		}
	}
	if candidate != "" {
		return candidate, nil
	}
	return "", driver.NewErr(driver.KindInterfaceChanged, "百度 locatedownload 未返回可用直链", nil)
}

// filemetasDlink 按 fs_id 从 filemetas 取 dlink(个人文件/转存后文件皆可)。
func (d *Driver) filemetasDlink(ctx context.Context, cookie, token, fsid string) (string, error) {
	fsids := `["` + fsid + `"]`
	u := d.base + "/api/filemetas?dlink=1&fsids=" + url.QueryEscape(fsids) +
		"&bdstoken=" + url.QueryEscape(token) +
		"&clienttype=0&app_id=" + appID + "&web=1"
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Info   []struct {
			Dlink string `json:"dlink"`
		} `json:"info"`
	}
	if _, err := d.call(ctx, "GET", u, nil, map[string]string{"cookie": cookie}, &out); err != nil {
		return "", err
	}
	if err := mustOK(out.Errno, out.ErrMsg); err != nil {
		return "", err
	}
	if len(out.Info) == 0 || out.Info[0].Dlink == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "百度 filemetas 未返回 dlink", nil)
	}
	return out.Info[0].Dlink, nil
}

// cleanupSaved 尽力删除转存副本,避免每次提链都在用户网盘堆积重复文件。
// 直链已取回,删除失败仅忽略。用独立预算执行,请求被取消也能清掉。
func (d *Driver) cleanupSaved(ctx context.Context, cred *driver.Credential, shareID, uk, sekey, path string) {
	cleanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := d.getBdstoken(cleanCtx, cred, false)
	if err != nil || path == "" {
		return
	}
	form := url.Values{}
	form.Set("filelist", `["`+path+`"]`)
	u := d.base + "/api/filemanager?async=2&onnest=fail&opera=delete&bdstoken=" +
		url.QueryEscape(token) + "&newVerify=1&clienttype=0&app_id=" + appID + "&web=1"
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
	}
	_, _ = d.call(cleanCtx, "POST", u, form, map[string]string{
		"cookie": cookieHeader(cred.Cookie, sekey),
	}, &out)
}
