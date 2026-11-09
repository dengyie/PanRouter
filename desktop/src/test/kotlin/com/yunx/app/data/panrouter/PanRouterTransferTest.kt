package com.yunx.app.data.panrouter

import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test

/**
 * M4/M4.3 云对云转存契约测试：MockWebServer 回放服务端 `POST/GET /api/v1/transfer*` 样本。
 *
 * 服务端字段来源 panrouter 的 `TransferTask`（json: id/state/channel/source_url/dest_path/
 * file_name/size/done/error/hint/started_at/ended_at）。两仓零共享代码，契约变更先改样本。
 */
class PanRouterTransferTest {

    private lateinit var server: MockWebServer
    private lateinit var client: PanRouterClient

    private val taskSample = """
        {
          "id": "t-42",
          "state": "running",
          "channel": "stream",
          "source_url": "https://pan.quark.cn/s/abc",
          "dest_path": "/pool/panrouter/out/movie.mkv",
          "file_name": "movie.mkv",
          "size": 104857600,
          "done": 26214400,
          "error": "",
          "hint": "",
          "started_at": "2026-11-08T10:00:00+08:00",
          "ended_at": ""
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

    @Test
    fun `submitTransfer posts request fields and parses accepted task`() = runBlocking {
        server.enqueue(
            MockResponse()
                .setResponseCode(202)
                .setBody("""{"code":0,"task":$taskSample}""")
        )

        val task = client.submitTransfer(
            TransferRequest(
                sourceUrl = "https://pan.quark.cn/s/abc",
                pwd = "9527",
                fid = "f1",
                destDir = "/pool/panrouter/out",
                name = "movie.mkv",
                channel = TransferChannel.STREAM,
                ua = "Mozilla/5.0"
            )
        ).getOrThrow()

        assertEquals("t-42", task.id)
        assertEquals(TransferTask.STATE_RUNNING, task.state)
        assertEquals("stream", task.channel)
        assertEquals("/pool/panrouter/out/movie.mkv", task.destPath)
        assertEquals(104857600L, task.size)
        assertEquals(26214400L, task.done)
        assertEquals(0.25f, task.progress)
        assertTrue(task.isRunning)
        assertFalse(task.isTerminal)
        assertTrue(task.startedAtMs > 0L)
        assertEquals(0L, task.endedAtMs)

        val req = server.takeRequest()
        assertEquals("/api/v1/transfer/mangodrive", req.path)
        assertEquals("POST", req.method)
        assertEquals("Bearer test-token", req.getHeader("Authorization"))
        val body = JSONObject(req.body.readUtf8())
        assertEquals("https://pan.quark.cn/s/abc", body.getString("source_url"))
        assertEquals("9527", body.getString("pwd"))
        assertEquals("f1", body.getString("fid"))
        assertEquals("/pool/panrouter/out", body.getString("dest_dir"))
        assertEquals("movie.mkv", body.getString("name"))
        assertEquals("stream", body.getString("channel"))
        assertEquals("Mozilla/5.0", body.getString("ua"))
    }

    @Test
    fun `submitTransfer omits blank optional fields`() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(202).setBody("""{"task":$taskSample}"""))

        client.submitTransfer(TransferRequest(sourceUrl = "https://pan.quark.cn/s/abc")).getOrThrow()

        val body = JSONObject(server.takeRequest().body.readUtf8())
        assertEquals("https://pan.quark.cn/s/abc", body.getString("source_url"))
        assertEquals("auto", body.getString("channel"))
        assertFalse(body.has("pwd"))
        assertFalse(body.has("fid"))
        assertFalse(body.has("dest_dir"))
        assertFalse(body.has("name"))
        assertFalse(body.has("ua"))
    }

    @Test
    fun `listTransfers parses task array`() = runBlocking {
        server.enqueue(
            MockResponse().setBody(
                """{"tasks":[
                  $taskSample,
                  {"id":"t-43","state":"done","channel":"offline","file_name":"b.bin",
                   "size":10,"done":10,"started_at":"2026-11-08T09:00:00Z",
                   "ended_at":"2026-11-08T09:00:05Z"}
                ]}""".trimIndent()
            )
        )

        val tasks = client.listTransfers().getOrThrow()

        assertEquals(2, tasks.size)
        assertEquals("t-42", tasks[0].id)
        assertEquals(TransferTask.STATE_DONE, tasks[1].state)
        assertTrue(tasks[1].isTerminal)
        assertEquals(1f, tasks[1].progress)
        assertTrue(tasks[1].endedAtMs > tasks[1].startedAtMs)
        assertEquals("/api/v1/transfer", server.takeRequest().path)
    }

    @Test
    fun `listTransfers empty array yields empty list`() = runBlocking {
        server.enqueue(MockResponse().setBody("""{"tasks":[]}"""))
        assertTrue(client.listTransfers().getOrThrow().isEmpty())
    }

    @Test
    fun `transferTask encodes id and parses single task`() = runBlocking {
        server.enqueue(MockResponse().setBody("""{"task":$taskSample}"""))

        val task = client.transferTask("t 42/α").getOrThrow()

        assertEquals("t-42", task.id)
        assertEquals("/api/v1/transfer/t%2042%2F%CE%B1", server.takeRequest().path)
    }

    @Test
    fun `transfer http error maps to panrouter error`() = runBlocking {
        server.enqueue(
            MockResponse()
                .setResponseCode(401)
                .setBody("""{"code":401,"message":"token is invalidated"}""")
        )
        val result = client.listTransfers()
        assertTrue(result.isFailure)
        assertTrue((result.exceptionOrNull() as PanRouterError).kind == PanRouterError.Kind.SESSION_EXPIRED)
    }

    @Test
    fun `storageInfo parses server-provided endpoint`() = runBlocking {
        server.enqueue(
            MockResponse().setBody(
                """{"code":200,"driver":"mangodrive","enabled":true,
                   "public_base":"https://pool.example.net/","dest_dir":"/dav/inbox/",
                   "max_bytes":68719476736}"""
            )
        )

        val info = client.storageInfo().getOrThrow()

        assertEquals("/api/v1/storage", server.takeRequest().path)
        assertTrue(info.enabled)
        assertTrue(info.usable)
        assertEquals("https://pool.example.net", info.publicBase)
        assertEquals("/dav/inbox", info.destDir)
        assertEquals(68719476736L, info.maxBytes)
    }

    @Test
    fun `storageInfo disabled is not usable`() = runBlocking {
        server.enqueue(MockResponse().setBody("""{"code":200,"driver":"mangodrive","enabled":false}"""))

        val info = client.storageInfo().getOrThrow()

        assertFalse(info.enabled)
        assertFalse(info.usable)
        assertEquals("", info.publicBase)
    }

    @Test
    fun `task model progress handles unknown size and state labels`() {
        val unknown = TransferTask(
            id = "x", state = TransferTask.STATE_QUEUED, channel = "",
            sourceUrl = "", destPath = "", fileName = "", size = 0L, done = 99L,
            error = "", hint = "", startedAtMs = 0L, endedAtMs = 0L
        )
        assertNull(unknown.progress)
        assertTrue(unknown.isRunning)
        assertFalse(unknown.isTerminal)

        // offline 受理后停在 pending：不是终态，必须继续轮询，
        // 否则桌面端会在产物真正落池前停止轮询。
        assertEquals("落池中", TransferTask.stateLabelOf(TransferTask.STATE_PENDING))
        val pending = TransferTask(
            id = "p", state = TransferTask.STATE_PENDING, channel = TransferChannel.OFFLINE,
            sourceUrl = "", destPath = "/Pool/a.bin", fileName = "a.bin", size = 0L, done = 0L,
            error = "", hint = "", startedAtMs = 0L, endedAtMs = 0L
        )
        assertTrue(pending.isRunning)
        assertFalse(pending.isTerminal)

        assertEquals("排队中", TransferTask.stateLabelOf(TransferTask.STATE_QUEUED))
        assertEquals("搬运中", TransferTask.stateLabelOf(TransferTask.STATE_RUNNING))
        assertEquals("已完成", TransferTask.stateLabelOf(TransferTask.STATE_DONE))
        assertEquals("失败", TransferTask.stateLabelOf(TransferTask.STATE_FAILED))
        assertEquals("服务端接力", TransferChannel.labelOf(TransferChannel.STREAM))
        assertEquals("远端离线", TransferChannel.labelOf(TransferChannel.OFFLINE))
        assertEquals("自动选择", TransferChannel.labelOf(TransferChannel.AUTO))
    }
}
