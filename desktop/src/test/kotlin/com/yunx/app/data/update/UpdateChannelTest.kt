package com.yunx.app.data.update

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 更新通道守卫：本 fork 的更新检查必须指向自己的发布通道（dengyie/PanRouter）。
 * 指向上游 tidain/YunX-Desktop（1.3.x 版本线）会导致 0.1.x 永远被判定「有新版本」，
 * 且用户点更新会装上没有 PanRouter 整合的上游版 —— 曾真实发生过，加测试防回归。
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
    fun forkVersionComparesCorrectlyAgainstForkReleases() {
        // 0.1.2 应大于当前线上 v0.1.1、小于将来的 v0.2.0（弹更新）
        assertTrue(UpdateChecker.compareVersions("v0.1.2", "v0.1.1") > 0)
        assertTrue(UpdateChecker.compareVersions("v0.1.2", "v0.2.0") < 0)
        assertEquals(0, UpdateChecker.compareVersions("v0.1.2", "0.1.2"))
    }
}
