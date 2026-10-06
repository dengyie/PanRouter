# PanRouter 开发文档 v2.0(2026-10-05)

> 本文以当前实现为基准重写,取代原《方案设计》(docs/design.md v1.1.x,已删除)。
> 编写原则:**已实现行为、已知问题、接口契约、重构计划分开表述**——旧方案中未实现的规划统一移入 §17 路线图,不再与现状混排;代码注释不再引用本文档章节号。
> 定位:自用优先的单管理员网盘分享解析、直链聚合与下载服务。**不破解限速、不绕过会员权限、仅解析用户主动提供的分享链接。**

---

## 1. 范围与明确不做

做:
- 粘贴网盘分享链接 → 解析/提链 → 缓存直链 → 按 **302 透传 / aria2 直下 / 服务端中转** 自动择路下载
- 免登录解析(蓝奏云一类)与登录态提链(夸克一类);游客可解析,账号管理与 aria2 需登录
- driver 插件化:网盘接口改版时只改 driver 包

不做(原则不变):
- 多租户 / 公共站点化 / 商业 SaaS
- 破解客户端限速、绕过 DRM/版权保护
- 微服务、消息队列等重基建;保持 **Go 单二进制 + SQLite**

---

## 2. 技术栈与总体架构

| 层 | 选型 | 说明 |
|---|---|---|
| 语言/框架 | Go 1.26 + chi v5 | 单二进制;chi 路由 + NotFound 静态分发 |
| 存储 | GORM + glebarez/sqlite(WAL) | 零 CGO;busy_timeout 5s |
| 配置 | viper(YAML) | 5s 轮询热加载(边界见 §4.2) |
| 日志 | zap + request_id | 结构化;敏感操作落 audit_logs |
| 并发治理 | singleflight + token bucket + breaker | 刷新互斥 / 限频 / 熔断 |
| 加密 | AES-256-GCM(凭据)、HMAC-SHA256(下载签名)、bcrypt(口令)、HS256(会话 JWT) | 全部标准库实现 |
| 前端 | 原生单页(占位)→ 任意 SPA 构建产物 | 生产 go:embed;**不加 CORS** |

```
浏览器 / aria2 / curl
   │ HTTPS(CF Tunnel / Caddy)
   ▼
PanRouter 单二进制(Go)
 ├─ api      chi 路由、JWT/Token 认证、错误→HTTP 映射、web/dist 静态托管
 ├─ service  解析编排、路径决策(纯函数)、账号、中转、aria2
 ├─ driver   quark / lanzou(防腐层,网盘差异隔离在此)
 ├─ repo     GORM 模型 + 查询(SQLite WAL)
 └─ pkg      httpx(SSRF/acw)、breaker、limiter、crypto、sign、metrics
```

依赖严格单向向下:`api → service → driver/repo → pkg`,跨层传结构体值。§15.2 item 1/2 收口后:api 与 service 零直接 GORM,handlers 只做参数与响应;剩余偏差仅 repo.Account 等模型以值跨 service→api 边界(handler 转 DTO 输出)。

---

## 3. 目录结构(实际)

```
panrouter/
├─ cmd/panrouter/main.go        # 配置加载、安全守卫、主密钥、看门狗、热加载、优雅停机
├─ internal/app/app.go          # ★ 装配层:main 与 e2e 测试共用;ApplyConfig 热应用
├─ internal/api/
│  ├─ router.go                 # 路由表、NotFound 静态分发、错误→HTTP 映射
│  ├─ middleware.go             # requestid、zap、recoverer、optionalAuth/auth
│  ├─ auth.go                   # 自实现 HS256 JWT、登录
│  ├─ handlers.go               # resolve(/batch)、accounts、downloads 处理器
│  ├─ links.go                  # /d、/stream、/json 签名校验与 302
│  ├─ web.go / web_test.go      # web/dist 静态托管 + SPA fallback
│  └─ errors_test.go / e2e_test.go
├─ internal/service/
│  ├─ resolver.go               # 解析编排:识别→缓存→driver→落库;singleflight 续命
│  ├─ routing.go                # ★ 下载路径决策(纯函数,表驱动单测)
│  ├─ account.go                # 凭据生命周期:singleflight 校验、状态流转
│  ├─ download.go               # aria2 推送与任务状态
│  └─ relay.go                  # 中转流:白名单重定向、Range 透传、死链失效
├─ internal/driver/
│  ├─ driver.go                 # 接口 + 错误类型 + 注册表(域名路由热更新)
│  ├─ quark/                    # 转存链、stoken、任务轮询、Cookie 合并
│  └─ lanzou/                   # iframe/modern/密码流、CDN 二次验证
├─ internal/repo/repo.go        # GORM 模型 + 查询 + Open/Close
├─ internal/pkg/
│  ├─ httpx/                    # 客户端池:SSRF 防护、acw_sc__v2 求解重放
│  └─ breaker/ limiter/ crypto/ sign/ metrics/ log/
├─ internal/config/config.go    # 默认值 + YAML 加载 + Provider
├─ web/                         # 前端:dist(生产 embed)+ README
├─ deploy/pxed-install.sh       # 生产原子安装
├─ .github/workflows/ci.yml     # vet/test + 交叉编译 + 部署
└─ config.example.yaml
```

---

## 4. 运行时行为

### 4.1 启动流程(cmd/panrouter/main.go)

1. `config.Load`:`-config` 显式路径,否则 `./config.yaml` → `./config/config.yaml`;显式路径不存在或解析失败必须报错(不允许带默认密钥静默启动);未显式指定且无文件 → 纯默认值(仅本地开发)
2. 安全守卫:监听非 loopback 且 api_token/jwt_secret 仍为默认值 → 拒绝启动
3. 主密钥:优先环境变量 `PANROUTER_MASTER_KEY`;空则回落 jwt_secret(打警告)
4. embed `web.Dist` → `fs.Sub("dist")`
5. `app.Build`:建 data_dir → SQLite(WAL + busy_timeout=5000 + AutoMigrate) → limiter / breaker Registry(连续 5 次,冷却 10min) / signer / metrics → 每个 driver 两个 httpx 客户端(API 30s 总超时;中转不限体时长) → factory 注册(quark/lanzou) → registry(域名路由) → AccountService / Resolver / Relay / Aria2
6. `http.Server`(ReadHeaderTimeout 10s)启动
7. 看门狗:每 30min 校验全部账号(经 singleflight 互斥)
8. 配置热加载:仅显式配置路径时启用;5s 轮询,listen 变更忽略,其余经 `ApplyConfig`
9. SIGINT/SIGTERM → `srv.Shutdown(15s)` → `App.Close`(当前仅关闭 DB)

### 4.2 配置(当前实际 + 已知边界)

- 默认值在 `internal/config/config.go`(含默认凭据常量,仅供本地开发)
- 热加载实际行为与文档承诺**有差**(§15 P1-1/P1-2):`ApplyConfig` 直接替换整个 Config 指针——限频、部署画像、域名路由、鉴权密钥即时生效;但 signer 与 AES 固定在启动时构造,jwt_secret 轮换后三者不同代;遗漏 auth 段的热加载会被默认值补回并绕过启动守卫
- 代理与 `redirect_allow` 在客户端构建时固化,变更需重启


### 4.3 停机顺序(§15.2 item 4 已落地)

后台任务(凭据看门狗 30min、配置热加载 5s)由 `app.App.StartBackground` 编排(main 只传配置路径);
`stop`/`App.Shutdown` 先取消并 **等待后台协程退出**,再关闭 Store——杜绝"协程用库时库已关"的竞态。
`srv.Shutdown` 超时会记 Warn(不再静默忽略);`Shutdown` 未启动后台时为空操作(e2e 纯装配路径)。
回归:`TestBackgroundWatchdogRunsAndStopWaits`、`TestShutdownWithoutBackground`。

---

## 5. 鉴权与会话契约

| 机制 | 实现 |
|---|---|
| 会话 | 自实现 HS256 JWT `{sub, exp}`,TTL 7 天,secret = 配置 jwt_secret(热加载跟随);前端存 `localStorage.panrouter_token`,请求头 `Authorization: Bearer` |
| 脚本 | `Authorization: Bearer <api_token>`(常量时间比较) |
| 口令 | config `password_bcrypt`(bcrypt 哈希)或明文兼容(每次登录打警告);登录成功/失败写 audit_logs |

- `optionalAuth`(/resolve、/resolve/batch):无 Bearer = 游客;坏 Bearer 返回 `session_expired`/401;有效 = 上下文允许 Pick 账号 Cookie
- `auth`(其余 /api/v1):必须登录;缺失或失效 Bearer 返回 `session_expired`/401
- `/d`、`/stream`:HMAC 签名,无需网页登录态
- 游客规则(v1.1.12):游客解析不读取账号 Cookie;不得命中带 Cookie 的直链缓存;限频/熔断按 `pan:guest` 分键
- 已签发下载链接过期后的续命刷新可以 Pick(由库内是否已有 Link 决定,不跟请求的登录态走)

站点身份错误与上游凭据错误均使用 HTTP 401，但通过 `kind` 区分：`session_expired` 表示 PanRouter Bearer 缺失或失效，`auth_invalid` 表示登录口令错误，`auth_expired` 表示网盘上游 Cookie/凭据失效。前端仅对 `session_expired` 清理 `panrouter_token`，上游 Cookie 失效不会退出管理员会话。

---

## 6. 解析链路

### 6.1 分享解析 ResolveShare

`POST /api/v1/resolve {url, pwd, fid?, ua?}`(游客可调用)

1. 整次请求 90s 预算;`Detect(url)` 按域名路由识别网盘
2. 游客不 Pick 账号;guard(熔断→限频→调用→熔断记录)执行 `ResolveShare`
3. 结果快照落 `shares`(raw_tree JSON)
4. 自动提链:非目录文件按序 `ResolveFile`;上限夸克 2、其余 8;剩余时间不足 15s 停;AuthExpired 停后续;超限 `truncated=true` + hint;单文件失败写 `files[].link_error`,响应仍 200

### 6.2 单文件提链 ResolveFile

1. 查 `links` 缓存(share_key + fid):DB 故障显式报错(不得当 cache miss);游客命中带 Cookie 缓存 → 401;未过期 → 直接返回(cache_hit=true)
2. Pick 账号(游客 nil)→ `buildRef`(ext 优先取缓存 link,其次分享快照;DB 故障向上传播)→ 在途并发闸(download_concurrency,超额排队)→ guard 执行 `GetDirectLink`
3. 成功:直链 Cookie AES-GCM 加密落 `links`;落库失败必须报错,不返回看似成功的下载入口(P2-6 已修)
4. `buildResult`:路径决策 + 签名 URL + `need_headers`(明文 Cookie 非空 / 有 Referer / UA 与客户端不一致)

### 6.3 缓存与 singleflight

- `shareKey = hex(sha1(pan|url|pwd))[:16]`;fid 不具备全局唯一性,下载路径一律带 share_key 段
- repo 读取契约:`GetLink`/`GetShare` 不存在返回 `(nil, nil)`(cache miss 语义);DB 故障返回 error,service 层必须向上传播
- `GetFreshLink`:并发刷新经 singleflight(`link:shareKey|fid`)收敛为一次真实调用;共享任务使用独立有界 ctx(120s,不绑定首个调用者,首调取消不拖垮等待者,P2-12 已修);等待者自身 ctx 取消时立即失败;库内已有 Link 时续命上下文可 Pick 账号;share 快照不存在则明确报错;DB 故障显式传播(P2-6 已修)

### 5.1 夸克扫码登录(免手动贴 Cookie)

- 端点:`GET /api/v1/accounts/quark/qr/token`(返回 `{token, url, png}`,`png` 为 `data:image/png;base64` data URI——后端 skip2/go-qrcode 直出,前端 `<img src>` 零依赖渲染,生成失败时省略该字段回退展示链接);`POST /api/v1/accounts/quark/qr/poll {token, name?}` 轮询状态
- 状态机:`pending`(未扫/确认中)→ `confirmed`(返回 `account`,已自动以换好的 Cookie 建号入库)或 `expired`(二维码失效,重新取 token)
- 上游流程(官方 CAS,client_id=532):`uop.quark.cn/cas/ajax/getTokenForQrcodeLogin` 取 token → 二维码 `su.quark.cn/4_eMHBJ?token=…&ssb=weblogin` → 轮询 `getServiceTicketByQrcodeToken` 拿 `service_ticket` → `pan.quark.cn/account/info?st=<ticket>&lw=scan` 的 Set-Cookie 即登录态(含 __puus 强校验项),`joinCookies` 提取后经 `AccountService.Create` 加密入库
- 契约:token 一次性且服务端无会话存储(前端持 token 轮询,O(1) 状态);CAS 业务码 80005000/1/2=等待、80005003/4=过期、2000000=成功;Cookie 检测必须含 __pus/__puus 否则报错;两个端点均为登录态保护(POST/GET 均在 auth 组)
- 实现:`internal/driver/quark/qrlogin.go`(端点域可注入 `QRUopDomain/QRInfoURL` 供 httptest);回归 `TestQRTokenAndPollChain`、`TestQRPollExpiredAndMissingCookie`、`TestE2EQuarkQRLogin`

### 6.4 批量解析契约(POST /api/v1/resolve/batch)

- 预算:每项 90s(`api.ResolvePerItemTimeout`),单请求最多 50 项(`ResolveBatchMaxItems`);超限返回 400 + `kind=unsupported`(非 404)
- 部分成功:逐项独立执行,单项失败不中断批;响应 200,`results[i].ok=false` 时 `error` 字段携带该项错误文本

---

## 7. 下载链路

### 7.1 路径决策(service/routing.go,纯函数,表驱动单测)

```
Can302   = Cookie=="" && !BindIP && Referer=="" && (UA=="" || 等值匹配客户端 UA)
CanAria2 = !BindIP || aria2 与服务同机(aria2.same_host)
Route    = 302 > aria2 > stream(成本从低到高)
```

依据:浏览器 302 无法附加自定义 UA/Referer/Cookie;cloud 画像下绑 IP 直链对客户端 403。

### 7.2 签名(internal/pkg/sign)

- 密钥:`jwt_secret + "|panrouter-sign"`(启动时固定)
- payload:`pan|shareKey|fid`;token = `b64url(exp) + "." + b64url(HMAC_SHA256(payload|exp))`
- 校验:exp 过期拒绝;`hmac.Equal` 常量时间比对;仅建连时校验,Range 续传复用同一 URL 不中断

**TTL 契约(已统一,P2-5 已修)**:所有下载入口签名(`/d` 签发、`/d` 降级 stream、`/api/v1/json`、aria2 stream URL、resolve 返回的 download_url/stream_url)统一使用配置 `server.sign_ttl`(`Server.EffectiveSignTTL()`,非正配置回落 72h);上游直链剩余有效期独立由 `expires_at` 表示,签名有效期内可通过 `GetFreshLink` 续命。

### 7.3 GET /d/{pan}/{key}/{fid}?sig=

验签 → `GetFreshLink` → `Can302`? 302 直链 : 302 到 `/stream/...`(sign_ttl 新签名,用户无感降级);计数 `download_route_total`。

### 7.4 GET /stream/{pan}/{key}/{fid}?sig=

Relay.Serve:验签 → GetFreshLink → 解密 Cookie → 构造上游请求(UA/Referer/Cookie/Range)→ httpx DoStream(逐跳白名单重定向 + acw 自动重放)→

- 上游 ≥400:读 4KB 丢弃;401/403/404/410/412 时 `ExpireLink` 失效本地缓存(死链自愈)后报错
- 成功:透传 Content-Type/Length/Range/Accept-Ranges/ETag/Last-Modified,32KB 流式写 + flush;客户端断开视为正常取消

### 7.5 aria2 推送

`POST /api/v1/downloads {pan, share_key, fid, dest?}`(登录):pan 与 share 快照一致性校验 → GetFreshLink → `CanAria2`? 直链 + headers(UA/Referer/Cookie) : stream URL(sign_ttl) → `aria2.addUri`(RPC secret)→ 落 `downloads` + 计数。任务状态 `GET /downloads/{gid}`。

---

## 8. 错误契约

`driver.Error{Kind, Retriable, UserHint}`,Kind 全集与 HTTP 映射:

| Kind | 语义 | HTTP |
|---|---|---|
| auth_expired | 网盘上游凭据失效 | 401 |
| session_expired | PanRouter 站点会话缺失或失效 | 401 |
| auth_invalid | PanRouter 登录口令错误 | 401 |
| risk_control | 风控/限流/验证码 | 429 |
| share_gone | 分享失效 | 404 |
| not_found | 文件不存在/参数缺失 | 404 |
| interface_changed | 返回结构解析失败(接口改版) | 502 |
| upstream_error | 上游 5xx/网络错误 | 502 |
| unsupported | 功能暂不支持 | 400 |

- API 错误体:`{code, kind, message, retriable}`(message = UserHint;无独立 hint 字段);认证相关 401 必须保留 `kind`，前端只将 `session_expired` 视为站点会话失效
- guard 行为:熔断开启 → 429(计 breaker_open);本地限频 → 429(不计熔断);`RiskControl/Upstream` 计入熔断连续失败(默认 5 次 → 冷却 10min);`InterfaceChanged` 不熔断,打 `[ALARM]` 错误日志
- 解码失败分类:200 响应非 JSON 经 `httpx.ErrBadPayload` 哨兵归类 `interface_changed`(打 `[ALARM]` 不熔断);凭据检查传播解码错误(P2-11 已修)

---

## 9. 数据模型(SQLite WAL)

| 表 | 关键字段 | 说明 |
|---|---|---|
| accounts | id, pan_type, name, cred_enc(AES-GCM BLOB), cred_version, status, cooldown_until, last_check_at | 多账号;状态机 ok →(risk_control)cooling →(冷却到期选中)ok;expired 由 AuthExpired 标记 |
| shares | pan_type, share_url, pwd, share_key, raw_tree(JSON), resolved_at | 分享快照 |
| links | share_key, fid, file_name, size, direct_link, ua, referer, cookie_enc, bind_ip, expires_at, ext(JSON) | 直链缓存主表;ext 存 driver 上下文(pwd_id/stoken 等) |
| downloads | gid, fid, dest, route, status | aria2 任务记录 |
| settings | key, value | 预留 |
| audit_logs | ts, action, detail | login_ok / login_failed / cred_check / account_create / account_delete |

单管理员不建用户表,凭据来自 config。
已知(P1-3):账号检查整对象 `Save` 存在删除复活/冷却覆盖竞争。

---

## 10. Driver 契约

### 10.1 接口与注册表

```go
type Driver interface {
    ID() string
    ResolveShare(ctx context.Context, share ShareLink, cred *Credential) ([]FileNode, error)
    GetDirectLink(ctx context.Context, cred *Credential, ref FileRef) (DirectLink, error)
    CheckCredential(ctx context.Context, cred *Credential) (CredStatus, error)
}
type DirectLink struct {
    URL, UA, Referer, Cookie string
    BindIP    bool
    ExpiresAt time.Time
}
```

- 注册启动时装配(config enabled 过滤);`Detect` 按域名路由表(host 精确/后缀匹配)热更新
- 直链约束(UA/Referer/Cookie/BindIP)是路径决策与 need_headers 的唯一依据;错误分类必须落到 §8 的 Kind,不允许 driver 私自定义

### 10.2 quark(转存链)

分享直链端点已下线(实测 404),现行流程为网页同款转存链:
`detail/stoken → save(fid_list 精确单文件;save_as_select_top_fids 会 41013)→ task 轮询(status==2,最多 30 次)→ file/download → CDN 直链`

- 转存进专用暂存目录 `panrouter_tmp`(按登录态指纹 sha1(cookie)[:16] 缓存目录 fid,find-or-create);夸克对同 hash 同名去重返回既有 fid,cleanup 只删目录内受控副本
- stoken 过期(快照里的有时效)自动重取重试一次;转存容量/次数受限诚实分类 risk_control
- 直链 Cookie 基底 = 完整登录态,file/download 的 Set-Cookie(如 `__puus`)同名覆盖(mergeCookies 保持基底顺序,按 ";" 分割+trim 容忍紧凑串/混合空白/含 = 值)
- 取链后 `defer cleanupSaved(WithoutCancel)` 清理副本;任务被接受但轮询中断时经 `cleanupAbandonedSave` 以独立有界 ctx(60s)重轮询拿 fid 并删除(P2-9 已修)

### 10.3 lanzou

iframe / modern / 密码分享三条流;最终 CDN 对非浏览器客户端先 acw_sc__v2 挑战(httpx 自动解题,§11),再可能落「验证并下载」页(`down_r` + POST `ajax.php`)换真实文件 URL。

### 10.4 限频 / 熔断 / 并发闸

- 限频:每网盘 token bucket,键 = `pan`(登录)/ `pan:guest`(游客),速率 = `drivers.*.limit_qps`(float64,支持小数;容量 ≥ 1、首扣令牌,P2-7 已修)
- 熔断:per-key breaker,连续 5 次 RiskControl/Upstream → 冷却 10min;无 half-open
- 并发闸:per-pan 在途 `download_concurrency`,超额排队等待

---

## 11. HTTP 客户端(internal/pkg/httpx)

- 每个 driver 两个客户端:API 调用 30s 总超时;中转流不限体传输时长
- SSRF:重定向逐跳校验 per-driver `redirect_allow`;私网地址默认禁止(`AllowPrivate` 仅测试注入)
- acw_sc__v2:检测 412/挑战页 → posList 重排 + hexXor 求解 → 重放(最多 1 次),按 host 缓存 Cookie;二进制/206 响应跳过窥探;重放沿用调用方 context;重放模板取最终生效请求(302/303 已 GET 化剥 Body、307/308 保 Body,P2-10 已修)
- DoJSON:JSON 编解码 + 响应体大小上限;解码失败错误携带 `ErrBadPayload` 哨兵,调用方可区分网络错误与接口改版

---

## 12. 前端与静态托管

### 12.1 托管行为(internal/api/web.go)

- chi `NotFound` 分发:WebFS != nil 且 GET/HEAD 且非保留路径 → handleWeb;否则 JSON 404
- 保留路径(精确 + 子路径,永不回退 HTML):`/api` `/d` `/stream` `/healthz` `/readyz` `/metrics`
- handleWeb:`path.Clean` → 命中文件用 `http.ServeContent`(MIME/Range/HEAD);未命中 → 无扩展名或显式接受 `text/html`(q>0)回退 `index.html`(`Cache-Control: no-cache`);否则 JSON 404
- 带 hash 的静态资源交给浏览器默认缓存
- 不加 CORS:开发期 dev server 代理 `/api` `/d` `/stream` `/healthz` 到 :6400(见 web/README.md);生产同源 embed 或 Caddy 同域反代

### 12.2 前端依赖的稳定契约(重构前端时的边界)

- 端点:`POST /api/v1/auth/login`、`POST /api/v1/resolve(/batch)`、`GET|POST|DELETE /api/v1/accounts*`、`POST|GET /api/v1/downloads*`、`GET /api/v1/json/{pan}/{key}/{fid}`
- 探活:`GET /healthz → {status, version}`、`GET /readyz`
- 错误体:`{code, kind, message, retriable}`;401 按 `kind` 区分站点会话、登录口令和上游凭据错误，前端只对 `session_expired` 清理 Token
- 下载:`download_url` 浏览器直接打开(302 或降级);`stream_url`/`ua`/`referer`/`need_headers` 供 aria2
- 现有页面交互见 `web/dist/index.html`(原生单文件页,可整目录替换)

---

## 13. 部署与运维

### 13.1 CI / 发版(.github/workflows/ci.yml)

- PR / push main:`go vet` + `go test -count=1`
- push main / workflow_dispatch:`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 交叉编译(ldflags 注入 sha12 版本;ELF 闸门)→ scp → `deploy/pxed-install.sh`
- 安装脚本:校验 ELF64 → `panrouter.new` + mv 原子替换 → 不覆盖 config/env → supervisor reread/update/restart → `curl --noproxy '*'` healthz 循环

### 13.2 生产拓扑

- 公网 https://pan.mangoqwq.com → CF Tunnel → 生产机 127.0.0.1:6400
- 生产 config.yaml / `PANROUTER_MASTER_KEY` 只在主机控制面,不入库(本仓 config.yaml 已 gitignore)
- 生产机无 Go 编译器,不能机上构建;机上探活必须 `curl --noproxy '*'`(全局代理会假 503)
- 详细运维手册:Obsidian「PanRouter 部署与运维」

### 13.3 配置项(config.example.yaml)

| 项 | 说明 |
|---|---|
| server.listen / base_url | 监听与对外地址(下载 URL 由 base_url 派生) |
| server.deploy_profile | home(与客户端同 LAN)/ cloud(绑 IP 直链自动走中转) |
| server.sign_ttl | /d /stream 签名默认 TTL(72h) |
| auth.* | 管理员口令(bcrypt)、api_token、jwt_secret |
| domain_routes | 分享域名 → 网盘 ID,热更新 |
| drivers.* | enabled / limit_qps / download_concurrency / redirect_allow / proxy |
| aria2.* | endpoint / secret / same_host |

---

## 14. 观测

- 指标(/metrics,Prometheus 文本):`panrouter_resolve_total{pan,kind}`、`panrouter_download_route_total{pan,route}`、`panrouter_link_cache_hit_total{pan}`、`panrouter_driver_error_total{pan,kind}`、`panrouter_breaker_open{pan,scope}`(scope=login|guest)、`panrouter_account_status{pan,status}`(状态全集补零)
- 日志:zap 结构化 + request_id;Cookie/Token 不入日志
- 审计:audit_logs(见 §9)

---

## 15. 已知问题与重构计划(2026-10-05 深度评审结论)

### 15.1 必须修复(先补能触发问题的回归测试,再改实现)

| 优先级 | 问题 | 最小修复 | 验收 |
|---|---|---|---|
| P1-1 | 配置热加载绕过启动守卫:遗漏 auth 段的热加载补回默认凭据并即时生效 | 配置分区"启动固定/可热更";候选配置校验通过才发布 | 遗漏 auth / 回退默认密钥 / 非法配置的热加载被拒绝且保留旧配置 |
| P1-2 | 密钥轮换与持久化凭据生命周期未对齐:AES/签名启动固定,JWT 热加载跟随;主密钥回落 jwt_secret 时轮换后旧 Cookie 不可解密 | 明确密钥更新规则;拒绝未迁移的加密密钥变更 | jwt_secret 轮换后签名/会话/解密行为有明确契约与测试 |
| P1-3 | 账号检查整对象 Save:检查期间删除账号会被写回复活;覆盖并发冷却更新 | 按 ID 更新指定字段 + RowsAffected;竞争状态用条件更新 | 检查期间删除/风控不复活、不覆盖 |
| P2-4 | 上游凭据 401 与站点会话 401 冲突,夸克 Cookie 失效会退出管理员 | 已通过 `auth_expired`、`session_expired`、`auth_invalid` 区分认证错误;前端仅按 `session_expired` 清 Token | 夸克凭据失败保留站点会话;坏 Bearer 清会话;错误回归测试覆盖 |
| P2-5 | ~~下载签名 TTL 混用~~ 已修:所有下载入口签名统一 `server.sign_ttl`,上游有效期独立表示 | — | `TestBuildResultUsesConfigSignTTL` 覆盖短 TTL 直链签名仍为 sign_ttl |
| P2-6 | ~~links 落库失败仍返回成功~~ 已修:落库失败即报错;`GetLink`/`GetShare`/快照查询 DB 故障显式传播,不存在与故障语义分离 | — | `TestResolveFilePropagatesCacheReadFailure` 覆盖 DB 故障不触发提链、不返回虚假下载入口 |
| P2-7 | ~~限流桶:首次放行不扣 token;低速率下容量被封顶无法积攒~~ 已修:容量与速率分离(容量 ≥ 1),首扣令牌,refill 先按旧速率结算再改速率 | — | `TestAllowConsumesInitialToken`、`TestFractionalRateRefillsToOneToken`(0.5 QPS 2s 补 1 token)、`TestSetRateSettlesBeforeChangingRate` |
| P2-8 | ~~metrics Handler 持全局锁写响应~~ 已修:`snapshot()` 锁内快照、Handler 锁外写 | — | `TestHandlerDoesNotHoldRegistryLockDuringWrite` |
| P2-9 | ~~quark 转存任务接受后轮询中断,副本清理缺失~~ 已修:轮询错误路径经 `cleanupAbandonedSave` 以独立有界 ctx(60s,WithoutCancel)重轮询拿 fid 并删除 | — | `TestAbandonedSaveTaskCleanup` 覆盖首调取消后副本被删(delete 含 newfid) |
| P2-10 | ~~ACW 重放沿用重定向前方法/Body~~ 已修:重放模板取 `resp.Request`(最终生效请求),保留调用方 ctx | — | `TestACWReplayAfter302DropsStaleBody`(302 重放 GET 化)、`TestACWReplayAfter307KeepsBody`(307 保 Body) |
| P2-11 | ~~Cookie 合并仅按 "; " 分隔;解码失败归类 upstream;凭据检查忽略解码错误~~ 已修:按 ";" 分割+trim 容忍紧凑串/混合空白/含 = 值;DoJSON 解码失败带 `httpx.ErrBadPayload` 哨兵,quark 归类 interface_changed;凭据检查传播解码错误 | — | `TestMergeCookiesTolerantSplit`、`TestMalformed200BodyIsInterfaceChanged`、`TestCheckCredentialPropagatesDecodeError` |
| P2-12 | ~~singleflight 刷新任务使用第一个调用者的 ctx~~ 已修:共享任务用 `WithoutCancel`+120s 预算;等待者自身 ctx 取消时立即失败 | — | `TestGetFreshLinkFirstCallerCancelDoesNotKillWaiters` |

### 15.2 结构重构(在 P1 修复后进行)

1. ~~账号管理业务收口 AccountService(api/handlers 只做参数与响应)~~ 已完成:增删与手动校验收口为 `AccountService.Create/Delete/Refresh`(网盘校验、Cookie 非空、加密落库、审计、指标刷新全在 service;driver 注册表经函数注入,避免 service→resolver 反向依赖);handlers 只做参数解析与响应写出;`Check` 抽出 `checkAcc` 核心,手动校验复用 singleflight 链且账号只查一次;删号后该 pan 清空时 gauge 补零。回归:`TestAccountServiceCreate/Delete/Refresh`
2. ~~service 内直接 GORM 查询(分享快照)收口为 repo 方法~~ 已完成:service 快照/直链全走 repo 方法,readyz 走 `Store.Ping`,`Store.DB()` 已删除,生产路径零 gorm 暴露
3. ~~下载结果构造统一(签名 TTL、need_headers 一处派生;保留 routing.go 纯函数)~~ 已完成:签名 TTL 已统一走 `EffectiveSignTTL`(P2-5);need_headers 由共享纯函数 `service.NeedHeaders` 一处派生,修正 `/api/v1/json` 出口漏 UA 不匹配项的分歧;aria2 headers 的冗余 cookie 判空清理。回归:`TestNeedHeadersSharedRule`、`TestBuildResultNeedHeadersUAMismatch`
4. ~~运行生命周期归 App:后台任务接入、停机等待与超时处理、资源关闭顺序~~ 已完成:看门狗与热加载协程收编 `App.StartBackground`(测试可注入周期),`App.Shutdown` 按"停后台并等待退出→关 Store"排序,stop 幂等且 Shutdown 兜底;main 的 HTTP `Shutdown` 超时不再被忽略;回归 `TestBackgroundWatchdogRunsAndStopWaits`/`TestShutdownWithoutBackground`
5. ~~driver 文件按协议职责拆分(响应分类/分享遍历/转存任务/直链与 Cookie),保持接口不变~~ 已完成:quark 包拆为 `driver.go`(类型/构造/classify/call/stoken)、`share.go`(ResolveShare+遍历)、`transfer.go`(转存链+副本清理)、`directlink.go`(GetDirectLink/DirectLink 组装/Cookie 合并/CheckCredential),接口与行为零变化,现有测试零改动全绿
6. ~~metrics 补 account_status 接线与 guest 维度熔断标签~~ 已完成:account_status 由 AccountService 状态流转与账号增删后重算;breaker_open 带 scope 标签且 guard 入口即刷新

### 15.3 契约整理(前端重构前完成)

- ~~批量解析总预算与部分成功规则(当前每项 90s、最多 50 项)~~ 已完成:固化于 `api.ResolvePerItemTimeout`(90s)与 `ResolveBatchMaxItems`(50);超限从 404 修正为 400(`KindUnsupported`);批量契约写入 §6.4
- ~~错误 kind 与 HTTP 状态映射稳定化(配合 P2-4)~~ 已完成:九种 kind→状态映射固化在 `kindStatus` 并有 `TestKindStatusMapping` 契约测试锁定;writeErr 对非 driver.Error 一律按 upstream/502 兜底
- ~~cred_version 语义定义(当前健康检查也递增)~~ 已完成:语义=凭据代际,仅凭据内容变更(重新保存)时经 `UpdateAccount` 递增;`UpdateAccountCheck`(健康检查)不再 +1(`TestUpdateAccountCheckKeepsCredVersion`)

### 15.4 实施顺序

1. P1 安全/正确性(P1-1/2/3)+ 基础组件(P2-7/8)
2. 前端依赖契约稳定(P2-4/5/6 + §15.3)
3. 结构重构(§15.2)
4. ~~文档与死代码清理(`app.Options.Version`、`AccountService.sfMu`、`drivers.*.upstream_allow` 等)~~ 已完成:三项已删——`Options.Version`(healthz 用 `api.Deps.Version`)、`AccountService.sfMu`(singleflight 自身并发安全)、`drivers.*.upstream_allow`(未接线死配置,§3 描述同步移除);全仓 grep 清零

每步:`gofmt / go vet ./... / go test ./... -count=1` 全绿;先写能触发问题的失败测试再改实现。

---

## 16. 测试策略

- 原则:CI 不打真实网盘;driver 合约测试用 httptest 回放响应样本;网盘改版时先补样本再修代码,回归全绿
- 现有覆盖:service 决策表(表驱动)、singleflight 收敛与并发闸(峰值在途断言)、driver 合约(quark 转存链/stoken/Cookie、lanzou 三流/CDN)、httpx(SSRF/重定向/acw/超时)、web 托管(fstest.MapFS + 保留路径 + Accept 语义)、e2e(装配级)、repo/errors/config 单测
- 缺口:§15 每项标注的回归测试;limiter/metrics/breaker 目前仅经集成覆盖,无直接包测试

---

## 17. 路线图

已实现(截至 2026-10-05):
- M1 骨架:分层 + driver 契约 + 决策引擎 + 夸克/蓝奏全链 + 前端最小页
- 夸克转存链(专用暂存目录/stoken 重试/死链自愈)、蓝奏 acw + CDN 二次验证页
- 游客解析与隔离加固(坏 Bearer 401、缓存/限频/熔断分键、续命不跟游客态)
- 90s 预算自动提链、截断与失败提示
- 前后端托管分离:任意 SPA dist + ServeContent + SPA fallback

规划(未排期,按需启动):
- 正式前端(Vue3/任意栈,覆盖 web/dist 即可)
- 多盘 driver(123/UC/139/189 与夸克同构;阿里/迅雷前先做可行性 spike;百度前置开放平台配额 spike)
- SWR 缓存刷新(仅非绑定直链)、client_golang 替换自研 metrics、playwright 扫码 sidecar
- own 模式 /files(网盘内文件提链)、WebDAV、代理池、自动凭据续期
- metrics 面板、litestream 备份

不做:多租户/公共站点化、破解限速/DRM、商业 SaaS。

---

## 18. 风险与合规

| 风险 | 应对 |
|---|---|
| 网盘接口改版导致 driver 失效 | driver 插件化 + interface_changed ALARM + 合约测试快速回归 |
| 自用账号被风控/封禁 | 限频/熔断/并发闸/游客分键;只读操作优先;不把单一网盘当唯一存储 |
| 法律合规 | 仅解析用户主动提供的分享链接;不破解限速/DRM;自用不公开分发解析能力 |
| 开源协议传染 | 全部独立实现(抓包分析/观察行为),不复制 GPL/AGPL 项目代码;自身 Apache-2.0 |
| 凭据泄露 | AES-GCM 落盘 + 独立主密钥;日志/错误不打明文;审计留痕 |

---

## 变更记录

- **v2.0(2026-10-05)**:以实际实现为基准重写,取代《方案设计》v1.1.x(已删除);现状/已知问题/契约/路线图分开表述;新增 §15 深度评审结论与分阶段重构计划;代码注释不再引用章节号
