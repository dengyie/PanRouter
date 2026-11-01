package com.yunx.app.data.panrouter

/**
 * 应用级单例装配点（对齐 [com.yunx.app.data.network.HttpClients] 的 object 模式）。
 *
 * 唯一实例：解析页 ViewModel、设置页共用；设置/Token 每次调用实时读取（客户端无状态），
 * 改配置即时生效，无需重建。
 */
object PanRouterHub {

    /** 懒构造：首次用到（解析页装配/设置页测试连接）才建；探活宿主作用域随实例常驻 */
    val gateway: PanRouterResolveGateway by lazy {
        PanRouterResolveGateway(
            client = PanRouterClient(
                baseUrlProvider = { PanRouterSettings().baseUrl },
                tokenProvider = { PanRouterTokenStore.getToken() }
            )
        )
    }
}
