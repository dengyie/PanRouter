// Relay:服务端中转流——补请求头、白名单重定向、Range 透传与续传。
package service

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
	"github.com/dengyie/panrouter/internal/repo"
)

// aesCipher 解耦 crypto 包,便于单测注入。
type aesCipher interface {
	EncryptBytes(plain []byte) ([]byte, error)
	DecryptBytes(data []byte) ([]byte, error)
}

type Relay struct {
	resolver *Resolver
	clients  map[string]*httpx.Client // pan → client(带 per-driver 重定向白名单)
	store    *repo.Store
	aes      aesCipher
	log      *loggerType
}

func NewRelay(resolver *Resolver, clients map[string]*httpx.Client, store *repo.Store, aes aesCipher, log *loggerType) *Relay {
	return &Relay{resolver: resolver, clients: clients, store: store, aes: aes, log: log}
}

// StreamInput 是一次中转请求的已验证参数(签名校验在 api 层完成)。
type StreamInput struct {
	Pan      string
	ShareKey string
	FID      string
	ClientUA string
}

// Serve 执行中转:取新鲜直链 → 携带约束头请求上游 → 透传响应(含 Range/续传)。
// 签名只在建立连接时校验,Range 续传复用同一 URL,不会中途 401。
func (s *Relay) Serve(ctx context.Context, w http.ResponseWriter, in StreamInput, rangeHeader string) error {
	link, err := s.resolver.GetFreshLink(ctx, in.ShareKey, in.FID, in.ClientUA)
	if err != nil {
		return err
	}
	cookie, err := s.aes.DecryptBytes(link.CookieEnc)
	if err != nil {
		return fmt.Errorf("decrypt link cookie: %w", err)
	}
	client := s.clients[in.Pan]
	if client == nil {
		return fmt.Errorf("no http client for pan %q", in.Pan)
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodGet, link.DirectLink, nil)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "构造上游请求失败", err)
	}
	if link.UA != "" {
		upReq.Header.Set("User-Agent", link.UA)
	}
	if link.Referer != "" {
		upReq.Header.Set("Referer", link.Referer)
	}
	if len(cookie) > 0 && string(cookie) != "" {
		upReq.Header.Set("Cookie", string(cookie))
	}
	if rangeHeader != "" {
		upReq.Header.Set("Range", rangeHeader)
	}

	resp, err := client.DoStream(upReq)
	if err != nil {
		return driver.NewErr(driver.KindUpstream, "中转上游请求失败", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// 直链可能提前失效(风控/过期):尚未写响应头,可安全报错让上层提示重试
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		// 上游明确否决时立即失效本地缓存,否则重新解析仍命中同一死链(缓存优先),
		// 用户会在 TTL 内 (最长 2h) 反复失败。失效后下次 GetFreshLink 触发真实重解析。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone ||
			resp.StatusCode == http.StatusPreconditionFailed {
			if err := s.store.ExpireLink(in.ShareKey, in.FID); err != nil {
				s.log.Warnf("expire stale link: share=%s fid=%s err=%v", in.ShareKey, in.FID, err)
			}
		}
		return driver.NewErr(driver.KindUpstream,
			fmt.Sprintf("上游返回 %d,直链可能已失效,请重新解析", resp.StatusCode), nil)
	}

	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil // 客户端断开,属正常取消
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			// 上游中断:响应头已发出,无法改写状态码;记录日志供排查
			s.log.Warnf("relay upstream interrupted: share=%s fid=%s err=%v", in.ShareKey, in.FID, rerr)
			return nil
		}
	}
}
