package quark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/driver"
	"github.com/dengyie/panrouter/internal/pkg/httpx"
)

// 回归:pan.quark.cn/account/info 业务码为字符串 "OK"(非 drive-pc 接口的 int 0),
// apiResp.Code 声明为 int 曾导致反序列化失败被误判为"接口改版",有效凭据被记为无效。
// CheckCredential 对该端点改用宽松解码,字符串/整型业务码均接受。
func TestCheckCredentialStringCodeOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/info" {
			http.NotFound(w, r)
			return
		}
		// 真实形态:code 为字符串,含 nickname
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "code": "OK",
			"data": map[string]any{"nickname": "夸父0154"},
		})
	}))
	t.Cleanup(ts.Close)

	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, "https://drive-pc.quark.cn")
	d.QRInfoURL = ts.URL + "/account/info"

	st, err := d.CheckCredential(context.Background(), driver.Credential{Cookie: "__pus=a; __puus=b"})
	if err != nil {
		t.Fatalf("字符串业务码必须被接受: %v", err)
	}
	if !st.Valid || st.Nickname != "夸父0154" {
		t.Fatalf("status: %+v", st)
	}
}

// 整型 0(code==0)业务成功路径保持兼容。
func TestCheckCredentialIntCodeOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"nickname": "int-ok"},
		})
	}))
	t.Cleanup(ts.Close)

	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, "https://drive-pc.quark.cn")
	d.QRInfoURL = ts.URL + "/account/info"

	st, err := d.CheckCredential(context.Background(), driver.Credential{Cookie: "c=1"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Valid || st.Nickname != "int-ok" {
		t.Fatalf("status: %+v", st)
	}
}

// 回归(review 2026-10-07):200 响应缺 code 字段(null/空)说明形态未识别,
// 必须归类 interface_changed,不得静默把有效凭据记为 expired 且诊断留空。
func TestCheckCredentialMissingCodeIsInterfaceChanged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 合法 JSON 但无 code 字段
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"nickname": "ghost"},
		})
	}))
	t.Cleanup(ts.Close)

	cl, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	d := New(cl, "https://drive-pc.quark.cn")
	d.QRInfoURL = ts.URL + "/account/info"

	st, err := d.CheckCredential(context.Background(), driver.Credential{Cookie: "c=1"})
	if err == nil {
		t.Fatal("缺业务码必须报错,不得返回 Valid=false 的静默过期")
	}
	if st.Valid {
		t.Fatal("不得报告 valid")
	}
	de, ok := err.(*driver.Error)
	if !ok || de.Kind != driver.KindInterfaceChanged {
		t.Fatalf("kind 应为 interface_changed, got %v", err)
	}
}
