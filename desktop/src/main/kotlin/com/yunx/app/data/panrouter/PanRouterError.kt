package com.yunx.app.data.panrouter

import org.json.JSONObject

/**
 * PanRouter 错误模型：与服务端九种错误 kind 一一对应（panrouter/docs/development.md §8），
 * 另加 NETWORK 表示桌面端自身的网络层失败（连不上 / 超时 / DNS）。
 *
 * 错误链完整保留 [cause]；日志由调用方单点记录，本类只承载数据与分类判定。
 */
class PanRouterError(
    message: String,
    val kind: Kind,
    /** 服务端明确可重试（upstream 一类）；客户端网络失败一律 true */
    val retriable: Boolean,
    /** 服务端 HTTP 状态码；网络失败为 0 */
    val httpStatus: Int = 0,
    cause: Throwable? = null
) : RuntimeException(message, cause) {

    enum class Kind {
        /** PanRouter 站点会话缺失或失效（含 api_token 错误——桌面端不清 token，报错引导去设置页修正） */
        SESSION_EXPIRED,
        /** PanRouter 登录口令错误 */
        AUTH_INVALID,
        /** 网盘上游 Cookie/凭据失效（服务端账号池问题，与桌面端会话无关） */
        AUTH_EXPIRED,
        /** 风控 / 限流 / 验证码 / 熔断中 */
        RISK_CONTROL,
        /** 分享失效 */
        SHARE_GONE,
        /** 文件不存在 / 参数缺失 */
        NOT_FOUND,
        /** 服务端接口改版（返回结构解析失败） */
        INTERFACE_CHANGED,
        /** 服务端上游 5xx / 网络错误 */
        UPSTREAM_ERROR,
        /** 功能暂不支持（该盘未接 driver） */
        UNSUPPORTED,
        /** 桌面端网络层失败（连不上 / 超时 / DNS）——服务端不会返回此 kind */
        NETWORK
    }

    /** 面向用户的文案（不暴露内部细节，保留服务端 UserHint 主体） */
    fun userMessage(): String = when (kind) {
        Kind.SESSION_EXPIRED -> "PanRouter 连接已失效，请到设置重新配置 api_token"
        Kind.AUTH_INVALID -> "PanRouter 管理员口令错误"
        Kind.AUTH_EXPIRED -> "PanRouter 账号池凭据已失效，请到服务端更新；$message"
        Kind.RISK_CONTROL -> "服务端限频/风控中：$message"
        Kind.SHARE_GONE -> "分享已失效（服务端确认）"
        Kind.NOT_FOUND -> message?.ifBlank { "文件不存在" } ?: "文件不存在"
        Kind.INTERFACE_CHANGED -> "PanRouter 接口待适配（服务端网盘接口可能改版）"
        Kind.UPSTREAM_ERROR -> "PanRouter 上游异常：$message"
        Kind.UNSUPPORTED -> "PanRouter 暂不支持该网盘"
        Kind.NETWORK -> "无法连接 PanRouter：$message"
    }

    /**
     * 失败后是否值得回落本地解析（规格 §3.2 / §5.2）：
     * 风控 / 不支持 / 上游错误 / 接口改版 / 网络失败 → 可回落；
     * 分享失效 / 文件不存在 → 回落也必然失败，直接透出；
     * 凭据类（session/auth_invalid）由调用方先尝试修复，不在此自动回落。
     */
    val shouldFallbackToLocal: Boolean
        get() = when (kind) {
            Kind.RISK_CONTROL,
            Kind.UNSUPPORTED,
            Kind.UPSTREAM_ERROR,
            Kind.INTERFACE_CHANGED,
            Kind.NETWORK -> true
            else -> false
        }

    companion object {
        /** kind 字符串 → 枚举；未知 kind 兜底为 UPSTREAM_ERROR（规格 §10） */
        internal fun kindOf(raw: String): Kind = when (raw) {
            "session_expired" -> Kind.SESSION_EXPIRED
            "auth_invalid" -> Kind.AUTH_INVALID
            "auth_expired" -> Kind.AUTH_EXPIRED
            "risk_control" -> Kind.RISK_CONTROL
            "share_gone" -> Kind.SHARE_GONE
            "not_found" -> Kind.NOT_FOUND
            "interface_changed" -> Kind.INTERFACE_CHANGED
            "upstream_error" -> Kind.UPSTREAM_ERROR
            "unsupported" -> Kind.UNSUPPORTED
            else -> Kind.UPSTREAM_ERROR
        }

        /** 解析服务端错误体 `{code, kind, message, retriable}`；body 不是 JSON 时按状态码兜底 */
        fun fromHttp(status: Int, bodyText: String?): PanRouterError {
            val json = bodyText?.let { runCatching { JSONObject(it) }.getOrNull() }
            val kind = json?.optString("kind")?.takeIf { it.isNotEmpty() }?.let(::kindOf)
                ?: defaultKindFor(status)
            val message = json?.optString("message").orEmpty()
                .ifBlank { "HTTP $status" }
            val retriable = json?.optBoolean("retriable", false) ?: (status >= 500)
            return PanRouterError(message, kind, retriable, status)
        }

        /** 无错误体时的状态码兜底映射（对齐服务端 kindStatus 表；其余一律按上游/服务端异常处理） */
        internal fun defaultKindFor(status: Int): Kind = when (status) {
            401 -> Kind.SESSION_EXPIRED
            429 -> Kind.RISK_CONTROL
            404 -> Kind.NOT_FOUND
            400 -> Kind.UNSUPPORTED
            else -> Kind.UPSTREAM_ERROR
        }

        /** 桌面端网络层失败统一包装（retriable=true，供退避与回落判定） */
        fun fromNetwork(cause: Throwable): PanRouterError = PanRouterError(
            message = cause.message ?: cause.javaClass.simpleName,
            kind = Kind.NETWORK,
            retriable = true,
            httpStatus = 0,
            cause = cause
        )
    }
}
