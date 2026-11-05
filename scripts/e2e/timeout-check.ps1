# 超时路径验证（票 #3 / #16 修复版）
# 口径（Lead 2026-11-05 裁定）：
#   1) precheck:"off" + 单个 test Task 超时 → Run 必须 failed（TASK_TIMEOUT 是平台 C 类错误）；
#      真 Pre-Check 超时才按 C-5.8（retryable → service_unavailable）。
#   2) Attempt 时长容差 = timeout + 30s（watchdog 10s 粒度 + 删 Job grace），不要求 ≈5s。
#   3) 事件接受 task.state_changed running→failed + payload.error.code=TASK_TIMEOUT。
# 票 #16：docker/kubectl 查询失败必须 fail-fast；轮询上界与 run.within_30s 的断言阈值分离。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$Namespace = 'testplane',
    [string]$PgContainer = 'testplane-pg',
    [string]$PgUser = 'testplane',
    [string]$PgDb = 'testplane',
    [int]$AssertRunSeconds = 30,
    [int]$LoopLimitSeconds = 90,
    [int]$CleanupWaitSeconds = 60,
    [int]$TaskTimeoutSeconds = 5,
    [string]$OutDir = '',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-timeout' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$headers = @{ Authorization = "Bearer $User"; Accept = 'application/json' }

function Invoke-Api {
    param([string]$Method, [string]$Path, [string]$Body = '')
    $uri = "$BaseUrl$Path"
    if ($Body -ne '') {
        return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $Body
    }
    return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers
}

function Invoke-PsqlCount {
    param([string]$Sql)
    $out = & docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -c $Sql 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "docker exec psql 失败（exit $LASTEXITCODE）：$($out -join ' ')"
    }
    $text = (($out | Out-String).Trim())
    if ($text -notmatch '^\d+$') {
        throw "psql 输出无法解析为计数：$text"
    }
    return [int]$text
}

function Get-K8sObjectNames {
    param([string]$Kind, [string]$Selector)
    $out = & kubectl -n $Namespace get $Kind -l $Selector -o name 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl get $Kind -l $Selector 失败（exit $LASTEXITCODE）：$($out -join ' ')"
    }
    return @($out | Where-Object { $_ -ne '' })
}

function Get-K8sOneObject {
    param([string]$Kind, [string]$Name)
    $out = & kubectl -n $Namespace get $Kind $Name -o name 2>&1
    if ($LASTEXITCODE -ne 0) {
        # NotFound 是“已删除”的正常路径；其他错误（坏 context/权限）必须 throw。
        $text = ($out -join ' ')
        if ($text -match 'NotFound|not found') { return @() }
        throw "kubectl get $Kind $Name 失败（exit $LASTEXITCODE）：$text"
    }
    return @($out | Where-Object { $_ -ne '' })
}

function New-Manifest {
    param([string]$Stamp)
    return @"
{"run":{"name":"timeout-check-$Stamp","service":{"url":"http://mock-llm-slow.default.svc:18080/v1","model_name":"mock-llm"},"precheck":"off","tests":[{"name":"slow-timeout","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=600","timeout_seconds":$TaskTimeoutSeconds}]}}
"@
}

if ($DryRun) {
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    Write-Host "timeout-check dry-run（不连后端）" -ForegroundColor Cyan
    Write-Host "  base_url=$BaseUrl namespace=$Namespace"
    Write-Host "  断言：Run failed ≤ ${AssertRunSeconds}s；循环上界 ${LoopLimitSeconds}s；Attempt 容差 timeout+30s"
    Write-Host "  manifest:"
    Write-Host (New-Manifest -Stamp $stamp)
    Write-Host "dry-run OK" -ForegroundColor Green
    exit 0
}

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host "超时路径验证：$BaseUrl" -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$manifest = New-Manifest -Stamp $stamp
$submitted = Invoke-Api -Method POST -Path '/api/v1/runs' -Body $manifest
$runId = [string]$submitted.run.id
Write-Host "已提交 Run $runId（timeout_seconds=$TaskTimeoutSeconds，mock-llm-slow）"

# 轮询上界与断言阈值分离（票 #16）。
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt $LoopLimitSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$runId"
    $brief = ($run.tasks | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N1}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $brief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('failed', 'completed_with_errors', 'service_unavailable', 'succeeded', 'canceled')) { break }
    Start-Sleep -Milliseconds 500
}
$sw.Stop()
$terminalElapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan
Add-Check 'run.terminal_failed' ($run.status -eq 'failed') ("status=" + $run.status + "（期望 failed：TASK_TIMEOUT 属平台 C 类）")
Add-Check 'run.within_30s' ($terminalElapsed -le $AssertRunSeconds) ($terminalElapsed.ToString() + "s <= " + $AssertRunSeconds + "s（阈值独立于循环上界 " + $LoopLimitSeconds + "s）")

$taskList = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/tasks?limit=200").items
$task = @($taskList | Where-Object { $_.name -eq 'slow-timeout' })[0]
if (-not $task) { Add-Check 'task.present' $false '找不到 slow-timeout Task'; $task = $taskList[0] }
Add-Check 'task.present' ($null -ne $task) ("task=" + $task.id)
Add-Check 'task.failed' ($task.status -eq 'failed') ("status=" + $task.status)
Add-Check 'task.error_code' ($task.error.code -eq 'TASK_TIMEOUT') ("code=" + $task.error.code)
Add-Check 'task.retryable' ($task.error.retryable -eq $true) ("retryable=" + $task.error.retryable)

$attempt = @((Invoke-Api -Method GET -Path "/api/v1/tasks/$($task.id)/attempts").items)[0]
if (-not $attempt) {
    Add-Check 'attempt.present' $false '没有 Attempt'
} else {
    Add-Check 'attempt.present' $true ("attempt=" + $attempt.id)
    Add-Check 'attempt.terminal' ($attempt.status -in @('failed', 'canceled')) ("status=" + $attempt.status + "/phase=" + $attempt.phase)
    Add-Check 'attempt.error_code' ($attempt.error.code -eq 'TASK_TIMEOUT') ("code=" + $attempt.error.code)
    try {
        if ([string]::IsNullOrWhiteSpace([string]$attempt.started_at) -or [string]::IsNullOrWhiteSpace([string]$attempt.finished_at)) {
            throw "attempt 缺 started_at/finished_at"
        }
        $span = ([datetime]$attempt.finished_at - [datetime]$attempt.started_at).TotalSeconds
        if ($span -lt 0) { throw "attempt finished_at 早于 started_at" }
        $spanLimit = $TaskTimeoutSeconds + 30
        Add-Check 'attempt.timestamps_present' $true ("started_at/finished_at 可解析，span=" + [math]::Round($span, 2) + "s")
        Add-Check 'attempt.span_le_timeout_plus_30s' ($span -le $spanLimit) ([math]::Round($span, 2).ToString() + "s <= " + $spanLimit + "s（独立于轮询变量）")
    } catch {
        Add-Check 'attempt.timestamps_present' $false ($_.Exception.Message)
    }
}

# 事件：task.state_changed running->failed + payload.error.code=TASK_TIMEOUT，或有 task.timeout。
$events = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/events/history?limit=200").items
$timeoutEvent = @($events | Where-Object {
    $_.type -eq 'task.timeout' -or
    ($_.type -eq 'task.state_changed' -and $_.from -eq 'running' -and $_.to -eq 'failed' -and $_.payload.error.code -eq 'TASK_TIMEOUT')
})
Add-Check 'events.timeout_recorded' ($timeoutEvent.Count -gt 0) ("匹配事件 " + $timeoutEvent.Count + " 条（" + (($timeoutEvent | ForEach-Object { $_.type + '#' + $_.seq }) -join ', ') + "）")

# 清理：Job/Pod 删除 + 租约释放（终态后最多等 CleanupWaitSeconds）。
$cleanupSw = [System.Diagnostics.Stopwatch]::StartNew()
$podQueryFailed = $false
$jobQueryFailed = $false
$podsLeft = @()
$jobLeft = @()
while ($cleanupSw.Elapsed.TotalSeconds -lt $CleanupWaitSeconds) {
    try {
        $podsLeft = Get-K8sObjectNames -Kind 'pods' -Selector "job-name=tp-t-$($task.id.ToLower())-a1"
        $jobLeft = Get-K8sOneObject -Kind 'jobs' -Name "tp-t-$($task.id.ToLower())-a1"
    } catch {
        $podQueryFailed = $true
        Add-Check 'k8s.cleanup_query' $false ($_.Exception.Message)
        break
    }
    if ($podsLeft.Count -eq 0 -and $jobLeft.Count -eq 0) { break }
    Start-Sleep -Seconds 2
}
if (-not $podQueryFailed) {
    Add-Check 'k8s.cleanup_query' $true ("查询成功，等待 " + [math]::Round($cleanupSw.Elapsed.TotalSeconds, 1) + "s")
    Add-Check 'k8s.pod_deleted' ($podsLeft.Count -eq 0) $(if ($podsLeft.Count -eq 0) { '无 Pod 残留' } else { $podsLeft -join ', ' })
    Add-Check 'k8s.job_deleted' ($jobLeft.Count -eq 0) $(if ($jobLeft.Count -eq 0) { '无 Job 残留' } else { $jobLeft -join ', ' })
}

try {
    $leaseCount = Invoke-PsqlCount "select count(*) from endpoint_leases where task_id='$($task.id)';"
    Add-Check 'endpoint_leases.query' $true '查询成功'
    Add-Check 'endpoint_leases.empty' ($leaseCount -eq 0) ("剩余租约=$leaseCount")
} catch {
    Add-Check 'endpoint_leases.query' $false ($_.Exception.Message)
}

$statusRows = @()
try {
    $out = & docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -F '|' -c "select status,count(*) from runs group by status order by 2 desc,1;" 2>&1
    if ($LASTEXITCODE -ne 0) { throw "docker exec psql 失败（exit $LASTEXITCODE）：$($out -join ' ')" }
    $statusRows = @($out | Where-Object { $_ -ne '' })
} catch {
    Add-Check 'runs.status_query' $false ($_.Exception.Message)
}
Write-Host ""
Write-Host "runs 状态分布：" -ForegroundColor Cyan
foreach ($row in $statusRows) { $p = $row -split '\|'; Write-Host ("  {0,-22} {1}" -f $p[0], $p[1]) }

$ok = -not ($checks | Where-Object { -not $_.ok })
$outObj = [pscustomobject]@{
    ok = $ok
    run_id = $runId
    task_id = $task.id
    attempt_id = $(if ($attempt) { $attempt.id } else { '' })
    final_status = $run.status
    elapsed_seconds = $terminalElapsed
    assert_run_seconds = $AssertRunSeconds
    loop_limit_seconds = $LoopLimitSeconds
    attempt_span_seconds = $(if ($attempt -and $attempt.started_at -and $attempt.finished_at) { [math]::Round(([datetime]$attempt.finished_at - [datetime]$attempt.started_at).TotalSeconds, 2) } else { $null })
    status_trace = $trace
    checks = $checks
    runs_status_counts = @($statusRows | ForEach-Object { $p = $_ -split '\|'; [pscustomobject]@{ status = $p[0]; count = [int]$p[1] } })
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$outObj | ConvertTo-Json -Depth 6 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
