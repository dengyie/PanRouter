package httpx

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
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

func skipChallengeInspect(resp *http.Response) bool {
	if resp.StatusCode == http.StatusPartialContent {
		return true
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return true
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength > maxChallengePeek {
		return true
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	switch {
	case strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"), strings.HasPrefix(ct, "image/"):
		return true
	case ct == "application/octet-stream", ct == "application/zip", ct == "application/pdf":
		return true
	}
	return false
}

type readCloser struct {
	io.Reader
	io.Closer
}

// inspectChallenge 窥探响应是否为 acw 挑战页。
// 非挑战:把已读字节塞回 Body;挑战:耗尽并关闭 Body,返回页内容。
func inspectChallenge(resp *http.Response) (page []byte, challenge bool, err error) {
	if skipChallengeInspect(resp) {
		return nil, false, nil
	}
	peek, err := io.ReadAll(io.LimitReader(resp.Body, maxChallengePeek))
	if err != nil {
		resp.Body.Close()
		return nil, false, fmt.Errorf("read response: %w", err)
	}
	if !isACWChallenge(resp.StatusCode, peek) {
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(peek), resp.Body), Closer: resp.Body}
		return peek, false, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
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
