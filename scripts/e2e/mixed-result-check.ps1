# 一成一败混合结果（票 #15 / PR #18）
# 一个 Run 里两个 test：
#   mixed-ok   -> Authorization: Bearer ok   -> router -> mock-llm（成功）
#   mixed-fail -> Authorization: Bearer fail -> router -> mock-llm-500（失败）
# 平台不允许 tests[].args 覆盖 --url，所以用 test 级 env ENDPOINT_API_KEY + router 分流；
# 这也顺带验证了 Task 级 env 注入。
# 期望：Run failed；A succeeded / B failed；
#       report.counts={total:2,succeeded:1,failed:1}；failures 非空；
#       Run.error 取首个失败 Task（无 precheck 时即 B）。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8096',
    [string]$User = 'dev',
    [string]$Namespace = 'default',
    [string]$OutDir = '',
    [switch]$SkipApply,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-mixed' }
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

function New-Manifest {
    param([string]$Stamp)
    return @"
{"run":{"name":"mixed-$Stamp","service":{"url":"http://mixed-router.$Namespace.svc:18080/v1","model_name":"mock-llm"},"precheck":"off","cleanup":{"policy":"never"},"tests":[{"name":"mixed-ok","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=10 --retry_interval_seconds=1","env":{"ENDPOINT_API_KEY":"ok"}},{"name":"mixed-fail","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=10 --retry_interval_seconds=1","env":{"ENDPOINT_API_KEY":"fail"}}]}}
"@
}

if ($DryRun) {
    Write-Host "mixed-result-check dry-run（不连后端）" -ForegroundColor Cyan
    Write-Host "  base_url=$BaseUrl router=http://mixed-router.$Namespace.svc:18080/v1"
    Write-Host "  A=Bearer ok -> mock-llm；B=Bearer fail -> mock-llm-500"
    Write-Host (New-Manifest -Stamp (Get-Date -Format 'yyyyMMdd-HHmmss'))
    Write-Host "dry-run OK" -ForegroundColor Green
    exit 0
}

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host "一成一败混合结果：$BaseUrl" -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

if (-not $SkipApply) {
    Write-Host "确保 mixed-router / mock-llm-500 就绪…"
    kubectl apply -f (Join-Path $repo 'scripts\e2e\kind\mixed-router.yaml') 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "kubectl apply mixed-router 失败" }
    kubectl -n $Namespace rollout status deploy/mixed-router --timeout=90s 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "mixed-router rollout 失败" }
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$submitted = Invoke-Api -Method POST -Path '/api/v1/tasks' -Body (New-Manifest -Stamp $stamp)
$runId = [string]$submitted.task.id
Write-Host "已提交 Run $runId（mixed-ok / mixed-fail）"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt 120) {
    $run = Invoke-Api -Method GET -Path "/api/v1/tasks/$runId"
    $brief = ($run.runners | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N1}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $brief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('succeeded', 'failed', 'service_unavailable', 'canceled')) { break }
    Start-Sleep -Milliseconds 500
}
$sw.Stop()
$elapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan
Add-Check 'run.terminal_failed' ($run.status -eq 'failed') ("status=" + $run.status + "，耗时 " + $elapsed + "s")
Add-Check 'run.within_120s' ($elapsed -le 120) ($elapsed.ToString() + "s <= 120s")

$tasks = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/runners?limit=200").items
$a = @($tasks | Where-Object { $_.name -eq 'mixed-ok' })[0]
$b = @($tasks | Where-Object { $_.name -eq 'mixed-fail' })[0]
Add-Check 'tasks.present' ($null -ne $a -and $null -ne $b) ("tasks=" + $tasks.Count)
if ($a) { Add-Check 'task.mixed_ok_succeeded' ($a.status -eq 'succeeded') ("status=" + $a.status) }
if ($b) { Add-Check 'task.mixed_fail_failed' ($b.status -eq 'failed') ("status=" + $b.status + "/code=" + $b.error.code + "/retryable=" + $b.error.retryable) }

# 报告：counts 与 failures（异步生成，最多等 30s）
$report = $null
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Date) -lt $deadline) {
    try {
        $report = Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/report"
        break
    } catch {
        Start-Sleep -Milliseconds 500
    }
}
Add-Check 'report.present' ($null -ne $report) $(if ($report) { "run_id=" + $report.run_id } else { "30s 内未生成" })
if ($report) {
    $c = $report.counts
    Add-Check 'report.counts' ([int]$c.total -eq 2 -and [int]$c.succeeded -eq 1 -and [int]$c.failed -eq 1) ("total={0} succeeded={1} failed={2}" -f $c.total, $c.succeeded, $c.failed)
    Add-Check 'report.failures_nonempty' (@($report.failures).Count -gt 0) ("failures=" + @($report.failures).Count)
    $first = @($report.failures)[0]
    if ($first) {
        Add-Check 'report.failure_is_mixed_fail' ([string]$first.name -eq 'mixed-fail') ("first failure name=" + $first.name + " code=" + $first.error_code)
        $runErr = $run.error
        Add-Check 'run.error_first_failed_task' ($runErr.code -eq $first.error_code) ("run.error.code=" + $runErr.code + " / report first=" + $first.error_code)
        if ($b) { Add-Check 'run.error_matches_task_b' ($runErr.code -eq $b.error.code) ("run.error.code=" + $runErr.code + " / task B=" + $b.error.code) }
    }
}

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok
    run_id = $runId
    final_status = $run.status
    elapsed_seconds = $elapsed
    tasks = @($tasks | ForEach-Object { [pscustomobject]@{ name = $_.name; status = $_.status; code = $_.error.code } })
    counts = $(if ($report) { $report.counts } else { $null })
    failures = $(if ($report) { $report.failures } else { $null })
    run_error = $run.error
    checks = $checks
    status_trace = $trace
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$out | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
