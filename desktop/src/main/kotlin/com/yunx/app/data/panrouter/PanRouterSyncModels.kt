package com.yunx.app.data.panrouter

import org.json.JSONArray
import org.json.JSONObject

/**
 * 历史同步（M3）契约模型：桌面端 ↔ `POST|GET /api/v1/sync`（服务端 development.md §7.6）。
 *
 * 三类 payload 的字段是**桌面端语义**，服务端按不透明 JSON 存储（两仓零共享代码边界）：
 * - bookmark：`{link,title,platform,pwd,category,created_at}`
 * - link_history：`{url,title,platform,pwd,created_at}`
 * - download_record：`{share_url,platform,file_name,total_size,status,error,created_at}`
 *   **脱敏清单：不含本机保存路径 / 请求头 / 直链**（规格 §11-3 隐私裁定）。
 *
 * 解析全部走 optX 安全默认（模式对齐 [PanRouterModels]）：字段缺失/类型漂移不抛异常。
 */

/** 单条推送条目（push 请求体里的 items 元素） */
data class SyncPushItem(
    val kind: String,
    val uid: String,
    val deleted: Boolean,
    /** 本地变更时刻（毫秒）：服务端 LWW 仲裁键 */
    val updatedAt: Long,
    /** 业务 payload JSON；墓碑（deleted=true）时为 null */
    val payload: JSONObject?
) {
    fun toJson(): JSONObject = JSONObject()
        .put("kind", kind)
        .put("uid", uid)
        .put("deleted", deleted)
        .put("updated_at", updatedAt)
        .put("payload", payload ?: JSONObject.NULL)
}

/** pull 单页条数：与服务端 SyncMaxBatch 上限一致，一页拉满 */
const val SYNC_PULL_PAGE = 500

fun parseSyncPush(o: JSONObject): SyncPushResult = SyncPushResult.parse(o)

fun parseSyncPull(o: JSONObject): SyncPullResult = SyncPullResult.parse(o)

/** push 响应：`{accepted, skipped, server_time}` */
data class SyncPushResult(
    val accepted: Int,
    val skipped: Int,
    val serverTime: Long
) {
    companion object {
        fun parse(o: JSONObject) = SyncPushResult(
            accepted = o.optInt("accepted", 0),
            skipped = o.optInt("skipped", 0),
            serverTime = o.optLong("server_time", 0L)
        )
    }
}

/** pull 响应里的单条变更 */
data class SyncRemoteItem(
    /** 服务端全局变更序号（pull 游标依据） */
    val id: Long,
    val kind: String,
    val uid: String,
    val deviceId: String,
    /** 不透明 payload 字符串（客户端自行解析；空 = 墓碑通常无内容） */
    val payload: String,
    val deleted: Boolean,
    /** 客户端时钟毫秒（原变更设备的 LWW 仲裁键） */
    val clientUpdatedAt: Long
) {
    fun payloadJson(): JSONObject? =
        runCatching { JSONObject(payload) }.getOrNull()?.takeIf { it.length() > 0 }

    companion object {
        fun parse(o: JSONObject) = SyncRemoteItem(
            id = o.optLong("id", 0L),
            kind = o.optString("kind"),
            uid = o.optString("uid"),
            deviceId = o.optString("device_id"),
            payload = o.optString("payload"),
            deleted = o.optBoolean("deleted", false),
            clientUpdatedAt = o.optLong("client_updated_at", 0L)
        )
    }
}

/** pull 响应：`{items, last_seq}` */
data class SyncPullResult(
    val items: List<SyncRemoteItem>,
    /** 下一页游标（服务端当前最大变更序号；无新数据时等于请求的 since） */
    val lastSeq: Long
) {
    companion object {
        fun parse(o: JSONObject) = SyncPullResult(
            items = buildList {
                val arr = o.optJSONArray("items") ?: JSONArray()
                for (i in 0 until arr.length()) {
                    val item = arr.optJSONObject(i) ?: continue
                    // uid 为空的行无法定位本地数据，跳过（服务端不会产出，防御契约漂移）
                    val parsed = SyncRemoteItem.parse(item)
                    if (parsed.kind.isNotEmpty() && parsed.uid.isNotEmpty()) add(parsed)
                }
            },
            lastSeq = o.optLong("last_seq", 0L)
        )
    }
}

// ---- payload（客户端语义，服务端不解析） ----

/**
 * 单条 payload 的服务端上限（development.md §7.6：16KB）。超限的后果是**整批 400**
 * ——服务端逐条校验，一条超限就让整次推送失败，桌面端若不就地收敛就会每轮重推同一批、队列永久卡死。
 */
const val SYNC_PAYLOAD_MAX_BYTES = 16 * 1024

/**
 * 按 UTF-8 字节数截断，且不切断多字节字符（含代理对）。
 * 服务端按**字节**计预算，而 String.length 是 UTF-16 单元数——中文/emoji 按 length 判断会算错，
 * 必须按 toByteArray(UTF_8).size 判断与截断。
 */
internal fun String.utf8Truncate(maxBytes: Int): String {
    if (maxBytes <= 0) return ""
    val bytes = toByteArray(Charsets.UTF_8)
    if (bytes.size <= maxBytes) return this
    // 从头按 UTF-8 序列长度推进，停在预算内最后一个**完整字符**的边界——
    // 不能从尾部回退判断：边界附近的续字节可能属于一个完整字符（回退会多丢字符）。
    var end = 0
    while (end < maxBytes) {
        val b = bytes[end].toInt() and 0xFF
        val need = when {
            b < 0x80 -> 1    // ASCII
            b < 0xE0 -> 2    // 0xC0..0xDF（合法输入不会出现裸续字节 0x80..0xBF）
            b < 0xF0 -> 3    // 0xE0..0xEF
            else -> 4        // 0xF0..0xF7
        }
        if (end + need > maxBytes) break
        end += need
    }
    return String(bytes, 0, end, Charsets.UTF_8)
}

/** 各字段的字节预算（正常数据远小于此，仅作失控长度的收敛闸） */
private const val CAP_LINK = 8 * 1024
private const val CAP_TITLE = 4 * 1024
private const val CAP_ERROR = 2 * 1024
private const val CAP_FILE = 512
private const val CAP_CATEGORY = 256
private const val CAP_PWD = 128
private const val CAP_PLATFORM = 64

/** 可截断字段：越靠前越先被牺牲（error 是诊断信息，link/url 是数据本体，最后才动） */
private val SYNC_TRIMMABLE = listOf("error", "title", "file_name", "category", "pwd", "url", "link")

/**
 * 把 payload 压进服务端字节预算：
 * 1. 各字段先按 CAP_* 截断（覆盖常规情况）；
 * 2. 仍超限（JSON 转义膨胀：控制字符/引号会变成 `\u0000` 这类 6 字节序列）则反复砍最长的可截断字段一半，直到进预算。
 * 收敛后的 payload 保证 < 16KB，不会再出现「单条脏数据把整批推送卡死」。
 */
internal fun JSONObject.fittedForSync(limit: Int = SYNC_PAYLOAD_MAX_BYTES - 64): JSONObject {
    val out = JSONObject(toString())
    var guard = 0
    while (out.toString().toByteArray(Charsets.UTF_8).size > limit && guard++ < 32) {
        val key = SYNC_TRIMMABLE
            .mapNotNull { k ->
                out.optString(k).takeIf { it.isNotEmpty() }?.let { k to it.toByteArray(Charsets.UTF_8).size }
            }
            .maxByOrNull { it.second }?.first ?: break
        val cur = out.optString(key)
        out.put(key, cur.utf8Truncate(cur.toByteArray(Charsets.UTF_8).size / 2))
    }
    return out
}

/** 收藏 payload */
data class SyncBookmarkPayload(
    val link: String,
    val title: String,
    val platform: String,
    val pwd: String,
    val category: String,
    val createdAt: Long
) {
    fun toJson(): JSONObject = JSONObject()
        .put("link", link.utf8Truncate(CAP_LINK))
        .put("title", title.utf8Truncate(CAP_TITLE))
        .put("platform", platform.utf8Truncate(CAP_PLATFORM))
        .put("pwd", pwd.utf8Truncate(CAP_PWD))
        .put("category", category.utf8Truncate(CAP_CATEGORY))
        .put("created_at", createdAt)
        .fittedForSync()

    companion object {
        fun parse(o: JSONObject) = SyncBookmarkPayload(
            link = o.optString("link"),
            title = o.optString("title"),
            platform = o.optString("platform"),
            pwd = o.optString("pwd"),
            category = o.optString("category"),
            createdAt = o.optLong("created_at", 0L)
        )
    }
}

/** 解析历史 payload */
data class SyncLinkHistoryPayload(
    val url: String,
    val title: String,
    val platform: String,
    val pwd: String,
    val createdAt: Long
) {
    fun toJson(): JSONObject = JSONObject()
        .put("url", url.utf8Truncate(CAP_LINK))
        .put("title", title.utf8Truncate(CAP_TITLE))
        .put("platform", platform.utf8Truncate(CAP_PLATFORM))
        .put("pwd", pwd.utf8Truncate(CAP_PWD))
        .put("created_at", createdAt)
        .fittedForSync()

    companion object {
        fun parse(o: JSONObject) = SyncLinkHistoryPayload(
            url = o.optString("url"),
            title = o.optString("title"),
            platform = o.optString("platform"),
            pwd = o.optString("pwd"),
            createdAt = o.optLong("created_at", 0L)
        )
    }
}

/** 下载记录 payload（脱敏：不含本机保存路径 / 请求头 / 直链） */
data class SyncDownloadRecordPayload(
    val shareUrl: String,
    val platform: String,
    val fileName: String,
    val totalSize: Long,
    /** 状态码（DownloadTaskEntity.STATUS_*） */
    val status: Int,
    val error: String,
    val createdAt: Long
) {
    fun toJson(): JSONObject = JSONObject()
        .put("share_url", shareUrl.utf8Truncate(CAP_LINK))
        .put("platform", platform.utf8Truncate(CAP_PLATFORM))
        .put("file_name", fileName.utf8Truncate(CAP_FILE))
        .put("total_size", totalSize)
        .put("status", status)
        .put("error", error.utf8Truncate(CAP_ERROR))
        .put("created_at", createdAt)
        .fittedForSync()

    companion object {
        fun parse(o: JSONObject) = SyncDownloadRecordPayload(
            shareUrl = o.optString("share_url"),
            platform = o.optString("platform"),
            fileName = o.optString("file_name"),
            totalSize = o.optLong("total_size", 0L),
            status = o.optInt("status", 0),
            error = o.optString("error"),
            createdAt = o.optLong("created_at", 0L)
        )
    }
}
