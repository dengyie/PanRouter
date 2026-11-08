package com.yunx.app.data.panrouter

import com.yunx.app.data.download.DownloadPlatform
import com.yunx.app.data.network.SharePlatform

/**
 * 解析择路决策器（规格 §3.2）——纯函数、零框架依赖、O(1) 表驱动（规格 §7.2）。
 *
 * 输入：平台 × 用户开关 × 服务端健康 × 服务端覆盖集；
 * 输出：REMOTE（走服务端解析）/ LOCAL（本地原生链）。
 * 时间相关输入一律由调用方传入（本对象不读时钟，规格 §7.6）。
 *
 * 「REMOTE 失败后回落 LOCAL 一次」不在这里 —— 那是用例编排（Gateway/ViewModel）的事，
 * 决策器只回答「给定当前状态该走哪条路」。
 */
object ResolveRouter {

    enum class Route { REMOTE, LOCAL }

    /**
     * 服务端 driver 覆盖集（M1，对应 panrouter 生产 v1.1.12：quark / lanzou）。
     * 服务端 M2 扩容（123/UC/139/189）后随版本更新此表；
     * 运行期「服务端说不支持」的自学习覆盖修正见 [PanRouterResolveGateway] 的 uncovered 集。
     */
    private val SERVER_COVERAGE: Map<SharePlatform, String> = mapOf(
        SharePlatform.QUARK to "quark",
        SharePlatform.LANZOU to "lanzou"
    )

    /** 平台 → 服务端 pan 标识；未覆盖返回 null（调用方据此走 LOCAL） */
    fun serverPanFor(platform: SharePlatform): String? = SERVER_COVERAGE[platform]

    /**
     * 下载入队用的平台标识（线程数设置按平台独立，规格 §4.2）：
     * 服务端 pan 名 → YunX 本地 [DownloadPlatform] 常量；未识别的 pan 用 GENERIC。
     */
    fun downloadPlatformFor(pan: String): String = when (pan) {
        "quark" -> DownloadPlatform.QUARK
        "lanzou" -> DownloadPlatform.LANZOU
        "mangodrive" -> DownloadPlatform.MANGO
        else -> DownloadPlatform.GENERIC
    }

    /** 决策表：开关关 / 服务端 DOWN / 平台未覆盖（含会话内学习到的未覆盖）→ LOCAL，否则 REMOTE */
    fun decide(
        platform: SharePlatform,
        enabled: Boolean,
        serverUp: Boolean,
        covered: Boolean
    ): Route = if (enabled && serverUp && covered) Route.REMOTE else Route.LOCAL
}
