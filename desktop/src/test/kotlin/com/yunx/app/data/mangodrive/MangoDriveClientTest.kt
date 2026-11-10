package com.yunx.app.data.mangodrive

import com.yunx.app.data.panrouter.RemoteLinkResult
import com.yunx.app.data.panrouter.PanRouterClient
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.After
import org.junit.Before
import org.junit.Test

class MangoDriveClientTest {

    private lateinit var server: MockWebServer

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    private fun client(settings: MangoDriveSettings = MangoDriveSettings()): MangoDriveClient =
        MangoDriveClient(
            settings = settings,
            routerClient = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = { server.url("/").toString().trimEnd('/') },
                tokenProvider = { "test-token" }
            )
        )

    /** 服务端 `GET /api/v1/storage` 样本：端点基址由服务端下发，客户端不内置 */
    private val storageSample = """
        {
          "code": 200, "driver": "mangodrive", "enabled": true,
          "public_base": "https://drive.example.com",
          "dest_dir": "/pool/inbox", "max_bytes": 68719476736
        }
    """.trimIndent()

    /** 让客户端先拿到端点基址，再执行被断言的那次调用 */
    private fun enqueueStorageThen(body: String) {
        server.enqueue(MockResponse().setBody(storageSample))
        server.enqueue(MockResponse().setBody(body))
    }

    private val listSample = """
        {
          "pan": "mangodrive",
          "share_key": "abc123",
          "files": [
            {"fid": "/dir/a.mkv", "name": "a.mkv", "size": 2048, "is_dir": false,
             "route": "aria2", "download_url": "https://pan.example.com/d/mangodrive/abc123//dir/a.mkv?sig=1"},
            {"fid": "/dir/sub", "name": "sub", "size": 0, "is_dir": true}
          ]
        }
    """.trimIndent()

    private val linkSample = """
        {
          "pan": "mangodrive", "share_key": "abc123", "fid": "/dir/a.mkv",
          "file_name": "a.mkv", "size": 2048, "route": "aria2",
          "download_url": "https://pan.example.com/d/mangodrive/abc123//dir/a.mkv?sig=1",
          "stream_url": "https://pan.example.com/stream/mangodrive/abc123//dir/a.mkv?sig=2",
          "direct_url": "https://drive.example.com/p/dir/a.mkv?sign=x:0",
          "ua": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/126",
          "referer": "https://drive.example.com/",
          "need_headers": true,
          "expires_at": "2026-11-08T12:00:00+08:00",
          "cache_hit": true
        }
    """.trimIndent()

    @Test
    fun `listDir asks server for endpoint then posts public url`() = runBlocking {
        enqueueStorageThen(listSample)
        val result = client().listDir("/dir").getOrThrow()

        val storage = server.takeRequest()
        assertEquals("GET", storage.method)
        assertEquals("/api/v1/storage", storage.path)

        val recorded = server.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("Bearer test-token", recorded.getHeader("Authorization"))
        val body = recorded.body.readUtf8()
        assertTrue(body, body.contains("\"url\":\"https://drive.example.com/dir\""))

        assertEquals("mangodrive", result.pan)
        assertEquals(2, result.files.size)
        assertEquals("/dir/a.mkv", result.files[0].fid)
        assertEquals("a.mkv", result.files[0].name)
        assertEquals(2048L, result.files[0].size)
        assertEquals(false, result.files[0].isDir)
        assertTrue(result.files[1].isDir)
    }

    @Test
    fun `explicit settings base skips the storage lookup`() = runBlocking {
        val prefs = java.util.prefs.Preferences.userRoot()
            .node("yunx/test-mangodrive-" + System.nanoTime())
        try {
            val settings = MangoDriveSettings(prefs).apply { publicBaseUrl = "https://override.example.org" }
            server.enqueue(MockResponse().setBody(listSample))
            client(settings).listDir("/dir").getOrThrow()

            val only = server.takeRequest()
            assertEquals("POST", only.method)
            assertTrue(only.body.readUtf8().contains("\"url\":\"https://override.example.org/dir\""))
        } finally {
            prefs.removeNode()
        }
    }

    @Test
    fun `endpoint disabled surfaces an unsupported error`() = runBlocking {
        server.enqueue(
            MockResponse().setBody("""{"code":200,"driver":"mangodrive","enabled":false}""")
        )
        val result = client().listDir("/dir")
        assertTrue(result.isFailure)
        val err = result.exceptionOrNull() as com.yunx.app.data.panrouter.PanRouterError
        assertEquals(com.yunx.app.data.panrouter.PanRouterError.Kind.UNSUPPORTED, err.kind)
    }

    @Test
    fun `listDir encodes non-ascii and spaces per segment`() = runBlocking {
        enqueueStorageThen(listSample)
        client().listDir("/目录/带 空格").getOrThrow()
        server.takeRequest() // storage
        val body = server.takeRequest().body.readUtf8()
        assertTrue(body, body.contains("%E7%9B%AE%E5%BD%95"))
        assertTrue(body, body.contains("%E5%B8%A6%20%E7%A9%BA%E6%A0%BC"))
    }

    @Test
    fun `link posts dir path and fid`() = runBlocking {
        enqueueStorageThen(linkSample)
        val link = client().link("/dir", "/dir/a.mkv").getOrThrow()

        server.takeRequest() // storage
        val body = server.takeRequest().body.readUtf8()
        assertTrue(body, body.contains("\"url\":\"https://drive.example.com/dir\""))
        assertTrue(body, body.contains("\"fid\":\"/dir/a.mkv\""))

        assertEquals("/dir/a.mkv", link.fid)
        assertEquals("a.mkv", link.fileName)
        assertEquals(2048L, link.size)
        assertEquals("https://drive.example.com/p/dir/a.mkv?sign=x:0", link.directUrl)
        assertTrue(link.needHeaders)
    }

    @Test
    fun `encodePath keeps root and separators`() {
        assertEquals("/", MangoDriveClient.encodePath(""))
        assertEquals("/", MangoDriveClient.encodePath("/"))
        assertEquals("/a/b", MangoDriveClient.encodePath("/a/b/"))
        assertEquals("/a/%20b", MangoDriveClient.encodePath("/a/ b"))
    }

    @Test
    fun `downloadUrlOf prefers direct_url then falls back to route`() {
        val direct = RemoteLinkResult(
            pan = "mangodrive", shareKey = "k", fid = "/f", fileName = "f", size = 1,
            route = "aria2",
            downloadUrl = "https://pan.example.com/d/x", streamUrl = "https://pan.example.com/stream/x",
            directUrl = "https://drive.example.com/p/f?sign=1", ua = "UA", referer = "",
            needHeaders = true, expiresAtMs = 0, cacheHit = false
        )
        assertEquals("https://drive.example.com/p/f?sign=1", com.yunx.app.ui.viewmodel.MangoCloudViewModel.downloadUrlOf(direct))

        val streamOnly = direct.copy(route = "stream", directUrl = null)
        assertEquals("https://pan.example.com/stream/x", com.yunx.app.ui.viewmodel.MangoCloudViewModel.downloadUrlOf(streamOnly))
    }

    /**
     * isHashFid 契约："/" 开头 = 路径形态；其余（"p:<sha1>" 哈希 / 空串）= 哈希形态。
     * v1.22 起客户端提链不再按形态分流（一律携带真实目录——历史反查兜底只覆盖
     * 每目录 auto-link 前 8 个，生产实测 9+ 必 404）；本测试锁定形态判定本身，
     * 与服务端 `mangodrive.FidHashPrefix` 门控共用同一契约。
     */
    @Test
    fun `isHashFid distinguishes path-form and hash-form fids`() {
        val client = client()
        assertEquals(false, client.isHashFid("/Pool-Secure/浅层路径.bin"))
        assertEquals(false, client.isHashFid("/"))
        assertEquals(true, client.isHashFid("p:516a5cf51d429a145632dfb29cfeb35f28e92ac0"))
        // 空串不是路径形态：按哈希处理走服务端反查，服务端会干净报错而不是把空串当路径
        assertEquals(true, client.isHashFid(""))
    }
}
