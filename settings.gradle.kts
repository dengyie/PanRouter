// 阿里云镜像开关（默认关）。
// ★ 镜像只为国内本机加速；GitHub CI runner 直连 mavenCentral/google 更稳更快。
//   Gradle 对仓库 5xx（如 aliyun 偶发 502，2026-11-12 v0.1.12 发版实证）不会
//   落到下一个仓库而是直接中止解析 → CI 不设开关就完全不碰镜像，镜像故障
//   不再能打挂发版。本机开启方式（二选一）：
//     - 全局 %USERPROFILE%\.gradle\gradle.properties 加 yunx.useAliyunMirror=true
//     - 环境变量 YUNX_USE_ALIYUN_MIRROR=1
// 注意：pluginManagement 是独立编译作用域，访问不到脚本顶层声明，
// 判定必须在两个块内各自内联（勿上提为顶层 val）。

pluginManagement {
    val useAliyunMirror = providers.environmentVariable("YUNX_USE_ALIYUN_MIRROR").isPresent ||
            providers.gradleProperty("yunx.useAliyunMirror").isPresent
    repositories {
        if (useAliyunMirror) {
            maven("https://maven.aliyun.com/repository/gradle-plugin")
            maven("https://maven.aliyun.com/repository/google")
            maven("https://maven.aliyun.com/repository/central")
        }
        gradlePluginPortal()
        mavenCentral()
        google()
    }
}

dependencyResolutionManagement {
    val useAliyunMirror = providers.environmentVariable("YUNX_USE_ALIYUN_MIRROR").isPresent ||
            providers.gradleProperty("yunx.useAliyunMirror").isPresent
    repositories {
        if (useAliyunMirror) {
            maven("https://maven.aliyun.com/repository/google")
            maven("https://maven.aliyun.com/repository/central")
        }
        mavenCentral()
        google()
    }
}

// 工具链自动下载仓库（foojay）：`updateDaemonJvm` / daemon JVM criteria（gradle-daemon-jvm.properties）
// 生成与解析时要用 —— 本机与 CI 都已装 JDK 17（走本地探测），这里只是兜底下载通道。
plugins {
    id("org.gradle.toolchains.foojay-resolver-convention") version "0.8.0"
}

rootProject.name = "YunX-Desktop"
include(":desktop")
