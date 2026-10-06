// 转存链:暂存目录 find-or-create → save → 任务轮询 → 副本清理(stoken 过期重试为常态路径)。
package quark

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dengyie/panrouter/internal/driver"
	"net/url"
	"time"
)

func (d *Driver) saveWithStokenRetry(ctx context.Context, cred *driver.Credential, pwdID string, ref driver.FileRef, tmpFID string) (string, error) {
	st := ref.Ext["stoken"]
	if st == "" {
		var err error
		if st, err = d.stoken(ctx, pwdID, ref.Ext["pwd"], cred); err != nil {
			return "", err
		}
	}
	savedFid, err := d.saveToDrive(ctx, cred, pwdID, st, ref.FID, tmpFID)
	if err != nil && stokenExpired(err) {
		var st2 string
		if st2, err = d.stoken(ctx, pwdID, ref.Ext["pwd"], cred); err != nil {
			return "", err
		}
		savedFid, err = d.saveToDrive(ctx, cred, pwdID, st2, ref.FID, tmpFID)
	}
	return savedFid, err
}

// tmpDir 确保暂存目录存在,返回其 fid(按登录态指纹缓存,目录被删则自动重建)。
// 转存集中在受控目录内,清理只需删除其中的转存副本;夸克 save 对同一文件会去重
// 返回既有 fid(实测),若转存到根目录则 cleanup 可能误删用户已有文件。
func (d *Driver) tmpDir(ctx context.Context, cred *driver.Credential) (string, error) {
	key := credFingerprint(cred)
	if fid := d.cachedTmpDir(key); fid != "" {
		dbg("tmpDir cache key=%s fid=%s", key, fid)
		return fid, nil
	}
	u := d.base + "/1/clouddrive/file?pr=ucpro&fr=pc&uc_param_str="
	body := map[string]any{"pdir_fid": "0", "file_name": tmpDirName, "dir_path": "", "dir_init_lock": false}
	var resp apiResp
	if _, err := d.call(ctx, "POST", u, body, cred, &resp); err != nil {
		return "", err
	}
	var data struct {
		FID string `json:"fid"`
	}
	if err := mustOK(&resp); err != nil {
		// 已存在同名目录时上传接口报错,退化为按名查找
		dbg("tmpDir mkdir notOK code=%d msg=%s → findTmpDir", resp.Code, resp.Message)
		fid, ferr := d.findTmpDir(ctx, cred)
		if ferr != nil {
			return "", err
		}
		dbg("tmpDir findTmpDir key=%s fid=%s", key, fid)
		d.storeTmpDir(key, fid)
		return fid, nil
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克建目录响应结构变化,解析失败", err)
	}
	if data.FID == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "夸克建目录响应缺少 fid", nil)
	}
	dbg("tmpDir mkdir-new key=%s fid=%s", key, data.FID)
	d.storeTmpDir(key, data.FID)
	return data.FID, nil
}

// findTmpDir 在根目录下按名查找暂存目录(分页扫描;file/sort 无服务端按名过滤)。
func (d *Driver) findTmpDir(ctx context.Context, cred *driver.Credential) (string, error) {
	const pageSize = 200
	const maxPages = 100 // 上界防异常响应导致无限翻页(覆盖 2 万条目)
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("pr", "ucpro")
		q.Set("fr", "pc")
		q.Set("pdir_fid", "0")
		q.Set("_page", fmt.Sprint(page))
		q.Set("_size", fmt.Sprint(pageSize))
		q.Set("_fetch_total", "1")
		q.Set("_sort", "file_type:asc,updated_at:desc")
		var resp apiResp
		if _, err := d.call(ctx, "GET", d.base+"/1/clouddrive/file/sort?"+q.Encode(), nil, cred, &resp); err != nil {
			return "", err
		}
		var data struct {
			List []struct {
				FID      string `json:"fid"`
				FileName string `json:"file_name"`
				Dir      bool   `json:"dir"`
			} `json:"list"`
		}
		if err := mustOK(&resp); err != nil {
			return "", err
		}
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return "", driver.NewErr(driver.KindInterfaceChanged, "夸克目录查询响应结构变化,解析失败", err)
		}
		for _, f := range data.List {
			if f.Dir && f.FileName == tmpDirName {
				return f.FID, nil
			}
		}
		if len(data.List) < pageSize {
			break // 末页
		}
	}
	return "", driver.NewErr(driver.KindUpstream, "夸克暂存目录创建失败,请重试", nil)
}

func (d *Driver) cachedTmpDir(key string) string {
	d.tmpMu.Lock()
	defer d.tmpMu.Unlock()
	return d.tmpDirs[key]
}

func (d *Driver) storeTmpDir(key, fid string) {
	d.tmpMu.Lock()
	d.tmpDirs[key] = fid
	d.tmpMu.Unlock()
}

func (d *Driver) forgetTmpDir(cred *driver.Credential) {
	d.tmpMu.Lock()
	delete(d.tmpDirs, credFingerprint(cred))
	d.tmpMu.Unlock()
}

// credFingerprint 用 Cookie 的 hash 区分账号,避免不同账号复用同一目录 fid。
func credFingerprint(cred *driver.Credential) string {
	h := sha1.Sum([]byte(cred.Cookie))
	return hex.EncodeToString(h[:])[:16]
}

// saveToDrive 转存分享文件到暂存目录 tmpFID,轮询任务拿到转存后的新 fid。
// 请求体为线上实测:fid_list 精确选择单文件(pdir_save_all=true 会转存整个分享,
// save_as_select_top_fids 字段实测不生效,一律 41013)。
func (d *Driver) saveToDrive(ctx context.Context, cred *driver.Credential, pwdID, stoken, fid, tmpFID string) (string, error) {
	body := map[string]any{
		"fid_list": []string{fid}, "to_pdir_fid": tmpFID, "pwd_id": pwdID, "stoken": stoken,
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
	dbg("saveToDrive fid=%s tmpFID=%s task_id=%s", fid, tmpFID, data.TaskID)
	newFid, err := d.pollSaveTask(ctx, cred, data.TaskID)
	if err != nil {
		// 任务已被上游接受:轮询中断(取消/超时/错误)会残留副本,
		// 用独立预算重新轮询拿 fid 并尽力删除。
		d.cleanupAbandonedSave(ctx, cred, data.TaskID)
		return "", err
	}
	return newFid, nil
}

// saveCleanupBudget 是副本清理重试轮询的独立预算:即使原请求被取消,
// 任务仍在服务端执行,尽力等它完成并删除副本(拿不到 fid 只能放弃)。
const saveCleanupBudget = 60 * time.Second

// cleanupAbandonedSave 在转存任务轮询中断后清理暂存副本。
// 新 fid 未知,须以独立有界 ctx 重新轮询任务成功后删除。
func (d *Driver) cleanupAbandonedSave(ctx context.Context, cred *driver.Credential, taskID string) {
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveCleanupBudget)
	defer cancel()
	fid, err := d.pollSaveTask(cleanCtx, cred, taskID)
	if err != nil || fid == "" {
		return
	}
	d.cleanupSaved(cleanCtx, cred, fid)
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
				dbg("pollSaveTask done newFid=%s (select_top)", data.SaveAs.SaveAsSelectTopFids[0])
				return data.SaveAs.SaveAsSelectTopFids[0], nil
			}
			if len(data.SaveAs.SaveAsTopFids) > 0 && data.SaveAs.SaveAsTopFids[0] != "" {
				dbg("pollSaveTask done newFid=%s (top)", data.SaveAs.SaveAsTopFids[0])
				return data.SaveAs.SaveAsTopFids[0], nil
			}
			dbg("pollSaveTask status=2 but no fid: %s", string(resp.Data))
			return "", driver.NewErr(driver.KindInterfaceChanged, "夸克转存任务完成但缺少新文件 fid", nil)
		}
	}
	return "", driver.NewErr(driver.KindUpstream, "夸克转存任务轮询超时,请稍后重试", nil)
}

// stokenExpired 识别 stoken 过期类错误(重试前提)。依赖哨兵错误而非文案。
func stokenExpired(err error) bool {
	return errors.Is(err, errStokenExpired)
}

// cleanupSaved 尽力删除暂存目录内的转存副本,避免每次解析都在网盘堆积重复文件。
// 直链已取回,删除失败仅忽略(副本残留不影响本次下载)。
func (d *Driver) cleanupSaved(ctx context.Context, cred *driver.Credential, fid string) {
	u := d.base + "/1/clouddrive/file/delete?pr=ucpro&fr=pc"
	body := map[string]any{"action_type": 2, "filelist": []string{fid}, "exclude_fids": []string{}}
	var resp apiResp
	_, _ = d.call(ctx, "POST", u, body, cred, &resp)
}
