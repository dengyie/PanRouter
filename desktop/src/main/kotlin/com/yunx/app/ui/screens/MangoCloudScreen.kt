package com.yunx.app.ui.screens

import com.yunx.app.ui.BackHandler
import com.yunx.app.ui.SnackbarController
import com.yunx.app.ui.theme.Motion
import com.yunx.app.ui.theme.animateItemIfAllowed
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInVertically
import androidx.compose.animation.slideOutVertically
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.outlined.CheckCircle
import androidx.compose.material.icons.outlined.Download
import androidx.compose.material.icons.outlined.FolderOpen
import androidx.compose.material.icons.outlined.History
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBarScrollBehavior
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.graphics.vector.ImageVector
import com.yunx.app.data.network.model.ShareFile
import com.yunx.app.ui.components.ScrollToTopButton
import com.yunx.app.ui.components.rememberDownloadDirPicker
import com.yunx.app.ui.items.MultiSelectAction
import com.yunx.app.ui.items.MultiSelectBar
import com.yunx.app.ui.resolve.BackToParentItem
import com.yunx.app.ui.resolve.CrumbBar
import com.yunx.app.ui.resolve.DownloadLinkDialog
import com.yunx.app.ui.resolve.ShareFileRow
import com.yunx.app.ui.viewmodel.MangoCloudUiState
import com.yunx.app.ui.viewmodel.MangoCloudViewModel

/**
 * 自建云盘（OpenList 兼容端点）浏览页（镜像蓝奏云个人盘页）：
 * - 目录逐级浏览 + 下拉刷新 + 面包屑回退
 * - 长按多选（批量下载 / 下载文件夹 / 下载到…）
 * - 只读：加密池的上传/删除/重命名走 OpenList 网页端，避免误操作毁数据
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MangoCloudScreen(
    viewModel: MangoCloudViewModel,
    scrollBehavior: TopAppBarScrollBehavior,
    onExit: () -> Unit,
    onDownloadStarted: () -> Unit = {},
    onOpenTransfers: (() -> Unit)? = null,
    modifier: Modifier = Modifier
) {
    val state by viewModel.uiState.collectAsState()
    BackHandler {
        val s = state
        if (s is MangoCloudUiState.Loaded && s.pathNames.isNotEmpty()) viewModel.back() else onExit()
    }
    val listState = rememberLazyListState()
    var showActionSheet by remember { mutableStateOf(false) }
    val pickDownloadDir = rememberDownloadDirPicker()

    LaunchedEffect(viewModel.cloudMessage) {
        viewModel.cloudMessage?.let {
            SnackbarController.show(it)
            viewModel.consumeMessage()
        }
    }

    LaunchedEffect(viewModel.downloadTriggered) {
        if (viewModel.downloadTriggered > 0) {
            viewModel.consumeDownloadTriggered()
            onDownloadStarted()
        }
    }

    viewModel.downloadLink?.let { link ->
        DownloadLinkDialog(
            link = link,
            onDownload = { viewModel.startDownload() },
            onDismiss = { viewModel.dismissDownloadDialog() },
            saveDir = viewModel.pendingSaveDir.takeIf { it.isNotBlank() }
        )
    }

    Surface(
        modifier = modifier.fillMaxSize(),
        color = MaterialTheme.colorScheme.surface
    ) {
        AnimatedContent(
            targetState = state,
            transitionSpec = { Motion.fade(200, 140) },
            label = "mangoCloudState"
        ) { s ->
            when (s) {
                is MangoCloudUiState.Loading -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center
                ) { CircularProgressIndicator() }

                is MangoCloudUiState.Error -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center
                ) {
                    Column(horizontalAlignment = Alignment.CenterHorizontally) {
                        Text("自建云盘")
                        Spacer(modifier = Modifier.height(8.dp))
                        Text(
                            text = s.message,
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            textAlign = TextAlign.Center,
                            modifier = Modifier.padding(horizontal = 32.dp)
                        )
                        Spacer(modifier = Modifier.height(16.dp))
                        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            OutlinedButton(onClick = onExit) { Text("返回") }
                            TextButton(onClick = { viewModel.loadRoot() }) { Text("重试") }
                        }
                    }
                }

                is MangoCloudUiState.Loaded -> Box(modifier = Modifier.fillMaxSize()) {
                    LazyColumn(
                        state = listState,
                        modifier = Modifier
                            .fillMaxSize()
                            .nestedScroll(scrollBehavior.nestedScrollConnection),
                        contentPadding = PaddingValues(
                            start = 16.dp, end = 16.dp, top = 16.dp,
                            bottom = if (viewModel.multiSelectMode) 96.dp else 16.dp
                        ),
                        verticalArrangement = Arrangement.spacedBy(8.dp)
                    ) {
                        item {
                            MangoHeader(
                                state = s,
                                multiSelectMode = viewModel.multiSelectMode,
                                selectedCount = viewModel.selected.size,
                                onBack = onExit,
                                onCancelSelect = { viewModel.exitMultiSelect() },
                                onToggleAll = { viewModel.toggleSelectAll(s.files) },
                                onRefresh = { viewModel.refresh() },
                                onNavigate = { viewModel.navigateToLevel(it) },
                                onOpenTransfers = onOpenTransfers
                            )
                        }

                        if (s.pathNames.isNotEmpty()) {
                            item { BackToParentItem(onClick = { viewModel.back() }) }
                        }

                        if (s.files.isEmpty()) {
                            item {
                                Text(
                                    text = "此目录为空",
                                    style = MaterialTheme.typography.bodyMedium,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier
                                        .fillMaxWidth()
                                        .padding(vertical = 32.dp),
                                    textAlign = TextAlign.Center
                                )
                            }
                        }

                        items(s.files, key = { it.fid }) { file ->
                            ShareFileRow(
                                file = file,
                                modifier = animateItemIfAllowed(),
                                onClick = {
                                    if (viewModel.multiSelectMode) {
                                        viewModel.toggleSelect(file)
                                    } else if (file.isdir) {
                                        viewModel.openFolder(file)
                                    } else {
                                        viewModel.openActions(file)
                                        showActionSheet = true
                                    }
                                },
                                onMore = if (!viewModel.multiSelectMode) {
                                    {
                                        viewModel.openActions(file)
                                        showActionSheet = true
                                    }
                                } else {
                                    null
                                },
                                onContextMenu = if (!viewModel.multiSelectMode) {
                                    {
                                        viewModel.openActions(file)
                                        showActionSheet = true
                                    }
                                } else {
                                    null
                                },
                                selected = viewModel.selected.contains(file),
                                showCheckbox = viewModel.multiSelectMode
                            )
                        }
                    }

                    ScrollToTopButton(
                        listState = listState,
                        modifier = Modifier
                            .align(Alignment.BottomEnd)
                            .padding(
                                end = 16.dp,
                                bottom = if (viewModel.multiSelectMode) 104.dp else 16.dp
                            )
                    )

                    AnimatedVisibility(
                        visible = viewModel.multiSelectMode,
                        enter = slideInVertically(tween(220)) { it } + fadeIn(tween(220)),
                        exit = slideOutVertically(tween(180)) { it } + fadeOut(tween(180)),
                        modifier = Modifier.align(Alignment.BottomCenter)
                    ) {
                        MultiSelectBar(
                            busy = viewModel.isOperating,
                            count = viewModel.selected.size,
                            actions = listOf(
                                MultiSelectAction("下载", Icons.Outlined.Download, MaterialTheme.colorScheme.primary) {
                                    viewModel.downloadSelected()
                                },
                                MultiSelectAction("下载到…", Icons.Outlined.FolderOpen, MaterialTheme.colorScheme.primary) {
                                    pickDownloadDir { viewModel.downloadSelected(it) }
                                }
                            )
                        )
                    }
                }
            }
        }
    }

    if (showActionSheet && viewModel.actionFile != null) {
        MangoActionSheet(
            file = viewModel.actionFile!!,
            onDownload = {
                showActionSheet = false
                viewModel.downloadFile()
            },
            onDownloadTo = {
                pickDownloadDir { dir ->
                    showActionSheet = false
                    viewModel.downloadFileTo(dir)
                }
            },
            onDownloadFolder = {
                showActionSheet = false
                viewModel.downloadFolder()
            },
            onDownloadFolderTo = {
                pickDownloadDir { dir ->
                    showActionSheet = false
                    viewModel.downloadFolder(dir)
                }
            },
            onMultiSelect = {
                showActionSheet = false
                viewModel.actionFile?.let { viewModel.enterMultiSelect(it) }
            },
            onDismiss = {
                showActionSheet = false
                viewModel.dismissActions()
            }
        )
    }
}

@Composable
private fun MangoHeader(
    state: MangoCloudUiState.Loaded,
    multiSelectMode: Boolean,
    selectedCount: Int,
    onBack: () -> Unit,
    onCancelSelect: () -> Unit,
    onToggleAll: () -> Unit,
    onRefresh: () -> Unit,
    onNavigate: (Int) -> Unit,
    onOpenTransfers: (() -> Unit)? = null
) {
    Column {
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (multiSelectMode) {
                IconButton(onClick = onCancelSelect) {
                    Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "取消选择")
                }
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = "已选 $selectedCount 项",
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.Medium
                    )
                    Text(
                        text = if (selectedCount == state.files.size) "已全选" else "点击选择更多文件",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
                TextButton(onClick = onToggleAll) {
                    Text(if (selectedCount == state.files.size) "取消全选" else "全选")
                }
            } else {
                IconButton(onClick = onBack) {
                    Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回")
                }
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = "自建云盘",
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.Medium,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis
                    )
                    Text(
                        text = "共 ${state.files.size} 项",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
                if (onOpenTransfers != null) {
                    IconButton(onClick = onOpenTransfers) {
                        Icon(Icons.Outlined.History, contentDescription = "转存记录")
                    }
                }
                IconButton(onClick = onRefresh) {
                    Icon(Icons.Outlined.Refresh, contentDescription = "刷新")
                }
            }
        }
        if (!multiSelectMode) {
            CrumbBar(
                rootTitle = "自建云盘",
                pathNames = state.pathNames,
                onNavigate = onNavigate
            )
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MangoActionSheet(
    file: ShareFile,
    onDownload: () -> Unit,
    onDownloadTo: () -> Unit,
    onDownloadFolder: (() -> Unit)? = null,
    onDownloadFolderTo: (() -> Unit)? = null,
    onMultiSelect: (() -> Unit)? = null,
    onDismiss: () -> Unit
) {
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(
        onDismissRequest = onDismiss,
        sheetState = sheetState,
        containerColor = MaterialTheme.colorScheme.surface
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(start = 24.dp, end = 24.dp, top = 4.dp, bottom = 32.dp)
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Surface(
                    modifier = Modifier.size(40.dp),
                    shape = MaterialTheme.shapes.large,
                    color = MaterialTheme.colorScheme.primaryContainer
                ) {
                    Box(contentAlignment = Alignment.Center) {
                        Icon(
                            imageVector = if (file.isdir) Icons.Outlined.FolderOpen else Icons.Outlined.Download,
                            contentDescription = null,
                            tint = MaterialTheme.colorScheme.onPrimaryContainer
                        )
                    }
                }
                Spacer(modifier = Modifier.width(12.dp))
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        file.fname,
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.Medium,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis
                    )
                    Text(
                        text = if (file.isdir) "文件夹" else formatMangoSize(file.fsize),
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
            }
            Spacer(modifier = Modifier.height(12.dp))
            HorizontalDivider()
            Spacer(modifier = Modifier.height(8.dp))
            if (!file.isdir) {
                MangoActionItem(Icons.Outlined.Download, "下载", "使用内置分片下载保存到本机", MaterialTheme.colorScheme.primary, onDownload)
                MangoActionItem(Icons.Outlined.FolderOpen, "下载到…", "选择本次保存的文件夹（不改全局设置）", MaterialTheme.colorScheme.primary, onDownloadTo)
            } else {
                if (onDownloadFolder != null) {
                    MangoActionItem(Icons.Outlined.Download, "下载文件夹", "递归下载整个文件夹，保持目录结构", MaterialTheme.colorScheme.primary, onDownloadFolder)
                }
                if (onDownloadFolderTo != null) {
                    MangoActionItem(Icons.Outlined.FolderOpen, "下载文件夹到…", "选择本次保存的文件夹（不改全局设置）", MaterialTheme.colorScheme.primary, onDownloadFolderTo)
                }
            }
            if (onMultiSelect != null) {
                MangoActionItem(Icons.Outlined.CheckCircle, "多选", "勾选多个项目批量下载", MaterialTheme.colorScheme.primary, onMultiSelect)
            }
        }
    }
}

@Composable
private fun MangoActionItem(
    icon: ImageVector,
    title: String,
    desc: String,
    tint: androidx.compose.ui.graphics.Color,
    onClick: () -> Unit
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(vertical = 10.dp, horizontal = 4.dp),
        verticalAlignment = Alignment.CenterVertically
    ) {
        Surface(
            modifier = Modifier.size(38.dp),
            shape = MaterialTheme.shapes.large,
            color = MaterialTheme.colorScheme.surfaceContainerHigh
        ) {
            Box(contentAlignment = Alignment.Center) {
                Icon(icon, contentDescription = null, tint = tint)
            }
        }
        Spacer(modifier = Modifier.width(12.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge, color = tint)
            Text(
                desc,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
        }
    }
}

/** 文件大小格式化（私有盘目录项可能不带大小：-1 → 未知） */
internal fun formatMangoSize(bytes: Long): String {
    if (bytes < 0) return "未知大小"
    if (bytes == 0L) return "0 B"
    val units = arrayOf("B", "KB", "MB", "GB", "TB")
    var value = bytes.toDouble()
    var unit = 0
    while (value >= 1024 && unit < units.size - 1) {
        value /= 1024
        unit++
    }
    return if (unit == 0) "$bytes B" else String.format("%.1f %s", value, units[unit])
}
