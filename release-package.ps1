# YunX-Desktop 发版脚本：把 release\YunX-Desktop 压成 zip 并创建 GitHub Release
# 用法(本地): .\release-package.ps1 -Tag v0.1.0
# 用法(CI):   release.yml 调用，gh 用 GITHUB_TOKEN 认证（runner 预装 gh 与 7z）
# 注意: 本文件必须保持 UTF-8 带 BOM 编码（PS 5.1 会把无 BOM 的 UTF-8 当 ANSI 解析，中文变乱码且破坏引号配对）

param(
    [Parameter(Mandatory=$true)]
    [string]$Tag
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

$name = "YunX-Desktop-portable-$Tag"
Push-Location .\release
try {
    # 进入 release 目录再打包，使 zip 根目录直接是 YunX-Desktop\（解开即可双击 exe）
    & 7z a -tzip -mx=5 "..\$name.zip" "YunX-Desktop" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "7z packaging failed with exit $LASTEXITCODE" }
} finally {
    Pop-Location
}
$mb = [math]::Round((Get-Item "$name.zip").Length / 1MB, 1)
Write-Host "[OK] release asset: $name.zip ($mb MB)"

$notes = "PanRouter 桌面端 $Tag 便携版（免安装，解压即用）。`n`n基于 YunX-Desktop（[tidain/YunX-Desktop](https://github.com/tidain/YunX-Desktop)，AGPL-3.0）二次开发，整合 PanRouter 服务端解析。源码与更新：https://github.com/dengyie/PanRouter"
gh release create $Tag "$name.zip" --title $Tag --notes $notes
if ($LASTEXITCODE -ne 0) { throw "gh release create failed with exit $LASTEXITCODE" }
Write-Host "[OK] Release $Tag published" -ForegroundColor Green
