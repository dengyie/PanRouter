// 分享解析:ResolveShare 与目录遍历(预算上界防异常响应拖死解析)。
package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dengyie/panrouter/internal/driver"
	"net/url"
)

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
	ext := map[string]string{"pwd_id": pwdID, "stoken": st, "pwd": share.Pwd}
	bud := &shareWalkBudget{files: maxShareFiles, dirs: maxShareDirs}
	nodes, err := d.walkShare(ctx, cred, "0", "", 0, ext, map[string]struct{}{}, bud)
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

type shareWalkBudget struct {
	files     int
	dirs      int
	truncated bool
}

func joinSharePath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

func quarkFileName(f quarkFile) string {
	if f.FileName != "" {
		return f.FileName
	}
	return f.ShareName
}

// listShareDir 分页列出分享内某一目录(pdir_fid=0 为根)。
func (d *Driver) listShareDir(ctx context.Context, cred *driver.Credential, pwdID, st, pdirFID string) ([]quarkFile, error) {
	if pdirFID == "" {
		pdirFID = "0"
	}
	var files []quarkFile
	for page := 1; page <= 100; page++ {
		if err := ctx.Err(); err != nil {
			return nil, driver.NewErr(driver.KindUpstream, "分享目录遍历已取消", err)
		}
		q := url.Values{}
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
		q.Set("pwd_id", pwdID)
		q.Set("stoken", st)
		q.Set("pdir_fid", pdirFID)
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
		files = append(files, data.List...)
		if len(data.List) == 0 || (data.Metadata.Total > 0 && len(files) >= data.Metadata.Total) {
			break
		}
	}
	return files, nil
}

// walkShare 递归展开分享目录,只返回文件(名称带相对路径)。根目录是文件夹时也能一次列出内部文件。
func (d *Driver) walkShare(ctx context.Context, cred *driver.Credential, pdirFID, prefix string, depth int, ext map[string]string, seen map[string]struct{}, bud *shareWalkBudget) ([]driver.FileNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "分享目录遍历已取消", err)
	}
	if depth > maxShareDepth || bud.files <= 0 {
		bud.truncated = true
		return nil, nil
	}
	entries, err := d.listShareDir(ctx, cred, ext["pwd_id"], ext["stoken"], pdirFID)
	if err != nil {
		return nil, err
	}
	var nodes []driver.FileNode
	var dirs []quarkFile
	for _, f := range entries {
		if f.FID == "" {
			continue
		}
		if _, ok := seen[f.FID]; ok {
			continue
		}
		seen[f.FID] = struct{}{}
		if f.Dir {
			dirs = append(dirs, f)
			continue
		}
		if bud.files <= 0 {
			bud.truncated = true
			break
		}
		bud.files--
		nodeExt := map[string]string{"pwd_id": ext["pwd_id"], "stoken": ext["stoken"], "pwd": ext["pwd"]}
		nodes = append(nodes, driver.FileNode{
			FID: f.FID, Name: joinSharePath(prefix, quarkFileName(f)), Size: f.Size, IsDir: false, Ext: nodeExt,
		})
	}
	for _, dir := range dirs {
		if bud.dirs <= 0 || bud.files <= 0 || depth >= maxShareDepth {
			bud.truncated = true
			break
		}
		bud.dirs--
		kids, err := d.walkShare(ctx, cred, dir.FID, joinSharePath(prefix, quarkFileName(dir)), depth+1, ext, seen, bud)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, kids...)
	}
	return nodes, nil
}
