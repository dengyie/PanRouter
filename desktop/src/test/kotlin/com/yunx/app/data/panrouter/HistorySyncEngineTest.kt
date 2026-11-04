package com.yunx.app.data.panrouter

import com.yunx.app.data.db.BookmarkDao
import com.yunx.app.data.db.BookmarkEntity
import com.yunx.app.data.db.DownloadTaskDao
import com.yunx.app.data.db.DownloadTaskEntity
import com.yunx.app.data.db.LinkHistoryDao
import com.yunx.app.data.db.LinkHistoryEntity
import com.yunx.app.data.db.SyncOutboxDao
import com.yunx.app.data.db.SyncOutboxEntity
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.runBlocking
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

/**
 * 历史同步引擎测试：内存 DAO fake + 假传输，覆盖回填/推送合并/拉取合并/LWW/墓碑/
 * download_record 不回填全链路（不打网络、不碰 AppDatabase/prefs 真实存储）。
 *
 * ★ fake 的记账契约与生产 JDBC DAO 逐条对齐（AppDatabase.kt）：insert 分配 uid + 记 outbox、
 *   delete 读 uid + 墓碑、updateSyncUid/applyFromRemote/deleteBySyncUid 不记 outbox、
 *   enqueue 按 (kind,uid,pushed) 压缩只留最新一条——改生产 DAO 的记账行为时必须同步改这里的 fake。
 */
class HistorySyncEngineTest {

    // ---- 内存 fake：与生产 DAO 契约对齐 ----

    /** 单调测试时钟：outbox 时间戳全部走它，与测试里硬编码的远端时间戳同一量级且确定 */
    private val clock = java.util.concurrent.atomic.AtomicLong(100)

    private class FakeOutboxDao : SyncOutboxDao {
        val entries = LinkedHashMap<Long, SyncOutboxEntity>()
        override suspend fun enqueue(kind: String, uid: String, deleted: Boolean, updatedAt: Long, pushed: Boolean) {
            val id = (entries.keys.maxOrNull() ?: 0L) + 1
            entries[id] = SyncOutboxEntity(id, kind, uid, deleted, updatedAt, updatedAt, pushed)
            // 与生产 enqueueOutbox 对齐：同 (kind,uid,pushed) 只留最新一条（时间戳并列删除赢）
            compact(kind, uid, pushed)
        }
        override suspend fun take(limit: Int): List<SyncOutboxEntity> =
            entries.values.filter { !it.pushed }.sortedBy { it.id }.take(limit)
        override suspend fun markPushed(ids: List<Long>) {
            ids.forEach { id -> entries[id]?.let { entries[id] = it.copy(pushed = true) } }
            // 与生产 markPushed 对齐：标记后压缩受影响 uid 的版本记忆
            ids.forEach { id -> entries[id]?.let { compact(it.kind, it.uid, pushed = true) } }
        }
        override suspend fun discard(ids: List<Long>) {
            // 与生产 discard 对齐：只删待推送行（已推送版本记忆不受影响）
            ids.forEach { id -> entries[id]?.takeIf { !it.pushed }?.let { entries.remove(id) } }
        }

        /** 与生产 compactOutbox 对齐：同 (kind,uid,pushed) 只留 updatedAt 最大者（并列：删除赢 → 新 id） */
        private fun compact(kind: String, uid: String, pushed: Boolean) {
            val keep = entries.values
                .filter { it.kind == kind && it.uid == uid && it.pushed == pushed }
                .maxWithOrNull(compareBy({ it.updatedAt }, { it.deleted }, { it.id })) ?: return
            entries.entries.removeAll {
                it.value.kind == kind && it.value.uid == uid && it.value.pushed == pushed && it.value.id != keep.id
            }
        }
        override suspend fun localUpdatedAt(kind: String, uid: String): Long? =
            entries.values.filter { it.kind == kind && it.uid == uid }.maxByOrNull { it.updatedAt }?.updatedAt
        override suspend fun deleteForUid(kind: String, uid: String) {
            entries.entries.removeAll { it.value.kind == kind && it.value.uid == uid }
        }
        override suspend fun count(): Int = entries.values.count { !it.pushed }
    }

    /** 模拟生产 JdbcBookmarkDao 的记账行为（insert 记 outbox / delete 墓碑） */
    private class FakeBookmarkDao(
        private val outbox: FakeOutboxDao,
        private val clock: java.util.concurrent.atomic.AtomicLong
    ) : BookmarkDao {
        val rows = LinkedHashMap<Long, BookmarkEntity>()
        private val flow = MutableStateFlow<List<BookmarkEntity>>(emptyList())
        private fun reload() { flow.value = rows.values.sortedByDescending { it.createTime } }
        override fun observeAll(): Flow<List<BookmarkEntity>> = flow
        override fun observeCategories(): Flow<List<String>> =
            flow.map { list -> list.map { it.category }.distinct().sorted() }
        override suspend fun insert(bookmark: BookmarkEntity): Long {
            val id = (rows.keys.maxOrNull() ?: 0L) + 1
            val uid = bookmark.syncUid.ifEmpty { "bm-" + java.util.UUID.randomUUID() }
            rows[id] = bookmark.copy(id = id, syncUid = uid)
            outbox.enqueue(HistorySyncEngine.KIND_BOOKMARK, uid, deleted = false, updatedAt = clock.incrementAndGet())
            reload(); return id
        }
        override suspend fun updateCategory(id: Long, category: String) {
            // 与生产 JdbcBookmarkDao 对齐：改分类也是一次变更（uid 缺失时当场补，并记待推送意图）
            val uid = rows.getValue(id).syncUid.ifEmpty { "bm-" + java.util.UUID.randomUUID() }
            rows[id] = rows.getValue(id).copy(category = category, syncUid = uid); reload()
            outbox.enqueue(HistorySyncEngine.KIND_BOOKMARK, uid, deleted = false, updatedAt = clock.incrementAndGet())
        }
        override suspend fun delete(id: Long) {
            rows.remove(id)?.let { b ->
                if (b.syncUid.isNotEmpty()) {
                    outbox.enqueue(HistorySyncEngine.KIND_BOOKMARK, b.syncUid, deleted = true, updatedAt = clock.incrementAndGet())
                }
            }
            reload()
        }
        override suspend fun updateSyncUid(id: Long, uid: String) {
            rows[id] = rows.getValue(id).copy(syncUid = uid); reload()
        }
        override suspend fun listWithEmptySyncUid(): List<BookmarkEntity> =
            rows.values.filter { it.syncUid.isEmpty() }
        override suspend fun listBySyncUids(uids: List<String>): List<BookmarkEntity> =
            if (uids.isEmpty()) emptyList() else rows.values.filter { it.syncUid in uids.toSet() }
        override suspend fun applyFromRemote(bookmark: BookmarkEntity) {
            val existing = rows.entries.firstOrNull { it.value.syncUid == bookmark.syncUid }
            if (existing != null) rows[existing.key] = bookmark.copy(id = existing.key)
            else { val id = (rows.keys.maxOrNull() ?: 0L) + 1; rows[id] = bookmark.copy(id = id) }
            reload()
        }
        override suspend fun deleteBySyncUid(uid: String) {
            rows.entries.removeAll { it.value.syncUid == uid }; reload()
        }
    }

    /** 模拟生产 JdbcLinkHistoryDao 的记账行为 */
    private class FakeLinkHistoryDao(
        private val outbox: FakeOutboxDao,
        private val clock: java.util.concurrent.atomic.AtomicLong
    ) : LinkHistoryDao {
        val rows = LinkedHashMap<Long, LinkHistoryEntity>()
        private val flow = MutableStateFlow<List<LinkHistoryEntity>>(emptyList())
        private fun reload() { flow.value = rows.values.sortedByDescending { it.createTime } }
        override fun observeAll(): Flow<List<LinkHistoryEntity>> = flow
        override fun search(query: String): Flow<List<LinkHistoryEntity>> =
            flow.map { list -> list.filter { it.url.contains(query, ignoreCase = true) } }
        override suspend fun insert(entity: LinkHistoryEntity): Long {
            val id = (rows.keys.maxOrNull() ?: 0L) + 1
            val uid = entity.syncUid.ifEmpty { "lh-" + java.util.UUID.randomUUID() }
            rows[id] = entity.copy(id = id, syncUid = uid)
            outbox.enqueue(HistorySyncEngine.KIND_LINK_HISTORY, uid, deleted = false, updatedAt = clock.incrementAndGet())
            reload(); return id
        }
        override suspend fun delete(id: Long) {
            rows.remove(id)?.let { h ->
                if (h.syncUid.isNotEmpty()) {
                    outbox.enqueue(HistorySyncEngine.KIND_LINK_HISTORY, h.syncUid, deleted = true, updatedAt = clock.incrementAndGet())
                }
            }
            reload()
        }
        override suspend fun clear() {
            rows.values.toList().forEach { h ->
                if (h.syncUid.isNotEmpty()) {
                    outbox.enqueue(HistorySyncEngine.KIND_LINK_HISTORY, h.syncUid, deleted = true, updatedAt = clock.incrementAndGet())
                }
            }
            rows.clear(); reload()
        }
        override suspend fun updateSyncUid(id: Long, uid: String) {
            rows[id] = rows.getValue(id).copy(syncUid = uid); reload()
        }
        override suspend fun listWithEmptySyncUid(): List<LinkHistoryEntity> =
            rows.values.filter { it.syncUid.isEmpty() }
        override suspend fun listBySyncUids(uids: List<String>): List<LinkHistoryEntity> =
            if (uids.isEmpty()) emptyList() else rows.values.filter { it.syncUid in uids.toSet() }
        override suspend fun applyFromRemote(entity: LinkHistoryEntity) {
            val existing = rows.entries.firstOrNull { it.value.syncUid == entity.syncUid }
            if (existing != null) rows[existing.key] = entity.copy(id = existing.key)
            else { val id = (rows.keys.maxOrNull() ?: 0L) + 1; rows[id] = entity.copy(id = id) }
            reload()
        }
        override suspend fun deleteBySyncUid(uid: String) {
            rows.entries.removeAll { it.value.syncUid == uid }; reload()
        }
    }

    /** 引擎只用 listWithEmptySyncUid / updateSyncUid / listBySyncUids 与终态落库；其余方法不应被触达 */
    private class FakeDownloadTaskDao(
        private val outbox: FakeOutboxDao,
        private val clock: java.util.concurrent.atomic.AtomicLong
    ) : DownloadTaskDao {
        val rows = LinkedHashMap<Long, DownloadTaskEntity>()
        override fun observeAll(): Flow<List<DownloadTaskEntity>> = MutableStateFlow(rows.values.toList())
        override suspend fun insert(task: DownloadTaskEntity): Long = TODO("unused in engine test")
        override suspend fun get(id: Long): DownloadTaskEntity? = rows[id]
        override suspend fun listAll(): List<DownloadTaskEntity> = rows.values.toList()
        override suspend fun repairCompletedProgress() = TODO("unused in engine test")
        override suspend fun updateProgress(id: Long, status: Int, downloadedSize: Long, totalSize: Long) = TODO("unused")
        override suspend fun updatePlan(id: Long, chunkCount: Int, totalSize: Long) = TODO("unused")
        override suspend fun updateRequestHeaders(id: Long, encryptedHeaders: String) = TODO("unused")
        override suspend fun markInterruptedAsPaused() = TODO("unused")
        override suspend fun updateStatus(id: Long, status: Int) {
            // 与生产 JdbcDownloadTaskDao 对齐：仅 FAILED 终态记 download_record 意图
            rows[id] = rows.getValue(id).copy(status = status)
            if (status == DownloadTaskEntity.STATUS_FAILED) {
                rows.getValue(id).syncUid.takeIf { it.isNotEmpty() }?.let {
                    outbox.enqueue(HistorySyncEngine.KIND_DOWNLOAD_RECORD, it, deleted = false, updatedAt = clock.incrementAndGet())
                }
            }
        }
        override suspend fun updateError(id: Long, errorMsg: String) {
            // 与生产 updateError 对齐：错误信息总在 updateStatus(FAILED) 之后写入，这里再记一笔
            rows[id] = rows.getValue(id).copy(errorMsg = errorMsg)
            rows.getValue(id).syncUid.takeIf { it.isNotEmpty() }?.let {
                outbox.enqueue(HistorySyncEngine.KIND_DOWNLOAD_RECORD, it, deleted = false, updatedAt = clock.incrementAndGet())
            }
        }
        override suspend fun complete(id: Long, status: Int, savePath: String, avgSpeed: Long) {
            // 与生产 complete 对齐：COMPLETED 终态记 download_record 意图
            rows[id] = rows.getValue(id).copy(status = status, savePath = savePath, avgSpeed = avgSpeed)
            rows.getValue(id).syncUid.takeIf { it.isNotEmpty() }?.let {
                outbox.enqueue(HistorySyncEngine.KIND_DOWNLOAD_RECORD, it, deleted = false, updatedAt = clock.incrementAndGet())
            }
        }
        override suspend fun delete(id: Long) { rows.remove(id) }
        override suspend fun updateEngineTaskId(id: Long, engineTaskId: String) = TODO("unused")
        override suspend fun updateDirect(id: Long, partPath: String, mode: Int) = TODO("unused")
        override suspend fun updateFileName(id: Long, name: String) = TODO("unused")
        override suspend fun listSyncableEngineTasks(): List<DownloadTaskEntity> = TODO("unused")
        override suspend fun updateSyncUid(id: Long, uid: String) {
            rows[id] = rows.getValue(id).copy(syncUid = uid)
        }
        override suspend fun listWithEmptySyncUid(): List<DownloadTaskEntity> =
            rows.values.filter { it.syncUid.isEmpty() }
        override suspend fun listBySyncUids(uids: List<String>): List<DownloadTaskEntity> =
            if (uids.isEmpty()) emptyList() else rows.values.filter { it.syncUid in uids.toSet() }
    }

    /** 录制推送、回放拉取的假传输（push/pull 失败队列可注入，默认成功） */
    private class FakeTransport : HistorySyncEngine.SyncTransport {
        val pushed = mutableListOf<Pair<String, List<SyncPushItem>>>()
        val pushResponses = ArrayDeque<Result<SyncPushResult>>()
        val pullResponses = ArrayDeque<Result<SyncPullResult>>()
        override suspend fun push(deviceId: String, items: List<SyncPushItem>): Result<SyncPushResult> {
            pushed += deviceId to items
            return pushResponses.removeFirstOrNull()
                ?: Result.success(SyncPushResult(accepted = items.size, skipped = 0, serverTime = 0))
        }
        override suspend fun pull(since: Long, limit: Int): Result<SyncPullResult> =
            pullResponses.removeFirstOrNull() ?: Result.success(SyncPullResult(emptyList(), lastSeq = since))
    }

    private class FakeStores(
        override val bookmarkDao: BookmarkDao,
        override val linkHistoryDao: LinkHistoryDao,
        override val downloadTaskDao: DownloadTaskDao,
        override val outboxDao: SyncOutboxDao
    ) : HistorySyncEngine.SyncStores

    // ---- 装配 ----

    private val outbox = FakeOutboxDao()
    private val bookmarks = FakeBookmarkDao(outbox, clock)
    private val histories = FakeLinkHistoryDao(outbox, clock)
    private val tasks = FakeDownloadTaskDao(outbox, clock)
    private val transport = FakeTransport()

    private var savedEnabled = false
    private var savedBaseUrl = ""
    private var savedSyncEnabled = false
    private var savedCursor = 0L
    private var savedLastAt = 0L

    @Before
    fun setUp() {
        // prefs 快照（PanRouterSettings 直连注册表，与真实应用共享——照网关测试的隔离口径）
        val s = PanRouterSettings()
        savedEnabled = s.enabled
        savedBaseUrl = s.baseUrl
        savedSyncEnabled = s.historySyncEnabled
        savedCursor = s.syncCursor
        savedLastAt = s.syncLastAt
        s.enabled = true
        s.baseUrl = "https://pan.example.invalid"
        s.historySyncEnabled = true

        HistorySyncEngine.storesProvider = {
            FakeStores(bookmarks, histories, tasks, outbox)
        }
        HistorySyncEngine.transportFactory = { transport }
        HistorySyncEngine.tokenProvider = { "test-token" }
    }

    @After
    fun tearDown() {
        val s = PanRouterSettings()
        s.enabled = savedEnabled
        s.baseUrl = savedBaseUrl
        s.historySyncEnabled = savedSyncEnabled
        s.syncCursor = savedCursor
        s.syncLastAt = savedLastAt
    }

    private fun remoteItem(
        id: Long, kind: String, uid: String, deleted: Boolean = false,
        clientUpdatedAt: Long, payload: org.json.JSONObject? = null
    ) = SyncRemoteItem(
        id = id, kind = kind, uid = uid, deviceId = "pc-other",
        payload = payload?.toString() ?: "", deleted = deleted, clientUpdatedAt = clientUpdatedAt
    )

    // ---------- 回填 + 推送 ----------

    @Test
    fun `backfill assigns uids and pushes existing bookmarks and histories`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://pan.quark.cn/s/old", title = "旧收藏", category = "视频"))
        val lhId = histories.insert(LinkHistoryEntity(url = "https://pan.quark.cn/s/h1", title = "旧历史"))
        tasks.rows[1L] = DownloadTaskEntity(id = 1, url = "https://x", fileName = "a.zip", shareUrl = "https://pan.quark.cn/s/d1", status = 3)

        // 服务端没有新数据（游标 0 → 空响应）
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        // 收藏/历史被回填 uid 并推送；下载任务只回填 uid 不推送
        assertEquals(1, transport.pushed.size)
        val (deviceId, items) = transport.pushed[0]
        assertTrue(deviceId.isNotBlank())
        assertEquals(2, items.size)
        val pushedBm = items.first { it.kind == HistorySyncEngine.KIND_BOOKMARK }
        assertEquals("旧收藏", pushedBm.payload?.optString("title"))
        assertEquals("视频", pushedBm.payload?.optString("category"))
        val pushedLh = items.first { it.kind == HistorySyncEngine.KIND_LINK_HISTORY }
        assertEquals("https://pan.quark.cn/s/h1", pushedLh.payload?.optString("url"))
        assertTrue(items.none { it.kind == HistorySyncEngine.KIND_DOWNLOAD_RECORD })

        val bm = bookmarks.rows.getValue(bmId)
        assertTrue(bm.syncUid.startsWith("bm-"))
        assertEquals(bm.syncUid, pushedBm.uid)
        assertTrue(histories.rows.getValue(lhId).syncUid.startsWith("lh-"))
        assertTrue(tasks.rows.getValue(1L).syncUid.startsWith("dl-"))

        // outbox 消费干净，游标/最近同步时间已更新
        assertEquals(0, outbox.count())
        assertTrue(PanRouterSettings().syncLastAt > 0)
    }

    @Test
    fun `push coalesces intents for same uid into newest`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/1", title = "v1"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        bookmarks.updateCategory(bmId, "软件")
        // 手工补记一条更早的意图（模拟两次修改落了两条 outbox）——生产/真实库会在 DAO 层就地合并
        outbox.enqueue(HistorySyncEngine.KIND_BOOKMARK, uid, deleted = false, updatedAt = 1000)
        val latestAt = outbox.entries.values.filter { it.uid == uid }.maxOf { it.updatedAt }

        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        assertEquals(1, transport.pushed.size)
        val items = transport.pushed[0].second
        assertEquals(1, items.size) // 同 uid 的多条意图合并为一条
        assertEquals(latestAt, items[0].updatedAt)
        assertEquals("软件", items[0].payload?.optString("category")) // 推送时回读最新值
        assertEquals(0, outbox.count())
    }

    @Test
    fun `delete enqueues tombstone without payload`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/1", title = "待删"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        bookmarks.delete(bmId)

        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        val items = transport.pushed[0].second
        assertEquals(1, items.size)
        assertTrue(items[0].deleted)
        assertNull(items[0].payload)
        assertEquals(uid, items[0].uid)
    }

    @Test
    fun `repeated edits keep a single version memory row per uid`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/1", title = "v1"))
        val uid = bookmarks.rows.getValue(bmId).syncUid

        // 编辑 → 同步 循环三轮：每轮都会把待推送意图标记为已推送（版本记忆）
        repeat(3) { i ->
            bookmarks.updateCategory(bmId, "分类$i")
            transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
            assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        }

        // 版本记忆不累积：同一 uid 只有一条 pushed 行，且是本机最新版本的时间戳
        val memory = outbox.entries.values.filter { it.kind == HistorySyncEngine.KIND_BOOKMARK && it.uid == uid }
        assertEquals(1, memory.count { it.pushed })
        assertEquals(0, outbox.count())
        val latestLocalEdit = outbox.entries.values.filter { it.uid == uid }.maxOf { it.updatedAt }
        assertEquals(latestLocalEdit, outbox.localUpdatedAt(HistorySyncEngine.KIND_BOOKMARK, uid))
    }

    // ---------- 拉取合并 ----------

    @Test
    fun `pull applies remote bookmark and skips download_record`() = runBlocking {
        transport.pullResponses += Result.success(
            SyncPullResult(
                items = listOf(
                    remoteItem(
                        id = 1, kind = HistorySyncEngine.KIND_BOOKMARK, uid = "bm-r1", clientUpdatedAt = 2000,
                        payload = org.json.JSONObject()
                            .put("link", "https://pan.quark.cn/s/remote")
                            .put("title", "远端收藏")
                            .put("platform", "QUARK")
                            .put("category", "") // 空分类 → 回落默认分类
                    ),
                    remoteItem(
                        id = 2, kind = HistorySyncEngine.KIND_DOWNLOAD_RECORD, uid = "dl-r1", clientUpdatedAt = 2001,
                        payload = org.json.JSONObject().put("file_name", "别设备的文件.zip")
                    )
                ),
                lastSeq = 2
            )
        )
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        // 收藏落地（空分类回默认），download_record 不回填为本地任务
        val bm = bookmarks.rows.values.single()
        assertEquals("bm-r1", bm.syncUid)
        assertEquals("远端收藏", bm.title)
        assertEquals(BookmarkEntity.DEFAULT_CATEGORY, bm.category)
        assertTrue(tasks.rows.isEmpty())
        assertEquals(2L, PanRouterSettings().syncCursor)
    }

    @Test
    fun `pull tombstone deletes local row and drops pending intent`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/1", title = "将被远端删除"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        assertEquals(1, outbox.count()) // 本地还有待推送意图

        transport.pullResponses += Result.success(
            SyncPullResult(
                items = listOf(remoteItem(id = 9, kind = HistorySyncEngine.KIND_BOOKMARK, uid = uid, deleted = true, clientUpdatedAt = 9999)),
                lastSeq = 9
            )
        )
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        assertTrue(bookmarks.rows.isEmpty())
        assertEquals(0, outbox.count()) // 本地旧意图作废（远端墓碑更新）
        assertEquals(9L, PanRouterSettings().syncCursor)
    }

    @Test
    fun `local pending intent newer than remote wins`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/local-edit", title = "本地新改"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        val pendingAt = outbox.entries.values.first { it.uid == uid }.updatedAt
        assertTrue(pendingAt > 0)

        // 远端是更旧的版本 → 本地待推送意图保留，远端不落地
        transport.pullResponses += Result.success(
            SyncPullResult(
                items = listOf(
                    remoteItem(
                        id = 3, kind = HistorySyncEngine.KIND_BOOKMARK, uid = uid,
                        clientUpdatedAt = pendingAt - 1,
                        payload = org.json.JSONObject().put("link", "https://x/remote-old").put("title", "远端旧版")
                    )
                ),
                lastSeq = 3
            )
        )
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        val bm = bookmarks.rows.getValue(bmId)
        assertEquals("本地新改", bm.title) // 本地未被远端旧版覆盖
        // 本地版本记忆保留（推送后变为 pushed=1 的版本记录），且仍不旧于远端
        assertEquals(pendingAt, outbox.localUpdatedAt(HistorySyncEngine.KIND_BOOKMARK, uid))
    }

    @Test
    fun `newer remote than pending intent applies and drops intent`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/local", title = "本地旧改"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        val pendingAt = outbox.entries.values.first { it.uid == uid }.updatedAt

        // 远端比本地待推送意图更新（另一台设备基于更晚的修改）→ 远端落地，本地意图作废
        transport.pullResponses += Result.success(
            SyncPullResult(
                items = listOf(
                    remoteItem(
                        id = 4, kind = HistorySyncEngine.KIND_BOOKMARK, uid = uid,
                        clientUpdatedAt = pendingAt + 5_000,
                        payload = org.json.JSONObject().put("link", "https://x/remote-new").put("title", "远端新版")
                    )
                ),
                lastSeq = 4
            )
        )
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        assertEquals("远端新版", bookmarks.rows.getValue(bmId).title)
        // 本机版本记忆被替换为该次远端变更的时间戳（旧意图/旧记忆作废）
        assertEquals(pendingAt + 5_000, outbox.localUpdatedAt(HistorySyncEngine.KIND_BOOKMARK, uid))
    }

    @Test
    fun `bad remote payload leaves local row and version memory untouched`() = runBlocking {
        val bmId = bookmarks.insert(BookmarkEntity(link = "https://x/keep", title = "本机保留"))
        val uid = bookmarks.rows.getValue(bmId).syncUid
        val pendingAt = outbox.entries.values.first { it.uid == uid }.updatedAt

        transport.pullResponses += Result.success(
            SyncPullResult(
                items = listOf(
                    // 非 JSON payload：整条跳过——本机行、待推送意图、版本记忆都不动
                    SyncRemoteItem(
                        id = 5, kind = HistorySyncEngine.KIND_BOOKMARK, uid = uid, deviceId = "pc-other",
                        payload = "{not-json", deleted = false, clientUpdatedAt = 999_999
                    ),
                    // 空 link：同样跳过（不产生空数据行）
                    SyncRemoteItem(
                        id = 6, kind = HistorySyncEngine.KIND_BOOKMARK, uid = uid, deviceId = "pc-other",
                        payload = org.json.JSONObject().put("title", "无链接").toString(),
                        deleted = false, clientUpdatedAt = 999_998
                    )
                ),
                lastSeq = 6
            )
        )
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))

        assertEquals("本机保留", bookmarks.rows.getValue(bmId).title)
        // 关键性质：坏 payload 不得覆盖本机版本记忆——仍是本机 pending 的时间戳，而非远端 999_999
        assertEquals(pendingAt, outbox.localUpdatedAt(HistorySyncEngine.KIND_BOOKMARK, uid))
        // 本轮 push 阶段已把待推送意图正常推走（假传输默认成功），队列清空
        assertEquals(0, outbox.count())
        // 游标照常推进：跳过的坏条目不会卡住后续变更
        assertEquals(6L, PanRouterSettings().syncCursor)
    }

    // ---------- 毒丸 / 游标防御 ----------

    @Test
    fun `server rejected batch is discarded so queue keeps flowing`() = runBlocking {
        bookmarks.insert(BookmarkEntity(link = "https://x/poison", title = "毒丸"))
        // 服务端 4xx 明确拒绝（UNSUPPORTED/400）：重推永远失败，必须丢弃换回推进能力
        transport.pushResponses += Result.failure(
            PanRouterError("payload too large", PanRouterError.Kind.UNSUPPORTED, retriable = false, httpStatus = 400)
        )
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        assertEquals(0, outbox.count()) // 毒丸批被丢弃，不卡队列

        // 下一轮正常数据照常推送
        val bm2 = bookmarks.insert(BookmarkEntity(link = "https://x/ok", title = "正常"))
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        val lastPush = transport.pushed.last().second
        assertEquals(1, lastPush.size)
        assertEquals(bookmarks.rows.getValue(bm2).syncUid, lastPush[0].uid)
        assertEquals(0, outbox.count())
    }

    @Test
    fun `network failure keeps intent for retry`() = runBlocking {
        bookmarks.insert(BookmarkEntity(link = "https://x/net", title = "网络失败"))
        // 网络/服务端故障 ≠ 内容被拒：意图必须留在队列里下轮重试
        transport.pushResponses += Result.failure(
            PanRouterError("connect timeout", PanRouterError.Kind.NETWORK, retriable = true)
        )
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        assertEquals(1, outbox.count())
    }

    @Test
    fun `cursor rewinds to server reported max seq when local cursor is ahead`() = runBlocking {
        PanRouterSettings().syncCursor = 999_999L
        // 服务端库回档后真实最大序号变小：空页回报 42，客户端必须跟随回落
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 42L))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        assertEquals(42L, PanRouterSettings().syncCursor)
    }

    @Test
    fun `full page without cursor advance stops instead of looping`() = runBlocking {
        val items = (1L..SYNC_PULL_PAGE).map { i ->
            SyncRemoteItem(
                id = i, kind = HistorySyncEngine.KIND_DOWNLOAD_RECORD, uid = "dl-$i", deviceId = "pc-other",
                payload = "", deleted = false, clientUpdatedAt = i
            )
        }
        // 排两页相同响应：若没有「游标未推进即停」防御，第二页也会被消费（死循环）
        repeat(2) { transport.pullResponses += Result.success(SyncPullResult(items, lastSeq = 0)) }
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        assertEquals(1, transport.pullResponses.size) // 只消费了一页
        assertEquals(0L, PanRouterSettings().syncCursor)
    }

    @Test
    fun `download record terminal states push payload with status and error`() = runBlocking {
        // 完成链：complete 落终态 → 意图 → 推送组包带 share_url/file_name/total_size/status
        tasks.rows[1L] = DownloadTaskEntity(
            id = 1, url = "https://x", fileName = "a.zip", shareUrl = "https://pan.quark.cn/s/d1",
            platform = "QUARK", totalSize = 123, status = DownloadTaskEntity.STATUS_PENDING, syncUid = "dl-t1"
        )
        tasks.complete(1L, DownloadTaskEntity.STATUS_COMPLETED, "C:/a.zip", 999)
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        val done = transport.pushed.last().second.single { it.kind == HistorySyncEngine.KIND_DOWNLOAD_RECORD }
        assertEquals("dl-t1", done.uid)
        assertEquals("a.zip", done.payload?.optString("file_name"))
        assertEquals("https://pan.quark.cn/s/d1", done.payload?.optString("share_url"))
        assertEquals("QUARK", done.payload?.optString("platform"))
        assertEquals(123L, done.payload?.optLong("total_size"))
        assertEquals(DownloadTaskEntity.STATUS_COMPLETED, done.payload?.optInt("status"))

        // 失败链：updateStatus(FAILED) → updateError 写错误 → 推送带上 status=FAILED 与 error
        tasks.rows[2L] = DownloadTaskEntity(
            id = 2, url = "https://x", fileName = "b.zip", shareUrl = "https://pan.quark.cn/s/d2",
            platform = "QUARK", status = DownloadTaskEntity.STATUS_PENDING, syncUid = "dl-t2"
        )
        tasks.updateStatus(2L, DownloadTaskEntity.STATUS_FAILED)
        tasks.updateError(2L, "HTTP 403")
        transport.pullResponses += Result.success(SyncPullResult(emptyList(), lastSeq = 0))
        assertEquals(HistorySyncEngine.SyncOutcome.DONE, HistorySyncEngine.syncOnce("test"))
        val failed = transport.pushed.last().second.single { it.kind == HistorySyncEngine.KIND_DOWNLOAD_RECORD }
        assertEquals("dl-t2", failed.uid)
        assertEquals(DownloadTaskEntity.STATUS_FAILED, failed.payload?.optInt("status"))
        assertEquals("HTTP 403", failed.payload?.optString("error"))
    }

    // ---------- 前提判定 ----------

    @Test
    fun `not ready settings short circuit without touching transport`() = runBlocking {
        PanRouterSettings().historySyncEnabled = false
        assertEquals(HistorySyncEngine.SyncOutcome.NOT_READY, HistorySyncEngine.syncOnce("test")) // 未执行
        assertTrue(transport.pushed.isEmpty())
        assertEquals(0L, PanRouterSettings().syncCursor)
    }
}
