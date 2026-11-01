package com.yunx.app.data.panrouter

import org.json.JSONArray
import org.json.JSONObject

/**
 * PanRouter 服务端（pan.example.com）的数据契约。
 *
 * 字段名与 Go 侧 JSON tag 逐一对应（panrouter/docs/development.md §6/§12.2）：
 * 服务端契约变更时先补本文件与测试样本，再改代码。**两仓零共享代码（AGPL 边界）**，
 * 契约以文档与这里的手写解析为唯一同步点。
 */

/** `POST /api/v1/resolve`（不带 fid）的文件行 */
data class RemoteFileItem(
    val fid: String,
    val name: String,
    val size: Long,
    val isDir: Boolean,
    /** 自动提链的路由（"302" / "aria2" / "stream"）；未提链为 null */
    val route: String?,
    /** 自动提链的下载入口（302 直链或降级流）；未提链为 null */
    val downloadUrl: String?,
    /** 该文件自动提链失败的错误文本（其余文件不受影响） */
    val linkError: String?
)

/** `POST /api/v1/resolve`（不带 fid）的整体结果 */
data class RemoteResolveResult(
    val pan: String,
    val shareKey: String,
    val files: List<RemoteFileItem>,
    /** 服务端提示（自动提链截断原因等） */
    val hint: String?,
    /** 自动提链被截断（超出服务端单次提链上限，下载时逐文件取链即可） */
    val truncated: Boolean
)

/** `POST /api/v1/resolve`（带 fid）的单文件直链结果 */
data class RemoteLinkResult(
    val pan: String,
    val shareKey: String,
    val fid: String,
    val fileName: String,
    val size: Long,
    /** "302" | "aria2" | "stream" */
    val route: String,
    /** 浏览器可直接打开的下载入口（302 直链，或降级 /stream） */
    val downloadUrl: String,
    /** 中转流（透传 Range，支持分片并发） */
    val streamUrl: String,
    /** 服务端给出的原始上游直链（仅 302 路由时有值；桌面端不用它，统一走 download_url/stream_url） */
    val directUrl: String?,
    /** 直链要求的 UA；空 = 不校验 */
    val ua: String,
    /** 直链要求的 Referer；空 = 不校验 */
    val referer: String,
    /** true = 下载请求必须按 ua/referer 注入请求头 */
    val needHeaders: Boolean,
    /** 上游直链独立有效期（epoch 毫秒；0 = 服务端未给） */
    val expiresAtMs: Long,
    /** 本次直链是否命中服务端缓存 */
    val cacheHit: Boolean
) {
    /** 下载走哪条路径：stream = 中转流（桌面端降线程数），否则 302 直链全速分片 */
    val isRelayRoute: Boolean get() = route == ROUTE_STREAM || downloadUrl.isBlank()

    /** 桌面端实际请求的 URL：中转 → stream_url；直连 → download_url */
    fun effectiveUrl(): String = if (isRelayRoute) streamUrl else downloadUrl

    companion object {
        const val ROUTE_302 = "302"
        const val ROUTE_ARIA2 = "aria2"
        const val ROUTE_STREAM = "stream"
    }
}

/** `GET /healthz` 结果 */
data class RemoteServerInfo(
    val status: String,
    val version: String
)

/** 服务端账号池账号行（`GET/POST /api/v1/accounts`，panrouter handlers.go accountView） */
data class RemoteAccountView(
    val id: Long,
    val panType: String,
    val name: String,
    /** ok / cooling / expired（服务端状态机） */
    val status: String
)

/** `POST /api/v1/resolve/batch` 的单条结果（batchItem：失败时 error 为纯文本，无结构化 kind） */
data class RemoteBatchItem(
    val index: Int,
    val ok: Boolean,
    /** 成功时的分享解析结果（与单链接 resolve 同构） */
    val result: RemoteResolveResult?,
    /** 失败原因文本（服务端 err.Error()） */
    val error: String?
)

// ---------- JSON 解析（服务端字段缺失时取安全默认值，不抛解析异常） ----------

internal fun parseServerInfo(body: JSONObject): RemoteServerInfo = RemoteServerInfo(
    status = body.optString("status"),
    version = body.optString("version")
)

internal fun parseResolveResult(body: JSONObject): RemoteResolveResult = RemoteResolveResult(
    pan = body.optString("pan"),
    shareKey = body.optString("share_key"),
    files = body.optJSONArray("files").orEmpty().map { parseFileItem(it) },
    hint = body.optString("hint").takeIf { it.isNotEmpty() },
    truncated = body.optBoolean("truncated", false)
)

internal fun parseFileItem(o: JSONObject): RemoteFileItem = RemoteFileItem(
    fid = o.optString("fid"),
    name = o.optString("name"),
    size = o.optLong("size", -1L),
    isDir = o.optBoolean("is_dir", false),
    route = o.optString("route").takeIf { it.isNotEmpty() },
    downloadUrl = o.optString("download_url").takeIf { it.isNotEmpty() },
    linkError = o.optString("link_error").takeIf { it.isNotEmpty() }
)

internal fun parseLinkResult(body: JSONObject): RemoteLinkResult = RemoteLinkResult(
    pan = body.optString("pan"),
    shareKey = body.optString("share_key"),
    fid = body.optString("fid"),
    fileName = body.optString("file_name"),
    size = body.optLong("size", -1L),
    route = body.optString("route"),
    downloadUrl = body.optString("download_url"),
    streamUrl = body.optString("stream_url"),
    directUrl = body.optString("direct_url").takeIf { it.isNotEmpty() },
    ua = body.optString("ua"),
    referer = body.optString("referer"),
    needHeaders = body.optBoolean("need_headers", false),
    expiresAtMs = parseEpochMs(body.optString("expires_at")),
    cacheHit = body.optBoolean("cache_hit", false)
)

internal fun parseAccountView(body: JSONObject): RemoteAccountView = RemoteAccountView(
    id = body.optLong("id", -1L),
    panType = body.optString("pan_type"),
    name = body.optString("name"),
    status = body.optString("status")
)

internal fun parseBatchItem(o: JSONObject): RemoteBatchItem = RemoteBatchItem(
    index = o.optInt("index", -1),
    ok = o.optBoolean("ok", false),
    result = o.optJSONObject("result")?.let(::parseResolveResult),
    error = o.optString("error").takeIf { it.isNotEmpty() }
)

/** RFC3339（Go time.Time JSON）→ epoch 毫秒；解析失败返回 0（不视为致命） */
internal fun parseEpochMs(rfc3339: String): Long = runCatching {
    java.time.OffsetDateTime.parse(rfc3339).toInstant().toEpochMilli()
}.getOrDefault(0L)

private fun JSONArray?.orEmpty(): JSONArray = this ?: JSONArray()

private inline fun <T> JSONArray.map(transform: (JSONObject) -> T): List<T> {
    val out = ArrayList<T>(length())
    for (i in 0 until length()) {
        out.add(transform(optJSONObject(i) ?: JSONObject()))
    }
    return out
}
