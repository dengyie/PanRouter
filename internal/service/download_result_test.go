package service

import (
	"context"
	"testing"
	"time"

	"github.com/dengyie/panrouter/internal/config"
	"github.com/dengyie/panrouter/internal/driver"
)

// §15.2 item 3 回归:need_headers 派生规则全站统一(cookie 非空 || Referer 非空 || UA 不匹配),
// 由共享纯函数 NeedHeaders 派生;/api/v1/json 出口曾漏 UA 不匹配项。
func TestNeedHeadersSharedRule(t *testing.T) {
	if NeedHeaders("", "", "", "any") || NeedHeaders("same", "", "", "same") || NeedHeaders("same", "", "", "SAME") {
		t.Fatal("no constraints must not need headers (EqualFold 匹配)")
	}
	if !NeedHeaders("", "", "c=1", "") {
		t.Fatal("cookie must force headers")
	}
	if !NeedHeaders("", "https://r", "", "") {
		t.Fatal("referer must force headers")
	}
	if !NeedHeaders("link-ua", "", "", "other-ua") {
		t.Fatal("ua mismatch must force headers")
	}
}

// §15.2 item 3 回归:buildResult 的 need_headers 走同一纯函数;
// 注入 UA 约束的直链与客户端 UA 不匹配时必须置位。
func TestBuildResultNeedHeadersUAMismatch(t *testing.T) {
	d := &twoFileDriver{}
	resolver, _ := newTestResolver(t, d, config.DriverCommon{Enabled: true, LimitQPS: 1000, DownloadConc: 3})
	d.fn = func(_ context.Context, fid string) (driver.DirectLink, error) {
		return driver.DirectLink{URL: "https://up.invalid/" + fid, UA: "link-required-ua", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	res, err := resolver.ResolveFile(context.Background(), testShareURL, "", "f1", false, "different-client")
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedHeaders {
		t.Fatalf("UA mismatch must set need_headers, got %+v", res)
	}
}
