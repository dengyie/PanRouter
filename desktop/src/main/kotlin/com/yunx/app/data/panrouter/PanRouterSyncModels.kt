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
        .put("link", link)
        .put("title", title)
        .put("platform", platform)
        .put("pwd", pwd)
        .put("category", category)
        .put("created_at", createdAt)

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
        .put("url", url)
        .put("title", title)
        .put("platform", platform)
        .put("pwd", pwd)
        .put("created_at", createdAt)

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
        .put("share_url", shareUrl)
        .put("platform", platform)
        .put("file_name", fileName)
        .put("total_size", totalSize)
        .put("status", status)
        .put("error", error)
        .put("created_at", createdAt)

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
