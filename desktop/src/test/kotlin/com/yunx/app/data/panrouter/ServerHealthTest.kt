package com.yunx.app.data.panrouter

import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue

/**
 * 健康状态机测试（规格 §8.1）：纯状态迁移，不依赖真实时间。
 */
class ServerHealthTest {

    @Test
    fun `starts up with zero failures`() {
        val h = ServerHealth()
        assertTrue(h.up)
        assertEquals(ServerHealth.State(up = true, failureStreak = 0), h.currentState())
    }

    @Test
    fun `hard failure turns down and backoff follows sequence capped at 10 minutes`() {
        val h = ServerHealth()
        // 5 次连续失败：退避 30s → 1m → 2m → 5m → 10m
        val expected = longArrayOf(30_000L, 60_000L, 120_000L, 300_000L, 600_000L)
        expected.forEachIndexed { i, ms ->
            h.markHardFailure()
            assertFalse("failure #$i", h.up)
            assertEquals("backoff #$i", ms, h.nextProbeDelayMs())
        }
        // 第 6 次起封顶 10m
        h.markHardFailure()
        assertEquals(600_000L, h.nextProbeDelayMs())
    }

    @Test
    fun `success resets to up and zero backoff`() {
        val h = ServerHealth()
        h.markHardFailure()
        h.markHardFailure()
        assertFalse(h.up)
        h.markSuccess()
        assertTrue(h.up)
        assertEquals(0L, h.nextProbeDelayMs())
        assertEquals(ServerHealth.State(up = true, failureStreak = 0), h.currentState())
    }
}
