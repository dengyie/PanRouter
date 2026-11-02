package com.yunx.app.util

/**
 * 渲染后端回退的**纯决策逻辑**（不碰注册表 / JNA / 窗口，全部可单测）。
 *
 * 背景（2026-11-02 实测）：OPENGL(ANGLE) 的白屏是**间歇性、环境依赖**的 —— RTX 3070 上
 * MuMu/GameViewer 等虚拟显示适配器活动时可能首帧永不绘制（纯白），虚拟屏幕消失后又完全正常。
 * 静态显卡名单（[RenderApiChooser.LEGACY_GPU_MARKERS]）永远覆盖不了这类组合，
 * 只能在运行时检测「窗口出现了但首帧迟迟不上屏」，然后自动回退重启。
 *
 * 状态机（状态持久化在 miscPrefs，由调用方读写）：
 * - 正常：chooser（环境变量 > 老显卡名单 > OPENGL）
 * - 白屏 1 次：记 failures=1，**立刻重启再试一次 OPENGL**（很多白屏是当次会话的竞态，重启即愈，
 *   不舍得 GPU 渲染——本机 SOFTWARE 只有 ~10FPS）
 * - 白屏 2 次：记 override=SOFTWARE（带显卡签名），重启后长期走 SOFTWARE
 * - 任一会话 OPENGL 健康：failures 清零（偶发竞态不累积）
 * - 显卡签名变化（换卡 / 升级驱动）：override 与 failures 全部作废，重新按 chooser 走
 *   （驱动更新正是 ANGLE 行为变化的最常见诱因，升级后应再给 GPU 渲染机会）
 */
internal object RenderApiPolicy {

    const val OPENGL = "OPENGL"
    const val SOFTWARE = "SOFTWARE"

    /** 环境变量强制指定时看门狗不介入（用户明确要什么就给什么） */
    const val ENV_RENDER_API = "YUNXPC_RENDER_API"

    /** 看门狗自重启的防抖间隔：两次自重启至少隔这么久（防异常环境下的重启循环） */
    const val MIN_RELAUNCH_INTERVAL_MS = 120_000L

    /** 一次会话里 OPENGL 白屏累计到这个次数，才写入长期 override=SOFTWARE */
    const val FAILURES_FOR_OVERRIDE = 2

    /** 看门狗判定用的测试钩子：设置后跳过真实像素采样、直接按白屏处理（E2E 自测用） */
    const val ENV_SIMULATE_WHITE = "YUNXPC_WATCHDOG_SIMULATE_WHITE"

    /** 持久化状态快照（与 miscPrefs 键一一对应，见 [RenderApiChooser]） */
    data class State(
        val override: String? = null,
        val failures: Int = 0,
        val gpuSig: String = "",
        val lastRelaunchMs: Long = 0L
    )

    enum class Source { ENV, OVERRIDE, AUTO }

    data class Decision(
        val api: String,
        val source: Source,
        /** OPENGL 才挂看门狗；SOFTWARE 是兜底，绝不再自重启（防循环） */
        val watchdog: Boolean,
        /** 显卡签名与记录不符：调用方应把持久化状态整体清掉再写入本次结果 */
        val resetState: Boolean = false
    )

    /**
     * 启动时解析本次渲染后端。
     * [env] = YUNXPC_RENDER_API；[gpuSigChanged] = 已存签名非空且与当前不一致（含原本没存、现在读到了）。
     */
    fun resolve(
        env: String?,
        state: State,
        gpuSigChanged: Boolean,
        chooserApi: String
    ): Decision {
        env?.takeIf { it.isNotBlank() }?.let {
            // 原样透传（历史行为）：用户写什么 skiko 收什么；强制指定不看门狗
            return Decision(env.trim(), Source.ENV, watchdog = false)
        }
        if (state.override == SOFTWARE && !gpuSigChanged) {
            return Decision(SOFTWARE, Source.OVERRIDE, watchdog = false)
        }
        val api = chooserApi.uppercase()
        return Decision(
            api = api,
            source = Source.AUTO,
            watchdog = api == OPENGL,
            // 显卡签名变了：旧 override / failures 全部作废（调用方清持久化状态）
            resetState = gpuSigChanged
        )
    }

    /** OPENGL 会话健康（看门狗看到内容 / 正常跑完）：偶发失败清零 */
    fun onHealthy(state: State): State =
        if (state.failures == 0) state else state.copy(failures = 0, gpuSig = "")

    /**
     * OPENGL 白屏确认：返回 (新状态, 本次要切的后端)。
     * [canRelaunch] = 距上次自重启已超过防抖间隔（或从未重启过）。
     * 返回 next=null 表示不重启（防抖窗口内）：应用保持现状，等用户手动处置。
     *
     * ★ [lastRelaunchMs] 只在**真的要自重启**时更新 —— 防抖的语义是「距上次实际重启」，
     *   若记账也刷新时间戳，防抖窗口会被每次白屏无限顺延（永远走不到长期回退）。
     */
    fun onWhiteScreen(state: State, gpuSig: String, nowMs: Long, canRelaunch: Boolean): Pair<State, String?> {
        val failures = state.failures + 1
        val lastRelaunch = if (canRelaunch) nowMs else state.lastRelaunchMs
        val nextState: State
        val next: String?
        if (failures >= FAILURES_FOR_OVERRIDE) {
            nextState = State(override = SOFTWARE, failures = failures, gpuSig = gpuSig, lastRelaunchMs = lastRelaunch)
            next = if (canRelaunch) SOFTWARE else null
        } else {
            // 第一次失败：先原样重试一次 OPENGL（很多白屏重启即愈，不轻易放弃 GPU 渲染）
            nextState = state.copy(failures = failures, gpuSig = gpuSig, lastRelaunchMs = lastRelaunch)
            next = if (canRelaunch) OPENGL else null
        }
        return nextState to next
    }

    /** canRelaunch 判定：距上次自重启超过 [MIN_RELAUNCH_INTERVAL_MS]（从未重启过视为可以） */
    fun canRelaunch(state: State, nowMs: Long): Boolean =
        state.lastRelaunchMs <= 0L || nowMs - state.lastRelaunchMs >= MIN_RELAUNCH_INTERVAL_MS

    /**
     * 首帧探针判定：采样点量化后的颜色桶 ≤2 且主桶占比 ≥98% → 视为「没画出来」。
     * [samples] 为 ARGB/颜色引用值列表。真实 UI（深色主题、侧栏、文字、弹窗）桶数几十起步；
     * 白屏 = 单桶 100%，纯黑故障同理被兜住。
     */
    fun isUniformBlank(samples: List<Int>): Boolean {
        if (samples.isEmpty()) return false
        val buckets = HashMap<Int, Int>()
        for (c in samples) {
            // 量化到 /16 抗抖动；布局按 COLORREF（0x00BBGGRR）：低 3 字节 = B/G/R。
            // 换成 ARGB 也不影响判定（只是通道名换了，桶数与占比不变）。
            val r = (c and 0xFF) shr 4          // 0..15
            val g = ((c shr 8) and 0xFF) shr 4
            val b = ((c shr 16) and 0xFF) shr 4
            val key = (r shl 8) or (g shl 4) or b
            buckets[key] = (buckets[key] ?: 0) + 1
        }
        if (buckets.size > 2) return false
        val top = buckets.values.max()
        return top * 100 >= samples.size * 98
    }
}
