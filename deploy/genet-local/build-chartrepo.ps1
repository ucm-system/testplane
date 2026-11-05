# TestPlane infra · 打包 mock-llm chart 并部署「假 ChartMuseum」
#
# 用途：Genet 的 Helm 管理功能只认 harbor-oci / harbor-chartmuseum 两种 chart 仓库。
# 前者要真 Harbor artifacts API（普通 registry:2 不行），后者可以用静态 nginx 假冒 ——
# 本脚本把 deploy/charts/mock-llm 打成 tgz，塞进 ConfigMap，再 apply deploy/genet-local/chartrepo.yaml。
#
# 用法（仓库根执行）：
#   pwsh deploy/genet-local/build-chartrepo.ps1
#
# 产物：
#   - .data/chartrepo/mock-llm-<version>.tgz   （helm package 的原始产物）
#   - .data/genet-local/chartrepo.rendered.yaml（占位符已替换、可直接 apply 的清单）
#   - 集群里的 cm/deploy/svc genet-chartrepo（namespace genet）
#
# 改了 chart 版本要同步改 deploy/genet-local/chartrepo.yaml 里
# 的 key 名 `mock-llm-0.1.0.tgz` 与 charts 列表里的 version/urls。
[CmdletBinding()]
param(
    [string]$Namespace = 'genet',
    [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'
if (-not $RepoRoot) { $RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path }
$helm = Join-Path $RepoRoot '.tools\helm.exe'
if (-not (Test-Path -LiteralPath $helm)) { throw "找不到 $helm（先按 deploy/README.md 第 1 步装 helm）" }
$kubectl = 'kubectl'
$ctx = 'kind-testplane'

$chartDir = Join-Path $RepoRoot 'deploy\charts\mock-llm'
$tmpl = Join-Path $PSScriptRoot 'chartrepo.yaml'
$outDir = Join-Path $RepoRoot '.data\chartrepo'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

Write-Host "== 1/4 打包 chart =="
& $helm package $chartDir -d $outDir | Write-Host
$tgz = Get-ChildItem $outDir -Filter 'mock-llm-*.tgz' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
if (-not $tgz) { throw "helm package 没产出 tgz" }
Write-Host "   $($tgz.Name)  $($tgz.Length) B"

Write-Host "== 2/4 替换占位符 =="
$manifest = Get-Content -LiteralPath $tmpl -Raw -Encoding UTF8
$expectedKey = "$($tgz.BaseName -replace '^mock-llm-','mock-llm-').tgz"
if ($manifest -notmatch [regex]::Escape($tgz.Name)) {
    throw "chartrepo.yaml 里没有出现 $($tgz.Name)：chart 版本变了，请同步改清单里的 binaryData key 与 charts.url"
}
$b64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes($tgz.FullName))
$manifest = $manifest -replace '__CHART_TGZ_BASE64__', $b64
$rendered = Join-Path $RepoRoot '.data\genet-local\chartrepo.rendered.yaml'
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $rendered) | Out-Null
[IO.File]::WriteAllText($rendered, $manifest, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "   → $rendered"

Write-Host "== 3/4 apply =="
& $kubectl --context $ctx apply -f $rendered | Write-Host

Write-Host "== 4/4 等就绪 =="
& $kubectl --context $ctx -n $Namespace rollout status deploy/genet-chartrepo --timeout=120s | Write-Host

Write-Host ''
Write-Host '自检（从集群内打一次，确认 Genet 会看到的两个路径都对）：'
Write-Host "  kubectl --context $ctx -n $Namespace exec deploy/genet-chartrepo -- wget -qO- http://127.0.0.1/api/chartrepo/charts/charts"
Write-Host "  kubectl --context $ctx -n $Namespace exec deploy/genet-chartrepo -- sh -c 'wget -qO- http://127.0.0.1/chartrepo/charts/charts/mock-llm-0.1.0.tgz | wc -c'"
