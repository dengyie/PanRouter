package com.yunx.app.data.panrouter

import com.yunx.app.data.network.SharePlatform
import com.yunx.app.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.io.IOException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean

/**
 * PanRouter 解析用例编排（规格 §3.2/§4.1）：
 * - 决策：[ResolveRouter] 表驱动 + 服务端「说不支持」的会话内自学习（uncovered 集，O(1) 读写）；
 * - 探活：仅硬网络失败置 DOWN；DOWN 后在本 [watchScope] 里按退避循环探测，成功即回 UP 并停循环；
 * - 健康标记单点收口：所有服务端调用都经 [tracked]，成功/硬失败/覆盖学习的状态迁移只写这一处（规格 §7.5）。
 *
 * 不在这里做「REMOTE 失败回落 LOCAL」——那是 ViewModel 的用例职责（它持有本地链）。
 */
class PanRouterResolveGateway(
    private val client: PanRouterClient,
    private val settings: PanRouterSettings = PanRouterSettings(),
    private val health: ServerHealth = ServerHealth(),
    /** 探测循环宿主作用域；测试注入隔离作用域避免泄漏 */
    private val watchScope: CoroutineScope = CoroutineScope(SupervisorJob() + Dispatchers.IO),
    /** 探测等待的可注入延迟（测试免真实等待）；生产即 kotlinx delay */
    private val probeDelay: suspend (Long) -> Unit = { delay(it) }
) {

    /** 会话内学习到的「服务端不支持」平台（resolve 返回 kind=unsupported 时登记，进程内有效） */
    private val uncovered: MutableSet<SharePlatform> = ConcurrentHashMap.newKeySet()

    /** 探测循环在跑标记（原子占位：DOWN 期间并发 startHealthWatch 只放行一个） */
    private val probeRunning = AtomicBoolean(false)

    /** 服务端 pan 标识；未覆盖返回 null */
    fun serverPanFor(platform: SharePlatform): String? = ResolveRouter.serverPanFor(platform)

    /** 健康快照（设置页/调试用） */
    fun isServerUp(): Boolean = health.up

    /** 本次解析是否走服务端（O(1)：配置 + 健康 + 静态覆盖 + 会话内学习） */
    fun shouldResolveRemotely(platform: SharePlatform): Boolean {
        if (!settings.isConfigured()) return false
        if (uncovered.contains(platform)) return false
        if (ResolveRouter.serverPanFor(platform) == null) return false
        return ResolveRouter.decide(platform, enabled = true, serverUp = health.up, covered = true) ==
            ResolveRouter.Route.REMOTE
    }

    /** 解析分享（不带 fid）：成功/硬失败/覆盖学习统一在 [tracked] 收口 */
    suspend fun resolveShare(platform: SharePlatform, url: String, pwd: String?): Result<RemoteResolveResult> =
        tracked(platform) { client.resolveShare(url, pwd) }

    /** 单文件直链（带 fid）：签名 TTL 内服务端缓存命中，桌面端不自建缓存（规格 §8.3） */
    suspend fun resolveLink(url: String, pwd: String?, fid: String): Result<RemoteLinkResult> =
        tracked { client.resolveLink(url, pwd, fid) }

    /**
     * 多链接批量解析（规格 §5.1 resolve/batch）：部分成功语义，逐项 [RemoteBatchItem] 原样返回。
     * 批量整体失败（HTTP 错误）仍走 [tracked] 健康标记；单项失败是字符串原因，无 kind，不学习覆盖集。
     */
    suspend fun resolveBatch(items: List<Pair<String, String?>>): Result<List<RemoteBatchItem>> =
        tracked { client.resolveBatch(items) }

    /** 设置页「测试连接」：主动探活一次，返回服务端信息；不改变健康状态机的自动判定 */
    suspend fun probeNow(): Result<RemoteServerInfo> = client.health()

    // ---------- 账号池（M2 账号贡献，管理面操作：不参与解析健康判定） ----------

    /** 服务端账号池列表（pan 为空 = 全部） */
    suspend fun listServerAccounts(pan: String? = null): Result<List<RemoteAccountView>> =
        client.listAccounts(pan)

    /**
     * 贡献本地账号进服务端账号池：服务端实时校验凭据后加密落库（规格 §5.3）。
     * 调用方（设置页）负责二次确认与凭据不落日志；此处不做健康标记（管理面 ≠ 解析面）。
     */
    suspend fun contributeAccount(panType: String, name: String, cookie: String): Result<RemoteAccountView> =
        client.createAccount(panType, name, cookie)

    // ---------- 内部 ----------

    /** 服务端调用统一出口：成功 → markSuccess；网络硬失败 → markHardFailure + 拉起探测循环 */
    private suspend fun <T> tracked(call: suspend () -> Result<T>): Result<T> {
        val result = call()
        result.fold(
            onSuccess = { health.markSuccess() },
            onFailure = { e -> if (isHardNetworkFailure(e)) { health.markHardFailure(); startHealthWatch() } }
        )
        return result
    }

    private suspend fun <T> tracked(
        platform: SharePlatform,
        call: suspend () -> Result<T>
    ): Result<T> = tracked(call).onFailure { e ->
        // 「服务端说不支持」：会话内把该平台标记为未覆盖，后续解析直接走本地（规格 §3.2）
        if (e is PanRouterError && e.kind == PanRouterError.Kind.UNSUPPORTED) uncovered.add(platform)
    }

    /** 硬网络失败判定：NETWORK kind 且根因链上有连接级 IOException（规格 §8.1，业务失败不计 DOWN） */
    private fun isHardNetworkFailure(e: Throwable): Boolean {
        if (e !is PanRouterError || e.kind != PanRouterError.Kind.NETWORK) return false
        var cause: Throwable? = e.cause
        while (cause != null) {
            if (cause is IOException) return true
            cause = cause.cause
        }
        return false
    }

    /**
     * DOWN 期间拉起探测循环；[probeRunning] 原子占位保证「DOWN ⇒ 恰好一个循环」（P3 修复：
     * 原实现 check-then-act 非原子，两个硬失败并发时会拉起双循环、探测频率翻倍）。
     * 退出时在 finally 里补判：循环判定退出的瞬间又被置 DOWN 的窄窗口，重新占位拉起，不留无循环的 DOWN。
     */
    internal fun startHealthWatch() {
        if (!probeRunning.compareAndSet(false, true)) return
        watchScope.launch {
            try {
                while (!health.up) {
                    runCatching { probeDelay(health.nextProbeDelayMs()) }
                    val ok = client.health().isSuccess
                    if (ok) {
                        health.markSuccess()
                        Log.i(TAG, "PanRouter 已恢复可达")
                    } else {
                        health.markHardFailure()
                        Log.w(TAG, "PanRouter 仍不可达（退避 ${health.nextProbeDelayMs() / 1000}s 后重试）")
                    }
                }
            } finally {
                probeRunning.set(false)
                if (!health.up && watchScope.isActive) startHealthWatch()
            }
        }
    }

    companion object {
        private const val TAG = "YunX-PanRouter"
    }
}
