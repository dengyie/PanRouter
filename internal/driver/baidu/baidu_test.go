package baidu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

func newTestDriver(t *testing.T, h http.Handler) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true, RedirectAllow: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	return New(cl, srv.URL).WithPCSBase(srv.URL)
}

func cred() *driver.Credential { return &driver.Credential{Cookie: "BDUSS=abc; STOKEN=def"} }

// 新旧 surl 格式都必须识别:新格式含 `_`/`-`(base64url),旧实现会误报 STOKEN 缺失。
func TestParseShareFormats(t *testing.T) {
	cases := []struct {
		name, in, pwd, wantSurl, wantPwd string
	}{
		{"old base62", "https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv", "", "AbCdEfGhIjKlMnOpQrStUv", ""},
		{"new base64url", "https://pan.baidu.com/s/1sjaPJJvG00g3B_aaU-zw6w", "", "sjaPJJvG00g3B_aaU-zw6w", ""},
		{"pwd in query", "https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv?pwd=ab12", "", "AbCdEfGhIjKlMnOpQrStUv", "ab12"},
		{"surl form", "https://pan.baidu.com/share/init?surl=AbCdEfGhIjKlMnOpQrStUv", "", "AbCdEfGhIjKlMnOpQrStUv", ""},
		{"explicit pwd", "https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv", "zz99", "AbCdEfGhIjKlMnOpQrStUv", "zz99"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			surl, pwd, err := parseShare(driver.ShareLink{URL: c.in, Pwd: c.pwd})
			if err != nil {
				t.Fatalf("parseShare: %v", err)
			}
			if surl != c.wantSurl || pwd != c.wantPwd {
				t.Fatalf("got surl=%q pwd=%q, want %q %q", surl, pwd, c.wantSurl, c.wantPwd)
			}
		})
	}
	if _, _, err := parseShare(driver.ShareLink{URL: "https://example.com/x"}); err == nil {
		t.Fatal("non-baidu URL must fail")
	}
}

func TestClassifyErrno(t *testing.T) {
	cases := []struct {
		errno int
		msg   string
		want  driver.Kind
	}{
		{-6, "", driver.KindAuthExpired},
		{-20, "", driver.KindAuthExpired},
		{2, "提取码错误", driver.KindNotFound},
		{105, "", driver.KindUpstream},
		{110, "", driver.KindShareGone},
		{113, "", driver.KindShareGone},
		{9019, "", driver.KindRiskControl},
		{31066, "", driver.KindRiskControl},
		{31061, "", driver.KindConflict},
		{111, "", driver.KindUpstream},
		{42, "请稍后重试", driver.KindRiskControl},
		{42, "分享已取消", driver.KindShareGone},
		{42, "unexpected", driver.KindUpstream},
	}
	for _, c := range cases {
		de := classifyErrno(c.errno, c.msg)
		if de == nil {
			t.Fatalf("errno=%d: expected error", c.errno)
		}
		if de.Kind != c.want {
			t.Fatalf("errno=%d msg=%q: kind=%s want %s", c.errno, c.msg, de.Kind, c.want)
		}
	}
	if classifyErrno(0, "") != nil {
		t.Fatal("errno 0 must be nil")
	}
}

// 分享解析:根目录 root=1;子目录 root=0 且必须带 BDCLND(=sekey);节点 Ext 带转存上下文。
func TestResolveShareWalksAndCarriesContext(t *testing.T) {
	var sawRoot, sawSub, sawBDCLND bool
	d := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/share/verify":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "randsk": "%2Bsekey%3D"})
		case "/rest/2.0/xpan/share":
			q := r.URL.Query()
			if q.Get("sekey") != "%2Bsekey%3D" {
				t.Errorf("sekey not propagated: %q", q.Get("sekey"))
			}
			if q.Get("root") == "1" {
				sawRoot = true
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errno": 0, "share_id": 12345, "uk": 67890,
					"list": []map[string]any{
						{"fs_id": 111, "isdir": "0", "path": "/a.bin", "server_filename": "a.bin", "size": 5},
						{"fs_id": 222, "isdir": "1", "path": "/sub", "server_filename": "sub"},
					},
				})
				return
			}
			if q.Get("dir") == "/sub" {
				sawSub = true
				if strings.Contains(r.Header.Get("Cookie"), "BDCLND=%2Bsekey%3D") {
					sawBDCLND = true
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errno": 0,
					"list": []map[string]any{
						{"fs_id": 333, "isdir": "0", "path": "/sub/b.txt", "server_filename": "b.txt", "size": 7},
					},
				})
				return
			}
			t.Errorf("unexpected list dir=%q root=%q", q.Get("dir"), q.Get("root"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	nodes, err := d.ResolveShare(context.Background(), driver.ShareLink{
		URL: "https://pan.baidu.com/s/1sjaPJJvG00g3B_aaU-zw6w", Pwd: "ab12",
	}, cred())
	if err != nil {
		t.Fatalf("ResolveShare: %v", err)
	}
	if !sawRoot || !sawSub || !sawBDCLND {
		t.Fatalf("root=%v sub=%v bdclnd=%v", sawRoot, sawSub, sawBDCLND)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(nodes), nodes)
	}
	if nodes[0].Name != "a.bin" || nodes[1].Name != "sub/b.txt" {
		t.Fatalf("names: %q %q", nodes[0].Name, nodes[1].Name)
	}
	for _, n := range nodes {
		if n.Ext["share_id"] != "12345" || n.Ext["uk"] != "67890" || n.Ext["sekey"] != "%2Bsekey%3D" {
			t.Fatalf("missing context: %+v", n.Ext)
		}
	}
}

// 加密分享未给提取码:应提示需要提取码,而不是笼统的上游错误。
func TestResolveShareNeedsPwd(t *testing.T) {
	d := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errno": 2, "errmsg": "need pwd"})
	}))
	_, err := d.ResolveShare(context.Background(), driver.ShareLink{URL: "https://pan.baidu.com/s/1AbCdEfGhIjKlMnOpQrStUv"}, nil)
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.KindNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

// 转存链:暂存目录 → 转存 → locatedownload 选 encrypt=0 → 清理副本。
func TestGetDirectLinkTransferChain(t *testing.T) {
	var transferred, cleaned bool
	d := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/gettemplatevariable":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "result": map[string]string{"bdstoken": "tok"}})
		case "/api/create":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0})
		case "/share/transfer":
			_ = r.ParseForm()
			transferred = r.Form.Get("fsidlist") == `["111"]` && r.Form.Get("path") == "/panrouter_tmp"
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errno": 0,
				"extra": map[string]any{"list": []map[string]any{
					{"to_fs_id": "999", "to": "/panrouter_tmp/a.bin"},
				}},
			})
		case "/rest/2.0/pcs/file":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errno": 0,
				"urls": []map[string]any{
					{"url": "https://d2-ant.baidu.com/enc", "encrypt": 1},
					{"url": "https://appall01.baidupcs.com/plain", "encrypt": 0},
				},
			})
		case "/api/filemanager":
			_ = r.ParseForm()
			cleaned = strings.Contains(r.Form.Get("filelist"), "/panrouter_tmp/a.bin")
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	dl, err := d.GetDirectLink(context.Background(), cred(), driver.FileRef{
		FID: "111",
		Ext: map[string]string{"share_id": "12345", "uk": "67890", "sekey": "%2Bsekey%3D"},
	})
	if err != nil {
		t.Fatalf("GetDirectLink: %v", err)
	}
	if !transferred {
		t.Fatal("transfer body mismatch")
	}
	if dl.URL != "https://appall01.baidupcs.com/plain" {
		t.Fatalf("want encrypt=0 https link, got %q", dl.URL)
	}
	if dl.UA != NetdiskUA || !strings.Contains(dl.Cookie, "BDUSS=abc") {
		t.Fatalf("link constraints: ua=%q cookie=%q", dl.UA, dl.Cookie)
	}
	if !cleaned {
		t.Fatal("transferred copy must be cleaned up")
	}
}

// locatedownload 失败(常量被轮换)时必须退回 filemetas。
func TestGetDirectLinkFallsBackToFilemetas(t *testing.T) {
	var metas bool
	d := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/gettemplatevariable":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "result": map[string]string{"bdstoken": "tok"}})
		case "/api/create":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0})
		case "/share/transfer":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "extra": map[string]any{"list": []map[string]any{
				{"to_fs_id": "999", "to": "/panrouter_tmp/a.bin"},
			}}})
		case "/rest/2.0/pcs/file":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": -1, "errmsg": "psign invalid"})
		case "/api/filemetas":
			metas = true
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "info": []map[string]any{
				{"dlink": "https://d.pcs.baidu.com/rest/2.0/pcs/file?fid=999&sign=x"},
			}})
		case "/api/filemanager":
			_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	dl, err := d.GetDirectLink(context.Background(), cred(), driver.FileRef{
		FID: "111", Ext: map[string]string{"share_id": "1", "uk": "2"},
	})
	if err != nil {
		t.Fatalf("GetDirectLink: %v", err)
	}
	if !metas || !strings.Contains(dl.URL, "fid=999") {
		t.Fatalf("fallback not used: metas=%v url=%q", metas, dl.URL)
	}
}

// 日转存硬闸:达上限后返回风控错误,不允许继续硬闯。
func TestTransferQuotaGate(t *testing.T) {
	d := New(nil, "")
	for i := 0; i < DailyTransferLimit; i++ {
		if err := d.takeTransferQuota("BDUSS=x"); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	err := d.takeTransferQuota("BDUSS=x")
	var de *driver.Error
	if !errors.As(err, &de) || de.Kind != driver.KindRiskControl {
		t.Fatalf("want risk_control, got %v", err)
	}
	// 另一账号互不影响。
	if err := d.takeTransferQuota("BDUSS=y"); err != nil {
		t.Fatalf("other account: %v", err)
	}
}

func TestCheckCredential(t *testing.T) {
	valid := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errno": 0, "result": map[string]string{"username": "nick", "bdstoken": "tok"}})
	}))
	st, err := valid.CheckCredential(context.Background(), driver.Credential{Cookie: "BDUSS=abc"})
	if err != nil || !st.Valid || st.Nickname != "nick" {
		t.Fatalf("valid: %+v %v", st, err)
	}

	expired := newTestDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"errno": -6, "errmsg": "not login"})
	}))
	st, err = expired.CheckCredential(context.Background(), driver.Credential{Cookie: "BDUSS=old"})
	if err != nil || st.Valid {
		t.Fatalf("expired: %+v %v", st, err)
	}
}

func TestCookieHeader(t *testing.T) {
	if got := cookieHeader("BDUSS=a", "%2Bsk%3D"); got != "BDUSS=a; BDCLND=%2Bsk%3D" {
		t.Fatalf("append: %q", got)
	}
	if got := cookieHeader("BDUSS=a; BDCLND=x", "y"); got != "BDUSS=a; BDCLND=x" {
		t.Fatalf("no dup: %q", got)
	}
	if got := cookieHeader("", "sk"); got != "BDCLND=sk" {
		t.Fatalf("empty base: %q", got)
	}
	_ = url.Values{}
}
