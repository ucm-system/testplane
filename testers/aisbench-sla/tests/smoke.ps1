# TestPlane · aisbench-sla Tester 离线冒烟（Windows / pwsh）
#
# 不经平台：docker run 一次 Tester 镜像，容器内起 mock vLLM + 用 stub_aisbench.py 当假
# ais_bench，跑完整的 /tester/run.sh（成功路径 + 退出码 2/3 两个故障路径），然后断言：
#   - 容器退出码 0
#   - result.json 的 status = succeeded、error = null
#   - metrics 的**名字/单位/统计量/labels** 符合附录 A 口径（ttft / tpot / hit_rate /
#     output_throughput / sla_max_concurrency / probes_total），且没有旧名字
#   - artifacts 列出 sla_summary.json 与 sla_points.csv，且文件真实存在
#   - cases/exit2、cases/exit3 的 result.json 分别映射到 SLA_PRECHECK_FAILED /
#     SLA_NO_FEASIBLE_CONCURRENCY（退出码 2/3 的自动化覆盖）
#   - 带 ENDPOINT_API_KEY canary 跑完，产物目录里 grep 不到它（凭据零泄漏）
#
# 用法：
#   pwsh testers/aisbench-sla/tests/smoke.ps1
#   pwsh testers/aisbench-sla/tests/smoke.ps1 -Image aisbench-sla:t1 -OutputRoot D:\tmp\smoke
#
# 退出码：0 通过；1 断言失败；2 环境/镜像问题。

[CmdletBinding()]
param(
    # stub 小镜像（Dockerfile 构建），不是 6.4 GB 的真 ais_bench 镜像：
    # 两者 tag 约定见 README「构建与推送」——`:0.4.0` = 真版，`:0.4.0-stub` = stub 版。
    [string]$Image = 'ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0-stub',
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
    Write-Host "镜像 $Image 不存在；先构建（context = Tester 目录，与发版矩阵一致）：" -ForegroundColor Red
    Write-Host "  docker build -f testers/aisbench-sla/build/Dockerfile -t $Image testers/aisbench-sla"
    exit 2
}

# 挂载 tests/ 进容器：smoke.sh 与 fixtures 都在里面，镜像不必包含它们。
$testsDir = ($here -replace '\\', '/')
$outDir = ((Resolve-Path $OutputRoot).Path -replace '\\', '/')

Write-Host "==> docker run $Image（离线：mock vLLM + stub ais_bench，含退出码 2/3 用例）"
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

$byName = @{}
foreach ($m in @($result.metrics)) { $byName[$m.name] = $m }
$expected = [ordered]@{
    'sla_max_concurrency' = @('count', 'count')
    'output_throughput'   = @('tokens_per_s', 'max')
    'ttft'                = @('ms', 'mean')
    'tpot'                = @('ms', 'p90')
    'probes_total'        = @('count', 'count')
}
foreach ($name in $expected.Keys) {
    $m = $byName[$name]
    if ($null -eq $m) { $problems += "缺少指标 $name"; continue }
    if ($m.unit -ne $expected[$name][0]) { $problems += "$name.unit=$($m.unit)（期望 $($expected[$name][0])）" }
    if ($m.statistic -ne $expected[$name][1]) { $problems += "$name.statistic=$($m.statistic)（期望 $($expected[$name][1])）" }
}
foreach ($legacy in @('ttft_avg_ms', 'tpot_p90_ms', 'hit_rate_percent', 'max_output_throughput_tok_s')) {
    if ($byName.ContainsKey($legacy)) { $problems += "仍在使用旧指标名 $legacy" }
}
if ($byName['sla_max_concurrency'].labels.scope -ne 'search') { $problems += 'sla_max_concurrency 缺 labels.scope=search' }
if ($byName['probes_total'].labels.scope -ne 'search') { $problems += 'probes_total 缺 labels.scope=search' }
if ($null -eq $byName['output_throughput'].labels.concurrency) { $problems += 'output_throughput 缺 labels.concurrency' }
if ($null -eq $byName['ttft'].labels.concurrency) { $problems += 'ttft 缺 labels.concurrency' }
if ($null -eq $result.workload.concurrency) { $problems += 'workload 缺 concurrency' }

$paths = @($result.artifacts | ForEach-Object { $_.path })
foreach ($p in @('sla_out/sla_summary.json', 'sla_out/sla_points.csv')) {
    if ($paths -notcontains $p) { $problems += "artifacts 未列出 $p" }
}
foreach ($f in @('sla_out\sla_summary.json', 'sla_out\sla_points.csv', 'sla_out\run.log')) {
    if (-not (Test-Path -LiteralPath (Join-Path $OutputRoot $f))) { $problems += "缺少产物 $f" }
}

# 降级用例：request_rate 很小 → 并发压不满 → 该点 degraded，sla_max_concurrency 退回到达标点
$degPath = Join-Path $OutputRoot 'cases\degraded\result.json'
if (-not (Test-Path -LiteralPath $degPath)) {
    $problems += '缺少 cases\degraded\result.json（降级用例没跑）'
} else {
    $deg = Get-Content -Raw -Encoding utf8 $degPath | ConvertFrom-Json
    $degSummaryPath = Join-Path $OutputRoot 'cases\degraded\sla_out\sla_summary.json'
    if ($deg.status -ne 'succeeded') { $problems += "降级用例: status=$($deg.status)（期望 succeeded）" }
    if ($deg.notes -notmatch '并发点降级') { $problems += '降级用例: notes 没写降级说明' }
    $degMaxOk = $null
    if (Test-Path -LiteralPath $degSummaryPath) {
        $degMaxOk = (Get-Content -Raw -Encoding utf8 $degSummaryPath | ConvertFrom-Json).max_ok_concurrency
    }
    $degReported = ($deg.metrics | Where-Object { $_.name -eq 'sla_max_concurrency' }).value
    if ($null -eq $degReported) {
        $problems += '降级用例: 没有 sla_max_concurrency（期望退回到达标的 c=1）'
    } elseif ($null -ne $degMaxOk -and $degReported -ge $degMaxOk) {
        $problems += "降级用例: sla_max_concurrency=$degReported 等于/超过工具口径 $degMaxOk"
    }
    Write-Host "==> 降级用例：max_ok=$degMaxOk → sla_max_concurrency=$degReported"
}

# 退出码 2 / 3 的自动化覆盖（容器内 smoke.sh 负责跑，这里复核落盘结果）
$cases = @(
    @{ dir = 'cases\exit2'; code = 'SLA_PRECHECK_FAILED'; toolStatus = 'preflight_failed' },
    @{ dir = 'cases\exit3'; code = 'SLA_NO_FEASIBLE_CONCURRENCY'; toolStatus = 'none_passed' }
)
foreach ($case in $cases) {
    $casePath = Join-Path $OutputRoot (Join-Path $case.dir 'result.json')
    if (-not (Test-Path -LiteralPath $casePath)) {
        $problems += "缺少 $($case.dir)\result.json（退出码用例没跑）"
        continue
    }
    $caseResult = Get-Content -Raw -Encoding utf8 $casePath | ConvertFrom-Json
    if ($caseResult.status -ne 'failed') { $problems += "$($case.dir): status=$($caseResult.status)（期望 failed）" }
    if ($caseResult.error.code -ne $case.code) { $problems += "$($case.dir): error.code=$($caseResult.error.code)（期望 $($case.code)）" }
    if ($caseResult.error.detail.tool_status -ne $case.toolStatus) {
        $problems += "$($case.dir): detail.tool_status=$($caseResult.error.detail.tool_status)（期望 $($case.toolStatus)）"
    }
}

# 凭据零泄漏（smoke.sh 里已 grep 过；宿主侧再扫一遍落盘产物）
$canary = 'sk-test-SECRET123'
$leaks = @(Get-ChildItem -Recurse -File -LiteralPath $OutputRoot -ErrorAction SilentlyContinue |
    Where-Object { (Get-Content -Raw -Encoding utf8 -LiteralPath $_.FullName -ErrorAction SilentlyContinue) -match [regex]::Escape($canary) })
if ($leaks.Count -gt 0) {
    $problems += "凭据 canary 出现在产物里：" + (($leaks | ForEach-Object { $_.Name }) -join ', ')
}

Write-Host ""
Write-Host "==> result.json 摘要"
$result.metrics | ForEach-Object { "    {0} = {1} {2} ({3}) labels={4}" -f $_.name, $_.value, $_.unit, $_.statistic, ($_.labels | ConvertTo-Json -Compress) }
"    artifacts = {0} 个；status = {1}" -f @($result.artifacts).Count, $result.status
"    notes = {0}" -f $result.notes

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
