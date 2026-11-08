// Transfer:云对云转存——把 PanRouter 里解析出的直链流式搬进私有云盘(Mango Drive)。
//
// 与 /stream 中继的区别:不经过客户端,服务端直连两端,适合大文件搬运。
// 与 OpenList 离线下载的区别:离线由远端 aria2 拉取,不占本机带宽,但无法携带
// UA/Referer/Cookie 等提取链约束,也拿不到字节级进度——两条通道由本服务按需选择。
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/driver/mangodrive"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
)

// TransferChannel 是转存通道。
type TransferChannel string

const (
	// ChannelStream 服务端拉上游 → 流式 PUT 到 OpenList。兼容任意提取链约束。
	ChannelStream TransferChannel = "stream"
	// ChannelOffline 调 OpenList 离线下载(aria2)由远端直拉。省本机带宽,仅公开直链可用。
	ChannelOffline TransferChannel = "offline"
	// ChannelAuto 有提取链约束走 stream,否则走 offline。
	ChannelAuto TransferChannel = "auto"
)

// defaultMaxTransferBytes 单文件转存上限:64GiB(0 = 不限)。
const defaultMaxTransferBytes = 64 << 30

// 已完成任务在内存中保留的条数与时长,供 UI 回看进度。
const (
	keepTasks     = 200
	keepTaskAfter = 24 * time.Hour
)

type Transfer struct {
	cfg      *config.Provider
	resolver *Resolver
	store    *repo.Store
	aes      aesCipher
	met      *metrics.Registry
	log      *loggerType

	upstream map[string]*httpx.Client // pan → 拉取上游直链的客户端(复用 Relay 的)
	drive    *httpx.Client            // 调 OpenList:内网地址,须 AllowPrivate
	seq      int64

	mu    sync.RWMutex
	tasks map[string]*TransferTask
}

// NewTransfer 装配转存编排。upstream 为 Relay 的 per-pan 客户端(带各自重定向白名单);
// drive 仅用于探测 Mango Drive 端点,内网地址需要 AllowPrivate。
func NewTransfer(cfg *config.Provider, resolver *Resolver, upstream map[string]*httpx.Client,
	store *repo.Store, aes aesCipher, met *metrics.Registry, log *loggerType) (*Transfer, error) {
	dcl, err := httpx.New(httpx.Options{
		Timeout: 20 * time.Second, AllowPrivate: true, NoBodyTimeout: true,
		Proxy: cfg.Get().Transfer.Proxy,
	})
	if err != nil {
		return nil, fmt.Errorf("transfer client: %w", err)
	}
	return &Transfer{cfg: cfg, resolver: resolver, upstream: upstream, drive: dcl,
		store: store, aes: aes, met: met, log: log, tasks: map[string]*TransferTask{}}, nil
}

// TransferRequest 是一次转存请求。SourceURL 可以是任意受支持网盘的分享链接。
type TransferRequest struct {
	SourceURL string `json:"source_url"`
	Pwd       string `json:"pwd"`
	FID       string `json:"fid"`
	// DestDir 目标目录(OpenList 路径);空 = 使用 transfer.dest_dir 默认目录。
	DestDir string `json:"dest_dir"`
	// Name 目标文件名;空 = 沿用源文件名。
	Name    string          `json:"name"`
	Channel TransferChannel `json:"channel"`
	// UA 调用方 UA:决定提取链约束与 302 可行性。
	UA string `json:"ua"`
}

// TransferTask 是转存任务的可观测快照。
type TransferTask struct {
	ID        string    `json:"id"`
	State     string    `json:"state"` // queued|running|done|failed
	Channel   string    `json:"channel"`
	SourceURL string    `json:"source_url"`
	DestPath  string    `json:"dest_path"`
	FileName  string    `json:"file_name"`
	Size      int64     `json:"size"`
	Done      int64     `json:"done"`
	Error     string    `json:"error,omitempty"`
	Hint      string    `json:"hint,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	// 运行期字段(不序列化)
	mu      sync.Mutex
	met     *metrics.Registry
	log     *loggerType
	done    int64
	destDir string
}

func (t *TransferTask) set(delta int64) {
	atomic.AddInt64(&t.done, delta)
}

func (t *TransferTask) finish(state, errMsg, hint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.State = state
	t.Error = errMsg
	t.Hint = hint
	t.EndedAt = time.Now()
	// 成功即 Done=Size:done 是搬运计数,终值不落会让轮询永远停在 0%。
	// Size 在此覆盖为实际字节(解析期登记的可能是 0 或过期值)。
	if state == "done" {
		if n := atomic.LoadInt64(&t.done); n > 0 {
			t.Size = n
		}
	}
	if t.met != nil {
		t.met.Inc("panrouter_transfer_total", map[string]string{"state": state, "channel": t.Channel})
	}
}

// Snapshot 返回可序列化的当前状态(进度原子读取;不复制锁)。
func (t *TransferTask) Snapshot() TransferTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TransferTask{
		ID:        t.ID,
		State:     t.State,
		Channel:   t.Channel,
		SourceURL: t.SourceURL,
		DestPath:  t.DestPath,
		FileName:  t.FileName,
		Size:      t.Size,
		Done:      atomic.LoadInt64(&t.done),
		Error:     t.Error,
		Hint:      t.Hint,
		StartedAt: t.StartedAt,
		EndedAt:   t.EndedAt,
	}
}

// destDefault 默认落盘目录。
func (s *Transfer) destDefault() string {
	if d := s.cfg.Get().Transfer.DestDir; strings.TrimSpace(d) != "" {
		return cleanStorePath(d)
	}
	return "/pool/panrouter"
}

func (s *Transfer) driveAPI() *mangodrive.API {
	c := s.cfg.Get().Transfer
	return mangodrive.NewAPI(s.drive, c.Endpoint, c.Token)
}

// Get 返回任务快照;不存在返回 nil。
func (s *Transfer) Get(id string) *TransferTask {
	s.mu.RLock()
	t := s.tasks[id]
	s.mu.RUnlock()
	if t == nil {
		return nil
	}
	c := t.Snapshot()
	return &c
}

// List 返回全部任务快照(新→旧)。
func (s *Transfer) List() []TransferTask {
	s.mu.RLock()
	all := make([]*TransferTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		all = append(all, t)
	}
	s.mu.RUnlock()
	out := make([]TransferTask, 0, len(all))
	for _, t := range all {
		out = append(out, t.Snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Submit 校验入参并登记后台任务;校验失败同步返回 driver.Error(客户端拿 4xx)。
// 任务在独立 ctx 里跑:浏览器断开不应中断已受理的搬运。
func (s *Transfer) Submit(req TransferRequest) (*TransferTask, error) {
	req.SourceURL = strings.TrimSpace(req.SourceURL)
	if req.SourceURL == "" {
		return nil, driver.NewErr(driver.KindUnsupported, "source_url 必填", nil)
	}
	if !strings.HasPrefix(req.SourceURL, "http://") && !strings.HasPrefix(req.SourceURL, "https://") {
		return nil, driver.NewErr(driver.KindUnsupported, "仅支持 http(s) 链接", nil)
	}
	cfg := s.cfg.Get()
	if cfg.Transfer.Endpoint == "" || cfg.Transfer.Token == "" {
		return nil, driver.NewErr(driver.KindUnsupported, "Mango Drive 未配置(transfer.endpoint / transfer.token)", nil)
	}
	channel := req.Channel
	if channel == "" {
		channel = ChannelAuto
	}
	switch channel {
	case ChannelAuto, ChannelStream, ChannelOffline:
	default:
		return nil, driver.NewErr(driver.KindUnsupported, "未知转存通道:"+string(channel), nil)
	}
	dest := s.destDefault()
	if d := strings.TrimSpace(req.DestDir); d != "" {
		dest = cleanStorePath(d)
	}
	task := &TransferTask{
		ID:        "t" + time.Now().Format("0206150405") + "-" + strconv.Itoa(int(atomic.AddInt64(&s.seq, 1))),
		State:     "queued",
		Channel:   string(channel),
		SourceURL: req.SourceURL,
		StartedAt: time.Now(),
		met:       s.met,
		log:       s.log,
	}
	task.destDir = dest
	// 先登记再异步执行:客户端立刻能查到任务。
	s.mu.Lock()
	s.tasks[task.ID] = task
	s.pruneLocked()
	s.mu.Unlock()

	go func() {
		rctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// 后台任务已过管理员鉴权(路由挂在 auth 组内):显式打标,
		// 否则 resolver 按游客处理,不读账号 Cookie → auth_expired。
		rctx = WithAuthed(rctx, true)
		if err := s.Run(rctx, task, req); err != nil {
			task.finish("failed", err.Error(), hintOf(err))
			s.log.Warnw("transfer failed", "id", task.ID, "err", err)
			return
		}
		task.finish("done", "", "")
		s.log.Infow("transfer done", "id", task.ID, "dest_path", task.DestPath, "size", task.Size)
	}()
	return task, nil
}

// pruneLocked 清理过期/超量的终结任务(调用方持有 s.mu)。
func (s *Transfer) pruneLocked() {
	now := time.Now()
	var terminal []*TransferTask
	for _, t := range s.tasks {
		t.mu.Lock()
		done := t.State == "done" || t.State == "failed"
		old := done && !t.EndedAt.IsZero() && now.Sub(t.EndedAt) > keepTaskAfter
		t.mu.Unlock()
		if old {
			delete(s.tasks, t.ID)
			continue
		}
		if done {
			terminal = append(terminal, t)
		}
	}
	if len(terminal) <= keepTasks {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].EndedAt.After(terminal[j].EndedAt)
	})
	for i := keepTasks; i < len(terminal); i++ {
		delete(s.tasks, terminal[i].ID)
	}
}

// hintOf 取 driver.Error 的分类作为 hint,其余留空。
func hintOf(err error) string {
	var de *driver.Error
	if errors.As(err, &de) {
		return string(de.Kind)
	}
	return ""
}

// Run 执行一次已登记的转存任务。Submit 在 goroutine 里调用;
// 测试可直接同步调用以断言结果。
func (s *Transfer) Run(ctx context.Context, task *TransferTask, req TransferRequest) error {
	task.mu.Lock()
	task.State = "running"
	task.mu.Unlock()

	src, err := s.resolveSource(ctx, req)
	if err != nil {
		return err
	}
	destDir := task.destDir
	destPath := destDir + "/" + src.name
	if destDir == "/" {
		destPath = "/" + src.name
	}
	task.mu.Lock()
	task.FileName = src.name
	task.Size = src.size
	task.DestPath = destPath
	task.mu.Unlock()

	channel := req.Channel
	if channel == "" || channel == ChannelAuto {
		channel = ChannelStream
		if !src.needHdr && !src.meta.BindIP && src.size < 8<<30 {
			channel = ChannelOffline
		}
	}
	task.mu.Lock()
	task.Channel = string(channel)
	task.mu.Unlock()

	switch channel {
	case ChannelOffline:
		return s.transferOffline(ctx, task, src, destPath)
	default:
		return s.transferStream(ctx, task, src, destPath)
	}
}

// ---- 执行 ----

type transferSource struct {
	file *ResolveFileResult
	meta LinkMeta
	name string
	size int64
	// serverURL 服务端自取直链;空 = 用 file.DirectURL(公网直链,给客户端用)。
	serverURL string
	upstream  string
	needHdr   bool
}

// resolveSource 把分享链接解析成直链 + 提取链约束。
// FID 为空时先展开分享取第一个文件:目录分享的批量转存由客户端逐个提交(可观测、可重试)。
func (s *Transfer) resolveSource(ctx context.Context, req TransferRequest) (*transferSource, error) {
	ua := req.UA
	if ua == "" {
		ua = mangodrive.BrowserUA
	}
	fid := req.FID
	pan := ""
	if fid == "" {
		sh, err := s.resolver.ResolveShare(ctx, req.SourceURL, req.Pwd, ua)
		if err != nil {
			return nil, err
		}
		pan = sh.Pan
		for _, f := range sh.Files {
			if f.FID != "" && !f.IsDir {
				fid = f.FID
				break
			}
		}
		if fid == "" {
			return nil, driver.NewErr(driver.KindUnsupported, "分享中没有可转存的文件", nil)
		}
	}
	res, err := s.resolver.ResolveFile(ctx, req.SourceURL, req.Pwd, fid, false, ua)
	if err != nil {
		return nil, err
	}
	if pan == "" {
		pan = res.Pan
	}
	// 用 GetFreshLink 拿权威直链:过期的缓存会被 singleflight 续签,
	// 且 Cookie 在此解密——缺了它夸克一类直链会 403。
	link, err := s.resolver.GetFreshLink(ctx, res.ShareKey, res.FID, ua)
	if err != nil {
		return nil, err
	}
	cookie, err := s.aes.DecryptBytes(link.CookieEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt link cookie: %w", err)
	}
	name := req.Name
	if name == "" {
		name = link.FileName
	}
	if name == "" {
		name = res.FileName
	}
	return &transferSource{
		file:      res,
		meta:      LinkMeta{UA: link.UA, Referer: link.Referer, Cookie: string(cookie), BindIP: link.BindIP},
		name:      cleanName(name),
		size:      link.Size,
		serverURL: link.ServerLink,
		upstream:  pan,
		needHdr:   NeedHeaders(link.UA, link.Referer, string(cookie), ua),
	}, nil
}

// cleanName 去掉路径成分与 OpenList 不接受的控制字符。
func cleanName(n string) string {
	n = strings.TrimSpace(strings.ReplaceAll(n, "\\", "/"))
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	n = strings.Trim(n, ". ")
	var b strings.Builder
	for _, r := range n {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "transfer_" + time.Now().Format("20060102_150405")
	}
	return out
}

// transferStream 本机拉上游 → 边读边 PUT 到 OpenList。
// io.Pipe 串流:内存占用与文件大小无关;Range 不参与,整文件一次流。
func (s *Transfer) transferStream(ctx context.Context, task *TransferTask, src *transferSource, destPath string) error {
	up := s.upstream[src.upstream]
	if up == nil {
		return driver.NewErr(driver.KindUnsupported, "网盘 "+src.upstream+" 未启用中转客户端", nil)
	}
	// 服务端自取优先走 driver 给的内网直链:私有盘公网域名可能不可回源
	// (Cloudflare 隧道 100MB 限制/拒绝回源),内网直连才是可靠出口。
	fetchURL := src.serverURL
	if fetchURL == "" {
		fetchURL = src.file.DirectURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "构造上游请求失败", err)
	}
	if src.meta.UA != "" {
		req.Header.Set("User-Agent", src.meta.UA)
	}
	if src.meta.Referer != "" {
		req.Header.Set("Referer", src.meta.Referer)
	}
	if src.meta.Cookie != "" {
		req.Header.Set("Cookie", src.meta.Cookie)
	}
	resp, err := up.DoStream(req)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "上游直链请求失败", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return driver.NewErr(driver.KindUpstream, "上游直链返回 "+strconv.Itoa(resp.StatusCode), nil)
	}
	// 长度以实际上游响应头为准:解析时登记的大小可能与真实字节不符(转码/分包),
	// 未知则按 -1 走 chunked,绝不伪造 Content-Length 让 PUT 断流。
	size := resp.ContentLength
	if size <= 0 {
		size = 0
	}
	if limit := s.maxTransferBytes(); limit > 0 && size > limit {
		return driver.NewErr(driver.KindUnsupported, "文件超过转存上限:"+strconv.FormatInt(limit, 10)+" 字节", nil)
	}

	pr, pw := io.Pipe()
	go func() {
		_, cErr := io.Copy(pw, &progressReader{r: resp.Body, task: task})
		pw.CloseWithError(cErr)
	}()
	if err := s.driveAPI().Put(ctx, destPath, size, pr); err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	if done := atomic.LoadInt64(&task.done); size > 0 && done != size {
		return driver.NewErr(driver.KindUpstream,
			"上传字节数不一致:期望 "+strconv.FormatInt(size, 10)+" 实收 "+strconv.FormatInt(done, 10), nil)
	}
	return nil
}

// maxTransferBytes 单文件上限;配置为负视为不限。
func (s *Transfer) maxTransferBytes() int64 {
	n := s.cfg.Get().Transfer.MaxBytes
	switch {
	case n < 0:
		return 0
	case n == 0:
		return defaultMaxTransferBytes
	default:
		return n
	}
}

// progressReader 统计已搬运字节(经 TransferTask.done 原子共享给快照)。
type progressReader struct {
	r    io.Reader
	task *TransferTask
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.task.set(int64(n))
	}
	return n, err
}

// transferOffline 提交 OpenList 离线下载(远端 aria2 直拉)。
// 仅接受无需额外请求头的公开直链:SSRF 面从"本机带白名单跳转"扩大为
// "远端任意拉取",源 host 必须先落进 aria2 的 http 链接白名单。
func (s *Transfer) transferOffline(ctx context.Context, task *TransferTask, src *transferSource, destPath string) error {
	if src.needHdr {
		return driver.NewErr(driver.KindUnsupported,
			"该直链需要 UA/Referer/Cookie,离线通道无法携带,请改用 stream 通道", nil)
	}
	dir := pathDir(destPath)
	if err := s.driveAPI().Mkdir(ctx, dir); err != nil {
		return err
	}
	// OpenList 离线端点用内网直链:aria2 与本机同网络,无需绕 Cloudflare。
	offlineURL := src.serverURL
	if offlineURL == "" {
		offlineURL = src.file.DirectURL
	}
	tasks, err := s.driveAPI().AddOffline(ctx, dir, []string{offlineURL})
	if err != nil {
		return err
	}
	// OpenList 离线端点异步建任务,成功时空响应体亦属正常;任务由远端队列接管。
	if len(tasks) > 0 && task != nil {
		task.log.Infow("offline task created", "id", task.ID, "remote", tasks[0].ID, "url", offlineURL)
	}
	task.set(src.size)
	return nil
}

// cleanStorePath 归一化 OpenList 内的绝对路径。
func cleanStorePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// pathDir 取父目录。
func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}
