// OpenList(AList 社区版)REST v3 线协议封装。
// 所有对 OpenList 的 HTTP 细节收敛在此文件;driver 与 transfer service 只依赖本类型。
package mangodrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// API 是带 token 的 OpenList 客户端。base 形如 https://drive.example.com 或
// http://10.0.0.3:5244(内网直连,绕开 Cloudflare 隧道 100MB 请求体上限)。
type API struct {
	client *httpx.Client
	base   string
	token  string
}

func NewAPI(c *httpx.Client, base, token string) *API {
	return &API{client: c, base: strings.TrimRight(base, "/"), token: token}
}

// Obj 是 /api/fs/get、/api/fs/list 返回的条目。
type Obj struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
	Sign     string `json:"sign"`
	RawURL   string `json:"raw_url"`
	Provider string `json:"provider"`
}

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// classifyMsg 把 OpenList 文案映射到契约错误类别。
func classifyMsg(msg string) *driver.Error {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "token is invalidated"), strings.Contains(m, "token is expired"),
		strings.Contains(m, "not logged in"), strings.Contains(m, "unauthorized"),
		strings.Contains(m, "无效的令牌"), strings.Contains(m, "登录"):
		return driver.NewErr(driver.KindAuthExpired, "Mango Drive 凭据已失效,请到账号页更新", nil)
	case strings.Contains(m, "not found"), strings.Contains(m, "object not exist"),
		strings.Contains(m, "not exist"), strings.Contains(m, "不存在"):
		return driver.NewErr(driver.KindNotFound, "Mango Drive 路径不存在:"+msg, nil)
	case strings.Contains(m, "limit"), strings.Contains(m, "too many"), strings.Contains(m, "频繁"):
		return driver.NewErr(driver.KindRiskControl, "Mango Drive 限流:"+msg, nil)
	default:
		return driver.NewErr(driver.KindUpstream, "Mango Drive 接口异常:"+msg, nil)
	}
}

// do 发送 JSON 请求并校验业务码;body 为 nil 时发空体。
func (a *API) do(ctx context.Context, method, endpoint string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+endpoint, rd)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "构造请求失败", err)
	}
	req.Header.Set("User-Agent", BrowserUA)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", a.token)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "Mango Drive 网络请求失败", err)
	}
	var resp apiResp
	if len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, &resp); err != nil {
			return driver.NewErr(driver.KindInterfaceChanged, "Mango Drive 响应结构变化,解析失败", err)
		}
	}
	if res.StatusCode == http.StatusUnauthorized {
		return driver.NewErr(driver.KindAuthExpired, "Mango Drive 凭据已失效,请到账号页更新", nil)
	}
	if res.StatusCode >= 400 && resp.Message == "" {
		return driver.NewErr(driver.KindUpstream, fmt.Sprintf("Mango Drive 响应异常:%d", res.StatusCode), nil)
	}
	if resp.Code != http.StatusOK {
		msg := resp.Message
		if msg == "" {
			msg = strconv.Itoa(resp.Code)
		}
		return classifyMsg(msg)
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return driver.NewErr(driver.KindInterfaceChanged, "Mango Drive 响应结构变化,解析失败", err)
		}
	}
	return nil
}

// Login 用用户名密码换 token(供账号引导与凭据校验使用)。
func (a *API) Login(ctx context.Context, username, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := a.do(ctx, http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": password}, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", driver.NewErr(driver.KindInterfaceChanged, "Mango Drive 登录响应缺少 token", nil)
	}
	return out.Token, nil
}

// Me 校验当前 token 是否有效,返回用户名。
func (a *API) Me(ctx context.Context) (string, error) {
	var out struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
	}
	if err := a.do(ctx, http.MethodGet, "/api/me", nil, &out); err != nil {
		return "", err
	}
	return out.Username, nil
}

// Get 读取单个路径的元数据(文件含 raw_url;目录 is_dir=true)。
func (a *API) Get(ctx context.Context, p string) (*Obj, error) {
	var out Obj
	if err := a.do(ctx, http.MethodPost, "/api/fs/get",
		map[string]any{"path": p, "password": ""}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List 列目录(自动翻页,上界 maxListPages)。
//
// 终止条件有两个,都必须成立才早停:
//   - 本页不满 per_page → 已是最后一页(上游返回不足一页即到底);
//   - total > 0 且已取条数 >= total → 已取完上游声明的总数。
//
// ★ total 的坑:部分 OpenList 版本/接口形态**不返回 total**(字段缺省 → 0)。
//   原实现写 `page*listPageSize >= out.Total`,total=0 时该式恒真
//   (200 >= 0) → 恰好取满一页就停 → 目录超过 per_page(200)时**静默截断**,
//   后面的文件对客户端完全不可见(表现为「文件夹里少了文件」且无任何报错)。
//   故 total 只在 > 0 时作为终止判据;total 缺失时只能靠「本页不满」判断到底。
func (a *API) List(ctx context.Context, p string) ([]Obj, error) {
	var all []Obj
	for page := 1; page <= maxListPages; page++ {
		var out struct {
			Content []Obj `json:"content"`
			Total   int   `json:"total"`
		}
		err := a.do(ctx, http.MethodPost, "/api/fs/list", map[string]any{
			"path": p, "password": "", "page": page, "per_page": listPageSize, "refresh": false,
		}, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Content...)
		lastPage := len(out.Content) < listPageSize
		exhausted := out.Total > 0 && page*listPageSize >= out.Total
		if lastPage || exhausted {
			break
		}
	}
	return all, nil
}

// Mkdir 创建目录(已存在视为成功)。
func (a *API) Mkdir(ctx context.Context, p string) error {
	err := a.do(ctx, http.MethodPost, "/api/fs/mkdir", map[string]string{"path": p}, nil)
	if err != nil {
		var de *driver.Error
		if errors.As(err, &de) && de.Kind == driver.KindUpstream {
			return nil // 已存在等幂等场景,交给后续 Put 报真实错误
		}
		return err
	}
	return nil
}

// Put 流式上传:PUT /api/fs/put,目标路径放在 File-Path 头(URL 编码)。
// 不做整文件缓冲,内存占用与文件大小无关。
func (a *API) Put(ctx context.Context, p string, size int64, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.base+"/api/fs/put", r)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "构造上传请求失败", err)
	}
	req.Header.Set("User-Agent", BrowserUA)
	req.Header.Set("Authorization", a.token)
	req.Header.Set("File-Path", url.PathEscape(p))
	req.Header.Set("As-Task", "false")
	req.Header.Set("Content-Type", "application/octet-stream")
	// 上游长度未知(如 chunked)时保持 -1:走 chunked PUT,不设虚假 Content-Length。
	if size > 0 {
		req.ContentLength = size
	}
	res, err := a.client.Do(req)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "Mango Drive 上传失败", err)
	}
	var resp apiResp
	// ★ 200 + 空 body 不是成功:OpenList 的 fs/put 正常必回 {"code":200,...}。
	//   空 body 意味着代理截断/协议异常,此时 resp.Code 恒为 0,若按「code==0
	//   即成功」处理,转存会报 done 而远端**没有文件**(静默丢文件)。
	if len(res.Body) == 0 {
		return driver.NewErr(driver.KindUpstream,
			fmt.Sprintf("Mango Drive 上传响应为空(HTTP %d),无法确认结果", res.StatusCode), nil)
	}
	if err := json.Unmarshal(res.Body, &resp); err != nil {
		return driver.NewErr(driver.KindInterfaceChanged, "Mango Drive 上传响应结构变化", err)
	}
	if res.StatusCode >= 400 || (resp.Code != 0 && resp.Code != http.StatusOK) {
		msg := resp.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return classifyMsg(msg)
	}
	return nil
}

// OfflineTask 是离线下载任务。
type OfflineTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    int    `json:"state"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Error    string `json:"error"`
}

// 离线下载任务状态码(OpenList task 模型;与 /api/task/offline_download/done
// 实测一致)。转存层据此判断「远端不可能再产出产物」,避免空等确认窗口。
const (
	OfflineTaskStatePending   = 0
	OfflineTaskStateRunning   = 1
	OfflineTaskStateSucceeded = 2 // "offline download completed, maybe transferring"
	OfflineTaskStateCancelled = 3
	OfflineTaskStateError     = 4 // 不再推进,产物不会出现
)

// Failed 报告远端任务是否已进入「不会再产出文件」的终态。
// Error 单独判:上游可能先填 Error 再推进 state,只看其一都会漏。
func (t OfflineTask) Failed() bool {
	return t.Error != "" ||
		t.State == OfflineTaskStateCancelled || t.State == OfflineTaskStateError
}

// Done 报告远端是否已把文件传完(完成态:产物应已可见,可与落池确认竞速)。
func (t OfflineTask) Done() bool {
	return t.State == OfflineTaskStateSucceeded && t.Error == ""
}

// OfflineTasks 读取远端离线下载队列(/api/task/offline_download/undone):尚未
// 完成的 token 及其状态。入参 token 为空时取全部。OpenList 归属表随任务落库,
// 重启后 undone 仍可回读,是「远端任务是否还活着」的唯一事实源。
func (a *API) OfflineTasks(ctx context.Context, token string) ([]OfflineTask, error) {
	var out []OfflineTask
	if err := a.do(ctx, http.MethodGet, "/api/task/offline_download/undone", nil, &out); err != nil {
		return nil, err
	}
	if token == "" {
		return out, nil
	}
	for _, t := range out {
		if t.ID == token {
			return []OfflineTask{t}, nil
		}
	}
	return nil, nil
}

// OfflineTaskByToken 查询单个离线任务当前状态;未找到返回 (nil, nil)
// ——「查不到」不是错误(任务可能已被清理),调用方不应据此判失败。
func (a *API) OfflineTaskByToken(ctx context.Context, token string) (*OfflineTask, error) {
	tasks, err := a.OfflineTasks(ctx, token)
	if err != nil || len(tasks) > 0 {
		if err != nil {
			return nil, err
		}
		return &tasks[0], nil
	}
	return nil, nil
}

// 仅适用于不需要额外请求头的直链;需要 UA/Cookie/Referer 的源走 Put 通道。
func (a *API) AddOffline(ctx context.Context, p string, urls []string) ([]OfflineTask, error) {
	var out struct {
		Tasks []OfflineTask `json:"tasks"`
	}
	err := a.do(ctx, http.MethodPost, "/api/fs/add_offline_download", map[string]any{
		"path": p, "urls": urls, "tool": "aria2", "delete_policy": "delete_on_upload_succeed",
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// Ping 探测服务活性(健康检查/凭据校验的轻量入口)。
func (a *API) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/ping", nil)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "构造请求失败", err)
	}
	req.Header.Set("User-Agent", BrowserUA)
	res, err := a.client.Do(req)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "Mango Drive 网络请求失败", err)
	}
	if res.StatusCode >= 400 {
		return driver.NewErr(driver.KindUpstream, fmt.Sprintf("Mango Drive 响应异常:%d", res.StatusCode), nil)
	}
	return nil
}

// WithTimeout 返回带总超时的 ctx 包装(上传等长任务用调用方自带预算)。
func WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
