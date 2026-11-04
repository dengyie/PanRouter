package com.yunx.app.data.panrouter

import com.yunx.app.data.db.AppDatabase
import com.yunx.app.data.db.BookmarkDao
import com.yunx.app.data.db.BookmarkEntity
import com.yunx.app.data.db.DownloadTaskDao
import com.yunx.app.data.db.LinkHistoryDao
import com.yunx.app.data.db.LinkHistoryEntity
import com.yunx.app.data.db.SyncOutboxDao
import com.yunx.app.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.util.concurrent.atomic.AtomicBoolean

/**
 * PanRouter 历史同步引擎（M3）：把本机收藏/解析历史/下载记录与自部署服务端做多设备双向同步。
 *
 * 数据流（outbox 模式）：
 * 1. 业务 DAO 在增/删/改的同一数据库写里把「变更意图」记进 `sync_outbox`（零漏事件，见 AppDatabase）；
 * 2. 本引擎每轮：回填旧数据 uid → 消费 outbox 推送（payload 推送时回读业务行当前值，多次修改自然合并）
 *    → 按游标拉取其他设备的变更合并进本地（LWW：client_updated_at 新者胜，墓碑传播删除）；
 * 3. 服务端把 payload 当不透明 JSON 存储（两仓零共享代码边界），仲裁与游标全部由服务端 sync_items 表承担。
 *
 * 语义约定：
 * - **download_record 只上行不回填**：别设备的下载记录不落本地 download_task（避免假任务污染下载列表），
 *   服务端管理页可查；本机删除下载任务也不传播墓碑（日志语义）；
 * - **pull 合并规则**：本地有同 uid 待推送意图且不旧于远端 → 跳过（等推送反压）；否则远端落地、旧意图作废；
 * - **重复归一**：同一 link/url 在多设备开启同步前各自产生异 uid 实体时，按「createTime 最早者胜，
 *   并列取 uid 字典序小者」收敛到一行（确定性规则，各设备独立裁决出同一赢家），
 *   败者以墓碑上传、双端删行——收藏按 link、解析历史按 url 判定；
 * - **幂等**：重推同 uid 由服务端 LWW skip；pull 游标（[PanRouterSettings.syncCursor]）断点续拉。
 *
 * 触发：启动后常驻循环（30s 一轮，未开启时是廉价 early-return）+ 设置页「立即同步」。
 */
object HistorySyncEngine {

    private const val TAG = "YunX-PanRouterSync"

    internal const val KIND_BOOKMARK = "bookmark"
    internal const val KIND_LINK_HISTORY = "link_history"
    internal const val KIND_DOWNLOAD_RECORD = "download_record"

    /** 一轮同步的结果（供 UI 区分「没配好」「已在跑」「完成」） */
    enum class SyncOutcome {
        /** 完整跑完一轮（不论有无变更） */
        DONE,
        /** 已有一轮在跑（定时器撞上手动「立即同步」），本次没执行 */
        BUSY,
        /** 前提不满足：未开启历史同步 / 未配置服务端地址 / 无 api_token */
        NOT_READY
    }

    /** 单批推送条数（与服务端 SyncMaxBatch 上限一致） */
    private const val PUSH_BATCH = 500

    /** 常驻循环间隔 */
    private const val LOOP_INTERVAL_MS = 30_000L

    /** 启动延迟：让启动期任务（下载恢复/账号解密/公告检查）先跑完 */
    private const val STARTUP_DELAY_MS = 8_000L

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    @Volatile
    private var started = false

    /** 同一轮里 push+pull 串行；并发调用（定时器撞上手动「立即同步」）直接让后者扑空 */
    private val syncing = AtomicBoolean(false)

    // ---- 可注入点（测试用；生产走真实单例与网络） ----

    /** 引擎用到的 DAO 集合抽象（AppDatabase 是 final 无法伪造，测试注入内存实现） */
    internal interface SyncStores {
        val bookmarkDao: BookmarkDao
        val linkHistoryDao: LinkHistoryDao
        val downloadTaskDao: DownloadTaskDao
        val outboxDao: SyncOutboxDao
    }

    internal var storesProvider: () -> SyncStores = {
        val db = AppDatabase.get()
        object : SyncStores {
            override val bookmarkDao = db.bookmarkDao()
            override val linkHistoryDao = db.linkHistoryDao()
            override val downloadTaskDao = db.downloadTaskDao()
            override val outboxDao = db.syncOutboxDao()
        }
    }

    internal var settingsProvider: () -> PanRouterSettings = { PanRouterSettings() }

    internal var tokenProvider: () -> String? = { PanRouterTokenStore.getToken() }

    /** 传输层工厂：生产=OkHttp 客户端；测试注入录制的假传输 */
    internal var transportFactory: (baseUrl: String) -> SyncTransport = { baseUrl ->
        val client = PanRouterClient(baseUrlProvider = { baseUrl })
        object : SyncTransport {
            override suspend fun push(deviceId: String, items: List<SyncPushItem>) =
                client.pushSync(deviceId, items)

            override suspend fun pull(since: Long, limit: Int) = client.pullSync(since, limit)
        }
    }

    /** 服务端往返抽象（push/pull 各一个方法，契约见 PanRouterClient 同名方法） */
    internal interface SyncTransport {
        suspend fun push(deviceId: String, items: List<SyncPushItem>): Result<SyncPushResult>
        suspend fun pull(since: Long, limit: Int): Result<SyncPullResult>
    }

    /** 应用启动时挂载常驻循环（MainScreen LaunchedEffect 调用；重复调用无害） */
    fun ensureStarted() {
        if (started) return
        synchronized(this) {
            if (started) return
            started = true
        }
        scope.launch {
            delay(STARTUP_DELAY_MS)
            while (true) {
                runCatching { syncOnce("loop") }
                    .onFailure { Log.w(TAG, "同步轮异常：${it.message ?: it.javaClass.simpleName}") }
                delay(LOOP_INTERVAL_MS)
            }
        }
    }

    /**
     * 跑一轮完整同步（回填 → 推送 → 拉取）。
     * 返回 [SyncOutcome]：UI 必须能区分「没执行（前提不满足）」与「已在同步中」，两者提示完全不同。
     */
    suspend fun syncOnce(reason: String = "manual"): SyncOutcome {
        if (!syncing.compareAndSet(false, true)) return SyncOutcome.BUSY
        try {
            val settings = settingsProvider()
            val token = tokenProvider()
            // 就绪判定走 tokenProvider 注入缝而非 PanRouterTokenStore 直连：
            // 两者生产等价（默认实现即 TokenStore），但测试可注入，不隐性依赖真实加密密钥
            if (!settings.historySyncEnabled || !settings.isConfigured() || token.isNullOrBlank()) {
                return SyncOutcome.NOT_READY
            }
            val stores = storesProvider()
            val transport = transportFactory(settings.baseUrl)

            backfillSyncUids(stores)
            pushPending(stores, settings.deviceId, transport)
            pullRemote(stores, settings, transport)
            settings.syncLastAt = System.currentTimeMillis()
            Log.i(TAG, "历史同步完成（$reason）：游标=${settings.syncCursor}")
            return SyncOutcome.DONE
        } finally {
            syncing.set(false)
        }
    }

    /**
     * 旧数据回填：syncUid 为空的行分配 uid。收藏/解析历史回填后**补推**（首开启同步时把既有库
     * 备份上服务端，这是用户开启同步的核心预期）；下载任务只回填 uid 不补推（日志语义：
     * 历史终态早已发生，全量补传既慢又无回放价值，此后的新终态会自然上报）。
     */
    private suspend fun backfillSyncUids(stores: SyncStores) {
        val outbox = stores.outboxDao
        // 常驻循环每 30s 都会跑这里：只取 syncUid 为空的行（走索引），稳态时零全表物化
        stores.bookmarkDao.listWithEmptySyncUid().forEach { b ->
            val uid = newUid("bm")
            stores.bookmarkDao.updateSyncUid(b.id, uid)
            outbox.enqueue(KIND_BOOKMARK, uid, deleted = false, updatedAt = b.createTime)
        }
        stores.linkHistoryDao.listWithEmptySyncUid().forEach { h ->
            val uid = newUid("lh")
            stores.linkHistoryDao.updateSyncUid(h.id, uid)
            outbox.enqueue(KIND_LINK_HISTORY, uid, deleted = false, updatedAt = h.createTime)
        }
        stores.downloadTaskDao.listWithEmptySyncUid().forEach { t ->
            stores.downloadTaskDao.updateSyncUid(t.id, newUid("dl"))
        }
    }

    /** 消费 outbox：同 kind+uid 合并取最新意图 → 回读业务行组包 → 推送 → 成功后整批标记已推送 */
    private suspend fun pushPending(stores: SyncStores, deviceId: String, transport: SyncTransport) {
        val outbox = stores.outboxDao
        while (true) {
            val batch = outbox.take(PUSH_BATCH)
            if (batch.isEmpty()) return
            // 同 kind+uid 合并取最新意图；时间戳并列时删除赢（宁传墓碑也不复活已删数据）
            val merged = batch.groupBy { it.kind to it.uid }
                .map { (_, g) -> g.maxWithOrNull(compareBy({ it.updatedAt }, { it.deleted }))!! }

            // payload 推送时回读（不带在意图里）：多次修改合并成最新状态，DAO 层不必知道契约结构。
            // 只回读本批要的行（syncUid 上有索引）——大历史库首次同步不再每批整表加载。
            val live = merged.filter { !it.deleted }
            val bookmarkByUid = stores.bookmarkDao
                .listBySyncUids(live.filter { it.kind == KIND_BOOKMARK }.map { it.uid })
                .associateBy { it.syncUid }
            val historyByUid = stores.linkHistoryDao
                .listBySyncUids(live.filter { it.kind == KIND_LINK_HISTORY }.map { it.uid })
                .associateBy { it.syncUid }
            val taskByUid = stores.downloadTaskDao
                .listBySyncUids(live.filter { it.kind == KIND_DOWNLOAD_RECORD }.map { it.uid })
                .associateBy { it.syncUid }
            val items = merged.mapNotNull { e ->
                // 墓碑：行已删（也可能从未在本机存在），直接透传删除意图，不做行回读
                if (e.deleted) {
                    SyncPushItem(e.kind, e.uid, deleted = true, updatedAt = e.updatedAt, payload = null)
                } else when (e.kind) {
                    KIND_BOOKMARK -> bookmarkByUid[e.uid]?.let {
                        SyncPushItem(e.kind, e.uid, false, e.updatedAt, SyncBookmarkPayload(
                            link = it.link, title = it.title, platform = it.platform,
                            pwd = it.pwd, category = it.category, createdAt = it.createTime
                        ).toJson())
                    }
                    KIND_LINK_HISTORY -> historyByUid[e.uid]?.let {
                        SyncPushItem(e.kind, e.uid, false, e.updatedAt, SyncLinkHistoryPayload(
                            url = it.url, title = it.title, platform = it.platform,
                            pwd = it.pwd, createdAt = it.createTime
                        ).toJson())
                    }
                    KIND_DOWNLOAD_RECORD -> taskByUid[e.uid]?.let { t ->
                        SyncPushItem(e.kind, e.uid, false, e.updatedAt, SyncDownloadRecordPayload(
                            shareUrl = t.shareUrl, platform = t.platform, fileName = t.fileName,
                            totalSize = t.totalSize, status = t.status, error = t.errorMsg,
                            createdAt = t.createTime
                        ).toJson())
                    }
                    else -> null // 未知 kind（防御）：随整批一起消费掉
                }
            }
            // 非墓碑但行已不存在（推送前被删）的意图自然丢弃；全空批不用打网络
            if (items.isNotEmpty()) {
                val r = transport.push(deviceId, items)
                if (r.isFailure) {
                    val e = r.exceptionOrNull()
                    if (isRejectedByServer(e)) {
                        // 服务端 4xx 明确拒绝内容（kind/uid 非法、payload 超限等）：重推同一批永远失败，
                        // 队列会被这批「毒丸」永久堵死（后面的变更全部推不出去）。丢弃本批意图换回推进能力。
                        // 被拒 ≠ 已同步，故用 discard 而非 markPushed——不留版本记忆。
                        Log.e(TAG, "推送被服务端拒绝，丢弃本批 ${batch.size} 条意图：${e?.message}")
                        outbox.discard(batch.map { it.id })
                        continue
                    }
                    Log.w(TAG, "推送失败（下轮重试）：${e?.message ?: "网络异常"}")
                    return // 留在 outbox，下轮再推
                }
            }
            // 标记已推送（保留行作 LWW 版本记忆——拉取合并要跟远端时间戳比大小）
            outbox.markPushed(batch.map { it.id })
            if (batch.size < PUSH_BATCH) return
        }
    }

    /**
     * 服务端以 4xx **明确拒绝内容**（不是网络/鉴权/服务端故障）：这类重试永远不会成功。
     * 服务端所有校验失败都走 KindUnsupported(400)，故只认这个组合；
     * 404（旧服务端没有该端点）/401/429/5xx/网络失败一律保留意图继续重试。
     */
    private fun isRejectedByServer(e: Throwable?): Boolean =
        e is PanRouterError && e.kind == PanRouterError.Kind.UNSUPPORTED && e.httpStatus in 400..499

    /** 按游标翻页拉取远端变更并合并 */
    private suspend fun pullRemote(stores: SyncStores, settings: PanRouterSettings, transport: SyncTransport) {
        val outbox = stores.outboxDao
        while (true) {
            val since = settings.syncCursor
            val r = transport.pull(since, SYNC_PULL_PAGE)
            if (r.isFailure) {
                Log.w(TAG, "拉取失败（下轮重试）：${r.exceptionOrNull()?.message ?: "网络异常"}")
                return
            }
            val res = r.getOrThrow()
            res.items.forEach { applyRemote(stores, outbox, it) }
            // 短页 = 已到底。此时服务端回报的是「库内真实最大序号」：比 since 小说明本机游标超前
            //（服务端库从备份回档 / 换机迁移误带了旧游标），必须跟随回落，否则永远拉不到任何变更。
            if (res.lastSeq != since) settings.syncCursor = res.lastSeq
            if (res.items.size < SYNC_PULL_PAGE) return
            // 防御：满页却没能推进游标 = 服务端契约违约，宁可停下也不死循环
            if (settings.syncCursor <= since) {
                Log.w(TAG, "拉取游标未推进（since=$since lastSeq=${res.lastSeq}），本轮停止")
                return
            }
        }
    }

    /**
     * 单条远端变更落地（合并规则见类注释）：
     * 本机已持有不旧于远端的版本（outbox 里的 LWW 记忆，含已推送与待推送）→ 跳过；
     * 否则远端落地（墓碑 = 删本地行），落地**成功后**才作废旧记忆并记录新版本时间戳。
     * 顺序很关键：先删记忆再写库的话，坏 payload / 写库失败会让本机既没更新也没记忆，
     * 后续更旧的远端变更就能覆盖本机更新状态。
     */
    private suspend fun applyRemote(stores: SyncStores, outbox: SyncOutboxDao, item: SyncRemoteItem) {
        // download_record 只上行不回填（避免别设备的下载记录变成假任务污染本地列表）
        if (item.kind != KIND_BOOKMARK && item.kind != KIND_LINK_HISTORY) return
        val localVersion = outbox.localUpdatedAt(item.kind, item.uid)
        if (localVersion != null && localVersion >= item.clientUpdatedAt) return

        if (item.deleted) {
            when (item.kind) {
                KIND_BOOKMARK -> stores.bookmarkDao.deleteBySyncUid(item.uid)
                KIND_LINK_HISTORY -> stores.linkHistoryDao.deleteBySyncUid(item.uid)
            }
        } else {
            val payload = item.payloadJson() ?: return // 空/坏 payload：整条跳过，本机行与版本记忆都不动
            when (item.kind) {
                KIND_BOOKMARK -> {
                    val p = SyncBookmarkPayload.parse(payload)
                    if (p.link.isEmpty()) return
                    val remoteRow = BookmarkEntity(
                        link = p.link, title = p.title, platform = p.platform, pwd = p.pwd,
                        category = p.category.ifEmpty { BookmarkEntity.DEFAULT_CATEGORY },
                        syncUid = item.uid, createTime = p.createdAt
                    )
                    // 同链接异 uid = 多设备开启同步前各自收藏的重复实体，先归一再落地
                    val dups = stores.bookmarkDao.listByLinkForSync(p.link)
                        .filter { it.syncUid != item.uid && it.syncUid.isNotEmpty() }
                    val winner = (dups + remoteRow).minWithOrNull(compareBy({ it.createTime }, { it.syncUid }))!!
                    if (winner.syncUid != item.uid) {
                        reconcileLose(outbox, item.kind, item.uid, item.clientUpdatedAt)
                        return
                    }
                    dups.forEach { stores.bookmarkDao.delete(it.id) }
                    stores.bookmarkDao.applyFromRemote(remoteRow)
                }
                KIND_LINK_HISTORY -> {
                    val p = SyncLinkHistoryPayload.parse(payload)
                    if (p.url.isEmpty()) return
                    val remoteRow = LinkHistoryEntity(
                        url = p.url, title = p.title, platform = p.platform, pwd = p.pwd,
                        syncUid = item.uid, createTime = p.createdAt
                    )
                    val dups = stores.linkHistoryDao.listByUrlForSync(p.url)
                        .filter { it.syncUid != item.uid && it.syncUid.isNotEmpty() }
                    val winner = (dups + remoteRow).minWithOrNull(compareBy({ it.createTime }, { it.syncUid }))!!
                    if (winner.syncUid != item.uid) {
                        reconcileLose(outbox, item.kind, item.uid, item.clientUpdatedAt)
                        return
                    }
                    dups.forEach { stores.linkHistoryDao.delete(it.id) }
                    stores.linkHistoryDao.applyFromRemote(remoteRow)
                }
            }
        }
        // 记录「本机现在持有的版本」= 该次远端变更的时间戳（后续更旧的远端/推送都以它仲裁）
        outbox.deleteForUid(item.kind, item.uid)
        outbox.enqueue(item.kind, item.uid, deleted = item.deleted, updatedAt = item.clientUpdatedAt, pushed = true)
    }

    /**
     * 重复实体归一的败者处置：本机保留旧实体、远端 uid 墓碑掉（或反之）。
     * 墓碑时间戳取 max(now, 对方时间戳+1)——必须严格新于对方才能赢服务端 LWW；
     * 记为待推送意图（不留记忆：被顶掉的实体并非「已同步的本机版本」），
     * 推送后各设备收到墓碑删行，全网收敛到同一个赢家行。
     */
    private suspend fun reconcileLose(outbox: SyncOutboxDao, kind: String, uid: String, opponentTs: Long) {
        outbox.deleteForUid(kind, uid)
        outbox.enqueue(kind, uid, deleted = true, updatedAt = maxOf(System.currentTimeMillis(), opponentTs + 1), pushed = false)
    }

    private fun newUid(prefix: String): String = "$prefix-" + java.util.UUID.randomUUID().toString()
}
