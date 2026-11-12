// 分享解析:链接解析 → 提取码校验 → xpan/share 递归列目录(预算上界防拖死)。
package baidu

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/dengyie/panrouter/internal/driver"
)

func (d *Driver) ResolveShare(ctx context.Context, share driver.ShareLink, cred *driver.Credential) ([]driver.FileNode, error) {
	surl, pwd, err := parseShare(share)
	if err != nil {
		return nil, err
	}
	sekey := ""
	if pwd != "" {
		if sekey, err = d.verify(ctx, surl, pwd, cred); err != nil {
			return nil, err
		}
	}
	var cookie string
	if cred != nil {
		cookie = cred.Cookie
	}
	ext := map[string]string{"surl": surl, "sekey": sekey, "pwd": pwd}
	bud := &shareWalkBudget{files: maxShareFiles, dirs: maxShareDirs}
	nodes, err := d.walkShare(ctx, cookie, "/", "", 0, ext, map[string]struct{}{}, bud)
	if err != nil {
		return nil, err
	}
	if bud.truncated {
		if len(nodes) == 0 {
			nodes = []driver.FileNode{{Ext: map[string]string{"truncated": "1"}}}
		} else {
			last := &nodes[len(nodes)-1]
			if last.Ext == nil {
				last.Ext = map[string]string{}
			}
			last.Ext["truncated"] = "1"
		}
	}
	return nodes, nil
}

// parseShare 从分享链接提取 surl 与提取码。
//
// surl 口径(两个来源,均需去掉百度链接里固定前缀 "1"):
//   - https://pan.baidu.com/s/1<22位>          → 取 /s/ 后的整段,去掉首位 1
//   - https://pan.baidu.com/share/init?surl=xx → 取 surl 参数
//
// 新格式 surl 含 `_`/`-`(base64url),旧实现只认 base62 会误判(上游 issue #534)。
func parseShare(share driver.ShareLink) (string, string, error) {
	raw := strings.TrimSpace(share.URL)
	if raw == "" {
		return "", "", driver.NewErr(driver.KindNotFound, "百度分享链接为空", nil)
	}
	pwd := strings.TrimSpace(share.Pwd)
	surl := ""
	if m := surlPathRe.FindStringSubmatch(raw); m != nil {
		surl = m[1]
	} else if m := surlQueryRe.FindStringSubmatch(raw); m != nil {
		surl = m[1]
	} else {
		return "", "", driver.NewErr(driver.KindNotFound, "无法识别的百度分享链接", nil)
	}
	// /s/1 前缀:百度分享短链固定以 1 开头,surl 本身是后 22 位。
	if len(surl) > 22 && surl[0] == '1' {
		surl = surl[1:]
	}
	if pwd == "" {
		if m := pwdRe.FindStringSubmatch(raw); m != nil {
			pwd = m[1]
		}
	}
	return surl, pwd, nil
}

// verify 校验提取码,返回 randsk(即 sekey;URL 编码形态,直接参与查询与 Cookie)。
func (d *Driver) verify(ctx context.Context, surl, pwd string, cred *driver.Credential) (string, error) {
	form := url.Values{}
	form.Set("pwd", pwd)
	form.Set("vcode_str", "")
	form.Set("vcode", "")
	h := map[string]string{"referer": "https://pan.baidu.com/s/" + surl}
	if cred != nil {
		h["cookie"] = cred.Cookie
	}
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"errmsg"`
		Randsk string `json:"randsk"`
	}
	if _, err := d.call(ctx, "POST", d.base+"/share/verify?surl="+url.QueryEscape(surl), form, h, &out); err != nil {
		return "", err
	}
	if err := mustOK(out.Errno, out.ErrMsg); err != nil {
		return "", err
	}
	if out.Randsk == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "百度提取码校验响应缺少 randsk", nil)
	}
	return out.Randsk, nil
}

// ---- 目录遍历 ----

type shareWalkBudget struct {
	files     int
	dirs      int
	truncated bool
}

type shareItem struct {
	FSID           json.RawMessage `json:"fs_id"`
	IsDir          json.RawMessage `json:"isdir"`
	Path           string          `json:"path"`
	ServerFilename string          `json:"server_filename"`
	Size           flexInt64       `json:"size"`
}

// flexInt64 容忍百度把数字字段同时给成 number 或 string(xpan/share 的 size
// 实测是字符串;用 int64 直接解码会让整次解析变成 interface_changed)。
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

func rawString(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	return strings.Trim(s, `"`)
}

func (i shareItem) dir() bool { return rawString(i.IsDir) == "1" }

func (i shareItem) fsid() string { return rawString(i.FSID) }

func (i shareItem) name() string {
	if i.ServerFilename != "" {
		return i.ServerFilename
	}
	if idx := strings.LastIndex(i.Path, "/"); idx >= 0 {
		return i.Path[idx+1:]
	}
	return i.Path
}

type shareListResp struct {
	Errno   int             `json:"errno"`
	ErrMsg  string          `json:"errmsg"`
	ShareID json.RawMessage `json:"share_id"`
	UK      json.RawMessage `json:"uk"`
	List    []shareItem     `json:"list"`
	HasMore int             `json:"has_more"`
}

// listShareDir 分页列出分享内某一目录。root=true 是分享根(root=1),否则 root=0。
// 子目录必须带 BDCLND(= sekey),否则百度返回 errno=2。
func (d *Driver) listShareDir(ctx context.Context, cookie, surl, sekey, dir string, root bool) (_ []shareItem, shareID, uk string, _ error) {
	rootFlag := "0"
	if root {
		rootFlag = "1"
	}
	var all []shareItem
	for page := 1; page <= maxListPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, "", "", driver.NewErr(driver.KindUpstream, "百度分享目录遍历已取消", err)
		}
		q := url.Values{}
		q.Set("method", "list")
		q.Set("shorturl", surl)
		q.Set("page", itoa(page))
		q.Set("num", itoa(listPageSize))
		q.Set("root", rootFlag)
		q.Set("dir", dir)
		if sekey != "" {
			q.Set("sekey", sekey)
		}
		h := map[string]string{
			"cookie":  cookieHeader(cookie, sekey),
			"referer": "https://pan.baidu.com/s/" + surl,
		}
		var out shareListResp
		if _, err := d.call(ctx, "GET", d.base+"/rest/2.0/xpan/share?"+q.Encode(), nil, h, &out); err != nil {
			return nil, "", "", err
		}
		if out.Errno != 0 {
			// 无 sekey 却失败 → 多数情况是加密分享;单独提示,避免让用户白找密码。
			if sekey == "" && (out.Errno == 2 || out.Errno == -9) {
				return nil, "", "", driver.NewErr(driver.KindNotFound,
					"该分享需要提取码,或需要登录百度网盘(errno="+itoa(out.Errno)+")", nil)
			}
			return nil, "", "", mustOK(out.Errno, out.ErrMsg)
		}
		if len(out.List) == 0 && len(all) == 0 {
			return nil, "", "", driver.NewErr(driver.KindNotFound, "分享内没有可解析的文件", nil)
		}
		all = append(all, out.List...)
		if rawString(out.ShareID) != "" {
			shareID = rawString(out.ShareID)
		}
		if rawString(out.UK) != "" {
			uk = rawString(out.UK)
		}
		// 终止条件:本页不满 / 上游明确 has_more=0 / 已达单目录上界。
		if len(out.List) < listPageSize || out.HasMore == 0 {
			break
		}
	}
	return all, shareID, uk, nil
}

// walkShare 递归展开分享目录,只返回文件(名称带相对路径)。
func (d *Driver) walkShare(ctx context.Context, cookie, dir, prefix string, depth int, ext map[string]string, seen map[string]struct{}, bud *shareWalkBudget) ([]driver.FileNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "百度分享目录遍历已取消", err)
	}
	if depth > maxShareDepth || bud.files <= 0 {
		bud.truncated = true
		return nil, nil
	}
	root := dir == "/"
	entries, shareID, uk, err := d.listShareDir(ctx, cookie, ext["surl"], ext["sekey"], dir, root)
	if err != nil {
		return nil, err
	}
	if shareID != "" {
		ext["share_id"] = shareID
	}
	if uk != "" {
		ext["uk"] = uk
	}
	var nodes []driver.FileNode
	var dirs []shareItem
	for _, f := range entries {
		if f.Path == "" {
			continue
		}
		if _, ok := seen[f.Path]; ok {
			continue
		}
		seen[f.Path] = struct{}{}
		if f.dir() {
			dirs = append(dirs, f)
			continue
		}
		if bud.files <= 0 {
			bud.truncated = true
			break
		}
		bud.files--
		nodes = append(nodes, driver.FileNode{
			FID:  f.fsid(),
			Name: joinSharePath(prefix, f.name()),
			Size: int64(f.Size),
			Ext: map[string]string{
				"share_id": ext["share_id"],
				"uk":       ext["uk"],
				"sekey":    ext["sekey"],
				"surl":     ext["surl"],
				"dir":      dir,
				"path":     f.Path,
			},
		})
	}
	for _, sub := range dirs {
		if bud.dirs <= 0 || bud.files <= 0 || depth >= maxShareDepth {
			bud.truncated = true
			break
		}
		bud.dirs--
		kids, err := d.walkShare(ctx, cookie, sub.Path, joinSharePath(prefix, sub.name()), depth+1, ext, seen, bud)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, kids...)
	}
	return nodes, nil
}

func joinSharePath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}
