package com.yunx.app.data.panrouter

import java.util.concurrent.atomic.AtomicReference

/**
 * 服务端健康状态机（规格 §8.1）：
 * - 启动即假设 UP（不前置探测 —— UP 状态下解析不探活，PanRouter 失败自会报错）；
 * - 只有**硬网络失败**（连不上 / DNS / 连接被拒）才置 DOWN（规格：请求业务失败不计入，避免网络抖动误判）；
 * - DOWN 期间按退避序列探测：30s → 1m → 2m → 5m → 10m（封顶），探测成功立即回 UP；
 * - 状态迁移全部是纯函数（可注入时钟的调用方负责真正的等待），单测 100% 可重现（规格 §7.6）。
 */
class ServerHealth {

    data class State(val up: Boolean, /** 连续硬失败次数（决定下一次探测的退避档位） */
                     val failureStreak: Int)

    private val ref = AtomicReference(State(up = true, failureStreak = 0))

    /** 当前是否可达；解析前置判定 O(1) 读（规格 §7.2） */
    val up: Boolean get() = ref.get().up

    /** 下一次探测前应等待的毫秒数（DOWN 期间由探测循环调用）；UP 时返回 0（无需探测） */
    fun nextProbeDelayMs(): Long {
        val state = ref.get()
        if (state.up) return 0L
        val idx = (state.failureStreak - 1).coerceIn(0, BACKOFF_MS.size - 1)
        return BACKOFF_MS[idx]
    }

    /** 探测成功 / 请求成功：回 UP 并清零退避 */
    fun markSuccess() {
        ref.set(State(up = true, failureStreak = 0))
    }

    /** 硬网络失败：置 DOWN，失败计数 +1（连续失败推高退避档位） */
    fun markHardFailure() {
        ref.updateAndGet { State(up = false, failureStreak = it.failureStreak + 1) }
    }

    fun currentState(): State = ref.get()

    companion object {
        /** 退避序列（毫秒）：30s → 1m → 2m → 5m → 10m 封顶（规格 §8.1） */
        internal val BACKOFF_MS = longArrayOf(30_000L, 60_000L, 120_000L, 300_000L, 600_000L)
    }
}
