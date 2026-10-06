package quark

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// P2-11 回归:mergeCookies 必须按 ";" 分隔并 trim,容忍混合空白/紧凑串/含 = 值;
// 同名项按 patch 覆盖 base 且保持 base 顺序。
func TestMergeCookiesTolerantSplit(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		patch string
		want  string
	}{
		{
			name:  "compact patch overrides base order",
			base:  "__puus=old; foo=bar",
			patch: "__puus=new",
			want:  "__puus=new; foo=bar",
		},
		{
			name:  "mixed whitespace",
			base:  "a=1;  b=2",
			patch: "b=3;c=4",
			want:  "a=1; b=3; c=4",
		},
		{
			name:  "value containing equals",
			base:  "a=1",
			patch: "tok=xx=yy;  b = 2 ",
			want:  "a=1; tok=xx=yy; b = 2",
		},
		{
			name:  "empty base takes patch",
			base:  "",
			patch: "k=v",
			want:  "k=v",
		},
		{
			name:  "malformed part dropped from base",
			base:  "broken; a=1",
			patch: "b=2",
			want:  "a=1; b=2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeCookies(tc.base, tc.patch); got != tc.want {
				t.Fatalf("mergeCookies(%q,%q)=%q want %q", tc.base, tc.patch, got, tc.want)
			}
		})
	}
}

// P2-11 回归:200 响应非 JSON(HTTP 层成功但结构畸形)必须分类为 interface_changed,
// 不得归为可重试 upstream(旧实现会烧熔断并掩盖接口改版告警)。
func TestMalformed200BodyIsInterfaceChanged(t *testing.T) {
	var seenKind driver.Kind
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>unexpected</html>`))
	}))
	defer srv.Close()

	cl, err := httpx.New(httpx.Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, srv.URL)
	d.QRInfoURL = srv.URL + "/account/info"
	_, err = d.CheckCredential(context.Background(), driver.Credential{Cookie: "c=1"})
	if err == nil {
		t.Fatal("malformed 200 body must fail")
	}
	var de *driver.Error
	if !asDriverErr(err, &de) {
		t.Fatalf("want driver.Error, got %T", err)
	}
	seenKind = de.Kind
	if seenKind != driver.KindInterfaceChanged {
		t.Fatalf("kind=%s want interface_changed", seenKind)
	}
}

// P2-11 回归:CheckCredential 必须传播 DoJSON 解码错误,不得吞掉后误报 Valid=true。
func TestCheckCredentialPropagatesDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"nickname":`))
	}))
	defer srv.Close()

	cl, err := httpx.New(httpx.Options{AllowPrivate: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, srv.URL)
	d.QRInfoURL = srv.URL + "/account/info"
	st, err := d.CheckCredential(context.Background(), driver.Credential{Cookie: "c=1"})
	if err == nil {
		t.Fatal("truncated JSON must propagate error")
	}
	if st.Valid {
		t.Fatal("must not report valid on decode failure")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("root cause lost: %v", err)
	}
}

func asDriverErr(err error, target **driver.Error) bool {
	de, ok := err.(*driver.Error)
	if !ok {
		return false
	}
	*target = de
	return true
}
