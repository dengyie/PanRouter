package com.yunx.app.data.announcement

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 公告静态源（本仓 app-announcements.json）解析语义测试：
 * 可见性过滤（定时发布 / 平台 targets）、排序（置顶在前 / 时间倒序）、
 * 置顶过期、ISO 时间解析。网络层不走单测（fetchDocument 依赖 OkHttp + 镜像）。
 */
class AnnouncementStaticSourceTest {

    private fun doc(vararg items: String): JSONObject =
        JSONObject("""{"announcements": [${items.joinToString(",")}]}""")

    private fun item(
        id: String,
        publishAt: String? = "2026-01-01T00:00:00+08:00",
        pinned: Boolean = false,
        targets: List<String>? = null,
        pinExpireAt: String? = null,
        content: String? = "正文"
    ): String {
        val t = targets?.joinToString(",") { """"$it" """ }?.let { """"targets": [$it],""" } ?: ""
        val p = publishAt?.let { """"publishAt": "$it",""" } ?: ""
        val pe = pinExpireAt?.let { """"pinExpireAt": "$it",""" } ?: ""
        val c = content?.let { """"content": "$it",""" } ?: ""
        return """{"id": "$id", "title": "$id 标题", "summary": "摘要", $c $p $t $pe "isPinned": $pinned}"""
    }

    @Test
    fun parsesBasicFieldsAndKeepsContent() {
        val list = AnnouncementApi.visibleAnnouncements(doc(item("a")))
        assertEquals(1, list.size)
        with(list[0]) {
            assertEquals("a", id)
            assertEquals("a 标题", title)
            assertEquals("正文", content)
            assertTrue(targets.isEmpty())
        }
    }

    @Test
    fun scheduledAnnouncementHiddenUntilPublishTime() {
        // 2100 年才发布 → 现在（2026 前后运行测试）不可见
        val future = AnnouncementApi.visibleAnnouncements(
            doc(item("past", publishAt = "2020-01-01T00:00:00+08:00"), item("future", publishAt = "2100-01-01T00:00:00+08:00"))
        )
        assertEquals(listOf("past"), future.map { it.id })
    }

    @Test
    fun desktopTargetingFiltersNonDesktopEntries() {
        val list = AnnouncementApi.visibleAnnouncements(
            doc(
                item("all"),                                            // 无 targets = 全平台
                item("desktop-only", targets = listOf("desktop")),      // 电脑端可见
                item("android-only", targets = listOf("android"))       // 电脑端不可见
            )
        )
        assertEquals(setOf("all", "desktop-only"), list.map { it.id }.toSet())
    }

    @Test
    fun pinnedFirstThenNewestFirst() {
        val list = AnnouncementApi.visibleAnnouncements(
            doc(
                item("old-normal", publishAt = "2020-01-01T00:00:00+08:00"),
                item("newest-normal", publishAt = "2026-06-01T00:00:00+08:00"),
                item("pinned-old", publishAt = "2020-01-01T00:00:00+08:00", pinned = true)
            )
        )
        assertEquals(listOf("pinned-old", "newest-normal", "old-normal"), list.map { it.id })
    }

    @Test
    fun expiredPinIsTreatedAsUnpinned() {
        val list = AnnouncementApi.visibleAnnouncements(
            doc(
                item("expired-pin", publishAt = "2026-01-01T00:00:00+08:00", pinned = true, pinExpireAt = "2026-02-01T00:00:00+08:00"),
                item("normal-new", publishAt = "2026-05-01T00:00:00+08:00")
            )
        )
        // 过期置顶不再霸榜：按时间倒序，normal-new 在前
        assertEquals(listOf("normal-new", "expired-pin"), list.map { it.id })
    }

    @Test
    fun entriesWithoutIdAreDropped() {
        val list = AnnouncementApi.visibleAnnouncements(
            doc(item("ok"), """{"id": "", "title": "脏数据"}""")
        )
        assertEquals(listOf("ok"), list.map { it.id })
    }

    @Test
    fun isoParsingHandlesNullAndInvalid() {
        assertNull(AnnouncementApi.parseIsoMillis(null))
        assertNull(AnnouncementApi.parseIsoMillis(""))
        assertNull(AnnouncementApi.parseIsoMillis("not-a-date"))
        assertEquals(1_759_968_000_000L, AnnouncementApi.parseIsoMillis("2025-10-09T00:00:00Z")!!)
    }

    /** v0.1.4 回归：列表必须每次重新下载（否则手动刷新拿不到新公告），详情缓存优先 */
    @Test
    fun fetchPageRefetchesSourceButDetailUsesCache() {
        kotlinx.coroutines.runBlocking {
            AnnouncementApi.clearCacheForTest()
            var version = 0
            var loadCount = 0
            AnnouncementApi.documentLoader = {
                loadCount++
                AnnouncementApi.Result.Success(doc(item("v$version")))
            }

            // 首次列表：下载 1 次，内容 v0
            val first = AnnouncementApi.fetchPage()
            assertEquals(listOf("v0"), (first as AnnouncementApi.Result.Success).data.list.map { it.id })
            assertEquals(1, loadCount)

            // 数据源变了：再拉列表必须重新下载，拿到 v1（修复前这里命中缓存返回 v0）
            version = 1
            val second = AnnouncementApi.fetchPage()
            assertEquals(listOf("v1"), (second as AnnouncementApi.Result.Success).data.list.map { it.id })
            assertEquals(2, loadCount)

            // 详情走缓存：不触发下载
            val detail = AnnouncementApi.fetchDetail("v1")
            assertEquals("v1", (detail as AnnouncementApi.Result.Success).data.id)
            assertEquals(2, loadCount)

            // 缓存里没有的 id（下架/新发布）：重新下载一次再查；仍查不到才报「不存在」
            version = 2
            val miss = AnnouncementApi.fetchDetail("ghost")
            assertTrue(miss is AnnouncementApi.Result.Failure)
            assertEquals(3, loadCount)
            // 下载后新 id 可直查
            val hit = AnnouncementApi.fetchDetail("v2")
            assertEquals("v2", (hit as AnnouncementApi.Result.Success).data.id)
            assertEquals(3, loadCount) // 刚下载过，缓存已含 v2，不再下载

            AnnouncementApi.clearCacheForTest()
            AnnouncementApi.documentLoader = { AnnouncementApi.fetchDocument() } // 还原真实加载器
        }
    }

    /** 数据源失败时缓存不清空：缓存已有的详情可用；缓存 miss 时下载失败原因正确透出 */
    @Test
    fun failedReloadKeepsStaleCacheUsable() {
        kotlinx.coroutines.runBlocking {
            AnnouncementApi.clearCacheForTest()
            var fail = false
            AnnouncementApi.documentLoader = {
                if (fail) AnnouncementApi.Result.Failure("网络炸了")
                else AnnouncementApi.Result.Success(doc(item("cached")))
            }
            AnnouncementApi.fetchPage()
            fail = true

            // 缓存已有条目：直接命中，不碰（会失败的）下载器
            val cached = AnnouncementApi.fetchDetail("cached")
            assertTrue(cached is AnnouncementApi.Result.Success)

            // 缓存没有的条目：触发重新下载 → 失败原因原样返回
            val miss = AnnouncementApi.fetchDetail("ghost")
            assertTrue(miss is AnnouncementApi.Result.Failure)
            assertEquals("网络炸了", (miss as AnnouncementApi.Result.Failure).message)

            AnnouncementApi.clearCacheForTest()
            AnnouncementApi.documentLoader = { AnnouncementApi.fetchDocument() }
        }
    }
}
