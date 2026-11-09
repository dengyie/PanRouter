# YunX-Desktop × PanRouter 深度整合开发规格

> 状态：v1.14（2026-11-10，直链直传 fid=direct + 公网测试文件转存矩阵全绿，服务端 `a346df8`）
> 本文是「云析桌面版二次开发 + PanRouter 深度整合」的工程规格（Engineering Spec）。
> 生产运维与拓扑以 Obsidian《PanRouter 部署与运维》为准；PanRouter 服务端契约以仓内
> `panrouter/docs/development.md` v2.1 为准。三份文档冲突时以各自领域的最新 canonical 为准并回写对齐。

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

### 5.3 M2/M3 端点（已落地，实测契约）

| 端点 | 用途 | 状态 |
|---|---|---|
| 复用 `POST /api/v1/accounts` | 桌面端账号贡献（把本机某网盘 Cookie 上传进服务端账号池；扫码走 `POST /api/v1/accounts/<platform>/qr/poll`，桌面端只做「上传已有 Cookie」一条路） | **已上线**（M2，端点服务端原有） |
| `POST /api/v1/sync` | 设备历史同步上行：`{device_id, items:[{kind, uid, deleted, updated_at, payload}]}` → `{accepted, skipped, server_time}`。单批 ≤500 条、单条 payload ≤16KB；`(kind,uid)` 唯一，LWW 按 `client_updated_at` 仲裁（较新者胜，相同后来者胜） | **已上线**（M3，服务端 v2.1 §7.6） |
| `GET /api/v1/sync?since=<seq>&limit=500` | 下行拉取：`{items, last_seq}`，按全局变更序号 `id` 升序返回 `id > since`；`since=0` 全量，`last_seq` 作下一页游标 | **已上线**（M3，同上） |
| `GET /api/v1/downloads` 列表过滤 `client=desktop` | 桌面端任务与服务端 aria2 任务区分展示 | **不做**（aria2 路径已裁定不做，需求消失） |

**同步协议要点（两仓唯一共享契约，实现见服务端 development.md §7.6 与桌面端 §9 M3）**：
- **payload 不透明**：服务端只存 JSON 字符串、不解析字段（R1 零共享代码边界的落点）。脱敏在桌面端完成——下载记录只传 `share_url / platform / file_name / total_size / status / error / created_at`，**不含本机保存路径与请求头**。
- **kind 白名单**：`bookmark | link_history | download_record`。
- **墓碑**：删除以 `deleted=true` 空 payload 传播；下载任务删除不传播（日志语义）。
- **游标**：服务端对生效变更做 DELETE+INSERT 换取新 rowid，故 `id` 严格单调，客户端游标只进不退。
- **默认关闭**：同步开关默认 false，用户显式开启才上行（落实 §11-3 隐私裁定）。

### 5.4 M4 端点（自建云盘 / OpenList 兼容端点，已上线）

| 端点 | 用途 | 状态 |
|---|---|---|
| `GET /api/v1/storage` | 端点信息下发（auth）：`{code, driver:"mangodrive", enabled, public_base, dest_dir, max_bytes}`；未接入时回 200 + `enabled=false`，桌面端隐藏入口并报 UNSUPPORTED，**不猜域名** | **已上线**（M4.1 服务端 `24acc8b`） |
| `POST /api/v1/transfer/mangodrive` | 云对云转存受理（auth）：`{source_url, pwd, fid, dest_dir, name, channel, ua, client?}` → **202** `{code, task, poll:"/api/v1/transfer/<id>"}`；校验失败同步 4xx（source_url 必填、仅 http(s)、channel 白名单、端点未配置 KindUnsupported）；`client` 为可选调用方标识（≤64 字节，控制字符剔除，仅观测用）；**`fid="direct"` = 直链直传（v1.14，见下）** | **已上线**（同上） |
| `GET /api/v1/transfer` / `GET /api/v1/transfer/{id}` | 任务列表 / 单任务快照。快照字段：`{id, state, channel, source_url, dest_path, file_name, size, done, error, hint, started_at, ended_at, client?}`；`state = queued\|running\|done\|failed` | **已上线**（同上） |

**M4 契约要点（客户端零部署信息的落点）**：
- **基址服务端下发**：桌面端 `MangoDriveClient` 先问 `GET /storage`（实例内缓存），`publicBaseUrl` 仅作服务端不可达时的手动覆盖；`enabled=false → UNSUPPORTED`。换一套 OpenList 端点只改服务端 `transfer` 配置。
- **channel 语义**：`stream`（服务端拉上游 → 流式 PUT，兼容任意提取链约束）/ `offline`（OpenList 侧 aria2 直拉，仅公开直链，需 UA/Referer/Cookie 或 BindIP 的源拒收）/ `auto`（默认；有约束或 BindIP 或 `size ≥ transfer.auto_stream_bytes` 走 stream，否则 offline）。
- **offline 可用性前提（v1.12）**：offline 依赖 OpenList 宿主侧的 aria2 RPC（`http://localhost:6800/jsonrpc`）——**未部署 aria2 的端点上 offline 必然失败**。`auto` 选中的 offline 提交失败时**自动回退 stream**（任务快照 `channel` 落最终实际通道 `stream`，warn 日志留 `offline unavailable, falling back to stream`），因此桌面端默认通道在无 aria2 部署上仍可用；**显式** `channel=offline` 不回退，失败原因原样下发（`hint=upstream_error`，error 含 aria2 RPC 拒绝原文），调用方需自行降级重试。**（v1.13 生产现状：Mango Drive 端点已部署 aria2 sidecar，offline/auto/stream 三通道真实可用；offline 产物名规则见下。）**
- **offline 落盘名（v1.13）**：OpenList 离线任务**不可重命名**——落盘文件名 = 直链 URL 末段路径（aria2 `determineFilename`：percent-decode，源带 Content-Disposition 时以其为准；路径型直链两者一致且等于源文件真实名）。请求里的 `name` **仅 stream 通道生效**（File-Path 头）；offline 任务快照的 `file_name`/`dest_path` 由服务端改写为真实产物名（`43a4075`），桌面端展示以快照为准。同源并发 offline 在目标目录同名覆盖（aria2 的 `.1` 冲突后缀只发生在单任务临时目录内，跨任务不生效）。
- **直链直传 `fid="direct"`（v1.14，服务端 `a346df8`）**：`source_url` 即任意**公网** http(s) 直链，不经网盘解析直接进转存管线，三通道全适用。受理前探测（HEAD，回退 `GET Range:0-0`）拿大小供通道预检，**探测失败即拒绝**（不受理再失败）；落盘名 = URL 末段 percent-decode（三通道统一，请求 `name` 可覆盖 stream 命名）。**SSRF 防护**：专用 direct 客户端 `AllowPrivate=false`（拨号级拦私网）+ 每跳重定向 DNS 级 `privateGuard`（双保险）；跨域公网 302 放行（`SuffixMatch` 支持 `"*"` 哨兵，github release → objects.githubusercontent.com 一类）；URL 携带 userinfo 拒绝。`transfer.direct_allow_private` 配置仅供测试 fixture，生产必须 false。**已知限制**：① 同目标同名并发写 = 最后写者胜（离线上传遇冲突会自动重试整任务；勿对同一 URL 同目录并发提交两通道）；② 个别源站对非浏览器 TLS 指纹敏感（实测 `nbg1-speed.hetzner.com` 对 Go 客户端直接 EOF，curl 可通）——表现为 `直链不可达`，换源即可。
- **快照幂等**：`done` 原子计数，二次快照（handler 内 Get→Snapshot）不回零；成功终态 `Done=Size=实际字节`。
- **转存上限**：单文件 `transfer.max_bytes`（0=默认 64GiB，负=不限）。offline 在提交远端前按源大小硬校验：超限拒绝（`Unsupported`，error 含上限字节数），**大小未知时显式 offline 亦拒绝**（远端无法回滚；auto 改走 stream 由实收字节兜底）；stream 既预检 `Content-Length` 也在**每条下载途中按实收字节截断**（chunked/无长度源无法绕过）。`GET /api/v1/storage` 对不限（负值）回报 `max_bytes=0`，桌面端按 `0 = 不限` 展示（v1.11 记为「展示口径差异」者即此，v1.12 定为契约）。
- **任务可观测**：任务内存态保留 `keep_tasks`(200)/`keep_task_after`(24h)；浏览器断开不中断已受理任务（独立 ctx）。
- **解析/浏览链**：`mangodrive` driver 把存储路径当分享（FID 即路径，≤100 字符直用、超长 `p:<sha1>`）；浏览 = 目录 URL + fid 定位文件（`url` 给目录、`fid` 给文件，直接给文件路径会 404——桌面端 `MangoDriveClient.link(dirPath, fid)` 契约）；目录分享批量转存由桌面端逐文件提交（可观测、可重试）。
- **桌面直连下载**：`DownloadPlatform.MANGO` 取 `direct_url`（OpenList 签名直链，只签路径 host 可换，支持 Range 206）走本地 ChunkDownloader，**不经 /stream 中转降档**；必须携带返回的 UA。
- **红线延伸**：桌面端零部署信息（域名/容量/目录全部服务端下发）；OpenList token 只在服务端（加密），桌面端只持 api_token。

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

- [x] `data/panrouter/` 包全部落地（§4.1，M3 后共 12 个文件），`.\run.ps1 build` 零错误
- [x] 设置页：开关 / base_url / api_token（加密存储）/ 「测试连接」（healthz + 版本展示）
- [x] 解析页：夸克 + 蓝奏云链接 → REMOTE 路径 → 文件列表 → 单文件直链进 `DownloadManager` 本地分片下载成功（HTTP 206 多连接可验证）
- [x] 断网 PanRouter → 自动 LOCAL 落回（蓝奏云本地原生链），恢复后自动切回
- [x] 错误映射 9 kind 全覆盖（MockWebServer 回放），401 三分语义正确
- [x] 单测：Router 决策表 / 错误映射表 / Client 契约（MockWebServer）/ Health 状态机；`gradle test` 全绿

### M2 —— 批量 + 账号贡献 + 全平台覆盖对齐

- [x] 批量粘贴 → `resolve/batch`；`truncated` 与单项失败 UI 呈现
- [x] 「贡献账号」：设置页选择本地已登录网盘账号 → `POST /api/v1/accounts`（二次确认 + 明示该 Cookie 将存于服务端）
- [ ] 服务端 M2 driver（123/UC/139/189）上线后桌面端自动获得（覆盖集自学习，无需发版）
      —— **服务端现状（2026-11-04）只有 quark / lanzou 两个 driver**，其余平台未落地；属服务端路线图项，桌面端侧无待办
- [x] aria2 推送：**裁定不做**（生产未部署 aria2 sidecar，实现即死代码；v1.3 决议，v1.6 确认）

### M3 —— 同步与协同

- [x] **下载历史/收藏双向同步**（端点已定案并上线，v0.1.5 起可用）：范围 = 收藏 + 解析历史 + 下载记录（脱敏）；
      拓扑 = 多设备双向（服务端 LWW + 墓碑）；默认关闭，设置页「历史同步」显式开启。
      实现：三业务表加 `syncUid` 跨设备稳定标识 + `sync_outbox` 变更队列（业务 DAO 在增删改同一写事务内记账，
      引擎推送时回读业务行组包 → 多次修改自然合并、DAO 层不必知道契约 JSON）；服务端侧见 §5.3 / development.md §7.6
- [x] 浏览器插件嗅探的网盘分享链接可选交给服务端解析（插件协议不动，桌面端内部路由）—— v1.4 确认由架构天然满足：
      剪贴板 / 插件 hook / 解析页 / 历史全部汇聚 `startResolve` 单漏斗
- [x] fork 发版流水线：`version.txt` 单源 + `.\run.ps1 package/installer` + Release 分发（命名与上游区分方案见 §11-1）
      —— 已建立并跑通 v0.1.0~v0.1.4。**CI 拓扑注意**：GitHub 仓 `dengyie/PanRouter` 的 `main` 是桌面端版本线、
      `server` 分支是 Go 服务端线，两线无共同祖先——服务端改动 push 到 `server`，桌面端改动 push 到 `main`

---

### M4 —— 自建云盘（OpenList 兼容端点）接入 ✅（2026-11-08 收口）

- **M4.1 服务端**：`mangodrive` driver（OpenList REST v3：列目录/提链/凭据校验）+ 云对云转存服务与三端点（§5.4）；内网/公网双基址（EasyTier 直连绕 CF 100MB 请求体上限，`swapBase` 仅同 host 换基址——签名只签路径）。
- **M4.2 桌面浏览**：网盘页「自建云盘」卡片 + 浏览页（列目录/面包屑/长按多选/文件夹递归入队，`DownloadPlatform.MANGO` 直连分块下载 + sha256 校验）。
- **M4.3 桌面转存**：分享页一键转存（服务端解析模式下逐文件提交）+ 转存记录页（3s 轮询、终结自停）。
- **验收**：三层线上 E2E + sha256 字节级比对全过（140000 / 5242880 / 1048576 B）；`MangoDriveClientTest` / `PanRouterTransferTest` / 两个真机 E2E（env 门控，无凭据自动跳过）。
- **红线**：桌面端零部署信息；存储解耦轮后端点信息全部服务端下发（`GET /storage`），`mangodrive` 只作协议类别名（品牌 → 类别）。
- **后续加固（2026-11-09 review 轮）**：MANGO 补入按平台线程设置列表（v0.1.11）；offline 通道远端任务状态不回传为已知限制（转存记录对 offline 显示「已受理」语义即可，客户端不轮询远端 aria2）。

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
| 3 | **任务/历史回传的隐私边界** | **M3 已定案（v1.6）**：同步**默认关闭**，用户显式开启才上行；payload 脱敏（下载记录不含本机保存路径与请求头，不含账号 Cookie/token）；数据只进用户自部署的 pxed，不经第三方。残余风险仍成立——开启即等于把使用记录写进自己的服务器，设置页文案已明示。 |
| 4 | **api_token 泄露面** | api_token 是长期不过期凭证：桌面端加密存储（R4）+ 服务端侧未来可加 token 轮换端点（登记 PanRouter 侧 backlog，非本仓范围）。 |
| 5 | **风控连带** | 服务端账号池被桌面端多设备放大调用——频控已有，但 M2 账号贡献上线后需观察夸克侧 risk_control 频次；超预期则桌面端对登录盘默认 LOCAL（用户可开）。 |
| 6 | **AGPL 边界执行** | 誊抄协议样本（§10）而非复制实现；两仓 CI 互不引用；本文档为唯一共享契约源，服务端契约变更必须同步本文 §5。 |

---

## 变更记录

- **v1.14（2026-11-10）**：**直链直传 `fid=direct` + 公网测试文件转存矩阵（服务端 `a346df8`，桌面端零改动）**。① 新能力：任意公网 http(s) 直链不经网盘解析直接进转存管线（受理前探测大小、SSRF 双保险、跨域公网 302 放行，详见 §5.4 直链直传节）。② **公网真实文件矩阵**（OVH/Cloudflare 源，全部字节级验证）：OVH `10Mb.dat` auto→offline **sha256 MATCH**；CF `__down` 50MB auto→offline（HEAD 有 Content-Length，探测结论修正预期）**sha256 MATCH**；OVH `1Gb.dat` **stream 通道 1GiB 完整搬运 done（size=done 字节精确）**；OVH `100Mb.dat` offline 落池（OVH 单连接限速致 aria2 下载 ~15 分钟，属源站行为）。③ 实测过程中撞出并登记「同目标同名并发写」语义（最后写者胜，离线任务冲突自动重试）与源站 TLS 指纹敏感（Hetzner nbg1 对 Go 客户端 EOF）两条已知限制。④ offline 受理即乐观 done 的已知限制维持（真实落池以产物核验为准）。
- **v1.13（2026-11-10）**：**offline 通道生产可用 + 真实落盘名契约（服务端 `43a4075`，桌面端零改动）**。① Mango Drive 端点部署 aria2 sidecar（GoogleDrive 仓 `62de330`：共享 openlist 网络命名空间、RPC 密钥仅主机 .env、temp/aria2 权限模型 65534:1001+setgid+UMASK 002）——显式 offline / auto / stream 三通道 74B 生产探针全 done、产物落池核验；auto 不再需要回退兜底（保留为韧性逻辑）。② 生产探针暴露快照谎报缺陷：OpenList 离线不可重命名，落盘名 = URL 末段（aria2 percent-decode / Content-Disposition 优先），请求 `name` 仅 stream 生效——服务端在 AddOffline 成功后把快照 `file_name`/`dest_path` 改写为真实产物名，桌面端转存记录从此指向真实文件。③ 已知限制更新：offline 远端任务状态不回传（乐观 done）依旧成立；新增「同源并发 offline 目标同名覆盖」。
- **v1.12（2026-11-09）**：**M4 转存契约与生产实测对齐（服务端 `3aac3ee`，桌面端零改动）**。对生产用同一 74B 源跑 stream/offline/auto 三通道探针，定位并根因修复：① **P1 auto 在无 aria2 部署上必失败**：OpenList 侧 `add_offline_download` 返回 500 `failed init offline download tool aria2: ... localhost:6800 connection refused`，而 auto 对小文件（默认 <8GiB）本就选 offline → 桌面端默认通道 100% 失败，stream 明明可用。修复：`auto` 选中的 offline 提交失败时 warn 并回退 stream，快照 `channel` 落**最终实际通道**；显式 offline 仍不回退（§5.4 offline 可用性前提）。② 转存上限语义补全并落规格（v1.11 已实现，本轮提供生产证据）：offline 提交远端前硬校验、大小未知显式拒收、stream 预检 + 途中实收字节截断。③ 任务请求/快照增加可选 `client` 观测字段。④ storage `max_bytes` 负值→0 定为契约（0=不限），标注 v1.11「展示口径差异」结项。⑤ 生产探针证据：stream 74B `done=74`、产物落盘；修复前 offline/auto 均 `hint=upstream_error` 且无产物。规格同步修正 offline 任务状态不回传仍为已知限制（显式 offline 成功时 `done=size` 为乐观值，客户端按「已受理」语义展示）。
- **v1.11（2026-11-09，已被 v1.12 覆盖部分契约）**：**M4 契约补规格（桌面端 v0.1.10/0.1.11）**。M4 三层（自建云盘接入）此前只在 vault 记录，规格缺位——本轮把 §5.4（storage/transfer 端点契约 + channel 语义 + 快照幂等 + 转存上限 + 解析浏览链 + 桌面直连下载 + 红线延伸）与 §9 M4 里程碑正式写入。随批：MANGO 补入按平台线程设置列表（v0.1.11，修复「下载线程数无法按平台调整、落全局默认」）；登记已知限制：offline 通道远端 aria2 任务状态不回传（转存记录对其为「已受理」语义），storage 端点 max_bytes 对 ≤0 回报默认值的展示口径差异。
- **v1.10（2026-11-05）**：**测试隔离根治 + 去重查找索引（桌面端 v0.1.9，服务端零改动）**。① 测试 JVM 数据目录整体重定向（`tasks.withType<Test>` 注入 `YUNX_DESKTOP_DATA_DIR`→`build/test-data`）：Log 门面/AppContext 等单例在测试中只写构建产物目录，不再污染真实用户目录（线上排障时曾把引擎测试的假错误「推送被拒」写进真实 `yunx-desktop.log` 干扰判读）。重定向**暴露并根治一颗潜伏缺陷**：引擎 `syncOnce` 就绪判定原经 `isSyncReady()` 直连真实 `PanRouterTokenStore`，其解密密钥在 `<dbDir>/credential.key`——引擎测试一直隐性依赖用户真实 token+密钥才通过；改为经 `tokenProvider` 注入缝判定（生产默认实现不变，测试可注入）。② 去重查找索引迁移：`idx_bookmark_link` / `idx_link_history_url`——重复归一 pull 每条远端变更都要按 link/url 查本地，首次大积压同步（数千条×全表扫）线性放大；真库测试同步断言索引存在。`:desktop:test` **172/172** 绿（+1 URL 真库用例）。
- **v1.9（2026-11-05）**：**重复实体归一（v1.7 遗留的「跨设备同 URL 重复」裁定项落地，桌面端 v0.1.8，服务端零改动）**。多设备在开启同步前各自解析/收藏同一链接 → 异 uid 两行 → 同步后各端重复。根因修复：`applyRemote` 落地前按 link/url 找齐本地同实体异 uid 行（新 DAO `listByLinkForSync`/`listByUrlForSync`，`ORDER BY createTime ASC, id ASC`），按「**createTime 最早者胜，并列取 uid 字典序小者**」的确定性规则裁决唯一赢家——每台设备独立算出同一赢家，全网收敛；败者走 `reconcileLose`：作废旧记忆 + 记待推送墓碑（时间戳取 `max(now, 对方+1)` 保证必赢服务端 LWW），双端收到墓碑删行后只剩赢家一行。只改拉取合并侧即可收敛全部场景（本机新增与远端撞车时，对端收到墓碑自行删行）。测试 +4（远端旧胜→本地墓碑上传 / 本地旧胜→远端 uid 墓碑且幂等重拉被记忆挡住 / 历史页同规则 / 真库排序口径），并修正 `not ready` 用例的游标顺序依赖（改为断言「不变」）；`:desktop:test` **171/171** 绿。服务端本轮零改动（纯客户端合并逻辑）。
- **v1.8（2026-11-05）**：**M3 Review 修复轮二批（final review 7 项全修，桌面端 v0.1.7 ↔ 服务端体上限+日志）**。① P2 生产 outbox SQL 零覆盖：新增 `SyncOutboxStoreTest` 6 例——`AppDatabase.open` 增内部测试入口（可传临时目录开独立真库，不触碰单例），真实 SQLite 驱动生产 DAO 验证同 uid 压缩（编辑/墓碑/记忆各只留最新）、版本记忆 5000 上限挤最老且待推送不动、discard 删意图不留记忆且不误伤已推送、900 uid 分块 IN 回读 + 空 uid 行提取 + 索引迁移、download_record 终态意图接线（insert 不记账 / FAILED+error / COMPLETED）、applyFromRemote 按 uid 幂等且不记账；② P2 设置页「立即同步」协程无兜底：整段 runCatching（DAO/prefs IO 异常裸抛会崩 UI 协程且 syncingNow 卡死），对齐同文件既有惯例；③ P2 回填每轮全表物化：三 DAO 加 `listWithEmptySyncUid()`（`WHERE syncUid=''` 走本轮所建索引），引擎 backfill 不再 observeAll 全量——空 uid 行只存在于老库迁移形态；④ P3 服务端 sync/accounts 请求体上限：`decodeBoundedBody` = `http.MaxBytesReader(10MB)` + `MaxBytesError→KindUnsupported(400)`（客户端毒丸语义可收敛），e2e `TestE2ESyncPushBodyCap` 锁定（11MB 拒 + 正常体不受影响）；⑤ P3 游标回退查询失败补 `Warnw` 日志（原静默吞掉，运维无痕）；⑥ 注释订正（chunked(400)）+ 清理未用 import；⑦ 明确不修：历史记录跨设备同 URL 重复（uid 即身份模型语义，需产品裁定）。`:desktop:test` **167/167** 绿（+7），服务端 `go vet` + 全量绿（+1 e2e）。**线上实测（CI 编译产物实机部署）**：服务端 healthz `9eda5b91992c` + 生产同步契约冒烟 8/8（401/LWW skip/pull 回读/墓碑/17KB payload 400/游标回落/**10MB 请求体 400**）；桌面端 CI 便携包（`AppVersionKt`=0.1.7）实机启动，真实旧库完成迁移（三表 syncUid + sync_outbox + 索引），**双向全通**——生产推送的测试收藏被应用拉取落地（游标 0→3）、运行中应用把注入的空 uid 行回填并推送上生产（change id=4，device=DESKTOP-VST7EIE-6018，payload 完整，积压清零）、服务端墓碑下发后应用本地删除（游标 4→6，记忆更新为墓碑版本）；全程引擎日志零错误。
- **v1.7（2026-11-05）**：**M3 历史同步生产级 Review 根因修复轮（8 项全修，桌面端 v0.1.6 ↔ 服务端 d6045da）**。① P1 payload 字节预算失控（单条超 16KB = 服务端整批 400，同一批每轮重推、队列永久卡死）：三组 payload 全部加字段级 CAP + `fittedForSync` 收敛（JSON 转义膨胀路径按 UTF-8 字节反复折半直至进预算）；`utf8Truncate` 重写为**正向推进**——尾部回退法会把边界处的完整多字节字符误判为截断（`"a中文"` 截 4 字节丢成 `"a"`）、纯 ASCII 场景少截一字节；② P1 服务端 4xx 明确拒绝的「毒丸批」（KindUnsupported+400）重推永远失败：`isRejectedByServer` 识别后 `discard` 整批丢弃换回推进能力（被拒 ≠ 已同步，不留版本记忆）；网络/服务端故障仍保留意图下轮重试；③ P1 outbox 无限增长且待推送意图会被硬上限挤掉（删除墓碑丢失 = 别设备复活已删数据）：同 `(kind,uid,pushed)` 就地压缩只留最新一条（时间戳并列删除赢），版本记忆另设 5000 条总量上限且**只清已推送**；④ P2 `applyRemote` 时序：坏 payload/空 link 在**写库前**拦截，本机行与版本记忆都不动；落地成功后才作废旧记忆并记录新版本（此前先删记忆，写库失败会让更旧的远端变更覆盖本机更新状态）；⑤ P2 拉取游标未推进防御：满页但游标不前进（服务端契约违约）即停，不死循环；⑥ P2 大库推送每批整表加载（O(N×批数)）→ `listBySyncUids` 分批回读（`sync_uid` 三表建索引，IN 分块 400）；⑦ P3 三项：下载失败 `errorMsg` 由 `updateError` 单独写入且不触发 outbox → 服务端失败记录永久缺错误信息，现补记意图（推送时回读整行自然带上）；服务端空页回报库内真实最大序号（库从备份回档后客户端游标超前会永久脱离同步，现自愈回落；配套 e2e `TestE2ESyncPullCursorRewind`）；设置页「立即同步」三态反馈（`SyncOutcome`：未配置 / 已在同步中 / 完成），并发触发不再误报完成；⑧ 测试：新增 `PanRouterSyncPayloadTest` 5 例（多字节/emoji 截断、预算收敛、转义膨胀、error 钳制、正常透传）+ 引擎用例毒丸丢弃 / 网络失败保留 / 坏 payload 保记忆 / 游标回退 / 满页不推进防御；`:desktop:test` **160/160 全绿**，服务端 `go vet` 干净 + 全量测试全绿。
- **v1.6（2026-11-04）**：**M3 历史同步落地，路线图 M1/M2/M3 全部收口（桌面端 v0.1.5 ↔ 服务端 development.md v2.1）**。① 服务端先发：新表 `sync_items`（`(kind,uid)` 唯一、payload 不透明 JSON、`deleted` 墓碑、`client_updated_at` 为 LWW 键）+ `POST/GET /api/v1/sync` + 管理页「设备同步数据」只读浏览 + 4 个 e2e 用例；生效变更以 DELETE+INSERT 换新 rowid，使 `id` 成为严格单调的全局变更序号（pull 游标可断点续拉、旧变更不会被“改旧覆盖”打乱顺序）。② 桌面端 outbox（CDC）：三业务表加 `syncUid`（UUID，走 `ALTER TABLE ... runCatching` 增量迁移），业务 DAO 在增/删/改的**同一写事务内**记变更意图（零漏事件），引擎推送时回读业务行组包（多次修改自然合并，DAO 层不必知道契约结构）；`pushed=true` 条目**保留不删，作本机当前版本的 LWW 时间戳记忆**——设计期真缺陷：删除已推送条目会让推送后到达的旧远端变更把本地新状态覆盖回去，测试阶段抓出并修正（另修同毫秒并列时墓碑优先）。③ 合并规则：本地版本 ≥ 远端 → skip；否则 `deleteForUid` + 远端落地（墓碑删本地行）+ 记落地时间戳；**download_record 只上行不回填**（别设备记录不落本地下载列表，避免假任务），本机删除下载任务不传播墓碑（日志语义）。④ 首次开启回填：收藏/解析历史补 uid 并**补推**（既有库备份上服务端是开启同步的核心预期）；下载任务只补 uid 不补推。⑤ 触发：启动后常驻循环（30s 一轮，未开启时廉价 early-return）+ 设置页「立即同步」（显示积压条数）；同步为 opt-in，默认关闭（落实 §11-3 隐私裁定）。⑥ 测试：`HistorySyncEngineTest` 8 例（回填补推 / 同 uid 合并 / 墓碑推送 / 拉取落地且 download_record 跳过 / 拉取墓碑 / LWW 本地赢 / LWW 远端赢 / 未开启短路，内存 fake 与生产 DAO 记账契约逐条对齐）+ 2 个 Client 契约用例；`:desktop:test` **148/148 全绿**。⑦ 基础设施：GitHub 仓双线拓扑（`main`=桌面端线 / `server`=服务端线，无共同祖先）导致服务端 CI 触发与 deploy 条件必须改指 `server` 分支——已修正并记入 §9 M3。
- **v1.5（2026-11-02）**：**v0.1.0 生产级 Code Review 后的根因修复轮（8 项全修，发版 v0.1.1）**。① P1 远端会话残留：`startResolve` 入口统一 `resetRemoteSession()`（剪贴板/收藏入口会在 Detail 态直接开新解析，旧会话的 remoteShare/pendingRemoteLink/remoteBatchContext 残留会把本地解析误判为远端——无目录导航、转存禁用、startDownload 走已失效的 pendingRemoteLink）；`backToInput` 与批量空结果路径复用同一函数（后者原只清 remoteBatchLinks 不清 context）；② P2 批量超时预算失配：客户端切片 50→10（§8.2 更新；服务端 batch 串行逐项，50 项必超 120s callTimeout）；③ P2 INTERFACE_CHANGED 死路径：Client 加 `jsonBody()` 单点，2xx 非 JSON body（网关故障页/契约漂移）映射为 interface_changed（原先裸抛 JSONException，绕过 shouldFallbackToLocal，无友好文案也不回落本地），六处解析点全部收口 + 契约测试回归锁；④ P3 探活循环原子占位：probeJob check-then-act 改 `AtomicBoolean.compareAndSet`，并发硬失败不再拉起双循环，finally 补判消除「DOWN 无循环」窄窗口；⑤ P3 设置弹窗 remember 以 visible 为 key（取消后重开显示已保存值；旧测试结果不再跨会话残留）；⑥ P3 批量 shareUrl/历史语义：入队传所属分享 url（右键「复制分享链接」不再落整段粘贴文本）、历史记录存第一个分享链接、`enqueueRemote` 对空 effectiveUrl 显式拒绝（批量计失败数，单文件进 downloadError）；⑦ P3 规格修订（§5.2/决策注记）：session_expired **不自动清桌面端 token**——服务端中间件对缺/错 token 统一返回此 kind，自动清理误伤输错场景且丢失回显，报错引导设置页修正（代码行为本就更安全，文档对齐代码）；⑧ 顺手清理：defaultKindFor 冗余 else 分支、SESSION_EXPIRED 过时 KDoc。`:desktop:test` 106/106 全绿（+1 契约测试）。
- **v1.4（2026-11-01）**：**GUI 实机验收通过，M1/M2 全部闭环；M3 部分收口**。① GUI 端到端（截图实证）：应用拉起 → 设置页 PanRouter 区块渲染 → 弹窗「测试连接」真实回读生产版本 `v0d8ba6b630d3` → token AES-GCM 加密落盘 → 解析页死链走服务端路由（401 session_expired →「连接已失效」；token 修正后 404 share_gone →「分享已失效（服务端确认）」——REMOTE 专属文案，本地链不会出现）→ 401→修 token→404 行为变化证明加密存取链正确；② 过程揪出两个真缺陷并修复：`SendKeys` 自动输入丢字符 `1` 导致坏 token（自动化手段问题，非集成代码缺陷；错误链按设计工作）；`PanRouterResolveGatewayTest` 直写 `java.util.prefs` 注册表污染真实应用配置 → 改为快照/还原隔离（回归全绿）；③ 环境坑：`run.ps1` 的 `Find-JdkHome` 拾到本机 JDK 11（`:desktop:run` 报 UnsupportedClassVersionError）——用 `~/.gradle/jdks/eclipse_adoptium-17`（toolchain 自动下载）注入 `JAVA_HOME` 启动；④ M3-2（插件链接路由）确认由架构天然满足：剪贴板/插件 hook/解析页/历史全部汇聚 `startResolve` 单漏斗，服务端路由自动生效。剩余 M3：fork 推 GitHub + 发版流水线（待建仓授权）、历史同步（依赖服务端端点定案）；aria2 维持不做。
- **v1.3（2026-11-01）**：**M2-C 跨分享批量 + 三项验收通过，M1/M2 全部收口**。① `ShareLinkParser.parseAll`（多链接提取，文案级提取码兜底——单测抓出并修复「只传 URL 丢失提取码」缺陷）+ `Client.resolveBatch`（items 数组契约）+ `Gateway.resolveBatch` + 解析页多链接分支（≥2 条覆盖平台链接 → resolve/batch → 合并扁平列表 `[序号] 文件名`，合成 fid 路由回所属分享取链；集成关闭/服务端不可达静默或提示退回单链接流程）；② 验收 A：官方入口 `.\run.ps1 build` BUILD SUCCESSFUL（Gradle 8.13 发行包经腾讯镜像预置解决官方源卡死）；验收 B：对真实生产 pan.example.com 契约冒烟四项全过（healthz 版本回读 / api_token 认证账号池 / 死链结构化错误 kind / batch 部分成功语义），临时用例跑完即删、token 未入库；验收 C：`:desktop:test` 105/105 全绿。aria2 推送正式裁定**不做**（生产 aria2 sidecar 未部署，实现即死代码；服务端上线后按 §5.3 契约补齐）。
- **v1.2（2026-11-01）**：**M2 主体落地**（批量 + 账号贡献）。① 远端模式批量下载：解除 M1 守卫改真实现——多选逐文件带 fid 取直链（串行 + 150ms 间隔自律，§8.4）、统一 `enqueueRemote`（单文件弹窗与批量共用）、中断/失败/入队计数反馈；② 账号贡献：`PanRouterClient.listAccounts/createAccount`（服务端契约 `{pan_type,name,cookie}`，实时校验）+ Gateway 管理面透传（不参与解析健康判定）+ `PanRouterContribution` 本机枚举（SecureAccountDaos 透明解密，仅对话框生命周期驻留内存）+ 设置页贡献弹窗（「我已知晓」二次确认门控、服务端同名去重提示、Cookie 不回显不落日志）。`:desktop:test` 101/101 全绿（+3 账号契约）。
- **v1.1（2026-11-01）**：**M1 代码全部落地**。新增 `data/panrouter/` 包 9 文件（Models/Error/Settings/TokenStore/Client/Health/Router/Gateway/Hub）+ ResolveViewModel 远端分支（解析分流/单文件取链/远端入队含中转降档/转存与批量守卫/状态清理）+ MainScreen 装配 + 设置页 PanRouter 区块（FadeAlertDialog 弹窗：开关/地址/token 掩码输入/失败回落/测试连接）。测试 5 个类 29 例（决策表/错误映射/Client 契约 MockWebServer/Health 状态机/Gateway 自学习与探活恢复），`:desktop:test` 98/98 全绿。待办：真实 pan.example.com 联调验收（GUI 环境）。
- **v1.0（2026-11-01）**：调研定稿。基于 YunX-Desktop v1.3.3（README/FEATURES/BUILD/USAGE/源码结构）与 PanRouter development.md v2.0（生产 v1.1.12）完成能力对照、许可证边界、架构与择路策略、模块落点（接线点均已源码核实）、M1-M3 里程碑与测试策略。
