package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/repo"
)

// P2-5 回归:下载入口签名必须使用配置 sign_ttl,与上游直链剩余有效期(ExpiresAt)解耦;
// 短 TTL 直链过期后签名仍有效,GetFreshLink 可在入口有效期内续命。
func TestBuildResultUsesConfigSignTTL(t *testing.T) {
	resolver, _ := newTestResolver(t, &twoFileDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	cfg := resolver.cfg.Get()
	cfg.Server.SignTTL = 2 * time.Hour
	resolver.cfg.Set(cfg)

	res, err := resolver.ResolveFile(context.Background(), testShareURL, "", "f1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	// twoFileDriver 默认直链 1h 后过期;旧实现签名 TTL 会跟随直链剩余时间
	u, err := url.Parse(res.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	sig := u.Query().Get("sig")
	expB64, _, ok := strings.Cut(sig, ".")
	if !ok {
		t.Fatalf("bad sig shape: %q", sig)
	}
	expBytes, err := base64.RawURLEncoding.DecodeString(expB64)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(string(expBytes), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	remain := time.Until(time.Unix(exp, 0))
	if remain < 119*time.Minute || remain > 121*time.Minute {
		t.Fatalf("入口签名 TTL 应为 sign_ttl(2h),实际剩余 %v", remain)
	}
	payload := "fake|" + res.ShareKey + "|f1"
	if err := resolver.signer.Verify(payload, sig); err != nil {
		t.Fatalf("签名应可用当前 signer 校验: %v", err)
	}
}

// P2-6 回归:直链缓存读取失败(DB 故障)必须显式报错,不得静默当作 cache miss 触发真实提链。
func TestResolveFilePropagatesCacheReadFailure(t *testing.T) {
	d := &twoFileDriver{}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := resolver.ResolveFile(context.Background(), testShareURL, "", "f1", false, "")
	if err == nil || !strings.Contains(err.Error(), "query link cache") {
		t.Fatalf("缓存读取失败必须传播, got %v", err)
	}
	if d.calls.Load() != 0 {
		t.Fatalf("DB 故障不得触发真实提链, calls=%d", d.calls.Load())
	}

	_, err = resolver.GetFreshLink(context.Background(), resolver.shareKey("fake", testShareURL, ""), "f1", "")
	if err == nil || !strings.Contains(err.Error(), "query link cache") {
		t.Fatalf("GetFreshLink 缓存读取失败必须传播, got %v", err)
	}
}

// P2-12 回归:singleflight 共享刷新任务不得绑定首个调用者的 ctx。
// 首调取消后,共享任务必须继续完成,后续等待者仍能拿到结果;首调自身响应其 ctx 取消。
type gateDriver struct {
	fakeDriver
	enteredOnce sync.Once
	entered     chan struct{}
	gate        chan struct{}
}

func (d *gateDriver) GetDirectLink(ctx context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	d.enteredOnce.Do(func() { close(d.entered) })
	<-d.gate
	return d.fakeDriver.GetDirectLink(ctx, cred, ref)
}

func TestGetFreshLinkFirstCallerCancelDoesNotKillWaiters(t *testing.T) {
	gd := &gateDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}, entered: make(chan struct{}), gate: make(chan struct{})}
	resolver, store := newTestResolver(t, gd, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	key := resolver.shareKey("fake", testShareURL, "")
	// GetFreshLink 需要 share 快照存在;直接落库,不经过会阻塞在 gate 的自动提链
	if err := store.UpsertShare(&repo.Share{
		PanType: "fake", ShareURL: testShareURL, ShareKey: key,
		RawTree:    `[{"fid":"f1","name":"n.bin","size":1,"is_dir":false,"ext":{}}]`,
		ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	type res struct {
		link *repo.Link
		err  error
	}
	ctx1, cancel := context.WithCancel(context.Background())
	ch1 := make(chan res, 1)
	go func() {
		l, e := resolver.GetFreshLink(ctx1, key, "f1", "")
		ch1 <- res{l, e}
	}()
	<-gd.entered // 共享任务已进入真实刷新
	cancel()     // 首调取消

	ch2 := make(chan res, 1)
	go func() {
		l, e := resolver.GetFreshLink(context.Background(), key, "f1", "")
		ch2 <- res{l, e}
	}()

	close(gd.gate) // 放行真实刷新
	r1 := <-ch1
	if !errors.Is(r1.err, context.Canceled) {
		t.Fatalf("首调应响应自身 ctx 取消, got %v", r1.err)
	}
	r2 := <-ch2
	if r2.err != nil {
		t.Fatalf("等待者应在首调取消后仍拿到结果, got %v", r2.err)
	}
	if r2.link == nil {
		t.Fatal("nil link")
	}
}
