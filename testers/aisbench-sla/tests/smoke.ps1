# TestPlane · aisbench-sla Tester 离线冒烟（Windows / pwsh）
#
# 不经平台：docker run 一次 Tester 镜像，容器内起 mock vLLM + 用 stub_aisbench.py 当假
# ais_bench，跑完整的 /tester/run.sh，然后断言：
#   - 容器退出码 0
#   - result.json 的 status = succeeded、error = null
#   - metrics 里有 sla_max_concurrency / max_output_throughput_tok_s / probes_total
#   - artifacts 列出 sla_summary.json 与 sla_points.csv，且文件真实存在
#
# 用法：
#   pwsh testers/aisbench-sla/tests/smoke.ps1
#   pwsh testers/aisbench-sla/tests/smoke.ps1 -Image aisbench-sla:t1 -OutputRoot D:\tmp\smoke
#
# 退出码：0 通过；1 断言失败；2 环境/镜像问题。

[CmdletBinding()]
param(
    [string]$Image = 'ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.1.0',
    [string]$OutputRoot = ''
)

$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here '..\..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutputRoot)) {
    $OutputRoot = Join-Path $repo '.data\sla-tester\smoke'
}
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null

& docker image inspect $Image *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "镜像 $Image 不存在；先构建：" -ForegroundColor Red
    Write-Host "  docker build -f testers/aisbench-sla/build/Dockerfile -t $Image ."
    exit 2
}

# 挂载 tests/ 进容器：smoke.sh 与 fixtures 都在里面，镜像不必包含它们。
$testsDir = ($here -replace '\\', '/')
$outDir = ((Resolve-Path $OutputRoot).Path -replace '\\', '/')

Write-Host "==> docker run $Image（离线：mock vLLM + stub ais_bench）"
& docker run --rm `
    --entrypoint bash `
    -e "OUTPUT_DIR=/workspace/output" `
    -v "${outDir}:/workspace/output" `
    -v "${testsDir}:/tests:ro" `
    $Image /tests/smoke.sh
$rc = $LASTEXITCODE
Write-Host "==> 容器退出码 = $rc"

$resultPath = Join-Path $OutputRoot 'result.json'
if (-not (Test-Path -LiteralPath $resultPath)) {
    Write-Host "!! 没有产出 result.json（违反 §5.9）" -ForegroundColor Red
    exit 1
}

$result = Get-Content -Raw -Encoding utf8 $resultPath | ConvertFrom-Json
$problems = @()
if ($rc -ne 0) { $problems += "容器退出码 $rc（期望 0）" }
if ($result.status -ne 'succeeded') { $problems += "status=$($result.status)（期望 succeeded）" }
if ($null -ne $result.error) { $problems += "error 非空：$($result.error.code)" }

$metricNames = @($result.metrics | ForEach-Object { $_.name })
foreach ($name in @('sla_max_concurrency', 'max_output_throughput_tok_s', 'probes_total')) {
    if ($metricNames -notcontains $name) { $problems += "缺少指标 $name" }
}
$paths = @($result.artifacts | ForEach-Object { $_.path })
foreach ($p in @('sla_out/sla_summary.json', 'sla_out/sla_points.csv')) {
    if ($paths -notcontains $p) { $problems += "artifacts 未列出 $p" }
}
foreach ($f in @('sla_out\sla_summary.json', 'sla_out\sla_points.csv', 'sla_out\run.log')) {
    if (-not (Test-Path -LiteralPath (Join-Path $OutputRoot $f))) { $problems += "缺少产物 $f" }
}

Write-Host ""
Write-Host "==> result.json 摘要"
$result.metrics | ForEach-Object { "    {0} = {1} {2} ({3})" -f $_.name, $_.value, $_.unit, $_.statistic }
"    artifacts = {0} 个；status = {1}；notes = {2}" -f @($result.artifacts).Count, $result.status, $result.notes

if ($problems.Count -gt 0) {
    Write-Host ""
    Write-Host "!! 断言失败：" -ForegroundColor Red
    $problems | ForEach-Object { Write-Host "   - $_" -ForegroundColor Red }
    Write-Host "产物目录：$OutputRoot"
    exit 1
}
Write-Host ""
Write-Host "==> 冒烟通过；产物在 $OutputRoot" -ForegroundColor Green
exit 0
