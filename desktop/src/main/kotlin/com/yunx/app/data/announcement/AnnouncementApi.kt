package com.yunx.app.data.announcement

import com.yunx.app.data.network.GitHubMirror
import com.yunx.app.data.network.HttpClients
import com.yunx.app.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.Request
import org.json.JSONArray
import org.json.JSONObject

/**
 * 应用内公告客户端（PanRouter 整合版自有数据源）。
 *
 * **数据源 = 本仓库根目录的 `app-announcements.json` 静态文件**
 * （`raw.githubusercontent.com/dengyie/PanRouter/main/app-announcements.json`，
 * 经 [GitHubMirror] 镜像前缀访问，失败回落 raw 直连）。
 *
 * 为什么不用上游作者的公告服务器：那是安卓原作者的个人虚拟主机（明文 HTTP），
 * fork 的公告数据源必须自有化——发布一条公告 = 往仓库根的 JSON 里加一个对象并提交，
 * 无需任何服务端。**没有分页/浏览量统计**这些服务端能力，对应简化：
 * - 列表一次全量返回（[fetchPage] 忽略 page/pageSize，恒 `hasMore=false`）；
 * - 详情不再单独请求（也不再有 viewCount +1 副作用），直接从同一份数据里按 id 查。
 *
 * 文件格式（字段名与原服务端契约保持一致，客户端其余层零改动）：
 * ```json
 * { "announcements": [ { "id", "title", "summary", "content", "publishAt",
 *     "author", "publisher": {"name","avatarUrl"}, "isPinned", "targets": ["desktop"] } ] }
 * ```
 * - `content` 支持 Markdown（渲染与 README 预览同一套 GFM 渲染器，不注入 WebView）；
 * - `targets` 缺省/为空 = 全平台可见；只发给电脑端的公告写 `["desktop"]`；
 * - 发布时间未到的公告对用户不可见（定时发布语义与原服务端一致，只是改在客户端判）。
 *
 * 日志统一走 [Log]，失败原因一律 E 级打印，方便直接看日志定位。
 */
object AnnouncementApi {

    /**
     * 公告数据文件在本仓库的 raw 地址。**测试守卫**：[com.yunx.app.data.update.UpdateChecker]
     * 所在更新通道测试同样断言这里必须指向 dengyie/PanRouter、不得回落上游作者域名。
     */
    const val ANNOUNCEMENTS_RAW_URL =
        "https://raw.githubusercontent.com/dengyie/PanRouter/main/app-announcements.json"

    /**
     * 列表分页大小（保留常量：ViewModel 的启动检查/刷新调用点都带着它）。
     * 静态源一次全量返回，这个值不再限制实际条数。
     */
    const val PAGE_SIZE = 100

    /** 公告相关日志统一走这个标签 */
    private const val TAG = "YunX-Announce"

    /** targets 数组里表示电脑端的值 */
    private const val TARGET_DESKTOP = "desktop"

    /** 发布者（`publisher` 对象） */
    data class Publisher(val name: String, val avatarUrl: String?)

    /**
     * 公告对象（字段名与上游公告系统契约一致）。
     * [content] 在静态源里**始终随列表返回**（没有单独的详情接口）。
     */
    data class Announcement(
        val id: String,
        val title: String,
        val summary: String,
        val content: String?,
        val coverImage: String?,
        val images: List<String>,
        val publishAt: String?,
        val author: String,
        val viewCount: Long,
        val publisher: Publisher,
        val isPinned: Boolean,
        val pinnedAt: String?,
        val pinExpireAt: String?,
        val sortOrder: Int,
        val createdAt: String,
        val updatedAt: String,
        /** 可见平台；空 = 全平台。只发给电脑端的公告为 ["desktop"]（静态源新增字段，上游契约没有） */
        val targets: List<String> = emptyList(),
    ) {
        /** 生效时间（毫秒）：`publishAt` 为空表示「立即发布」，退回 createdAt；两者都解析不出来才是 0 */
        val effectiveMillis: Long get() = parseIsoMillis(publishAt) ?: parseIsoMillis(createdAt) ?: 0L
    }

    /** 分页结果（静态源恒为单页全量） */
    data class Page(
        val total: Int,
        val page: Int,
        val pageSize: Int,
        val totalPages: Int,
        val hasMore: Boolean,
        val list: List<Announcement>,
    )

    /** 请求结果：失败带上可直接展示的原因（HTTP 码 / 异常信息 / 解析失败） */
    sealed interface Result<out T> {
        data class Success<T>(val data: T) : Result<T>
        data class Failure(val message: String) : Result<Nothing>
    }

    /** 获取公告列表。**每次调用都重新下载**（与上游「列表请求永远打服务端」的契约一致，
     *  手动刷新/列表页重开靠它拿到最新数据）；[page]/[pageSize] 仅保留签名兼容，静态源恒返回第 1 页全量 */
    suspend fun fetchPage(page: Int = 1, pageSize: Int = PAGE_SIZE): Result<Page> {
        val list = when (val r = reloadFromSource()) {
            is Result.Success -> r.data
            is Result.Failure -> return r
        }
        return Result.Success(
            Page(
                total = list.size,
                page = 1,
                pageSize = list.size.coerceAtLeast(1),
                totalPages = 1,
                hasMore = false,
                list = list
            )
        )
    }

    /**
     * 获取公告详情（含正文）。静态源没有单独详情接口：**缓存优先**（与上游「详情按 id 缓存、
     * 不重复请求」的契约一致），缓存里没有这条 id 时才重新下载一份再查；
     * 查不到（下架/写错 id）返回 Failure，message 可直接展示。
     */
    suspend fun fetchDetail(id: String): Result<Announcement> {
        val wanted = id.trim()
        var list = when (val r = peekCache()) {
            is Result.Success -> r.data
            is Result.Failure -> return r
        }
        if (list.firstOrNull { it.id == wanted } == null) {
            list = when (val r = reloadFromSource()) {
                is Result.Success -> r.data
                is Result.Failure -> return r
            }
        }
        val hit = list.firstOrNull { it.id == wanted }
        return if (hit != null) Result.Success(hit)
        else Result.Failure("公告不存在或已下架")
    }

    /**
     * 内存缓存：详情查找优先走它，避免每次进详情页都下载；
     * 列表（[fetchPage]）每次成功下载后覆盖它。失败时**不清空**——断网时刷新列表
     * 返回失败，但已加载的公告在详情页/未读角标口径里仍然可用。
     */
    @Volatile
    private var cache: List<Announcement>? = null

    /** 仅供测试注入：文档加载器（默认走网络镜像链路） */
    internal var documentLoader: suspend () -> Result<JSONObject> = { fetchDocument() }

    /** 清空缓存（测试用；运行时刷新走重新 fetch 即可覆盖） */
    internal fun clearCacheForTest() {
        cache = null
    }

    /** 缓存快照；无缓存时（详情首开且列表尚未拉过）下载一份填充 */
    private suspend fun peekCache(): Result<List<Announcement>> {
        cache?.let { return Result.Success(it) }
        return reloadFromSource()
    }

    /** 强制从数据源重新下载 → 过滤排序 → 覆盖缓存 */
    private suspend fun reloadFromSource(): Result<List<Announcement>> {
        return when (val r = documentLoader()) {
            is Result.Failure -> r
            is Result.Success -> {
                val visible = visibleAnnouncements(r.data)
                cache = visible
                Result.Success(visible)
            }
        }
    }

    /** 下载公告 JSON：先走 GitHub 镜像前缀（国内直连 raw 常不可达），失败回落 raw 直连 */
    internal suspend fun fetchDocument(): Result<JSONObject> = withContext(Dispatchers.IO) {
        val prefixes = listOf(GitHubMirror.activePrefix(), "").filter { it.isNotEmpty() }.distinct()
        // 记录最后一次尝试的失败原因（而非第一条）：两个源都失败时，用户看到的应是最末次直连的失败原因
        var lastMessage = "网络异常"
        for (prefix in prefixes) {
            val url = prefix + ANNOUNCEMENTS_RAW_URL
            val outcome = runCatching {
                val call = Request.Builder()
                    .url(url)
                    .header("Accept", "application/json")
                    .header("User-Agent", "YunX-Desktop")
                    .get()
                    .build()
                HttpClients.apiClient().newCall(call).execute().use { resp ->
                    val text = resp.body?.string().orEmpty()
                    if (!resp.isSuccessful || text.isBlank()) {
                        Log.e(TAG, "公告数据请求失败（HTTP ${resp.code}，$url）")
                        return@use null to "响应为空（HTTP ${resp.code}）"
                    }
                    try {
                        JSONObject(text) to ""
                    } catch (e: Exception) {
                        Log.e(TAG, "公告数据不是合法 JSON（$url，前 200 字=${text.take(200)}）", e)
                        null to "响应格式异常"
                    }
                }
            }.getOrElse { e ->
                Log.e(TAG, "公告请求异常（$url）：${e.javaClass.simpleName}: ${e.message}", e)
                null to "${e.javaClass.simpleName}: ${e.message ?: "网络异常"}"
            }
            val json = outcome.first
            if (json != null) return@withContext Result.Success(json)
            lastMessage = outcome.second
        }
        Result.Failure(lastMessage)
    }

    /**
     * 从整份文档得出「用户可见」的公告列表：过滤未发布/定时未到/非本平台，
     * 置顶（未过期）在前，其余按生效时间倒序——静态文件里随便写顺序，展示顺序在这里定。
     */
    internal fun visibleAnnouncements(document: JSONObject): List<Announcement> {
        val now = System.currentTimeMillis()
        val all = parseAnnouncementArray(document.optJSONArray("announcements") ?: JSONArray())
        return all
            .filter { it.effectiveMillis <= now || it.effectiveMillis == 0L }
            .filter { it.targets.isEmpty() || TARGET_DESKTOP in it.targets }
            .sortedWith(
                compareByDescending<Announcement> { it.isPinned && !pinExpired(it, now) }
                    .thenByDescending { it.effectiveMillis }
            )
    }

    /** 置顶过期：pinExpireAt 已过 → 不再算置顶（与原服务端语义一致，改在客户端判） */
    private fun pinExpired(item: Announcement, nowMs: Long): Boolean =
        parseIsoMillis(item.pinExpireAt)?.let { it <= nowMs } == true

    private fun parseAnnouncementArray(arr: JSONArray): List<Announcement> {
        val out = ArrayList<Announcement>(arr.length())
        for (i in 0 until arr.length()) {
            val item = arr.optJSONObject(i) ?: continue
            val parsed = parseAnnouncement(item)
            // id 是列表 key（LazyColumn 的 key 必须唯一）与已读记录的键：空 id 直接丢掉，
            // 否则一条脏数据就会让整个列表抛 "Key was already used"。
            if (parsed.id.isEmpty()) {
                Log.w(TAG, "公告列表第 $i 条缺少 id，已跳过（title=${parsed.title.take(30)}）")
                continue
            }
            out.add(parsed)
        }
        return out
    }

    private fun parseStringArray(arr: JSONArray): List<String> {
        val out = ArrayList<String>(arr.length())
        for (i in 0 until arr.length()) {
            if (arr.isNull(i)) continue
            val text = arr.optString(i).trim()
            if (text.isNotEmpty()) out.add(text)
        }
        return out
    }

    private fun parseAnnouncement(json: JSONObject): Announcement = Announcement(
        id = json.stringOrNull("id").orEmpty(),
        title = json.stringOrNull("title").orEmpty(),
        summary = json.stringOrNull("summary").orEmpty(),
        content = json.stringOrNull("content"),
        coverImage = json.stringOrNull("coverImage"),
        images = json.optJSONArray("images")?.let { parseStringArray(it) } ?: emptyList(),
        publishAt = json.stringOrNull("publishAt"),
        author = json.stringOrNull("author").orEmpty(),
        viewCount = json.optLong("viewCount"),
        publisher = json.optJSONObject("publisher")?.let { p ->
            Publisher(
                name = p.stringOrNull("name").orEmpty(),
                avatarUrl = p.stringOrNull("avatarUrl")
            )
        } ?: Publisher(name = "", avatarUrl = null),
        isPinned = json.optBoolean("isPinned"),
        pinnedAt = json.stringOrNull("pinnedAt"),
        pinExpireAt = json.stringOrNull("pinExpireAt"),
        sortOrder = json.optInt("sortOrder"),
        createdAt = json.stringOrNull("createdAt").orEmpty(),
        updatedAt = json.stringOrNull("updatedAt").orEmpty(),
        targets = json.optJSONArray("targets")?.let { parseStringArray(it) } ?: emptyList()
    )

    /** ISO-8601 → 毫秒；解析失败返回 null（调用方退回 createdAt / 0） */
    internal fun parseIsoMillis(iso: String?): Long? {
        if (iso.isNullOrBlank()) return null
        return runCatching {
            java.time.OffsetDateTime.parse(iso.trim()).toInstant().toEpochMilli()
        }.getOrNull()
    }
}

/**
 * `optString` 遇到 JSON null 会返回字符串 `"null"`（不是空串），可空字段一律走这里。
 * 「字段缺失」与「字段为 null」在数据文件里是两种含义（如 publishAt = null 表示立即发布），
 * 所以这里只做「取不到 / 为 null ⇒ null」，空串也算没值（避免把 "" 当时间/图片地址用）。
 */
private fun JSONObject.stringOrNull(key: String): String? =
    if (isNull(key)) null else optString(key).trim().takeIf { it.isNotEmpty() }
