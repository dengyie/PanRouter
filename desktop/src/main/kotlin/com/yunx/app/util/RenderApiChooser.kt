package com.yunx.app.util

import com.sun.jna.Native
import com.sun.jna.Pointer
import com.sun.jna.Structure
import com.sun.jna.platform.win32.Advapi32Util
import com.sun.jna.platform.win32.WinReg
import com.sun.jna.win32.StdCallLibrary
import com.sun.jna.win32.W32APIOptions
import java.io.File
import kotlin.concurrent.thread

/**
 * 启动时选择 Skiko 渲染后端（仅 Windows 用）。
 *
 * 背景：默认的 OPENGL（ANGLE）在**老旧 / 基础显示适配器**上会出现
 * 「界面有形状、但一个文字都不渲染」的问题（典型：ATI Radeon 3000 这类 DX10 时代的集显）。
 * 这类设备自动退到 SOFTWARE（CPU 光栅化）：帧率低一些，但文字与图形一定正确。
 *
 * ★ 2026-11 起叠加**首帧看门狗**（[startFirstFrameWatchdog]，策略见 [RenderApiPolicy]）：
 *   白屏还有一类**间歇性、环境依赖**的形态（实测 RTX 3070 + MuMu/GameViewer 虚拟显示适配器：
 *   虚拟屏幕活动时首帧永不绘制，消失后一切正常），静态名单覆盖不了 —— 只能在窗口出现后
 *   采样客户区像素，确认「迟迟没画任何东西」就写回退状态、自动重启切 SOFTWARE。
 *
 * ★ 仍可用环境变量 `YUNXPC_RENDER_API=OPENGL / DIRECT3D / SOFTWARE` 强制指定，优先级最高；
 *   环境变量强制时看门狗不介入（尊重用户显式选择）。
 */
internal object RenderApiChooser {

    /**
     * 已知在 ANGLE/OpenGL 下容易丢失文字的老旧 / 基础显卡关键字（小写匹配）。
     * 只列「确定有问题」的类型，避免误伤能正常跑 GPU 渲染的机器。
     */
    private val LEGACY_GPU_MARKERS = listOf(
        // AMD/ATI 老集显与入门卡
        "radeon 3000", "radeon 3100", "radeon 3200", "radeon 3300", "radeon 3400",
        "radeon hd 2", "radeon hd 3", "radeon hd 4", "radeon hd 5",
        "radeon x1", "radeon x2", "radeon x3", "radeon x6",
        // Intel 老集显
        "intel(r) gma", "intel gma", "mobile intel",
        // 没有真实 GPU 驱动时的兜底适配器
        "microsoft basic", "standard vga",
        // 常见虚拟机显卡
        "vmware svga", "virtualbox", "qemu", "cirrus"
    )

    /** 返回写入 `skiko.renderApi` 的值；拿不到显卡信息时维持 OPENGL（保持现状） */
    fun chooseWindows(): String {
        val gpu = gpuAdapters().firstOrNull { info ->
            val n = info.name.lowercase()
            LEGACY_GPU_MARKERS.any { n.contains(it) }
        }
        if (gpu != null) {
            Log.w("YunX-Render", "检测到老旧/基础显卡「${gpu.name}」，改用 SOFTWARE 渲染（避免文字不显示）")
            return "SOFTWARE"
        }
        return "OPENGL"
    }

    private data class AdapterInfo(val name: String, val driverVersion: String)

    /**
     * 从注册表取当前显示适配器（名称 + 驱动版本）：
     * `HKLM\HARDWARE\DEVICEMAP\VIDEO` 的 `\Device\VideoN` 指向 `Control\Video\{GUID}\0000`，
     * 该键的 `DriverDesc` / `DriverVersion` 即驱动的显示名与版本。毫秒级、不起子进程。
     *
     * ★ 刻意只读 DEVICEMAP 指向的**当前**适配器键：直接枚举 `Control\Video\*` 会把历史遗留的
     *   老显卡条目也读出来，导致换过显卡的机器被误判成老设备。
     */
    private fun gpuAdapters(): List<AdapterInfo> = runCatching {
        val adapters = mutableListOf<AdapterInfo>()
        for (index in 0 until 8) {
            val devicePath = runCatching {
                Advapi32Util.registryGetStringValue(
                    WinReg.HKEY_LOCAL_MACHINE,
                    "HARDWARE\\DEVICEMAP\\VIDEO",
                    "\\Device\\Video$index"
                )
            }.getOrNull() ?: break
            val relative = devicePath.substringAfter("\\Registry\\Machine\\", "").trim()
            if (relative.isEmpty()) continue
            val desc = runCatching {
                Advapi32Util.registryGetStringValue(WinReg.HKEY_LOCAL_MACHINE, relative, "DriverDesc")
            }.getOrNull()
            if (!desc.isNullOrBlank()) {
                val ver = runCatching {
                    Advapi32Util.registryGetStringValue(WinReg.HKEY_LOCAL_MACHINE, relative, "DriverVersion")
                }.getOrNull().orEmpty()
                adapters += AdapterInfo(desc, ver)
            }
        }
        adapters
    }.getOrDefault(emptyList())

    /**
     * 显卡签名（名字+驱动版本合串）：白屏回退状态跟着它走 —— 签名变化（换卡/升级驱动）即作废
     * 旧回退，重新给 OPENGL 机会。读不到返回空串（此时签名比较视为「没变」）。
     */
    fun gpuSignature(): String =
        gpuAdapters().joinToString("|") { "${it.name}#${it.driverVersion}" }

    // ---------- miscPrefs 键（java.util.prefs 同步读，启动早期无 IO 阻塞顾虑） ----------

    private const val KEY_OVERRIDE = "render_api_override"
    private const val KEY_FAILURES = "render_api_failures"
    private const val KEY_GPU_SIG = "render_api_gpu_sig"
    private const val KEY_LAST_RELAUNCH = "render_api_last_relaunch"

    private fun prefs() = com.yunx.app.AppContext.miscPrefs

    private fun loadState(): RenderApiPolicy.State = RenderApiPolicy.State(
        override = prefs().get(KEY_OVERRIDE, null)?.takeIf { it.isNotBlank() },
        failures = prefs().getInt(KEY_FAILURES, 0),
        gpuSig = prefs().get(KEY_GPU_SIG, ""),
        lastRelaunchMs = prefs().getLong(KEY_LAST_RELAUNCH, 0L)
    )

    private fun saveState(s: RenderApiPolicy.State) {
        if (s.override == null) prefs().remove(KEY_OVERRIDE) else prefs().put(KEY_OVERRIDE, s.override)
        if (s.failures == 0) prefs().remove(KEY_FAILURES) else prefs().putInt(KEY_FAILURES, s.failures)
        prefs().put(KEY_GPU_SIG, s.gpuSig)
        prefs().putLong(KEY_LAST_RELAUNCH, s.lastRelaunchMs)
        runCatching { prefs().sync() }
    }

    /**
     * 启动时解析本次渲染后端（含白屏回退状态与显卡签名比对）。
     * 返回决策；调用方负责 `System.setProperty("skiko.renderApi", decision.api)`。
     */
    fun resolveWindows(): RenderApiPolicy.Decision {
        val state = loadState()
        val currentSig = gpuSignature()
        // 签名比对：记录过签名（说明记录过白屏）且与当前不同 → 旧状态作废
        val sigChanged = state.gpuSig.isNotEmpty() && currentSig.isNotEmpty() && state.gpuSig != currentSig
        val decision = RenderApiPolicy.resolve(
            env = System.getenv(RenderApiPolicy.ENV_RENDER_API),
            state = state,
            gpuSigChanged = sigChanged,
            chooserApi = chooseWindows()
        )
        if (decision.resetState) {
            Log.i("YunX-Render", "显卡/驱动签名变化，白屏回退状态作废，重新按默认策略选择")
            saveState(RenderApiPolicy.State())
        }
        val sourceLabel = when (decision.source) {
            RenderApiPolicy.Source.ENV -> "环境变量"
            RenderApiPolicy.Source.OVERRIDE -> "白屏回退记录（failures=${state.failures}）"
            RenderApiPolicy.Source.AUTO -> "自动"
        }
        Log.i("YunX-Render", "渲染后端：${decision.api}（来源：$sourceLabel）")
        return decision
    }

    /** 看门狗确认 OPENGL 会话健康：清零失败计数 */
    fun markOpenGlHealthy() {
        saveState(RenderApiPolicy.onHealthy(loadState()))
    }

    // ---------- 首帧看门狗 ----------

    private interface User32Probe : StdCallLibrary {
        fun GetDC(hwnd: Pointer?): Pointer?
        fun ReleaseDC(hwnd: Pointer?, hdc: Pointer?): Int
        fun GetClientRect(hwnd: Pointer?, rect: RectByRef?): Boolean
        fun ClientToScreen(hwnd: Pointer?, point: PointByRef?): Boolean
        fun GetPixel(hdc: Pointer?, x: Int, y: Int): Int

        class RectByRef : Structure() {
            @JvmField var left = 0
            @JvmField var top = 0
            @JvmField var right = 0
            @JvmField var bottom = 0
            override fun getFieldOrder() = listOf("left", "top", "right", "bottom")
        }

        class PointByRef : Structure() {
            @JvmField var x = 0
            @JvmField var y = 0
            override fun getFieldOrder() = listOf("x", "y")
        }
    }

    private val user32 by lazy {
        Native.load("user32", User32Probe::class.java, W32APIOptions.UNICODE_OPTIONS)
    }

    /**
     * 客户区像素采样（GetClientRect + ClientToScreen 换成屏幕设备像素，DPI 天然正确）。
     * 返回 COLORREF 颜色引用列表；窗口无效 / 过小 / 采样失败返回 null。
     */
    private fun sampleClientArea(hwnd: Pointer, grid: Int = 24): List<Int>? {
        val rect = User32Probe.RectByRef()
        if (!user32.GetClientRect(hwnd, rect)) return null
        val w = rect.right - rect.left
        val h = rect.bottom - rect.top
        if (w < 50 || h < 50) return null
        val origin = User32Probe.PointByRef()
        if (!user32.ClientToScreen(hwnd, origin)) return null
        val hdc = user32.GetDC(hwnd) ?: return null
        try {
            // 内缩 5% 避开边框阴影；按网格均布采样
            val mx = (w * 0.05).toInt()
            val my = (h * 0.05).toInt()
            val stepX = ((w - 2 * mx) / grid).coerceAtLeast(1)
            val stepY = ((h - 2 * my) / (grid / 2).coerceAtLeast(1)).coerceAtLeast(1)
            val samples = ArrayList<Int>()
            var y = my
            while (y < h - my) {
                var x = mx
                while (x < w - mx) {
                    samples += user32.GetPixel(hdc, origin.x + x, origin.y + y) and 0xFFFFFF
                    x += stepX
                }
                y += stepY
            }
            return if (samples.size >= 30) samples else null
        } finally {
            user32.ReleaseDC(hwnd, hdc)
        }
    }

    private const val SIM_ENV_LABEL = "YUNXPC_WATCHDOG_SIMULATE_WHITE"

    /**
     * 首帧看门狗：OPENGL 模式下，窗口出现后 15s / 25s 两次采样客户区像素，
     * 两次都是「整片均匀纯色」（≥98% 同桶）就判定白屏 → 写回退状态 → 自重启。
     *
     * 前台守卫：采样要求本窗口此刻在前台（否则读到的是盖在上面的别的窗口，会误判）。
     * 用户在干别的（如全屏游戏）时顺延检查、最多 3 次，仍轮不到就放弃本次 —— 绝不在用户
     * 使用中途杀进程重启；下次启动再查。
     *
     * 测试钩子：环境变量 [RenderApiPolicy.ENV_SIMULATE_WHITE]=1 时跳过采样直接按白屏处理
     * （E2E 自测回退链路用，日志带【模拟】标记）。
     *
     * [decision] 必须来自 [resolveWindows] 且 watchdog=true（本次以 OPENGL 启动、非环境变量强制）。
     */
    fun startFirstFrameWatchdog(frame: java.awt.Frame, decision: RenderApiPolicy.Decision) {
        if (!decision.watchdog) return
        val simulate = System.getenv(RenderApiPolicy.ENV_SIMULATE_WHITE) == "1"
        val hwnd: Pointer = runCatching { Native.getComponentPointer(frame) }.getOrNull() ?: return
        thread(name = "yunx-render-watchdog", isDaemon = true) {
            val t0 = System.nanoTime()
            fun sleepUntil(ms: Long) {
                val target = t0 + ms * 1_000_000
                while (true) {
                    val rest = (target - System.nanoTime()) / 1_000_000
                    if (rest <= 0) return
                    Thread.sleep(minOf(rest, 1_000))
                }
            }

            // 两次检查（15s / 25s），都均匀纯色才算白屏；任一次「有内容」即健康并清零失败计数
            var firstBlank = false
            var postpone = 0
            val checks = longArrayOf(15_000L, 25_000L)
            for ((idx, at) in checks.withIndex()) {
                sleepUntil(at)
                if (!simulate && !isForeground(hwnd)) {
                    if (postpone >= 3) {
                        Log.i("YunX-Render", "看门狗：窗口不在前台（用户在做别的事），本次不再检查")
                        return@thread
                    }
                    postpone++
                    Log.i("YunX-Render", "看门狗：窗口不在前台，顺延 20s 再查（第 $postpone 次）")
                    sleepUntil(at + postpone * 20_000L)
                    if (!isForeground(hwnd)) {
                        Log.i("YunX-Render", "看门狗：顺延后仍不在前台，本次不再检查")
                        return@thread
                    }
                }
                val blank = if (simulate) {
                    Log.w("YunX-Render", "看门狗【模拟】：跳过采样直接按白屏处理（$SIM_ENV_LABEL=1）")
                    true
                } else {
                    val samples = sampleClientArea(hwnd)
                    if (samples == null) {
                        Log.i("YunX-Render", "看门狗：采样失败（窗口过小/不可见），本次不再检查")
                        return@thread
                    }
                    RenderApiPolicy.isUniformBlank(samples)
                }
                if (!blank) {
                    if (idx > 0 || firstBlank) {
                        Log.i("YunX-Render", "看门狗：首帧正常（OPENGL 渲染中），看门狗解除")
                        markOpenGlHealthy()
                    }
                    return@thread
                }
                if (idx == 0) {
                    firstBlank = true
                    Log.w("YunX-Render", "看门狗：15s 客户区仍是整片纯色，25s 复查确认…")
                }
            }

            // ---- 两次确认白屏 → 先记账（failures 累计推进状态机），再按防抖决定是否自重启 ----
            // ★ 记账与重启是两件事：防抖只拦「重启」——若拦下记账，第二次白屏永远写不进
            //   长期 override（E2E 实测踩过：第 2 次白屏落在 120s 防抖窗口内，被整段跳过）
            val now = System.currentTimeMillis()
            val state = loadState()
            val canRestart = RenderApiPolicy.canRelaunch(state, now)
            val result = RenderApiPolicy.onWhiteScreen(state, gpuSignature(), now, canRelaunch = canRestart)
            val newState = result.first
            val nextApi: String? = result.second
            saveState(newState)
            if (nextApi == null) {
                Log.e(
                    "YunX-Render",
                    "看门狗：确认白屏（累计 ${newState.failures} 次），但距上次自重启不足 " +
                        "${RenderApiPolicy.MIN_RELAUNCH_INTERVAL_MS / 1000}s，为防循环本次不重启。" +
                        "可设 ${RenderApiPolicy.ENV_RENDER_API}=SOFTWARE 手动兜底，或重启应用以应用回退"
                )
                return@thread
            }
            val tailNote: String = if (newState.override != null) {
                "（已记长期回退，显卡签名=${newState.gpuSig.take(60)}）"
            } else {
                "（下次仍先试 OPENGL，第二次白屏才长期回退）"
            }
            Log.e(
                "YunX-Render",
                "看门狗：确认 OPENGL 白屏（累计 ${newState.failures} 次）→ 自动重启切 $nextApi$tailNote"
            )
            if (!relaunchSelf()) {
                Log.e("YunX-Render", "自重启失败（拿不到可复制的启动命令）——请手动重启应用；仍白屏可设 ${RenderApiPolicy.ENV_RENDER_API}=SOFTWARE")
                return@thread
            }
            // 给新进程 500ms 起步时间再退出；shutdown hook 会收尾（WAL checkpoint / JCEF 释放）
            Thread.sleep(500)
            Log.i("YunX-Render", "看门狗：本进程退出，交给新进程（$nextApi）")
            kotlin.system.exitProcess(0)
        }
    }

    /** 本窗口是否为前台窗口（看门狗采样前提） */
    private fun isForeground(hwnd: Pointer): Boolean = runCatching {
        com.sun.jna.platform.win32.User32.INSTANCE.GetForegroundWindow()?.pointer == hwnd
    }.getOrDefault(false)

    /**
     * 自重启：便携版 = 重新拉起 YunX-Desktop.exe（jpackage.app-path）；
     * 开发运行 = 用 `sun.java.command`（HotSpot："<主类> [程序参数]"）+ `java.class.path`
     * + 当前 JDK 的 java.exe 复刻同一条命令。新进程输出接到数据目录 files/relaunch.log，
     * 便于排查回退后的启动问题。
     */
    private fun relaunchSelf(): Boolean = runCatching {
        val viaJpackage = System.getProperty("jpackage.app-path")?.takeIf { File(it).isFile }
        val command: List<String> = if (viaJpackage != null) {
            listOf(viaJpackage)
        } else {
            val sunCommand = System.getProperty("sun.java.command")?.trim().orEmpty()
            val mainAndArgs = splitCommandLine(sunCommand)
            val classpath = System.getProperty("java.class.path")?.trim().orEmpty()
            val javaExe = File(System.getProperty("java.home"), "bin/java.exe").takeIf { it.isFile }
                ?: File(System.getProperty("java.home"), "bin/java").takeIf { it.isFile }
            if (mainAndArgs.isEmpty() || classpath.isEmpty() || javaExe == null) return false
            // classpath 不在 sun.java.command 里，从 java.class.path 还原
            listOf(javaExe.absolutePath, "-Djava.class.path=$classpath") + mainAndArgs
        }
        val logFile = File(com.yunx.app.AppContext.filesDir, "relaunch.log")
        val pb = ProcessBuilder(command)
        pb.redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
        pb.redirectErrorStream(true)
        pb.start()
        Log.i("YunX-Render", "已拉起新进程：${command.first()}（日志：${logFile.absolutePath}）")
        true
    }.onFailure {
        Log.e("YunX-Render", "自重启异常：${it.javaClass.simpleName}: ${it.message}")
    }.getOrDefault(false)

    /**
     * 极简命令行切分：按空格切、双引号包裹的段整体保留。
     * 只用于 sun.java.command（我们自己进程的命令行，无转义嵌套），够用即可。
     */
    private fun splitCommandLine(line: String): List<String> {
        val out = ArrayList<String>()
        val sb = StringBuilder()
        var inQuotes = false
        for (ch in line) {
            when {
                ch == '"' -> inQuotes = !inQuotes
                ch == ' ' && !inQuotes -> {
                    if (sb.isNotEmpty()) { out.add(sb.toString()); sb.setLength(0) }
                }
                else -> sb.append(ch)
            }
        }
        if (sb.isNotEmpty()) out.add(sb.toString())
        return out
    }
}
