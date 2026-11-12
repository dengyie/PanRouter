package com.yunx.app.data.panrouter

import com.yunx.app.data.db.AppDatabase
import com.yunx.app.data.download.DownloadPlatform
import com.yunx.app.data.network.BaiduConstants

/**
 * 本机可贡献给 PanRouter 账号池的网盘账号（规格 §5.3 M2「账号贡献」）。
 *
 * 只枚举**服务端覆盖集内**的平台（夸克 / 蓝奏云 / 百度，对齐 ResolveRouter）；
 * 凭据经 SecureAccountDaos 透明解密读出 —— 仅在贡献对话框打开期间驻留内存
 * （**不落日志、不回显、不入持久化**），上传请求发出后随对话框关闭而释放。
 *
 * 无状态：可按需构造（MainScreen 侧持有 db）。
 */
class PanRouterContribution(private val db: AppDatabase) {

    /** 一条可贡献账号（credential 仅在贡献请求时使用，UI 层不得持有） */
    data class Contributable(
        val platform: String,
        /** 服务端 pan_type（quark / lanzou / baidu） */
        val panType: String,
        /** 展示名：备注优先，回退昵称，再回退账号 id */
        val displayName: String,
        val isPrimary: Boolean,
        /** 是否已存在于服务端账号池（按展示名粗匹配，仅作展示提示） */
        val alreadyOnServer: Boolean,
        val credential: String
    )

    /**
     * 枚举覆盖平台的本地账号。[serverNames] 为服务端账号池现有名字集合（展示去重提示用）。
     * 单个平台读取失败不拖垮整体（返回其余平台），账号空 Cookie 的行跳过。
     */
    suspend fun list(serverNames: Set<String> = emptySet()): List<Contributable> {
        val out = ArrayList<Contributable>(8)
        val quark = runCatching { db.quarkAccountDao().getAccounts() }.getOrDefault(emptyList())
        quark.forEach { a ->
            if (a.cookie.isBlank()) return@forEach
            out += contributable(DownloadPlatform.QUARK, "quark", a.nickname, a.remark, a.isPrimary, a.cookie, serverNames)
        }
        val lanzou = runCatching { db.lanzouAccountDao().getAccounts() }.getOrDefault(emptyList())
        lanzou.forEach { a ->
            if (a.cookie.isBlank()) return@forEach
            out += contributable(DownloadPlatform.LANZOU, "lanzou", a.nickname, a.remark, a.isPrimary, a.cookie, serverNames)
        }
        // 百度网盘：服务端 baidu driver 是多账号池,转存有 50 次/天/账号硬闸,
        // 贡献多个账号能线性分散配额与风控面。Cookie 仅含 BDUSS/STOKEN,无设备指纹。
        val baidu = runCatching { db.baiduAccountDao().getAccounts() }.getOrDefault(emptyList())
        baidu.forEach { a ->
            if (!BaiduConstants.isValidCookie(a.cookie)) return@forEach
            out += contributable(DownloadPlatform.BAIDU, "baidu", a.nickname, a.remark, a.isPrimary, a.cookie, serverNames)
        }
        return out
    }

    private fun contributable(
        platform: String,
        panType: String,
        nickname: String,
        remark: String,
        isPrimary: Boolean,
        cookie: String,
        serverNames: Set<String>
    ): Contributable {
        val display = remark.ifBlank { nickname.ifBlank { platform } }
        return Contributable(
            platform = platform,
            panType = panType,
            displayName = display,
            isPrimary = isPrimary,
            alreadyOnServer = display in serverNames,
            credential = cookie
        )
    }
}
