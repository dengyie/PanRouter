package com.yunx.app.data.network

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ShareLinkParserTest {

    @Test
    fun parsesAllSupportedPlatforms() {
        val cases = listOf(
            "https://pan.quark.cn/s/Abc123?pwd=a1B2" to (SharePlatform.QUARK to "Abc123"),
            "https://drive.uc.cn/s/Abc123" to (SharePlatform.UC to "Abc123"),
            "https://pan.xunlei.com/s/Abc_123-xy" to (SharePlatform.XUNLEI to "Abc_123-xy"),
            "https://pan.baidu.com/s/1Abc_123-xy?pwd=9xYz" to (SharePlatform.BAIDU to "Abc_123-xy"),
            "https://yun.139.com/shareweb/#/w/i/Abc_123" to (SharePlatform.C139 to "Abc_123"),
            "https://www.123pan.com/s/2785Vv-T4Ded" to (SharePlatform.PAN123 to "2785Vv-T4Ded")
        )

        cases.forEach { (text, expected) ->
            val parsed = ShareLinkParser.parse(text)!!
            assertEquals(expected.first, parsed.platform)
            assertEquals(expected.second, parsed.shareId)
        }
    }

    @Test
    fun explicitTextPasswordIsExtracted() {
        val parsed = ShareLinkParser.parse("链接 https://drive.uc.cn/s/Abc123 提取码：a1B2")!!
        assertEquals("a1B2", parsed.pwd)
    }

    @Test
    fun rejectsUnrelatedUrl() {
        assertNull(ShareLinkParser.parse("https://example.com/s/Abc123"))
    }

    @org.junit.Test
    fun parseAllExtractsMultipleLinksInOrderAndDedupes() {
        val text = """
            第一个 https://pan.quark.cn/s/Abc123?pwd=a1B2 看看
            第二个 https://www.lanzouw.com/def567 提取码：9x8y7
            重复贴一遍 https://pan.quark.cn/s/Abc123?pwd=a1B2
            无关内容 https://www.example.com/notashare 不算
        """.trimIndent()
        val links = ShareLinkParser.parseAll(text)
        org.junit.Assert.assertEquals(2, links.size)
        org.junit.Assert.assertEquals("https://pan.quark.cn/s/Abc123?pwd=a1B2", links[0].url)
        org.junit.Assert.assertEquals(SharePlatform.QUARK, links[0].share.platform)
        org.junit.Assert.assertEquals("Abc123", links[0].share.shareId)
        org.junit.Assert.assertEquals("a1B2", links[0].share.pwd)
        org.junit.Assert.assertEquals(SharePlatform.LANZOU, links[1].share.platform)
        org.junit.Assert.assertEquals("9x8y7", links[1].share.pwd)
    }

    @org.junit.Test
    fun parseAllSingleLinkAndNoLink() {
        org.junit.Assert.assertEquals(
            1,
            ShareLinkParser.parseAll("只发一个 https://pan.quark.cn/s/Xyz789").size
        )
        org.junit.Assert.assertTrue(ShareLinkParser.parseAll("这里没有任何分享链接").isEmpty())
    }
}
