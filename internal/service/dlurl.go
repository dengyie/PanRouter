package service

import (
	"fmt"
	"net/url"
	"strings"
)

// 下载入口 URL(/d、/stream)的 fid 段编码。
//
// ★ fid 不是 URL 安全的:mangodrive 的 path 形态 fid 就是存储路径("/a/b/c.txt",
//   含 "/"),而 chi 的 {fid} 参数只匹配单个路径段(tree.go 对含 "/" 的段直接拒绝)。
//   修复前(fid 原样拼进路径)这类文件的下载入口**必然 404** —— 生产实证:
//   mangodrive 的 welcome.txt 提链 200、其 download_url 请求 404。
//
// 编码约定(签发端与验签端必须使用同一规范形):
//   - 签名 payload 恒为 `pan|key|fid` 的 **原始 fid**(编码前),HMAC 对身份签名,
//     与传输编码无关;
//   - URL 路径段用 url.PathEscape(fid) 承载("/" → "%2F");
//   - 验签端先用 FIDFromPathSeg 解回原始 fid,再验签。
//
// 兼容性:fid 不含保留字符时 PathEscape 是恒等变换(夸克/蓝奏云的十六进制 fid
// 前后一致),只有 mangodrive 的路径 fid 形态受影响 —— 而它修复前本就 404,
// 不存在依赖旧形态的可用客户端。

// FIDPathSeg 把 fid 编码为可安全放入单个 URL 路径段的形式。
func FIDPathSeg(fid string) string { return url.PathEscape(fid) }

// FIDFromPathSeg 是 FIDPathSeg 的逆变换:把路径段解回原始 fid。
// 非法转义(如 "%zz")返回错误,调用方必须报 400 而不是当普通字符串用。
func FIDFromPathSeg(seg string) (string, error) {
	return url.PathUnescape(seg)
}

// EntryURL 组装下载入口 URL:kind ∈ {"d","stream"}。
// base 允许带尾斜杠(内部统一剥掉);空 base 产出相对路径(测试/同机场景)。
func EntryURL(base, kind, pan, key, fid, sig string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s?sig=%s",
		strings.TrimSuffix(base, "/"), kind, pan, key, FIDPathSeg(fid), sig)
}
