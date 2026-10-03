// Package web 内嵌前端静态资源(dist/ 随二进制分发;分离部署时也可由 Caddy 直接托管 dist)。
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
