package com.yunx.app.data.panrouter

import com.yunx.app.data.download.DownloadPlatform
import com.yunx.app.data.network.SharePlatform
import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull

/**
 * 择路决策器测试（规格 §3.2 决策表全格 + 平台映射）。
 * 纯函数，无环境依赖。
 */
class ResolveRouterTest {

    // ---------- 决策表 ----------

    @Test
    fun `enabled up covered routes remote`() {
        assertEquals(
            ResolveRouter.Route.REMOTE,
            ResolveRouter.decide(SharePlatform.QUARK, enabled = true, serverUp = true, covered = true)
        )
    }

    @Test
    fun `switch off routes local`() {
        assertEquals(
            ResolveRouter.Route.LOCAL,
            ResolveRouter.decide(SharePlatform.QUARK, enabled = false, serverUp = true, covered = true)
        )
    }

    @Test
    fun `server down routes local`() {
        assertEquals(
            ResolveRouter.Route.LOCAL,
            ResolveRouter.decide(SharePlatform.QUARK, enabled = true, serverUp = false, covered = true)
        )
    }

    @Test
    fun `platform not covered routes local`() {
        assertEquals(
            ResolveRouter.Route.LOCAL,
            ResolveRouter.decide(SharePlatform.UC, enabled = true, serverUp = true, covered = false)
        )
    }

    // ---------- 服务端覆盖集（M1：quark / lanzou） ----------

    @Test
    fun `server coverage is quark and lanzou only`() {
        assertEquals("quark", ResolveRouter.serverPanFor(SharePlatform.QUARK))
        assertEquals("lanzou", ResolveRouter.serverPanFor(SharePlatform.LANZOU))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.UC))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.XUNLEI))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.BAIDU))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.C139))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.PAN123))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.PAN115))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.GUANGYA))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.ILANZOU))
        assertNull(ResolveRouter.serverPanFor(SharePlatform.GITHUB))
    }

    // ---------- 下载平台映射（线程数设置按平台复用） ----------

    @Test
    fun `pan maps to download platform`() {
        assertEquals(DownloadPlatform.QUARK, ResolveRouter.downloadPlatformFor("quark"))
        assertEquals(DownloadPlatform.LANZOU, ResolveRouter.downloadPlatformFor("lanzou"))
        assertEquals(DownloadPlatform.GENERIC, ResolveRouter.downloadPlatformFor("unknown_pan"))
    }
}
