package com.yunx.app.data.download

import com.yunx.app.AppContext
import com.yunx.app.data.db.DownloadTaskDao
import com.yunx.app.data.db.DownloadTaskEntity
import com.yunx.app.data.mangodrive.MangoDriveClient
import com.yunx.app.data.mangodrive.MangoDriveSettings
import com.yunx.app.data.panrouter.PanRouterClient
import com.yunx.app.data.security.DbKeyStore
import com.yunx.app.ui.viewmodel.MangoCloudViewModel
import com.yunx.app.util.WindowsToastNotifier
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test
import java.io.File
import java.security.MessageDigest
import java.util.concurrent.atomic.AtomicLong

/**
 * M4.2 真机 E2E：私有池文件 → MangoDriveClient 取 direct_url → DownloadManager 分块直连 → sha256 校验。
 *
 * 默认跳过（CI 无凭据）。本地跑：
 * ```
 * PANROUTER_BASE=https://pan.example.com PANROUTER_TOKEN=... \
 *   ./gradlew :desktop:test --tests "com.yunx.app.data.download.MangoDriveEndToEndTest"
 * ```
 *
 * 夹具需预先放进私有池（确定性内容，测试侧独立重算摘要）：
 * - `MANGO_E2E_DIR`  目录，默认 `/pool/panrouter/out`
 * - `MANGO_E2E_FILE` 文件名，默认 `mango-e2e-fixture.bin`
 * - `MANGO_E2E_SIZE` 大小，默认 5 MiB；内容 `byte[i] = (i * 31 + 7) % 251`
 */
class MangoDriveEndToEndTest {

    private val base = System.getenv("PANROUTER_BASE").orEmpty()
    private val token = System.getenv("PANROUTER_TOKEN").orEmpty()
    private val dir = System.getenv("MANGO_E2E_DIR")?.takeIf { it.isNotBlank() }
        ?: "/pool/panrouter/out"
    private val fixtureName = System.getenv("MANGO_E2E_FILE")?.takeIf { it.isNotBlank() }
        ?: "mango-e2e-fixture.bin"
    private val fixtureSize = System.getenv("MANGO_E2E_SIZE")?.toLongOrNull() ?: (5L * 1024L * 1024L)

    private lateinit var saveDir: File

    @Before
    fun setUp() {
        WindowsToastNotifier.enabled = false
        seedPlainCredentialKey()
        saveDir = File("build/mango-e2e/downloads").apply {
            deleteRecursively()
            mkdirs()
        }
    }

    @After
    fun tearDown() {
        WindowsToastNotifier.enabled = true
    }

    @Test
    fun privatePoolFileDownloadsByteExactOverDirectLink() = runBlocking {
        assumeTrue("未设置 PANROUTER_BASE / PANROUTER_TOKEN，跳过真机 E2E", base.isNotBlank() && token.isNotBlank())

        val client = MangoDriveClient(
            settings = MangoDriveSettings(),
            routerClient = PanRouterClient(
                baseUrlProvider = { base },
                tokenProvider = { token }
            )
        )

        // 1) 列目录找到夹具（服务端把 path 当分享解析）
        val listing = client.listDir(dir).getOrThrow()
        val item = listing.files.firstOrNull { it.name == fixtureName && !it.isDir }
            ?: fail("私有池里没有夹具 $dir/$fixtureName（先用脚本上传）") as Nothing

        assertEquals("服务端返回的文件大小应与夹具一致", fixtureSize, item.size)

        // 2) 取单文件直链：url 必须是父目录、fid 定位文件（服务端契约）
        // v1.23 契约:提链一律显式携带真实所在目录(fixture 目录已知),不再按 fid 形态推父目录
        val link = client.link(dir, item.fid).getOrThrow()
        assertEquals("直链返回大小应与夹具一致", fixtureSize, link.size)
        assertTrue("应拿到 OpenList 签名直链 direct_url", !link.directUrl.isNullOrBlank())
        assertTrue("direct_url 必须带 UA（否则被 Cloudflare 拦）", link.needHeaders && link.ua.isNotBlank())

        // 3) 走真实下载链路：MANGO 平台 + 服务端要求的 UA/Referer
        val dao = MangoE2eTaskDao()
        val manager = newManager(dao, threads = 4)
        val id = manager.enqueue(
            url = MangoCloudViewModel.downloadUrlOf(link),
            fileName = fixtureName,
            size = link.size,
            platform = DownloadPlatform.MANGO,
            headers = MangoCloudViewModel.downloadHeadersOf(link),
            saveDir = saveDir.absolutePath
        )
        awaitCompleted(dao, id)

        // 4) 字节级校验：长度 + 确定性内容的 sha256
        val saved = File(saveDir, fixtureName)
        assertTrue("下载文件应存在", saved.isFile)
        assertEquals("下载长度应与夹具一致", fixtureSize, saved.length())
        assertEquals(
            "sha256 应逐字节一致",
            expectedSha256(fixtureSize),
            sha256Of(saved)
        )
        assertNoLeftovers(id)
    }

    /**
     * 预置一把**未包裹**的 32 字节主密钥，让 FileCredentialCipher 直接采用
     * （[DbKeyStore.loadOrCreate] 对裸密钥原样返回，不走机器指纹 / DPAPI）——
     * 开发机是 macOS/Linux，读不到 Windows MachineGuid，否则写请求头时会抛
     * MachineFingerprintUnavailable。只写测试数据目录，绝不触碰真实用户数据。
     */
    private fun seedPlainCredentialKey() {
        // 安全闸：只允许在 build/ 下的临时数据目录里造密钥（dbDir 可能被偏好指到别处）
        val buildMarker = "${File.separator}build${File.separator}"
        if (!AppContext.dataDir.absolutePath.contains(buildMarker)) return
        if (!AppContext.dbDir.absolutePath.contains(buildMarker)) return
        val keyFile = File(AppContext.dbDir, DbKeyStore.KEY_FILE)
        if (keyFile.exists()) return
        keyFile.parentFile?.mkdirs()
        keyFile.writeBytes(ByteArray(32) { (it * 17 + 3).toByte() })
    }

    // ---------- 夹具与校验 ----------

    /** 夹具的确定性内容：byte[i] = (i * 31 + 7) % 251（同一条公式用于上传脚本） */
    private fun expectedSha256(size: Long): String {
        val digest = MessageDigest.getInstance("SHA-256")
        val block = ByteArray(64 * 1024)
        var written = 0L
        while (written < size) {
            val n = minOf(block.size.toLong(), size - written).toInt()
            for (k in 0 until n) {
                val idx = written + k
                block[k] = (((idx % 251L) * 31L + 7L) % 251L).toByte()
            }
            digest.update(block, 0, n)
            written += n
        }
        return digest.digest().joinToString("") { "%02x".format(it) }
    }

    private fun sha256Of(file: File): String {
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { input ->
            val buf = ByteArray(64 * 1024)
            while (true) {
                val n = input.read(buf)
                if (n < 0) break
                digest.update(buf, 0, n)
            }
        }
        return digest.digest().joinToString("") { "%02x".format(it) }
    }

    private fun assertNoLeftovers(id: Long) {
        val leftovers = saveDir.listFiles()?.filter { it.name.contains(".yunx-") } ?: emptyList()
        assertTrue("不应残留直写临时文件：${leftovers.map { it.name }}", leftovers.isEmpty())
        assertTrue("目标目录不应残留分片缓存目录", saveDir.listFiles()?.none { it.isDirectory } ?: true)
    }

    // ---------- 下载链路宿主 ----------

    private fun newManager(dao: DownloadTaskDao, threads: Int): DownloadManager = DownloadManager(
        dao = dao,
        downloader = ChunkDownloader { OkHttpClient() },
        threadProvider = { threads },
        saveDirProvider = { saveDir.absolutePath },
        concurrencyProvider = { 1 },
        retryCountProvider = { 0 },
        keepAwakeProvider = { false },
        showSpeedProvider = { false },
        directWriteSingleProvider = { false },
        directWriteMultiProvider = { true }
    )

    private suspend fun awaitCompleted(dao: DownloadTaskDao, id: Long) {
        withTimeout(180_000) {
            while (true) {
                val task = dao.get(id)
                when (task?.status) {
                    DownloadTaskEntity.STATUS_COMPLETED -> return@withTimeout
                    DownloadTaskEntity.STATUS_FAILED -> fail("任务失败：${task.errorMsg}")
                }
                delay(100)
            }
        }
    }
}

/** 仅测试用的内存 DAO（不碰真实数据库；独立实现，避免与 DownloadManagerDirectIntegrationTest 的私有 DAO 重名） */
/**
 * 测试用 DAO 的并发语义护栏。
 *
 * 背景：[MangoE2eTaskDao] 用 `MutableStateFlow` 模拟 DB。最初写成
 * `state.value = state.value + task`（读 → 改 → 写三步非原子），
 * 并发入队时互相覆盖 → 丢任务。该丢任务在聚合用例里表现为
 * 「成功数 8，落库只有 7 条」这类与被测逻辑无关的偶发失败，
 * 排查成本极高。
 *
 * 本用例用**同一协程**内的并发屏障复现：无原子性时必丢，有原子性时必不丢。
 */
class MangoE2eTaskDaoConcurrencyTest {

    @Test
    fun `concurrent inserts must not lose tasks`() = runBlocking {
        val dao = MangoE2eTaskDao()
        val n = 200
        // 所有 insert 先在屏障前就位，随后同时放行 → 制造真实并发窗口
        val gate = CompletableDeferred<Unit>()
        val jobs = (0 until n).map { i ->
            async(Dispatchers.Default) {
                gate.await()
                dao.insert(
                    DownloadTaskEntity(
                        url = "http://127.0.0.1/$i",
                        fileName = "f$i.bin",
                        totalSize = 1L
                    )
                )
            }
        }
        gate.complete(Unit)
        jobs.awaitAll()
        val all = dao.listAll()
        assertEquals("并发插入不得丢任务（应恰好 $n 条）", n, all.size)
        assertEquals("任务 id 不得重复", n, all.map { it.id }.toSet().size)
    }
}

internal class MangoE2eTaskDao : DownloadTaskDao {
    private val state = MutableStateFlow<List<DownloadTaskEntity>>(emptyList())
    private val nextId = AtomicLong(1L)

    // ★ 必须用 state.update{}（CAS 循环）而不是 `state.value = state.value +/- x`：
    //   后者是「读 → 改 → 写」三步非原子操作，并发入队（enqueueEach 默认 3 并发）
    //   会互相覆盖 → 丢任务。实测：12 任务并发入队，DB 只剩 7 条（丢 5 条）。
    //   MutableStateFlow.update 是原子的 compare-and-set 循环，不会丢更新。
    private fun mutate(id: Long, block: (DownloadTaskEntity) -> DownloadTaskEntity) {
        state.update { list -> list.map { if (it.id == id) block(it) else it } }
    }

    override fun observeAll(): Flow<List<DownloadTaskEntity>> = state
    override suspend fun insert(task: DownloadTaskEntity): Long {
        val id = nextId.getAndIncrement()
        state.update { list -> list + task.copy(id = id) }
        return id
    }
    override suspend fun get(id: Long): DownloadTaskEntity? = state.value.firstOrNull { it.id == id }
    override suspend fun listAll(): List<DownloadTaskEntity> = state.value
    override suspend fun repairCompletedProgress() = Unit
    override suspend fun updateProgress(id: Long, status: Int, downloadedSize: Long, totalSize: Long) =
        mutate(id) { it.copy(status = status, downloadedSize = downloadedSize, totalSize = totalSize) }
    override suspend fun updatePlan(id: Long, chunkCount: Int, totalSize: Long) =
        mutate(id) { it.copy(chunkCount = chunkCount, plannedTotalSize = totalSize) }
    override suspend fun updateRequestHeaders(id: Long, encryptedHeaders: String) =
        mutate(id) { it.copy(requestHeadersJson = encryptedHeaders) }
    override suspend fun markInterruptedAsPaused() = Unit
    override suspend fun updateStatus(id: Long, status: Int) = mutate(id) { it.copy(status = status) }
    override suspend fun updateError(id: Long, errorMsg: String) = mutate(id) { it.copy(errorMsg = errorMsg) }
    override suspend fun complete(id: Long, status: Int, savePath: String, avgSpeed: Long) =
        mutate(id) {
            it.copy(
                status = status,
                savePath = savePath,
                avgSpeed = avgSpeed,
                downloadedSize = if (it.totalSize > 0) it.totalSize else it.downloadedSize
            )
        }
    override suspend fun delete(id: Long) {
        state.value = state.value.filterNot { it.id == id }
    }
    override suspend fun updateEngineTaskId(id: Long, engineTaskId: String) =
        mutate(id) { it.copy(engineTaskId = engineTaskId) }
    override suspend fun updateFileName(id: Long, name: String) = mutate(id) { it.copy(fileName = name) }
    override suspend fun listSyncableEngineTasks(): List<DownloadTaskEntity> = emptyList()
    override suspend fun updateSyncUid(id: Long, uid: String) = mutate(id) { it.copy(syncUid = uid) }
    override suspend fun listWithEmptySyncUid(): List<DownloadTaskEntity> =
        state.value.filter { it.syncUid.isEmpty() }
    override suspend fun listBySyncUids(uids: List<String>): List<DownloadTaskEntity> =
        if (uids.isEmpty()) emptyList() else state.value.filter { it.syncUid in uids.toSet() }
    override suspend fun updateDirect(id: Long, partPath: String, mode: Int) =
        mutate(id) { it.copy(directPart = partPath, directMode = mode) }
}
