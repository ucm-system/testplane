# TestPlane · 渲染 kind 集群配置模板
#
# 为什么要有这一步：kind 的 extraMounts 里是**宿主绝对路径**，写死在仓库里就会
# 变成"只有作者那台机器能建集群"。所以 `deploy/kind/testplane.yaml` 是模板
# （含占位符 `__STORAGE_HOST_PATH__`），由本脚本渲染成一份可直接给 kind 的清单。
#
# 用法（在仓库根或任意目录都行）：
#   pwsh deploy/kind/render.ps1
#       → 宿主路径默认 <仓库根>/.data/storage，产物 .data/kind-testplane.yaml
#   pwsh deploy/kind/render.ps1 -StoragePath D:\data\tp-storage
#       → 换成别的盘/目录（不存在会自动创建）
#   pwsh deploy/kind/render.ps1 -OutFile .data\my-kind.yaml
#
# 产物位置默认 `.data/`（已 gitignore）：渲染结果与机器相关，**不入库**。
#
# 接着：
#   .\.tools\kind.exe create cluster --config .data\kind-testplane.yaml --wait 180s
#
# 注意：改了模板或换了 -StoragePath 都必须 `kind delete cluster --name testplane` 再建，
# extraMounts 只在 create 时生效。
[CmdletBinding()]
param(
    # 宿主上给节点 /storage-root 用的目录。默认 <仓库根>/.data/storage。
    [string]$StoragePath,

    # 渲染产物路径。默认 <仓库根>/.data/kind-testplane.yaml。
    [string]$OutFile,

    # 仓库根；默认按本脚本位置推（deploy/kind/..\..）
    [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'

if (-not $RepoRoot) {
    $RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
}
if (-not $StoragePath) {
    $StoragePath = Join-Path $RepoRoot '.data\storage'
}
if (-not $OutFile) {
    $OutFile = Join-Path $RepoRoot '.data\kind-testplane.yaml'
}

$template = Join-Path $PSScriptRoot 'testplane.yaml'
if (-not (Test-Path -LiteralPath $template)) {
    throw "模板不存在: $template"
}

# 1) 目录必须存在：kind 的 extraMounts 指向不存在的宿主目录时，docker 会把它建成
#    一个 root 拥有的空目录（或直接失败），事后很难查。
if (-not (Test-Path -LiteralPath $StoragePath)) {
    New-Item -ItemType Directory -Force -Path $StoragePath | Out-Null
    Write-Host "已创建存储目录: $StoragePath"
}
$full = (Resolve-Path -LiteralPath $StoragePath).Path

# 2) Docker Desktop 的 -v 只认正斜杠：D:/x/y，反斜杠会被当成转义序列。
#    另外 Windows 路径可能含空格，kind 的 YAML 里要能原样读，用双引号包起来。
$posix = $full -replace '\\', '/'
if ($posix -match '[\s"]') {
    # 含空格/引号的路径在 kind 的 -v 里本就不可靠，直接拒绝比事后排查便宜
    throw "存储路径含空格或引号，kind 的 extraMounts 不支持: $full"
}

# 3) 渲染：只替换占位符，不解析 YAML（模板里有注释与缩进，正则替换最不容易失真）
$content = Get-Content -LiteralPath $template -Raw -Encoding UTF8
if ($content -notmatch '__STORAGE_HOST_PATH__') {
    throw "模板里找不到占位符 __STORAGE_HOST_PATH__，是不是已经被手改过？"
}
$content = $content -replace '__STORAGE_HOST_PATH__', $posix

$outDir = Split-Path -Parent $OutFile
if ($outDir -and -not (Test-Path -LiteralPath $outDir)) {
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
}
# kind 读文件按 UTF-8；PowerShell 5.1 的 -Encoding UTF8 会带 BOM，这里明确无 BOM
[System.IO.File]::WriteAllText($OutFile, $content, (New-Object System.Text.UTF8Encoding($false)))

Write-Host "已渲染: $OutFile"
Write-Host "  宿主存储路径: $posix  ->  节点 /storage-root"
Write-Host ''
Write-Host '下一步：'
Write-Host '  .\.tools\kind.exe create cluster --config .data\kind-testplane.yaml --wait 180s'
Write-Host '（改过模板或路径要先删除旧集群：.\.tools\kind.exe delete cluster --name testplane）'
