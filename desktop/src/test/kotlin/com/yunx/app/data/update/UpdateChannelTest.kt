package com.yunx.app.data.update

import com.yunx.app.data.announcement.AnnouncementApi
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 更新通道守卫：本 fork 的更新检查必须指向自己的发布通道（dengyie/PanRouter）。
 * 指向上游 tidain/YunX-Desktop（1.3.x 版本线）会导致 0.1.x 永远被判定「有新版本」，
 * 且用户点更新会装上没有 PanRouter 整合的上游版 —— 曾真实发生过，加测试防回归。
 *
 * 同一守卫覆盖**公告数据源**：v0.1.3 前公告一直请求安卓原作者的个人服务器
 * （yunx.cyqawa.os.kg，明文 HTTP），已自有化为本仓静态 JSON，同样不许回落。
 */
class UpdateChannelTest {

    @Test
    fun releaseEndpointsPointToForkRepo() {
        assertTrue(UpdateChecker.RELEASES_LATEST_URL.startsWith("https://api.github.com/repos/dengyie/PanRouter/"))
        assertTrue(UpdateChecker.RELEASES_LIST_URL.startsWith("https://api.github.com/repos/dengyie/PanRouter/"))
    }

    @Test
    fun releaseEndpointsNeverPointToUpstream() {
        assertFalse(UpdateChecker.RELEASES_LATEST_URL.contains("tidain"))
        assertFalse(UpdateChecker.RELEASES_LIST_URL.contains("tidain"))
    }

    @Test
    fun announcementSourcePointsToForkRepo() {
        // 公告数据源 = 本仓静态 JSON（raw 地址可带镜像前缀访问）
        assertTrue(AnnouncementApi.ANNOUNCEMENTS_RAW_URL.contains("dengyie/PanRouter"))
        assertTrue(AnnouncementApi.ANNOUNCEMENTS_RAW_URL.startsWith("https://raw.githubusercontent.com/"))
    }

    @Test
    fun announcementSourceNeverPointsToUpstreamAuthorServers() {
        // 原作者私货域名：上游更新仓（tidain）与安卓作者的公告虚拟主机（cyqawa / os.kg）
        val url = AnnouncementApi.ANNOUNCEMENTS_RAW_URL
        assertFalse(url.contains("tidain"))
        assertFalse(url.contains("cyqawa"))
        assertFalse(url.contains("os.kg"))
    }

    @Test
    fun forkVersionComparesCorrectlyAgainstForkReleases() {
        // 0.1.3 应大于当前线上 v0.1.2、小于将来的 v0.2.0（弹更新）
        assertTrue(UpdateChecker.compareVersions("v0.1.3", "v0.1.2") > 0)
        assertTrue(UpdateChecker.compareVersions("v0.1.3", "v0.2.0") < 0)
        assertEquals(0, UpdateChecker.compareVersions("v0.1.3", "0.1.3"))
    }
}
