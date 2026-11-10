package com.yunx.app.data.panrouter

import org.json.JSONObject

/**
 * M4 云对云转存（服务端 `POST /api/v1/transfer/mangodrive`）契约模型。
 *
 * 语义：桌面端把「已解析出的直链」交给服务端，服务端直连上游与私有盘两端搬运字节，
 * 全程不经过本机 —— 适合大文件，也不需要本机一直开着。任务在服务端内存里异步推进，
 * 桌面端用 [PanRouterClient.transferTask] 轮询进度。
 */

/** 转存通道（服务端 `channel` 字段；空 = auto）。 */
object TransferChannel {
    /** 服务端拉上游 → 流式 PUT 到私有盘：兼容任意提取链约束（UA/Referer/Cookie），最稳 */
    const val STREAM = "stream"

    /** 交给私有盘远端 aria2 直拉：省服务端带宽，但上游必须公开可直连（带约束的链会失败） */
    const val OFFLINE = "offline"

    /** 有提取链约束走 stream，否则 offline（默认） */
    const val AUTO = "auto"

    /** 界面文案 */
    fun labelOf(channel: String): String = when (channel) {
        STREAM -> "服务端接力"
        OFFLINE -> "远端离线"
        AUTO -> "自动选择"
        else -> channel
    }
}

/** 一次转存请求。[destDir] 空 = 服务端默认目录；[name] 空 = 沿用源文件名。 */
data class TransferRequest(
    val sourceUrl: String,
    val pwd: String = "",
    val fid: String = "",
    val destDir: String = "",
    val name: String = "",
    val channel: String = TransferChannel.AUTO,
    val ua: String = ""
)

/** 转存任务快照（服务端 `TransferTask`）。 */
data class TransferTask(
    val id: String,
    /** queued | running | pending | done | failed */
    val state: String,
    val channel: String,
    val sourceUrl: String,
    val destPath: String,
    val fileName: String,
    val size: Long,
    val done: Long,
    val error: String,
    val hint: String,
    val startedAtMs: Long,
    val endedAtMs: Long
) {
    val isTerminal: Boolean get() = state == STATE_DONE || state == STATE_FAILED
    val isRunning: Boolean get() = state == STATE_RUNNING || state == STATE_QUEUED || state == STATE_PENDING

    /**
     * 0..1；未知总大小（size <= 0）时为 null，界面用不确定进度条。
     *
     * pending（离线受理、产物尚未落池）同样返回 null：此时已知的 size 只是源端
     * 预测大小，字节并没有落到私有盘，给出确定进度会让用户误以为已经搬完。
     */
    val progress: Float?
        get() = when {
            state == STATE_PENDING -> null
            size > 0L -> (done.toDouble() / size.toDouble()).toFloat().coerceIn(0f, 1f)
            else -> null
        }

    companion object {
        const val STATE_QUEUED = "queued"
        const val STATE_RUNNING = "running"
        /** 远端已受理（离线通道），产物尚未落池确认：仍占着目标路径，需继续轮询 */
        const val STATE_PENDING = "pending"
        const val STATE_DONE = "done"
        const val STATE_FAILED = "failed"

        fun stateLabelOf(state: String): String = when (state) {
            STATE_QUEUED -> "排队中"
            STATE_RUNNING -> "搬运中"
            STATE_PENDING -> "落池中"
            STATE_DONE -> "已完成"
            STATE_FAILED -> "失败"
            else -> state
        }
    }
}

/** `TransferTask` JSON 快照 → 模型（服务端时间戳为 RFC3339 字符串，转 epoch ms 供 UI 排序）。 */
internal fun parseTransferTask(o: JSONObject): TransferTask = TransferTask(
    id = o.optString("id"),
    state = o.optString("state", "queued"),
    channel = o.optString("channel"),
    sourceUrl = o.optString("source_url"),
    destPath = o.optString("dest_path"),
    fileName = o.optString("file_name"),
    size = o.optLong("size", 0L),
    done = o.optLong("done", 0L),
    error = o.optString("error"),
    hint = o.optString("hint"),
    startedAtMs = parseEpochMs(o.optString("started_at")),
    endedAtMs = parseEpochMs(o.optString("ended_at"))
)
