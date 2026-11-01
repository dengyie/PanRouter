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

    /** 是否已具备可用配置（开关开 + 地址非空）；token 未配时解析会 401，仍算已配置（可游客解析免登录盘） */
    fun isConfigured(): Boolean = enabled && baseUrl.isNotEmpty()

    /** 中转（/stream）路径的任务级线程数上限：pxed 上行带宽是多设备共享瓶颈（规格 §8.5） */
    fun relayThreadsFor(platform: String): Int =
        minOf(SettingsRepository().downloadThreadsFor(platform), RELAY_MAX_THREADS)

    companion object {
        internal const val KEY_ENABLED = "panrouter_enabled"
        internal const val KEY_BASE_URL = "panrouter_base_url"
        internal const val KEY_FALLBACK_LOCAL = "panrouter_fallback_local"

        /** 中转流分片并发上限（规格 §8.5）：直连路径维持用户设置，不在此限 */
        internal const val RELAY_MAX_THREADS = 4
    }
}
