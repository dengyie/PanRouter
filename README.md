# PanRouter — 网盘直链聚合与加速下载服务

自用优先:粘贴网盘分享链接 → 提取直链 → 按**直链约束自动择路**(302 透传 / aria2 直下 / 服务端中转)下载。
当前进度:**M1**(骨架 + 夸克/蓝奏云 driver + 决策引擎 + 前端最小页)。完整开发文档见 [docs/development.md](docs/development.md)。

## 快速开始(本地)

```bash
cp config.example.yaml config.yaml   # 改 api_token / jwt_secret / password_bcrypt
go build -o panrouter ./cmd/panrouter
./panrouter                            # 默认监听 127.0.0.1:6400
```

浏览器打开 `http://127.0.0.1:6400`,粘贴蓝奏等免登录分享即可解析。夸克需要管理员登录并添加 Cookie(**务必先改密码**)。

## 快速开始(Docker)

```bash
cd deploy
cp ../config.example.yaml config.yaml   # 修改配置
export PANROUTER_MASTER_KEY=32位随机串 ARIA2_SECRET=随机串
docker compose up -d --build
```

## API 速览

```bash
# 登录换 token
curl -s localhost:6400/api/v1/auth/login -d '{"username":"admin","password":"admin123"}'

# 解析分享(游客可解析蓝奏;夸克需登录后添加 Cookie)
curl -s localhost:6400/api/v1/resolve \
  -d '{"url":"https://xxx.lanzouw.com/xxxx"}'

# 刷新单个文件直链(可选)
curl -s localhost:6400/api/v1/resolve -H "Authorization: Bearer $T" \
  -d '{"url":"https://pan.quark.cn/s/xxxx","fid":"文件fid"}'

# 下载:浏览器直接打开 download_url(302 或自动降级 stream)
# 脚本直链(带 UA/Referer 约束说明):GET /api/v1/json/{pan}/{share_key}/{fid}

# 推送 aria2(服务端自动带 headers 或走 stream)
curl -s localhost:6400/api/v1/downloads -H "Authorization: Bearer $T" \
  -d '{"pan":"quark","share_key":"...","fid":"..."}'
```
## 架构与代码导读

```
cmd/panrouter/main.go        配置加载、安全守卫、看门狗、热加载、优雅停机
internal/app/app.go        ★ 装配层:main 与端到端测试共用,保证单实例装配
internal/driver/           ★ 防腐层:接口 + 错误分类契约 + 注册表 + quark/lanzou
internal/service/          解析编排、路径决策(routing.go,纯函数)、账号(singleflight 互斥刷新)、中转、aria2
internal/api/              chi 路由、JWT/Token 认证、错误→HTTP 映射、web/dist 静态托管
web/                       前端(生产 embed `dist/`;可整目录替换,见 web/README.md)
internal/repo/             GORM + SQLite(WAL)
internal/pkg/              httpx(SSRF 防护)、breaker、limiter、crypto(AES-GCM)、sign(HMAC)、metrics
```

关键设计(详见 docs/development.md):
- **下载路径决策** §7.1:`service/routing.go` 纯函数,表驱动单测覆盖
- **错误分类契约** §8:`driver.Error{Kind,...}`,InterfaceChanged 禁熔断
- **凭据刷新互斥**:`AccountService.Check` singleflight(§4.1)
- **SSRF 防护** §11:私网直连禁止 + per-driver 重定向白名单逐跳校验

## CI / 生产发版

GitHub Actions(`.github/workflows/ci.yml`):

- PR / push `main`:`go vet ./...` + `go test ./... -count=1`
- push `main`(及 `workflow_dispatch`):交叉编译 `linux/amd64` 静态二进制(`CGO_ENABLED=0`,ELF 闸门)后 SSH 到生产机,`deploy/pxed-install.sh` 原子替换并 `supervisorctl restart`
- 生产 `config.yaml` / `PANROUTER_MASTER_KEY` 只在主机控制面,不入库(本仓 `config.yaml` 已 gitignore)

## 开发

```bash
go build ./... && go vet ./... && go test ./...
```

- driver 合约测试用 httptest 回放响应样本,不碰真实网盘
- 改接口/改结构时:先补 fixture 样本,再改代码,回归全绿

## M1 已知限制(均为有意取舍)

- 夸克:分享**列表解析已线上验证**(匿名可用,含提取码);**直链需要登录态**(已实现,无账号时返回 401 + 添加账号提示);直链端点走 drive.quark.cn + 任务轮询
- 蓝奏云:**带密码分享已线上验证**(真实链接+提取码解析→提链→302 全链路打通,含 lanzouu 新域名与 lanrar/dmpdmp CDN);文件夹分享暂不支持
- 蓝奏云最终 CDN 对非浏览器客户端先过 acw_sc__v2,再过「验证并下载」页(`ajax.php`);`GetDirectLink` 已跟到真实文件地址,浏览器 302 与 `/stream` 都走这条链
- 前端现为 `web/dist` 最小页(原生 JS);后端已静态资源托管(`http.ServeContent`) + SPA fallback,正式 UI 覆盖 `dist/` 即可,不必改 Go
- 多账号轮换(picker)、扫码登录(夸克 CAS,`/api/v1/accounts/quark/qr/token`+`/qr/poll`)已落地;WebDAV 在 M3
- `/metrics` 为极简 Prometheus 文本实现,M2 替换 client_golang

## 免责声明

本项目仅供个人学习与技术交流:仅解析用户主动提供的分享链接,不破解限速、不绕过会员权限、不绕过版权保护;高频调用可能触发网盘风控导致账号受限,请自担风险并遵守各网盘服务条款。License:Apache-2.0。
