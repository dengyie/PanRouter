package com.yunx.app.data.mangodrive

import com.yunx.app.data.panrouter.PanRouterClient
import com.yunx.app.data.panrouter.PanRouterError
import com.yunx.app.data.panrouter.PanRouterSettings
import com.yunx.app.data.panrouter.PanRouterTokenStore
import com.yunx.app.data.panrouter.RemoteLinkResult
import com.yunx.app.data.panrouter.RemoteResolveResult

/**
 * Mango Drive 私有聚合云盘客户端（桌面端角度）。
 *
 * 经 PanRouter 服务端（mangodrive 驱动）访问，不直连 OpenList / Google：
 * - [listDir]：列目录（服务端把 path 当分享解析，返回子项；fid 即存储路径，长路径为哈希）
 * - [link]：取单文件直链（服务端回问 driver 取名字/大小 —— 见 resolver 的 fid-only 回查）
 *
 * ⚠ 两个关键契约（服务端 resolver 实测）：
 * 1. **url 必须是目录路径**：单文件解析时 url 定位目录、fid 定位文件；url 直接给文件路径会 404
 *    （"路径不存在"）。所以 [link] 的 [dirPath] 由调用方（面板当前目录）显式传入。
 * 2. **[directUrl] 是 OpenList 签名直链**：只按「路径 + 签名」签发，host 可换；支持 Range（206 实测），
 *    桌面端用它直连分块下载，要求带返回的 [RemoteLinkResult.ua]（不带 UA 会被 Cloudflare 边缘拦 000）。
 *    下载线程数由面板按平台（MANGO）设置走 —— 私有盘不经 pxed 中转，不需要 relay 限线程。
 *
 * 路径默认做了逐段 URL 编码（服务端会 unescape，中文/空格文件名安全）。
 */
class MangoDriveClient(
    private val settings: MangoDriveSettings = MangoDriveSettings(),
    private val routerClient: PanRouterClient = defaultRouterClient()
) {

    /** 服务端下发的端点基址缓存（实例级；改设置=新建实例，天然生效） */
    private var cachedBase: String? = null

    /** 列目录：path 为存储路径（"/" = 根目录），返回该目录下子项 */
    suspend fun listDir(path: String): Result<RemoteResolveResult> {
        val base = endpointBase().getOrElse { return Result.failure(it) }
        return routerClient.resolveShare(base + encodePath(path), null)
    }

    /** 取单文件直链：dirPath 为文件所在目录（可由 fid 推导），fid 为文件标识 */
    suspend fun link(dirPath: String, fid: String): Result<RemoteLinkResult> {
        val base = endpointBase().getOrElse { return Result.failure(it) }
        return routerClient.resolveLink(base + encodePath(dirPath), null, fid)
    }

    /** 服务端默认落盘目录（配置值）；取不到时返回空串（= 端点根目录）。 */
    suspend fun defaultDestDir(): String =
        routerClient.storageInfo().getOrNull()?.destDir.orEmpty()

    /**
     * 端点基址：本地显式覆盖优先，否则向服务端要（`GET /api/v1/storage`），成功后缓存。
     *
     * 桌面端不内置任何部署域名 —— 换一套 OpenList 端点只需改服务端 `transfer` 配置，
     * 客户端下次启动（或首次调用）自动跟随。
     */
    private suspend fun endpointBase(): Result<String> {
        settings.publicBaseUrl.takeIf { it.isNotBlank() }?.let { return Result.success(it) }
        cachedBase?.let { return Result.success(it) }
        val info = routerClient.storageInfo().getOrElse { return Result.failure(it) }
        if (!info.usable) {
            return Result.failure(
                PanRouterError(
                    message = "服务端未接入自建云盘端点（storage.enabled=false）",
                    kind = PanRouterError.Kind.UNSUPPORTED,
                    retriable = false
                )
            )
        }
        cachedBase = info.publicBase
        return Result.success(info.publicBase)
    }

    /** 从 fid（存储路径）推导父目录：fid 非路径形态（长路径哈希）时无解，返回 "/" 由服务端兜底 */
    fun parentDirOf(fid: String): String {
        if (!fid.startsWith("/")) return "/"
        val idx = fid.lastIndexOf('/')
        return if (idx <= 0) "/" else fid.substring(0, idx)
    }

    companion object {
        /** 逐段 URL 编码：保留 "/" 分隔，其余按 RFC 3986（空格 → %20）；空/根/带尾斜杠都归一为根或去尾 */
        internal fun encodePath(path: String): String {
            val p = path.trim().trimEnd('/')
            if (p.isEmpty()) return "/"
            val withLead = if (p.startsWith("/")) p else "/$p"
            val segments = withLead.split("/")
            return segments.joinToString("/") { seg ->
                if (seg.isEmpty()) "" else java.net.URLEncoder.encode(seg, "UTF-8").replace("+", "%20")
            }
        }

        fun defaultRouterClient(): PanRouterClient = PanRouterClient(
            baseUrlProvider = { PanRouterSettings().baseUrl },
            tokenProvider = { PanRouterTokenStore.getToken() }
        )
    }
}
