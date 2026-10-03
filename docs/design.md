# PanRouter — 网盘直链聚合与加速下载服务 · 方案设计 v1.1

> 自用优先的单机服务:聚合主流网盘的分享解析/提链,缓存直链,按直链特性自动在 **302 透传 / aria2 直下 / 服务端中转** 三条路径中选择最优下载方式。
> 定位:个人自部署、单管理员、可接受登录态(Cookie / Token)。**不破解限速、不绕过会员权限、仅解析用户主动提供的分享链接。**
> License:Apache-2.0(自研实现,不链接 GPL/AGPL 代码)。

---

## 1. 目标与范围

### 1.1 目标
- 一套自用 Web 服务:粘贴网盘分享链接 → 一键提取直链 → 浏览器/aria2/Motrix 高速下载
- 支持自己网盘内文件的直链获取(登录态),以及他人分享链接的解析(匿名或登录态)
- 后端高效简洁、分层清晰;网盘接口频繁改版时可快速适配(driver 插件化)
- 单机高可用:自动重试、熔断限频、凭据失效告警、崩溃自恢复

### 1.2 部署画像(决定下载路径的默认值,先于一切)

| 画像 | 场景 | 直链绑 IP 的影响 | 默认下载路径 |
|---|---|---|---|
| `home` | 服务跑在家庭 NAS/PC,客户端同 LAN | 无(客户端 IP = 解析 IP) | 302 优先,relay 兜底 |
| `cloud` | 服务跑在云主机,客户端在家庭宽带 | **直链对客户端 403** | BindIP 直链自动走 relay;aria2 与服务同机时仍可直下 |

配置项 `deploy.profile: home | cloud`,**所有下载路径决策都以此为先验**(详见 §4.3)。

### 1.3 网盘分期范围
| 阶段 | 网盘 | 说明 |
|---|---|---|
| M1 | 夸克、蓝奏云 | 夸克代表"需 Cookie 登录态"一类;蓝奏云代表"免登录解析"一类,两种模式都先打通 |
| M2 | 123云盘、UC、移动云盘(139)、天翼云盘(189) | 均为 Cookie/Token 登录态类,与夸克同构 |
| M3 | 阿里云盘、迅雷云盘 | 需开放平台或 Token(M3 前各做一次可行性 spike) |
| M4 | 百度网盘(开放平台 / 秒传绕道)、115(仅中转) | 风控最严;**M4 前置 spike:验证百度开放平台个人开发者审核与下载配额**,秒传路径为保底方案 |

Backlog(暂不排期):分享转存到自己网盘后再提链(绕分享下载限制,参考 quark-auto-save 场景)、秒传链接生成、driver 脚本化热更新(见 §4.2)。

### 1.4 明确不做
- 多租户 / 公共站点化(自用,不需要账号体系复杂化)
- 破解客户端限速、绕过 DRM/版权保护
- 商业化 SaaS

---

## 2. 总体架构

```
                    ┌────────────────────────────────────────────┐
 浏览器 SPA ────────┤  Caddy (HTTPS / 静态托管 web/dist, 反代 API) │
 aria2 / Motrix ────┤        │                                    │
 脚本 (curl) ───────┤        ▼                                    │
                    │  PanRouter 后端 (Go, 单二进制)                 │
                    │  ┌──────────┐                               │
                    │  │ api 层    │ /api/v1  /d/*  /stream/*     │
                    │  ├──────────┤                               │
                    │  │ service 层│ 解析编排·路径决策·账号·下载     │
                    │  ├──────────┤                               │
                    │  │ driver 层 │ quark│lanzou│pan123│...     │
                    │  ├──────────┤                               │
                    │  │ infra     │ SQLite│httpx│代理池│熔断限频   │
                    │  └──────────┘                               │
                    └────────────────────────────────────────────┘
                              │(可选 sidecar)
                              ▼
                    playwright 容器(扫码登录, 仅 M2 引入)
```

- **前后端分离**:前端 Vue3 SPA(独立目录 `web/`),后端纯 REST;开发期 Vite 代理,部署期 `go:embed` 嵌入二进制(单文件)或 Caddy 托管(真分离),两种都支持。同源部署时无需 CORS;分离部署时 Caddy 同域反代,避免 CORS 配置。
- **部署形态**:Docker Compose(`panrouter` + `caddy` + 可选 `aria2` + 可选 `playwright`),`restart: unless-stopped`;同时提供 systemd 单机部署。aria2 必须开启 RPC secret,且与 panrouter 的通信仅走内网。

---

## 3. 技术选型

| 层 | 选型 | 理由 |
|---|---|---|
| 后端语言 | **Go 1.22+** | 单二进制、并发模型适合流式中转;alist 同栈,driver 生态可参考(只参考行为,不复用 GPL 代码) |
| HTTP 框架 | **chi**(或 Gin) | chi 更轻、中间件模型干净;两者皆可,默认 chi |
| ORM/DB | **GORM + SQLite(WAL)** | 自用单机,零外部依赖;模型简单,后期可平移 MySQL |
| 缓存 | 进程内(hashicorp/golang-lru)+ links 表持久化 | 直链缓存带 TTL,无需引入 Redis |
| 日志 | zap(结构化)+ request ID | 便于排查风控问题 |
| 配置 | viper + 轮询热加载 | 限频/部署画像/域名路由热生效;代理与重定向白名单变更需重启 |
| 前端 | **Vue 3 + Vite + TypeScript + Pinia + Naive UI** | 国内生态成熟,管理后台组件齐全 |
| 反代/HTTPS | Caddy | 自动证书,配置最少 |
| 下载器 | aria2(外部进程,RPC 对接) | 成熟多线程, Motrix/Gopeed 兼容其协议 |

> 备选:TS/Node 全栈(CF Workers 部署,参考 JxPan)——胜在零运维,输在流式中转和长连接性能;Java/Vert.x(参考 netdisk-fast-download)——JS 解析器热更新最成熟,但自用单机偏重。**推荐 Go 路线**:接受"新增/修复 driver 需重新编译发版"的代价(自用场景发版成本低),v2 可选把 driver 的解析逻辑(正则/字段提取)外置为脚本(yaegi)获得热更新,核心请求逻辑保持编译型。

---

## 4. 后端分层设计

依赖严格单向向下:`api → service → driver/repo → pkg`。跨层只传结构体值,不传 GORM 模型。

### 4.1 目录结构
```
panrouter/
├─ cmd/panrouter/main.go          # 配置加载、安全守卫、看门狗、热加载、优雅停机
├─ internal/app/app.go          # ★ 装配层:main 与端到端测试共用,保证单实例装配
├─ internal/
│  ├─ api/                      # 传输层:handler、middleware,不含业务逻辑
│  │  ├─ middleware/            # auth、recovery、requestid、ratelimit、log
│  │  ├─ resolve.go  links.go  accounts.go  download.go  settings.go
│  ├─ service/                  # 业务编排层
│  │  ├─ resolve.go             # 解析编排(识别→缓存→driver→落库),含直链缓存 TTL
│  │  ├─ routing.go             # ★ 下载路径决策(Can302 派生与降级)
│  │  ├─ account.go             # 凭据生命周期:互斥刷新/看门狗/选号/并发闸
│  │  ├─ download.go            # aria2 推送(URL 选择)、任务状态
│  │  └─ relay.go               # 中转流(Range 透传、续传)
│  ├─ driver/                   # ★ 防腐层:所有网盘差异隔离在此
│  │  ├─ driver.go              # 接口 + 注册表 + 错误类型 + 域名路由
│  │  ├─ quark/  lanzou/  pan123/  uc/  pan139/  pan189/ ...
│  ├─ repo/                     # 数据访问:GORM 模型 + 查询
│  └─ pkg/
│     ├─ httpx/                 # HTTP 客户端池:UA/TLS指纹/代理拨号/重试
│     ├─ breaker/               # 熔断器
│     ├─ limiter/               # 每网盘 token bucket
│     ├─ crypto/                # 凭据 AES-GCM 加解密
│     └─ errs/                  # 业务错误码(HTTP 层)
├─ internal/driver/..._test.go  # 合约测试 + fixture(见 §9)
├─ web/                         # Vue3 前端
├─ deploy/                      # docker-compose.yml、Caddyfile、systemd unit
├─ config.example.yaml
└─ go.mod
```

### 4.2 Driver 契约(项目成败的核心)

```go
// driver/driver.go
type Driver interface {
    ID() string
    // 分享链接 → 文件树(含 fid、大小),免登录盘 cred 可为 nil
    ResolveShare(ctx context.Context, share ShareLink, cred *Credential) ([]FileNode, error)
    // 网盘内文件 → 直链(携带 UA 透传上下文)
    GetDirectLink(ctx context.Context, cred Credential, ref FileRef) (DirectLink, error)
    // 凭据健康检查(供看门狗定时调用)
    CheckCredential(ctx context.Context, cred Credential) (CredStatus, error)
}

type DirectLink struct {
    URL       string
    UA        string            // 直链要求的 UA;空 = 不校验(浏览器 302 的 UA 等值匹配见 §4.3)
    Referer   string            // 直链要求的 Referer;非空则 302 不可用(浏览器只会带 PanRouter 页面的 Referer)
    Cookie    string            // 直链校验 Cookie 时非空(此时 Can302 恒为 false)
    BindIP    bool              // 直链绑定解析出口 IP(百度 dlink 等典型)
    ExpiresAt time.Time         // 直链有效期,驱动缓存 TTL
}

// 解析请求上下文:客户端 UA 透传(前端自动带 navigator.userAgent),决定"要求 UA 的直链"能否 302
type ResolveCtx struct {
    ClientUA string
}
```

**错误分类(契约的一部分,不允许 driver 私自定义):**

```go
type Error struct {
    Kind      Kind   // 见下表
    Retriable bool   // service 是否应重试
    UserHint  string // 面向用户的可操作提示
}

type Kind string
const (
    KindAuthExpired      Kind = "auth_expired"       // 凭据失效
    KindRiskControl      Kind = "risk_control"       // 风控/限流/验证码
    KindShareGone        Kind = "share_gone"         // 分享失效/被取消
    KindNotFound         Kind = "not_found"          // 文件不存在
    KindInterfaceChanged Kind = "interface_changed"  // 返回结构解析失败(接口改版)
    KindUpstream         Kind = "upstream_error"     // 上游 5xx/网络错误
    KindUnsupported      Kind = "unsupported"        // 功能暂不支持(如带密码蓝奏云、文件夹分享)
)
```

| Kind | 重试 | 换代理重试 | 触发熔断 | 触发凭据刷新 | 用户提示 |
|---|---|---|---|---|---|
| AuthExpired | 刷新后重试 1 次 | 否 | 否 | ✅(互斥) | "请到账号页更新 Cookie" |
| RiskControl | 否 | ✅ 1 次 | ✅ | 否 | "该网盘暂时限流,请稍后" |
| ShareGone | 否 | 否 | 否 | 否 | "分享已失效" |
| NotFound | 否 | 否 | 否 | 否 | "文件不存在" |
| InterfaceChanged | 否 | 否 | ❌(禁!) | 否 | "接口改版,等待适配"(高优告警) |
| Unsupported | 否 | 否 | 否 | 否 | "该功能暂不支持" |
| Upstream | ✅ 指数退避 ×3 | 否 | 连续 N 次 | 否 | "上游异常,请稍后" |

> 识别规则属于各 driver 实现(风控的响应形态各盘不同),但**分类必须落到上表 Kind**,并在合约测试中用样本固化。`InterfaceChanged` 与 `RiskControl` 混淆是禁手——前者换代理重试会加剧封号且毫无意义。

**其他契约要点:**
- `Detect(shareURL)` 域名路由表(`pan.quark.cn → quark`)**外置到 config**,热加载——网盘换域名(如 aliyundrive→alipan)不改代码
- driver 注册表启动时装配,config 可禁用;新增网盘 = 新增一个 driver 包,不动 service/api
- 直链缓存同时存 `BindIP/BindUA`,见 §5

### 4.3 下载路径决策(核心规则,service 层 `routing.go`)

派生规则(从 `DirectLink` + `deploy.profile` 计算;`fid` 不具备全局唯一性,**下载路径一律带 `share_key` 段**):

```
Can302   = Cookie=="" && !BindIP && (UA=="" || UA==ClientUA) && Referer==""
CanAria2 = !BindIP || aria2 与 panrouter 同机(同 LAN / 本机)
NeedRelay = !Can302 && !CanAria2
```

> M1 实现注记:`Referer==""` 是保守规则——302 后浏览器只会携带 PanRouter 页面的 Referer,无法满足直链对自定义 Referer 的校验,故统一禁止 302 走中转/aria2。

| 直链特性 | 浏览器 302 | aria2 直下(带头) | 服务端中转 |
|---|---|---|---|
| 无任何校验 | ✅ | ✅ | ✅ |
| 校验 UA(resolve 透传了客户端 UA) | ✅(同 UA) | ✅(带 UA) | ✅ |
| 校验 Referer / Cookie | ❌ | ✅(带对应 header) | ✅ |
| 绑定解析 IP | ❌(cloud 画像) | ✅ 同机 aria2 / ❌ 否则 | ✅ |

**选择优先级(成本从低到高):`302 → aria2 直下 → 中转`。**

- `/d/{pan}/{fid}`:先算 Can302;为 true → 302 直链;为 false → **302 到 `/stream/{fid}` 签名 URL**(用户无感,仍是"一个链接");两者都不可用时返回 JSON 错误 + aria2 命令提示
- `POST /downloads`(aria2 推送):`CanAria2` → 直链 + `{User-Agent, Referer, Cookie}` headers;否则推送 `/stream` URL
- 前端 resolve 结果展示"可 302 / 需 aria2 / 走中转"徽标,由服务端派生,前端不做决策

### 4.4 关键流程

**① 解析提链**
```
POST /api/v1/resolve {url, pwd, fid?, ua?}
→ api: 参数校验(ua 缺省时记 "unknown",此时 BindUA 直链视为不可 302)
→ service.Resolve:
   1. driver.Detect(url) 识别网盘(config 路由表)
   2. 查 links 缓存(shareKey+fid),未过期 → 直接返回(cacheHit=true)
   3. limiter 取令牌 → account picker 选号(可多账号)→ per-account 信号量
      → driver.ResolveShare/GetDirectLink(携带 ClientUA)
   4. 失败:按 §4.2 错误决策表分流(刷新凭据走互斥刷新,见 §7.6)
   5. 成功:写 links(TTL=ExpiresAt,含 bind_ip/bind_ua),返回
      {directLink, ua, referer, expiresAt, route: "302|aria2|stream", streamURL}
```

**② 302 下载**
```
GET /d/{pan}/{fid}
→ routing: Can302?
   ✅ → 缓存未过期:302 Location(directLink)
        已过期:非绑定直链 → SWR(先用旧链重试+后台刷新);绑定直链 → 同步刷新(旧链对客户端无意义)
   ❌ → 302 到 /stream/{fid}(签名 URL,用户无感降级)
```

**③ 中转流(NeedRelay 或显式选择)**
```
GET /stream/{fid}?sig=...
→ 验签(仅建连时校验,Range 续传复用同一 URL)→ 取直链
→ httpx 构造上游请求(带 UA/Referer/Cookie),上游重定向按 driver 白名单逐跳校验
→ 透传 Range/Content-Length/Accept-Ranges,io.Copy 双向流
→ 断链自动重连续传(记录 offset),上游限速时分段并发(可配,默认关)
```

**④ 加速下载(aria2)**
```
POST /api/v1/downloads {fid, dest, options?}
→ download service: 按 §4.3 选择 URL(直链+headers 或 stream URL)
→ aria2.addUri(url, headers) → 返回 gid;前端轮询 /downloads/{gid}
```

---

## 5. 数据模型(SQLite,自用精简)

| 表 | 关键字段 | 说明 |
|---|---|---|
| `accounts` | id, pan_type, name, cred_enc(BLOB, AES-GCM), cred_version, status(ok/cooling/expired/banned), cooldown_until, last_check_at | 登录态账号;多账号支持,由 picker 调度(§7.7);`cred_version` 供 in-flight 请求识别凭据代际 |
| `shares` | id, pan_type, share_url, pwd, share_key(唯一), raw_tree(JSON), resolved_at | 分享解析结果快照 |
| `links` | id, share_key, fid, file_name, size, direct_link, ua, referer, cookie_enc, **bind_ip, bind_ua**, expires_at, created_at | 直链缓存主表;bind 字段驱动 §4.3 路径决策 |
| `downloads` | id, gid, fid, dest, route, status, error, created_at | 下载任务记录(route 记录实际选择的路径) |
| `settings` | key, value | 站点开关、API Token、deploy.profile 等 |
| `audit_logs` | id, ts, action, detail | 凭据变更、登录、设置修改等敏感操作 |

> 单管理员用户不建表,凭据来自 config(用户名+bcrypt 哈希+可选 TOTP);脚本调用用 API Token(settings 表管理)。

---

## 6. API 设计(v1,REST,统一前缀 /api/v1)

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/resolve` | `{url, pwd, fid?, ua?}` → 直链 JSON(含派生 route 与 streamURL) |
| POST | `/resolve/batch` | `{items: [{url, pwd, fid?}]}` → 批量解析(文件夹提链;串行经 limiter,返回逐项状态) |
| GET | `/d/{pan}/{fid}` | 302 直链;Can302=false 时自动 302 到 /stream(带签名) |
| GET | `/stream/{fid}?sig=` | 中转流(签名 URL,支持 Range) |
| GET | `/json/{pan}/{fid}` | 302 的 JSON 版(供脚本) |
| GET | `/shares/{key}/files` | 浏览分享文件树 |
| GET | `/files?pan=&path=` | 浏览自己网盘目录(登录态) |
| GET/POST/DELETE | `/accounts` | 账号列表 / 导入 Cookie 或 Token / 删除 |
| POST | `/accounts/{id}/refresh` | 手动触发凭据校验刷新(走互斥刷新) |
| POST/GET | `/downloads`、`/downloads/{gid}` | 推送 aria2(URL+headers 由服务端选择)、查询进度;`/downloads/batch` 批量 |
| GET | `/healthz` `/readyz` `/metrics` | 健康检查、Prometheus 指标 |

- 认证:Web 端 JWT(Cookie,HttpOnly);脚本端 `Authorization: Bearer <api-token>`
- 错误格式:`{code, kind, message, retriable, hint}`——`kind` 即 §4.2 的 Kind,`hint` 为可操作提示
- `/d`、`/stream` 的签名:HMAC(fid + cred_version, master_key) + 过期时间,**默认 TTL 72h**(≥ 最长下载时长);签名只在建立连接时校验,Range 续传复用同一 URL,不会中途 401

---

## 7. 高可用与可靠性(自用语义)

自用单机的"高可用"不是集群,而是:**崩溃自恢复、外因(风控/改版)可自愈、故障可观测**。

1. **进程级**:Docker `restart: unless-stopped` / systemd `Restart=always`;优雅停机(等待中转流 flush);`healthz` 供监控
2. **熔断**:driver 级熔断器——连续 N 次(默认 5)`RiskControl/Upstream` 错误 → 熔断 X 分钟(默认 10),期间直接返回"该网盘暂时不可用",**避免高频重试加重风控导致封号**;`InterfaceChanged` 不触发熔断,改为高优告警
3. **限频**:每网盘独立 token bucket(默认夸克 2 QPS、百度 1 QPS、免登录盘 5 QPS),service 层统一取令牌
4. **重试**:仅 `Retriable` 错误重试;指数退避(1s/4s/16s,共 3 次);`RiskControl` 自动换代理重试一次(且仅一次)
5. **直链缓存刷新**:M1 实现为过期后**同步刷新**(自用延迟可接受;绑 IP/绑 UA 直链本就不允许复用旧链)。SWR(先旧链 + 后台刷新)留作 M2 优化,且仅对非绑定直链生效
6. **凭据刷新互斥(singleflight)**:看门狗定时校验与请求失败触发的刷新**合并为一次**(per-account 锁);刷新成功后原子替换凭据并递增 `cred_version`,in-flight 请求重试时取最新代际;部分盘的 refresh token 一次性,并发双刷会互踢——此锁为强制项,不是优化项
7. **账号编排(picker + 信号量)**:同盘多账号时,选号策略 = 粘性优先(上次成功的号)+ 失败进冷却(`cooldown_until` 指数增长)+ 其余 round-robin;**per-account 并发闸**:解析 1 并发、下载默认 3 并发(可配),防止并发拉爆单账号触发风控
8. **代理池**:按 driver 配置(http/socks5),轮询 + 失败计分淘汰;无代理时直连
9. **数据**:SQLite WAL 模式;可选 litestream 每小时备份到对象存储/本地目录;凭据 AES-GCM 加密落盘,主密钥来自环境变量 `PANROUTER_MASTER_KEY`
10. **观测**:zap 结构化日志 + request_id 贯穿;`/metrics` 暴露:每盘解析成功率/P95 延迟/熔断状态/缓存命中率/账号健康/各 route 占比(302 vs relay)
11. **配置热加载**:轮询 config.yaml(5s),限频/部署画像/域名路由即时生效;代理与重定向白名单在客户端构建时固化,变更需重启;Cookie 存于 DB,随请求即时生效。安全守卫:非 loopback 监听且密钥仍为默认值时拒绝启动

---

## 8. 安全设计

- 管理端:单用户 + bcrypt + 登录限速;可选 TOTP;会话 JWT HttpOnly
- `/d`、`/stream` 签名 URL(见 §6),防直链被外链滥用;签名不含明文凭据
- **SSRF 防护(与上游重定向兼容)**:中转上游仅允许 **per-driver 声明的白名单**——`upstream_allow`(直链域)+ `redirect_allow`(重定向目标域,覆盖各盘 CDN),**每一跳都校验**;同时全局禁止重定向到私网地址(10/8、127/8、169.254/16、192.168/16 等)与非常规端口;config 只能加严白名单
- 日志脱敏:Cookie/Token 打码(`quark_ctoken=a***f`)
- Caddy 强制 HTTPS;可配置 `listen` 仅内网(127.0.0.1 / LAN)

---

## 9. 测试策略

driver 是变更频率最高的模块,**必须在不碰真实账号的前提下获得回归保护**:

1. **service 决策单测**:§4.2 错误决策表、§4.3 路径派生(Can302/CanAria2 组合全覆盖)、SWR 分支、picker 冷却逻辑——纯函数,表驱动
2. **driver 合约测试**:每个 driver 声明实现契约(接口 conformance + 错误分类正确性);用**录制回放 fixture**(真实响应样本:成功/风控文案/凭据失效/接口改版各留档)回放,网盘改版时先补样本再修代码,修完回归全绿
3. **端到端冒烟**:每日一次低频真实调用(固定小样本 + 专用测试账号 + 独立限频),手动触发为主;**CI 不打真实盘**
4. **中转流测试**:本地起 mock 上游(支持 Range/断链/重定向),覆盖续传与白名单拦截

---

## 10. 分期路线图

| 里程碑 | 内容 | 验收标准 |
|---|---|---|
| **M1 骨架打通** | 分层骨架 + driver 契约(含错误分类)+ 注册表 + routing.go + 夸克/蓝奏云 driver + resolve/302/降级/缓存 + 前端最小页 | 粘贴夸克分享链接提链后,浏览器或 aria2 至少一条路径可下载(302 或自动降级 stream);蓝奏云 302 直下;决策单测全绿 |
| **M2 账号与多盘** | 账号管理页(手动导入 Cookie + playwright 扫码 sidecar)+ 123/UC/139/189 driver + 刷新互斥/picker/信号量 + 熔断/限频/代理池 + aria2 推送 + 下载任务页 | 5 个盘可提链;并发刷新不互踢(有测试);凭据失效有告警;aria2 满速跑通 |
| **M3 加速与生态** | 中转流(Range 续传 + 白名单)+ 阿里/迅雷 driver(M3 前 spike)+ WebDAV | 115/夸克大文件经中转稳定下载;WebDAV 可挂载播放 |
| **M4 硬骨头与打磨** | 百度 spike(开放平台个人配额验证)→ 实施(秒传为保底)+ 115(仅中转)+ 指标面板 + 打包文档 | 全盘位可用;release 附一键部署包 |

---

## 11. 风险与合规

| 风险 | 应对 |
|---|---|
| 网盘接口改版导致 driver 失效 | driver 插件化 + `InterfaceChanged` 高优告警 + 合约测试快速回归;保持对 alist/nfd 动态的关注 |
| 自用账号被风控/封禁 | 低频调用、限频、per-account 并发闸、必要时代理;**只读操作优先**;重要资源多盘冗余,不把单一网盘当唯一存储 |
| 法律合规 | 仅解析用户主动提供的分享链接;不破解限速/DRM;README 附免责声明(参考 nfd 条款);自用不公开分发解析能力 |
| 开源协议传染 | 全部独立实现(抓包分析/观察行为),**不复制** BaiduPCS-Go(GPL)、kuake_cli(AGPL)等项目的代码;自身 Apache-2.0 |
| 直链请求头/绑定类下载失败 | §4.3 路径决策显式化,302 失败自动降级,不产生"静默 403" |

---

## 12. 待确认问题(不阻塞 M1)

1. **部署画像**:当前默认 `home` 还是 `cloud`?这决定 BindIP 直链的默认处理(§1.2)——请确认一次,写入 config 默认值
2. **扫码登录顺序**:M1 手动导入 Cookie、M2 引入 playwright 扫码——维持此顺序?
3. **WebDAV**:放 M3,主要服务 Emby/挂载场景;无该需求可降级为可选
4. **中转多线程分段**:默认单流透传(简洁),带宽不足再开(复杂度↑)——维持默认关

---

## 变更记录

- **v1.1.4(2026-10-03)**:线上联调修复——夸克分享直链端点修正(drive-pc 旧端点 404 → drive.quark.cn + `fid` 单数 + 异步任务轮询,实测取证);夸克分享直链确认需登录态(无 Cookie 时按契约返回 AuthExpired);蓝奏云**密码分享支持**(密码页 isngis+fileid → ajaxfile.php,真实链接+提取码全链路验证);新增 lanzouu 域名路由与 lanrar/dmpdmp CDN 白名单;蓝奏云 CDN acw_sc__v2 反爬挑战对非浏览器客户端的影响已定位并列为 M2 专项
## 变更记录(历史)

- **v1.1.3(2026-10-03)**:基于端到端测试的深度优化——新增 `internal/app` 装配层(main 与 e2e 共用,消除装配漂移类缺陷);`Registry.UpdateRoutes` 使域名路由真正热生效(带锁热替换 + 测试);过期直链并发刷新经 singleflight 收敛为一次真实调用;实现 §7.7 在途并发闸(`download_concurrency`,排队等待 + 15s 上限,不再 fail-fast);路由计数语义修正为"实际服务决策点"(/d 与 aria2 推送);app 支持 `ExtraDrivers` 注入式 driver(测试 fake 与未来插件共用通道)
- **v1.1.2(2026-10-03)**:生产化评审修复——中转流与 API 调用拆分客户端(去除 30s 体传输截断,P1);账号 cooling 冷却到期自动复位(补全状态机);配置解析失败不再静默回落默认密钥;管理页文件名 XSS 转义;扩展点收敛(`drivers` 为 map + 工厂注册,新增网盘只改 driver 包与 config);`FID` 列名显式映射(修复 GORM 默认命名 `f_id` 与查询字面量不一致导致的落库必炸 bug);新增 repo/sign/config/httpx/relay 共 11 个回归测试;非 loopback + 默认密钥拒绝启动;删除未接线的全局 Proxy 配置与 own 模式死参数
- **v1.1.1(2026-10-03)**:M1 实现同步——下载路径带 `share_key` 段(`/d/{pan}/{share_key}/{fid}`,fid 非全局唯一);`BindUA` 字段并入 UA 等值匹配规则;错误契约新增 `KindUnsupported`;M1 落地说明:JWT 为自实现 HS256、`/metrics` 为极简 Prometheus 文本实现(M2 替换 client_golang)
- **v1.1(2026-10-03)**:按深度评审修订——新增 §4.3 下载路径决策(302/aria2/中转三条路径 + Can302 派生规则,修复 302 无法携带自定义头与 IP 绑定问题);§4.2 增加错误分类契约与决策表(InterfaceChanged 禁熔断);§7.6 凭据刷新互斥(singleflight)与 §7.7 账号编排(picker+信号量);§8 SSRF 改为 per-driver 重定向白名单 + 私网黑名单;§6 明确签名 TTL 语义(72h,建连校验);新增 §9 测试策略(合约测试 + 录制回放);新增 §1.2 部署画像;Detect 路由表外置 config;批量端点;License 定为 Apache-2.0;移除 M1 工时承诺,M4 百度前置 spike
- v1(2026-10-03):初版
