# 超时路径验证（票 #3 收口用）
# 口径（Lead 2026-11-05 裁定）：
#   1) precheck:"off" + 单个 test Task 超时 → Run 必须 failed（TASK_TIMEOUT 是平台 C 类错误）；
#      真 Pre-Check 超时才按 C-5.8（retryable → service_unavailable）。当前实现按 seq==0 判 Pre-Check，是 bug。
#   2) Attempt 时长容差 = timeout + 30s（watchdog 10s 粒度 + 删 Job grace），不要求 ≈5s。
#   3) 事件接受 task.state_changed running→failed + payload.error.code=TASK_TIMEOUT，不要求 task.timeout 事件。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$Namespace = 'testplane',
    [string]$PgContainer = 'testplane-pg',
    [string]$PgUser = 'testplane',
    [string]$PgDb = 'testplane',
    [int]$TimeoutSeconds = 30,
    [int]$CleanupWaitSeconds = 60,
    [int]$TaskTimeoutSeconds = 5,
    [string]$OutDir = ''
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

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host "超时路径验证：$BaseUrl" -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$manifest = @"
{"run":{"name":"timeout-check-$stamp","service":{"url":"http://mock-llm-slow.default.svc:18080/v1","model_name":"mock-llm"},"precheck":"off","tests":[{"name":"slow-timeout","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=600","timeout_seconds":$TaskTimeoutSeconds}]}}
"@

$submitted = Invoke-Api -Method POST -Path '/api/v1/runs' -Body $manifest
$runId = [string]$submitted.run.id
Write-Host "已提交 Run $runId（timeout_seconds=$TaskTimeoutSeconds，mock-llm-slow）"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt ($TimeoutSeconds + 5)) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$runId"
    $brief = ($run.tasks | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N1}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $brief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('failed', 'completed_with_errors', 'service_unavailable', 'succeeded', 'canceled')) { break }
    Start-Sleep -Milliseconds 500
}
$sw.Stop()

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan
$terminalElapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)
Add-Check 'run.terminal_failed' ($run.status -eq 'failed') ("status=" + $run.status + "（期望 failed：TASK_TIMEOUT 属平台 C 类）")
Add-Check 'run.within_30s' ($terminalElapsed -le $TimeoutSeconds) ($terminalElapsed.ToString() + "s <= " + $TimeoutSeconds + "s")

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
    $span = ([datetime]$attempt.finished_at - [datetime]$attempt.started_at).TotalSeconds
    Add-Check 'attempt.span_le_timeout_plus_30s' ($span -le ($TaskTimeoutSeconds + 30)) ([math]::Round($span, 2).ToString() + "s <= " + ($TaskTimeoutSeconds + 30) + "s")
}

# 事件：task.state_changed running->failed + payload.error.code=TASK_TIMEOUT，或有 task.timeout
$events = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/events/history?limit=200").items
$timeoutEvent = @($events | Where-Object {
    $_.type -eq 'task.timeout' -or
    ($_.type -eq 'task.state_changed' -and $_.from -eq 'running' -and $_.to -eq 'failed' -and $_.payload.error.code -eq 'TASK_TIMEOUT')
})
Add-Check 'events.timeout_recorded' ($timeoutEvent.Count -gt 0) ("匹配事件 " + $timeoutEvent.Count + " 条（" + (($timeoutEvent | ForEach-Object { $_.type + '#' + $_.seq }) -join ', ') + "）")

# 清理：Job/Pod 删除 + 租约释放（终态后最多等 CleanupWaitSeconds）
$cleanupSw = [System.Diagnostics.Stopwatch]::StartNew()
$podsLeft = ''
$jobLeft = ''
while ($cleanupSw.Elapsed.TotalSeconds -lt $CleanupWaitSeconds) {
    $podsLeft = (kubectl -n $Namespace get pods -l "job-name=tp-t-$($task.id.ToLower())-a1" -o name 2>&1) -join ','
    if ($LASTEXITCODE -ne 0) { $podsLeft = '' }
    $jobLeft = (kubectl -n $Namespace get jobs "tp-t-$($task.id.ToLower())-a1" -o name 2>&1) -join ','
    if ($LASTEXITCODE -ne 0) { $jobLeft = '' }
    if ($podsLeft -eq '' -and $jobLeft -eq '') { break }
    Start-Sleep -Seconds 2
}
Add-Check 'k8s.pod_deleted' ($podsLeft -eq '') $(if ($podsLeft -eq '') { "已删除（等待 " + [math]::Round($cleanupSw.Elapsed.TotalSeconds, 1) + "s）" } else { "残留：" + $podsLeft })
Add-Check 'k8s.job_deleted' ($jobLeft -eq '') $(if ($jobLeft -eq '') { '已删除' } else { "残留：" + $jobLeft })

$lease = docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -c "select count(*) from endpoint_leases where task_id='$($task.id)';" 2>&1
$leaseCount = 0
[void][int]::TryParse(([string]($lease | Select-Object -First 1)).Trim(), [ref]$leaseCount)
Add-Check 'endpoint_leases.empty' ($leaseCount -eq 0) ("剩余租约=$leaseCount")

$statusRows = docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -F '|' -c "select status,count(*) from runs group by status order by 2 desc,1;" 2>&1
Write-Host ""
Write-Host "runs 状态分布：" -ForegroundColor Cyan
$statusRows | Where-Object { $_ -ne '' } | ForEach-Object { $p = $_ -split '\|'; Write-Host ("  {0,-22} {1}" -f $p[0], $p[1]) }

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok
    run_id = $runId
    task_id = $task.id
    attempt_id = $(if ($attempt) { $attempt.id } else { '' })
    final_status = $run.status
    elapsed_seconds = $terminalElapsed
    attempt_span_seconds = $(if ($attempt) { [math]::Round(([datetime]$attempt.finished_at - [datetime]$attempt.started_at).TotalSeconds, 2) } else { $null })
    status_trace = $trace
    checks = $checks
    runs_status_counts = @($statusRows | Where-Object { $_ -ne '' } | ForEach-Object { $p = $_ -split '\|'; [pscustomobject]@{ status = $p[0]; count = [int]$p[1] } })
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$out | ConvertTo-Json -Depth 6 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
