# TestPlane · 失败注入（经平台，Windows / pwsh）
#
# 前置：控制面可用（8080）、kind 集群里能跑 Tester Pod、mock-llm 镜像已 load。
# 会创建 5 个 fixture Run，并部署 4 个 mock-llm 故障变体（default ns，可 -Cleanup 清掉）。
#
# 场景与设计预期（C-5.8 终态判定优先级，Lead 2026-11-05 已拍板）：
#   success       mock-llm（正常）            -> Run succeeded
#   fail-500      持续 500（可重试）          -> service_unavailable，precheck failed(retryable=true)，其余 skipped(upstream_abort)
#   fail-401      401（不可重试）             -> completed_with_errors，precheck failed(retryable=false)，其余 skipped(upstream_abort)
#   error-in-200  200 + 错误对象（不可重试）  -> completed_with_errors，同上
#   slow-ready    窗口内永不就绪（可重试）    -> service_unavailable，retryable=true，耗满 precheck 窗口
#
# 判定口径（C-5.8 #4 / #6）：
#   - precheck 失败且 retryable=true（窗口耗尽的 PRECHECK_FAILED / ENDPOINT_UNREACHABLE）→ service_unavailable；
#   - precheck 失败且 retryable=false（401/403、200 包错误、DNS/TLS）→ completed_with_errors，
#     precheck Task 是 failed（不是 skipped），其余 Task skipped(upstream_abort)。
#   若实测与这条不一致，是后端 bug；脚本会判红并把实际终态打出来。
#
# 平台错误（如 Job 创建失败）预期 completed_with_errors 而不是 failed。
# 已知证据：executor job-spec 失败时 Run 终态就是 completed_with_errors（ticket #3 comment 的
# r-20261105-G3K3 / Z6TJ）。需要自动化时可临时移除 Role 的 jobs:create 权限，
# 脚本不默认做（会影响共享集群）。
#
# 用法：
#   pwsh scripts/e2e/failure-injection.ps1
#   pwsh scripts/e2e/failure-injection.ps1 -BaseURL http://127.0.0.1:8080
#   pwsh scripts/e2e/failure-injection.ps1 -Only fail-401,success
#   pwsh scripts/e2e/failure-injection.ps1 -SkipApply -Cleanup
#
# 退出码：0 全部符合；1 有场景不符；2 环境/构建问题。

[CmdletBinding()]
param(
    [string]$BaseURL = 'http://127.0.0.1:8080',
    [string]$Namespace = 'default',
    [string]$Profile = 'k8s-npu',
    [string]$MockImage = 'testplane/mock-llm:dev',
    [string]$OutDir = '',
    [string[]]$Only = @(),
    [switch]$SkipApply,
    [switch]$Cleanup
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) {
    $OutDir = Join-Path $repo '.data\qa-failure'
}
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$faultsYAML = Join-Path $repo 'scripts\e2e\kind\mock-llm-faults.yaml'
$tmp = [System.IO.Path]::GetTempPath()

# ---- 1. 故障 mock 变体 -----------------------------------------------------
if (-not $SkipApply) {
    Write-Host "部署 mock-llm 故障变体（ns=$Namespace）…"
    & kubectl apply -f $faultsYAML
    if ($LASTEXITCODE -ne 0) {
        Write-Host "kubectl apply 失败（镜像 $MockImage 是否已经 kind load？）" -ForegroundColor Red
        exit 2
    }
    foreach ($d in 'mock-llm-500', 'mock-llm-401', 'mock-llm-error200', 'mock-llm-slow') {
        & kubectl -n $Namespace rollout status "deployment/$d" --timeout=90s | Out-Host
        if ($LASTEXITCODE -ne 0) {
            Write-Host "deployment/$d 未就绪" -ForegroundColor Red
            exit 2
        }
    }
}

# ---- 2. 构建 e2e -----------------------------------------------------------
Push-Location (Join-Path $repo 'tools\e2e')
$e2eExe = Join-Path $tmp 'testplane-e2e.exe'
& go build -o $e2eExe .
Pop-Location
if ($LASTEXITCODE -ne 0) {
    Write-Host "go build e2e 失败" -ForegroundColor Red
    exit 2
}

# ---- 3. 场景 ---------------------------------------------------------------
$allScenarios = @(
    @{ Name = 'success';      Service = 'mock-llm';          Expect = 'succeeded';                                       TaskErr = '';                Retryable = '';      MaxSeconds = 300; Note = '' }
    @{ Name = 'fail-500';     Service = 'mock-llm-500';      Expect = 'service_unavailable';                             TaskErr = 'PRECHECK_FAILED'; Retryable = 'true';  MaxSeconds = 180; Note = '可重试' }
    @{ Name = 'fail-401';     Service = 'mock-llm-401';      Expect = 'completed_with_errors';                            TaskErr = 'PRECHECK_FAILED'; Retryable = 'false'; MaxSeconds = 120; Note = '不可重试 → completed_with_errors' }
    @{ Name = 'error-in-200'; Service = 'mock-llm-error200'; Expect = 'completed_with_errors';                            TaskErr = 'PRECHECK_FAILED'; Retryable = 'false'; MaxSeconds = 120; Note = '不可重试 → completed_with_errors' }
    @{ Name = 'slow-ready';   Service = 'mock-llm-slow';     Expect = 'service_unavailable';                             TaskErr = 'PRECHECK_FAILED'; Retryable = 'true';  MaxSeconds = 180; Note = '窗口内不可就绪' }
)
$scenarios = $allScenarios
if ($Only.Count -gt 0) {
    $scenarios = $allScenarios | Where-Object { $Only -contains $_.Name }
    if ($scenarios.Count -eq 0) {
        Write-Host "-Only 没有匹配任何场景；可选：$($allScenarios.Name -join ', ')" -ForegroundColor Red
        exit 2
    }
}

function New-FixtureManifest {
    param([hashtable]$Scenario)
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $url = "http://$($Scenario.Service).$Namespace.svc.cluster.local:18080/v1"
    return @"
# 由 scripts/e2e/failure-injection.ps1 生成：$($Scenario.Name)
run:
  name: "fi-$($Scenario.Name)-$stamp"
  service:
    url: $url
    model_name: mock-llm
  precheck:
    tester: endpoint-probe
    version: "1.0.0"
    execution: { profile: $Profile }
    args: "--wait_timeout_seconds=8 --retry_interval_seconds=1"
  cleanup: { policy: never }
  tests:
    - name: probe-a
      tester: endpoint-probe
      version: "1.0.0"
      execution: { profile: $Profile }
      args: "--wait_timeout_seconds=8 --retry_interval_seconds=1"
"@
}

$rows = @()
$failed = 0
$serverOk = $false
try {
    $null = Invoke-WebRequest -UseBasicParsing "$BaseURL/healthz" -TimeoutSec 3
    $serverOk = $true
} catch {
    Write-Host "控制面不可达：$BaseURL（先启动控制面）" -ForegroundColor Red
    exit 3
}

foreach ($s in $scenarios) {
    $manifestPath = Join-Path $OutDir ("manifest-" + $s.Name + ".yaml")
    Set-Content -Encoding utf8 -NoNewline -Path $manifestPath -Value (New-FixtureManifest -Scenario $s)
    $jsonPath = Join-Path $OutDir ("result-" + $s.Name + ".json")

    $e2eArgs = @(
        'run',
        '--base-url', $BaseURL,
        '--manifest', $manifestPath,
        '--expect', $s.Expect,
        '--poll', '1s',
        '--timeout', '3m',
        '--json-out', $jsonPath
    )
    if ($s.TaskErr -ne '') { $e2eArgs += @('--expect-task-error-code', $s.TaskErr) }
    if ($s.Retryable -ne '') { $e2eArgs += @('--expect-retryable=' + $s.Retryable) }

    Write-Host ""
    Write-Host ("==> 场景 $($s.Name)：期望 $($s.Expect)" + $(if ($s.TaskErr) { " / $($s.TaskErr) / retryable=$($s.Retryable)" } else { "" }))
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $log = & $e2eExe @e2eArgs 2>&1 | Out-String
    $exit = $LASTEXITCODE
    $sw.Stop()
    $log | Set-Content -Encoding utf8 (Join-Path $OutDir ("e2e-" + $s.Name + ".log"))

    $res = $null
    if (Test-Path -LiteralPath $jsonPath) {
        $res = Get-Content -Raw -Encoding utf8 $jsonPath | ConvertFrom-Json
    }
    $actual = ''
    if ($res) { $actual = [string]$res.actual }

    $problems = @()
    if ($exit -ne 0) { $problems += "e2e 退出码 $exit（期望 0）" }
    if (-not $res) { $problems += "没有 result JSON" }
    elseif (-not $res.ok) { $problems += "e2e ok=false" }
    if ($actual -ne $s.Expect) { $problems += "终态 $actual（期望 $($s.Expect)）" }
    if ($sw.Elapsed.TotalSeconds -gt $s.MaxSeconds) {
        $problems += ("耗时 {0:N1}s > {1}s" -f $sw.Elapsed.TotalSeconds, $s.MaxSeconds)
    }
    $ok = ($problems.Count -eq 0)
    if (-not $ok) {
        $failed++
        Write-Host ($problems -join '；') -ForegroundColor Red
        Write-Host ($log.TrimEnd()) -ForegroundColor DarkGray
    }

    $rows += [pscustomobject]@{
        scenario = $s.Name
        ok       = $ok
        exit     = $exit
        actual   = if ($actual -ne '') { $actual } else { '-' }
        seconds  = [math]::Round($sw.Elapsed.TotalSeconds, 1)
        note     = $s.Note
        problem  = ($problems -join '；')
    }
}

$rows | Format-Table -AutoSize | Out-String -Width 200 | Write-Host

if ($Cleanup) {
    Write-Host "清理故障 mock 变体…"
    & kubectl delete -f $faultsYAML --ignore-not-found | Out-Host
}

if ($failed -gt 0) {
    Write-Host "$failed 个场景不符合预期；明细在 $OutDir" -ForegroundColor Red
    exit 1
}
Write-Host "全部失败注入场景符合预期；明细在 $OutDir" -ForegroundColor Green
exit 0
