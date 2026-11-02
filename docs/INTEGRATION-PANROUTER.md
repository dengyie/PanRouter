# YunX-Desktop × PanRouter 深度整合开发规格

> 状态：v1.0（2026-11-01，调研定稿）
> 本文是「云析桌面版二次开发 + PanRouter 深度整合」的工程规格（Engineering Spec）。
> 生产运维与拓扑以 Obsidian《PanRouter 部署与运维》为准；PanRouter 服务端契约以仓内
> `panrouter/docs/development.md` v2.0 为准。三份文档冲突时以各自领域的最新 canonical 为准并回写对齐。

---

## 1. 背景与定位

### 1.1 两个项目是什么

| | YunX-Desktop（云析桌面版） | PanRouter（网盘直链聚合） |
|---|---|---|
| 形态 | Windows 桌面客户端（Kotlin + Compose Multiplatform，单机） | Go 单二进制 Web 服务（自部署，pxed `127.0.0.1:6400`，公网 `pan.example.com`） |
| 核心能力 | 10 家网盘分享解析 + 视频解析 + Range 分片高速下载 + P2P + 多账号管理（JCEF 登录/Cookie 导入/加密备份） | 分享解析 → 直链缓存 → 按 **302 透传 / aria2 直下 / `/stream` 中转** 自动择路；账号池（Cookie 加密落库、扫码登录）；限频/熔断/singleflight |
| 进度 | v1.3.3 成熟（上游 tidain/YunX-Desktop，AGPL-3.0） | M1 已上生产（夸克/蓝奏云全链路；123/UC/139/189 在 M2 路线） |
| 协议 | **AGPL-3.0** | **Apache-2.0**（明确不链接 GPL/AGPL 代码） |

### 1.2 为什么整合（互补性结论）

两者是同一赛道的**互补形态**，整合后 1+1>2 的点全部来自「对方的独有能力」：

1. **服务端解析 IP 固定**：取链写操作（夸克转存等）发生在 pxed 固定出口，家庭宽带 IP 不暴露给网盘风控面；直链下载流量仍走本地宽带（302）或服务端中转（cloud 画像 BindIP 时）。
2. **直链缓存与账号池共享**：服务端解析结果落 `links` 表（TTL 内复用），桌面端免重复转存；服务端账号池可被多设备复用。
3. **桌面端下载引擎是服务端不具备的**：PanRouter 的下载路径只有浏览器 302 / aria2 / 32KB 串流中转；YunX 的 `ChunkDownloader`（Range 分片并发 + 断点续传 + 分片 SHA-256 + 令牌桶限速）可以直接吃 PanRouter 的直链/流 URL，体验全面优于网页端。
4. **桌面端能带自定义请求头**：浏览器 302 无法附加 UA/Referer/Cookie，所以 PanRouter 的 `Can302` 条件苛刻；桌面端 OkHttp 可带任意头，**凡非 BindIP 的直链桌面端都能直连分片下载**，不必降级 `/stream`。
5. **多账号库双向流动**：桌面端已有成熟的多账号加密存储（AES-256-GCM + PBKDF2/DPAPI 密钥包裹 + `.yunx` 备份），可作为服务端账号池的「冷启动供给方」。

### 1.3 目标与非目标

目标：
- YunX-Desktop fork 成为 **PanRouter 的官方 Windows 桌面客户端**：解析优先走服务端（pan.example.com），结果直接进本地分片下载引擎。
- 服务端未覆盖的平台/网络不可达时，**无缝落回本地原生解析链**（零感知降级）。
- 桌面端多账号可选择性贡献给服务端账号池（M2）。

非目标：
- 不改 PanRouter「单管理员自用」定位（不做多租户）。
- 不破解限速、不绕过会员权限、仅解析用户主动提供的分享链接（两项目既有合规底线不变）。
- 不做 iOS/macOS/Linux 端（YunX-Desktop 上游为 Windows 专用栈）。

---

## 2. 合规与许可证红线（先于一切设计）

| # | 红线 | 含义 |
|---|---|---|
| R1 | **AGPL 传染隔离** | YunX-Desktop 是 AGPL-3.0，PanRouter 刻意「自研实现、不复制 GPL/AGPL 代码」（`panrouter/docs/development.md` §18）。整合必须保持**进程/网络边界**：两仓零共享代码、零共享构建产物。Kotlin 代码永不进 panrouter 仓，Go 代码永不进 YunX fork。接口契约以**文档（本文 §5）**定义，不共享协议生成代码。跨进程 HTTP 通信的独立程序属聚合（aggregate）而非衍生作品，各自 License 不变。 |
| R2 | **fork 义务** | 本 fork 继续以 AGPL-3.0 开源（或私有自用不分发——AGPL 网络服务条款仅约束「向网络用户提供」的分发形态；自用不分发不触发源码公开义务，但一旦分发必须开放）。保留上游版权与 LICENSE，README 注明 fork 来源。 |
| R3 | **风控自担与频控** | 服务端解析已有 per-pan 令牌桶 + 熔断 + 游客分键；桌面端**不得绕过**（不做本地并发打点），服务端解析默认低频（见 §8.4）。 |
| R4 | **凭据安全** | api_token / 管理员口令在桌面端使用既有 `credential.key` 加密体系（对齐 `GitHubTokenStore` 模式：AES-256-GCM、不写日志、401 自动清除提示）；贡献给服务端的 Cookie 走 HTTPS + 服务端 AES-GCM 落库（已有）。 |

---

## 3. 总体架构

### 3.1 数据流

```
┌────────────────────────── Windows 桌面端（YunX fork，AGPL）──────────────────────────┐
│                                                                                      │
│  粘贴链接/剪贴板/浏览器插件 ──▶ ShareLinkParser.parse() ──▶ ParsedShare(platform, …)   │
│                                                        │                             │
│                                                        ▼                             │
│                                              ┌──────────────────┐                    │
│                                              │ ResolveRouter    │  纯函数，O(1) 表驱动 │
│                                              │ (新增)           │                    │
│                                              └───┬──────────┬───┘                    │
│                            服务端覆盖&可达        │          │  未覆盖/不可达/服务端失败 │
│                                                  ▼          ▼                        │
│                                  ┌──────────────────┐  ┌──────────────────────────┐  │
│                                  │ PanRouterGateway │  │ 本地原生链                │  │
│                                  │ (新增, OkHttp)   │  │ <Pan>ResolveRepository   │  │
│                                  │ POST /resolve    │  │ (Quark/UC/百度/115/…)    │  │
│                                  └────────┬─────────┘  └────────────┬─────────────┘  │
│                                           └──────────┬──────────────┘               │
│                                                      ▼  统一收敛为 DownloadItem      │
│                                              ┌──────────────────┐                    │
│                                              │ DownloadManager  │ 分片并发/续传/SHA-256│
│                                              │ ChunkDownloader  │（直链带 UA/Referer/ │
│                                              └────────┬─────────┘  Cookie 自定义头） │
└───────────────────────────────────────────────────────┼──────────────────────────────┘
                                                        │ HTTPS（OkHttp 直连，无 CORS 约束）
                                                        ▼
                              PanRouter（Go 单二进制，Apache-2.0，pxed）
                              POST /api/v1/resolve(/batch) · GET /d · GET /stream
                              · /api/v1/accounts*（M2 账号贡献）· /healthz
```

### 3.2 解析择路策略（ResolveRouter）

纯函数决策表（输入：`SharePlatform` × 服务端状态 × 用户开关；输出：路径）：

| 条件 | 路径 | 说明 |
|---|---|---|
| PanRouter 开关关闭 | LOCAL | 用户显式禁用 |
| platform ∉ 服务端 driver 覆盖集（当前 quark/lanzou；后续 123/UC/139/189） | LOCAL | 服务端不认识该盘 |
| 服务端不可达（健康状态为 DOWN，§8.4 退避中） | LOCAL（后台探测恢复后自动切回） | 零感知降级 |
| 服务端覆盖且可达，游客盘（蓝奏一类） | REMOTE | 服务端游客解析（`pan:guest` 分键限频），结果带直链/流 URL |
| 服务端覆盖且可达，登录盘（夸克一类） | REMOTE | 服务端 Pick 账号池 Cookie 执行转存链；`auth_expired` 时提示账号池凭据失效 |
| REMOTE 失败（非凭据类错误）且用户未关闭「失败回落本地」 | REMOTE→LOCAL 一次 | 每次解析至多回落一次，防抖 |

决策依据的「服务端覆盖集」与「健康状态」为桌面端内存态（`AtomicReference`），来自 `/healthz` 探测 + 解析结果自学习（解析 400 `kind=unsupported` 即把该 platform 标记为未覆盖，会话内缓存）。

### 3.3 下载择路（对 PanRouter 结果的二次决策）

PanRouter `resolve` 返回的每个文件带 `download_url`（302 或降级 `/stream`）与 `stream_url`、`need_headers`、`ua`/`referer`/`cookie` 明文字段。桌面端侧决策：

| PanRouter 侧事实 | 桌面端下载路径 |
|---|---|
| 直链非 BindIP（`Can302` 或 aria2 可直下类） | **本地 `ChunkDownloader` 直连直链**，请求头按 `need_headers`/`ua`/`referer`/`cookie` 注入 —— 分片并发 + 断点续传全部生效（桌面端独有优势） |
| 直链 BindIP（cloud 画像） | **本地引擎下 `stream_url`**：`/stream` 透传 Range（PanRouter §7.4），分片并发对中转同样有效；带宽瓶颈在 pxed 上行，线程数自动降档（见 §8.5） |
| 用户偏好 aria2 | `POST /api/v1/downloads` 推送（登录态），桌面端展示任务状态（轮询 `GET /downloads/{gid}`） |

> 关键洞察：**`/stream` 支持 Range 透传**意味着「服务端中转」不再等于「单线程慢速」——桌面端可以对同一 stream URL 开多连接分片，这是网页/aria2 路径做不到的组合。

---

## 4. 桌面端模块设计

### 4.1 新包落点

```
desktop/src/main/kotlin/com/yunx/app/
├─ data/panrouter/                    ★ 新增包（独立，不侵入既有 network/ 语义）
│  ├─ PanRouterSettings.kt            # base_url / api_token / 开关 / 策略项；prefs 持久化
│  ├─ PanRouterTokenStore.kt          # token 加密存取（复用 credential.key 体系，模式对齐 GitHubTokenStore）
│  ├─ PanRouterClient.kt              # OkHttp 网关：healthz / login / resolve / resolve-batch / accounts / downloads
│  ├─ PanRouterModels.kt              # 请求/响应 DTO + 错误体（code/kind/message/retriable）
│  ├─ PanRouterError.kt               # 错误模型 + kind → 本地用户文案映射（表驱动）
│  ├─ ResolveRouter.kt                # 纯函数决策器（§3.2 表驱动）
│  ├─ ServerHealth.kt                 # 健康状态机：AtomicReference + 指数退避探测
│  └─ PanRouterResolveGateway.kt      # 解析用例编排：Router 分流 → Client/本地链 → 统一 DownloadItem
└─ ui/screens/panrouter/              # 设置页 PanRouter 区块 + 解析结果来源标注（服务端/本地徽标）
```

### 4.2 明确不做的抽象（务实分层）

- **不实现 `ShareResolveRepository` 接口**：该接口契约是「客户端持 Cookie 的多步转存链」（`createSession/listFiles/transferFile/getDownloadLink` 全部携带 `cookie` 参数，见 `data/repository/ShareResolveRepository.kt`），而服务端解析是「服务端自持账号池的一步式 POST」。硬实现会造出空壳透传（cookie 参数无意义、`ensureTempDir` 等方法不可能成立）。改为独立 `PanRouterResolveGateway`，在**调用层**（解析页 ViewModel）做分流，两者输出统一收敛为 `DownloadItem`。
- **不新增本地 HTTP 服务**：M1 无反向控制需求（服务端不主动叫醒桌面端）；`BrowserBridgeServer`（127.0.0.1:37021-37030）保持浏览器插件专用，不复用不扩展。M3 若做任务回传再评估。
- **不引入新依赖**：OkHttp（已有 4.12.0）、kotlinx.serialization 或 org.json（已有）、Coroutines（已有）全部够用。

### 4.3 关键接线点（已核实）

| 接线点 | 位置 | 动作 |
|---|---|---|
| 链接识别 | `data/network/ShareLinkParser.kt`（`SharePlatform` 枚举 11 值） | 不改；`ParsedShare.platform` 即 Router 输入 |
| 分流入口 | 解析页 ViewModel（`ui/resolve` / `ui/viewmodel`） | 由「直调平台 Repository」改为「调 `PanRouterResolveGateway.resolve(parsed)`」 |
| 本地链 | `<Pan>ResolveRepository`（`data/repository/`） | 不改；作为 Router 的 LOCAL 分支被 Gateway 调用 |
| 下载入口 | `DownloadManager` / `DownloadSource` | 不改；Gateway 输出的直链/流 URL + 请求头装配为既有 `DownloadLink` 语义（URL/UA/Referer/Cookie） |
| 凭据存储 | `data/db/SecureAccountDaos` + `credential.key` | 不改；`PanRouterTokenStore` 对齐同一加密体系 |
| 设置 UI | `ui/screens` 设置区 + `data/prefs` | 新增 PanRouter 区块（开关/base_url/token/连接测试按钮/贡献账号入口） |

---

## 5. 服务端契约（复用 PanRouter 现有 API，零服务端改动起步）

### 5.1 M1 使用的端点（全部已上线，PanRouter development.md §5/§6/§7/§12.2）

| 端点 | 用途 | 认证 |
|---|---|---|
| `GET /healthz` → `{status, version}` | 健康探测（低频退避，§8.4） | 无 |
| `POST /api/v1/auth/login` `{username,password}` → JWT(7d) | 管理员登录（首次配置时） | — |
| `POST /api/v1/resolve` `{url, pwd?, fid?, ua?}` | 单链接解析（游客可调；登录后服务端可 Pick 账号） | optionalAuth |
| `POST /api/v1/resolve/batch`（≤50 项，单项 90s，部分成功语义） | 批量解析 | optionalAuth |
| `GET /d/{pan}/{key}/{fid}?sig=` | 直链下载（302 或自动降级 stream；HMAC 签名 TTL=`server.sign_ttl` 默认 72h） | 无（签名） |
| `GET /stream/{pan}/{key}/{fid}?sig=` | 中转流（透传 Range；UA/Referer/Cookie/白名单重定向服务端处理） | 无（签名） |
| `POST /api/v1/downloads` `{pan, share_key, fid, dest?}` | aria2 推送（M1 可选功能） | auth |

响应要点（契约锁定，前端依赖见 PanRouter §12.2）：
- resolve 返回 `files[]`（含 `file_name/size/fid`）与提链结果 `link_error`（单文件失败不整单失败，响应仍 200）；`truncated=true` 表示自动提链截断（夸克 2 / 其余 8 上限）。
- 下载字段：`download_url`（浏览器可直接打开）+ `stream_url` + `ua/referer/need_headers`；`need_headers=true` 时桌面端必须按字段注入请求头。
- 直链过期后重新 `resolve` 同 URL/fid 走 `GetFreshLink` 续命（singleflight 收敛），桌面端无需自建缓存失效逻辑，**收到 403/404/410 即整链接重解析**（服务端 `/stream` 上游 ≥400 时已自动 `ExpireLink` 死链自愈）。

### 5.2 错误契约与桌面端映射

服务端错误体 `{code, kind, message, retriable}`，九种 kind（HTTP 映射已由服务端契约测试锁定）。桌面端映射表（表驱动单测覆盖）：

| kind（HTTP） | 桌面端处理 |
|---|---|
| `session_expired`(401) | 透出「连接已失效，请到设置重新配置 api_token」；**不清本地 token**（服务端对「token 缺失/错误/会话过期」统一返回此 kind——输错一个字符也会清空，反而丢失设置页回显；保留原值供用户修正后重试） |
| `auth_invalid`(401) | 提示管理员口令错误（登录弹窗） |
| `auth_expired`(401) | **服务端账号池凭据失效**：提示并在用户已登录同平台时提供「用本地账号解析」一键回落 |
| `risk_control`(429) | 尊重退避：本次解析回落本地（若本地支持），并提示服务端该盘冷却中 |
| `share_gone`(404) / `not_found`(404) | 直接透出文案，不回落（本地同样会失败） |
| `interface_changed`(502) | 透出「服务端接口待适配」；不回落（回落本地可能仍可用，按 retriable 决定） |
| `upstream_error`(502) | `retriable=true` 时自动重试 1 次，仍失败回落本地 |
| `unsupported`(400) | 标记 platform 未覆盖（会话内），立即 LOCAL |

> 401 三分（session/auth_invalid/auth_expired）与服务端 P2-4 修复后的语义严格对齐（v1.5 修订）：**桌面端对 `session_expired` 不自动清本地 token**——服务端中间件（middleware.go auth/optionalAuth）对「缺 token、错 token、会话过期」一律返回 session_expired，自动清理会把「输错 api_token」误伤为「强制重新配置」，且丢失设置页回显；报错引导用户到设置页修正即可。上游 Cookie 失效（auth_expired）与桌面端 token 无关，不动。

### 5.3 M2/M3 拟新增端点（服务端小改造，另立 PR）

| 端点（拟） | 用途 | 服务端改动量 |
|---|---|---|
| 复用 `POST /api/v1/accounts` | 桌面端账号贡献（把本机某网盘 Cookie 举手里上传进服务端账号池；`POST /api/v1/accounts/quark/qr/poll` 已支持服务端扫码，桌面端改为「上传已有 Cookie」一条路） | **零改动**（端点已有） |
| `GET /api/v1/downloads` 列表过滤 `client=desktop` | 桌面端任务与服务端 aria2 任务区分展示 | 小（downloads 表加列 + 查询参数） |
| 设备事件上报（开放问题 §11-3） | 下载完成/失败回传统计 | 中（新表或 settings 复用），M3 再定 |

---

## 6. 服务端（panrouter 仓）配套事项

M1 目标是**零服务端改动**。需要留意（不在本仓做，登记到 PanRouter 侧）：

1. `api_token` 长期凭证正适合桌面端设备使用（常量时间比较、不过期）；管理员 JWT 7 天过期不适合设备端常驻 —— 桌面端默认用 **api_token**，登录弹窗只是辅助取 token 的引导（或直接让用户粘贴 api_token）。
2. cloud 画像下 BindIP 直链走 `/stream`，pxed 上行带宽成为多设备共享瓶颈 —— 桌面端对 stream 路径限并发（§8.5），避免独占。
3. M2 账号贡献放量后，服务端看门狗（30min 全量校验）的账号量增长 —— 已有 singleflight 互斥，预期无压力，观察即可。

---

## 7. 工程规约（六铁律落地）

1. **分层单向**：`ui → PanRouterResolveGateway → {PanRouterClient, 本地 ResolveRepository}`；`ResolveRouter/ServerHealth` 为纯领域层（零框架依赖，不碰 OkHttp/prefs，输入输出全是值类型）。Gateway 是唯一有副作用的编排点。
2. **复杂度预算**：Router 决策 O(1)（`EnumMap<SharePlatform, Capability>`）；错误映射 O(1)（`EnumMap<Kind, Handler>`）；解析 O(n)（n=服务端返回文件数，批量硬上限 50 对齐服务端）；健康探测并发收敛为单协程（`AtomicReference` + `Mutex` 保护退避状态，杜绝每次解析打 `/healthz`）。
3. **零占位符**：所有类型完备、分支闭环；禁止 TODO/伪代码进主干。
4. **错误链**：`PanRouterError` 显式携带 `kind/retriable/cause`，保留原始异常链（`RuntimeException(message, cause)`）；**禁止吞异常**；日志单点——Gateway 层记一次结构化事件（platform/路径/耗时/错误 kind），Client 层不打（对齐 YunX 诊断模式六模块分级）。
5. **无状态**：`PanRouterClient` 无请求态字段（OkHttpClient + token 读取函数注入）；`ServerHealth` 是唯一有状态单例，状态机迁移全部走单一协程。
6. **确定性**：`ResolveRouter` 决策不读时钟（时间相关输入由调用方传入 `now: Long`），单测 100% 可重现；退避计时注入 `CoroutineDispatcher` + 可注入 `delay`。
7. **大整数**：文件 `size`（可 >2^53? 否，网盘单文件远小于 2^53，但 >4GB 超 Int32）一律 `Long`，DTO 层禁用 `Int` 承载字节数；签名/时间戳用服务端返回原文透传，不重新计算。

---

## 8. 性能与频控细则

### 8.1 探测退避（ServerHealth 状态机）

`UP →(探测失败)→ DOWN(backoff)`；backoff 序列 30s → 1m → 2m → 5m → 10m（封顶）；探测成功立即 UP。**解析请求本身失败不计入 DOWN**（避免网络抖动误判），仅主动探测与 `ConnectException/UnknownHost` 类硬失败计入。UP 状态下解析不再前置探测（PanRouter 失败自会报错）。

### 8.2 批量解析

粘贴多链接（>1）走 `resolve/batch`（服务端单批上限 50，**客户端切片取 10/次**——服务端 batch 串行逐项经 limiter，单项真实时延秒级，50 项一片必然超出 120s callTimeout，10 项 ≈ 10-30s/片，切片间自然间隔）；单项 90s 预算内桌面端不重复设超时（服务端已兜底），HTTP 客户端总超时放宽为 120s/项 × 项数（封顶 10min）。

### 8.3 直链复用与重解析

同一 `share_key+fid` 在签名 TTL 内重复下载：桌面端可直接复用 `download_url/stream_url`（签名仍有效）——**不需要**本地缓存直链内容；仅当下载 403/410 时整链重解析。

### 8.4 服务端解析频次自律

- 单次解析会话内同 URL 幂等（用户重复点击 → 复用在途请求，`Mutex` per url）。
- 连续失败 3 次自动切 LOCAL 并提示（用户可手动重试 REMOTE）。
- 不做后台自动批量解析（永远由用户动作驱动）。

### 8.5 `/stream` 中转并发降档

pxed 上行有限且多设备共享：stream 路径线程数 = `min(用户设置, 4)`，分片 8MB（直链路径维持用户设置与既有 64KB 读缓冲默认）。该差异在下载任务元数据中标注来源（`route=relay`），调试页可见。

---

## 9. 里程碑与验收

### M1 —— 服务端解析接入 + 本地分片下载（对齐 PanRouter M1 能力）

- [ ] `data/panrouter/` 包 8 个文件全部落地（§4.1），`.\run.ps1 build` 零错误
- [ ] 设置页：开关 / base_url / api_token（加密存储）/ 「测试连接」（healthz + 版本展示）
- [ ] 解析页：夸克 + 蓝奏云链接 → REMOTE 路径 → 文件列表 → 单文件直链进 `DownloadManager` 本地分片下载成功（HTTP 206 多连接可验证）
- [ ] 断网 PanRouter → 自动 LOCAL 落回（蓝奏云本地原生链），恢复后自动切回
- [ ] 错误映射 9 kind 全覆盖（MockWebServer 回放），401 三分语义正确
- [ ] 单测：Router 决策表 / 错误映射表 / Client 契约（MockWebServer）/ Health 状态机；`gradle test` 全绿

### M2 —— 批量 + 账号贡献 + 全平台覆盖对齐

- [ ] 批量粘贴 → `resolve/batch`；`truncated` 与单项失败 UI 呈现
- [ ] 「贡献账号」：设置页选择本地已登录网盘账号 → `POST /api/v1/accounts`（二次确认 + 明示该 Cookie 将存于服务端）
- [ ] 服务端 M2 driver（123/UC/139/189）上线后桌面端自动获得（覆盖集自学习，无需发版）
- [ ] aria2 推送可选路径 + 任务状态轮询展示

### M3 —— 同步与协同

- [ ] 下载历史/收藏双向同步（依赖服务端设备事件端点定案）
- [ ] 浏览器插件嗅探的网盘分享链接可选交给服务端解析（插件协议不动，桌面端内部路由）
- [ ] fork 发版流水线：`version.txt` 单源 + `.\run.ps1 package/installer` + Release 分发（命名与上游区分方案见 §11-1）

---

## 10. 测试策略

- **契约测试**：`PanRouterClientTest` 用 OkHttp `MockWebServer` 回放 PanRouter 真实响应样本（样本文件对齐 PanRouter 仓 httptest fixture 风格，**手工誊抄协议字段、不复制其代码**，维持 R1 边界）。服务端契约变更（development.md §12.2）时先补样本再改代码。
- **决策表**：`ResolveRouterTest` 表驱动（platform × 服务端状态 × 开关 → 路径），覆盖 §3.2 全格。
- **状态机**：`ServerHealthTest` 退避序列 / 抖动不计 DOWN / 恢复即 UP。
- **错误映射**：9 kind + 未知 kind 兜底（按 `upstream_error` 处理并打点）。
- **不打真实网盘**：与 PanRouter 测试纪律一致，CI 无外网网盘依赖。
- **自检命令**：`.\run.ps1 build`（Kotlin 编译，无需 C++ 编译器）→ `gradlew :desktop:test`。发版前 `.\run.ps1 package` 冒烟（便携版产物可启动、解析→下载全链）。

---

## 11. 风险与开放问题

| # | 项 | 处理 |
|---|---|---|
| 1 | **fork 命名与上游关系** | 建议 fork 改名（如 `YunX-Desktop-M` / 「云析 M 版」）避免与上游 release 混淆；AGPL 要求保留上游版权声明。最终命名待定。 |
| 2 | **上游同步策略** | 上游 tidain/YunX-Desktop 活跃（v1.3.x）；整合代码全部隔离在独立包（§4.1），rebase 上游冲突面最小。每季度对齐一次上游。 |
| 3 | **任务/历史回传的隐私边界** | 下载行为数据回传服务端=把使用记录写到 pxed。M3 定案前默认**不回传**，仅本地历史。 |
| 4 | **api_token 泄露面** | api_token 是长期不过期凭证：桌面端加密存储（R4）+ 服务端侧未来可加 token 轮换端点（登记 PanRouter 侧 backlog，非本仓范围）。 |
| 5 | **风控连带** | 服务端账号池被桌面端多设备放大调用——频控已有，但 M2 账号贡献上线后需观察夸克侧 risk_control 频次；超预期则桌面端对登录盘默认 LOCAL（用户可开）。 |
| 6 | **AGPL 边界执行** | 誊抄协议样本（§10）而非复制实现；两仓 CI 互不引用；本文档为唯一共享契约源，服务端契约变更必须同步本文 §5。 |

---

## 变更记录

- **v1.5（2026-11-02）**：**v0.1.0 生产级 Code Review 后的根因修复轮（8 项全修，发版 v0.1.1）**。① P1 远端会话残留：`startResolve` 入口统一 `resetRemoteSession()`（剪贴板/收藏入口会在 Detail 态直接开新解析，旧会话的 remoteShare/pendingRemoteLink/remoteBatchContext 残留会把本地解析误判为远端——无目录导航、转存禁用、startDownload 走已失效的 pendingRemoteLink）；`backToInput` 与批量空结果路径复用同一函数（后者原只清 remoteBatchLinks 不清 context）；② P2 批量超时预算失配：客户端切片 50→10（§8.2 更新；服务端 batch 串行逐项，50 项必超 120s callTimeout）；③ P2 INTERFACE_CHANGED 死路径：Client 加 `jsonBody()` 单点，2xx 非 JSON body（网关故障页/契约漂移）映射为 interface_changed（原先裸抛 JSONException，绕过 shouldFallbackToLocal，无友好文案也不回落本地），六处解析点全部收口 + 契约测试回归锁；④ P3 探活循环原子占位：probeJob check-then-act 改 `AtomicBoolean.compareAndSet`，并发硬失败不再拉起双循环，finally 补判消除「DOWN 无循环」窄窗口；⑤ P3 设置弹窗 remember 以 visible 为 key（取消后重开显示已保存值；旧测试结果不再跨会话残留）；⑥ P3 批量 shareUrl/历史语义：入队传所属分享 url（右键「复制分享链接」不再落整段粘贴文本）、历史记录存第一个分享链接、`enqueueRemote` 对空 effectiveUrl 显式拒绝（批量计失败数，单文件进 downloadError）；⑦ P3 规格修订（§5.2/决策注记）：session_expired **不自动清桌面端 token**——服务端中间件对缺/错 token 统一返回此 kind，自动清理误伤输错场景且丢失回显，报错引导设置页修正（代码行为本就更安全，文档对齐代码）；⑧ 顺手清理：defaultKindFor 冗余 else 分支、SESSION_EXPIRED 过时 KDoc。`:desktop:test` 106/106 全绿（+1 契约测试）。
- **v1.4（2026-11-01）**：**GUI 实机验收通过，M1/M2 全部闭环；M3 部分收口**。① GUI 端到端（截图实证）：应用拉起 → 设置页 PanRouter 区块渲染 → 弹窗「测试连接」真实回读生产版本 `v0d8ba6b630d3` → token AES-GCM 加密落盘 → 解析页死链走服务端路由（401 session_expired →「连接已失效」；token 修正后 404 share_gone →「分享已失效（服务端确认）」——REMOTE 专属文案，本地链不会出现）→ 401→修 token→404 行为变化证明加密存取链正确；② 过程揪出两个真缺陷并修复：`SendKeys` 自动输入丢字符 `1` 导致坏 token（自动化手段问题，非集成代码缺陷；错误链按设计工作）；`PanRouterResolveGatewayTest` 直写 `java.util.prefs` 注册表污染真实应用配置 → 改为快照/还原隔离（回归全绿）；③ 环境坑：`run.ps1` 的 `Find-JdkHome` 拾到本机 JDK 11（`:desktop:run` 报 UnsupportedClassVersionError）——用 `~/.gradle/jdks/eclipse_adoptium-17`（toolchain 自动下载）注入 `JAVA_HOME` 启动；④ M3-2（插件链接路由）确认由架构天然满足：剪贴板/插件 hook/解析页/历史全部汇聚 `startResolve` 单漏斗，服务端路由自动生效。剩余 M3：fork 推 GitHub + 发版流水线（待建仓授权）、历史同步（依赖服务端端点定案）；aria2 维持不做。
- **v1.3（2026-11-01）**：**M2-C 跨分享批量 + 三项验收通过，M1/M2 全部收口**。① `ShareLinkParser.parseAll`（多链接提取，文案级提取码兜底——单测抓出并修复「只传 URL 丢失提取码」缺陷）+ `Client.resolveBatch`（items 数组契约）+ `Gateway.resolveBatch` + 解析页多链接分支（≥2 条覆盖平台链接 → resolve/batch → 合并扁平列表 `[序号] 文件名`，合成 fid 路由回所属分享取链；集成关闭/服务端不可达静默或提示退回单链接流程）；② 验收 A：官方入口 `.\run.ps1 build` BUILD SUCCESSFUL（Gradle 8.13 发行包经腾讯镜像预置解决官方源卡死）；验收 B：对真实生产 pan.example.com 契约冒烟四项全过（healthz 版本回读 / api_token 认证账号池 / 死链结构化错误 kind / batch 部分成功语义），临时用例跑完即删、token 未入库；验收 C：`:desktop:test` 105/105 全绿。aria2 推送正式裁定**不做**（生产 aria2 sidecar 未部署，实现即死代码；服务端上线后按 §5.3 契约补齐）。
- **v1.2（2026-11-01）**：**M2 主体落地**（批量 + 账号贡献）。① 远端模式批量下载：解除 M1 守卫改真实现——多选逐文件带 fid 取直链（串行 + 150ms 间隔自律，§8.4）、统一 `enqueueRemote`（单文件弹窗与批量共用）、中断/失败/入队计数反馈；② 账号贡献：`PanRouterClient.listAccounts/createAccount`（服务端契约 `{pan_type,name,cookie}`，实时校验）+ Gateway 管理面透传（不参与解析健康判定）+ `PanRouterContribution` 本机枚举（SecureAccountDaos 透明解密，仅对话框生命周期驻留内存）+ 设置页贡献弹窗（「我已知晓」二次确认门控、服务端同名去重提示、Cookie 不回显不落日志）。`:desktop:test` 101/101 全绿（+3 账号契约）。
- **v1.1（2026-11-01）**：**M1 代码全部落地**。新增 `data/panrouter/` 包 9 文件（Models/Error/Settings/TokenStore/Client/Health/Router/Gateway/Hub）+ ResolveViewModel 远端分支（解析分流/单文件取链/远端入队含中转降档/转存与批量守卫/状态清理）+ MainScreen 装配 + 设置页 PanRouter 区块（FadeAlertDialog 弹窗：开关/地址/token 掩码输入/失败回落/测试连接）。测试 5 个类 29 例（决策表/错误映射/Client 契约 MockWebServer/Health 状态机/Gateway 自学习与探活恢复），`:desktop:test` 98/98 全绿。待办：真实 pan.example.com 联调验收（GUI 环境）。
- **v1.0（2026-11-01）**：调研定稿。基于 YunX-Desktop v1.3.3（README/FEATURES/BUILD/USAGE/源码结构）与 PanRouter development.md v2.0（生产 v1.1.12）完成能力对照、许可证边界、架构与择路策略、模块落点（接线点均已源码核实）、M1-M3 里程碑与测试策略。
