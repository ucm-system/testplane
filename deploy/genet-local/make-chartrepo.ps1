# TestPlane infra · 一键「把我们的 chart 改名冒充成 Genet 固定 chart」并刷新 ChartMuseum
#
# 背景：真 Genet 的 Open API 不能指定任意 chart —— `POST /api/open/helm/uc-packages/{stack|pd}/releases`
# 永远用 `backend.config.helm.ucPackages.{stack|pd}.chart`（本地是 `charts/unified-cache-stack@0.0.1`）。
# 想让它装我们的服务，就把 `deploy/charts/mock-llm` 复制一份、改掉 name/version，冒充成那个 chart。
#
# ⚠️ 冒充出来的 chart **是 Genet 侧资产，不入库**：产物落在 `.data/packages/<name>/`，
#    仓库里只保留本脚本。chart 的真实源码仍是 `deploy/charts/mock-llm`。
#
# 用法（仓库根执行）：
#   pwsh deploy/genet-local/make-chartrepo.ps1                                   # 默认 unified-cache-stack 0.0.1
#   pwsh deploy/genet-local/make-chartrepo.ps1 -Name other-chart -Version 1.2.3
#   pwsh deploy/genet-local/make-chartrepo.ps1 -SkipCacheWait                    # 不等 Genet 缓存刷新
#
# 步骤：
#   1) 复制源 chart → .data/packages/<Name>/，重写 Chart.yaml 的 name/version
#   2) 调用 build-chartrepo.ps1（helm package + 重建假 ChartMuseum 的 CM/Deployment/Service）
#   3) 重启 genet-backend 触发立刻刷新，并轮询日志等 `Chart cache refreshed`（TTL 300s，重启是唯一立刻生效的办法）
[CmdletBinding()]
param(
    [string]$Name = 'unified-cache-stack',
    [string]$Version = '0.0.1',
    [string]$SourceChart = 'deploy/charts/mock-llm',
    [string]$RepoRoot,
    [switch]$SkipCacheWait
)

$ErrorActionPreference = 'Stop'
if (-not $RepoRoot) { $RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path }
$helm = Join-Path $RepoRoot '.tools\helm.exe'
if (-not (Test-Path -LiteralPath $helm)) { throw "找不到 $helm（先按 deploy/README.md 第 1 步装 helm）" }

$src = Join-Path $RepoRoot $SourceChart
if (-not (Test-Path -LiteralPath $src)) { throw "源 chart 不存在: $src" }

$pkgRoot = Join-Path $RepoRoot '.data\packages'
$dst = Join-Path $pkgRoot $Name
New-Item -ItemType Directory -Force -Path $pkgRoot | Out-Null

Write-Host "== 1/3 生成冒充 chart: $Name $Version =="
if (Test-Path -LiteralPath $dst) { Remove-Item -Recurse -Force $dst }
Copy-Item -Recurse $src $dst
$chartYamlPath = Join-Path $dst 'Chart.yaml'
$cy = Get-Content -LiteralPath $chartYamlPath -Raw -Encoding UTF8
if ($cy -notmatch '(?m)^name:\s') { throw "Chart.yaml 里没有 name: 字段" }
$cy = [regex]::Replace($cy, '(?m)^name:\s*\S+', "name: $Name")
if ($cy -match '(?m)^version:\s') {
    $cy = [regex]::Replace($cy, '(?m)^version:\s*\S+', "version: $Version")
} else {
    $cy = $cy.TrimEnd() + "`nversion: $Version`n"
}
# 顶部留一行来源说明，方便以后认出来这是冒充件
$cy = "# 本地联调生成物（$((Get-Date).ToString('yyyy-MM-dd')) 由 deploy/genet-local/make-chartrepo.ps1 生成）`n" +
      "# 内容 = $SourceChart 的副本，仅改了 name/version 用来冒充 Genet 固定 chart；不要提交、不要改这份。`n" + $cy
[IO.File]::WriteAllText($chartYamlPath, $cy, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "   $dst"

Write-Host "== 2/3 打包 + 重建假 ChartMuseum =="
& (Join-Path $PSScriptRoot 'build-chartrepo.ps1') -RepoRoot $RepoRoot

if ($SkipCacheWait) { Write-Host '== 3/3 跳过等缓存（-SkipCacheWait）=='; return }

Write-Host '== 3/3 重启 genet-backend 并等 Chart cache refreshed =='
& kubectl --context kind-testplane -n genet rollout restart deploy/genet-backend | Write-Host
& kubectl --context kind-testplane -n genet rollout status deploy/genet-backend --timeout=180s | Write-Host

$deadline = (Get-Date).AddMinutes(3)
$ok = $false
# 期望缓存里至少有：mock-llm + .data/packages 下每个冒充件
$want = 1 + (@(Get-ChildItem -LiteralPath (Join-Path $RepoRoot '.data\packages') -Directory -ErrorAction SilentlyContinue).Count)
if ($want -lt 2) { $want = 2 }
while ((Get-Date) -lt $deadline) {
    $line = & kubectl --context kind-testplane -n genet logs -l app=genet-backend --tail=200 2>$null |
            Select-String 'Chart cache refreshed' | Select-Object -Last 1
    if ($line -and $line.Line -match '"totalChartsInCache":\s*(\d+)' -and [int]$Matches[1] -ge $want) {
        Write-Host "   $($line.Line.Trim())"
        $ok = $true
        break
    }
    Start-Sleep -Seconds 5
}
if (-not $ok) { throw "3 分钟内没等到 Chart cache refreshed（totalChartsInCache >= $want）；手动看：kubectl -n genet logs -l app=genet-backend | Select-String 'Chart cache'" }

Write-Host ''
Write-Host '完成。自检：'
Write-Host "  kubectl --context kind-testplane -n genet exec deploy/genet-chartrepo -- wget -qO- http://127.0.0.1/api/chartrepo/charts/charts"
Write-Host '  然后 POST /api/open/helm/uc-packages/stack/releases（values 至少给一行，如 replicaCount: 1）'
