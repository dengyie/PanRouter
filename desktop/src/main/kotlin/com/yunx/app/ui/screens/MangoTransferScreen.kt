package com.yunx.app.ui.screens

import com.yunx.app.data.panrouter.TransferChannel
import com.yunx.app.data.panrouter.TransferTask
import com.yunx.app.ui.SnackbarController
import com.yunx.app.ui.theme.animateItemIfAllowed
import com.yunx.app.ui.viewmodel.MangoTransferViewModel
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBarScrollBehavior
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay

/**
 * 自建云盘「转存记录」页（M4.3）。
 *
 * 云对云转存由服务端执行（本机可关），这里只展示任务快照：状态、字节进度、通道、目标路径。
 * 有进行中任务时自动轻刷（只重拉快照，不重建 VM）。
 */
@OptIn(androidx.compose.material3.ExperimentalMaterial3Api::class)
@Composable
fun MangoTransferScreen(
    viewModel: MangoTransferViewModel,
    scrollBehavior: TopAppBarScrollBehavior,
    onBack: () -> Unit,
    modifier: Modifier = Modifier
) {
    val tasks by viewModel.tasks.collectAsState()
    val listState = rememberLazyListState()

    LaunchedEffect(viewModel.message) {
        viewModel.message?.let {
            SnackbarController.show(it)
            viewModel.consumeMessage()
        }
    }

    // 进行中任务在服务端推进：轻量轮询快照（VM 已在 initiating 时 kick，这里只做兜底）
    LaunchedEffect(Unit) {
        viewModel.refresh()
        while (true) {
            delay(3_000)
            if (tasks.any { it.isRunning }) viewModel.refresh()
        }
    }

    LazyColumn(
        state = listState,
        modifier = modifier
            .fillMaxSize()
            .nestedScroll(scrollBehavior.nestedScrollConnection),
        contentPadding = PaddingValues(start = 16.dp, end = 16.dp, top = 16.dp, bottom = 24.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp)
    ) {
        item {
            Row(verticalAlignment = Alignment.CenterVertically) {
                IconButton(onClick = onBack) {
                    Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回")
                }
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = "转存记录",
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.Medium
                    )
                    Text(
                        text = "云端接力搬运 · 关掉客户端也会继续",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
                IconButton(onClick = { viewModel.refresh() }) {
                    Icon(Icons.Outlined.Refresh, contentDescription = "刷新")
                }
            }
        }

        when {
            viewModel.loading && tasks.isEmpty() -> item {
                Box(modifier = Modifier.fillMaxWidth().padding(vertical = 48.dp), contentAlignment = Alignment.Center) {
                    CircularProgressIndicator(modifier = Modifier.size(28.dp))
                }
            }

            tasks.isEmpty() -> item {
                Column(modifier = Modifier.fillMaxWidth().padding(top = 48.dp, bottom = 24.dp)) {
                    Text(
                        text = "还没有转存任务",
                        style = MaterialTheme.typography.bodyLarge,
                        textAlign = TextAlign.Center,
                        modifier = Modifier.fillMaxWidth()
                    )
                    Text(
                        text = "在分享页把链接转存到自建云盘后，这里会出现任务进度",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        textAlign = TextAlign.Center,
                        modifier = Modifier.fillMaxWidth().padding(top = 8.dp)
                    )
                }
            }

            else -> items(tasks, key = { it.id }) { task ->
                TransferTaskCard(task, modifier = animateItemIfAllowed())
            }
        }
    }
}

@Composable
private fun TransferTaskCard(task: TransferTask, modifier: Modifier = Modifier) {
    Surface(
        modifier = modifier.fillMaxWidth(),
        shape = MaterialTheme.shapes.medium,
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f)
    ) {
        Column(modifier = Modifier.fillMaxWidth().padding(14.dp)) {
            Text(
                text = task.fileName.ifBlank { task.destPath.ifBlank { task.id } },
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis
            )
            Text(
                text = "${TransferTask.stateLabelOf(task.state)} · ${TransferChannel.labelOf(task.channel)}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 4.dp)
            )
            Text(
                text = if (task.size > 0) "${formatMangoSize(task.done)} / ${formatMangoSize(task.size)}"
                else "已搬运 ${formatMangoSize(task.done)}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 6.dp)
            )
            val progress = task.progress
            if (progress != null) {
                LinearProgressIndicator(
                    progress = { progress },
                    modifier = Modifier.fillMaxWidth().padding(top = 6.dp)
                )
            } else if (!task.isTerminal) {
                LinearProgressIndicator(modifier = Modifier.fillMaxWidth().padding(top = 6.dp))
            }
            if (task.error.isNotBlank()) {
                Text(
                    text = task.error,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(top = 6.dp)
                )
            }
            if (task.destPath.isNotBlank()) {
                Text(
                    text = task.destPath,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.padding(top = 6.dp)
                )
            }
        }
    }
}
