package com.yunx.app.ui.viewmodel

import com.yunx.app.data.download.DownloadManager
import com.yunx.app.data.mangodrive.MangoDriveClient
import com.yunx.app.data.mangodrive.MangoDriveSettings
import com.yunx.app.util.WindowsToastNotifier
import com.yunx.app.data.panrouter.PanRouterClient
import com.yunx.app.data.panrouter.RemoteLinkResult
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
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

    /**
     * 服务端下发的 limit_qps；null = 响应里**不下发该字段**（旧版本服务端形态）。
     * 用于真正跑到 resolveConcurrency 的「未下发 → 回退默认」分支，
     * 而不是拿 limit_qps=10 的结果冒充回退分支断言（那样断言恒真、无鉴别力）。
     */
    private var limitQps: Int? = 10

    @Before
    fun setUp() {
        WindowsToastNotifier.enabled = false
        limitQps = 10
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val path = request.path.orEmpty()
                return when {
                    path.startsWith("/api/v1/storage") -> {
                        val qpsField = limitQps?.let { "\"limit_qps\":$it," } ?: ""
                        json(
                            """{"code":200,"driver":"mangodrive","enabled":true,
                               "public_base":"${server.url("/drive")}",
                               "dest_dir":"/Pool","max_bytes":0,$qpsField"reserved":0}"""
                        )
                    }
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
        // ★ 必须先停掉后台下载再关 MockWebServer：enqueue 会立即启动真实下载，
        //   若在请求仍在飞时 shutdown，MockWebServer 抛 "Gave up waiting for queue
        //   to shut down"（IOException），把用例失败原因从「被测逻辑」带偏到「测试环境」。
        runBlocking {
            dao.listAll().forEach { manager.remove(it.id, deleteLocal = true) }
            delay(300)
        }
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
        // limit_qps=10 → 1/3 = 3（与历史默认一致）
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
    }

    @Test
    fun `resolveConcurrency falls back to default when limit_qps absent`() {
        // 服务端**不下发** limit_qps（旧版本形态）→ 必须回退到默认并发，
        // 且此时默认（3）≠ limit_qps=10 时的计算值才有鉴别力——否则断言恒真。
        limitQps = null
        val client = MangoDriveClient(
            settings = MangoDriveSettings(),
            routerClient = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = { server.url("/").toString().trimEnd('/') },
                tokenProvider = { "test-token" }
            )
        )
        val conc = runBlocking { client.resolveConcurrency() }
        assertEquals("未下发 limit_qps → 回退默认", MangoDriveClient.DEFAULT_RESOLVE_CONCURRENCY, conc)
    }

    @Test
    fun `resolveConcurrency clamps low and high limit_qps`() {
        // limit_qps=1 → 1/3 = 0 → 钳到下限 1
        limitQps = 1
        val client = MangoDriveClient(
            settings = MangoDriveSettings(),
            routerClient = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = { server.url("/").toString().trimEnd('/') },
                tokenProvider = { "test-token" }
            )
        )
        assertEquals("limit_qps=1 → 并发钳到 1", 1, runBlocking { client.resolveConcurrency() })

        // limit_qps=100 → 1/3 = 33 → 钳到上限 6
        limitQps = 100
        assertEquals("limit_qps=100 → 并发钳到 6", 6, runBlocking { client.resolveConcurrency() })
    }

    @Test
    fun `enqueueEach does not report cancellation as failure`() {
        // 取消不是「失败」：CancellationException 被 runCatching 吞掉后，
        // onFailure 会把文件塞进 failed 名单 → 用户看到「失败 N 个」，
        // 而实际语义是「已中断下载」。
        //
        // 鉴别力（修复前 vs 修复后）：
        //   修复前：runCatching 吞掉 CancellationException → onFailure 记入 failed
        //           → failed = 3 条 → 本用例断言失败；
        //   修复后：CancellationException 原样重抛 → 该子协程以「取消」结束，
        //           failed 为空、ok 为 0，用户看到的是「已中断」而非「失败 3 个」。
        //
        // 注：协程语义上，子协程抛 CancellationException 不会让父作用域失败
        //     （父只等它结束），所以断言点是**失败名单**而非异常外抛。
        val vm = vm()
        val (ok, failed) = runBlocking {
            vm.enqueueEach(
                tasks(3),
                saveDir.absolutePath,
                concurrency = 3,
                linkProvider = { _, _ -> throw CancellationException("cancelled") }
            )
        }
        assertEquals("取消不应计入成功", 0, ok)
        assertTrue("取消不得被记成失败（会被误报为「失败 3 个」）", failed.isEmpty())
        assertEquals("取消后不应入队任何任务", 0, runBlocking { dao.listAll().size })
    }

    @Test
    fun `enqueueEach counts only real failures and never counts cancellation`() {
        // 普通异常（404 等）仍必须计入 failed —— 取消修复不能把「失败」也一起吞掉。
        val vm = vm()
        val (ok, failed) = runBlocking {
            vm.enqueueEach(
                tasks(3),
                saveDir.absolutePath,
                concurrency = 3,
                linkProvider = { _, fid ->
                    if (fid.endsWith("quota0.bin")) throw IllegalStateException("boom")
                    RemoteLinkResult(
                        pan = "mangodrive",
                        shareKey = "k",
                        fid = fid,
                        fileName = fid.substringAfterLast('/'),
                        size = 4,
                        route = "aria2",
                        downloadUrl = server.url("/file").toString(),
                        streamUrl = server.url("/stream").toString(),
                        directUrl = null,
                        ua = "UA/1",
                        referer = "",
                        needHeaders = true,
                        expiresAtMs = 0,
                        cacheHit = false
                    )
                }
            )
        }
        assertEquals("成功数 = 3 - 1 个真失败", 2, ok)
        assertEquals("失败名单只含真失败项", listOf("folder/quota0.bin"), failed)
        assertEquals("成功项才落库", 2, runBlocking { dao.listAll().size })
    }
}
