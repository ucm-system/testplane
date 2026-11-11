# 取消场景验证（票 #3 / #16 修复版）
#   POST /runs -> 立刻 POST /runs/{id}/cancel -> 轮询到 canceled
# 断言：Run canceled（≤60s，独立阈值）、Task 全 skipped(reason canceled|upstream_abort)、
#       Attempt（若有）canceled、endpoint_leases 无残留、无 Pod 残留。
# 票 #16：docker/kubectl 查询失败必须 fail-fast，不能把“查询失败”当成“无残留”；
#         轮询上界与断言阈值分离（上界 180s，断言 60s）。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$Namespace = 'argus',
    [string]$PgContainer = 'argus-pg',
    [string]$PgUser = 'argus',
    [string]$PgDb = 'argus',
    [int]$AssertCancelSeconds = 60,
    [int]$LoopLimitSeconds = 180,
    [string]$OutDir = '',
    [switch]$DryRun
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

# docker exec psql，非零退出或输出不是整数一律 throw（绝不把失败当 0）。
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

# kubectl get <kind>，非零退出一律 throw；成功时返回对象名数组。
function Get-K8sObjectNames {
    param([string]$Kind, [string]$Selector)
    $out = & kubectl -n $Namespace get $Kind -l $Selector -o name 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl get $Kind -l $Selector 失败（exit $LASTEXITCODE）：$($out -join ' ')"
    }
    return @($out | Where-Object { $_ -ne '' })
}

function New-Manifest {
    param([string]$Stamp)
    return @"
{"task":{"name":"qa-cancel-$Stamp","service":{"url":"http://mock-llm.default.svc:18080/v1","model_name":"mock-llm"},"precheck":"auto","runners":[{"name":"probe","runner":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"input":"{\"wait_timeout_seconds\":20}","on_failure":"continue"}]}}
"@
}

if ($DryRun) {
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    Write-Host "cancel-check dry-run（不连后端）" -ForegroundColor Cyan
    Write-Host "  base_url=$BaseUrl namespace=$Namespace"
    Write-Host "  断言：Task canceled ≤ ${AssertCancelSeconds}s；循环上界 ${LoopLimitSeconds}s；其余 runner 全 skipped；租约/Pod 0"
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

Write-Host "取消场景验证：$BaseUrl" -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$manifest = New-Manifest -Stamp $stamp
$submitted = Invoke-Api -Method POST -Path '/api/v1/tasks' -Body $manifest
$runId = [string]$submitted.task.id
Write-Host "已提交 Run $runId，立即取消…"
$null = Invoke-Api -Method POST -Path "/api/v1/tasks/$runId/cancel"

# 轮询上界与断言阈值分离：LoopLimitSeconds ≥ AssertCancelSeconds + 余量（票 #16）。
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt $LoopLimitSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/tasks/$runId"
    $taskBrief = ($run.runners | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N1}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $taskBrief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('canceled', 'failed', 'service_unavailable', 'succeeded')) { break }
    Start-Sleep -Seconds 1
}
$sw.Stop()
$elapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan
Add-Check 'run.terminal_canceled' ($run.status -eq 'canceled') ("status=" + $run.status + "，实际耗时 " + $elapsed + "s（循环上界 " + $LoopLimitSeconds + "s）")
Add-Check 'run.cancel_within_assert' ($elapsed -le $AssertCancelSeconds) ($elapsed.ToString() + "s <= " + $AssertCancelSeconds + "s（独立阈值，不复用循环上界）")

$taskList = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/runners?limit=200").items
Add-Check 'run.tasks_present' ($taskList.Count -gt 0) ("tasks=" + $taskList.Count)

$badTask = @()
$badAttempt = @()
$reasonBad = @()
foreach ($t in $taskList) {
    if ($t.status -ne 'skipped') { $badTask += ("$($t.name)=$($t.status)") }
    if ($t.status -eq 'skipped' -and $t.status_reason -notin @('canceled', 'upstream_abort')) {
        $reasonBad += ("$($t.name)=$($t.status_reason)")
    }
}
# v2：attempt 是 Task 级（D-68），一次查询而不是逐 runner。
$atts = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/attempts").items
foreach ($a in $atts) { if ($a.status -ne 'canceled') { $badAttempt += ("a$($a.attempt_no)=$($a.status)") } }

Add-Check 'run.tasks_all_skipped' ($badTask.Count -eq 0) $(if ($badTask.Count -eq 0) { 'all skipped' } else { $badTask -join ', ' })
Add-Check 'run.skip_reasons' ($reasonBad.Count -eq 0) $(if ($reasonBad.Count -eq 0) { 'canceled|upstream_abort' } else { $reasonBad -join ', ' })
Add-Check 'run.attempts_canceled' ($badAttempt.Count -eq 0) $(if ($badAttempt.Count -eq 0) { '无残留 running/failed Attempt' } else { $badAttempt -join ', ' })

# 租约：查询失败与“查询成功但非空”分开断言。
$ids = "'$runId'"   # v2：租约按 Task id（attempt/租约都是 Task 级）
try {
    $leaseCount = Invoke-PsqlCount "select count(*) from endpoint_leases where task_id in ($ids);"
    Add-Check 'endpoint_leases.query' $true ("查询成功，task 数=" + $taskList.Count)
    Add-Check 'endpoint_leases.empty' ($leaseCount -eq 0) ("剩余租约=$leaseCount")
} catch {
    Add-Check 'endpoint_leases.query' $false ($_.Exception.Message)
}

# Pod：逐个 Task 的 Job 查；查询失败 fail-fast，不做“空结果”推断。
$podQueryFailed = $false
$podLeft = @()
# v2：Job 打 argus.io/task=<task_id> 标签（附录 B），按标签查一次，不拼 Job 名。
try {
    $podLeft = Get-K8sObjectNames -Kind 'pods' -Selector "argus.io/task=$runId"
} catch {
    $podQueryFailed = $true
    Add-Check 'k8s.pods_query' $false ($_.Exception.Message)
}
if (-not $podQueryFailed) {
    Add-Check 'k8s.pods_query' $true '查询成功'
    Add-Check 'k8s.pods_empty' ($podLeft.Count -eq 0) $(if ($podLeft.Count -eq 0) { '无 Pod 残留' } else { $podLeft -join ', ' })
}

$statusRows = @()
try {
    $out = & docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -F '|' -c "select status,count(*) from tasks group by status order by 2 desc,1;" 2>&1
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
    base_url = $BaseUrl
    run_id = $runId
    final_status = $run.status
    elapsed_seconds = $elapsed
    assert_cancel_seconds = $AssertCancelSeconds
    loop_limit_seconds = $LoopLimitSeconds
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
