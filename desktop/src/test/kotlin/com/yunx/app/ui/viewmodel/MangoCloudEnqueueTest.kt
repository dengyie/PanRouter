package com.yunx.app.ui.viewmodel

import com.yunx.app.data.download.DownloadManager
import com.yunx.app.data.mangodrive.MangoDriveClient
import com.yunx.app.data.mangodrive.MangoDriveSettings
import com.yunx.app.util.WindowsToastNotifier
import com.yunx.app.data.panrouter.PanRouterClient
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.io.File

/**
 * 文件夹下载入队核心（[MangoCloudViewModel.enqueueEach]）的并发/聚合回归：
 * 12 个任务、4 个提链失败 → ok=8、失败名单精确、任务全部落库、消息含明细。
 * 真实 DownloadManager（内存 DAO）保证 enqueue 契约不被 mock 掩盖。
 */
class MangoCloudEnqueueTest {

    private lateinit var server: MockWebServer
    private lateinit var saveDir: File
    private lateinit var dao: com.yunx.app.data.download.MangoE2eTaskDao
    private lateinit var manager: DownloadManager

    /** 失败的文件序号（0..11 中的 4 个）；dispatcher 按请求里的 fid 判定 */
    private val failingIdx = setOf(8, 9, 10, 11)

    @Before
    fun setUp() {
        WindowsToastNotifier.enabled = false
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val path = request.path.orEmpty()
                return when {
                    path.startsWith("/api/v1/storage") -> json(
                        """{"code":200,"driver":"mangodrive","enabled":true,
                           "public_base":"${server.url("/drive")}",
                           "dest_dir":"/Pool","max_bytes":0,"limit_qps":10}"""
                    )
                    path.startsWith("/api/v1/resolve") -> {
                        val body = request.body.readUtf8()
                        val idx = Regex("\"fid\":\"[^\"]*quota(\\d+)\\.bin\"").find(body)
                            ?.groupValues?.get(1)?.toIntOrNull() ?: -1
                        if (idx in failingIdx) {
                            MockResponse().setResponseCode(404).setBody(
                                """{"code":404,"kind":"share_gone","message":"mock 404","retriable":false}"""
                            )
                        } else {
                            json(
                                """{"pan":"mangodrive","share_key":"k","fid":"/d/quota$idx.bin",
                                   "file_name":"quota$idx.bin","size":4,"route":"aria2",
                                   "download_url":"${server.url("/file$idx")}",
                                   "stream_url":"${server.url("/stream$idx")}",
                                   "direct_url":"${server.url("/file$idx")}",
                                   "ua":"UA/1","referer":"","need_headers":true,
                                   "expires_at_ms":0,"cache_hit":false}"""
                            )
                        }
                    }
                    path.startsWith("/file") || path.startsWith("/stream") ->
                        MockResponse().setResponseCode(200).setBody("DATA")
                    else -> MockResponse().setResponseCode(404).setBody("""{"code":404}""")
                }
            }
        }
        server.start()
        saveDir = File("build/mango-enqueue-test/downloads").apply { deleteRecursively(); mkdirs() }
        dao = com.yunx.app.data.download.MangoE2eTaskDao()
        manager = DownloadManager(
            dao = dao,
            downloader = com.yunx.app.data.download.ChunkDownloader { OkHttpClient() },
            threadProvider = { 2 },
            saveDirProvider = { saveDir.absolutePath },
            concurrencyProvider = { 1 },
            retryCountProvider = { 0 },
            keepAwakeProvider = { false },
            showSpeedProvider = { false },
            directWriteSingleProvider = { false },
            directWriteMultiProvider = { true }
        )
    }

    @After
    fun tearDown() {
        WindowsToastNotifier.enabled = true
        server.shutdown()
    }

    private fun json(s: String) = MockResponse()
        .setResponseCode(200)
        .setHeader("Content-Type", "application/json")
        .setBody(s.filter { !it.isWhitespace() })

    private fun vm(): MangoCloudViewModel {
        val client = MangoDriveClient(
            settings = MangoDriveSettings(),
            routerClient = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = { server.url("/").toString().trimEnd('/') },
                tokenProvider = { "test-token" }
            )
        )
        return MangoCloudViewModel(client, manager)
    }

    private fun tasks(n: Int) = (0 until n).map { i ->
        MangoCollectItem("folder/quota$i.bin", "/d/quota$i.bin", "/d")
    }

    @Test
    fun `enqueueEach aggregates ok count and per-file failures`() = runBlocking {
        val (ok, failed) = vm().enqueueEach(tasks(12), saveDir.absolutePath)
        assertEquals("成功数 = 12 - 4 个失败", 8, ok)
        assertEquals("失败名单 = 4 个且序号正确", setOf(8, 9, 10, 11),
            failed.map { Regex("quota(\\d+)").find(it)!!.groupValues[1].toInt() }.toSet())
        // 失败的不落库,成功的全部落库
        assertEquals(8, dao.listAll().size)
    }

    @Test
    fun `enqueueResultMessage includes truncation and failure details`() {
        val msg = vm().enqueueResultMessage(
            ok = 8, failed = listOf("folder/quota8.bin", "folder/quota9.bin", "folder/quota10.bin", "folder/quota11.bin"),
            skipped = 2
        )
        assertTrue("含成功数", msg.contains("已加入 8 个"))
        assertTrue("含截断计数", msg.contains("2 个子目录超 8 层上限"))
        assertTrue("含失败数", msg.contains("失败 4 个"))
        assertTrue("明细最多 3 个 + 等", msg.contains("等"))
    }

    @Test
    fun `resolveConcurrency clamps server limit_qps`() {
        // 直接对解析函数断言:10 QPS → 1/3 = 3(与历史默认一致);0 → 默认 3
        val client = MangoDriveClient(
            settings = MangoDriveSettings(),
            routerClient = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = { server.url("/").toString().trimEnd('/') },
                tokenProvider = { "test-token" }
            )
        )
        val conc = runBlocking { client.resolveConcurrency() }
        assertEquals("limit_qps=10 → 并发 3", 3, conc)
        assertEquals("未下发时回退默认", MangoDriveClient.DEFAULT_RESOLVE_CONCURRENCY, conc)
    }
}

