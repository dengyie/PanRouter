package com.yunx.app.data.panrouter

import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Before
import org.junit.Test
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue

/**
 * PanRouterClient 契约测试：MockWebServer 回放服务端响应样本
 * （协议字段手工誊抄自 panrouter/docs/development.md §6/§8，两仓零共享代码）。
 * 服务端契约变更时：先改这里的样本（按 development.md），再改解析代码。
 */
class PanRouterClientTest {

    private lateinit var server: MockWebServer
    private lateinit var client: PanRouterClient

    private val resolveSample = """
        {
          "pan": "quark",
          "share_key": "abc123def456",
          "files": [
            {"fid": "f1", "name": "剧集/第01集.mkv", "size": 1048576, "is_dir": false,
             "route": "302", "download_url": "https://dl.quark.cn/f1?sig=x"},
            {"fid": "f2", "name": "big.iso", "size": 999999999, "is_dir": false,
             "link_error": "需要登录态"}
          ],
          "hint": "仅自动提链 1 个文件",
          "truncated": true
        }
    """.trimIndent()

    private val linkSample = """
        {
          "pan": "quark", "share_key": "abc123def456", "fid": "f1",
          "file_name": "剧集/第01集.mkv", "size": 1048576,
          "route": "stream",
          "download_url": "https://pan.example.com/d/quark/abc123def456/f1?sig=aaa",
          "stream_url": "https://pan.example.com/stream/quark/abc123def456/f1?sig=bbb",
          "ua": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
          "referer": "https://pan.quark.cn/",
          "need_headers": true,
          "expires_at": "2026-11-01T12:00:00+08:00",
          "cache_hit": false
        }
    """.trimIndent()

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
        client = PanRouterClient(
            http = OkHttpClient(),
            baseUrlProvider = { server.url("/").toString().trimEnd('/') },
            tokenProvider = { "test-token" }
        )
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    // ---------- healthz ----------

    @Test
    fun `health parses status and version`() = runBlocking {
        server.enqueue(MockResponse().setBody("""{"status":"ok","version":"1ad5d0974f8a"}"""))
        val info = client.health().getOrThrow()
        assertEquals("ok", info.status)
        assertEquals("1ad5d0974f8a", info.version)
        val path = server.takeRequest().path
        assertEquals("/healthz", path)
    }

    // ---------- resolve（分享列表） ----------

    @Test
    fun `resolveShare parses files hint and truncated`() = runBlocking {
        server.enqueue(MockResponse().setBody(resolveSample))
        val result = client.resolveShare("https://pan.quark.cn/s/abc", pwd = null).getOrThrow()
        assertEquals("quark", result.pan)
        assertEquals("abc123def456", result.shareKey)
        assertEquals(2, result.files.size)
        assertTrue(result.truncated)
        assertEquals("仅自动提链 1 个文件", result.hint)
        val first = result.files.first()
        assertEquals("f1", first.fid)
        assertEquals("剧集/第01集.mkv", first.name)
        assertEquals(1048576L, first.size)
        assertEquals(false, first.isDir)
        assertEquals("302", first.route)
        assertEquals("https://dl.quark.cn/f1?sig=x", first.downloadUrl)
        assertNull(result.files[1].downloadUrl)
        assertEquals("需要登录态", result.files[1].linkError)
    }

    @Test
    fun `resolveShare posts url and pwd and bearer token`() = runBlocking {
        server.enqueue(MockResponse().setBody(resolveSample))
        client.resolveShare("https://pan.quark.cn/s/abc", pwd = "8k3d").getOrThrow()
        val recorded = server.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("Bearer test-token", recorded.getHeader("Authorization"))
        val body = recorded.body.readUtf8()
        assertTrue(body, body.contains("\"url\":\"https://pan.quark.cn/s/abc\""))
        assertTrue(body, body.contains("\"pwd\":\"8k3d\""))
    }

    @Test
    fun `resolveShare omits pwd and token when absent`() = runBlocking {
        val anon = PanRouterClient(
            http = OkHttpClient(),
            baseUrlProvider = { server.url("/").toString().trimEnd('/') },
            tokenProvider = { null }
        )
        server.enqueue(MockResponse().setBody(resolveSample))
        anon.resolveShare("https://pan.quark.cn/s/abc", pwd = null).getOrThrow()
        val recorded = server.takeRequest()
        assertNull(recorded.getHeader("Authorization"))
        assertTrue(!recorded.body.readUtf8().contains("pwd"))
    }

    @Test
    fun `resolveLink posts fid and parses link result`() = runBlocking {
        server.enqueue(MockResponse().setBody(linkSample))
        val link = client.resolveLink(
            "https://pan.quark.cn/s/abc",
            pwd = "8k3d",
            fid = "f1"
        ).getOrThrow()
        assertEquals("stream", link.route)
        assertEquals("https://pan.example.com/stream/quark/abc123def456/f1?sig=bbb", link.streamUrl)
        assertEquals("Mozilla/5.0 (Windows NT 10.0; Win64; x64)", link.ua)
        assertEquals("https://pan.quark.cn/", link.referer)
        assertTrue(link.needHeaders)
        assertTrue(link.isRelayRoute)
        assertTrue("expires_at 应解析为 epoch 毫秒", link.expiresAtMs > 0L)
        val recorded = server.takeRequest()
        val containsFid = recorded.body.readUtf8().contains("\"fid\":\"f1\"")
        assertTrue("request body should contain fid", containsFid)
    }

    // ---------- 错误契约 ----------

    @Test
    fun `http 401 with error body maps to session_expired`() = runBlocking {
        server.enqueue(
            MockResponse().setResponseCode(401)
                .setBody("""{"code":401,"kind":"session_expired","message":"登录已过期","retriable":false}""")
        )
        val e = client.resolveShare("https://pan.quark.cn/s/abc", pwd = null).exceptionOrNull()
        assertTrue("expected PanRouterError but got ${e?.javaClass}", e is PanRouterError)
        val err = e as PanRouterError
        assertEquals(PanRouterError.Kind.SESSION_EXPIRED, err.kind)
        assertEquals(401, err.httpStatus)
        assertEquals("登录已过期", err.message)
    }

    @Test
    fun `http 400 unsupported maps to unsupported kind`() = runBlocking {
        server.enqueue(
            MockResponse().setResponseCode(400)
                .setBody("""{"code":400,"kind":"unsupported","message":"暂不支持该网盘","retriable":false}""")
        )
        val e = client.resolveShare("https://pan.baidu.com/s/1abc", pwd = null).exceptionOrNull()
        assertTrue("expected PanRouterError but got ${e?.javaClass}", e is PanRouterError)
        assertEquals(PanRouterError.Kind.UNSUPPORTED, (e as PanRouterError).kind)
    }

    @Test
    fun `connection refused maps to network error`() = runBlocking {
        val dead = PanRouterClient(
            http = OkHttpClient(),
            baseUrlProvider = { "http://127.0.0.1:1" }
        )
        val e = dead.health().exceptionOrNull()
        assertTrue("expected PanRouterError but got ${e?.javaClass}", e is PanRouterError)
        val ne = e as PanRouterError
        assertEquals(PanRouterError.Kind.NETWORK, ne.kind)
        assertTrue(ne.retriable)
    }

    // ---------- 账号池（M2 账号贡献） ----------

    @Test
    fun `createAccount posts pan_type name cookie and parses view`() = runBlocking {
        server.enqueue(
            MockResponse().setBody(
                """{"account":{"id":7,"pan_type":"quark","name":"mango","status":"ok","cred_version":1,"cooldown_until":null,"last_check_at":null}}"""
            )
        )
        val acc = client.createAccount("quark", "mango", "cookie-value").getOrThrow()
        assertEquals(7L, acc.id)
        assertEquals("quark", acc.panType)
        assertEquals("mango", acc.name)
        assertEquals("ok", acc.status)
        val recorded = server.takeRequest()
        assertEquals("/api/v1/accounts", recorded.path)
        assertEquals("Bearer test-token", recorded.getHeader("Authorization"))
        val body = recorded.body.readUtf8()
        assertTrue(body, body.contains("\"pan_type\":\"quark\""))
        assertTrue(body, body.contains("\"cookie\":\"cookie-value\""))
    }

    @Test
    fun `createAccount invalid credential maps to auth_expired`() = runBlocking {
        server.enqueue(
            MockResponse().setResponseCode(401)
                .setBody("""{"code":401,"kind":"auth_expired","message":"Cookie 已失效","retriable":false}""")
        )
        val e = client.createAccount("quark", "mango", "dead-cookie").exceptionOrNull()
        assertTrue("expected PanRouterError but got ${e?.javaClass}", e is PanRouterError)
        assertEquals(PanRouterError.Kind.AUTH_EXPIRED, (e as PanRouterError).kind)
    }

    @Test
    fun `listAccounts parses array and passes pan filter`() = runBlocking {
        server.enqueue(
            MockResponse().setBody(
                """{"accounts":[{"id":1,"pan_type":"quark","name":"a","status":"ok"},
                    {"id":2,"pan_type":"lanzou","name":"b","status":"cooling"}]}"""
            )
        )
        val list = client.listAccounts("quark").getOrThrow()
        assertEquals(2, list.size)
        assertEquals("cooling", list[1].status)
        assertEquals("/api/v1/accounts?pan=quark", server.takeRequest().path)
    }

    // ---------- resolve/batch（M2 批量解析） ----------

    @Test
    fun `resolveBatch posts items array and parses partial success`() = runBlocking {
        server.enqueue(
            MockResponse().setBody(
                """{"results":[
                    {"index":0,"ok":true,"result":{"pan":"quark","share_key":"k1","files":[{"fid":"f1","name":"a.zip","size":10,"is_dir":false,"route":"302","download_url":"https://dl/x"}]}},
                    {"index":1,"ok":false,"error":"share gone: 分享已取消"}
                ]}"""
            )
        )
        val items = client.resolveBatch(
            listOf("https://pan.quark.cn/s/aaa" to "p1", "https://pan.quark.cn/s/bbb" to null)
        ).getOrThrow()
        assertEquals(2, items.size)
        assertTrue(items[0].ok)
        assertEquals("k1", items[0].result?.shareKey)
        assertEquals("a.zip", items[0].result?.files?.first()?.name)
        assertTrue(!items[1].ok)
        assertEquals("share gone: 分享已取消", items[1].error)
        val recorded = server.takeRequest()
        assertEquals("/api/v1/resolve/batch", recorded.path)
        val body = recorded.body.readUtf8()
        assertTrue(body, body.contains("\"url\":\"https://pan.quark.cn/s/aaa\""))
        assertTrue(body, body.contains("\"pwd\":\"p1\""))
        assertTrue(body, body.contains("\"url\":\"https://pan.quark.cn/s/bbb\""))
    }

    @Test
    fun `resolveBatch over limit is unsupported`() = runBlocking {
        server.enqueue(
            MockResponse().setResponseCode(400)
                .setBody("""{"code":400,"kind":"unsupported","message":"items 数量须在 1-50","retriable":false}""")
        )
        val e = client.resolveBatch(List(51) { "https://pan.quark.cn/s/x$it" to null }).exceptionOrNull()
        assertTrue("expected PanRouterError", e is PanRouterError)
        assertEquals(PanRouterError.Kind.UNSUPPORTED, (e as PanRouterError).kind)
    }
}
