package com.yunx.app.data.panrouter

import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.assertTrue

/**
 * 错误契约测试：kind 字符串解析、HTTP 状态兜底、回落判定（规格 §5.2 表全格）。
 */
class PanRouterErrorTest {

    // ---------- kind 字符串 → 枚举 ----------

    @Test
    fun `kind strings map to enums`() {
        val expected = mapOf(
            "session_expired" to PanRouterError.Kind.SESSION_EXPIRED,
            "auth_invalid" to PanRouterError.Kind.AUTH_INVALID,
            "auth_expired" to PanRouterError.Kind.AUTH_EXPIRED,
            "risk_control" to PanRouterError.Kind.RISK_CONTROL,
            "share_gone" to PanRouterError.Kind.SHARE_GONE,
            "not_found" to PanRouterError.Kind.NOT_FOUND,
            "interface_changed" to PanRouterError.Kind.INTERFACE_CHANGED,
            "upstream_error" to PanRouterError.Kind.UPSTREAM_ERROR,
            "unsupported" to PanRouterError.Kind.UNSUPPORTED
        )
        expected.forEach { (raw, kind) -> assertEquals("kind $raw", kind, PanRouterError.kindOf(raw)) }
    }

    @Test
    fun `unknown kind falls back to upstream`() {
        assertEquals(PanRouterError.Kind.UPSTREAM_ERROR, PanRouterError.kindOf("something_new"))
        assertEquals(PanRouterError.Kind.UPSTREAM_ERROR, PanRouterError.kindOf(""))
    }

    // ---------- HTTP 状态码兜底（无错误体时对齐服务端 kindStatus 表） ----------

    @Test
    fun `default kinds by status`() {
        assertEquals(PanRouterError.Kind.SESSION_EXPIRED, PanRouterError.defaultKindFor(401))
        assertEquals(PanRouterError.Kind.RISK_CONTROL, PanRouterError.defaultKindFor(429))
        assertEquals(PanRouterError.Kind.NOT_FOUND, PanRouterError.defaultKindFor(404))
        assertEquals(PanRouterError.Kind.UNSUPPORTED, PanRouterError.defaultKindFor(400))
        assertEquals(PanRouterError.Kind.UPSTREAM_ERROR, PanRouterError.defaultKindFor(502))
        assertEquals(PanRouterError.Kind.UPSTREAM_ERROR, PanRouterError.defaultKindFor(500))
    }

    @Test
    fun `fromHttp parses error body and keeps cause chain fields`() {
        val e = PanRouterError.fromHttp(
            401,
            """{"code":401,"kind":"session_expired","message":"登录已过期","retriable":false}"""
        )
        assertEquals(PanRouterError.Kind.SESSION_EXPIRED, e.kind)
        assertEquals(401, e.httpStatus)
        assertEquals("登录已过期", e.message)
        assertFalse(e.retriable)
    }

    @Test
    fun `fromHttp with malformed body falls back to status kind`() {
        val e = PanRouterError.fromHttp(502, "<html>bad gateway</html>")
        assertEquals(PanRouterError.Kind.UPSTREAM_ERROR, e.kind)
        assertTrue(e.retriable)
        assertEquals("HTTP 502", e.message)
    }

    // ---------- 回落判定（规格 §5.2 表） ----------

    @Test
    fun `risk control unsupported upstream interface network fall back to local`() {
        listOf(
            PanRouterError.Kind.RISK_CONTROL,
            PanRouterError.Kind.UNSUPPORTED,
            PanRouterError.Kind.UPSTREAM_ERROR,
            PanRouterError.Kind.INTERFACE_CHANGED,
            PanRouterError.Kind.NETWORK
        ).forEach { kind ->
            val e = PanRouterError("x", kind, retriable = true)
            assertTrue(kind.name, e.shouldFallbackToLocal)
        }
    }

    @Test
    fun `share gone not found and auth kinds do not fall back`() {
        listOf(
            PanRouterError.Kind.SHARE_GONE,
            PanRouterError.Kind.NOT_FOUND,
            PanRouterError.Kind.SESSION_EXPIRED,
            PanRouterError.Kind.AUTH_INVALID,
            PanRouterError.Kind.AUTH_EXPIRED
        ).forEach { kind ->
            val e = PanRouterError("x", kind, retriable = false)
            assertFalse(kind.name, e.shouldFallbackToLocal)
        }
    }

    // ---------- 网络失败包装 ----------

    @Test
    fun `network failure is retriable with kind network`() {
        val e = PanRouterError.fromNetwork(java.net.ConnectException("connection refused"))
        assertEquals(PanRouterError.Kind.NETWORK, e.kind)
        assertTrue(e.retriable)
        assertEquals(0, e.httpStatus)
        assertTrue(e.cause is java.net.ConnectException)
    }
}
