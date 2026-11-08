package com.yunx.app.data.panrouter

import com.yunx.app.data.mangodrive.MangoDriveClient
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.Request
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Test
import java.security.MessageDigest

/**
 * M4.3 真机 E2E：桌面端 `PanRouterClient` 提交云对云转存 → 轮询到 done → 从私有盘取回并校验 sha256。
 *
 * 前置（与 M4.2 E2E 一样由外部准备）：
 * - PanRouter 可达（本地 SSH 隧道到 pxed 即可），并带 api_token；
 * - 私有池里已存在确定性源文件（默认 `/pool/panrouter/src/mango-e2e-src.bin`，
 *   内容按 `byte[i] = ((i%251)*31+7)%251` 生成，1 MiB）；
 * - 服务端 mangodrive 账号池已配置（transfer 解析源需要）。
 *
 * 环境变量：
 * - `PANROUTER_BASE` / `PANROUTER_TOKEN`：缺一即跳过（CI 不跑真机）。
 * - `MANGO_XFER_SRC`、`MANGO_XFER_DEST_DIR`、`MANGO_XFER_SIZE`、`MANGO_PUBLIC_BASE`。
 */
class MangoTransferEndToEndTest {

    private val base = System.getenv("PANROUTER_BASE").orEmpty().trimEnd('/')
    private val token = System.getenv("PANROUTER_TOKEN").orEmpty()

    private val srcPath = System.getenv("MANGO_XFER_SRC")
        ?.takeIf { it.isNotBlank() } ?: "/pool/panrouter/src/mango-e2e-src.bin"
    private val destDir = System.getenv("MANGO_XFER_DEST_DIR")
        ?.takeIf { it.isNotBlank() } ?: "/pool/panrouter/out"
    private val publicBaseEnv = System.getenv("MANGO_PUBLIC_BASE")?.takeIf { it.isNotBlank() }
    private val size = System.getenv("MANGO_XFER_SIZE")?.toLongOrNull() ?: (1L shl 20)

    private fun expectedSha256(size: Long): String {
        val md = MessageDigest.getInstance("SHA-256")
        val buf = ByteArray(64 * 1024)
        var written = 0L
        while (written < size) {
            val n = minOf(buf.size.toLong(), size - written).toInt()
            for (i in 0 until n) {
                val abs = written + i
                buf[i] = (((abs % 251) * 31 + 7) % 251).toByte()
            }
            md.update(buf, 0, n)
            written += n
        }
        return md.digest().joinToString("") { "%02x".format(it) }
    }

    @Test
    fun `serverTransferMovesFileAndResultIsByteExact`() = runBlocking {
        assumeTrue("PANROUTER_BASE 未设置，跳过真机 E2E", base.isNotBlank())
        assumeTrue("PANROUTER_TOKEN 未设置，跳过真机 E2E", token.isNotBlank())

        val client = PanRouterClient(
            baseUrlProvider = { base },
            tokenProvider = { token }
        )
        // 端点基址由服务端下发（桌面端不内置部署域名）；显式环境变量仅用于临时覆盖
        val storage = client.storageInfo().getOrThrow()
        val publicBase = publicBaseEnv
            ?: storage.publicBase.takeIf { it.isNotBlank() }
            ?: error("服务端未下发 public_base（GET /api/v1/storage 为 disabled）")
        val destName = "mango-e2e-xfer-${System.currentTimeMillis()}.bin"

        val submitted = client.submitTransfer(
            TransferRequest(
                sourceUrl = publicBase.trimEnd('/') + srcPath,
                fid = srcPath,
                destDir = destDir,
                name = destName,
                channel = TransferChannel.STREAM
            )
        ).getOrThrow()
        assertTrue("服务端应返回任务 id", submitted.id.isNotBlank())

        var task = submitted
        val deadline = System.currentTimeMillis() + 180_000
        while (!task.isTerminal && System.currentTimeMillis() < deadline) {
            delay(1_000)
            task = client.transferTask(submitted.id).getOrThrow()
        }
        assertEquals("转存失败：${task.error} ${task.hint}", TransferTask.STATE_DONE, task.state)
        assertEquals(size, task.size)
        assertEquals(size, task.done)

        val listed = client.listTransfers().getOrThrow()
        assertTrue("转存记录应包含 ${submitted.id}", listed.any { it.id == submitted.id })

        // 从私有盘取回：列目录 → 取直链 → 带 UA 下载 → sha256 校验
        val drive = MangoDriveClient(
            routerClient = PanRouterClient(baseUrlProvider = { base }, tokenProvider = { token })
        )
        val item = drive.listDir(destDir).getOrThrow().files.firstOrNull { it.name == destName }
            ?: error("目标目录里找不到转存结果：$destName")
        assertEquals(size, item.size)

        val link = drive.link(drive.parentDirOf(item.fid), item.fid).getOrThrow()
        val url = link.directUrl?.takeIf { it.isNotBlank() } ?: link.effectiveUrl()
        val req = Request.Builder().url(url).get()
        if (link.needHeaders) {
            if (link.ua.isNotBlank()) req.header("User-Agent", link.ua)
            if (link.referer.isNotBlank()) req.header("Referer", link.referer)
        }
        val body = OkHttpClient().newCall(req.build()).execute().use { resp ->
            assertTrue("下载 HTTP ${resp.code}", resp.isSuccessful)
            resp.body?.bytes() ?: ByteArray(0)
        }
        assertEquals(size, body.size.toLong())
        val actual = MessageDigest.getInstance("SHA-256").digest(body)
            .joinToString("") { "%02x".format(it) }
        assertEquals(expectedSha256(size), actual)
    }
}
