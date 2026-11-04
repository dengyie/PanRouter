package com.yunx.app.data.panrouter

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 同步 payload 预算锁定测试（服务端 §7.6：单条 16KB，超限 = 整批 400）。
 * 覆盖：字节级截断的多字节安全、失控字段就地收敛、JSON 转义膨胀路径、round-trip 可解析。
 */
class PanRouterSyncPayloadTest {

    // ---- utf8Truncate：按字节预算截断，不切断多字节字符 ----

    @Test
    fun `utf8Truncate respects byte budget without splitting multibyte chars`() {
        assertEquals("", "中文".utf8Truncate(0))
        assertEquals("", "中文".utf8Truncate(-1))
        assertEquals("中", "中".utf8Truncate(3))      // 恰好装下（3 字节）
        assertEquals("中", "中文".utf8Truncate(3))    // 第二个字装不下 → 整个丢
        assertEquals("中", "中文".utf8Truncate(4))    // 预算内只剩一个完整字
        assertEquals("ab", "ab中".utf8Truncate(3))    // a+b+中 超 3 字节 → 只留 ASCII
        assertEquals("abc", "abcdef".utf8Truncate(3)) // ASCII 精确截到预算（不少截）
        assertEquals("a", "a中".utf8Truncate(3))      // 中 需 3 字节，预算装不下
        // 边界续字节属于完整字符：必须保留「a中」（4 字节），不能回退误判成只留「a」
        assertEquals("a中", "a中文".utf8Truncate(4))
        assertEquals("a中", "a中文".utf8Truncate(6))
    }

    @Test
    fun `utf8Truncate keeps emoji surrogate sequences whole`() {
        val emoji = "👍" // 4 字节 UTF-8
        assertEquals("", emoji.utf8Truncate(3))
        assertEquals(emoji, emoji.utf8Truncate(4))
        assertEquals("a", "a$emoji".utf8Truncate(4))
    }

    // ---- payload 预算收敛 ----

    @Test
    fun `oversize bookmark payload is clamped under server budget`() {
        val p = SyncBookmarkPayload(
            link = "https://pan.quark.cn/s/" + "a".repeat(20_000),
            title = "标".repeat(20_000),
            platform = "QUARK", pwd = "x", category = "分类".repeat(500), createdAt = 1
        ).toJson()
        val bytes = p.toString().toByteArray(Charsets.UTF_8)
        assertTrue("expected < $SYNC_PAYLOAD_MAX_BYTES, got ${bytes.size}", bytes.size < SYNC_PAYLOAD_MAX_BYTES)
        // round-trip：收敛后仍是合法契约结构，且数据本体（link）保住了前缀
        val back = SyncBookmarkPayload.parse(p)
        assertTrue(back.link.startsWith("https://pan.quark.cn/s/"))
        assertTrue(back.title.isNotEmpty())
        assertEquals("QUARK", back.platform)
    }

    @Test
    fun `escape bloat is halved until within budget`() {
        // 控制字符在 JSON 序列化时膨胀成 \u0001（1 字节 → 6 字节），按 String.length 判断会漏判
        val p = SyncLinkHistoryPayload(
            url = "https://x/\u0001".repeat(2000),
            title = "\u0002".repeat(4000),
            platform = "QUARK", pwd = "", createdAt = 1
        ).toJson()
        val bytes = p.toString().toByteArray(Charsets.UTF_8)
        assertTrue("expected < $SYNC_PAYLOAD_MAX_BYTES, got ${bytes.size}", bytes.size < SYNC_PAYLOAD_MAX_BYTES)
        val back = SyncLinkHistoryPayload.parse(p)
        assertTrue(back.url.startsWith("https://x/"))
    }

    @Test
    fun `download record payload clamps error text and stays parseable`() {
        val p = SyncDownloadRecordPayload(
            shareUrl = "https://pan.quark.cn/s/x", platform = "QUARK",
            fileName = "f.zip", totalSize = 123, status = 5,
            error = "E".repeat(50_000), createdAt = 1
        ).toJson()
        val bytes = p.toString().toByteArray(Charsets.UTF_8)
        assertTrue("expected < $SYNC_PAYLOAD_MAX_BYTES, got ${bytes.size}", bytes.size < SYNC_PAYLOAD_MAX_BYTES)
        val back = SyncDownloadRecordPayload.parse(p)
        assertEquals(2048, back.error.length) // CAP_ERROR 上限
        assertEquals(123L, back.totalSize)
        assertEquals(5, back.status)
    }

    @Test
    fun `normal payload passes through untouched`() {
        val p = SyncBookmarkPayload(
            link = "https://pan.quark.cn/s/abc", title = "普通收藏",
            platform = "QUARK", pwd = "pwd", category = "视频", createdAt = 42
        ).toJson()
        assertEquals("https://pan.quark.cn/s/abc", p.optString("link"))
        assertEquals("普通收藏", p.optString("title"))
        assertEquals("pwd", p.optString("pwd"))
        assertEquals(42L, p.optLong("created_at"))
    }
}
