package com.yunx.app.data.db

import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.sql.DriverManager
import java.nio.file.Files

/**
 * 真实 SQLite（生产 schema + 生产 JDBC DAO 实现）下的 outbox 记账锁定测试。
 *
 * 引擎测试（HistorySyncEngineTest）的内存 fake 只是「逐条对齐生产契约」的镜像，锁不住真实 SQL
 * ——压缩子查询 / 版本记忆上限 / discard 过滤 / 分块 IN / 终态意图接线 / 远端落地 SQL 都在这验证。
 * [AppDatabase.open] 传临时目录开独立真库，不触碰生产单例。
 */
class SyncOutboxStoreTest {

    private fun newDb(): Pair<AppDatabase, File> {
        val dir = Files.createTempDirectory("yunx-sync-test").toFile()
        return AppDatabase.open(dir) to dir
    }

    /** 第二条只读连接直查库文件（WAL 已提交数据可见），断言 DAO 方法暴露不到的行级事实 */
    private fun rawCount(dir: File, sql: String): Int =
        DriverManager.getConnection("jdbc:sqlite:${File(dir, "yunx.db").absolutePath}").use { c ->
            c.createStatement().use { st -> st.executeQuery(sql).use { rs -> rs.next(); rs.getInt(1) } }
        }

    // ---- 同 uid 压缩：待推送意图与版本记忆各只留最新一条，墓碑顶掉编辑 ----

    @Test
    fun `outbox compaction keeps single newest row per uid`() = runBlocking {
        val (db, dir) = newDb()
        val bookmarks = db.bookmarkDao()
        val outbox = db.syncOutboxDao()

        val id = bookmarks.insert(BookmarkEntity(link = "https://x/1", title = "v1"))
        val uid = bookmarks.observeAll().first().single { it.id == id }.syncUid
        assertTrue(uid.isNotEmpty())
        assertEquals(1, outbox.count())

        // 同 uid 连续编辑：待推送意图就地压缩只留最新一条（推送本来就会按 uid 合并）
        repeat(3) { i -> bookmarks.updateCategory(id, "分类$i") }
        assertEquals(1, outbox.count())
        val pending = outbox.take(10).single()
        assertEquals(uid, pending.uid)
        assertFalse(pending.deleted)

        // 删除：墓碑意图顶掉编辑意图
        bookmarks.delete(id)
        val tombstone = outbox.take(10).single()
        assertTrue(tombstone.deleted)
        assertEquals(uid, tombstone.uid)

        // 推送后变为版本记忆：本机版本时间戳 = 墓碑时间戳，且该 uid 只剩这一行
        outbox.markPushed(listOf(tombstone.id))
        assertEquals(0, outbox.count())
        assertEquals(1, rawCount(dir, "SELECT COUNT(*) FROM sync_outbox WHERE kind='bookmark' AND uid='$uid'"))
        assertEquals(tombstone.updatedAt, outbox.localUpdatedAt("bookmark", uid))
    }

    // ---- 版本记忆总量上限：挤最老的已推送行，待推送意图不动 ----

    @Test
    fun `version memory capped at 5000 evicting oldest pushed only`() = runBlocking {
        val (db, dir) = newDb()
        val bookmarks = db.bookmarkDao()
        val outbox = db.syncOutboxDao()

        // 先落一条最早的已推送记忆（应被挤出）
        val id = bookmarks.insert(BookmarkEntity(link = "https://x/old", title = "old"))
        val uid = bookmarks.observeAll().first().single { it.id == id }.syncUid
        bookmarks.delete(id)
        val tombstone = outbox.take(10).single()
        outbox.markPushed(listOf(tombstone.id))

        // 批量灌 5001 条更新的历史记忆（绕过逐条记账做夹具），再走一次生产 enqueue 触发 capOutboxMemory
        DriverManager.getConnection("jdbc:sqlite:${File(dir, "yunx.db").absolutePath}").use { c ->
            c.prepareStatement("INSERT INTO sync_outbox(kind,uid,deleted,updatedAt,createdAt,pushed) VALUES('bookmark',?,0,?,?,1)").use { ps ->
                for (i in 1..5001) {
                    ps.setString(1, "cap-$i"); ps.setLong(2, i.toLong()); ps.setLong(3, i.toLong()); ps.addBatch()
                }
                ps.executeBatch()
            }
        }
        outbox.enqueue("bookmark", "cap-trigger", deleted = false, updatedAt = 999_999, pushed = true)

        // 上限生效：总数 5000；最早的记忆（uid 与 cap-1）被挤出，最新的保留
        assertEquals(5000, rawCount(dir, "SELECT COUNT(*) FROM sync_outbox WHERE pushed = 1"))
        assertNull(outbox.localUpdatedAt("bookmark", uid))
        assertNull(outbox.localUpdatedAt("bookmark", "cap-1"))
        assertEquals(999_999L, outbox.localUpdatedAt("bookmark", "cap-trigger"))

        // 待推送意图不受上限影响（它们是未同步的真实变更，不能丢）
        bookmarks.insert(BookmarkEntity(link = "https://x/new", title = "new"))
        assertEquals(1, outbox.count())
    }

    // ---- discard：服务端 4xx 拒收的毒丸批，删行且不留版本记忆 ----

    @Test
    fun `discard removes pending intents without leaving memory`() = runBlocking {
        val (db, _) = newDb()
        val bookmarks = db.bookmarkDao()
        val outbox = db.syncOutboxDao()

        val id = bookmarks.insert(BookmarkEntity(link = "https://x/p", title = "poison"))
        val uid = bookmarks.observeAll().first().single { it.id == id }.syncUid
        bookmarks.updateCategory(id, "c2")
        val ids = outbox.take(10).map { it.id }
        assertEquals(1, ids.size) // 已压缩为一条

        outbox.discard(ids)
        assertEquals(0, outbox.count())
        assertNull(outbox.localUpdatedAt("bookmark", uid)) // 被拒 ≠ 已同步，不留记忆

        // discard 不得误伤已推送版本记忆
        val id2 = bookmarks.insert(BookmarkEntity(link = "https://x/q", title = "keep"))
        val uid2 = bookmarks.observeAll().first().single { it.id == id2 }.syncUid
        val keepIds = outbox.take(10).map { it.id }
        outbox.markPushed(keepIds)
        outbox.discard(keepIds) // 已推送行不受 discard 影响
        assertTrue(outbox.localUpdatedAt("bookmark", uid2) != null)
    }

    // ---- 分块 IN 回读 + 空 uid 行提取 + 索引迁移 ----

    @Test
    fun `listBySyncUids chunks beyond variable limit and lists empty uid rows`() = runBlocking {
        val (db, dir) = newDb()
        val bookmarks = db.bookmarkDao()

        for (i in 1..900) {
            bookmarks.insert(BookmarkEntity(link = "https://x/$i", title = "t$i", syncUid = "chunk-$i"))
        }
        // 一条「迁移旧数据」行：生产 insert 总是当场分配 uid，空 uid 只存在于
        // 老版本库迁移（ALTER TABLE 默认 ''）的行上——用裸 SQL 还原该形态
        bookmarks.insert(BookmarkEntity(link = "https://x/legacy", title = "旧数据", syncUid = "legacy-x"))
        DriverManager.getConnection("jdbc:sqlite:${File(dir, "yunx.db").absolutePath}").use { c ->
            c.createStatement().execute("UPDATE bookmark SET syncUid = '' WHERE syncUid = 'legacy-x'")
        }
        // 迁移建了索引（分块回读与空 uid 提取都依赖它收敛）
        assertEquals(
            1, rawCount(dir, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_bookmark_sync_uid'")
        )
        val empty = bookmarks.listWithEmptySyncUid()
        assertEquals(1, empty.size)
        assertTrue(empty.single().syncUid.isEmpty())

        // 900 个 uid > 单语句变量上限 → 分块（400+400+100）必须全部取回
        val found = bookmarks.listBySyncUids((1..900).map { "chunk-$it" })
        assertEquals(900, found.size)
        assertEquals(emptySet<String>(), found.map { it.syncUid }.toSet() - (1..900).map { "chunk-$it" }.toSet())
    }

    // ---- download_record 终态意图接线（updateStatus(FAILED)/updateError/complete） ----

    @Test
    fun `download terminal states wire download_record intents`() = runBlocking {
        val (db, _) = newDb()
        val tasks = db.downloadTaskDao()
        val outbox = db.syncOutboxDao()

        val t1 = tasks.insert(DownloadTaskEntity(url = "https://x", fileName = "a.zip", syncUid = "dl-i1"))
        assertEquals(0, outbox.count()) // 插入不记账（下载记录只上报终态）

        tasks.updateStatus(t1, DownloadTaskEntity.STATUS_FAILED)
        tasks.updateError(t1, "boom")
        val failedIntents = outbox.take(50)
        assertEquals(1, failedIntents.size) // 两笔已压缩为一条（最新）
        assertEquals("download_record", failedIntents.single().kind)
        assertEquals("dl-i1", failedIntents.single().uid)

        val t2 = tasks.insert(DownloadTaskEntity(url = "https://x", fileName = "b.zip", syncUid = "dl-i2"))
        tasks.complete(t2, DownloadTaskEntity.STATUS_COMPLETED, "C:/b.zip", 0)
        val pendings = outbox.take(50)
        assertEquals(2, pendings.size)
        assertEquals(setOf("dl-i1", "dl-i2"), pendings.map { it.uid }.toSet())
    }

    // ---- 同链接行提取：重复归一的确定性裁决依赖排序口径 ----

    @Test
    fun `listByLinkForSync returns same-link rows ordered by createTime`() = runBlocking {
        val (db, _) = newDb()
        val bookmarks = db.bookmarkDao()
        bookmarks.insert(BookmarkEntity(link = "https://x/same", title = "b", syncUid = "u-b", createTime = 200))
        bookmarks.insert(BookmarkEntity(link = "https://x/same", title = "a", syncUid = "u-a", createTime = 100))
        bookmarks.insert(BookmarkEntity(link = "https://x/other", title = "o", syncUid = "u-o", createTime = 50))
        val rows = bookmarks.listByLinkForSync("https://x/same")
        assertEquals(listOf("u-a", "u-b"), rows.map { it.syncUid })
        assertEquals(listOf("a", "b"), rows.map { it.title })
    }

    // ---- 远端落地 SQL：按 uid 幂等覆盖、不记账、墓碑删行 ----

    @Test
    fun `applyFromRemote is idempotent per uid and never enqueues`() = runBlocking {
        val (db, _) = newDb()
        val bookmarks = db.bookmarkDao()
        val outbox = db.syncOutboxDao()

        bookmarks.applyFromRemote(BookmarkEntity(link = "https://r/1", title = "远端", syncUid = "bm-r1", createTime = 5))
        assertEquals(0, outbox.count()) // 远端数据不回推
        bookmarks.applyFromRemote(BookmarkEntity(link = "https://r/1", title = "远端v2", syncUid = "bm-r1", createTime = 5))
        val rows = bookmarks.observeAll().first()
        assertEquals(1, rows.size) // 同 uid 幂等覆盖而非插入
        assertEquals("远端v2", rows.single().title)

        bookmarks.deleteBySyncUid("bm-r1")
        assertTrue(bookmarks.observeAll().first().isEmpty())
        assertEquals(0, outbox.count())
    }
}
