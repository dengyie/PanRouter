// Relay:服务端中转流——补请求头、白名单重定向、Range 透传与续传。
package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/pkg/metrics"
	"github.com/dengyie/panrouter/internal/repo"
)

// aesCipher 解耦 crypto 包,便于单测注入。
type aesCipher interface {
	EncryptBytes(plain []byte) ([]byte, error)
	DecryptBytes(data []byte) ([]byte, error)
}

const (
	relayBufSize = 128 << 10
	// relayFlushBytes:攒够这么多字节再 Flush 一次。原实现每 32 KB Write+Flush,
	// 13 GB 就是约 40 万次系统调用;下载场景对首字节延迟不敏感,攒到 256 KB
	// (满速下 ~70 ms)可以省下大半开销,同时对边下边播仍然够快。
	relayFlushBytes = 256 << 10
	// relayResumeMax:一次请求最多续传几次(不含最初的连接)。有界是为了不对着
	// 「每次必断」的上游无限重连——续满次数后按 upstream_interrupt 记账收尾。
	relayResumeMax = 3

	// 指标名与归因标签集中在这里:同一名字在 NewRelay/Serve/countAbort 三处出现,
	// 一旦手写拼错就会悄悄多出一条空序列,监控上看不出异常。
	relayMetricBytes  = "panrouter_relay_bytes_total"
	relayMetricRotate = "panrouter_relay_rotate_total"
	relayMetricResume = "panrouter_relay_resume_total"
	relayMetricAbort  = "panrouter_relay_abort_total"
	relayMetricStall  = "panrouter_relay_stall_total"
	relayMetricActive = "panrouter_relay_streams_active"

	relayAbortClientGone = "client_gone"
	relayAbortUpstream   = "upstream_interrupt"
	relayAbortRotate     = "rotate_failed"

	// defaultRelayRotateBytes:单条上游连接最多搬运这么多字节就重开一条。
	//
	// 2026-11-13 实测(51 文件 / 13.16 GB 真实下载):quark 对**同一条上游长连接**
	// 在累计约 1 GB 后把速度压到 ~100 KB/s,而新建连接立刻回到 3.5 MB/s——三条老
	// 连接合计 0.32 MB/s、两条新连接合计 7.2 MB/s,同期账号状态始终 ok(既不是
	// 我方限频,也不是账号被风控)。旧实现一条连接跑到底,417 MB 文件要 185~220 s;
	// 按块换连接后 143~167 s,两个 576 MB 文件从「重试耗尽失败」变成正常跑完。
	//
	// 所以这里按固定字节数主动轮换上游连接:响应头早已发出,客户端侧字节流连续。
	defaultRelayRotateBytes int64 = 256 << 20

	// defaultRelayStallTimeout:上游连续这么久一个字节都没吐,就判它挂死(黑洞/半开
	// 连接),取消这条连接并尝试续传。
	//
	// 为什么必须有它:stream client 是 NoBodyTimeout=true(慢流不能被整体超时掐断),
	// 代价是上游真挂死时 Read 会一直阻塞,客户端连接就这么吊着——用户看到的是
	// 「0 B/s 卡死」而不是一个明确结局。有了看门狗,挂死会退化成一次有界续传/报错。
	// 看门狗只在「正阻塞在读上游」时生效(见 upstreamConn.reading):客户端自己
	// 写不动(w.Write 阻塞)不算上游挂死,那种情况取消连接只会白白重连。
	defaultRelayStallTimeout = 60 * time.Second
)

type Relay struct {
	resolver *Resolver
	clients  map[string]*httpx.Client // pan → 只给中转用的 client(带 per-driver 重定向白名单)
	store    *repo.Store
	aes      aesCipher
	met      *metrics.Registry
	log      *loggerType
	cfgp     *config.Provider // relay 段(stall_timeout / rotate_bytes);nil = 内置默认
	// rotateBytes / stallTimeout 是**覆盖值**,非 0 时优先于配置:生产恒为 0,
	// 只有单测用它把阈值压到 KB/百毫秒级。求值见 rotateLimit/stallLimit。
	rotateBytes  int64
	stallTimeout time.Duration
}

func NewRelay(resolver *Resolver, clients map[string]*httpx.Client, store *repo.Store,
	aes aesCipher, met *metrics.Registry, log *loggerType, cfgp *config.Provider) *Relay {
	s := &Relay{resolver: resolver, clients: clients, store: store, aes: aes, met: met, log: log, cfgp: cfgp}
	if met != nil {
		met.Help(relayMetricBytes, "Relay 转发给客户端的字节数(按网盘)")
		met.Help(relayMetricRotate, "Relay 主动轮换上游连接的次数")
		met.Help(relayMetricResume, "Relay 上游断流后按 Range 续传成功的次数")
		met.Help(relayMetricAbort, "Relay 未正常读完就结束的次数(按原因)")
		met.Help(relayMetricStall, "Relay 上游连续 stall_timeout 无字节判挂死而取消连接的次数")
		met.Help(relayMetricActive, "Relay 当前正在中转的流数(每请求开始时 +1,结束 -1)")
		// 先把各网盘的 0 值序列建出来:没有序列和「全 0」在监控上不可区分。
		for pan := range clients {
			met.Add(relayMetricBytes, map[string]string{"pan": pan}, 0)
			met.Add(relayMetricRotate, map[string]string{"pan": pan}, 0)
			met.Add(relayMetricResume, map[string]string{"pan": pan}, 0)
			met.Add(relayMetricStall, map[string]string{"pan": pan}, 0)
			met.AddGauge(relayMetricActive, map[string]string{"pan": pan}, 0)
			for _, reason := range []string{relayAbortClientGone, relayAbortUpstream, relayAbortRotate} {
				met.Add(relayMetricAbort, map[string]string{"pan": pan, "reason": reason}, 0)
			}
		}
	}
	return s
}

// relayCfg 返回 relay 段配置;未接配置(单测)时返回 nil。
func (s *Relay) relayCfg() *config.Relay {
	if s.cfgp == nil {
		return nil
	}
	if c := s.cfgp.Get(); c != nil {
		return &c.Relay
	}
	return nil
}

// rotateLimit 求轮换阈值:显式字段(单测注入) > 配置 > 内置默认。
//
// 每个请求只求值一次并锁进局部变量:热加载改 relay.rotate_bytes 只影响新请求,
// 已开始的流不会在轮换途中阈值突变。负值原样返回,由调用方按「关闭轮换」处理。
func (s *Relay) rotateLimit() int64 {
	if s.rotateBytes != 0 {
		return s.rotateBytes
	}
	if c := s.relayCfg(); c != nil && c.RotateBytes != 0 {
		return c.RotateBytes
	}
	return defaultRelayRotateBytes
}

// stallLimit 求停流看门狗阈值,优先级同 rotateLimit;<=0 表示关闭看门狗。
func (s *Relay) stallLimit() time.Duration {
	if s.stallTimeout != 0 {
		return s.stallTimeout
	}
	if c := s.relayCfg(); c != nil && c.StallTimeout != 0 {
		return c.StallTimeout
	}
	return defaultRelayStallTimeout
}

// StreamInput 是一次中转请求的已验证参数(签名校验在 api 层完成)。
type StreamInput struct {
	Pan      string
	ShareKey string
	FID      string
	ClientUA string
}

// streamLink 是发起一次上游请求所需的全部约束(来自缓存直链)。
type streamLink struct {
	URL     string
	UA      string
	Referer string
	Cookie  []byte
}

// streamSession 是一次 /stream 请求的可变状态:轮换与续传都要从同一个 pos 接着写,
// 且都必须落在客户端允许的范围 limit 之内(limited=false 表示无从判断,不钳制)。
type streamSession struct {
	in        StreamInput
	link      streamLink
	orig      http.Header // 首次上游响应头:校验器(ETag/Last-Modified)基线
	pos       int64       // 下一个要写给客户端的绝对偏移
	limit     int64       // 客户端允许的包含式终点(仅 limited 时有意义)
	limited   bool
	resumes   int
	refreshed bool // 直链失效后是否已重解析过(防死循环)
}

// upstreamConn 是一条上游连接 + 它专用的可取消 ctx + 停流看门狗。
// 每条连接独立 ctx:看门狗靠取消它来打断挂死的 Read,而客户端 ctx 不受影响,
// 所以「上游挂死」和「客户端走了」在错误归因上依然可分。
type upstreamConn struct {
	resp      *http.Response
	cancel    context.CancelFunc
	last      atomic.Int64 // 最近一次读到字节的 UnixNano
	readStart atomic.Int64 // 本轮 Read 开始的 UnixNano
	reading   atomic.Bool  // 是否正阻塞在 Read 上(写客户端期间不许判挂死)
	stop      chan struct{}
	once      sync.Once
	onStall   func() // 看门狗真正判挂死时回调一次(记 relay_stall_total;可为 nil)
}

func (c *upstreamConn) touch() { c.last.Store(time.Now().UnixNano()) }

// stallBase 是判挂死的计时基准:取「上次读到字节」与「本轮读开始」的较晚者。
//
// 只看 last 会在慢客户端场景误杀:relay 阻塞在 w.Write 期间不读上游,last 一直
// 变旧;客户端一恢复,刚发起的那个 Read 立刻显得「已空闲 N 秒」,任何一个 tick
// 落在 Read 返回之前就会把这条健康的连接取消掉(单测里 600 ms 的客户端阻塞就能
// 稳定复现)。以本轮读开始时间为基准后,只有「这次 Read 自身」久等不到字节才算挂死。
func (c *upstreamConn) stallBase() int64 {
	base := c.last.Load()
	if st := c.readStart.Load(); st > base {
		base = st
	}
	return base
}

// close 幂等:Body.Close / cancel / close(stop) 各只做一次。
func (c *upstreamConn) close() {
	c.once.Do(func() {
		close(c.stop)
		c.cancel()
		_ = c.resp.Body.Close()
	})
}

// read 是唯一的上游读入口:期间置 reading,让看门狗能区分「读上游卡住」和
// 「写客户端卡住」——后者若被当成上游挂死,会在客户端只是慢(暂停/断网切换)时
// 被反复取消重连,最终把一次正常的下载记成 upstream_interrupt 掐掉。
func (c *upstreamConn) read(buf []byte) (int, error) {
	c.readStart.Store(time.Now().UnixNano()) // 先记基准再置 reading,别让看门狗读到「在读但基准是上一轮」
	c.reading.Store(true)
	n, err := c.resp.Body.Read(buf)
	c.reading.Store(false)
	return n, err
}

func (c *upstreamConn) watchStall(timeout time.Duration) {
	tick := timeout / 4
	if tick <= 0 {
		tick = time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			if !c.reading.Load() {
				continue
			}
			if time.Since(time.Unix(0, c.stallBase())) >= timeout {
				// 先记账再取消:cancel 之后 Serve 会走续传/归因分支,这里记的是
				// 「上游真的停流了」这个事实,与后续续传是否成功无关。
				if c.onStall != nil {
					c.onStall()
				}
				c.cancel() // 打断 Read,交给 Serve 的续传逻辑
				return
			}
		}
	}
}

// touchReader 只用来统计「这条连接还在吐字节」,供丢弃 Range 前缀时喂看门狗。
type touchReader struct{ c *upstreamConn }

func (t touchReader) Read(p []byte) (int, error) {
	n, err := t.c.resp.Body.Read(p)
	if n > 0 {
		t.c.touch()
	}
	return n, err
}

// Serve 执行中转:取新鲜直链 → 携带约束头请求上游 → 透传响应(含 Range/续传)。
// 签名只在建立连接时校验;轮换/续传复用同一 URL,不会中途 401。
//
// 错误契约:只有**尚未写响应头**时才会返回 error(调用方据此写 JSON 错误);
// 一旦开始转发,结局只能通过返回值 nil + 日志/指标表达,调用方不得再写响应。
func (s *Relay) Serve(ctx context.Context, w http.ResponseWriter, in StreamInput, rangeHeader string) error {
	link, err := s.freshLink(ctx, in)
	if err != nil {
		return err
	}

	conn, err := s.openConn(ctx, in, link, rangeHeader)
	if err != nil {
		return err
	}
	defer func() { conn.close() }()

	if conn.resp.StatusCode >= 400 {
		// 直链可能提前失效(风控/过期):尚未写响应头,可安全报错让上层提示重试
		code := conn.resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(conn.resp.Body, 4096))
		// 上游明确否决时立即失效本地缓存,否则重新解析仍命中同一死链(缓存优先),
		// 用户会在 TTL 内 (最长 2h) 反复失败。失效后下次 GetFreshLink 触发真实重解析。
		if isStaleStatus(code) {
			s.expireLink(in)
		}
		return driver.NewErr(driver.KindUpstream,
			fmt.Sprintf("上游返回 %d,直链可能已失效,请重新解析", code), nil)
	}

	start, total, hasCR := parseContentRange(conn.resp.Header.Get("Content-Range"))
	// Content-Length 只在非 206 时代表整文件长度;206 时它是本段长度,不能当总量。
	if !hasCR {
		if cl := conn.resp.Header.Get("Content-Length"); cl != "" {
			if v, perr := strconv.ParseInt(cl, 10, 64); perr == nil && v > 0 {
				total = v
			}
		}
	}
	// 客户端允许的终点:轮换/续传只能写到这里。多段 Range(multipart/byteranges)
	// 无法用单条 Range 续接,直接禁用轮换——否则续传会把分段边界写坏。
	// known=false(总长未知 / 多段 / 语法不认识)时既要关掉轮换,也不能钳制写入:
	// 那种响应就是一个普通字节流,按「无上界」处理才对。
	limit, limitKnown := rangeEndLimit(rangeHeader, total, conn.resp.StatusCode)
	// 只有「知道总长度 + 上游支持 Range」时才轮换/续传;否则保持旧行为(单连接跑到底),
	// 避免为了一次性小响应(如 lanzou 的 HTML 挑战页)白折腾。
	// rotateBytes<=0 视为关闭轮换(配置负值或单测/排障用)。
	rotateBytes := s.rotateLimit()
	canRotate := rotateBytes > 0 && limitKnown && total > 0 && start <= limit &&
		(hasCR || strings.EqualFold(conn.resp.Header.Get("Accept-Ranges"), "bytes"))

	h := w.Header()
	// Content-Encoding 也照实透传:我们转发的是原始字节,若上游仍带编码声明而这里
	// 丢掉头,客户端会把压缩字节当成明文。
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Content-Encoding", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := conn.resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(conn.resp.StatusCode)
	// streams_active:从中转真正开始(响应头已发)算起,到 Serve 返回为止。
	// 与 bytes_total 的 rate() 一起看,就能分清「并发高但每条都慢」和「压根没有流」。
	s.metActiveStream(in, 1)
	defer s.metActiveStream(in, -1)

	ss := &streamSession{in: in, link: link, orig: conn.resp.Header, pos: start, limit: limit, limited: limitKnown}

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, relayBufSize)
	connBytes := int64(0) // 当前这条上游连接已搬运的字节
	pending := int64(0)   // 尚未 Flush 的字节

	for {
		n, rerr := conn.read(buf)
		if n > 0 {
			conn.touch() // 喂看门狗:有字节流动就不是挂死
			// 绝不越过客户端要的终点:一旦写超 Content-Length,net/http 会拒收整块
			// (ErrContentLength),客户端就少收尾巴;有界 Range + 轮换组合下这里必踩。
			if ss.limited {
				if room := ss.limit - ss.pos + 1; int64(n) > room {
					n = 0
					if room > 0 {
						n = int(room)
					}
				}
			}
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					s.countAbort(in, relayAbortClientGone)
					return nil // 客户端断开,属正常取消
				}
				if s.met != nil {
					s.met.Add(relayMetricBytes, map[string]string{"pan": in.Pan}, float64(n))
				}
				ss.pos += int64(n)
				connBytes += int64(n)
				pending += int64(n)
				if flusher != nil && pending >= relayFlushBytes {
					flusher.Flush()
					pending = 0
				}
			}
			if ss.limited && ss.pos > ss.limit {
				if flusher != nil && pending > 0 {
					flusher.Flush()
				}
				return nil // 客户端要的范围已送满,不必再读上游
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				if flusher != nil && pending > 0 {
					flusher.Flush()
				}
				return nil
			}
			// 客户端断开:父 ctx 先被取消,上游 Read 随之返回 context canceled。
			// 若一律记成 upstream_interrupt,运维就分不清「上游塌了」和「用户自己关页面」。
			if ctx.Err() != nil {
				s.countAbort(in, relayAbortClientGone)
				return nil
			}
			// 上游自己断了(连接被掐断/停流看门狗取消)。响应头和 Content-Length 早已发出,
			// 直接收尾只能让客户端收到「传输中断」的半截文件;能 Range 续传就续,续不成才算中断。
			if canRotate && ss.resumes < relayResumeMax {
				next, fellBack, rerr2 := s.rotate(ctx, ss)
				if rerr2 == nil {
					conn.close()
					conn = next
					connBytes = 0
					ss.resumes++
					if fellBack {
						canRotate = false // 上游忽略 Range:续用这条连接跑到底,不再轮换
					}
					if s.met != nil {
						s.met.Inc(relayMetricResume, map[string]string{"pan": in.Pan})
					}
					s.log.Warnf("relay resumed after upstream error: share=%s fid=%s pos=%d try=%d fallback=%v err=%v",
						in.ShareKey, in.FID, ss.pos, ss.resumes, fellBack, rerr)
					continue
				}
				s.log.Warnf("relay resume failed: share=%s fid=%s pos=%d err=%v",
					in.ShareKey, in.FID, ss.pos, rerr2)
			}
			s.countAbort(in, relayAbortUpstream)
			s.log.Warnf("relay upstream interrupted: share=%s fid=%s pos=%d err=%v",
				in.ShareKey, in.FID, ss.pos, rerr)
			return nil
		}

		// 主动轮换:这条连接搬够量且文件还有剩余。客户端侧字节流连续,不可感知。
		if canRotate && connBytes >= rotateBytes && ss.pos <= ss.limit {
			next, fellBack, rerr2 := s.rotate(ctx, ss)
			if rerr2 != nil {
				s.countAbort(in, relayAbortRotate)
				s.log.Warnf("relay rotate failed: share=%s fid=%s pos=%d err=%v",
					in.ShareKey, in.FID, ss.pos, rerr2)
				return nil
			}
			conn.close()
			conn = next
			connBytes = 0
			if fellBack {
				// 上游忽略 Range 却声明了 Accept-Ranges:再轮换就会把 pos 字节反复重下
				// (256 MB 起跳的带宽放大),所以这条连接直接跑到底。
				canRotate = false
				s.log.Warnf("relay rotation disabled for stream: upstream ignores range, share=%s fid=%s pos=%d",
					in.ShareKey, in.FID, ss.pos)
			}
			if s.met != nil {
				s.met.Inc(relayMetricRotate, map[string]string{"pan": in.Pan})
			}
		}
	}
}

// rotate 从 ss.pos 处重开一条上游连接(轮换与续传共用)。Range 一定收窄到 ss.limit,
// 保证不会读到客户端没要的字节;fellBack=true 表示上游忽略 Range(回 200),
// 此时已丢弃 pos 字节续用这条连接,调用方必须停用后续轮换。
func (s *Relay) rotate(ctx context.Context, ss *streamSession) (*upstreamConn, bool, error) {
	in := ss.in
	conn, err := s.openConn(ctx, in, ss.link, fmt.Sprintf("bytes=%d-%d", ss.pos, ss.limit))
	if err != nil {
		return nil, false, err
	}
	// 校验器基线:轮换边界前后的两个响应必须来自同一个对象,否则续写等于把
	// 两个版本的文件拼在一起(客户端拿到「长度正确但内容错」的文件,比报错更糟)。
	if !sameObject(ss.orig, conn.resp.Header) {
		conn.close()
		return nil, false, fmt.Errorf("upstream object changed at pos %d (validator mismatch)", ss.pos)
	}
	switch {
	case conn.resp.StatusCode == http.StatusPartialContent:
		if st, _, ok := parseContentRange(conn.resp.Header.Get("Content-Range")); ok && st == ss.pos {
			return conn, false, nil
		}
		conn.close()
		return nil, false, fmt.Errorf("content-range start mismatch, want %d", ss.pos)
	case conn.resp.StatusCode == http.StatusOK:
		// 上游忽略 Range:丢掉已有的 pos 字节继续用(用带宽换正确性)。
		// 丢前缀时也要喂看门狗,否则 pos 很大时(256 MB 起)会被判成挂死。
		if _, err := io.CopyN(io.Discard, touchReader{conn}, ss.pos); err != nil {
			conn.close()
			return nil, false, fmt.Errorf("discard %d bytes: %w", ss.pos, err)
		}
		conn.touch()
		s.log.Warnf("relay upstream ignored range: share=%s fid=%s pos=%d", in.ShareKey, in.FID, ss.pos)
		return conn, true, nil
	}

	code := conn.resp.StatusCode
	conn.close()
	if isStaleStatus(code) && !ss.refreshed {
		ss.refreshed = true
		s.expireLink(in)
		nl, ferr := s.freshLink(ctx, in)
		if ferr != nil {
			return nil, false, ferr
		}
		ss.link = nl
		return s.rotate(ctx, ss)
	}
	return nil, false, fmt.Errorf("upstream %d on rotate", code)
}

// sameObject 用 ETag/Last-Modified 判断两次响应是否同一对象。上游不给校验器时
// 无法判定,沿用「假定没变」的旧行为(总比不续传强)。
func sameObject(orig, cur http.Header) bool {
	if a, b := orig.Get("ETag"), cur.Get("ETag"); a != "" && b != "" {
		return a == b
	}
	if a, b := orig.Get("Last-Modified"), cur.Get("Last-Modified"); a != "" && b != "" {
		return a == b
	}
	return true
}

// rangeEndLimit 求客户端允许的「包含式终点」。
//
// 只有 206 才把客户端的 Range 当回事:上游回 200 说明它忽略了 Range(整段都是
// 客户端该收的),此时按整段末尾处理,免得把合法响应截短。多段 Range 或无法
// 解析的语法返回 ok=false,调用方据此禁用轮换。
func rangeEndLimit(rangeHeader string, total int64, status int) (int64, bool) {
	if total <= 0 {
		return 0, false
	}
	if status != http.StatusPartialContent || rangeHeader == "" {
		return total - 1, true
	}
	h := strings.TrimSpace(rangeHeader)
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	if strings.Contains(spec, ",") {
		return 0, false
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		return total - 1, true // bytes=-N:取末尾 N 字节,终点就是文件尾
	}
	// 起点也要可解析:bytes=abc-200 若只校验终点,就会被当成「从 abc 开始」放过去,
	// 错误被推迟到上游 416,定位成本更高。
	if _, err := strconv.ParseInt(first, 10, 64); err != nil {
		return 0, false
	}
	if last == "" {
		return total - 1, true // bytes=A-:到文件尾
	}
	end, err := strconv.ParseInt(last, 10, 64)
	if err != nil || end < 0 {
		return 0, false
	}
	if end >= total {
		end = total - 1
	}
	return end, true
}

func (s *Relay) freshLink(ctx context.Context, in StreamInput) (streamLink, error) {
	link, err := s.resolver.GetFreshLink(ctx, in.ShareKey, in.FID, in.ClientUA)
	if err != nil {
		return streamLink{}, err
	}
	cookie, err := s.aes.DecryptBytes(link.CookieEnc)
	if err != nil {
		return streamLink{}, fmt.Errorf("decrypt link cookie: %w", err)
	}
	return streamLink{URL: link.DirectLink, UA: link.UA, Referer: link.Referer, Cookie: cookie}, nil
}

// openConn 建立一条上游连接并启动停流看门狗。
func (s *Relay) openConn(ctx context.Context, in StreamInput, link streamLink, rangeHeader string) (*upstreamConn, error) {
	upCtx, cancel := context.WithCancel(ctx)
	resp, err := s.openUpstream(upCtx, in, link, rangeHeader)
	if err != nil {
		cancel()
		return nil, err
	}
	c := &upstreamConn{resp: resp, cancel: cancel, stop: make(chan struct{})}
	if s.met != nil {
		c.onStall = func() { s.met.Inc(relayMetricStall, map[string]string{"pan": in.Pan}) }
	}
	c.touch()
	if stall := s.stallLimit(); stall > 0 {
		go c.watchStall(stall)
	}
	return c, nil
}

func (s *Relay) openUpstream(ctx context.Context, in StreamInput, link streamLink, rangeHeader string) (*http.Response, error) {
	client := s.clients[in.Pan]
	if client == nil {
		return nil, fmt.Errorf("no http client for pan %q", in.Pan)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "构造上游请求失败", err)
	}
	if link.UA != "" {
		req.Header.Set("User-Agent", link.UA)
	}
	if link.Referer != "" {
		req.Header.Set("Referer", link.Referer)
	}
	if len(link.Cookie) > 0 {
		req.Header.Set("Cookie", string(link.Cookie))
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	// 显式声明 identity:一是文件字节必须原样透传(Go 只在「自己加的 Accept-Encoding」
	// 场景才透明解压,显式声明后就不再插手),二是解压会抹掉 Content-Length、让
	// Range/续传的边界对不上——下载场景压缩毫无收益。
	req.Header.Set("Accept-Encoding", "identity")
	// 上游一律走新连接(DoStreamFresh):一是避免复用一条已被网盘降速的旧连接,
	// 二是中转流量本身不回到池里,不把受降速的连接传染给下一个请求。
	resp, err := client.DoStreamFresh(req)
	if err != nil {
		return nil, driver.NewErr(driver.KindUpstream, "中转上游请求失败", err)
	}
	return resp, nil
}

func (s *Relay) expireLink(in StreamInput) {
	if err := s.store.ExpireLink(in.ShareKey, in.FID); err != nil {
		s.log.Warnf("expire stale link: share=%s fid=%s err=%v", in.ShareKey, in.FID, err)
	}
}

func (s *Relay) countAbort(in StreamInput, reason string) {
	if s.met != nil {
		s.met.Inc(relayMetricAbort, map[string]string{"pan": in.Pan, "reason": reason})
	}
}

// metActiveStream 增减 streams_active gauge(delta 为 +1/-1)。
func (s *Relay) metActiveStream(in StreamInput, delta float64) {
	if s.met != nil {
		s.met.AddGauge(relayMetricActive, map[string]string{"pan": in.Pan}, delta)
	}
}

// isStaleStatus:上游明确否决(直链已不可用)的状态码。
func isStaleStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusGone, http.StatusPreconditionFailed:
		return true
	}
	return false
}

// parseContentRange 解析 "bytes <start>-<end>/<total>"(total 可为 *)。
func parseContentRange(v string) (start, total int64, ok bool) {
	const prefix = "bytes "
	if !strings.HasPrefix(v, prefix) {
		return 0, 0, false
	}
	rest := strings.TrimPrefix(v, prefix)
	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		return 0, 0, false
	}
	span, totalPart := rest[:slash], rest[slash+1:]
	dash := strings.Index(span, "-")
	if dash <= 0 {
		return 0, 0, false
	}
	st, err := strconv.ParseInt(span[:dash], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if totalPart != "*" {
		if t, terr := strconv.ParseInt(totalPart, 10, 64); terr == nil {
			total = t
		}
	}
	return st, total, true
}
