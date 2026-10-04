# Decisions

## 2026-10-04 - 解析一次给出直链
- Decision: `ResolveShare` 递归展开夸克目录后，对文件自动走 `ResolveFile`（上限 8）；前端有 `download_url` 时直接给出下载，不再强制二次「提链」。无夸克 Cookie 只列文件并提示加账号。
- Rationale: 线上 `43d94cf65dc3` 根节点是文件夹，页面不给出提链；二次点击对自用场景过繁琐。
- Alternatives considered: 仅前端自动点提链（目录仍展不开）；无上限全量转存（风控/超时）。
- Impact: 分享解析响应 FileItem 可带 route/download_url；夸克 driver 按 `pdir_fid` 走子目录。
- Rollback trigger: 自动提链明显触发夸克风控或请求超时。
- Related files: `internal/driver/quark/driver.go`, `internal/service/resolver.go`, `web/dist/index.html`
