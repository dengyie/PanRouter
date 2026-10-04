# Project State

## Objective
- 登录后粘贴分享链接，一次解析直接给出可下载直链（含夸克文件夹展开）

## Current Phase
- M2 S3 一键提链：测试全绿，待推 main 部署

## Current Branch
- main

## Last Verified
- `go vet ./...` + `go test ./... -count=1` 通过（含夸克目录遍历合约与 e2e 自动提链）

## Active Risks
- 夸克转存链按文件串行，自动提链上限 8；过多仍可能慢或风控

## Active Blockers
- 夸克直链仍需账号 Cookie（Manual-required）

## Current Focus
- 提交 v1.1.9 并推 CI 部署

## Next Milestone
- 生产加夸克 Cookie 后用 `https://pan.quark.cn/s/43d94cf65dc3` 验收

## Key Artifacts
- `internal/driver/quark/driver.go`
- `internal/service/resolver.go`
- `web/dist/index.html`
- `docs/design.md`
