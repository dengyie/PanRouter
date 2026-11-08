package com.yunx.app.data.mangodrive

import com.yunx.app.AppContext
import java.util.prefs.Preferences

/**
 * 自建云盘（OpenList 兼容端点）的本地设置 —— java.util.prefs 持久化，模式对齐 PanRouterSettings。
 *
 * ★ 这里**不内置任何部署信息**：具体是哪一套存储（域、目录、容量、加密方式）完全由
 *   PanRouter 服务端的 `transfer` 配置决定，桌面端通过 `GET /api/v1/storage` 取回端点基址。
 *   因此本类只保留一个「手动覆盖」入口，用于服务端不可达时临时指定地址。
 *
 * 凭据同样不在本机：服务端以加密形式持有端点 token，桌面端只经 mangodrive driver 列目录 / 提链。
 */
class MangoDriveSettings(private val prefs: Preferences = AppContext.settingsPrefs) {

    /**
     * 手动覆盖的端点基址（如 `https://pool.example.com`），去尾斜杠。
     * 空串 = 不覆盖，改用服务端 `GET /api/v1/storage` 下发的 `public_base`。
     */
    var publicBaseUrl: String
        get() = prefs.get(KEY_PUBLIC_BASE, "").trim().trimEnd('/')
        set(value) {
            val v = value.trim().trimEnd('/')
            if (v.isEmpty()) prefs.remove(KEY_PUBLIC_BASE) else prefs.put(KEY_PUBLIC_BASE, v)
        }

    companion object {
        internal const val KEY_PUBLIC_BASE = "mangodrive_public_base"
    }
}
