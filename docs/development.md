# PanRouter 开发文档 v2.3(2026-11-10)

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
5. `app.Build`:建 data_dir → SQLite(WAL + busy_timeout=5000 + AutoMigrate) → limiter / breaker Registry(连续 5 次,冷却 10min) / signer / metrics → 每个 driver 三个 httpx 客户端(API 30s 总超时;stream 不限体时长;relay 再独立一份,避免轮换时关掉 Transfer 的空闲连接) → factory 注册(quark/lanzou) → registry(域名路由) → AccountService / Resolver / Relay / Aria2
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
3. 结果快照落 `shares`(raw_tree JSON);**写侧三防**(v2.10):nil 节点序列规范为 `"[]"`(不落字面量 `"null"`);`UpsertShare` 对无有效内容的 `raw_tree`(`""`/`"null"`/`"[]"`) **不更新该列**、保留原值(首次落库按原样写入);口径统一由 `repo.RawTreeHasContent` 判定(写侧与读侧复用,防两处漂移)
4. 自动提链:非目录文件按序 `ResolveFile`;上限夸克 2、其余 8;剩余时间不足 15s 停;AuthExpired 停后续;超限 `truncated=true` + hint;单文件失败写 `files[].link_error`,响应仍 200

### 6.2 单文件提链 ResolveFile

1. 查 `links` 缓存(share_key + fid):DB 故障显式报错(不得当 cache miss);游客命中带 Cookie 缓存 → 401;未过期 → 直接返回(cache_hit=true);**命中行的元数据是占位值(「文件」+0)时直链照用、展示名/大小再经 fileMeta 找一遍**(v2.11,防占位行永久遮蔽真实元数据)
2. Pick 账号(游客 nil)→ `buildRef`(ext 优先取缓存 link,其次分享快照;**ext 按「至少一个非空值」判定语义有效**,`{"api":""}` 这类「有键无值」不算命中;DB 故障向上传播)→ 在途并发闸(download_concurrency,超额排队)→ guard 执行 `GetDirectLink`
3. 成功:直链 Cookie AES-GCM 加密落 `links`;落库失败必须报错,不返回看似成功的下载入口(P2-6 已修)。**driver 在提链途中顺带拿到的元数据(`DirectLink.Name/Size`,mangodrive fs/get)优先于回问 driver 列目录**(v2.11);元数据兜底链:fileMeta(缓存行→快照→自愈→历史,占位行全部跳过)→ `DirectLink.Name/Size` → driverStat → 兜底名「文件」+0
4. `buildResult`:路径决策 + 签名 URL(fid 段按 §7.3 编码) + `need_headers`(明文 Cookie 非空 / 有 Referer / UA 与客户端不一致)
5. **快照自愈**(v2.10/v2.10.1):本分享快照为空壳(`raw_tree` 无有效内容)时,`buildRef`/`fileMeta` 从**同一 `pan_type` 的别的 share_key** 快照找回该 fid 的节点上下文 —— `share_key = sha1(pan|url|pwd)`,URL 形态不同即不同 key,故同一份分享常有两条快照(生产实证:同一夸克分享的「/s/x」与「/s/x#/list/share/y」两条快照一条完好一条损坏)。**身份闸 `sameShareIdentity`**:`fid` 不全局唯一(蓝奏云单文件节点 fid 恒为 `"file"`),跨快照取上下文必须先证明「同一份分享」——host 硬门槛 + 分享 id / `pwd_id` / host+path / 目录段前缀四级由强到弱,全不命中则**不自愈**(宁可不提链,也不静默给错文件)。**多条命中取最新**(v2.11:按 id 降序消费,最新快照的 stoken/path 最新);**扫描上限 `healScanLimit=64`**(v2.11.1:从最新往回数,窗口外老快照不参与自愈 —— 空壳 miss 的最坏成本封顶为常数次 JSON 解析)

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

**fid 段编码(v2.11)**:mangodrive 的 path 形态 fid 含 `/`,而 chi 的 `{fid}` 参数不跨段 —— 签发端(`/d`、`/stream`、`/api/v1/json`、aria2 中转 URL)一律经 `service.FIDPathSeg`(=`url.PathEscape`)编码;签名 payload 恒为**原始 fid**(编码前),验签端先 `FIDFromPathSeg` 解回原始形再验签。fid 不含保留字符时编码为恒等变换(夸克/蓝奏云不受影响)。

### 7.4 GET /stream/{pan}/{key}/{fid}?sig=

Relay.Serve:验签 → GetFreshLink → 解密 Cookie → 构造上游请求(UA/Referer/Cookie/Range)→ httpx DoStream(逐跳白名单重定向 + acw 自动重放)→

- 上游 ≥400:读 4KB 丢弃;401/403/404/410/412 时 `ExpireLink` 失效本地缓存(死链自愈)后报错
- 成功:透传 Content-Type/Length/Range/Content-Encoding/Accept-Ranges/ETag/Last-Modified;128 KB 块 + 攒够 256 KB Flush;单连接搬够 `relay.rotate_bytes`(默认 256 MiB)或上游断流时按收窄后的 `Range: bytes=<pos>-<limit>` 轮换/续传(≤3 次,且校验 ETag/Last-Modified 同一对象),`relay.stall_timeout`(默认 60s)看门狗掐掉黑洞连接,`relay.client_stall_timeout`(默认 120s)给每次写客户端/Flush 设软截止(客户端既不收也不关时放弃该流,记 `client_stalled`,离开前清掉截止以免 keep-alive 后续请求超时;见 §14 等待归因);多段 Range 原样整体透传、不轮换;客户端断开视为正常取消

### 7.5 aria2 推送

`POST /api/v1/downloads {pan, share_key, fid, dest?}`(登录):pan 与 share 快照一致性校验 → GetFreshLink → `CanAria2`? 直链 + headers(UA/Referer/Cookie) : stream URL(sign_ttl,fid 段同 §7.3 编码) → `aria2.addUri`(RPC secret)→ 落 `downloads` + 计数(空 gid 拒绝落库)。任务状态 `GET /downloads/{gid}`:`result` 缺失显式报上游错误(不再 nil 解引用);回写只更新非空字段(上游缺字段不得清空本地状态/失败原因),`UpdateDownload` 只更新 status/err_msg 两列。

### 7.6 设备历史同步(M3,POST/GET /api/v1/sync)

桌面端(PanRouter Desktop)把收藏/解析历史/下载记录推到服务端做多设备双向同步。**payload 是客户端语义的不透明 JSON,服务端不解析**(两仓零共享代码边界);删除以墓碑(`deleted=true`)传播;冲突按客户端时间戳 LWW 仲裁(`client_updated_at` 较新者胜,相同后来者胜)。

- `POST /api/v1/sync`(登录):`{device_id, items:[{kind, uid, deleted, updated_at, payload}]}` → `{accepted, skipped, server_time}`。kind ∈ `bookmark | link_history | download_record`;单批 ≤500 条,单条 payload ≤16KB;`(kind,uid)` 唯一,生效变更以 DELETE+INSERT 重写行 → `id` 即全局变更序号,严格单调
- `GET /api/v1/sync?since=<seq>&limit=500`(登录):`{items, last_seq}`,按 `id` 升序返回 `id > since` 的变更;`since=0` 全量,`last_seq` 作为下一页游标
- 管理页「设备同步数据」区块只读浏览全部上报数据(类型/关键词过滤)
- 隐私边界:同步默认关闭,桌面端用户显式开启;下载记录脱敏(不含本机路径/请求头),数据仅存自部署服务端

---

### 7.7 转存任务状态机与同目标写守卫

`TransferTask.state` 全集:`queued | running | pending | done | failed`,`done`/`failed` 为真终态。

| 状态 | 含义 | 同路径写占用 |
| --- | --- | --- |
| `queued` | 已登记,后台协程尚未开跑 | 未占用 |
| `running` | 解析源/搬运字节中(stream) | 占用 |
| `pending` | offline 端点已受理,产物尚未落池 | 占用 |
| `done` | 已确认写入目标路径(产物已确认存在) | 释放 |
| `failed` | 不可恢复失败(含确认窗口超时) | 释放 |

**offline 通道不再「受理即 done」**(v2.3 根因修复):

- 端点提交成功后任务停在 `pending`,`activeDest` 对目标路径的占用由确认器持有;
- 确认器(`watchOfflineArtifact`)按 `transfer.offline_confirm_poll`(默认 10s)轮询产物,命中 `!is_dir && size > 0 && (size >= expect 或产物 mtime 不早于任务起始)` → 收口 `done`,并用**真实落盘 size** 回填快照;
- 每轮除 `fs/get` 探预测路径外,还按约 6 倍轮询间隔对父目录做 `fs/list` 对账:产物若被上游按 HTTP `Content-Disposition` 改名落盘,对账能找回真实文件并把快照 `dest_path` 指向它;
- 远端任务轮询(`/api/task/offline_download/undone`)发现已取消/报错 → 立即收口 `failed`(`hint=upstream_error`,文案带远端原因),不空占 2h pending;
- 超过 `transfer.offline_confirm_after`(默认 2h)仍未见产物 → 最后对账一次,仍无则收口 `failed`(`hint=upstream_error`)并 Warn(含 expect/window),同路径写占用随之释放;
- 源大小未知(`expect<=0`)时不做「同名非空文件就算产物」:产物 mtime 必须不早于任务起始时刻(容忍 2min 时钟偏差),避免把同名旧残留误判成新产物;
- 配置为负数 = 关闭产物确认:回退到「受理即 done」并打 Warn,不再静默;
- pending 期间快照 `size=0`、进度未知;确认后才有真实大小/进度(桌面端 `progress` 对 pending 返回 null,用不确定进度条)。

为什么要改:旧契约下 offline 任务已报 `done` 而目标路径仍被写占用卡住,桌面端看到 done 就再提交同名 → 收到 `conflict`;生产查记显示 34 条后台转存中 6 条是这种误判重提(首次写入仍在进行,最长占用近 20min)。

状态语义对桌面端(`TransferTask` in `PanRouterTransferModels.kt`):

- `isTerminal = done || failed`;不包含 `pending`;
- `isRunning = queued || running || pending`;桌面端按 `isRunning` 决定是否继续轮询,所以 `pending` 必须入列,否则在产物真正落池前停抽;
- `progress` 对 `pending` 返回 null(字节未落池,不给确定进度);`stateLabelOf(pending) = "落池中"`。

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
| accounts | id, pan_type, name, cred_enc(AES-GCM BLOB), cred_version, status, cooldown_until, last_check_at | 多账号;状态机 ok →(risk_control)cooling →(冷却到期选中)ok;expired 由 AuthExpired 标记;**健康检查成功不解除 cooling/banned**(v2.11/v2.11.1,cooling 复位只经冷却到期,banned 解除只走人工);MarkStatus/MarkCooldown 定向列更新 |
| shares | pan_type, share_url, pwd, share_key, raw_tree(JSON), resolved_at | 分享快照;**raw_tree 是该分享唯一的 driver 上下文来源**(quark pwd_id/stoken、lanzou share_url/pwd),空壳(`null`/`[]`)即全分享失效 —— 写侧保护 + 读侧跨 share_key 自愈,见 §6.1/§6.2 |
| links | share_key, fid, file_name, size, direct_link, ua, referer, cookie_enc, bind_ip, expires_at, ext(JSON) | 直链缓存主表;ext 存 driver 上下文(pwd_id/stoken 等);ext 无有效内容时**不覆盖**该列(保原值),反查走 `ListLinksByFID` 按新→旧取第一条能解出 path 的行;**占位元数据(「文件」+0)不覆盖已有真实 file_name/size**(v2.11) |
| downloads | gid, fid, dest, route, status | aria2 任务记录;状态回写只更新 status/err_msg 且上游空值不清空本地已有值(v2.11) |
| settings | key, value | 预留 |
| audit_logs | ts, action, detail | login_ok / login_failed / cred_check / account_create / account_delete |
| sync_items | kind, uid((kind,uid) 唯一), device_id, payload(不透明 JSON), deleted, client_updated_at, server_updated_at | 设备历史同步条目(M3 §7.6);id 即全局变更序号(pull 游标) |

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

### 10.4 baidu(转存链 + 多账号池, v2.12)

与夸克同型(Cookie 登录态 + 分享链接 + 风控),差异是**必须转存到账号自己的网盘**才能取直链:

`share/verify(有提取码时)→ xpan/share 列目录(根 root=1,子目录 root=0 + BDCLND)→ share/transfer(ondup=newcopy 进 panrouter_tmp)→ locatedownload → 清理副本`

- **分享解析**匿名可用(未加密分享不需要登录);新 surl 是 base64url(含 `_`/`-`),旧 base62 正则会把新链接误判成「STOKEN 缺失」
- `xpan/share` 的 `size` 实测可能是 JSON **字符串**(生产实证 2026-11-13),按柔性整数解码;`fs_id`/`isdir` 同理按「number 或 string」读 —— 否则整次解析被误报成 `interface_changed`
- 不存在的 `/s/` 短链 errno=140(无 errmsg)→ `share_gone`,不落进含糊的 `upstream_error`
- 列目录根目录必须 `root=1`;子目录 `root=0` 且 Cookie 必须带 `BDCLND=<randsk>`(verify 返回的 sekey)
- **提链必须登录态**:转存 + locatedownload 全部依赖 bdstoken;`bdstoken` 随会话漂移,进程内按登录态指纹缓存,errno 105 时刷新重试一次
- 取链优先 `d.pcs.baidu.com` 客户端协议 locatedownload(硬编码 psign/rand/devuid 属上游公开常量,**会腐坏**),失败自动退回 `/api/filemetas?dlink=1`;两条路都拿不到才报 upstream_error
- 直链下发 `netdisk;…` UA + 完整 Cookie;`BindIP=false`,但链接 45 分钟后由 Resolver 续签
- 清理只删 `panrouter_tmp` 内本次转存路径(`defer cleanupSaved(WithoutCancel)`,独立有界 ctx),失败只记日志——宁可留副本,不误删用户文件
- **日转存硬闸 50 次/账号**(按登录态指纹 + 本地日计数,进程内):pilot 期防风控,超限诚实返回 `risk_control`;多账号池下配额按账号独立
- 默认 `limit_qps: 1`、`download_concurrency: 2`(百度风控最严);错误分类见 §8,`9019` 安全验证 → risk_control,账号侧 5min 冷却

### 10.5 限频 / 熔断 / 并发闸

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

- 端点:`POST /api/v1/auth/login`、`POST /api/v1/resolve(/batch)`、`GET|POST|DELETE /api/v1/accounts*`、`POST|GET /api/v1/downloads*`、`GET /api/v1/json/{pan}/{key}/{fid}`、`POST|GET /api/v1/sync`
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

- 公网 https://pan.example.com → CF Tunnel → 生产机 127.0.0.1:6400
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

- 指标(/metrics,Prometheus 文本):`panrouter_resolve_total{pan,kind}`、`panrouter_download_route_total{pan,route}`、`panrouter_link_cache_hit_total{pan}`、`panrouter_driver_error_total{pan,kind}`、`panrouter_local_throttle_total{pan,reason}`(reason=rate_limit|breaker|concurrency;我方准入拒绝,不计入上游错误)、`panrouter_breaker_open{pan,scope}`(scope=login|guest)、`panrouter_account_status{pan,status}`(状态全集补零)、`panrouter_relay_bytes_total{pan}`、`panrouter_relay_rotate_total{pan}`、`panrouter_relay_resume_total{pan}`、`panrouter_relay_stall_total{pan}`、`panrouter_relay_upstream_wait_seconds_total{pan}`(累计阻塞读上游秒数)、`panrouter_relay_client_wait_seconds_total{pan}`(累计阻塞写客户端秒数)、`panrouter_relay_streams_active{pan}`(gauge)、`panrouter_relay_link_refresh_total{pan}`(中转途中直链过期→重新解析成功的次数)、`panrouter_relay_abort_total{pan,reason=client_gone|client_stalled|upstream_interrupt|rotate_failed}`。等待归因用法:upstream_wait 的 rate(秒/秒)高而 bytes 率低 = 网盘限速;client_wait 高 = 我方或客户端瓶颈
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
- 竞态与顺序(2026-11-10):CI 增 `go test -race -shuffle=on ./...`——本 session 转存链新增并发面(同名守卫 claim/release、offline 落池确认器 goroutine、直链客户端),本机 32 位无法 -race,由 CI 兜底;shuffle 抓测试间耦合
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

- **v2.15.3(2026-11-14)**:transfer/account 九维 review 修复轮(HTTP 契约零变化)。**修复 4 项**:① **P1 Submit 后台 goroutine panic 无保护**:转存 goroutine 不在 HTTP handler 栈内,api 层 recoverer 保护不到——resolver/driver 解析畸形上游数据 panic 直接崩整个进程;`transfer.go` Submit 闭包加 `defer recover()`,收口成 `task.finishFailed("internal panic: ...")` + 日志,客户端可见、进程继续服务(新增契约测试 `TestTransferSubmitRecoversPanic`:resolver=nil 最小装配强制 nil 指针 panic → 5s 内 task 必须 failed 且 Error 非空);② **P2 offlineToken 裸读数据竞争**:`watchOfflineArtifact` 日志字段直接读 `task.offlineToken`,与 `transferOffline` 的写侧(t.mu 内)构成 race,-race CI 必报;改锁内读临时变量再进日志;③ **P3 Pick gauge 循环内放大刷新**:坏凭据循环内逐次 `RefreshStatusMetrics()`,n 个坏号触发 n 次全量 `ListAccounts` 重算,放大 DB 压力;提出循环——成功返回前与循环耗尽后各一次,一次 Pick 至多刷一次;④ **P3 retry 日志误导**:上游 fetch 首败即打 "retrying",但下一步其实还有第二跳主链路,日志撒谎;改为仅 `attempt == 0` 且确有重试语义时记录。**部署与生产验收**(CI run `43508099670`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `4d7cb3efef46`):带宽复测闭环——3MB fixture 经 transfer(stream channel,fid=direct,`speed.cloudflare.com` 源)云转存 done 13.8s;`/stream` 中转 2MiB 有界 Range → `206`、实收恰 2,097,152 B @112KB/s,全量 GET → `200` @550KB/s;**md5 全链路一致**(源文件经 resolve direct_url 直取 = 中转下载 = `aee2492f…`),零字节损坏;`relay_bytes_total{mangodrive}` 增量 5,243,028 ≈ 2MiB+3MB 计数吻合;`abort_total` 四类 reason × baidu/mangodrive 全 0,`resume`/`rotate` 无触发,新观测路径在真实流量下无异常。
- **v2.15.2(2026-11-14)**:**mangodrive ResolveShare 单层列表 → 服务端整树递归**(HTTP 契约向后兼容,`files[]` 形态不变、仅内容变全)。**问题**:mangodrive 是存储路径视图(URL path == 存储 path),没有分享快照可回放,`ResolveShare` 只 List 根层——嵌套分享的子目录文件对客户端完全不可见,auto-link 也只挂顶层;整树消费方(ingest_tree)只能在客户端自行 BFS 深链补全(2026-11-14 的 workaround,每次 resolve 是独立 share_key、快照碎片化)。**修复**:`ResolveShare` 目录分支改为 BFS 整树(`listTree`):根层 List 后对每个目录下探,`Name` 一律重写为**相对分享根的路径**(与其他网盘快照口径一致;`Ext["path"]` 仍是存储绝对路径,提链链路不变);上限 深度 8 层/总节点 1000(与客户端展开同量级,防循环引用/病态大树打爆 90s resolve 预算),触限或 `ctx` 耗尽时对最后一个节点打 `Ext["truncated"]="1"`(经 `listingTruncated` 升为 `ResolveShareResult.Truncated`,诚实截断不装完整);单层 List 失败弃置该子目录、其余继续(上游瞬时抖动不毁整树);目录 `seen` 去重防软链成环。文件单元素分支不变。**测试**:fake `fs/list` 改路径感知(旧 fake 对任意路径返回同一层,递归会撞上限);`TestDriverResolveAndLink` 升级断言整树(根 2 + sub 内 1,相对名/Ext path 双验);新增 `TestResolveShareTruncatesHonestyOnNodeCap`(1200 平铺文件 → 恰 1000 节点 + truncated=1)、`TestResolveShareDepthCapMarksTruncated`(自指目录成环 → 恰 8 层 + truncated=1 + List 恰 8 次不随环爆炸)。`go vet` 干净,全量 14 包绿。**部署与生产验收**(本机交叉编译 `CGO_ENABLED=0 GOOS=linux`,ldflags 注入 sha12;旧二进制备份 `panrouter.bak-20261114-040620`;deploy/pxed-install.sh 原子替换 + supervisor 重启,healthz 一次过):真实分享(`/_share_src`:根层 1 文件 + 子目录 2 payload)一次 resolve 返回**整棵 4 节点**(2 文件相对名 `子目录/payload-*.bin` + 3 文件全部 auto-link 带签名 URL,无 truncated/hint),share_key 不变(URL 同 key,快照升级为整树);`ingest_tree.py` 端到端 3/3 文件、rename 3 fixed、verify ALL-OK、raw_url 回读 SHA-256 全对——客户端 `expand_mangodrive_tree` 在服务端整树形态下自然空转(无目录节点),两层防线上互为冗余;提交期 29s→11s(省掉 N 次深链 resolve)。
- **v2.15.1(2026-11-14)**:v2.15.0 修复轮的自查回修(HTTP 契约零变化)。**问题**:复核上一轮六处改动,发现三处「修了但没生效 / 修出了新风险」、两处归因漏网:① **flush 错误归因在生产是死代码**:标准库 `ResponseController.Flush` 的解析顺序是 `FlushError → Flusher → Unwrap`,生产链路每个请求都被 `zapLogger` 用 chi `WrapResponseWriter` 包了一层,它只实现 `Flush()`(无返回值)→ 解析在包装器处就 `t.Flush(); return nil`,内层 `*http.response.FlushError` 永远走不到,`client_stalled` 的 flush 归因一次都不会触发;② **探针超时仍在伪造完整响应**:上一轮在窥探超时/读错时「把已读字节塞回 Body 并判非挑战」,而 relay 把 Body EOF 当正常收尾——chunked 下客户端会拿到一个「HTTP 完整」的截断文件且无任何错误,比干净报错更糟;③ **探针对所有响应生效**:`ContentLength <= 0`(chunked)一律窥探,二进制大流的首块交付被推迟到 64 KB 或 probe 超时,冷 CDN/限速下首 64 KB 慢于 5 s 时合法慢流被判死;④ **重放不可重读的请求体**:挑战/307 重放前不检查 `GetBody`,`io.Pipe` 一类流式 body 已被消费,重放发出空体,上游 4xx 后还会被误归因成「挑战持续」;⑤ 200 回退丢前缀的 `io.CopyN` 已改走 `touchReader`,但这段纯上游读取没有计入 `upstream_wait`,归因有缺口;⑥ 续传失败时若客户端恰在重连窗口断开(`rotate` 返回 `context.Canceled`),仍记 `upstream_interrupt`,与「用户关页面」混淆。**修复**:① 新增 `flushErr(w)`:按 `FlushError → Unwrap → Flusher` 自解析(只包 Flusher 的包装器先穿透拿到内层错误,`ErrNotSupported` 视为无需刷),relay 的 `flush` 改用它;② `inspectChallenge` 超时/读错一律返回错误,不再伪造完整 Body(`!stopped && len(peek)==max` 的竞态窗口同样按超时失败),并消除定时器与 `resp.Body` 字段替换的数据竞争(闭包只捕获局部 `body`,`atomic.Bool` 区分超时);③ 新增 `shouldProbeChallenge`:206/3xx 不探、412 必探、`ContentLength > 64 KB` 不探,其余只看文本类 Content-Type(空类型 / `text/*` / json / js / html),二进制 chunked 流零窥探;④ 新增 `rewindable`:重放前要求 `Body == nil || NoBody || GetBody != nil`,否则显式报错;⑤ 200 丢前缀的 `CopyN` 计时计入 `upstream_wait`;⑥ 续传失败且 `ctx.Err() != nil || errors.Is(rerr2, context.Canceled)` → 记 `client_gone` 并正常返回;⑦ 上游不可续传中断改为 `panic(http.ErrAbortHandler)` 断连接(chunked 下正常 return 会被 net/http 补结束块 = 静默坏文件),api `recoverer` 对该哨兵原样上抛(否则它写 500 后 net/http 仍会正常收尾)。**测试**:`TestInspectChallengeProbeIsBounded` 改为断言超时必须返回错误且不产生伪完整体;新增 `TestInspectChallengeSkipsBinaryChunkedStream`(octet-stream/chunked 零读取、原 Body 原封不动)、`TestRewindableRejectsStreamBody`(io.Pipe 拒绝,bytes.Reader/nil 放行)、`TestFlushErrSeesThroughFlusherOnlyWrapper`(chi 包装器下仍能拿到 `os.ErrDeadlineExceeded`,健康 writer 返回 nil)、`TestRecovererRepanicsAbortHandler`(哨兵透传)、`TestRecovererWrites500ForOtherPanics`;`TestRelayResumeExhaustedCountsUpstreamInterrupt` 改为断言 `panic(http.ErrAbortHandler)`(正常 return 即失败)。`go vet ./...` 干净,全量 17 包绿。**部署与生产验收**(CI run `43465930081`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `b65ea74854c4`):有界 Range 回归(`bytes=0-2097151`,loopback 绕 CF,mangodrive 3 MB 夹具)→ `206`、实收 **2,097,152 B** 恰好等于窗口,`relay_bytes_total` 增量恰为 2,097,152(3,036,002→5,133,154);本轮 `client_gone`/`upstream_interrupt`/`client_stalled`/`rotate_failed`/`resume`/`rotate` 全部 0→0,`streams_active` 回 0。下载速率仅 ~5.6 KB/s,瓶颈在宿主机出口(验收窗口内 pxed 对百度/夸克/Cloudflare 的 curl 全部超时,与 relay 无关);期间两次客户端超时断开被正确归因为 `client_gone`、一次上游断流成功续传(`resume_total`+1、`stall_total`+1),与既有归因语义一致。
- **v2.15.0(2026-11-13)**:中转 Code Review 根因修复轮(HTTP 契约零变化;`NewRelay` 签名不变)。**问题**:对 v2.12.6–v2.14.1 做九维评审,确认 6 处真实缺陷 + 1 处死代码,其中 3 处能把健康下载变成「半截文件」或「永久挂死」:① **上游体读取有两处绕过看门狗**:4xx 排空(`io.Copy(io.Discard, …)`)与 200 回退丢前缀(`io.CopyN`)原先直读 `resp.Body`,而 `upstreamConn.reading` 恒为 false → 看门狗按设计不判挂死,这段阻塞既无写截止也无任何出口,pos 很大或上游 tar-pit 时该流被永久钉住;② **ACW 挑战窥探无界**:`httpx.doWithACW` 在把 `*http.Response` 交给调用方**之前** `ReadAll` 最多 64 KB 判挑战页,而中转 client 是 `NoBodyTimeout=true`(`hc.Timeout=0`)→ 上游挂住时这一读永不返回,调用方连响应头都拿不到,看门狗/写截止全部无从介入(4xx 排空用例的栈实证卡在 `acw.go` 的 `io.ReadAll`);③ **直链过期只允许刷新一次**:`ss.refreshed` 在整条流生命周期内不复位,而跨两轮过期是大文件下载的常态,第二次过期直接按 `rotate_failed` 收尾;④ **短 `Content-Length` 未收敛写入上界**:上游 206 声明的 `Content-Length` 比客户端 Range 窗口更短时,`limit` 仍按窗口算,写超声明长度后 net/http 返回 `ErrContentLength` 并丢弃整块;⑤ **Flush 既无保护也不可归因**:`http.Flusher.Flush()` 无 deadline 且吞错误,客户端停读时 flush 可永久阻塞,失败还被混记成 `client_gone`;⑥ **解密失败静默当空 Cookie**:`handleDownload302`/`handleDirectJSON` 与 `resolver.buildResult` 都把 `DecryptBytes` 错误当「无 Cookie」继续走——302 把残缺凭据交给客户端,`buildResult` 还会据此给出错误的 `route`/`need_headers` 判断。**修复**:① 新增 `touchReader`(主循环之外的上游体读取唯一入口,走 `upstreamConn.read` 从而喂 `last`/`reading`),4xx 排空与 200 丢前缀都改用它,注释写明「直读 `resp.Body` 等于逃出看门狗」;② `inspectChallenge` 增 `probe` 参数(常量 `challengeProbe=5s`):`time.AfterFunc(probe, resp.Body.Close)` 打断窥探,超时/出错时**把已读字节塞回 `resp.Body`**并判「非挑战页」——窥探只影响挑战重放,不该判死一条合法字节流;③ `ss.refreshed` 语义改为「本轮过期事件是否已重解析」,续接成功后由 `Serve` 复位:刷新递归深度恒为 1,而一条流可经历多次过期事件;④ 206 且 `Content-Length` 可解析时把 `limit` 收到 `start+CL-1`,写入永不越过声明长度;⑤ 写侧统一用 `http.NewResponseController(w)`:`Flush` 前同样 `arming()` 设写截止、`FlushError()` 的错误进 `countClientAbort(..., "flush", ...)`,flush 阻塞时长计入 `client_wait`(退出时清写截止的逻辑不变);⑥ `links.go` 抽 `linkRoute`(解密失败 → `driver.NewErr(KindUpstream, "读取直链凭据失败,请重新解析")` 经 `writeErr` 输出 502),`buildResult` 解密失败时 fail-safe 为 `route=stream` + `need_headers=true`(未知 Cookie 必须经中转,由中转报真实错误);⑦ 删除死代码 `newRelayFixtureWithDriver`;`relay_link_refresh` 的 Help 改为「按网盘计数,并发流各自计数,重解析经 singleflight 合并」。**测试**(每条都能在无对应修复时失败):`TestRelayStalledBodyDrainDoesNotHang`(403 + 6 字节后挂住 → 100 ms 阈值下 403 必须 3 s 内浮出、`stall_total`≥1)、`TestRelayStalledPrefixDiscardDoesNotHang`(恒 200 上游 + 64 KB 轮换阈值,首连接 128 KB 后挂住 → Serve 4 s 内返回)、`TestRelayNeverWritesPastDeclaredContentLength`(上游每段收窄到 100 KB、32 KB 阈值 → 体精确 102400 B、每次轮换 Range 终点都是 102399)、`TestRelayRefreshesExpiredLinkTwiceInOneStream`(按位置失效 tok1→tok2→tok3、512 KB 体 → 体完整、恰 2 次 403、`link_refresh_total`=2、`rotate_failed`=0)、`TestRelayFlushStallAttributedAsClientStalled`(`flushFailWriter` 让 `FlushError` 睡到截止再返 `os.ErrDeadlineExceeded` → `client_stalled`=1、`client_gone`=0、`client_wait`≥0.1s)、`TestInspectChallengeProbeIsBounded`(阻塞体 + 50 ms probe → 有界返回、判非挑战、替换体恰好交出已读字节)、`TestE2EStreamClientStallThroughMiddleware`(完整中间件栈 + 热加载配置,读 1 KB 即停 → `client_stalled`=1、`streams_active` 回 0、`client_gone`=0)。`go vet ./...` 干净,全量 17 包绿。
**部署与生产验收**(CI run `43452152702`,Vet&Test 含 `-race` 1m43s + Deploy 2m43s 全绿,`/healthz` `ccb6e0fb95d7`、`/readyz` ready):① relay 全部指标 0 值序列就位,`abort_total{reason="client_stalled"}` 四个网盘齐备;② 有界 Range 回归(`bytes=0-2097151`,loopback 绕 CF):`206`、实收 **2097152 B** 恰好等于窗口、4.21s(≈499 KB/s),`relay_bytes_total{pan="quark"}` 增量恰为 2,097,152,`abort/rotate/resume` 全 0、`streams_active` 回 0;③ 同一文件整份下载时 `relay_bytes_total` 恰等于文件大小 10,446,961 B、无 abort/stall,而公网侧 curl 300s 只收到 721 KB——中转写入已完成,瓶颈在「客户端→CF 边缘」这一段,与本次改动无关(同时证明 `Flush` 错误不会在中转侧被误判);④ `/api/v1/json` 200(`route=aria2`、`need_headers=true`,linkRoute 解密成功路径),`/d` 按 cloud 画像正常 302 降级到 `/stream`;⑤ 解密失败路径无法在生产无损构造,由 `internal/api` 与 `internal/service` 单测覆盖。
- **v2.14.1(2026-11-13)**:补上「长下载途中直链过期」的端到端测试 + 可见性(HTTP 契约零变化)。**背景**:`rotate()` 里早有「401/403/404/410/412 → `ExpireLink` → 重新解析 → 用新直链从 pos 续」的逻辑,但**没有任何测试**——此前只有「首次请求就 403」的用例,那条走的是 `Serve` 开头的分支;真正危险的是几小时后直链到期、正在轮换的这条路径,写错了就是「下载到一半凭空中断」且无人能看出来。**本次**:① 新增 `TestRelayRefreshesExpiredLinkMidStream`(上游用 token 区分直链:初始 200 完整响应后立刻把老 token 作废,下一次轮换收到 403 → 重新解析拿到新 token → 206 续上;断言体逐字节完整、403 恰 1 次、`link_refresh_total`=1、`rotate_failed`=0、`resume`=0);② 新增 `TestRelayRefreshesExpiredLinkOnlyOnce`(刷新后仍 403 → 恰好 2 次被拒请求后按 `rotate_failed` 收尾,证明不会「刷新→失败→再刷新」递归);③ 新增指标 `panrouter_relay_link_refresh_total{pan}`(0 值预置)与 Warn 日志(`share/fid/pos/status`)——否则运维只能看到「偶尔慢一下」,查不到是直链过期;④ 测试 fixture 抽成 `newRelayFixtureLink`(可给带 token 的完整直链),`fakeDriver.linkURL` 加锁(该用例会在测试中途改写它,CI `-race` 下必须安全)。`go vet` 干净、17 包全绿,service 包 `-count=2` 无抖动。**部署与生产验收**(CI run `43437459489`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `3357e48c65b9`):① `panrouter_relay_link_refresh_total{pan="quark"}` 0 值序列就位、`# TYPE ... counter` 齐备;② 回归正常下载 2 MB:读满 2097152 字节、`active` 回 0,`rotate_failed` / `upstream_interrupt` / `link_refresh` 全部 0→0——本次只动「轮换遇到失效直链」那条分支,健康路径指标零漂移;③ 前后 healthz 版本一致。生产上 `link_refresh` 何时增长取决于直链真实过期,故只验序列就位与健康路径无扰动,过期恢复的确定性验证由两条新单测兜底。
- **v2.14.0(2026-11-13)**:中转「客户端写挂死」保护 + 等待时间归因(HTTP 契约零变化;`NewRelay` 签名不变,新增配置 `relay.client_stall_timeout`)。**问题**:`cmd/panrouter/main.go` 只设 `ReadHeaderTimeout`、**没有 `WriteTimeout`**——这是流式代理的必然选择(给整体写设硬超时会掐断慢速大文件下载),但代价是「客户端半死不活」这条路径无人兜底:客户端既不读也不关(暂停下载、进程被杀但 TCP 未 RST、网络黑洞)时 `w.Write` 永久阻塞,上游连接、128 KB 缓冲和一个 goroutine 被长占,`streams_active` 只涨不落;停流看门狗**正确地**不介入(它只管「正阻塞在读上游」,误判会把慢客户端反复重连)。**修复**:① 每次 `Write`/`Flush` 前用 `http.NewResponseController(w).SetWriteDeadline(now+clientStall)` 设**单次写软截止**,客户端恢复读后每次写自动重设——慢而健康的客户端不会被误杀;`ErrNotSupported`(httptest recorder 等)静默忽略。② 写错误按 `errors.Is(err, os.ErrDeadlineExceeded)` 分流:新增归因 `client_stalled`(写超时放弃,Warn 日志带 share/fid/pos),其余仍记 `client_gone`——否则会把「客户端卡死」混进「用户正常关页面」。③ **keep-alive 清理**:server 的 `WriteTimeout=0` 不在下一个请求开始时重置写截止,本请求留下的值会延续到同连接后续请求(空闲超过阈值再来,一写就超时),所以 Serve 返回前 `SetWriteDeadline(time.Time{})` 清掉。④ **等待归因**:新增 `panrouter_relay_upstream_wait_seconds_total{pan}`(阻塞在读上游的累计秒数)与 `panrouter_relay_client_wait_seconds_total{pan}`(阻塞在写客户端的累计秒数)——relay 时间只会花在这两处;配合 `relay_bytes_total` 的 `rate()`:upstream_wait 高而字节率低 = **网盘限速**,client_wait 高 = **我方或客户端瓶颈**,不再靠猜。⑤ 分块循环复用 `pan="..."` 标签 map(原来每 128 KB 新建一个)。**配置**:`relay.client_stall_timeout`(默认 120s;0=默认、负值=关闭),与另两项同规则热加载、每请求求值一次;`Relay.clientStallTimeout` 为单测覆盖值,优先级「显式字段 > 配置 > 内置默认」。**测试**:`TestRelayClientStallAbortedAndAttributed`(`deadlineWriter` 实现 `SetWriteDeadline` 并如实返回 `ErrDeadlineExceeded` → 150 ms 阈值下 Serve 必须放弃,断言 `client_stalled`=1、`client_gone`=0、`streams_active` 回 0、`client_wait`≥0.1s);stall 用例补 `upstream_wait`≥0.15s(黑洞段的等待必须归到上游侧)、慢客户端用例补 `client_wait`≥0.5s 且 `upstream_wait < client_wait`(600 ms 阻塞归到客户端侧);`TestRelayTuningPrecedence`/`TestLoadRelaySection` 覆盖新配置项的三级求值与热加载。`go vet ./...` 干净,全量 17 包绿。**部署与生产验收**(CI run `43428415280`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `6aca11905bd0`):① 两个 wait counter 与 `reason="client_stalled"` 的 0 值序列均已就位、`# TYPE` 为 counter;② 正常下载 4 MB 后断开——`upstream_wait` +3.07s、`client_wait` +0.07s(3 秒的下载几乎全花在等上游,归因正确),`active` 回 0、`client_gone` +1,无误报;③ 真实「客户端半死不活」两轮探测(分别读 1 MB、以及全速读 25 s 后停读 140 s,均不断连接):两轮 `active` 稳定 =1 直到客户端关闭,`client_stalled` 恒 0、`client_gone` 各 +1,而期间 `upstream_wait` +133s / +140s、`client_wait` 仅 +1.8s / +0.2s——该拓扑下 quark 限速太狠(25 s 只吐 2.6 MB),客户端停读时 relay 基本一直阻塞在**读上游**,写侧从未阻塞满 120s,CF/cloudflared 还额外缓冲了 14.5 MB,所以保护没有触发,**也说明它不会误杀正常流**;这条路径的确定性验证由 `TestRelayClientStallAbortedAndAttributed` 兜底(只有上游足够快、客户端真停读时才会走到,如内网私有盘);④ 之后同机继续正常下载 2 MB、healthz 不变,连接与中转链路未受影响。
- **v2.13.0(2026-11-13)**:Relay 参数配置化 + 多段 Range 端到端 + 停流/并发可观测性(HTTP 契约零变化;`NewRelay` 签名多一个 `*config.Provider`,调用方仅 app 装配一处)。**① relay 配置段**:新增 `relay.rotate_bytes` / `relay.stall_timeout`(0 = 内置默认 256 MiB / 60s,负值 = 关闭该特性;config.example.yaml 已补说明)。每个请求开始时读取一次,热加载改配置只影响新请求、**无需重启**;`Relay.rotateBytes/stallTimeout` 退化成单测覆盖值(生产恒 0),求值优先级「显式字段 > 配置 > 内置默认」。**② 多段 Range(multipart/byteranges)端到端**:新增 `TestRelayMultipartRangePassesThroughVerbatim`——客户端发 `bytes=0-3,10-13`,上游回 206 + 完整 multipart 体,断言整段逐字节透传(不按任何终点钳制)、`Content-Type` 原样、**恰 1 次上游请求**(多段无法用单条 Range 续接,轮换/续传必须禁用,32 KB 阈值下一条 128 KB Read 就足以暴露误开轮换)。**③ 可观测性**:新增 `panrouter_relay_stall_total{pan}`(看门狗真正判挂死的次数;先记账再 `cancel`,与续传成败无关——此前只能从 resume_total 倒推,分不清「上游偶发断流续上了」和「上游黑洞」)与 `panrouter_relay_streams_active{pan}`(gauge:响应头发送即 +1、Serve 返回 -1,与 bytes_total 的 rate() 配合可分「并发高但每条都慢」和「压根没有流」);metrics 包新增 `AddGauge`(并发增减型 gauge,`Set` 在这种场景会互相覆盖)。新增回归:`TestRelayTuningPrecedence`(三级求值 / 负值透传=关闭 / 配置 0=回落默认 / nil cfgp 不归零)、`TestLoadRelaySection`(YAML 解析 + 热加载允许变更 relay 段);stall 测试追加断言 `stall_total`=1(看门狗掐了一次)、慢客户端测试追加断言 `stall_total`=0 且 streams_active 归 0。测试:`go vet ./...` 干净,全量 17 包绿。**部署与生产验收**(CI run `43418169193`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `db2ee36d2b43`):① 新序列就位且类型正确——`panrouter_relay_stall_total{pan="quark"}` counter、`panrouter_relay_streams_active{pan="quark"}` gauge 均 0 值可见;② 下载 4 MB 断开:**流进行中 `streams_active` 实测 =1**,关流后回 0,`stall_total` 0→0,归因 `client_gone` 0→1、`upstream_interrupt` 0→0;③ 真实多段 Range 探测(`bytes=0-1023,4096-8191`):quark 对多段只回第一个分段的普通 206(`Content-Length: 1024`、非 multipart),relay 把它当单段完整透传、不启动任何轮换/续传(新 `rotate`/`resume`/`rotate_failed` 全 0),healthz 前后不变——多段真正会触发的 `multipart/byteranges` 体由 `TestRelayMultipartRangePassesThroughVerbatim` 兜底,因为 quark 这条上游从不发 multipart。

- **v2.12.9(2026-11-13)**:中转 Code Review 根因修复(HTTP 契约零变化;`NewRelay` 签名不变,`ExtraDriver` 新增可选 `RelayClient`)。**问题**:对 v2.12.6–v2.12.8 做生产向评审时确认了 5 处真实缺陷,其中两处能把正常下载掐成半截文件:① **看门狗误判慢客户端**:判挂死只看「距上次读到字节的时长」,而 relay 在 `w.Write` 阻塞(客户端暂停/网络抖动)期间不读上游,`last` 会一直变旧;客户端一恢复,刚发起的 `Read` 立刻显得已空闲 N 秒,任一 tick 落在 Read 返回之前就把健康连接 `cancel()` —— 单测里 600 ms 的客户端阻塞即可稳定复现「凭空续传」,默认 60s 阈值下只需客户端暂停超过 60s 再恢复;② **有界 Range 没有被尊重**:轮换/续传一律发 `Range: bytes=<pos>-`(开区间),客户端要的是 `bytes=A-B` 时上游会继续吐到文件尾,而响应头里的 `Content-Length` 早已按 B 发出,net/http 直接拒收整块(`http: wrote more than the declared Content-Length`),客户端收到意外 EOF 还被误归 `client_gone`(本地已复现);③ **声明 Range 却忽略时不降级**:上游 `Accept-Ranges: bytes` 但对 Range 回 200 时 `canRotate` 仍为真,每次轮换都重下并丢弃前缀(256 MB 起跳的带宽放大),与 v2.12.6 文档里「不再轮换」的表述不符;④ 丢弃前缀的 `io.CopyN` 期间不 `touch()`,pos 大(>5 MB/s × 60s)就会被看门狗判成挂死、白重下;⑤ 轮换/续传不比对 ETag/Last-Modified,上游在边界处换了对象就把两个版本拼成「长度正确、内容错误」的文件。**修复**:① `upstreamConn` 增 `reading`/`readStart`,看门狗只在「正阻塞在读上游」时生效,且基准取 `max(last, readStart)`(本轮读自身久等不到字节才算挂死);② 新增 `rangeEndLimit` 求客户端允许的包含式终点,轮换/续传发 `bytes=<pos>-<limit>`,写入前按 `limit` 夹取、写满即收尾;多段/畸形 Range 或总长未知时 `limited=false` → 关闭轮换且不钳制(普通字节流原样转发),并补上起点可解析校验;③ 回退到 200 时 `canRotate=false`(只兜底一次);④ 前缀丢弃改用 `touchReader` 喂看门狗;⑤ 新增 `sameObject()` 比对校验器,不一致立即中止并记 `rotate_failed`,绝不续写;⑥ `relayRotateBytes` 包级变量收进 `Relay.rotateBytes` 字段(与 v2.12.8 的 data race 同源);⑦ `Serve` 的「error 只在写响应头之前返回」契约写进注释与 `handleStream` 调用点;⑧ 指标名/归因标签常量化;⑨ 装配层为 Relay 单独建一套 client(`newClient(dc, true)`),`DoStreamFresh` 的 `Transport.CloseIdleConnections()` 不再波及 Transfer 共用的那份(此前会顺带关掉 Transfer 的空闲连接)。新增回归:`TestRelaySlowClientDoesNotTripStallWatchdog`(客户端阻塞 600 ms,`-count=6` → 上游请求恒为 1、resume=0)、`TestRelayBoundedRangeRotationStaysWithinRequest`(640 KB / `bytes=1000-399999` / 32 KB 阈值 → 体精确等于请求窗口、每次轮换 Range 终点都是 399999)、`TestRelayAdvertisedRangeIgnoredStopsRotating`(声明 Range 却回 200 → 恰 2 次请求、rotate=1)、`TestRelayAbortsOnValidatorMismatch`(每次请求换 ETag → 只写首个版本前缀、`rotate_failed`=1)、`TestRangeEndLimit`(11 组边界)。测试:`go vet ./...` 干净,全量 17 包绿。**部署与生产验收**(CI run `43323492098`,Vet&Test 含 `-race` + Deploy 全绿,`/healthz` `c49e566994a1`):① 10,446,961 B 文件整份下载字节全等(4.13 MB/s);② 有界 Range `bytes=1000-4999999` → 206、`Content-Length`=4,999,000 且与实收一致、内容与整份前缀切片逐字节相等(这正是旧实现会撞 `wrote more than the declared Content-Length` 的组合);③ 1 GB 文件读到 4 MB 主动断开 → `client_gone` +1、`upstream_interrupt` 0;④ **轮换首次在生产被观测到**:1 GB 文件取 `bytes=0-300000000`(越过 256 MiB 阈值约 31 MB)→ 实收 300,000,001 字节全等、`relay_rotate_total` 0→1、resume/rotate_failed 均为 0,9.7 MB/s —— 此前因网盘限速一直无法验证的轮换路径,这次在有界 Range 下同样成立。
- **v2.12.8(2026-11-13)**:中转断流自动续传 + 停流看门狗(HTTP 契约零变化,`NewRelay` 签名不变)。**问题**:响应头与 `Content-Length` 一旦发出就不能收回,所以上游中途断流时旧实现只能 `return nil` —— 客户端拿到的是"半截文件 + 传输中断",而 `stream` client 为了不掐掉慢流设了 `NoBodyTimeout:true`,上游真挂死(黑洞/半开连接)时 `Read` 会永久阻塞,客户端连接就一直吊在 0 B/s。**修复**:① 上游连接抽象成 `upstreamConn`(独立可取消 ctx + 最后读到字节的时间戳 + 幂等 `close`);② `Relay.stallTimeout`(默认 60s)看门狗(做成字段而非包级变量:看门狗在独立 goroutine 里读,包级可变变量会被单测"改了再改回去"撞出 data race,CI `-race` 实证过一次):连续一个字节都没吐就 `cancel()` 打断 `Read`,把"挂死"降级成一次可处理错误;③ 断流(非 EOF、非客户端取消)且在 Range 可达(`canRotate && pos<total`)时,用 `rotate()` 从 `pos` 续传,最多 `relayResumeMax=3` 次,成功记 `panrouter_relay_resume_total{pan}`,续不动才记 `upstream_interrupt`;④ 客户端断开仍按父 `ctx.Err()!=nil` 归 `client_gone`——看门狗取消的是连接级子 ctx,两者不会互相误判。新增回归:`TestRelayResumesAfterUpstreamAbort`(上游写一半 `ErrAbortHandler` 掐连接 → 第 2 次请求 `Range: bytes=102400-204799`(终点收窄到客户端允许范围,见 v2.12.9) → 体完整、resume=1、abort=0)、`TestRelayResumesAfterStall`(上游吐 64 KB 后挂住 → 200ms 阈值下看门狗取消 → 续传补齐体)、`TestRelayResumeExhaustedCountsUpstreamInterrupt`(每次必断 → 恰 1+3 次请求、resume=3、`upstream_interrupt`=1)。⑤ **编码协商收口**:中转上游请求显式 `Accept-Encoding: identity`(Go 只在"自己加的 Accept-Encoding"场景透明解压,显式声明后不再插手),并把 `Content-Encoding` 加入透传头白名单——原实现既可能让 Go 解压后抹掉 `Content-Length`(Range/续传边界错位),也可能把压缩字节当明文发给客户端。新增 `TestRelayAsksIdentityAndPassesContentEncoding`。测试:`go vet ./...` 干净,全量 17 包绿,relay 用例 `-count=2` 无抖动。
- **v2.12.7(2026-11-13)**:中转中断归因修正(HTTP 契约零变化)。客户端断开时请求 ctx 先被取消,上游 `Read` 返回 `context canceled`,原实现把它计入 `panrouter_relay_abort_total{reason="upstream_interrupt"}`——线上实测 23 次「上游中断」其实全是本地探测被 `timeout` 掐断的流,导致「用户关页面」与「上游塌了」不可区分。改为先判 `ctx.Err()!=nil || errors.Is(rerr, context.Canceled)` → `client_gone`,其余才是 `upstream_interrupt`。新增回归 `TestRelayClientCancelCountedAsClientGone`(上游挂住 → 取消 ctx → Serve 正常返回 → `client_gone` 为 1、`upstream_interrupt` 为 0)。测试:`go vet ./...` 干净,全量 17 包绿。
- **v2.12.6(2026-11-13)**:中转吞吐根因优化(HTTP 契约零变化,`NewRelay` 内部签名多一个 `*metrics.Registry`)。**实证**:把 51 文件 / 13.16 GB 的 quark 分享整包经生产链路下载到本地,单流起步 3.5 MB/s,但同一条上游长连接累计跑过约 1 GB 后被压到 ~100 KB/s(同一时刻三条老连接合计 0.32 MB/s、两条新建连接合计 7.2 MB/s,`panrouter_account_status{pan="quark"}="ok"` 且 cooling/expired/banned 全 0 —— 不是我方限频、也不是账号被风控)。**三处修复**:① 复制循环由「每 32 KB `Write`+`Flush`」改为 128 KB 块 + 攒够 256 KB 再 Flush,13 GB 从约 40 万次 flush 降到 5 万次(边下边播延迟仍 <100 ms);② 新增 `Relay` 侧字节/轮换/中断指标 `panrouter_relay_bytes_total{pan}`、`panrouter_relay_rotate_total{pan}`、`panrouter_relay_abort_total{pan,reason=client_gone|upstream_interrupt|rotate_failed}`,`NewRelay` 时按各网盘补 0 值序列(没有序列 ≠ 全 0);③ **上游连接主动轮换**:单连接搬够 `relayRotateBytes`(256 MB)且文件尚有剩余时,按 `Range: bytes=<pos>-` 重开一条连接续写同一个客户端响应,客户端侧字节流连续不可感知;仅在上游「已知总长度 + 声明支持 Range(206 或 `Accept-Ranges: bytes`)」时启用,上游忽略 Range 回 200 则丢弃已发字节继续用(不再轮换在 v2.12.9 才真正生效,防带宽放大);轮换遇 401/403/404/410/412 先 `ExpireLink` 再刷新直链重试一次。配套 `httpx.Client.DoStreamFresh`:请求带 `Connection: close` 且发起前 `CloseIdleConnections()`,保证轮换真的换 TCP 连接(复用池里一条已被降速的旧连接等于没轮换),同时中转流量不回连接池、不把降速传染给后续请求。新增回归 `TestRelayRotatesUpstreamConnection`(320 KB/32 KB 阈值 → ≥3 条上游请求、Range 单调前进、`ConnState` 计数证明每条请求都是新连接、字节与轮换指标命中)、`TestRelayNoRotateWhenUpstreamLacksRange`(无 `Accept-Ranges` 时只允许 1 次上游请求);既有 5 例 relay 用例保持绿。测试:`go vet ./...` 干净,全量 17 包绿,`-count=5` 无抖动。
- **v2.12.5(2026-11-13)**:启动即初始化 `panrouter_account_status`。该 gauge 原先只在账号事件(`Create/Delete/MarkStatus/MarkCooldown/checkAcc/Pick` 复位)后刷新,进程重启后要等看门狗首轮(默认 30min)或首个事件才出现 —— 对监控来说「序列缺失」与「全 0」不可区分,重启窗口内的账号状态面板是空的。`app.Build` 装配完 `AccountService` 后调用一次 `RefreshStatusMetrics()`(只读 `ListAccounts` + 按状态全集补零),新增回归 `TestBuildInitializesAccountStatusGauge`(预置账号 → Build → `/metrics` 必须含 `panrouter_account_status{pan="quark",status="ok"} 1`)。契约零变化;顺带修掉 v2.12.4 去白名单后暴露的「重启后 metrics 空 body」观感问题(有账号时不再为空)。测试:`go vet ./...` 干净,全量 `internal/app` 绿。
- **v2.12.4(2026-11-13)**:metrics 去掉手工白名单(静默丢失指标的根因)。`Registry.snapshot()` 原按硬编码列表逐条输出,`Inc/Set` 与导出是两处独立维护——v2.12.3 新增 `panrouter_local_throttle_total` 时计数已在跑,`/metrics` 里却看不到(只有对照单测断言时才暴露)。改为按已注册名字全集输出(counters 优先、名字排序,文本稳定;`Help` 继续生效),新增回归 `TestSnapshotExportsEveryRegisteredMetric`(未预登记的 counter/gauge 必须出现在导出中)。契约零变化:既有指标名/标签/类型不变,仅输出顺序变为按名排序(Prometheus 文本无语义顺序)。测试:`go vet ./...` 干净,全量 17 包绿。
- **v2.12.3(2026-11-13)**:本地准入拒绝不得冷却账号(生产深度测试实证,HTTP 契约零变化)。**根因**:`guard` 的熔断打开与本地令牌桶耗尽都复用 `driver.KindRiskControl` 上报,而 `mapErr` 对该 kind 一律 `MarkCooldown(5min)` —— 我方自己的准入闸把正常账号当成被上游风控惩罚,顺带记进冷却、引出轮转。**现场复现**:线上版本 `163805a`,6 个并发登录态解析(quark `limit_qps=2`)→ `200 429 429 429 200 200`,随后 `GET /api/v1/accounts` 显示 quark 账号 id=1 为 `cooling`(cooldown_until 5 分钟后)。**修复**:新增 `errLocalThrottle`(Unwrap 透出内层 `driver.Error`,`api.writeErr` 仍走 `errors.As` → risk_control → 429,客户端契约不变);`guard` 的熔断/限频与 `acquire` 的并发闸超时都包装为本地准入,`mapErr` 命中即早退、不改账号状态;新增 `panrouter_local_throttle_total{pan,reason}`(reason=rate_limit|breaker|concurrency)与 `driver_error_total` 分离,避免把「我方限流」统计成「上游出错」。**保留的副作用**:真正的上游 `risk_control` 仍冷却账号(对照组测试锁定,修复不得把该副作用一并抹掉)。回归 `internal/service/resolver_local_throttle_test.go` 3 例(本地限频→账号仍 ok 且 metric 命中/熔断打开→不再打上游且账号仍 ok/上游风控→仍 cooling)。测试:`go vet ./...` 干净,全量 17 包绿。
- **v2.12.2(2026-11-13)**:账号池根因修复轮(夸克真实分享端到端测试暴露,契约零变化)。**根因是同一条链上的三层缺陷**:① `AccountService.Pick` 轮转选中账号后,解密失败直接返回错误——一个解不开的凭据(主密钥变更/数据损坏)就让该 pan 的**全部**登录态请求 502「读取账号凭据失败」,且错误不落库(状态仍 ok),下一轮继续撞同一个坏号,等价于永久毒化账号池;② `checkAcc`(管理页刷新与 watchdog 共用)遇解密失败提前 return,`Valid=false` 只进响应、不落库 —— 手动刷新与每轮看门狗都"检查完成",坏号永远留在可用集合里;③ 真正的上游触发条件(主密钥来源变化:`PANROUTER_MASTER_KEY` 未设置回落 `jwt_secret`,或密钥轮换)在启动时完全不可见,只能在首个请求失败时表现为误导性的 502。**修复**:① `Pick` 改为遍历候选(仍从网盘维度轮转游标起选),遇解密失败即 `MarkAccountStatus(expired)` + `cred_unreadable` 审计 + 指标刷新并继续尝试下一个;全部不可用时退化为「无可用账号」(免登录盘可匿名,不再用 502 掩盖根因),坏号退出可选集合后轮转自动收敛;② `checkAcc` 解密失败落库 `expired`(写 `last_check_at`;`banned` 人工终态不越权改写),watchdog/手动刷新从此能自愈账号池;③ 新增 `AccountService.AuditUnreadable`,`app.Build` 启动时扫描并显式告警「N/M accounts cannot be decrypted with the current master key」。**生产/现场实证**(本地 DB 两个 quark 账号,其一密文由旧密钥加密):修复前同一 `POST /api/v1/resolve`(带管理员 token)稳定 502;修复后 200、51 个文件、自动提链 2/2、坏号被自动标 expired 并留 `cred_unreadable` 审计,启动日志首行即点明主密钥不匹配。回归 `internal/service/account_pool_test.go` 6 例(跳过坏号并落库 expired/全坏退化无账号/多账号轮转不退化/刷新落库 expired/banned 终态保持/启动自检只读)。测试:`go vet ./...` 干净,全量 17 包绿。
- **v2.12.1(2026-11-13)**:百度 driver 生产实跑两处口径修正(均在 v2.12 部署后的线上回归中发现,契约零变化)。① **`xpan/share` 的 `size` 可能是 JSON 字符串**:线上用真实加密分享(匿名 + 提取码)列目录时整次解析被报成 `interface_changed`,底层错误是 `json: cannot unmarshal string into Go struct field shareListResp.list.0.size of type int64`。`size` 改为柔性整数 `flexInt64`(number 与 string 都接受,`""`/`null` 归 0);`fs_id`/`isdir` 本就是 `json.RawMessage`,number 与 string 均可读。回归 `TestResolveShareToleratesStringSize`(字符串 size + 数字 fs_id/isdir 必须解析出正确大小与 fid)。② **errno 140 → `share_gone`**:线上喂不存在的 `/s/` 短链,xpan/share 直接回 `errno=140`、`errmsg` 为空,原走文案兜底报 502 `upstream_error`;显式归类后返回 404「百度分享链接无效或不存在」。判别力:修复后匿名 + `pwd=petr` 的加密分享解析出 9 个文件,不存在的短链 404 share_gone。测试:`go vet ./...` 干净,全量 17 包绿。

- **v2.12(2026-11-13)**:新增**百度网盘 driver**(`internal/driver/baidu`,契约零变化,纯增量)。① **链路**:`share/verify → xpan/share 列目录 → share/transfer(ondup=newcopy) → locatedownload → 清理副本`;与夸克同型但百度**必须转存**才能取直链。实现口径逐条来自本仓姊妹项目 YunX-Desktop 生产在用的 `BaiduApi` + 上游公开实现交叉验证,不是推测。② **已知坑显式处理**:新 surl 是 base64url(`_`/`-`),旧 base62 正则会把新链接误判成「STOKEN 缺失」,解析器按 `[0-9A-Za-z_-]` 取;列目录根目录必须 `root=1`、子目录 `root=0` 且 Cookie 带 `BDCLND=<randsk>`;`bdstoken` 随会话漂移 → 按登录态指纹缓存 + errno 105 刷新重试一次;locatedownload 的 psign/rand/devuid 是上游公开硬编码常量(会腐坏)→ 失败自动退回 `filemetas?dlink=1`,两路都失败才报 upstream_error。③ **安全/pilot 闸**:日转存 50 次/账号硬闸(按登录态指纹 + 本地日,超限诚实返回 risk_control,不硬闯风控);默认 `limit_qps: 1`、`download_concurrency: 2`;`redirect_allow` 仅 `.baidu.com/.baidupcs.com/.baidupcs.cn/.bdstatic.com`;清理只删 `panrouter_tmp` 内本次转存路径,独立有界 ctx,失败只记日志。④ **多账号池真正生效**(此前后端 `store.PickAccount` 恒取「最近检查优先」→ 同网盘多账号永远只有第一个在干活,池子退化成单账号,风控命中即全链路停摆):`AccountService.Pick` 改为按网盘轮转(候选集合语义不变:ok 或冷却到期,expired/banned 永不可选),新增 `TestPickRotatesAccounts`。⑤ 注册与配置:`app.go` factory + `config.Default()`/`config.example.yaml` 的 `domain_routes.baidu` 与 `drivers.baidu`。测试:新增 `internal/driver/baidu` 8 个用例(链接形态/errno 分类/根与子目录遍历 + BDCLND 透传/转存链选 encrypt=0/fallback/配额闸/凭据校验),`go vet ./...` 干净,全量 17 包绿。

- **v2.11.1(2026-11-12)**:深度 review 收口轮(phase-gate review 两项 backlog 落地)。① **banned 终态不被探测复活**:checkAcc 对凭据有效的状态保持逻辑原只保护 cooling —— banned(人工/风控终态,当前无自动写入方,保留语义供运维手改与仪表盘展示)的账号若凭据「碰巧有效」,一次健康检查就被写回 ok,终态被旁路打穿;改为 `switch acc.Status { case "cooling", "banned": status = acc.Status }`,终态的唯一解除路径是人工操作(与状态机单向逃逸只有冷却到期的口径一致);失效→expired 迁移仍放行。② **快照自愈扫描上限 `healScanLimit = 64`**:healShareNode 原对同 pan_type **全部** shares 逆序全扫 + 逐条 JSON 解析,而自愈找的是「同一份分享的另一条快照」(同一 URL 的另一种形态),命中几乎必然落在最近几条;封顶后空壳 miss 的最坏成本是常数次 JSON 解析,不随 shares 总量线性增长(当前生产 9 行,上限是数百倍冗余)。**判别力已验证**(行为级回退,只回退逻辑分支保留常量定义):回退 banned 保护 → `TestCheckKeepsBannedOnValidCredential` 失败;回退扫描上限 → `TestHealScanLimitBoundsSiblingSearch` 失败(窗口外老快照被自愈消费)。测试:`go vet ./...` 干净,全量 13 包绿。
- **v2.11(2026-11-12)**:深度审计修复轮(两轮独立审计交叉 + 生产实证,共 12 项根因修复)。**P0 ① 下载入口 fid 路径段编码**:mangodrive 的 path 形态 fid 就是存储路径(含 `/`),而 chi 的 `{fid}` 参数不跨段 → 签发端把 fid 原样拼进路径,这类文件的 `/d`、`/stream`、`/api/v1/json`、aria2 中转 URL **必然 404**(生产实证:mangodrive welcome.txt 提链 200、其 download_url 请求 404,同一响应里 quark fid 正常)。修复:新增 `service/dlurl.go`(`FIDPathSeg`/`FIDFromPathSeg`/`EntryURL`),四个签发点统一编码;签名 payload 恒为**原始 fid**,验签端先解回原始形再验签;fid 无保留字符时编码恒等(夸克/蓝奏云零影响)。**P1 ② 占位元数据遮蔽真实元数据**(与 v2.9/v2.10 同型第三例):「兜底名『文件』+0」落库后,`fileMeta` 只判 `FileName != ""` 即命中 → 快照/自愈/历史里的真实名字/大小被**永久遮蔽**(生产实证:池文件 resolve 返回 `file_name=文件, size=0`)。修复三层:`repo.UpsertLink` 占位元数据不覆盖已有真实值(`LinkMetaIsPlaceholder` 单一口径);`fileMeta`/缓存命中/历史回退全部跳过占位行;driver 提链途中带回的元数据(`DirectLink.Name/Size`,mangodrive fs/get 本就返回)优先采用 —— 根 URL + 深路径 fid 场景下这是唯一来源。**P1 ③ 账号健康检查解除风控冷却**:`checkAcc` 凭据有效即写回 ok,一次探测就复活被风控的账号;修复:cooling 账号校验成功保持 cooling(复位只经冷却到期),失效→expired 仍放行。**P1 ④ MarkStatus/MarkCooldown 整行回写**:并发下用过期快照冲掉 cooldown_until/cred_version;改 `MarkAccountStatus`/`MarkAccountCooldown` 定向列更新。**P1 ⑤ aria2 空响应 nil 解引用**:网关截断/aria2 重启时 `result` 缺失 → panic(500 丢真实原因);显式报「响应缺少 result 字段」,空 gid 拒绝落库,状态回写只更新非空字段且 `UpdateDownload` 改定向列(原 `Save` 整行覆盖,空串会抹掉已有失败原因)。**P1 ⑥ mangodrive 上传 200+空 body 判成功**:OpenList fs/put 正常必回 JSON,空 body(代理截断)时 `resp.Code==0` 被当成功 → 转存报 done 而远端无文件(静默丢文件);显式报「响应为空」。**P1 ⑦ 转存源选择不跳过提链失败文件**:目录转存取第一个非目录文件,若它 LinkError 非空则整次转存原样复现失败;改为跳过 LinkError 项,全失败时回退首个候选给出**真实**上游错误。**P1 ⑧ 账号 id 解析错误被吞**:`ParseUint` 错误丢弃 → id=0 继续执行并写误导性审计;改报 400(KindUnsupported)。**P1 ⑨ mangodrive 无基址发无 scheme 请求**:public_base/internal_base 均未配置时显式报「未配置 API 基址」(提链与 CheckCredential 两处)。**P2 ⑩ buildRef 的 ext 有效性只看 `len(map)>0`**:历史坏行 `{"api":""}`(mangodrive 恒空 api 键)以「有键无值」冒充有效上下文,把快照/自愈/历史恢复链全部短路 → 哈希 fid 提链 404;改按「至少一个非空值」判定(`extHasValue`),快照候选节点与自愈来源同口径。**P2 ⑪ 快照自愈取最旧命中**:按 id 升序扫,最新快照的 stoken/path 反而拿不到;改逆序消费(id 降序 = 最新优先)。**P2 ⑫ 转存空响应/0 字节三连**:stream 通道 `size==0` 跳过字节数校验 → 200 空 body 落 0 字节产物标 done(现以源登记大小为独立证据,登记非零却实收 0 必报错);offline 产物确认要求非空 → 源本身 0 字节的合法产物永远确认不了、超窗误报 failed(expect==0 改以 mtime 新鲜度判定,expect>0 仍严格);quark `findTmpDir` 失败冒充 mkdir 错误(`return "", ferr`)。**判别力已验证**:整体回退(仅保留测试)→ `TestE2EDownloadSlashFidFullPathEscaped`、`TestE2EAccountInvalidIDRejected`、`TestTmpDirFindFailureReturnsFindError` 失败;`TestHealPrefersNewestSiblingSnapshot` 在实现前实跑失败(取到 OLD-STOKEN)。新增回归:`internal/api/download_fidpath_test.go`(斜杠 fid 全链路 302/stream/400 对照)、`internal/repo/repo_meta_guard_test.go`、`internal/service/resolver_meta_guard_test.go`、`internal/service/account_cooling_test.go`、`internal/service/transfer_guard_test.go`、`internal/driver/mangodrive/upload_guard_test.go`、`internal/driver/quark/p212_tmpdir_err_test.go`。审计同时确认一批「看似可疑、实际安全」的位点不改(`UpsertSyncItem` DELETE+INSERT 的 AUTOINCREMENT 单调性、`ListLinksByFID` 排序、`RawTreeHasContent`/`extHasContent` 口径、`nil→"[]"` 规范化、身份闸、`PickAccount` 单选、`relay` 死链失效等)。测试:`go vet ./...` 干净,全量 13 包绿(CI 另跑 `-race -shuffle=on`)。
- **v2.10(2026-11-12)**:P0 根因修复轮(`shares.raw_tree` 空壳快照 + 跨 share_key 读侧自愈)。① **根因**:与 v2.9 的 `links.ext` **同型**,但后果更重一层。`shares.raw_tree` 是 driver 节点列表的 JSON,而快照里带着 quark 的 `pwd_id`/`stoken`、lanzou 的 `share_url`/`pwd` —— 那是该分享**唯一**的上下文来源(quark 的 auto-link 配额只有 2,第 3 个及以后的文件只可能从快照拿上下文)。一次「没拿到节点」的解析(`json.Marshal(nil slice)` → 字面量 `"null"`;截断标记节点还会被 `resolver.go` 规范成 nil 切片)就把快照清成空壳 → 读侧 Unmarshal 得到 nil 节点列表 → 该分享下**所有**文件都提不了链(driver 报「缺少夸克分享上下文」),且**不可**从 links 历史恢复(与 ext 不同,快照没有第二来源)。生产库实证:11 行 shares 中 2 行是空壳(quark 行 raw_tree='null'、mangodrive 行 raw_tree='[]')。② **写侧修复三层**(纵深防御):`resolver.go ResolveShare` 把 nil 节点规范成 `"[]"`(「解析成功但没有节点」的诚实表达,不与「null/解析失败」混淆);`repo.UpsertShare` 增加写保护 —— `raw_tree` 无有效内容(`""`/`"null"`/`"[]"`)时**不更新该列**(保留原值),首次落库仍按原样写入;判定口径抽为 `repo.RawTreeHasContent` 供读侧复用,避免两处口径漂移。③ **读侧自愈**:本分享快照是空壳时,从同一网盘**别的 share_key** 快照里找回该 fid 的节点(名字/大小/上下文)。自愈来源成立的依据:`share_key = sha1(pan|url|pwd)`,URL 形态不同即不同 key —— 同一份夸克分享的「/s/xxx」与「/s/xxx#/list/share/yyy」是两条快照,生产实测同一份分享的两条快照分别完好(50/51 个节点)与损坏。只在同一 `pan_type` 内扫、只取「有内容」的快照行、命中即返回不合并多来源(夸克 `stoken` 与 `pwd_id` 必须同源,混用比不混用更危险)。④ **v2.10.1 补丁(生产实测暴露的污染风险)**:`fid` **不是全局唯一**的 —— 蓝奏云每个分享的单文件节点 fid 恒为 `"file"`(lanzou/driver.go:294),不同分享/不同目录的节点 fid 也可能撞名。若只按「同 pan_type + fid 匹配」挑兄弟快照,自愈会把**别的分享**的上下文注入本次请求 → 提链成功、直链却指向另一个分享的文件,用户毫不知情(**静默给错文件比提链失败危险得多**)。新增 `sameShareIdentity` 身份闸:硬门槛 host 必须相同;再按「由强到弱」判定 —— ① 分享 id 相同(quark 的 `/s/<id>` 与节点 `pwd_id` 同值,与 URL 形态无关);② `pwd_id` 相同;③ host+path 相同(根链 vs 深链);④ 路径**目录段**前缀(mangodrive,`/ab` 不是 `/a` 的前缀)。四条全不命中则**不自愈**(宁可不提链,也不给错文件)。⑤ **判别力已验证**:回退写保护 → `TestUpsertShareDoesNotClobberRawTreeWithNull/WithEmptyArray/WithEmpty` 失败;回退写侧规范化 → `TestResolveShareDoesNotPersistNullRawTree`、`TestResolveShareTruncatedDoesNotPersistNullRawTree` 失败;回退自愈 → `TestResolveFileHealsContextFromSiblingShareSnapshot` 失败;回退身份闸 → `TestResolveFileDoesNotHealFromDifferentShare`、`TestResolveFileDoesNotHealFromDifferentLanzouShare` 失败。⑥ **生产验证**:注入线上历史形态的坏快照(raw_tree='null')后提链 → HTTP 200、文件名/大小/`pwd_id`/`stoken` 全部从兄弟快照正确恢复;对照组(裸 fid 不在任何快照)仍正确 404「缺少夸克分享上下文」;注入**别的分享**的同 fid 节点当污染源 → 被身份闸正确拒绝(返回真实文件而非污染源文件)。⑦ **运维清理**:坏快照两行指向的上游资源均已实证失效(quark 分享 `404 share_gone`、mangodrive 路径 `404 object not found`),无法自愈 → 删除空壳快照行及其关联 links 行;清理前 `sqlite3 .backup` 落盘备份。测试:`go vet ./...` 干净,全量 13 包绿(CI 另跑 `-race -shuffle=on`)。
- **v2.9(2026-11-11)**:P2 根因修复轮(`links.ext` 数据降级 + 历史反查坏行)。① **根因**:`ResolveFile` 落库时 `extJSON, _ := json.Marshal(ext)`,而 `ext` 可能为 nil map(fileMeta 各来源都查不到上下文时)→ `json.Marshal(nil map)` 产出**字面量 "null"**;且 `UpsertLink` 无条件覆盖 `ext` 列 → 一次「没拿到上下文」的提链就把同 (share_key,fid) 行里**已有的真实 path 永久抹掉**。生产库实证:127 行 links 中 7 行 ext 为字面量 "null"(`/Pool-Secure/welcome.txt`、`out/m42-e2e.bin`、`_ui_test/.../__down`、`_prod_test/浅层文件4K.bin` 等),且同一 fid 普遍「带 path 的好行 + 无内容的坏行」并存(如 welcome.txt:share `731383e1…` 有 path、share `7822137b…` ext=null)。**危害链**:哈希 fid(`p:<sha1>`)不可逆,`Ext["path"]` 是唯一恢复源;坏行 Unmarshal 后是 nil map,历史反查若只取最新一行就在坏行上翻车 → 明明库里存在带 path 的可用行,裸哈希提链仍 404(恰是 v2.4/v2.8 所修链路的数据面漏洞)。② **写侧修复**:service 层空上下文落**空串**(不再产生 "null");repo `UpsertLink` 增加写保护——`ext` 无有效内容(`""`/`"null"`/`"{}"`)时**不更新该列**(保留原值),有内容才覆盖。③ **读侧修复**:`FindLinkByFID` 改为 `ListLinksByFID(fid, limit)`,返回按 `created_at DESC, id DESC` 排序的历史行列表(排序仍走 `idx_fid_created`、无 TEMP B-TREE,原 EXPLAIN 回归继续断言);service `recoverHistory` 按新→旧遍历,取第一条能解出非空 `path` 的行,**坏行不参与候选**;若所有行都无 path,退而取最新「有文件名」的行补展示元数据(不给 driver 供路径,与旧行为对齐)。④ **判别力已验证**:回退读侧(只取最新一行)→ `TestResolveFileHashFidSkipsBadHistoryRow` 失败(`Mango Drive 路径不存在:rawPath: p:…`);回退写侧保护 → `TestUpsertLinkDoesNotClobberExtWithNull/Empty` 失败(真实 path 被覆盖)。新增测试:repo `TestUpsertLinkDoesNotClobberExtWithNull/WithEmpty/StillUpdatesValidExt`、`TestExtHasContent`、`TestListLinksByFIDReturnsBadAndGoodRowsOrdered`;service `TestResolveFileHashFidSkipsBadHistoryRow`(独立 share_key 的坏行 + 更新时间,正是线上形态)、`TestResolveFileHashFidOnlyBadHistoryFails`(只有坏行必须失败)、`TestResolveFileDoesNotPersistNullExt`。⑤ **运维清理(随生产回归一并完成)**:不按文件名猜测,改按 OpenList `fs/get` 实际存在性逐行探测 127 行 links 的引用路径 → 悬空 118 行删除;shares 快照根路径探测 → 悬空 15 行删除;9 行 live links 与 11 行 live shares 保留;清理前 `sqlite3 .backup`(含 WAL)落盘备份。测试:`go vet ./...` 干净,全量 13 包绿(CI 另跑 `-race -shuffle=on`)。
- **v2.8(2026-11-11)**:P1 根因修复轮(哈希 fid 全局反查的查询计划)。① **P1 根因**:`FindLinkByFID` 用 GORM `First()` 排序时,ORM 会强制追加主键 tiebreaker `ORDER BY created_at DESC, id`(**id 升序**),与索引 `(fid, created_at)` 的**降序扫描方向相反** → SQLite 必须 `USE TEMP B-TREE FOR RIGHT PART OF ORDER BY`。上一轮(v2.6)加复合索引时误判为「缺列」,实测证明:SQLite 索引隐含 rowid,`id` 列本就覆盖,加第三列无效——真正的变量是 tiebreaker **方向**。修复:`First()` 改 `Take()` + 显式 `Order("created_at DESC, id DESC")`(与索引同向)→ 临时 B 树彻底消除(生产 EXPLAIN 实证)。② **回归护栏** `internal/repo/repo_fid_index_test.go`:用 GORM DryRun 会话捕获**真实发出的 SQL**(手抄近似 SQL 无法约束 ORM 是否偷偷追加 tiebreaker,正是上一轮漏网之处),在**只含 2 列索引**的独立库上 EXPLAIN 断言走 `idx_fid_created` 且无 TEMP B-TREE;并断言取数语义(created_at 最大者优先、并列取 id 最大者、空/不存在 fid 返回 (nil,nil))。已验证判别力:回退 `Order("created_at DESC")` 或改回 `First()` 均失败。③ **P2** `destOwnedByOther` 重写:原实现把 `queued`/`failed` 任务的 destPath 也计入跳过集,而这两类任务的产物根本不存在 → 别处的真实产物被误挡、令其对账失败、超窗后被误判为失败;现只认 `done`/`pending`/`running`(产物可能已落池)。同时移除死代码 `type rec struct{ dest string }`。锁序说明就地更正:全仓统一 `s.mu → task.mu`,**严禁反向获取**(注释原写成「先收集再出锁」,但收集本身就要持 `t.mu`,与实际锁序矛盾,易误导后来者)。④ **P2 目录翻页静默截断**:mangodrive `API.List` 的终止条件原为 `page*listPageSize >= out.Total`,但部分 OpenList 形态**不返回 total**(缺省 → 0),此时 `200 >= 0` 恒真 → 恰好取满一页(200 条)就 break → 目录超 200 条时后面的文件对客户端完全不可见且**无任何报错**(表现为「文件夹里少了文件」)。现 `total` 仅在 > 0 时作为终止判据,total 缺失时靠「本页不满 per_page」判断到底。回归 `TestListDoesNotTruncateWhenTotalMissing`(上游 250 条且 total 缺省,必须翻到第 2 页取满 250 条;回退即失败,已验证判别力)+ `TestListStopsWhenTotalReached`(total 正常时不多翻页)。⑤ **P2 `limit_qps` 取整方向**:`GET /api/v1/storage` 的 `limit_qps` 原用 `int(x)` 截断**向下**,配置 0.5 → 下发 0 → 客户端视为「未配置」→ 用默认并发 3,而服务端实际只允许 0.5 QPS → **客户端并发远超服务端限速**,表现为批量提链被限流拒绝、大文件夹反复失败。现改为 `math.Ceil` 向上取整且保证 ≥ 1,保证「下发值 >= 实际限速」,客户端只会更保守。回归 `TestStorageInfoLimitQPSRoundsUp`(2.5→3 / 0.5→1 / 0.01→1;回退即失败,已验证判别力)。⑥ 三个改动文件经 gofmt 规范化(`gofmt -l` 的其余噪音是全仓既有的 CRLF 差异,非本次引入)。测试:`go vet ./...` 干净,全量 13 包绿。
- **v2.7(2026-11-11)**:深度评审修复轮 + 持续迭代。① **P2 修复**:offline 产物对账认领守卫——同目录并发转存(claimDest 只互斥同路径)下,`reconcileOfflineArtifact` 的启发式候选(非空/新鲜/size≥expect)可能命中其他任务的产物造成快照误归因;新增 `destOwnedByOther`(认领中的路径 + 其他任务已记录的 destPath 都跳过,调用方自身 destPath 不算"他人"以保同路径重试),回归 `TestOfflineReconcileSkipsOtherTasksArtifacts`(禁用守卫即失败,已验证判别力)。② **配额语义回归锁定**:12 文件目录三段断言(前 8 裸哈希反查成功/第 9+ 反查必失败/第 9+ 带目录成功),锁死 `defaultAutoLinkMax` 与「客户端恒携带目录」契约的交互(v1.22 客户端 P1 的盲区来源)。③ **迭代**:远端失败查询按 3 tick 节流(每 tick 全量拉 undone 对同目录多任务/长窗口是上游放大;失败检测延迟 ≤3×poll,窗口小时级不敏感);`GET /api/v1/storage` 新增 `limit_qps` 下发(mangodrive 驱动限速取整,缺失/0=未配置,客户端据此自适应批量提链并发度,附加字段向后兼容)。测试:全量 13 包绿。
- **v2.6(2026-11-11)**:持续迭代优化轮。① `links.fid` 反查索引升级为复合索引 `idx_fid_created(fid, created_at)`——上一轮的单列索引免了全表扫但 ORDER BY created_at 仍走 TEMP B-TREE(EXPLAIN 实证),复合索引让过滤+排序全走索引;旧孤儿索引 `idx_links_f_id` 部署后手工 DROP。② mangodrive 目录翻页上界 `maxListPages` 20→50(单目录 1 万条;生产 250 文件双页实测正常),防异常响应拖死解析的初衷不变。契约零变化;全量测试绿。
- **v2.5(2026-11-11)**:深度评审修复轮(哈希 fid 历史反查机制加固,契约零变化)。① `links.fid` 补单列索引——原只有 `(share_key, fid)` 复合唯一索引,fid 非前导列,`FindLinkByFID`(不带 share_key 的全局反查)走全表扫描,AutoMigrate 启动自动补建;② `buildRef`/`fileMeta` 历史反查的 DB 故障改为向上传播(原 `herr == nil &&` 静默吞掉 → 裸哈希给 driver → 误导性「路径不存在」404/「文件」+0),与同函数 GetLink/GetShare 的错误契约对齐;③ 历史反查加 `p:` 前缀门控(`isHashFid`,引用 `mangodrive.FidHashPrefix`)——仅哈希形态 fid 反查:path 形态未命中由 driverStat 兜底,省一次全局扫,并杜绝其它驱动形态 fid 撞名注入无关 Ext;④ `fileMeta` 残缺历史行(FileName 空)不算命中,落到 driverStat 兜底。测试 +2:`TestResolveFileHashFidDBErrorPropagates`(DB 故障不得降级驱动 404)、`TestResolveFilePathFidSkipsHistoryLookup`(门控回归),全量 13 包绿。
- **v2.4(2026-11-10)**:长路径(哈希 fid)直链提链根因修复——mangodrive 对超 100 字节路径生成不可逆哈希 fid(`p:<sha1>`),桌面端 `parentDirOf` 推不出父目录时只能以任意 url(如根目录)+ 裸 fid 提链,`buildRef` 的本分享缓存/快照都查不到 path,driver 把哈希当存储路径去 OpenList Get → 404。修复:`links` 表新增 `FindLinkByFID`(跨 share_key 按 fid 取最近一条历史行),`buildRef`/`fileMeta` 在本分享查不到时从历史行 Ext["path"]/元数据恢复。证据:生产 `_ui_test` 长中文目录实测,`url=<根目录>&fid=p:a5d8…` 修复前 404(`storage not found; rawPath: /p:a5d8…`);测试:`resolver_hashfid_test.go`(哈希 fid 历史反查成功 + 无历史时正确失败)
- **v2.3(2026-11-10)**:offline 通道状态机根因修复——「受理即 done」改为 `pending → done`(真实落盘 size)/ `failed`(确认窗口超时,`upstream_error`)。证据:生产 34 条后台转存中 6 条 `conflict` 由同路径二次提交造成(首任务已乐观 done 但仍持写占用,最长占用近 20min);`activeDest` 语义不变(claim/release 归确认器 goroutine 的 defer)。兼容性:`pending` 期间桌面端须继续轮询(`isRunning` 已纳入,`isTerminal` 不含),文案「落池中」;`offline_confirm_after` 配负数回退「受理即 done」并打 Warn。新增 §7.7 状态机与守卫表；确认器立即首查 + 默认轮询 10s（与 `d14f27a` 合并）;测试:`TestDestConflictOfflineHoldsUntilConfirm`(pending 期间拒收 → 确认真实 size/300 → 放行)、`TestDestConflictOfflineTimeoutReleases`(超时 failed + 路径释放)、`TestOfflineSnapshotUsesLandedName` / `TestDirectOfflineTransfer` / `TestTransferAutoChannelSelection` 由 `waitDone` 改为 `waitState(..., "pending")`。
- **v2.2(2026-11-10)**:M4 转存链路三轮生产迭代收口(契约权威源=桌面仓 INTEGRATION-PANROUTER.md v1.15,本页记服务端内部口径):① max_bytes 硬上限全覆盖(offline 提交前校验/stream 预检+途中实收截断)+ prune 竞态修复(`5914f89`);② auto 选中的 offline 提交失败自动回退 stream,显式 offline 原样报错(`3aac3ee`);③ 直链直传 `fid=direct`(受理前探测、SSRF 双保险、跨域公网 302 放行,`a346df8`);④ 同目标并发写守卫(destPath 互斥,offline 占用维持到落池确认/窗口超时,新增 `conflict` kind)+ offline 落池确认器(`36afa15`;v2.3 起改为收口器);⑤ 生产实测:公网文件矩阵 4/4 sha256 MATCH(含 1GiB stream)、守卫 GUARD/RELEASE 双 OK
- **v2.1(2026-11-04)**:新增设备历史同步端点 `POST|GET /api/v1/sync`(M3,§7.6)+ `sync_items` 表(§9)+ 管理页「设备同步数据」浏览区块;payload 不透明存储,服务端只做 LWW 仲裁与墓碑传播,两仓零共享代码边界不破
- **v2.0(2026-10-05)**:以实际实现为基准重写,取代《方案设计》v1.1.x(已删除);现状/已知问题/契约/路线图分开表述;新增 §15 深度评审结论与分阶段重构计划;代码注释不再引用章节号
