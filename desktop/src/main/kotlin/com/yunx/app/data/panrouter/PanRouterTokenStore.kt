package com.yunx.app.data.panrouter

import com.yunx.app.AppContext
import com.yunx.app.data.security.FileCredentialCipher
import com.yunx.app.util.Log

/**
 * PanRouter api_token 加密存储（模式逐行对齐 [GitHubTokenStore][com.yunx.app.data.network.GitHubTokenStore]）。
 *
 * 安全说明：
 * - api_token 是**长期不过期**的服务端凭证（规格决策 #2），泄露面大于 7 天 JWT，因此**严禁明文存储**：
 *   统一经 [FileCredentialCipher] AES-GCM 加密（密钥不可导出）后落盘；
 * - **严禁输出到日志**，**严禁在 UI 回显完整 token**（输入框用 PasswordVisualTransformation）。
 */
object PanRouterTokenStore {

    private const val KEY_TOKEN = "panrouter_api_token_encrypted"
    private const val PURPOSE = "panrouter_api_token"

    private val cipher = FileCredentialCipher()

    /** 读取已加密 token 并解密；未配置或当前解不开返回 null（不解开时**不删**，密钥恢复后自动可用） */
    fun getToken(): String? {
        val stored = AppContext.miscPrefs.get(KEY_TOKEN, null) ?: return null
        return runCatching { cipher.decrypt(stored, PURPOSE) }
            .getOrNull()
            ?.takeIf { it.isNotBlank() }
    }

    /** 设置/更新 token；传 null 或空串则清除。返回是否确实写入成功（加密失败保留原值） */
    fun setToken(token: String?): Boolean {
        val prefs = AppContext.miscPrefs
        val trimmed = token?.trim().orEmpty()
        if (trimmed.isEmpty()) {
            prefs.remove(KEY_TOKEN)
            return true
        }
        return runCatching {
            prefs.put(KEY_TOKEN, cipher.encrypt(trimmed, PURPOSE))
            true
        }.getOrElse {
            Log.w("YunX-PanRouter", "api_token 加密失败，保留原值：${it.message ?: it.javaClass.simpleName}")
            false
        }
    }

    /** 是否「已配置且当前能读出来」——界面按此判断，避免显示已配置实际每请求都裸奔 */
    fun hasToken(): Boolean = getToken() != null
}
