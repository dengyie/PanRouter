// Package service:下载路径决策(设计文档 §4.3)。
// 从直链约束 + 部署画像派生 Can302 / CanAria2 / Route,是纯函数,表驱动单测覆盖。
package service

import "strings"

// LinkMeta 是路径决策所需的直链约束子集。
type LinkMeta struct {
	UA      string
	Referer string
	Cookie  string
	BindIP  bool
}

type RouteEnv struct {
	Profile       string // home | cloud
	ClientUA      string // 解析时透传的客户端 UA(空 = 脚本调用未知 UA)
	Aria2SameHost bool   // aria2 是否与 PanRouter 同机/同内网
}

// Can302:浏览器 302 能否直接下载。
// 浏览器无法为重定向请求附加自定义 UA/Referer/Cookie,因此:
//   - 带 Cookie 校验的直链 302 必 403 → false
//   - 绑定解析 IP 的直链,cloud 画像下客户端 IP ≠ 解析 IP → false(home 画像同 LAN 无影响,统一保守处理)
//   - 直链要求自定义 Referer,而 302 后浏览器携带的是 PanRouter 页面 Referer → false
//   - 直链要求特定 UA,仅当与客户端 UA 一致时可行
func Can302(l LinkMeta, env RouteEnv) bool {
	if l.Cookie != "" || l.BindIP || l.Referer != "" {
		return false
	}
	if l.UA != "" && !strings.EqualFold(l.UA, env.ClientUA) {
		return false
	}
	return true
}

// CanAria2:aria2 直下能否工作。aria2 可携带任意请求头,唯一硬约束是 IP 绑定。
func CanAria2(l LinkMeta, env RouteEnv) bool {
	return !l.BindIP || env.Aria2SameHost
}

// Route 返回成本最低的可用路径:"302" → "aria2" → "stream"。
func Route(l LinkMeta, env RouteEnv) string {
	switch {
	case Can302(l, env):
		return "302"
	case CanAria2(l, env):
		return "aria2"
	default:
		return "stream"
	}
}
