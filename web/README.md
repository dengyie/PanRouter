# web/

前端静态资源。生产由 Go `//go:embed all:dist` 打进单二进制，`internal/api` 用 `http.ServeContent` + SPA fallback 托管。

当前 `dist/index.html` 是 M1 最小页。正式 UI 在此目录用任意栈重构后，覆盖 `dist/` 即可，不必改后端。

## 契约（不要打破）

- 相对路径：`fetch('/api/v1...')`、`fetch('/healthz')`
- 登录：`POST /api/v1/auth/login` → `{token, expires_in}`，存 `localStorage.panrouter_token`，请求头 `Authorization: Bearer …`
- 解析：`POST /api/v1/resolve`（游客可解析免登录盘；夸克 Cookie 仅登录后可用）
- 下载：用响应里的绝对 `download_url` / `stream_url` 做页面跳转（`/d` `/stream` 在 API 源站，HMAC 签名），不要 XHR 跟 CDN
- 错误体：`{code, kind, message, retriable}`；`401` 且已有 token 时清会话
- **不加 CORS**。开发走代理，生产同源或 Caddy 同域反代

## 替换 dist

任意前端栈 `build` 到 `web/dist/`：

```
web/dist/index.html
web/dist/assets/...
```

后端会：

- 命中文件原样返回（MIME 跟扩展名）
- 未命中且路径无扩展名，或 `Accept` 显式接受 `text/html`（q > 0）的 GET/HEAD 请求，回退 `index.html`（`Cache-Control: no-cache`）；带点号的 SPA 路由也支持浏览器直接访问/刷新
- 普通缺失 JS/CSS/图片请求返回 JSON 404，不回退 HTML
- `/api` `/d` `/stream` `/healthz` `/readyz` `/metrics` 精确路径及其子路径永不回退；名称相似的 `/healthz-dashboard` 不受影响

然后照旧 `go build ./cmd/panrouter`。CI 只编 Go，把提交进仓的 `dist/` 嵌进 ELF。

## 开发期代理

把前端 dev server 的下列前缀转到后端 `127.0.0.1:6400`：

`/api` `/d` `/stream` `/healthz` `/readyz` `/metrics`

Vite 示例：

```ts
server: {
  proxy: {
    '/api': 'http://127.0.0.1:6400',
    '/d': 'http://127.0.0.1:6400',
    '/stream': 'http://127.0.0.1:6400',
    '/healthz': 'http://127.0.0.1:6400',
    '/readyz': 'http://127.0.0.1:6400',
    '/metrics': 'http://127.0.0.1:6400',
  },
}
```
