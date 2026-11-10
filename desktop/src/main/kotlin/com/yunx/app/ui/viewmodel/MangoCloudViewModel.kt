package com.yunx.app.ui.viewmodel

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.CreationExtras
import com.yunx.app.data.download.DownloadManager
import com.yunx.app.data.download.DownloadPlatform
import com.yunx.app.data.mangodrive.MangoDriveClient
import com.yunx.app.data.network.model.DownloadLink
import com.yunx.app.data.network.model.ShareFile
import com.yunx.app.data.panrouter.RemoteLinkResult
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.atomic.AtomicInteger
import kotlin.reflect.KClass

/** Mango Drive 浏览 UI 状态（根目录 dirPath = "/"）。 */
sealed interface MangoCloudUiState {
    data object Loading : MangoCloudUiState
    data class Loaded(
        val files: List<ShareFile>,
        /** 从根到当前目录的目录名（不含根） */
        val pathNames: List<String>,
        /** 当前目录的存储路径（根 = "/"） */
        val dirPath: String
    ) : MangoCloudUiState
    data class Error(val message: String) : MangoCloudUiState
}

/** 递归收集的待下载项：rel = 相对路径（落盘保持目录结构），fid + dir = 服务端提链定位 */
internal data class MangoCollectItem(
    val rel: String,
    val fid: String,
    val dir: String
)

/**
 * Mango Drive 私有聚合云盘浏览 ViewModel（镜像蓝奏云个人盘浏览）。
 *
 * 认证不靠 Cookie：服务端 mangodrive 驱动持有 OpenList token，桌面端只需 PanRouter 的
 * baseUrl + api_token（未配置时 resolve 会 401，界面给出明确提示）。
 * 目录按存储路径逐级下钻（path 栈），每级都向服务端 resolve 一次文件列表。
 */
class MangoCloudViewModel(
    private val client: MangoDriveClient,
    private val downloadManager: DownloadManager
) : ViewModel() {

    private val _uiState = MutableStateFlow<MangoCloudUiState>(MangoCloudUiState.Loading)
    val uiState: StateFlow<MangoCloudUiState> = _uiState.asStateFlow()

    var actionFile by mutableStateOf<ShareFile?>(null)
        private set
    var cloudMessage by mutableStateOf<String?>(null)
        private set
    var isOperating by mutableStateOf(false)
        private set
    var folderProgress by mutableStateOf<String?>(null)
        private set
    var refreshing by mutableStateOf(false)
        private set
    var downloadTriggered by mutableStateOf(0)
        private set
    var multiSelectMode by mutableStateOf(false)
        private set

    private val _selected = mutableStateListOf<ShareFile>()
    val selected: List<ShareFile> get() = _selected

    var downloadLink by mutableStateOf<DownloadLink?>(null)
        private set

    /** 当前待确认下载的保存目录（空串 = 跟随全局设置）；下载弹窗展示用 */
    var pendingSaveDir by mutableStateOf("")
        private set

    private var pendingDownload: PendingDownload? = null
    private var downloadCancelRequested = false

    private val dirStack = ArrayDeque<String>()
    private val nameStack = ArrayDeque<String>()

    init {
        loadRoot()
    }

    // ---------- 目录浏览 ----------

    fun loadRoot() {
        dirStack.clear()
        nameStack.clear()
        load("/", emptyList())
    }

    fun openFolder(file: ShareFile) {
        if (!file.isdir) return
        val child = childPath(currentPath(), file.fname)
        dirStack.addLast(child)
        nameStack.addLast(file.fname)
        load(child, nameStack.toList())
    }

    fun back() {
        if (nameStack.isEmpty()) {
            loadRoot()
            return
        }
        dirStack.removeLast()
        nameStack.removeLast()
        load(dirStack.lastOrNull() ?: "/", nameStack.toList())
    }

    fun navigateToLevel(level: Int) {
        while (nameStack.size > level) {
            dirStack.removeLast()
            nameStack.removeLast()
        }
        load(dirStack.lastOrNull() ?: "/", nameStack.toList())
    }

    /** 下拉刷新 */
    fun refresh() {
        val current = uiState.value
        if (current !is MangoCloudUiState.Loaded) {
            loadRoot()
            return
        }
        refreshing = true
        viewModelScope.launch {
            try {
                _uiState.value = MangoCloudUiState.Loaded(
                    files = listFiles(current.dirPath),
                    pathNames = current.pathNames,
                    dirPath = current.dirPath
                )
            } catch (e: Exception) {
                cloudMessage = e.message ?: "刷新失败"
            } finally {
                refreshing = false
            }
        }
    }

    private fun currentPath(): String =
        (uiState.value as? MangoCloudUiState.Loaded)?.dirPath ?: "/"

    private fun load(path: String, pathNames: List<String>) {
        _uiState.value = MangoCloudUiState.Loading
        viewModelScope.launch {
            _uiState.value = try {
                MangoCloudUiState.Loaded(listFiles(path), pathNames, path)
            } catch (e: Exception) {
                MangoCloudUiState.Error(e.message ?: "加载失败")
            }
        }
    }

    /** 服务端列目录 → 本地 ShareFile（目录在前，同类按名称排序；服务端已按名称排序，这里只兜底） */
    private suspend fun listFiles(path: String): List<ShareFile> {
        val result = client.listDir(path).getOrThrow()
        return result.files.map { item ->
            ShareFile(
                fid = item.fid,
                fname = item.name,
                fsize = item.size.coerceAtLeast(-1L),
                isdir = item.isDir,
                // 私有盘没有分享目录层级：父目录用 path 自身记录，fidToken 留空
                pdirFid = path,
                fidToken = ""
            )
        }.sortedWith(compareByDescending<ShareFile> { it.isdir }.thenBy { it.fname })
    }

    // ---------- 多选 ----------

    fun enterMultiSelect(file: ShareFile) {
        multiSelectMode = true
        _selected.clear()
        _selected.add(file)
    }

    fun toggleSelect(file: ShareFile) {
        if (_selected.contains(file)) _selected.remove(file) else _selected.add(file)
    }

    fun toggleSelectAll(files: List<ShareFile>) {
        if (_selected.size == files.size) _selected.clear()
        else {
            _selected.clear()
            _selected.addAll(files)
        }
    }

    fun exitMultiSelect() {
        multiSelectMode = false
        _selected.clear()
    }

    fun openActions(file: ShareFile) {
        actionFile = file
    }

    fun dismissActions() {
        actionFile = null
    }

    fun consumeMessage() {
        cloudMessage = null
    }

    fun consumeDownloadTriggered() {
        downloadTriggered = 0
    }

    fun cancelDownload() {
        downloadCancelRequested = true
    }

    // ---------- 下载 ----------

    /** 下载文件：向服务端取直链 → 弹下载确认弹窗（确认后入队） */
    fun downloadFile() = beginDownload("")

    /** 下载到指定目录：单次覆盖保存目录，不改全局设置 */
    fun downloadFileTo(saveDir: String) = beginDownload(saveDir)

    private fun beginDownload(saveDir: String) {
        val file = actionFile ?: return
        viewModelScope.launch {
            isOperating = true
            try {
                val link = client.link(currentPath(), file.fid).getOrThrow()
                val url = downloadUrlOf(link)
                val headers = downloadHeadersOf(link)
                pendingSaveDir = saveDir
                pendingDownload = PendingDownload(
                    url = url,
                    fileName = file.fname.ifBlank { link.fileName },
                    size = if (link.size > 0) link.size else file.fsize,
                    saveDir = saveDir,
                    headers = headers
                )
                downloadLink = DownloadLink(
                    fid = link.fid,
                    filename = file.fname.ifBlank { link.fileName },
                    downloadUrl = url,
                    size = if (link.size > 0) link.size else file.fsize
                )
            } catch (e: Exception) {
                cloudMessage = e.message ?: "获取直链失败"
            } finally {
                isOperating = false
            }
        }
    }

    /** 下载弹窗确认：用已生成的直链入队（平台 MANGO，线程数跟「按平台设置」） */
    fun startDownload() {
        val pd = pendingDownload ?: return
        downloadLink = null
        pendingDownload = null
        pendingSaveDir = ""
        viewModelScope.launch {
            isOperating = true
            try {
                downloadManager.enqueue(
                    url = pd.url,
                    fileName = pd.fileName,
                    size = pd.size,
                    platform = DownloadPlatform.MANGO,
                    headers = pd.headers,
                    saveDir = pd.saveDir
                )
                cloudMessage = "已加入下载：${pd.fileName}"
                actionFile = null
                downloadTriggered++
            } catch (e: Exception) {
                cloudMessage = e.message ?: "下载失败"
            } finally {
                isOperating = false
            }
        }
    }

    fun dismissDownloadDialog() {
        downloadLink = null
        pendingDownload = null
        pendingSaveDir = ""
    }

    /**
     * 递归列出文件夹内全部文件：三元组 (相对路径, fid, 所在目录)。
     * 目录必须显式携带：fid 是长路径时服务端会哈希化（"p:<sha1>"，不可逆），
     * 提链时 parentDirOf 推不出父目录——列目录时就知道的 dirPath 是唯一可靠定位。
     * 返回值 = 因超出层级上限（8 层）未下探的子目录数（静默丢文件 → 明示给用户）。
     */
    private suspend fun collectFiles(
        dirPath: String,
        prefix: String,
        out: MutableList<MangoCollectItem>,
        depth: Int
    ): Int {
        if (depth > 8) return 1
        var skipped = 0
        for (item in listFiles(dirPath)) {
            val rel = if (prefix.isEmpty()) item.fname else "$prefix/${item.fname}"
            if (item.isdir) {
                skipped += collectFiles(childPath(dirPath, item.fname), rel, out, depth + 1)
            } else {
                out.add(MangoCollectItem(rel, item.fid, dirPath))
            }
        }
        return skipped
    }

    /**
     * 入队一批收集项：小并发（3）提链 + 入队。
     * 提链是服务端 QPS 限速下的逐文件 HTTP 解析（限速 10，3 并发安全）——串行时
     * 大文件夹要等 N×单次延迟才入完队；并发后总耗时 ≈ N/3。任务落库顺序允许交错
     * （下载列表按文件夹分组展示，组内顺序无业务语义）。
     */
    private suspend fun enqueueEach(
        tasks: List<MangoCollectItem>,
        saveDir: String
    ): Pair<Int, List<String>> {
        val ok = AtomicInteger(0)
        val processed = AtomicInteger(0)
        val failed = ConcurrentLinkedQueue<String>()
        val gate = Semaphore(ENQUEUE_CONCURRENCY)
        coroutineScope {
            for (task in tasks) {
                if (downloadCancelRequested) break
                launch {
                    gate.withPermit {
                        if (downloadCancelRequested) return@withPermit
                        folderProgress = "正在加入下载 ${processed.incrementAndGet()}/${tasks.size}"
                        runCatching {
                            // 哈希 fid（"p:" 前缀）推不出父目录，交给服务端按历史缓存反查；
                            // 路径形态 fid 直接用所在目录，零回查。
                            val dir = if (client.isHashFid(task.fid)) "/" else task.dir
                            val link = client.link(dir, task.fid).getOrThrow()
                            downloadManager.enqueue(
                                url = downloadUrlOf(link),
                                fileName = task.rel,
                                size = if (link.size > 0) link.size else -1L,
                                platform = DownloadPlatform.MANGO,
                                headers = downloadHeadersOf(link),
                                saveDir = saveDir
                            )
                            ok.incrementAndGet()
                        }.onFailure { if (!downloadCancelRequested) failed.add(task.rel) }
                    }
                }
            }
        }
        return ok.get() to failed.toList()
    }

    /** 汇总入队结果消息：成功数 + 截断提示 + 失败明细（最多展示 3 个）。 */
    private fun enqueueResultMessage(ok: Int, failed: List<String>, skipped: Int): String {
        val sb = StringBuilder("已加入 $ok 个下载任务")
        if (skipped > 0) sb.append("；$skipped 个子目录超 8 层上限未收集")
        if (failed.isNotEmpty()) {
            sb.append("；失败 ${failed.size} 个：")
            sb.append(failed.take(3).joinToString("、"))
            if (failed.size > 3) sb.append(" 等")
        }
        return sb.toString()
    }

    fun downloadFolder(saveDir: String = "") {
        val folder = actionFile ?: return
        if (!folder.isdir) return
        viewModelScope.launch {
            isOperating = true
            folderProgress = "正在收集文件…"
            downloadCancelRequested = false
            try {
                val tasks = mutableListOf<MangoCollectItem>()
                val skipped = collectFiles(childPath(currentPath(), folder.fname), folder.fname, tasks, 0)
                if (tasks.isEmpty()) {
                    cloudMessage = if (skipped > 0) "文件夹内容超出 8 层上限，未收集到文件" else "文件夹为空"
                    actionFile = null
                    return@launch
                }
                val (ok, failed) = enqueueEach(tasks, saveDir)
                if (downloadCancelRequested) {
                    cloudMessage = "已中断下载"
                } else {
                    cloudMessage = enqueueResultMessage(ok, failed, skipped)
                }
            } catch (e: Exception) {
                cloudMessage = e.message ?: "下载文件夹失败"
            } finally {
                isOperating = false
                folderProgress = null
                downloadCancelRequested = false
                actionFile = null
            }
        }
    }

    fun downloadSelected(saveDir: String = "") {
        val files = _selected.toList()
        if (files.isEmpty()) return
        val base = currentPath()
        viewModelScope.launch {
            isOperating = true
            folderProgress = "正在收集文件…"
            downloadCancelRequested = false
            try {
                val tasks = mutableListOf<MangoCollectItem>()
                var skipped = 0
                for (file in files) {
                    if (file.isdir) {
                        skipped += collectFiles(childPath(base, file.fname), file.fname, tasks, 0)
                    } else {
                        tasks.add(MangoCollectItem(file.fname, file.fid, base))
                    }
                }
                if (tasks.isEmpty()) {
                    cloudMessage = if (skipped > 0) "所选内容超出 8 层上限，未收集到文件" else "所选文件夹为空"
                    exitMultiSelect()
                    return@launch
                }
                val (ok, failed) = enqueueEach(tasks, saveDir)
                if (downloadCancelRequested) {
                    cloudMessage = "已中断批量下载"
                } else {
                    cloudMessage = enqueueResultMessage(ok, failed, skipped)
                }
                exitMultiSelect()
            } catch (e: Exception) {
                cloudMessage = e.message ?: "批量下载失败"
            } finally {
                isOperating = false
                folderProgress = null
                downloadCancelRequested = false
            }
        }
    }

    class Factory(
        private val client: MangoDriveClient,
        private val downloadManager: DownloadManager
    ) : ViewModelProvider.Factory {
        @Suppress("UNCHECKED_CAST")
        override fun <T : ViewModel> create(modelClass: KClass<T>, extras: CreationExtras): T =
            MangoCloudViewModel(client, downloadManager) as T
    }

    companion object {
        /** 递归收集入队的并发度：服务端解析限速 10 QPS，3 并发远低限速且提链延迟 ≈ 1/3 */
        private const val ENQUEUE_CONCURRENCY = 3

        /** 子目录路径：父路径 + "/" + 名（根 "/" 不产生 "//"） */
        internal fun childPath(parent: String, name: String): String {
            val p = parent.trim().trimEnd('/')
            val n = name.trim()
            if (n.isEmpty()) return if (p.isEmpty()) "/" else p
            return if (p.isEmpty()) "/$n" else "$p/$n"
        }

        /**
         * 直链取值：优先 direct_url（OpenList 签名直链，桌面端直连，不经 pxed 中转；
         * 服务端实测支持 Range，可多线程分块）。
         * 没有 direct_url 时按路由回退：302 → download_url；stream → stream_url。
         */
        internal fun downloadUrlOf(link: RemoteLinkResult): String {
            link.directUrl?.takeIf { it.isNotBlank() }?.let { return it }
            return link.effectiveUrl()
        }

        /**
         * 直链请求头：服务端 need_headers 时按 ua/referer 注入。
         * direct_url 会被 Cloudflare 边缘按 UA 校验（不带 UA 直接 000/拒连），所以必须带。
         */
        internal fun downloadHeadersOf(link: RemoteLinkResult): Map<String, String> {
            if (!link.needHeaders) return emptyMap()
            val out = LinkedHashMap<String, String>(2)
            if (link.ua.isNotBlank()) out["User-Agent"] = link.ua
            if (link.referer.isNotBlank()) out["Referer"] = link.referer
            return out
        }
    }
}
