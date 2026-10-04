package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/crypto"
	"github.com/dengyie/panrouter/internal/repo"
)

// twoFileDriver 列出两个文件,GetDirectLink 行为由 fn 决定。
type twoFileDriver struct {
	calls atomic.Int32
	fn    func(ctx context.Context, fid string) (driver.DirectLink, error)
}

func (d *twoFileDriver) ID() string { return "fake" }
func (d *twoFileDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{
		{FID: "f1", Name: "a.bin", Size: 1},
		{FID: "f2", Name: "b.bin", Size: 1},
	}, nil
}
func (d *twoFileDriver) GetDirectLink(ctx context.Context, _ *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	d.calls.Add(1)
	if d.fn != nil {
		return d.fn(ctx, ref.FID)
	}
	return driver.DirectLink{URL: "https://up.invalid/" + ref.FID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (d *twoFileDriver) CheckCredential(context.Context, driver.Credential) (driver.CredStatus, error) {
	return driver.CredStatus{Valid: true}, nil
}

type truncListDriver struct{ fakeDriver }

func (d *truncListDriver) ResolveShare(context.Context, driver.ShareLink, *driver.Credential) ([]driver.FileNode, error) {
	return []driver.FileNode{{
		FID: "f1", Name: "a.bin", Size: 1,
		Ext: map[string]string{"truncated": "1"},
	}}, nil
}

func TestResolveShareAuthExpiredStops(t *testing.T) {
	d := &twoFileDriver{fn: func(context.Context, string) (driver.DirectLink, error) {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "Cookie 已失效,请到账号页更新", nil)
	}}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveShare(WithAuthed(context.Background(), true), testShareURL, "", "")
	if err != nil {
		t.Fatalf("ResolveShare 遇 AuthExpired 应仍返回列表, got %v", err)
	}
	if res.Hint == "" || !strings.Contains(res.Hint, "账号") {
		t.Fatalf("hint 应提示加账号, got %q", res.Hint)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files=%d", len(res.Files))
	}
	if res.Files[0].LinkError == "" {
		t.Fatalf("首个文件应有 link_error: %+v", res.Files[0])
	}
	if res.Files[0].DownloadURL != "" {
		t.Fatalf("AuthExpired 不应给出 download_url")
	}
	if res.Files[1].LinkError != "" || res.Files[1].DownloadURL != "" {
		t.Fatalf("停后续提链后第二文件应保持未提链: %+v", res.Files[1])
	}
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("AuthExpired 后应只调一次 GetDirectLink, 实际 %d", got)
	}
}

func TestResolveShareDeadlineSkipsAutoLink(t *testing.T) {
	d := &twoFileDriver{}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := resolver.ResolveShare(ctx, testShareURL, "", "")
	if err != nil {
		t.Fatalf("列表仍应成功: %v", err)
	}
	if d.calls.Load() != 0 {
		t.Fatalf("剩余时间不足时不应自动提链, calls=%d", d.calls.Load())
	}
	if !strings.Contains(res.Hint, "剩余时间不足") {
		t.Fatalf("hint=%q", res.Hint)
	}
	for _, f := range res.Files {
		if f.DownloadURL != "" {
			t.Fatalf("不应带 download_url: %+v", f)
		}
	}
}

func TestResolveShareTruncationHint(t *testing.T) {
	resolver, _ := newTestResolver(t, &truncListDriver{}, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("truncated 应为 true")
	}
	if !strings.Contains(res.Hint, "仅展开前") {
		t.Fatalf("hint=%q", res.Hint)
	}
}

type credSpyDriver struct {
	fakeDriver
	cookie atomic.Value // string
	calls  atomic.Int32
}

func (d *credSpyDriver) GetDirectLink(_ context.Context, cred *driver.Credential, ref driver.FileRef) (driver.DirectLink, error) {
	cookie := ""
	if cred != nil {
		cookie = cred.Cookie
	}
	d.cookie.Store(cookie)
	d.calls.Add(1)
	dl, err := d.fakeDriver.GetDirectLink(context.Background(), cred, ref)
	dl.Cookie = cookie
	return dl, err
}

func seedCookieAccount(t *testing.T, store *repo.Store) {
	t.Helper()
	enc, err := crypto.New("k").EncryptBytes([]byte("secret-cookie"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(&repo.Account{PanType: "fake", Name: "t", CredEnc: enc, Status: "ok", CredVersion: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestGuestResolveSkipsAccountCookie(t *testing.T) {
	d := &credSpyDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	seedCookieAccount(t, store)

	if _, err := resolver.ResolveShare(context.Background(), testShareURL, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := d.cookie.Load().(string)
	if got != "" {
		t.Fatalf("游客不得读取账号 Cookie, got %q", got)
	}
	if err := store.DB().Where("1 = 1").Delete(&repo.Link{}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := resolver.ResolveShare(WithAuthed(context.Background(), true), testShareURL, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = d.cookie.Load().(string)
	if got != "secret-cookie" {
		t.Fatalf("登录后应使用账号 Cookie, got %q", got)
	}
}

func TestResolveShareGuestAuthExpiredHint(t *testing.T) {
	d := &twoFileDriver{fn: func(context.Context, string) (driver.DirectLink, error) {
		return driver.DirectLink{}, driver.NewErr(driver.KindAuthExpired, "夸克分享直链需要登录态", nil)
	}}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})

	res, err := resolver.ResolveShare(context.Background(), testShareURL, "", "")
	if err != nil {
		t.Fatalf("列表仍应成功: %v", err)
	}
	if !strings.Contains(res.Hint, "请先登录") {
		t.Fatalf("游客 AuthExpired 应提示登录, got %q", res.Hint)
	}
	if strings.Contains(res.Hint, "账号管理") {
		t.Fatalf("游客不应被指到账号管理: %q", res.Hint)
	}
}

func TestGuestResolveFileSkipsCookieCache(t *testing.T) {
	d := &credSpyDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	seedCookieAccount(t, store)

	if _, err := resolver.ResolveFile(WithAuthed(context.Background(), true), testShareURL, "", "f1", false, ""); err != nil {
		t.Fatal(err)
	}
	d.calls.Store(0)
	d.cookie.Store("stale")

	_, err := resolver.ResolveFile(context.Background(), testShareURL, "", "f1", false, "")
	if err == nil {
		t.Fatal("游客不得命中带 Cookie 的直链缓存")
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.KindAuthExpired {
		t.Fatalf("want AuthExpired, got %v", err)
	}
	if !strings.Contains(de.UserHint, "请先登录") {
		t.Fatalf("hint=%q", de.UserHint)
	}
	if d.calls.Load() != 0 {
		t.Fatalf("游客不应重提链, calls=%d", d.calls.Load())
	}
	if got, _ := d.cookie.Load().(string); got != "stale" {
		t.Fatalf("游客不得读取账号 Cookie, got %q", got)
	}
}

func TestGetFreshLinkRenewsExpiredCookieLink(t *testing.T) {
	d := &credSpyDriver{fakeDriver: fakeDriver{linkURL: "https://up.invalid/f"}}
	resolver, store := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	seedCookieAccount(t, store)

	if _, err := resolver.ResolveShare(WithAuthed(context.Background(), true), testShareURL, "", ""); err != nil {
		t.Fatal(err)
	}
	key := resolver.shareKey("fake", testShareURL, "")
	if err := store.DB().Model(&repo.Link{}).Where("share_key = ? AND fid = ?", key, "f1").
		Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	d.cookie.Store("")
	d.calls.Store(0)

	link, err := resolver.GetFreshLink(context.Background(), key, "f1", "")
	if err != nil {
		t.Fatalf("已签发下载过期后应能续命: %v", err)
	}
	if link == nil {
		t.Fatal("nil link")
	}
	got, _ := d.cookie.Load().(string)
	if got != "secret-cookie" {
		t.Fatalf("续命应 Pick 账号 Cookie, got %q", got)
	}
	if d.calls.Load() != 1 {
		t.Fatalf("calls=%d", d.calls.Load())
	}
}

func TestGuardKeySplitsGuest(t *testing.T) {
	r := &Resolver{}
	if got := r.guardKey(context.Background(), "quark"); got != "quark:guest" {
		t.Fatalf("guest key=%q", got)
	}
	if got := r.guardKey(WithAuthed(context.Background(), true), "quark"); got != "quark" {
		t.Fatalf("authed key=%q", got)
	}
}

func TestAutoLinkMaxFor(t *testing.T) {
	if autoLinkMaxFor("quark") != quarkAutoLinkMax {
		t.Fatalf("quark auto-link max=%d want %d", autoLinkMaxFor("quark"), quarkAutoLinkMax)
	}
	if autoLinkMaxFor("lanzou") != defaultAutoLinkMax {
		t.Fatalf("lanzou auto-link max=%d want %d", autoLinkMaxFor("lanzou"), defaultAutoLinkMax)
	}
}

func TestJoinHints(t *testing.T) {
	if got := joinHints("", "a", "", "b"); got != "a;b" {
		t.Fatalf("got %q", got)
	}
}

func TestCanAutoLink(t *testing.T) {
	if !canAutoLink(context.Background()) {
		t.Fatal("无 deadline 应允许提链")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if canAutoLink(ctx) {
		t.Fatal("剩余 <15s 不应继续提链")
	}
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if canAutoLink(done) {
		t.Fatal("已取消不应继续提链")
	}
}
