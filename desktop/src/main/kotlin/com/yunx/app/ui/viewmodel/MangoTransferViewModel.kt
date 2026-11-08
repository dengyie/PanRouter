package com.yunx.app.ui.viewmodel

import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.setValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.CreationExtras
import com.yunx.app.data.panrouter.PanRouterClient
import com.yunx.app.data.panrouter.TransferRequest
import com.yunx.app.data.panrouter.TransferTask
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlin.reflect.KClass

/**
 * 私有盘云对云转存（M4.3）：提交转存 + 轮询服务端任务进度。
 *
 * 任务在服务端内存异步推进；本机只做「提交 → 轮询快照」。有进行中任务时
 * 每 3s 轮询一次列表，全部终结后自动停下载入。
 */
class MangoTransferViewModel(
    private val client: PanRouterClient
) : ViewModel() {

    private val _tasks = MutableStateFlow<List<TransferTask>>(emptyList())
    val tasks: StateFlow<List<TransferTask>> = _tasks.asStateFlow()

    var loading by mutableStateOf(true)
        private set
    var operating by mutableStateOf(false)
        private set
    var message by mutableStateOf<String?>(null)
        private set
    var submitting by mutableStateOf(false)
        private set

    private var pollJob: Job? = null

    // 不在 init 里拉取：MainScreen 会提前创建本 VM，应用启动时不该打网络；
    // 由转存记录页首次进入时主动 refresh()。

    fun refresh() {
        viewModelScope.launch {
            loading = true
            try {
                _tasks.value = client.listTransfers().getOrThrow()
            } catch (e: Exception) {
                message = e.message ?: "加载转存记录失败"
            } finally {
                loading = false
                ensurePolling()
            }
        }
    }

    /** 提交一次转存（服务端 202 受理即返回） */
    fun submit(req: TransferRequest) {
        if (req.sourceUrl.isBlank()) return
        viewModelScope.launch {
            submitting = true
            try {
                val task = client.submitTransfer(req).getOrThrow()
                message = "已受理：${task.fileName.ifBlank { task.id }}（${TransferTask.stateLabelOf(task.state)}）"
                refresh()
            } catch (e: Exception) {
                message = e.message ?: "提交转存失败"
            } finally {
                submitting = false
            }
        }
    }

    fun consumeMessage() {
        message = null
    }

    private fun ensurePolling() {
        pollJob?.cancel()
        if (_tasks.value.none { it.isRunning }) return
        pollJob = viewModelScope.launch {
            while (true) {
                delay(3_000)
                val renewed = client.listTransfers().getOrNull() ?: continue
                _tasks.value = renewed
                if (renewed.none { it.isRunning }) {
                    pollJob?.cancel()
                    break
                }
            }
        }
    }

    class Factory(private val client: PanRouterClient) : ViewModelProvider.Factory {
        @Suppress("UNCHECKED_CAST")
        override fun <T : ViewModel> create(modelClass: KClass<T>, extras: CreationExtras): T =
            MangoTransferViewModel(client) as T
    }
}
