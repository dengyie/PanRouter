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
- 成功:透传 Content-Type/Length/Range/Accept-Ranges/ETag/Last-Modified,32KB 流式写 + flush;客户端断开视为正常取消

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
