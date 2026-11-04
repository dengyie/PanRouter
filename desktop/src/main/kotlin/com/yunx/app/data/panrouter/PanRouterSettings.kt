package com.yunx.app.data.panrouter

import com.yunx.app.AppContext
import com.yunx.app.data.prefs.SettingsRepository
import java.util.prefs.Preferences

/**
 * PanRouter 集成设置（java.util.prefs 持久化，模式对齐 [SettingsRepository]）。
 *
 * 实例无状态（每次读写直连偏好节点），可随处构造；改设置即时生效，无需重启。
 * api_token 不在这里 —— 加密存储见 [PanRouterTokenStore]。
 */
class PanRouterSettings {

    private val prefs: Preferences = AppContext.settingsPrefs

    /** 是否启用服务端解析（关闭后一切走本地原生链，行为与未集成时一致） */
    var enabled: Boolean
        get() = prefs.getBoolean(KEY_ENABLED, false)
        set(value) = prefs.putBoolean(KEY_ENABLED, value)

    /**
     * 服务端地址（如 `https://pan.example.com`）；去除尾部 `/`。
     * 空串视为未配置 —— 未配置时即使开关打开也走本地链。
     */
    var baseUrl: String
        get() = prefs.get(KEY_BASE_URL, "").trim().trimEnd('/')
        set(value) {
            val v = value.trim().trimEnd('/')
            if (v.isEmpty()) prefs.remove(KEY_BASE_URL) else prefs.put(KEY_BASE_URL, v)
        }

    /** 服务端解析失败后是否自动回落本地（规格 §3.2「失败回落本地」开关） */
    var fallbackToLocal: Boolean
        get() = prefs.getBoolean(KEY_FALLBACK_LOCAL, true)
        set(value) = prefs.putBoolean(KEY_FALLBACK_LOCAL, value)

    /**
     * 是否启用历史同步（M3）：收藏/解析历史/下载记录与本仓 PanRouter 服务端多设备双向同步。
     * **默认关闭**（隐私 opt-in，规格 §11-3：回传 = 使用记录写进服务端，须用户显式开启）；
     * 开启还需 [isConfigured]（服务端地址已配）且 api_token 已配（sync 端点挂 auth）。
     */
    var historySyncEnabled: Boolean
        get() = prefs.getBoolean(KEY_HISTORY_SYNC, false)
        set(value) = prefs.putBoolean(KEY_HISTORY_SYNC, value)

    /** pull 游标：服务端 sync_items 全局变更序号（只增不减；0 = 下次全量） */
    var syncCursor: Long
        get() = prefs.getLong(KEY_SYNC_CURSOR, 0L)
        set(value) = prefs.putLong(KEY_SYNC_CURSOR, value)

    /** 最近一次成功完成整轮同步的时刻（毫秒；0 = 从未同步），设置页展示用 */
    var syncLastAt: Long
        get() = prefs.getLong(KEY_SYNC_LAST_AT, 0L)
        set(value) = prefs.putLong(KEY_SYNC_LAST_AT, value)

    /**
     * 本机设备标识（服务端按它区分变更来源，管理页可见）。
     * 首次访问时生成：主机名（拿不到则 "desktop"）消毒截断 + 4 位随机后缀，此后稳定不变。
     */
    val deviceId: String
        get() = prefs.get(KEY_DEVICE_ID, null)?.takeIf { it.isNotBlank() } ?: run {
            val host = runCatching { java.net.InetAddress.getLocalHost().hostName }
                .getOrNull()?.trim().orEmpty()
            val sanitized = host.filter { it.isLetterOrDigit() || it == '-' }
                .take(24)
                .ifEmpty { "desktop" }
            val id = "$sanitized-" + (1000..9999).random()
            prefs.put(KEY_DEVICE_ID, id)
            id
        }

    /** 是否已具备可用配置（开关开 + 地址非空）；token 未配时解析会 401，仍算已配置（可游客解析免登录盘） */
    fun isConfigured(): Boolean = enabled && baseUrl.isNotEmpty()

    /** 历史同步是否具备完整前提（总开关 + 服务端地址 + api_token 三者齐备） */
    fun isSyncReady(): Boolean = historySyncEnabled && isConfigured() && PanRouterTokenStore.hasToken()

    /** 中转（/stream）路径的任务级线程数上限：pxed 上行带宽是多设备共享瓶颈（规格 §8.5） */
    fun relayThreadsFor(platform: String): Int =
        minOf(SettingsRepository().downloadThreadsFor(platform), RELAY_MAX_THREADS)

    companion object {
        internal const val KEY_ENABLED = "panrouter_enabled"
        internal const val KEY_BASE_URL = "panrouter_base_url"
        internal const val KEY_FALLBACK_LOCAL = "panrouter_fallback_local"
        internal const val KEY_HISTORY_SYNC = "panrouter_history_sync_enabled"
        internal const val KEY_SYNC_CURSOR = "panrouter_sync_cursor"
        internal const val KEY_SYNC_LAST_AT = "panrouter_sync_last_at"
        internal const val KEY_DEVICE_ID = "panrouter_device_id"

        /** 中转流分片并发上限（规格 §8.5）：直连路径维持用户设置，不在此限 */
        internal const val RELAY_MAX_THREADS = 4
    }
}
