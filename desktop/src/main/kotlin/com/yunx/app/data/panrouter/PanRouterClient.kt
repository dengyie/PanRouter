package com.yunx.app.data.panrouter

import com.yunx.app.data.network.HttpClients
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * PanRouter HTTP 网关（OkHttp）。
 *
 * - 健康探测走全局 [HttpClients.apiClient] 的超时配置；
 * - 解析请求走加长超时副本：服务端单次解析预算 90s（panrouter development.md §6.1），
 *   全局 api 客户端 45s callTimeout 会掐断还在跑的解析，这里放宽到 120s；
 * - 每次调用实时读 [baseUrlProvider]/[tokenProvider]（测试注入 MockWebServer 地址与固定 token，
 *   生产由设置页/TokenStore 提供），改配置即时生效，实例可长期复用；
 * - 所有调用经 `Dispatchers.IO`（OkHttp 同步执行），绝不阻塞调用方协程所在线程。
 *
 * 错误契约：非 2xx → [PanRouterError.fromHttp]；网络层失败 → [PanRouterError.fromNetwork]。
 * 本类不写日志（调用方单点记录），不持有请求态（无状态，规格 §7.5）。
 */
class PanRouterClient(
    http: OkHttpClient = HttpClients.apiClient(),
    private val baseUrlProvider: () -> String,
    private val tokenProvider: () -> String? = { PanRouterTokenStore.getToken() }
) {

    /** 解析专用客户端：同配置但放宽超时（服务端解析预算 90s） */
    private val resolveHttp: OkHttpClient = http.newBuilder()
        .callTimeout(120, TimeUnit.SECONDS)
        .readTimeout(110, TimeUnit.SECONDS)
        .build()

    private val baseHttp: OkHttpClient = http

    /**
     * 2xx body → JSONObject。body 非 JSON（网关故障页 / 契约漂移返回了 HTML 或空体）时
     * 映射为 [PanRouterError.Kind.INTERFACE_CHANGED]（P2 修复：原样抛 JSONException 会绕过
     * shouldFallbackToLocal 判定，用户看到原始异常而不会回落本地链）。
     */
    private fun jsonBody(body: String): JSONObject =
        try {
            JSONObject(body)
        } catch (e: org.json.JSONException) {
            throw PanRouterError(
                message = "响应结构非 JSON（${e.message?.take(120) ?: "body=${body.take(80)}"}）",
                kind = PanRouterError.Kind.INTERFACE_CHANGED,
                retriable = true,
                cause = e
            )
        }

    /** `GET /healthz` → {status, version} */
    suspend fun health(): Result<RemoteServerInfo> = withContext(Dispatchers.IO) {
        request {
            Request.Builder()
                .url("${baseUrlProvider()}/healthz")
                .get()
                .build()
        }.mapCatching { parseServerInfo(jsonBody(it)) }
    }

    /**
     * `POST /api/v1/resolve`（不带 fid）→ 分享文件列表（服务端已按预算自动提链，逐文件可能带 link_error）。
     * 游客可调用（optionalAuth）；带 token 时服务端可 Pick 账号池 Cookie 提链登录盘。
     */
    suspend fun resolveShare(url: String, pwd: String?): Result<RemoteResolveResult> =
        withContext(Dispatchers.IO) {
            resolveRequest(url, pwd, fid = null).mapCatching { parseResolveResult(jsonBody(it)) }
        }

    /**
     * `POST /api/v1/resolve`（带 fid）→ 单文件直链（含 stream_url / ua / referer / need_headers / route）。
     * 签名 TTL 内重复调用由服务端缓存命中（cache_hit=true），桌面端不自建直链缓存（规格 §8.3）。
     */
    suspend fun resolveLink(url: String, pwd: String?, fid: String): Result<RemoteLinkResult> =
        withContext(Dispatchers.IO) {
            resolveRequest(url, pwd, fid).mapCatching { parseLinkResult(jsonBody(it)) }
        }

    /**
     * `POST /api/v1/resolve/batch`（≤50 项，单项 90s 预算，部分成功语义）→ 逐项独立结果。
     * 调用方（解析页）负责分批：超出 50 项时切片串行；单项失败不中断整批。
     */
    suspend fun resolveBatch(items: List<Pair<String, String?>>): Result<List<RemoteBatchItem>> =
        withContext(Dispatchers.IO) {
            request {
                val arr = JSONArray()
                items.forEach { (url, pwd) ->
                    val o = JSONObject().put("url", url)
                    if (!pwd.isNullOrBlank()) o.put("pwd", pwd)
                    arr.put(o)
                }
                authedRequest("${baseUrlProvider()}/api/v1/resolve/batch", JSONObject().put("items", arr))
            }.mapCatching { body ->
                val results = jsonBody(body).optJSONArray("results")
                val out = ArrayList<RemoteBatchItem>(if (results == null) 0 else results.length())
                for (i in 0 until (results?.length() ?: 0)) {
                    out.add(parseBatchItem(results!!.optJSONObject(i) ?: JSONObject()))
                }
                out
            }
        }

    /** resolve 共用体：同一端点，带 fid 走单文件直链 */
    private fun resolveRequest(url: String, pwd: String?, fid: String?): Result<String> = request {
        val body = JSONObject().put("url", url)
        if (!pwd.isNullOrBlank()) body.put("pwd", pwd)
        if (fid != null) body.put("fid", fid)
        authedRequest("${baseUrlProvider()}/api/v1/resolve", body)
    }

    // ---------- 账号池（auth 端点，必须带 api_token/JWT） ----------

    /** `GET /api/v1/accounts?pan=` → 服务端账号池列表（pan 为空 = 全部） */
    suspend fun listAccounts(pan: String? = null): Result<List<RemoteAccountView>> =
        withContext(Dispatchers.IO) {
            request {
                val builder = Request.Builder()
                    .url("${baseUrlProvider()}/api/v1/accounts" + (pan?.takeIf { it.isNotBlank() }?.let { "?pan=$it" } ?: ""))
                    .get()
                tokenProvider()?.takeIf { it.isNotBlank() }?.let { builder.header("Authorization", "Bearer $it") }
                builder.build()
            }.mapCatching { body ->
                val arr = jsonBody(body).optJSONArray("accounts")
                val out = ArrayList<RemoteAccountView>(if (arr == null) 0 else arr.length())
                for (i in 0 until (arr?.length() ?: 0)) {
                    out.add(parseAccountView(arr!!.optJSONObject(i) ?: JSONObject()))
                }
                out
            }
        }

    /**
     * `POST /api/v1/accounts` {pan_type, name, cookie} → 贡献本地账号进服务端账号池。
     * 服务端会实时校验凭据（失败返回 auth_expired 等 kind）；成功返回建好的账号行。
     */
    suspend fun createAccount(panType: String, name: String, cookie: String): Result<RemoteAccountView> =
        withContext(Dispatchers.IO) {
            request {
                val body = JSONObject()
                    .put("pan_type", panType)
                    .put("name", name)
                    .put("cookie", cookie)
                authedRequest("${baseUrlProvider()}/api/v1/accounts", body)
            }.mapCatching { parseAccountView(jsonBody(it).optJSONObject("account") ?: JSONObject()) }
        }

    // ---------- 底层执行 ----------

    /** 构造 POST JSON 请求；已配置 token 时带 `Authorization: Bearer`（optionalAuth 端点游客可调） */
    private fun authedRequest(url: String, body: JSONObject): Request {
        val builder = Request.Builder()
            .url(url)
            .post(body.toString().toRequestBody("application/json; charset=utf-8".toMediaType()))
        tokenProvider()?.takeIf { it.isNotBlank() }?.let { builder.header("Authorization", "Bearer $it") }
        return builder.build()
    }

    /** 同步执行 + 统一错误映射：2xx 返回 body 文本；非 2xx → [PanRouterError.fromHttp]；IO 失败 → fromNetwork */
    private inline fun request(requestFactory: () -> Request): Result<String> =
        runCatching {
            newCall(requestFactory()).execute().use { resp -> readBody(resp) }
        }.recoverCatching { e ->
            throw if (e is PanRouterError) e else PanRouterError.fromNetwork(e)
        }

    private fun newCall(request: Request): okhttp3.Call =
        (if (request.method == "POST") resolveHttp else baseHttp).newCall(request)

    private fun readBody(response: Response): String {
        val body = response.body?.string().orEmpty()
        if (!response.isSuccessful) throw PanRouterError.fromHttp(response.code, body)
        return body
    }
}
