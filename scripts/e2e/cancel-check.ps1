# 取消场景验证（票 #3 收口用，严格断言）
#   POST /runs -> 立刻 POST /runs/{id}/cancel -> 轮询到 canceled
# 断言：Run canceled、Task 全 skipped(reason canceled|upstream_abort)、Attempt（若有）canceled、
#       endpoint_leases 无残留、无 Pod 残留，并输出 runs 状态分布。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$Namespace = 'testplane',
    [string]$PgContainer = 'testplane-pg',
    [string]$PgUser = 'testplane',
    [string]$PgDb = 'testplane',
    [int]$TimeoutSeconds = 60,
    [string]$OutDir = ''
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-cancel' }
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
    $color = if ($Ok) { 'Green' } else { 'Red' }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $color
}

Write-Host "取消场景验证：$BaseUrl" -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$manifest = @"
{"run":{"name":"qa-cancel-$stamp","service":{"url":"http://mock-llm.default.svc:18080/v1","model_name":"mock-llm"},"precheck":"auto","tests":[{"name":"probe","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=20"}]}}
"@

$submitted = Invoke-Api -Method POST -Path '/api/v1/runs' -Body $manifest
$runId = [string]$submitted.run.id
Write-Host "已提交 Run $runId，立即取消…"
$null = Invoke-Api -Method POST -Path "/api/v1/runs/$runId/cancel"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$runId"
    $taskBrief = ($run.tasks | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N0}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $taskBrief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('canceled', 'failed', 'service_unavailable', 'succeeded', 'completed_with_errors')) { break }
    Start-Sleep -Seconds 1
}
$sw.Stop()

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan
Add-Check 'run.terminal_canceled' ($run.status -eq 'canceled') ("status=" + $run.status + "，耗时 " + [math]::Round($sw.Elapsed.TotalSeconds, 1) + "s")
Add-Check 'run.cancel_within_60s' ($sw.Elapsed.TotalSeconds -le $TimeoutSeconds) ([math]::Round($sw.Elapsed.TotalSeconds, 1).ToString() + 's <= ' + $TimeoutSeconds + 's')

$taskList = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/tasks?limit=200").items
Add-Check 'run.tasks_present' ($taskList.Count -gt 0) ("tasks=" + $taskList.Count)

$badTask = @()
$badAttempt = @()
$reasonBad = @()
foreach ($t in $taskList) {
    if ($t.status -ne 'skipped') { $badTask += ("$($t.name)=$($t.status)") }
    if ($t.status -eq 'skipped' -and $t.status_reason -notin @('canceled', 'upstream_abort')) {
        $reasonBad += ("$($t.name)=$($t.status_reason)")
    }
    if ($t.attempts -gt 0) {
        $atts = (Invoke-Api -Method GET -Path "/api/v1/tasks/$($t.id)/attempts").items
        foreach ($a in $atts) { if ($a.status -ne 'canceled') { $badAttempt += ("$($t.name)/a$($a.idx)=$($a.status)") } }
    }
}
Add-Check 'run.tasks_all_skipped' ($badTask.Count -eq 0) $(if ($badTask.Count -eq 0) { 'all skipped' } else { $badTask -join ', ' })
Add-Check 'run.skip_reasons' ($reasonBad.Count -eq 0) $(if ($reasonBad.Count -eq 0) { 'canceled|upstream_abort' } else { $reasonBad -join ', ' })
Add-Check 'run.attempts_canceled' ($badAttempt.Count -eq 0) $(if ($badAttempt.Count -eq 0) { '无残留 running/failed Attempt' } else { $badAttempt -join ', ' })

$ids = @($taskList | ForEach-Object { "'" + $_.id + "'" }) -join ','
$leaseRows = docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -c "select count(*) from endpoint_leases where task_id in ($ids);" 2>&1
$leaseCount = 0
[void][int]::TryParse(([string]($leaseRows | Select-Object -First 1)).Trim(), [ref]$leaseCount)
Add-Check 'endpoint_leases.empty' ($leaseCount -eq 0) ("剩余租约=$leaseCount")

$podLeft = @()
foreach ($t in $taskList) {
    $job = "tp-t-" + $t.id.ToLower() + "-a1"
    $pods = kubectl -n $Namespace get pods -l "job-name=$job" -o name 2>&1
    if ($LASTEXITCODE -eq 0 -and $pods) { $podLeft += ($pods -join ',') }
}
Add-Check 'k8s.pods_empty' ($podLeft.Count -eq 0) $(if ($podLeft.Count -eq 0) { '无 Pod 残留' } else { $podLeft -join ', ' })

$dbCounts = kubectl -n $Namespace get pods > $null 2>&1; # 保持命令上下文
$statusRows = docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -F '|' -c "select status,count(*) from runs group by status order by 2 desc, 1;" 2>&1
Write-Host ""
Write-Host "runs 状态分布：" -ForegroundColor Cyan
$statusRows | Where-Object { $_ -ne '' } | ForEach-Object { $p = $_ -split '\|'; Write-Host ("  {0,-22} {1}" -f $p[0], $p[1]) }

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok
    base_url = $BaseUrl
    run_id = $runId
    final_status = $run.status
    elapsed_seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1)
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
