# PanRouter — 网盘直链聚合与加速下载服务

自用优先:粘贴网盘分享链接 → 提取直链 → 按**直链约束自动择路**(302 透传 / aria2 直下 / 服务端中转)下载。
当前进度:**M1**(骨架 + 夸克/蓝奏云 driver + 决策引擎 + 前端最小页)。完整设计文档见 [docs/design.md](docs/design.md)。

## 快速开始(本地)

```bash
cp config.example.yaml config.yaml   # 改 api_token / jwt_secret / password_bcrypt
go build -o panrouter ./cmd/panrouter
./panrouter                            # 默认监听 127.0.0.1:6400
```

浏览器打开 `http://127.0.0.1:6400`,用 `admin / admin123` 登录(**务必先改密码**),粘贴分享链接解析。

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

# 解析分享 → 文件列表
curl -s localhost:6400/api/v1/resolve -H "Authorization: Bearer $T" \
  -d '{"url":"https://pan.quark.cn/s/xxxx"}'

# 解析单个文件 → 直链 + 派生下载路径(route: 302|aria2|stream)
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
internal/api/              chi 路由、JWT/Token 认证、错误→HTTP 映射
internal/repo/             GORM + SQLite(WAL)
internal/pkg/              httpx(SSRF 防护)、breaker、limiter、crypto(AES-GCM)、sign(HMAC)、metrics
```

关键设计(对应设计文档章节):
- **下载路径决策** §4.3:`service/routing.go` 纯函数,表驱动单测覆盖
- **错误分类契约** §4.2:`driver.Error{Kind,...}`,InterfaceChanged 禁熔断
- **凭据刷新互斥** §7.6:`AccountService.Check` singleflight
- **SSRF 防护** §8:全局禁私网直连 + per-driver 重定向白名单逐跳校验

## 开发

```bash
go build ./... && go vet ./... && go test ./...
```

- driver 合约测试用 httptest 回放响应样本,不碰真实网盘
- 改接口/改结构时:先补 fixture 样本,再改代码,回归全绿

## M1 已知限制(均为有意取舍)

- 夸克 driver 接口基于公开开源实现的调用方式,**尚未对真实网盘联调**,风控形态可能与预期不符
- 蓝奏云:暂不支持带密码链接与文件夹分享;最终直链对浏览器友好,若实测 403 需在 driver 中补 UA/Referer(改一处即自动切换下载路径)
- 前端为最小可用页(原生 JS);Vue3 + Naive UI 正式前端在 M2
- 多账号轮换(picker)完整逻辑、扫码登录(playwright sidecar)、WebDAV 在 M2/M3
- `/metrics` 为极简 Prometheus 文本实现,M2 替换 client_golang

## 免责声明

本项目仅供个人学习与技术交流:仅解析用户主动提供的分享链接,不破解限速、不绕过会员权限、不绕过版权保护;高频调用可能触发网盘风控导致账号受限,请自担风险并遵守各网盘服务条款。License:Apache-2.0。
