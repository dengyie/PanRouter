package com.yunx.app.data.panrouter

import com.yunx.app.data.network.SharePlatform
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicLong

/**
 * 网关编排测试：决策（配置/覆盖/健康）、会话内自学习（unsupported → LOCAL）、
 * 硬网络失败置 DOWN + 探测循环恢复（注入延迟，免真实等待）。
 */
class PanRouterResolveGatewayTest {

    private lateinit var server: MockWebServer
    private lateinit var health: ServerHealth

    /** 探测延迟调用计数（探测循环是否真的在跑/收敛） */
    private val probeCount = AtomicLong(0)

    /** baseUrlProvider 的切换开关：false = 连死端口（硬网络失败），true = 打 MockWebServer */
    private val alive = AtomicBoolean(true)

    /**
     * 测试隔离：PanRouterSettings 直连 AppContext.settingsPrefs（java.util.prefs → 注册表），
     * 与真实应用共享存储 —— 直接 `apply { enabled = true }` 会把用户配置写脏（GUI 验收时实际发生）。
     * 这里测试前快照、测试后还原三个涉及的键，保证零残留。
     */
    private val testSettings = PanRouterSettings()
    private var savedEnabled: Boolean = false
    private var savedBaseUrl: String = ""
    private var savedFallback: Boolean = true

    private fun gateway(probeDelay: suspend (Long) -> Unit = { delay(it) }): PanRouterResolveGateway =
        PanRouterResolveGateway(
            client = PanRouterClient(
                http = OkHttpClient(),
                baseUrlProvider = {
                    if (alive.get()) server.url("/").toString().trimEnd('/') else "http://127.0.0.1:1"
                },
                tokenProvider = { null }
            ),
            settings = testSettings,
            health = health,
            watchScope = CoroutineScope(Dispatchers.Default),
            probeDelay = {
                probeCount.incrementAndGet()
                probeDelay(it)
            }
        )

    @Before
    fun setUp() {
        savedEnabled = testSettings.enabled
        savedBaseUrl = testSettings.baseUrl
        savedFallback = testSettings.fallbackToLocal
        testSettings.enabled = true
        testSettings.baseUrl = "https://pan.example.invalid"
        server = MockWebServer()
        server.start()
        health = ServerHealth()
        alive.set(true)
    }

    @After
    fun tearDown() {
        testSettings.enabled = savedEnabled
        testSettings.baseUrl = savedBaseUrl
        testSettings.fallbackToLocal = savedFallback
        server.shutdown()
    }

    // ---------- 决策 ----------

    @Test
    fun `covered platform resolves remotely when configured and up`() {
        val g = gateway()
        assertTrue(g.shouldResolveRemotely(SharePlatform.QUARK))
        assertTrue(g.shouldResolveRemotely(SharePlatform.LANZOU))
        assertFalse(g.shouldResolveRemotely(SharePlatform.UC))
        assertFalse(g.shouldResolveRemotely(SharePlatform.GITHUB))
    }

    @Test
    fun `server down routes local`() {
        health.markHardFailure()
        val g = gateway()
        assertFalse(g.shouldResolveRemotely(SharePlatform.QUARK))
    }

    // ---------- 会话内自学习 ----------

    @Test
    fun `unsupported response marks platform local for the session`() = runBlocking {
        server.enqueue(
            MockResponse().setResponseCode(400)
                .setBody("""{"code":400,"kind":"unsupported","message":"暂不支持","retriable":false}""")
        )
        val g = gateway()
        assertTrue(g.shouldResolveRemotely(SharePlatform.QUARK))
        val e = g.resolveShare(SharePlatform.QUARK, "https://pan.quark.cn/s/x", null).exceptionOrNull()
        assertTrue(e is PanRouterError && e.kind == PanRouterError.Kind.UNSUPPORTED)
        // 学习后：同会话内该平台直接走本地（不再打服务端）
        assertFalse(g.shouldResolveRemotely(SharePlatform.QUARK))
    }

    // ---------- 硬网络失败 → DOWN → 探测循环恢复 ----------

    @Test
    fun `hard network failure marks down and probe loop recovers`() = runBlocking {
        // 死端口：硬网络失败 → DOWN
        alive.set(false)
        val g = gateway(probeDelay = { /* 测试免等待 */ })
        val e = g.resolveShare(SharePlatform.QUARK, "https://pan.quark.cn/s/x", null).exceptionOrNull()
        assertTrue("got $e", e is PanRouterError && e.kind == PanRouterError.Kind.NETWORK)
        assertFalse(g.isServerUp())
        assertFalse(g.shouldResolveRemotely(SharePlatform.QUARK))

        // 拉起探测循环，随后服务恢复：下一次探测（免等待）即回 UP
        alive.set(true)
        server.enqueue(MockResponse().setBody("""{"status":"ok","version":"t"}"""))
        g.startHealthWatch()
        withTimeout(10_000) {
            while (!g.isServerUp()) delay(10)
        }
        assertTrue("探测循环应执行过至少一次探测", probeCount.get() >= 1)
        assertTrue(g.shouldResolveRemotely(SharePlatform.QUARK))
    }
}
