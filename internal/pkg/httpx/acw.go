package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 蓝奏云 CDN(dmpdmp/lanrar)对非浏览器客户端下发的 Aliyun WAF acw_sc__v2 挑战。
// 算法:posList 重排 + hexXor;密钥与 posList 为线上已验证常量。
const acwXORKey = "3000176000856006061501533003690027800375"

// acwPosList[i] 是 1-based 下标,指向 arg1 中应放到输出位置 i 的字符。
var acwPosList = [40]byte{
	0x0f, 0x23, 0x1d, 0x18, 0x21, 0x10, 0x01, 0x26, 0x0a, 0x09,
	0x13, 0x1f, 0x28, 0x1b, 0x16, 0x17, 0x19, 0x0d, 0x06, 0x0b,
	0x27, 0x12, 0x14, 0x08, 0x0e, 0x15, 0x20, 0x1a, 0x02, 0x1e,
	0x07, 0x04, 0x11, 0x05, 0x03, 0x1c, 0x22, 0x25, 0x0c, 0x24,
}

var arg1Re = regexp.MustCompile(`(?i)arg1\s*=\s*['"]([0-9a-f]{40})['"]`)

const (
	acwCookieName    = "acw_sc__v2"
	maxChallengePeek = 64 << 10
	maxACWReplay     = 1

	// challengeProbe 是「窥探响应体是否为挑战页」的最长读取时间。
	//
	// 为什么必须有它:中转 client 是 NoBodyTimeout=true(慢流不能被整体超时掐断),
	// 而这里的窥探发生在任何超时机制接管之前——响应体挂住时 ReadAll 会永远阻塞,
	// 调用方连 *http.Response 都还没拿到,停流看门狗无从介入。挑战页只有几 KB 的
	// HTML,不可能需要这么久;超时即判定「不是挑战页」并停止窥探(把已读字节交回),
	// 而不是把这条请求判死——毕竟这个判定只影响挑战重放,不影响字节流本身。
	challengeProbe = 5 * time.Second
)

type acwCache struct {
	mu sync.RWMutex
	m  map[string]string // host → cookie value;O(1)
}

func (a *acwCache) get(host string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.m == nil {
		return ""
	}
	return a.m[host]
}

func (a *acwCache) set(host, token string) {
	a.mu.Lock()
	if a.m == nil {
		a.m = make(map[string]string, 4)
	}
	a.m[host] = token
	a.mu.Unlock()
}

// SolveACW 从挑战页提取 arg1 并计算 acw_sc__v2。供单测与中间件共用。
func SolveACW(page []byte) (string, error) {
	m := arg1Re.FindSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("missing arg1")
	}
	return hexXor(unsbox(m[1]), acwXORKey)
}

func unsbox(arg1 []byte) []byte {
	out := make([]byte, 40)
	for i, p := range acwPosList {
		out[i] = arg1[p-1]
	}
	return out
}

func hexXor(a []byte, key string) (string, error) {
	if len(a) != 40 || len(key) != 40 {
		return "", fmt.Errorf("hexXor length: arg=%d key=%d", len(a), len(key))
	}
	const digits = "0123456789abcdef"
	var out [40]byte
	for i := 0; i < 40; i += 2 {
		v1, ok1 := hexByte(a[i], a[i+1])
		v2, ok2 := hexByte(key[i], key[i+1])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("hexXor: non-hex at %d", i)
		}
		x := v1 ^ v2
		out[i] = digits[x>>4]
		out[i+1] = digits[x&0x0f]
	}
	return string(out[:]), nil
}

func hexByte(hi, lo byte) (byte, bool) {
	h, ok1 := hexVal(hi)
	l, ok2 := hexVal(lo)
	return h<<4 | l, ok1 && ok2
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func isACWChallenge(status int, body []byte) bool {
	if len(body) == 0 || !arg1Re.Match(body) {
		return false
	}
	if bytes.Contains(body, []byte(acwCookieName)) {
		return true
	}
	return status == http.StatusPreconditionFailed
}

// shouldProbeChallenge 判断响应是否值得窥探。
//
// 挑战页只有几 KB 的 HTML。对二进制/未知长度的流做窥探没有收益,代价却真实存在:
// 首块交付要等满 64 KB 或读完;而首 64 KB 慢于 probe 时(冷 CDN、网盘限速、半挂)
// 会把一条合法慢流判死。所以反过来只窥探「可能承载挑战页」的响应:
//   - 412:阿里云 WAF 挑战的常见状态码,无条件窥探(哪怕 CT 异常);
//   - 无 Content-Type(部分 CDN 不带)或文本类(挑战页一定是 HTML/JS)。
//
// 206 是 Range 命中、3xx 由 CheckRedirect 内部消化,都不可能是挑战页。
func shouldProbeChallenge(resp *http.Response) bool {
	if resp.StatusCode == http.StatusPartialContent || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		return false
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return true
	}
	if resp.ContentLength > maxChallengePeek {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch ct {
	case "", "text/html", "application/xhtml+xml", "text/plain",
		"text/javascript", "application/javascript", "application/json":
		return true
	}
	return strings.HasPrefix(ct, "text/")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// inspectChallenge 窥探响应是否为 acw 挑战页。
// 非挑战:把已读字节塞回 Body;挑战:耗尽并关闭 Body,返回页内容。
//
// 窥探失败必须返回错误,不能把已读前缀伪装成完整响应:relay 会把「Body 读到 EOF」
// 当正常收尾,chunked 下客户端于是拿到一个「HTTP 完整」的截断文件且无任何错误。
func inspectChallenge(resp *http.Response, probe time.Duration) (page []byte, challenge bool, err error) {
	if !shouldProbeChallenge(resp) {
		return nil, false, nil
	}
	// 闭包只捕获局部变量:若捕获 resp.Body 字段,会与下面替换 resp.Body 形成数据竞争
	// (Stop 不等待定时器函数结束)。
	body := resp.Body
	var timedOut atomic.Bool
	stop := time.AfterFunc(probe, func() {
		timedOut.Store(true)
		_ = body.Close() // 打断阻塞中的 Read;Close 之后的 Read 立即返回错误
	})
	peek, rerr := io.ReadAll(io.LimitReader(body, maxChallengePeek))
	stopped := stop.Stop()
	if rerr != nil {
		// 窥探被超时打断(或读体本身出错):响应体已不可继续读。此时尚未下发响应头,
		// 干净的失败好过一条静默截断的流。
		if timedOut.Load() {
			return peek, false, fmt.Errorf("challenge probe timed out after %v: %w", probe, rerr)
		}
		return peek, false, fmt.Errorf("challenge probe read: %w", rerr)
	}
	if !stopped {
		// 竞态窗口:ReadAll 成功返回的同时定时器已开跑并关掉了 Body。ReadAll 返回 nil
		// 且未达上限 ⇒ 已读到 EOF,peek 就是完整体,可以安全交回;达到上限则无法确认
		// 尾部,按超时失败处理(返回一条读不动的半截流更糟)。
		if len(peek) < maxChallengePeek {
			resp.Body = io.NopCloser(bytes.NewReader(peek))
			return peek, false, nil
		}
		return peek, false, fmt.Errorf("challenge probe timed out after %v", probe)
	}
	if !isACWChallenge(resp.StatusCode, peek) {
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(peek), body), Closer: body}
		return peek, false, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
	return peek, true, nil
}

func setCookieKV(req *http.Request, name, value string) {
	parts := make([]string, 0, 4)
	replaced := false
	for _, p := range strings.Split(req.Header.Get("Cookie"), ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			parts = append(parts, name+"="+value)
			replaced = true
			continue
		}
		parts = append(parts, p)
	}
	if !replaced {
		parts = append(parts, name+"="+value)
	}
	req.Header.Set("Cookie", strings.Join(parts, "; "))
}
