package com.yunx.app.util

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 渲染后端回退策略（[RenderApiPolicy]）纯逻辑单测。
 * 实机行为（JNA 采样 / 注册表 / 自重启）由 GUI 验收覆盖，这里只测状态机。
 */
class RenderApiPolicyTest {

    private val fresh = RenderApiPolicy.State()

    // ---------- resolve：优先级 ----------

    @Test
    fun envOverridesEverythingAndDisablesWatchdog() {
        val d = RenderApiPolicy.resolve(
            env = "SOFTWARE",
            state = RenderApiPolicy.State(override = "SOFTWARE", failures = 2),
            gpuSigChanged = false,
            chooserApi = RenderApiPolicy.OPENGL
        )
        assertEquals("SOFTWARE", d.api)
        assertEquals(RenderApiPolicy.Source.ENV, d.source)
        assertFalse(d.watchdog)
        assertFalse(d.resetState)
    }

    @Test
    fun blankEnvFallsThroughToOverride() {
        val d = RenderApiPolicy.resolve(" ", fresh, gpuSigChanged = false, chooserApi = RenderApiPolicy.OPENGL)
        assertEquals(RenderApiPolicy.OPENGL, d.api)
        assertTrue(d.watchdog)
    }

    // ---------- resolve：长期回退与显卡签名 ----------

    @Test
    fun persistedOverrideWinsWhenSignatureMatches() {
        val state = RenderApiPolicy.State(override = RenderApiPolicy.SOFTWARE, failures = 2, gpuSig = "sig1")
        val d = RenderApiPolicy.resolve(null, state, gpuSigChanged = false, chooserApi = RenderApiPolicy.OPENGL)
        assertEquals(RenderApiPolicy.SOFTWARE, d.api)
        assertEquals(RenderApiPolicy.Source.OVERRIDE, d.source)
        assertFalse(d.watchdog)
        assertFalse(d.resetState)
    }

    @Test
    fun signatureChangeInvalidatesOverrideAndGivesOpenGlAnotherChance() {
        val state = RenderApiPolicy.State(override = RenderApiPolicy.SOFTWARE, failures = 2, gpuSig = "old")
        val d = RenderApiPolicy.resolve(null, state, gpuSigChanged = true, chooserApi = RenderApiPolicy.OPENGL)
        assertEquals(RenderApiPolicy.OPENGL, d.api)
        assertTrue(d.watchdog)
        assertTrue(d.resetState)
    }

    @Test
    fun chooserSoftwareIsWatchdogFree() {
        val d = RenderApiPolicy.resolve(null, fresh, gpuSigChanged = false, chooserApi = RenderApiPolicy.SOFTWARE)
        assertEquals(RenderApiPolicy.SOFTWARE, d.api)
        assertFalse(d.watchdog)
    }

    // ---------- onWhiteScreen：一次重试、两次长期回退 ----------

    @Test
    fun firstFailureRetriesOpenGlWithoutOverride() {
        val (state, next) = RenderApiPolicy.onWhiteScreen(fresh, gpuSig = "sig", nowMs = 1_000L, canRelaunch = true)
        assertEquals(RenderApiPolicy.OPENGL, next)
        assertEquals(1, state.failures)
        assertEquals("sig", state.gpuSig)
        assertNull(state.override)
        assertEquals(1_000L, state.lastRelaunchMs)
    }

    @Test
    fun secondFailurePersistsSoftwareOverride() {
        val afterFirst = RenderApiPolicy.State(failures = 1, gpuSig = "sig", lastRelaunchMs = 1_000L)
        val (state, next) = RenderApiPolicy.onWhiteScreen(afterFirst, gpuSig = "sig", nowMs = 200_000L, canRelaunch = true)
        assertEquals(RenderApiPolicy.SOFTWARE, next)
        assertEquals(2, state.failures)
        assertEquals(RenderApiPolicy.SOFTWARE, state.override)
        assertEquals("sig", state.gpuSig)
        assertEquals(200_000L, state.lastRelaunchMs)
    }

    @Test
    fun debouncedFailureStillBooksFailureButDoesNotRestartOrMoveDebounceWindow() {
        // E2E 实测场景：第二次白屏落在 120s 防抖窗口内 —— 记账推进（failures=2、长期 override 写入），
        // 但不重启、也不顺延防抖窗口（lastRelaunchMs 保持上次实际重启时刻）
        val afterFirst = RenderApiPolicy.State(failures = 1, gpuSig = "sig", lastRelaunchMs = 1_000L)
        val (state, next) = RenderApiPolicy.onWhiteScreen(
            afterFirst, gpuSig = "sig", nowMs = 1_000L + 28_000L, canRelaunch = false
        )
        assertNull(next)
        assertEquals(2, state.failures)
        assertEquals(RenderApiPolicy.SOFTWARE, state.override)
        assertEquals(1_000L, state.lastRelaunchMs)
    }

    // ---------- canRelaunch：防重启循环 ----------

    @Test
    fun relaunchGuardBlocksRapidSuccessiveRelaunches() {
        val state = RenderApiPolicy.State(lastRelaunchMs = 100_000L)
        assertFalse(RenderApiPolicy.canRelaunch(state, nowMs = 100_000L + 60_000L))
        assertTrue(RenderApiPolicy.canRelaunch(state, nowMs = 100_000L + RenderApiPolicy.MIN_RELAUNCH_INTERVAL_MS))
    }

    @Test
    fun neverRelaunchedBeforeMeansCanRelaunch() {
        assertTrue(RenderApiPolicy.canRelaunch(fresh, nowMs = 0L))
    }

    // ---------- onHealthy：健康会话清零失败计数 ----------

    @Test
    fun healthySessionResetsFailureCounter() {
        val state = RenderApiPolicy.State(failures = 1, gpuSig = "sig")
        val cleared = RenderApiPolicy.onHealthy(state)
        assertEquals(0, cleared.failures)
        assertEquals("", cleared.gpuSig)
    }

    @Test
    fun healthySessionKeepsLongTermOverride() {
        val state = RenderApiPolicy.State(override = RenderApiPolicy.SOFTWARE, failures = 2, gpuSig = "sig")
        val cleared = RenderApiPolicy.onHealthy(state)
        assertEquals(RenderApiPolicy.SOFTWARE, cleared.override)
    }

    // ---------- isUniformBlank：像素桶判定 ----------

    @Test
    fun pureWhiteSamplesAreBlank() {
        // 白屏：0x00FFFFFF（GetPixel 的 COLORREF 布局）
        val samples = List(300) { 0xFFFFFF }
        assertTrue(RenderApiPolicy.isUniformBlank(samples))
    }

    @Test
    fun pureBlackSamplesAreBlank() {
        val samples = List(300) { 0x000000 }
        assertTrue(RenderApiPolicy.isUniformBlank(samples))
    }

    @Test
    fun nearlyUniformWithTinyNoiseStillBlank() {
        val samples = List(300) { 0xFFFFFF } + List(5) { 0xF0F0F0 }
        assertTrue(RenderApiPolicy.isUniformBlank(samples))
    }

    @Test
    fun realUiPixelMixIsNotBlank() {
        // 深色主题粗略混合：背景/面板/文字/强调色
        val samples = buildList {
            repeat(120) { add(0x141414) } // 背景
            repeat(80) { add(0x1E1E22) } // 面板
            repeat(60) { add(0xD0D0D5) } // 文字
            repeat(40) { add(0x3D5AFE) } // 强调色
        }
        assertFalse(RenderApiPolicy.isUniformBlank(samples))
    }

    @Test
    fun twoBigBucketsWithinToleranceCountsAsBlank() {
        // 99% 单桶 → 空白；两桶各半 → 有内容
        val half = List(150) { 0x101010 } + List(150) { 0xE0E0E0 }
        assertFalse(RenderApiPolicy.isUniformBlank(half))
    }

    @Test
    fun emptySamplesNeverBlank() {
        assertFalse(RenderApiPolicy.isUniformBlank(emptyList()))
    }
}
