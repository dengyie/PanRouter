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
	"net/url"
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

// 转存编排的默认值:全部可被 transfer 配置段覆盖(见 config.Transfer)。
const (
	// defaultMaxTransferBytes 单文件转存上限:64GiB。
	defaultMaxTransferBytes = 64 << 30
	// defaultAutoStreamBytes channel=auto 的大小阈值:达到该值走 stream(本机中转)。
	defaultAutoStreamBytes = 8 << 30
	// defaultKeepTasks / defaultKeepTaskAfter 内存里保留的终结任务条数与时长。
	defaultKeepTasks     = 200
	defaultKeepTaskAfter = 24 * time.Hour
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

	mu         sync.RWMutex
	tasks      map[string]*TransferTask
	activeDest map[string]string // destPath → 占用它的任务 id(并发同名写守卫)
}

// NewTransfer 装配转存编排。upstream 为 Relay 的 per-pan 客户端(带各自重定向白名单);
// drive 仅用于探测 Mango Drive 端点,内网地址需要 AllowPrivate。
// FIDDirect 是直链直传的 FID 约定值:SourceURL 即任意公网 http(s) 直链,
// 不经网盘解析,直接进转存管线(stream 由服务端拉,offline 由远端 aria2 拉)。
const FIDDirect = "direct"

func NewTransfer(cfg *config.Provider, resolver *Resolver, upstream map[string]*httpx.Client,
	store *repo.Store, aes aesCipher, met *metrics.Registry, log *loggerType) (*Transfer, error) {
	dcl, err := httpx.New(httpx.Options{
		Timeout: 20 * time.Second, AllowPrivate: true, NoBodyTimeout: true,
		Proxy: cfg.Get().Transfer.Proxy,
	})
	if err != nil {
		return nil, fmt.Errorf("transfer client: %w", err)
	}
	// direct 客户端:直链直传专用。禁止私网(拨号级 + 每跳重定向 DNS 级双保险),
	// 公网跨域 302 放行(github release → objects.githubusercontent.com 一类);
	// 无总超时,响应头超时仍受 Timeout 约束。AllowPrivate 仅供测试 fixture 打开。
	// ForceHTTP1:GitHub 等站点对数据中心 IP 的 h2 下载型 GET 会 tar-pit(挂起
	// 不回响应头,生产实测),HTTP/1.1 实测可用;单流大文件也用不到 h2 多路复用。
	direct, err := httpx.New(httpx.Options{
		Timeout: 30 * time.Second, AllowPrivate: cfg.Get().Transfer.DirectAllowPrivate,
		RedirectAllow: []string{"*"}, NoBodyTimeout: true, ForceHTTP1: true,
	})
	if err != nil {
		return nil, fmt.Errorf("direct transfer client: %w", err)
	}
	ups := make(map[string]*httpx.Client, len(upstream)+1)
	for k, v := range upstream {
		ups[k] = v
	}
	ups["direct"] = direct
	return &Transfer{cfg: cfg, resolver: resolver, upstream: ups, drive: dcl,
		store: store, aes: aes, met: met, log: log,
		tasks: map[string]*TransferTask{}, activeDest: map[string]string{}}, nil
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
	// Client 可选的调用方标识(如 desktop/v0.1.11),只进任务快照便于服务端排查。
	Client string `json:"client,omitempty"`
}

// 任务状态。offline 通道的 done 不在提交成功时给:远端 aria2 刚建任务,
// 字节还在路上,此时报 done 是假完成(同路径会被锁到落池确认前,产物也可能
// 永远不出现)。受理成功后任务停在 pending,由 watchOfflineArtifact 观测到
// 产物才收口 done(真实落盘大小),确认窗口内未落池收口 failed。
const (
	stateQueued  = "queued"
	stateRunning = "running"
	// statePending offline 端点已受理,产物尚未落池确认;写守卫仍占用目标路径。
	statePending = "pending"
	stateDone    = "done"
	stateFailed  = "failed"
)

// TransferTask 是转存任务的可观测快照。
type TransferTask struct {
	ID        string    `json:"id"`
	State     string    `json:"state"` // queued|running|pending|done|failed
	Channel   string    `json:"channel"`
	SourceURL string    `json:"source_url"`
	DestPath  string    `json:"dest_path"`
	FileName  string    `json:"file_name"`
	Size      int64     `json:"size"`
	Done      int64     `json:"done"`
	Error     string    `json:"error,omitempty"`
	Hint      string    `json:"hint,omitempty"`
	Client    string    `json:"client,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	// 运行期字段(不序列化)
	mu      sync.Mutex
	met     *metrics.Registry
	log     *loggerType
	done    int64
	destDir string
	// offlineToken 远端离线任务 id(OpenList undones 表的 token)。仅由确认器在
	// t.mu 内读改写;空 = 远端未返回 id 或已确认/放弃确认。
	offlineToken string
}

func (t *TransferTask) set(delta int64) {
	atomic.AddInt64(&t.done, delta)
}

// finishDone 收口成功。size>0 表示已从产物确认真实大小(覆盖进度计数);
// finishDone 收口成功。size>0 表示已从产物确认真实大小(覆盖进度计数);
// size 为 0 走 io.Pipe 流式的既有口径,用搬运计数。
func (t *TransferTask) finishDone(size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.State = stateDone
	t.Error = ""
	t.Hint = ""
	t.EndedAt = time.Now()
	if size > 0 {
		atomic.StoreInt64(&t.done, size)
		t.Size = size
		t.Done = size
	} else if n := atomic.LoadInt64(&t.done); n > 0 {
		t.Size = n
		t.Done = n
	}
	if t.met != nil {
		t.met.Inc("panrouter_transfer_total", map[string]string{"state": stateDone, "channel": t.Channel})
	}
}

// markPending 标记 offline 端点已受理、产物尚未落池:任务停在 pending,
// 由确认器收口成 done(真实落盘大小)或 failed(确认窗口内未落池)。
// 必须在启动确认器之前调用,否则确认器可能先收口 done 再被压回 pending。
func (t *TransferTask) markPending() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.State = statePending
	t.Error = ""
	t.Hint = ""
	t.EndedAt = time.Time{}
}

// finishFailed 收口失败。
func (t *TransferTask) finishFailed(errMsg, hint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.State = stateFailed
	t.Error = errMsg
	t.Hint = hint
	t.EndedAt = time.Now()
	if t.met != nil {
		t.met.Inc("panrouter_transfer_total", map[string]string{"state": stateFailed, "channel": t.Channel})
	}
}

// Snapshot 返回可序列化的当前状态(进度原子读取;不复制锁)。
// 幂等:快照可能被再次快照(Get → handler),copy 里没有原子计数器,
// 进度靠 Done 字段传递,否则第二次快照会把 done 读回 0。
func (t *TransferTask) Snapshot() TransferTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	done := atomic.LoadInt64(&t.done)
	if t.Done > done {
		done = t.Done
	}
	return TransferTask{
		ID:        t.ID,
		State:     t.State,
		Channel:   t.Channel,
		SourceURL: t.SourceURL,
		DestPath:  t.DestPath,
		FileName:  t.FileName,
		Size:      t.Size,
		Done:      done,
		Error:     t.Error,
		Hint:      t.Hint,
		Client:    t.Client,
		StartedAt: t.StartedAt,
		EndedAt:   t.EndedAt,
	}
}

// DefaultDest 对外暴露的默认落盘目录(供 storage 信息接口)。
func (s *Transfer) DefaultDest() string { return s.destDefault() }

// destDefault 默认落盘目录;未配置时为端点根目录(由 OpenList 自身决定归属),
// 桌面端想让转存落在某个子目录时应显式传 dest_dir 或配置 transfer.dest_dir。
func (s *Transfer) destDefault() string {
	if d := s.cfg.Get().Transfer.DestDir; strings.TrimSpace(d) != "" {
		return cleanStorePath(d)
	}
	return "/"
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
		State:     stateQueued,
		Channel:   string(channel),
		SourceURL: req.SourceURL,
		Client:    clientLabel(req.Client),
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
			task.finishFailed(err.Error(), hintOf(err))
			s.log.Warnw("transfer failed", "id", task.ID, "err", err)
			return
		}
		// offline 的状态由确认器在 Run 内收口(pending / done / failed),
		// 这里不再碰任务状态,只打日志。确认器在 Run 返回前已启动,可能已经
		// 改写 DestPath(Content-Disposition 改名对账),所以日志字段必须在锁内取。
		task.mu.Lock()
		ch := task.Channel
		destPath := task.DestPath
		task.mu.Unlock()
		if ch == string(ChannelOffline) {
			s.log.Infow("offline accepted, awaiting artifact", "id", task.ID, "dest_path", destPath)
			return
		}
		task.finishDone(0)
		snap := task.Snapshot()
		s.log.Infow("transfer done", "id", task.ID, "dest_path", snap.DestPath, "size", snap.Size)
	}()
	return task, nil
}

// pruneLocked 清理过期/超量的终结任务(调用方持有 s.mu)。
func (s *Transfer) pruneLocked() {
	now := time.Now()
	keepTasks, keepTaskAfter := s.keepPolicy()
	// 终结任务的 EndedAt 在 finish 里写入:必须在 t.mu 内读取并连同任务一起
	// 快照,否则排序阶段会在锁外读 EndedAt 形成数据竞争(-race 可复现)。
	type terminalTask struct {
		ref   *TransferTask
		ended time.Time
	}
	var terminal []terminalTask
	for _, t := range s.tasks {
		t.mu.Lock()
		done := t.State == stateDone || t.State == stateFailed
		ended := t.EndedAt
		old := done && !ended.IsZero() && now.Sub(ended) > keepTaskAfter
		t.mu.Unlock()
		if old {
			delete(s.tasks, t.ID)
			continue
		}
		if done {
			terminal = append(terminal, terminalTask{ref: t, ended: ended})
		}
	}
	if len(terminal) <= keepTasks {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].ended.After(terminal[j].ended)
	})
	for i := keepTasks; i < len(terminal); i++ {
		delete(s.tasks, terminal[i].ref.ID)
	}
}

// keepPolicy 终结任务保留策略(条数 + 时长);配置为 0 走默认。
func (s *Transfer) keepPolicy() (int, time.Duration) {
	c := s.cfg.Get().Transfer
	n, d := c.KeepTasks, c.KeepTaskAfter
	if n <= 0 {
		n = defaultKeepTasks
	}
	if d <= 0 {
		d = defaultKeepTaskAfter
	}
	return n, d
}

// autoStreamBytes channel=auto 时改用 stream(本机中转)的文件大小阈值;
// 配置为负视为「全部走 offline」(仅受 max_bytes 与提取链约束限制),0 走默认 8GiB。
func (s *Transfer) autoStreamBytes() int64 {
	n := s.cfg.Get().Transfer.AutoStreamBytes
	switch {
	case n < 0:
		return 0
	case n == 0:
		return defaultAutoStreamBytes
	default:
		return n
	}
}

// PublicBase 端点的对外访问基址:优先 transfer.public_base,否则取 domain_routes
// 里 mangodrive 的首个域名。桌面端用它拼解析链接,所以不回落内网 endpoint。
func (s *Transfer) PublicBase() string {
	c := s.cfg.Get()
	if b := strings.TrimRight(strings.TrimSpace(c.Transfer.PublicBase), "/"); b != "" {
		if !strings.Contains(b, "://") {
			b = "https://" + b
		}
		return b
	}
	for _, d := range c.DomainRoutes["mangodrive"] {
		if d = strings.TrimRight(strings.TrimSpace(d), "/"); d != "" {
			return "https://" + d
		}
	}
	return ""
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
	task.State = stateRunning
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

	channel, err := chooseChannel(req.Channel, src.needHdr, src.meta.BindIP,
		src.size, s.autoStreamBytes(), s.maxTransferBytes())
	if err != nil {
		return err
	}
	task.mu.Lock()
	task.Channel = string(channel)
	task.mu.Unlock()

	// 同目标并发写守卫:同一落盘路径同时只允许一个任务在写。offline 的真实写入
	// (aria2 下载 → OpenList 上传)在 PanRouter 任务 done 之后仍在进行,所以 offline
	// 的占用维持到产物落池确认或确认窗口超时,由确认器 goroutine 释放(见 watchOfflineArtifact)。
	final := finalDestPath(channel, src, destPath)
	if err := s.claimDest(task.ID, final); err != nil {
		return err
	}
	held := final
	releaseOnce := false
	release := func() {
		if releaseOnce {
			return
		}
		releaseOnce = true
		s.releaseDest(held, task.ID)
	}
	defer release()

	switch channel {
	case ChannelOffline:
		err := s.transferOffline(ctx, task, src, destPath)
		if err != nil && req.Channel != ChannelOffline {
			// auto 选中的 offline 只是「优先尝试省带宽」:端点未部署 aria2
			// (localhost:6800 refused)或远端拒收时,auto 必须回退 stream,
			// 否则桌面端默认通道(AUTO)在无 aria2 的部署上全部失败。
			// 显式 offline 不回退:调用方明确要求省带宽,失败原因必须可见。
			// 回退后真实写入路径变回 destPath(stream 命名),换占。
			s.log.Warnw("offline unavailable, falling back to stream",
				"id", task.ID, "err", err)
			task.mu.Lock()
			task.Channel = string(ChannelStream)
			task.mu.Unlock()
			release()
			if cerr := s.claimDest(task.ID, destPath); cerr != nil {
				return cerr
			}
			held = destPath
			releaseOnce = false
			return s.transferStream(ctx, task, src, destPath)
		}
		if err != nil {
			return err
		}
		// offline 受理成功:占用交给确认器(产物落池/超时/任务被清理时释放)。
		// 注意不能用外层 release 闭包(其 releaseOnce 已置位防止 defer 重复释放),
		// 确认器退出时必须直接释放。
		releaseOnce = true
		// 先转 pending 再启动确认器:顺序反过来会有竞态(确认器先 done 再被压回)。
		task.markPending()
		go func() {
			defer s.releaseDest(final, task.ID)
			s.watchOfflineArtifact(task, final, src.size)
		}()
		return nil
	default:
		return s.transferStream(ctx, task, src, destPath)
	}
}

// finalDestPath 计算任务的最终落盘路径:offline 落盘名 = 直链 URL 末段(§5.4
// offline 落盘名),stream 沿用解析名。冲突守卫与快照改写共用本函数,防止口径漂移。
func finalDestPath(channel TransferChannel, src *transferSource, destPath string) string {
	if channel == ChannelOffline && src.file != nil {
		u := src.serverURL
		if u == "" {
			u = src.file.DirectURL
		}
		if ln := offlineLandedName(u); ln != "" {
			return pathDir(destPath) + "/" + ln
		}
	}
	return destPath
}

// claimDest 活跃任务目标路径互斥:同路径并发写(典型:stream PUT 与 offline 的
// OpenList 上传互踩)会让离线任务上传失败并整单重试,白白重拉整个源。
// 先完成再提交的串行同路径不受限(语义 = 覆盖)。
func (s *Transfer) claimDest(taskID, destPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.activeDest[destPath]; ok && owner != taskID {
		return driver.NewErr(driver.KindConflict,
			"目标路径已有进行中的转存任务:"+destPath, nil)
	}
	s.activeDest[destPath] = taskID
	return nil
}

// releaseDest 释放目标路径占用;非持有者调用是 no-op(幂等)。
func (s *Transfer) releaseDest(destPath, taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeDest[destPath] == taskID {
		delete(s.activeDest, destPath)
	}
}

func (s *Transfer) watchOfflineArtifact(task *TransferTask, destPath string, expect int64) {
	cfg := s.cfg.Get().Transfer
	after := cfg.OfflineConfirmAfter
	if after == 0 {
		after = 2 * time.Hour
	}
	if after < 0 {
		// 显式关闭产物确认:回落到旧的「受理即 done」语义,但必须告警,
		// 因为此时同路径写锁会立即释放而 aria2 可能仍在落盘。
		s.log.Warnw("offline artifact confirmation disabled, accepting optimistically",
			"id", task.ID, "dest", destPath)
		task.finishDone(0)
		return
	}
	poll := cfg.OfflineConfirmPoll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), after)
	defer cancel()
	// landed 观测产物:必须是非目录且非空;源大小已知时还要不小于它。
	// expect<=0(大小未知)时「非空」不足以证明是本次产物:同路径的历史残留
	// (上一次重传/失败任务的半截文件)同样非空,要求 mtime 不早于任务开始时间。
	landed := func(p string) (int64, bool) {
		obj, err := s.driveAPI().Get(ctx, p)
		if err != nil || obj == nil || obj.IsDir || obj.Size <= 0 {
			return 0, false
		}
		if expect > 0 {
			if obj.Size < expect {
				return 0, false
			}
		} else if !freshEnough(obj, task.StartedAt) {
			return 0, false
		}
		return obj.Size, true
	}
	// 立即首查:小文件(或重传时产物已在)等不到第一个 tick,占用窗口可以秒级收口。
	if size, ok := landed(destPath); ok {
		s.log.Infow("offline artifact confirmed",
			"id", task.ID, "dest", destPath, "size", size)
		task.finishDone(size)
		return
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	// reconcileEvery 每 N 个 tick 做一次目录对账:名字被上游改名、产物名与 URL
	// 末段不一致时,只看预测路径会空等到窗口超时再误判 failed。按窗口的 1/6
	// 摊开(默认 2h/10s → 每 20min 一次,加窗口末期一次,共 7 次),避免对
	// 大目录反复枚举;测试窗口极短时下限 4 tick。
	reconcileEvery := int(after/poll) / 6
	if reconcileEvery < 4 {
		reconcileEvery = 4
	}
	ticks := 0
	// remoteFailCheckEvery 远端失败查询节拍(3 tick):landing 检查保持每 tick,
	// 失败检测容忍 ≤3×poll 延迟。
	const remoteFailCheckEvery = 3
	for {
		select {
		case <-ctx.Done():
			// 窗口到期前最后再对账一次:产物可能刚落池、或落在了非预测名下。
			if size, obj, ok := s.reconcileOfflineArtifact(task, destPath, expect); ok {
				s.confirmOfflineArtifact(task, obj, size)
				return
			}
			s.log.Warnw("offline artifact not confirmed within window",
				"id", task.ID, "dest", destPath, "expect", expect, "window", after,
				"remote_token", task.offlineToken)
			task.finishFailed("artifact not landed within window: "+destPath,
				string(driver.KindUpstream))
			return
		case <-tick.C:
			if s.Get(task.ID) == nil {
				return
			}
			// 远端任务已明确失败(aria2 404 / 白名单拒收):产物永远不会出现,
			// 立即收口,不留 2h 的 pending 和路径占用。远端查询按 3 tick 节流
			//(失败检测延迟 ≤3×poll,窗口以小时计,远不敏感):每 tick 全量拉
			// undone 队列,对同目录多任务/长窗口是不必要的上游放大。
			ticks++
			if ticks%remoteFailCheckEvery == 0 {
				if failed, remoteErr := s.offlineRemoteFailed(ctx, task); failed {
					s.log.Warnw("offline artifact remote task failed",
						"id", task.ID, "dest", destPath, "remote_token", task.offlineToken)
					task.finishFailed("remote offline task failed: "+remoteErr,
						string(driver.KindUpstream))
					return
				}
			}
			if size, ok := landed(destPath); ok {
				s.log.Infow("offline artifact confirmed",
					"id", task.ID, "dest", destPath, "size", size)
				task.finishDone(size)
				return
			}
			// 目录对账(改名兜底)按节拍做,比 Get 贵。
			if ticks%reconcileEvery == 0 {
				if size, obj, ok := s.reconcileOfflineArtifact(task, destPath, expect); ok {
					s.confirmOfflineArtifact(task, obj, size)
					return
				}
			}
		}
	}
}

// artifactClockSkew is the cross-host clock tolerance for landing confirmation.
// mtime is written by the OpenList host while StartedAt comes from the PanRouter host;
// both rely on NTP, so a small skew must not turn a landed artifact into a failure.
const artifactClockSkew = 2 * time.Minute

// freshEnough reports whether obj was produced by this task (mtime >= StartedAt-skew).
// A stale same-name leftover from an earlier run must not count; an unparsable or
// missing mtime counts as "not fresh" (fail closed).
func freshEnough(obj *mangodrive.Obj, started time.Time) bool {
	if obj == nil || strings.TrimSpace(obj.Modified) == "" {
		return false
	}
	mt, err := time.Parse(time.RFC3339Nano, obj.Modified)
	if err != nil {
		mt, err = time.Parse(time.RFC3339, obj.Modified)
	}
	if err != nil {
		return false
	}
	return !mt.Add(artifactClockSkew).Before(started)
}

// offlineRemoteFailed looks up the remote queued task behind the token.
// It returns (true, reason) only when the remote side has definitively failed
// (so the artifact will never appear). A failed lookup, cap/unavailability,
// or an already-cleaned task is reported as "unknown" and left to the artifact
// confirmation -- a monitoring hiccup must never fail a healthy task.
func (s *Transfer) offlineRemoteFailed(ctx context.Context, task *TransferTask) (bool, string) {
	task.mu.Lock()
	token := task.offlineToken
	task.mu.Unlock()
	if token == "" {
		return false, ""
	}
	rt, err := s.driveAPI().OfflineTaskByToken(ctx, token)
	if err != nil || rt == nil {
		return false, ""
	}
	if !rt.Failed() {
		return false, ""
	}
	msg := strings.TrimSpace(rt.Error)
	if msg == "" {
		msg = "remote task state " + strconv.Itoa(rt.State)
	}
	return true, msg
}

// reconcileOfflineArtifact lists the destination directory and looks for the
// artifact this task actually produced. The predicted name (URL tail) is not
// authoritative: aria2 honors Content-Disposition and applies its own name
// sanitizing. Matching on predicted path alone would fail an artifact that DID
// land -- and would hold the write guard on a path nothing occupies.
// Criteria: non-dir, non-empty, mtime not older than task start; and when expect
// is known, size >= expect.
func (s *Transfer) reconcileOfflineArtifact(task *TransferTask, destPath string, expect int64) (int64, *mangodrive.Obj, bool) {
	dir := pathDir(destPath)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	objs, err := s.driveAPI().List(ctx, dir)
	if err != nil {
		s.log.Warnw("offline artifact reconcile list failed",
			"id", task.ID, "dir", dir, "err", err)
		return 0, nil, false
	}
	for i := range objs {
		o := &objs[i]
		if o.IsDir || o.Size <= 0 || !freshEnough(o, task.StartedAt) {
			continue
		}
		if expect > 0 && o.Size < expect {
			continue
		}
		// 认领守卫:同目录并发转存(claimDest 只互斥同路径)下,目录里可能有
		// 其他任务的产物——它同样非空/新鲜/够大。认领中的路径(写守卫)与
		// 其他任务已记录的 destPath 都不是本任务的产物,跳过,防止误归因。
		if s.destOwnedByOther(dir+"/"+o.Name, task, destPath) {
			continue
		}
		return o.Size, o, true
	}
	return 0, nil, false
}

// destOwnedByOther reports whether p belongs to a different transfer task:
// actively claimed by one (concurrent same-dir writes are legal, claimDest only
// excludes the same path), or recorded as the final destPath of a finished task
// whose artifact landed under its predicted name. The caller's own destPath is
// never "other", so a same-path retry still reconciles its own leftover.
func (s *Transfer) destOwnedByOther(p string, task *TransferTask, myDest string) bool {
	s.mu.Lock()
	if owner, ok := s.activeDest[p]; ok && owner != task.ID {
		s.mu.Unlock()
		return true
	}
	// tasks 遍历:DestPath/State 受各自 task.mu 保护,逐个短锁读。
	// 锁序说明:此处持有 s.mu 的同时取 t.mu —— 全仓锁序统一为 s.mu → task.mu
	// (pruneLocked 同此:s.mu 内遍历取 t.mu),严禁任何 t.mu → s.mu 的反向获取,
	// 否则两把锁交叉等待死锁。注释勿写成「先收集再出锁」——收集本身就要持 t.mu。
	owned := make([]string, 0, len(s.tasks))
	for id, t := range s.tasks {
		if id == task.ID {
			continue
		}
		t.mu.Lock()
		// 只认「产物可能已落池」的任务:done(已确认落盘)、pending(远端已受理待确认)、
		// running(受理瞬间的窄窗)。failed 的产物根本不存在、queued 尚未提交远端 ——
		// 把它们的 destPath 算进跳过集只会造成误跳过(别的任务的真实产物被挡住),
		// 让后者对账失败、超窗后被误判为失败。
		mayHaveArtifact := t.State == stateDone || t.State == statePending || t.State == stateRunning
		dest := t.DestPath
		t.mu.Unlock()
		if mayHaveArtifact {
			owned = append(owned, dest)
		}
	}
	s.mu.Unlock()
	for _, d := range owned {
		if d == p && p != myDest {
			return true
		}
	}
	return false
}

// confirmOfflineArtifact closes the task with the artifact found by reconciliation:
// rewrite the snapshot to the real name/path first (the record must point at the
// file that actually landed), then finish with its real size.
func (s *Transfer) confirmOfflineArtifact(task *TransferTask, obj *mangodrive.Obj, size int64) {
	if obj != nil && obj.Name != "" {
		real := pathDir(task.DestPath) + "/" + obj.Name
		task.mu.Lock()
		task.FileName = obj.Name
		task.DestPath = real
		task.mu.Unlock()
		s.log.Infow("offline artifact reconciled",
			"id", task.ID, "dest", real, "size", size)
	}
	task.finishDone(size)
}

// chooseChannel 决定最终通道(约束与阈值语义见 §5.4):
//   - 提取链有约束(UA/Referer/Cookie/BindIP)→ stream,offline 无法携带;
//   - auto_stream_bytes 负值 → 全部 offline;source 大小未知或 ≥ 阈值 → stream;
//   - maxBytes 是不可越过的硬上限(>0 生效):offline 在搬运开始前就能校验,
//     大小未知无法证明不超限 → auto 改 stream(由实收字节兜底),显式 offline 报错。
//
// 显式 offline 在超限时也报错而不是静默改道,避免调用方以为占了省带宽的通道。
func chooseChannel(req TransferChannel, needHdr, bindIP bool, size, autoThreshold, maxBytes int64) (TransferChannel, error) {
	ch := req
	if ch == "" || ch == ChannelAuto {
		ch = ChannelStream
		if !needHdr && !bindIP {
			if autoThreshold <= 0 || (size > 0 && size < autoThreshold) {
				ch = ChannelOffline
			}
		}
	}
	// offline 由远端 aria2 直拉,不受本机流式读取的实收字节校验约束。
	if ch == ChannelOffline && maxBytes > 0 && (size <= 0 || size > maxBytes) {
		if req == ChannelOffline {
			if size <= 0 {
				return "", driver.NewErr(driver.KindUnsupported,
					"源文件大小未知,离线通道无法校验转存上限;请改用 stream 通道", nil)
			}
			return "", driver.NewErr(driver.KindUnsupported,
				"文件超过转存上限:"+strconv.FormatInt(maxBytes, 10)+" 字节", nil)
		}
		// auto:大小未知/超限时改走 stream,由真实 Content-Length 或实收字节兜底。
		ch = ChannelStream
	}
	return ch, nil
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
// FID == "direct" 时 SourceURL 即公网直链,跳过网盘解析(见 resolveDirectSource)。
func (s *Transfer) resolveSource(ctx context.Context, req TransferRequest) (*transferSource, error) {
	ua := req.UA
	if ua == "" {
		ua = mangodrive.BrowserUA
	}
	if req.FID == FIDDirect {
		return s.resolveDirectSource(ctx, req.SourceURL, ua, req.Name)
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

// resolveDirectSource 把公网直链装配成 transferSource(fid=direct 直链直传)。
// SSRF 防护:探测请求经 direct 客户端(AllowPrivate=false),私网地址在拨号层被拒;
// 探测同时拿到大小,给 chooseChannel 做 offline 上限预检。探测失败即拒绝,
// 不放行「先受理再失败」的直链——offline 通道会把 URL 交给远端 aria2 拉取,
// 本机探测是唯一能在提交前拦住私网直链的闸门。
func (s *Transfer) resolveDirectSource(ctx context.Context, rawURL, ua, name string) (*transferSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, driver.NewErr(driver.KindUnsupported, "直链格式无效", nil)
	}
	if u.User != nil {
		return nil, driver.NewErr(driver.KindUnsupported, "直链不应携带用户凭据", nil)
	}
	size, perr := s.probeDirectSize(ctx, rawURL, ua)
	if perr != nil {
		return nil, perr
	}
	// 请求 name 可覆盖落盘名(v1.14 契约:三通道统一,仅 stream 生效;
	// offline 通道远端 aria2 不可重命名,落盘名仍由 URL 末段/Content-Disposition 决定)。
	// 注意 cleanName 对空串会回退 transfer_<时间戳>,所以先判空再清洗。
	if strings.TrimSpace(name) == "" {
		name = offlineLandedName(rawURL)
	}
	name = cleanName(name)
	return &transferSource{
		file: &ResolveFileResult{
			Pan: "direct", FID: FIDDirect, FileName: name,
			DirectURL: rawURL, ExpiresAt: time.Now().Add(time.Hour),
		},
		meta:     LinkMeta{UA: ua},
		name:     name,
		size:     size,
		upstream: "direct",
	}, nil
}

// probeDirectSize 探测直链大小:HEAD 取 Content-Length,不支持则 GET Range:0-0
// 取 Content-Range 总长。私网直链在这里被 direct 客户端拒绝(SSRF 闸门)。
func (s *Transfer) probeDirectSize(ctx context.Context, rawURL, ua string) (int64, error) {
	cl := s.upstream["direct"]
	blocked := func(err error) bool {
		return err != nil && (strings.Contains(err.Error(), "blocked private") ||
			strings.Contains(err.Error(), "resolves to private"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, driver.NewErr(driver.KindUnsupported, "直链格式无效", err)
	}
	req.Header.Set("User-Agent", ua)
	if resp, err := cl.DoStream(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
			return resp.ContentLength, nil
		}
	} else if blocked(err) {
		return 0, driver.NewErr(driver.KindUnsupported, "直链指向私网地址,已拒绝(SSRF 防护)", nil)
	}
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, driver.NewErr(driver.KindUnsupported, "直链格式无效", err)
	}
	req2.Header.Set("User-Agent", ua)
	req2.Header.Set("Range", "bytes=0-0")
	resp, err := cl.DoStream(req2)
	if err != nil {
		if blocked(err) {
			return 0, driver.NewErr(driver.KindUnsupported, "直链指向私网地址,已拒绝(SSRF 防护)", nil)
		}
		return 0, driver.NewErr(driver.KindUpstream, "直链不可达", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, e := strconv.ParseInt(cr[i+1:], 10, 64); e == nil && n > 0 {
					return n, nil
				}
			}
		}
	case resp.StatusCode == http.StatusOK && resp.ContentLength > 0:
		return resp.ContentLength, nil
	}
	return 0, nil
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
	buildReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
		if err != nil {
			return nil, err
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
		return req, nil
	}
	// 公网直链的瞬态网络错误(h2 tar-pit/连接复位/对端闪断)重试一次:
	// 此刻尚未向 OpenList 写入任何字节,重试无副作用。
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		req, berr := buildReq()
		if berr != nil {
			return driver.NewErr(driver.KindUpstream, "构造上游请求失败", berr)
		}
		resp, err = up.DoStream(req)
		if err == nil {
			break
		}
		s.log.Warnw("upstream fetch failed, retrying", "upstream", src.upstream,
			"attempt", attempt+1, "err", err.Error())
	}
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "上游直链请求失败", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return driver.NewErr(driver.KindUpstream, "上游直链返回 "+strconv.Itoa(resp.StatusCode), nil)
	}
	// 长度以实际上游响应头为准:解析时登记的大小可能与真实字节不符(转码/分包),
	// 未知则按 -1 走 chunked,绝不伪造 Content-Length 让 PUT 断流。
	limit := s.maxTransferBytes()
	size := resp.ContentLength
	if size <= 0 {
		size = 0
	}
	if limit > 0 && size > limit {
		return driver.NewErr(driver.KindUnsupported, "文件超过转存上限:"+strconv.FormatInt(limit, 10)+" 字节", nil)
	}

	pr, pw := io.Pipe()
	go func() {
		_, cErr := io.Copy(pw, &progressReader{r: resp.Body, task: task, max: limit})
		pw.CloseWithError(cErr)
	}()
	if err := s.driveAPI().Put(ctx, destPath, size, pr); err != nil {
		_ = pr.CloseWithError(err)
		// 上游 chunked(无 Content-Length)时只有实收字节能证明超限:
		// Put 的报错可能只是「请求体提前中断」,对外统一回 max_bytes 语义。
		if limit > 0 && atomic.LoadInt64(&task.done) > limit {
			return driver.NewErr(driver.KindUnsupported,
				"文件超过转存上限:"+strconv.FormatInt(limit, 10)+" 字节", err)
		}
		return err
	}
	done := atomic.LoadInt64(&task.done)
	if limit > 0 && done > limit {
		return driver.NewErr(driver.KindUnsupported,
			"文件超过转存上限:"+strconv.FormatInt(limit, 10)+" 字节", nil)
	}
	if size > 0 && done != size {
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
// max>0 时兼任硬上限:超出即返回错误让 io.Copy 中断,避免无 Content-Length
// 的上游(chunked)绕过 max_bytes 把超大文件整段搬进私有盘。
type progressReader struct {
	r    io.Reader
	task *TransferTask
	max  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.task.set(int64(n))
	}
	if n > 0 && p.max > 0 && atomic.LoadInt64(&p.task.done) > p.max {
		return n, driver.NewErr(driver.KindUnsupported,
			"文件超过转存上限:"+strconv.FormatInt(p.max, 10)+" 字节", nil)
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
	// 快照改写与冲突守卫共用 finalDestPath 口径:offline 落盘名 = URL 末段
	// (aria2 percent-decode;源带 Content-Disposition 时以其为准)。快照必须写
	// 真实产物名/路径,不能沿用请求的 name(否则转存记录指向不存在的文件)。
	if final := finalDestPath(ChannelOffline, src, destPath); final != destPath {
		task.mu.Lock()
		task.FileName = final[strings.LastIndex(final, "/")+1:]
		task.DestPath = final
		task.mu.Unlock()
	}
	// OpenList 离线端点异步建任务,成功时空响应体亦属正常;任务由远端队列接管。
	// task.log 可能为 nil(直接构造 TransferTask 的调用方),日志只是附加观测,不能因此 panic。
	// token 记进任务:确认器据此轮询远端终态,远端已失败则立即收口(不必等窗口超时)。
	if len(tasks) > 0 {
		task.mu.Lock()
		task.offlineToken = tasks[0].ID
		task.mu.Unlock()
		if task.log != nil {
			task.log.Infow("offline task created", "id", task.ID, "remote", tasks[0].ID, "url", offlineURL)
		}
	}
	// 进度不在这里预置:受理并不代表字节已落盘,预置会让 pending 显示满进度条
	// (确认/超时收口时会用真实大小或 0 覆盖)。
	return nil
}

// offlineLandedName 计算 offline 通道的真实落盘文件名:URL 末段路径百分号解码。
// 解析失败或为空返回 ""(调用方保留请求名兜底;URL 以 / 结尾时 aria2 落 index.html,
// 该形态直链本就不应进入离线通道)。
func offlineLandedName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return ""
	}
	name := u.Path
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return ""
	}
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	return name
}

// clientLabel 归一化调用方标识:去控制字符、限长 64,空值省略。
func clientLabel(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
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
