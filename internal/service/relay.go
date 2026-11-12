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
)

var (
	// relayRotateBytes:单条上游连接最多搬运这么多字节就重开一条。
	//
	// 2026-11-13 实测(51 文件 / 13.16 GB 真实下载):quark 对**同一条上游长连接**
	// 在累计约 1 GB 后把速度压到 ~100 KB/s,而新建连接立刻回到 3.5 MB/s——三条老
	// 连接合计 0.32 MB/s、两条新连接合计 7.2 MB/s,同期账号状态始终 ok(既不是
	// 我方限频,也不是账号被风控)。旧实现一条连接跑到底,417 MB 文件要 185~220 s;
	// 按块换连接后 143~167 s,两个 576 MB 文件从「重试耗尽失败」变成正常跑完。
	//
	// 所以这里按固定字节数主动轮换上游连接:响应头早已发出,客户端侧字节流连续。
	relayRotateBytes int64 = 256 << 20

	// relayStallTimeout 的默认值,以及为什么需要它:stream client 是 NoBodyTimeout=true
	// (慢流不能被整体超时掐断),代价是上游真挂死时 Read 会一直阻塞,客户端连接就这么吊着——
	// 用户看到的是「0 B/s 卡死」而不是一个明确结局。有了看门狗,挂死会退化成一次有界续传/报错。
	//
	// 放在 Relay 上而不是包级变量:看门狗在独立 goroutine 里读它,包级可变变量会被
	// 单测的「改了再改回去」撞出 data race(`-race` 实测)。字段在建连那一刻取值,
	// 之后只读,天然无竞争。
	defaultRelayStallTimeout = 60 * time.Second
)

type Relay struct {
	resolver     *Resolver
	clients      map[string]*httpx.Client // pan → client(带 per-driver 重定向白名单)
	store        *repo.Store
	aes          aesCipher
	met          *metrics.Registry
	log          *loggerType
	stallTimeout time.Duration
}

func NewRelay(resolver *Resolver, clients map[string]*httpx.Client, store *repo.Store,
	aes aesCipher, met *metrics.Registry, log *loggerType) *Relay {
	s := &Relay{resolver: resolver, clients: clients, store: store, aes: aes, met: met, log: log,
		stallTimeout: defaultRelayStallTimeout}
	if met != nil {
		met.Help("panrouter_relay_bytes_total", "Relay 转发给客户端的字节数(按网盘)")
		met.Help("panrouter_relay_rotate_total", "Relay 主动轮换上游连接的次数")
		met.Help("panrouter_relay_resume_total", "Relay 上游断流后按 Range 续传成功的次数")
		met.Help("panrouter_relay_abort_total", "Relay 未正常读完就结束的次数(按原因)")
		// 先把各网盘的 0 值序列建出来:没有序列和「全 0」在监控上不可区分。
		for pan := range clients {
			met.Add("panrouter_relay_bytes_total", map[string]string{"pan": pan}, 0)
			met.Add("panrouter_relay_rotate_total", map[string]string{"pan": pan}, 0)
			met.Add("panrouter_relay_resume_total", map[string]string{"pan": pan}, 0)
			for _, reason := range []string{"client_gone", "upstream_interrupt", "rotate_failed"} {
				met.Add("panrouter_relay_abort_total", map[string]string{"pan": pan, "reason": reason}, 0)
			}
		}
	}
	return s
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

// upstreamConn 是一条上游连接 + 它专用的可取消 ctx + 停流看门狗。
// 每条连接独立 ctx:看门狗靠取消它来打断挂死的 Read,而客户端 ctx 不受影响,
// 所以「上游挂死」和「客户端走了」在错误归因上依然可分。
type upstreamConn struct {
	resp   *http.Response
	cancel context.CancelFunc
	last   atomic.Int64 // 最近一次读到字节的 UnixNano
	stop   chan struct{}
	once   sync.Once
}

func (c *upstreamConn) touch() { c.last.Store(time.Now().UnixNano()) }

// close 幂等:Body.Close / cancel / close(stop) 各只做一次。
func (c *upstreamConn) close() {
	c.once.Do(func() {
		close(c.stop)
		c.cancel()
		_ = c.resp.Body.Close()
	})
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
			if time.Since(time.Unix(0, c.last.Load())) >= timeout {
				c.cancel() // 打断 Read,交给 Serve 的续传逻辑
				return
			}
		}
	}
}

// Serve 执行中转:取新鲜直链 → 携带约束头请求上游 → 透传响应(含 Range/续传)。
// 签名只在建立连接时校验;轮换/续传复用同一 URL,不会中途 401。
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
	// 只有「知道总长度 + 上游支持 Range」时才轮换/续传;否则保持旧行为(单连接跑到底),
	// 避免为了一次性小响应(如 lanzou 的 HTML 挑战页)白折腾。
	// relayRotateBytes<=0 视为关闭轮换(单测/排障用)。
	canRotate := relayRotateBytes > 0 && total > 0 &&
		(hasCR || strings.EqualFold(conn.resp.Header.Get("Accept-Ranges"), "bytes"))

	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := conn.resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(conn.resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, relayBufSize)
	pos := start          // 下一条上游请求应从哪个绝对偏移续
	connBytes := int64(0) // 当前这条上游连接已搬运的字节
	pending := int64(0)   // 尚未 Flush 的字节
	refreshed := false
	resumes := 0

	for {
		n, rerr := conn.resp.Body.Read(buf)
		if n > 0 {
			conn.touch() // 喂看门狗:有字节流动就不是挂死
			if _, werr := w.Write(buf[:n]); werr != nil {
				s.countAbort(in, "client_gone")
				return nil // 客户端断开,属正常取消
			}
			if s.met != nil {
				s.met.Add("panrouter_relay_bytes_total", map[string]string{"pan": in.Pan}, float64(n))
			}
			pos += int64(n)
			connBytes += int64(n)
			pending += int64(n)
			if flusher != nil && pending >= relayFlushBytes {
				flusher.Flush()
				pending = 0
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
				s.countAbort(in, "client_gone")
				return nil
			}
			// 上游自己断了(连接被掐断/停流看门狗取消)。响应头和 Content-Length 早已发出,
			// 直接收尾只能让客户端收到「传输中断」的半截文件;能 Range 续传就续,续不成才算中断。
			if canRotate && pos < total && resumes < relayResumeMax {
				next, rerr2 := s.rotate(ctx, in, &link, pos, &refreshed)
				if rerr2 == nil {
					conn.close()
					conn = next
					connBytes = 0
					resumes++
					if s.met != nil {
						s.met.Inc("panrouter_relay_resume_total", map[string]string{"pan": in.Pan})
					}
					s.log.Warnf("relay resumed after upstream error: share=%s fid=%s pos=%d try=%d err=%v",
						in.ShareKey, in.FID, pos, resumes, rerr)
					continue
				}
				s.log.Warnf("relay resume failed: share=%s fid=%s pos=%d err=%v",
					in.ShareKey, in.FID, pos, rerr2)
			}
			s.countAbort(in, "upstream_interrupt")
			s.log.Warnf("relay upstream interrupted: share=%s fid=%s pos=%d err=%v",
				in.ShareKey, in.FID, pos, rerr)
			return nil
		}

		// 主动轮换:这条连接搬够量且文件还有剩余。客户端侧字节流连续,不可感知。
		if canRotate && connBytes >= relayRotateBytes && pos < total {
			next, rerr2 := s.rotate(ctx, in, &link, pos, &refreshed)
			if rerr2 != nil {
				s.countAbort(in, "rotate_failed")
				s.log.Warnf("relay rotate failed: share=%s fid=%s pos=%d err=%v",
					in.ShareKey, in.FID, pos, rerr2)
				return nil
			}
			conn.close()
			conn = next
			connBytes = 0
			if s.met != nil {
				s.met.Inc("panrouter_relay_rotate_total", map[string]string{"pan": in.Pan})
			}
		}
	}
}

// rotate 从 pos 处重开一条上游连接(轮换与续传共用)。上游忽略 Range(回 200)时
// 丢掉已有的 pos 字节继续用这条连接(用带宽换正确性);直链失效则刷新一次凭据再试。
func (s *Relay) rotate(ctx context.Context, in StreamInput, link *streamLink, pos int64, refreshed *bool) (*upstreamConn, error) {
	conn, err := s.openConn(ctx, in, *link, fmt.Sprintf("bytes=%d-", pos))
	if err != nil {
		return nil, err
	}
	switch {
	case conn.resp.StatusCode == http.StatusPartialContent:
		if st, _, ok := parseContentRange(conn.resp.Header.Get("Content-Range")); ok && st == pos {
			return conn, nil
		}
		conn.close()
		return nil, fmt.Errorf("content-range start mismatch, want %d", pos)
	case conn.resp.StatusCode == http.StatusOK:
		if _, err := io.CopyN(io.Discard, conn.resp.Body, pos); err != nil {
			conn.close()
			return nil, fmt.Errorf("discard %d bytes: %w", pos, err)
		}
		conn.touch()
		s.log.Warnf("relay upstream ignored range on rotate: share=%s fid=%s pos=%d", in.ShareKey, in.FID, pos)
		return conn, nil
	}

	code := conn.resp.StatusCode
	conn.close()
	if isStaleStatus(code) && !*refreshed {
		*refreshed = true
		s.expireLink(in)
		nl, ferr := s.freshLink(ctx, in)
		if ferr != nil {
			return nil, ferr
		}
		*link = nl
		return s.rotate(ctx, in, link, pos, refreshed)
	}
	return nil, fmt.Errorf("upstream %d on rotate", code)
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
	c.touch()
	if stall := s.stallTimeout; stall > 0 {
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
	if len(link.Cookie) > 0 && string(link.Cookie) != "" {
		req.Header.Set("Cookie", string(link.Cookie))
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
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
		s.met.Inc("panrouter_relay_abort_total", map[string]string{"pan": in.Pan, "reason": reason})
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
