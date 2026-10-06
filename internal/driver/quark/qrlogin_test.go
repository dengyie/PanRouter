package quark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// qrMock 承载扫码登录三个上游端点:token、轮询、ticket 换 Cookie。
type qrMock struct {
	ts         *httptest.Server
	tokenHits  int
	pollHits   int
	ticketHits int
	pollCode   int  // 轮询返回的业务码(测试逐阶段改写)
	setCookie  bool // ticket 阶段是否下发 Set-Cookie
}

func newQRMock(t *testing.T) *qrMock {
	m := &qrMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/ajax/getTokenForQrcodeLogin", func(w http.ResponseWriter, r *http.Request) {
		m.tokenHits++
		q := r.URL.Query()
		if q.Get("client_id") != qrClientID || q.Get("v") != "1.2" {
			http.Error(w, "bad token params", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 2000000, "message": "ok",
			"data": map[string]any{"members": map[string]any{"token": "tok-abc"}},
		})
	})
	mux.HandleFunc("/cas/ajax/getServiceTicketByQrcodeToken", func(w http.ResponseWriter, r *http.Request) {
		m.pollHits++
		q := r.URL.Query()
		if q.Get("token") != "tok-abc" || q.Get("client_id") != qrClientID {
			http.Error(w, "bad poll params", http.StatusBadRequest)
			return
		}
		body := map[string]any{"status": m.pollCode, "message": "cas message"}
		if m.pollCode == 2000000 {
			body["data"] = map[string]any{"members": map[string]any{"service_ticket": "st-1"}}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/account/info", func(w http.ResponseWriter, r *http.Request) {
		m.ticketHits++
		q := r.URL.Query()
		if q.Get("st") != "st-1" || q.Get("lw") != "scan" {
			http.Error(w, "bad ticket params", http.StatusBadRequest)
			return
		}
		if m.setCookie {
			w.Header().Add("Set-Cookie", "__pus=abc; domain=.quark.cn; path=/")
			w.Header().Add("Set-Cookie", "__puus=xyz; domain=.quark.cn; path=/")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 2000000})
	})
	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

func newQRDriver(m *qrMock) *Driver {
	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		panic(err)
	}
	d := New(cl, "https://drive-pc.quark.cn")
	d.QRUopDomain = m.ts.URL
	d.QRInfoURL = m.ts.URL + "/account/info"
	return d
}

// 扫码登录全链路:token 形态、pending→confirmed 状态机、ticket 换 Cookie 并提取 __puus。
func TestQRTokenAndPollChain(t *testing.T) {
	m := newQRMock(t)
	d := newQRDriver(m)
	ctx := context.Background()

	qr, err := d.QRToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if qr.Token != "tok-abc" {
		t.Fatalf("token: %q", qr.Token)
	}
	if !strings.HasPrefix(qr.URL, "https://su.quark.cn/4_eMHBJ?") || !strings.Contains(qr.URL, "token=tok-abc") {
		t.Fatalf("qr url: %s", qr.URL)
	}

	// 未扫码 → pending
	m.pollCode = 80005000
	cookie, state, err := d.QRPoll(ctx, qr.Token)
	if err != nil || state != "pending" || cookie != "" {
		t.Fatalf("pending: %q %q %v", cookie, state, err)
	}

	// 已确认 → 换 Cookie,提取 Set-Cookie
	m.pollCode = 2000000
	m.setCookie = true
	cookie, state, err = d.QRPoll(ctx, qr.Token)
	if err != nil || state != "confirmed" {
		t.Fatalf("confirmed: %q %v", state, err)
	}
	if !strings.Contains(cookie, "__pus=abc") || !strings.Contains(cookie, "__puus=xyz") {
		t.Fatalf("cookie must contain session items: %q", cookie)
	}
	if m.ticketHits != 1 {
		t.Fatalf("ticket endpoint hit %d times", m.ticketHits)
	}
}

// 二维码过期 → expired 状态(非错误);缺会话 Cookie 的响应必须报错而非返回空串。
func TestQRPollExpiredAndMissingCookie(t *testing.T) {
	m := newQRMock(t)
	d := newQRDriver(m)
	ctx := context.Background()

	m.pollCode = 80005003
	if _, state, err := d.QRPoll(ctx, "tok-abc"); err != nil || state != "expired" {
		t.Fatalf("expired: %q %v", state, err)
	}

	m.pollCode = 2000000
	m.setCookie = false
	if _, _, err := d.QRPoll(ctx, "tok-abc"); err == nil {
		t.Fatal("missing session cookie must fail")
	}
}
