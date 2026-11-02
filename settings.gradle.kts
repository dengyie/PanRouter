pluginManagement {
    repositories {
        // 国内镜像优先（本机无法直连 dl.google.com）
        maven("https://maven.aliyun.com/repository/gradle-plugin")
        maven("https://maven.aliyun.com/repository/google")
        maven("https://maven.aliyun.com/repository/central")
        gradlePluginPortal()
        mavenCentral()
        google()
    }
}

dependencyResolutionManagement {
    repositories {
        maven("https://maven.aliyun.com/repository/google")
        maven("https://maven.aliyun.com/repository/central")
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
