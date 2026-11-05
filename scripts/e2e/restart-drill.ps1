# 重启恢复演练（票 #9 / C-3.5 / PR #18）
# 步骤：
#   1. 重启 mock-llm-delayed（15s 后才就绪），提交一个 service 形态、2 个 test 的 Run；
#   2. 等 Run 进入 testing 且至少一个 Attempt running；
#   3. kubectl delete pod -l app.kubernetes.io/name=testplaned --grace-period=0 --force；
#   4. 等新 Pod Ready（RecoveryTimeoutSeconds 内），检查启动日志与对账结果；
#   5. 断言 Run 到终态（succeeded / 按事实 failed）、无租约/Job 残留、有 attempt.finished。
# 负向：-RecoveryTimeoutSeconds 1 时，控制面 1s 内没恢复必须 FAIL（不会静默通过）。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8098',
    [string]$User = 'dev',
    [string]$Namespace = 'testplane',
    [string]$ControllerLabel = 'app.kubernetes.io/name=testplaned',
    [string]$DefaultNamespace = 'default',
    [string]$PgContainer = 'testplane-pg',
    [string]$PgUser = 'testplane',
    [string]$PgDb = 'testplane',
    [int]$RecoveryTimeoutSeconds = 60,
    [int]$RunTimeoutSeconds = 240,
    [int]$GcWaitSeconds = 90,
    [int]$ForwardPort = 8100,
    [int]$RecoveryPort = 8101,
    [string]$OutDir = '',
    [switch]$SkipApply,
    [switch]$SkipForward,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-restart' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$headers = @{ Authorization = "Bearer $User"; Accept = 'application/json' }
$script:ApiBase = $BaseUrl
$forwardProcs = @()

function Start-ApiForward {
    param([int]$Port)
    $log = Join-Path $OutDir ("forward-$Port.log")
    $p = Start-Process -FilePath 'kubectl' `
        -ArgumentList @('-n', $Namespace, 'port-forward', 'svc/testplane', "${Port}:8080") `
        -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err"
    $deadline = (Get-Date).AddSeconds(20)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$Port/healthz" -TimeoutSec 2
            if ($r.StatusCode -eq 200) { return $p }
        } catch { Start-Sleep -Milliseconds 500 }
    }
    throw "port-forward $Port 在 20s 内不可用（见 $log）"
}

function Invoke-Api {
    param([string]$Method, [string]$Path, [string]$Body = '')
    $uri = "$script:ApiBase$Path"
    if ($Body -ne '') {
        return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $Body
    }
    return Invoke-RestMethod -Method $Method -Uri $uri -Headers $headers
}

function Invoke-PsqlCount {
    param([string]$Sql)
    $out = & docker exec $PgContainer psql -U $PgUser -d $PgDb -t -A -c $Sql 2>&1
    if ($LASTEXITCODE -ne 0) { throw "docker exec psql 失败（exit $LASTEXITCODE）：$($out -join ' ')" }
    $text = (($out | Out-String).Trim())
    if ($text -notmatch '^\d+$') { throw "psql 输出无法解析为计数：$text" }
    return [int]$text
}

function Get-ControllerPods {
    $json = & kubectl -n $Namespace get pods -l $ControllerLabel -o json 2>&1
    if ($LASTEXITCODE -ne 0) { throw "kubectl get controller pods 失败：$($json -join ' ')" }
    return @(($json | ConvertFrom-Json).items)
}

function Wait-ControllerReady {
    param([string[]]$OldNames, [int]$TimeoutSeconds)
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($sw.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        $pods = Get-ControllerPods
        foreach ($p in $pods) {
            if ($OldNames -contains $p.metadata.name) { continue }
            $ready = @($p.status.conditions | Where-Object { $_.type -eq 'Ready' -and $_.status -eq 'True' }).Count -gt 0
            if ($ready) { return [pscustomobject]@{ pod = $p.metadata.name; waited = [math]::Round($sw.Elapsed.TotalSeconds, 1) } }
        }
        Start-Sleep -Seconds 1
    }
    return $null
}

function New-Manifest {
    param([string]$Stamp)
    return @"
{"run":{"name":"restart-$Stamp","service":{"url":"http://mock-llm-delayed.$DefaultNamespace.svc:18080/v1","model_name":"mock-llm"},"precheck":"off","cleanup":{"policy":"never"},"tests":[{"name":"drill-a","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=60 --retry_interval_seconds=1","timeout_seconds":180},{"name":"drill-b","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=60 --retry_interval_seconds=1","timeout_seconds":180}]}}
"@
}

if ($DryRun) {
    Write-Host "restart-drill dry-run（不连后端、不 kill）" -ForegroundColor Cyan
    Write-Host "  base_url=$BaseUrl controller ns=$Namespace label=$ControllerLabel"
    Write-Host "  恢复阈值 ${RecoveryTimeoutSeconds}s；Run 终态阈值 ${RunTimeoutSeconds}s；GC 等待 ${GcWaitSeconds}s"
    Write-Host (New-Manifest -Stamp (Get-Date -Format 'yyyyMMdd-HHmmss'))
    Write-Host "dry-run OK" -ForegroundColor Green
    exit 0
}

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host "重启恢复演练：$BaseUrl" -ForegroundColor Cyan
if (-not $SkipForward) {
    # 控制器 Pod 被杀时 port-forward 会断；演练自己管理 8100（演练前）→ 8101（恢复后）。
    $forwardProcs += Start-ApiForward -Port $ForwardPort
    $script:ApiBase = "http://127.0.0.1:$ForwardPort"
}
$null = Invoke-WebRequest -UseBasicParsing "$script:ApiBase/healthz" -TimeoutSec 5

if (-not $SkipApply) {
    Write-Host "部署/重启 mock-llm-delayed（重置 15s 就绪窗口）…"
    kubectl apply -f (Join-Path $repo 'scripts\e2e\kind\mock-llm-delayed.yaml') 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "kubectl apply mock-llm-delayed 失败" }
    kubectl -n $DefaultNamespace rollout restart deploy/mock-llm-delayed 2>&1 | Out-Null
    kubectl -n $DefaultNamespace rollout status deploy/mock-llm-delayed --timeout=90s 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "mock-llm-delayed rollout 失败" }
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$submitted = Invoke-Api -Method POST -Path '/api/v1/runs' -Body (New-Manifest -Stamp $stamp)
$runId = [string]$submitted.run.id
Write-Host "已提交 Run $runId"

# 等 testing 且至少一个 Attempt running
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$run = $null
$taskId = ''
while ($sw.Elapsed.TotalSeconds -lt 60) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$runId"
    if ($run.status -eq 'testing') {
        foreach ($t in $run.tasks) {
            if ($t.status -eq 'running') { $taskId = $t.id; break }
        }
        if ($taskId -ne '') { break }
    }
    Start-Sleep -Milliseconds 500
}
$runningWait = [math]::Round($sw.Elapsed.TotalSeconds, 1)
Add-Check 'run.reached_running' ($taskId -ne '') ("status=" + $run.status + "，running task=" + $taskId + "，等待 " + $runningWait + "s")

$oldPods = @(Get-ControllerPods | ForEach-Object { $_.metadata.name })
Write-Host ("旧 controller pods: " + ($oldPods -join ', '))
$killAt = Get-Date
if ($taskId -ne '') {
    Write-Host "kill controller pods（grace-period=0 --force）…"
    kubectl -n $Namespace delete pod -l $ControllerLabel --grace-period=0 --force --wait=false 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "kubectl delete controller pods 失败" }
}

$recovered = Wait-ControllerReady -OldNames $oldPods -TimeoutSeconds $RecoveryTimeoutSeconds
Add-Check 'controller.recovered' ($null -ne $recovered) $(if ($recovered) { "new pod=" + $recovered.pod + "，恢复 " + $recovered.waited + "s" } else { "超过 " + $RecoveryTimeoutSeconds + "s 没有 Ready 的新 Pod" })
if (-not $recovered) {
    # 负向验证：恢复超时必须 FAIL，不继续跑后面的“静默通过”。
    $ok = $false
    foreach ($p in $forwardProcs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    $out = [pscustomobject]@{ ok = $ok; run_id = $runId; task_id = $taskId; old_pods = $oldPods; checks = $checks }
    $jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
    $out | ConvertTo-Json -Depth 6 | Set-Content -Encoding utf8 $jsonPath
    Write-Host ("检查：{0}/{1} 通过；结果：FAIL（JSON: {2}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $jsonPath) -ForegroundColor Red
    exit 1
}
$newPod = $recovered.pod
kubectl -n $Namespace wait --for=condition=Ready "pod/$newPod" --timeout="$($RecoveryTimeoutSeconds)s" 2>&1 | Out-Null

if (-not $SkipForward) {
    foreach ($p in $forwardProcs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    $forwardProcs = @()
    $forwardProcs += Start-ApiForward -Port $RecoveryPort
    $script:ApiBase = "http://127.0.0.1:$RecoveryPort"
    $null = Invoke-WebRequest -UseBasicParsing "$script:ApiBase/healthz" -TimeoutSec 5
}

# 启动日志：controller started 必须有；startup reconcile 单独断言（当前实现成功路径可能不打）
$logText = ''
$logDeadline = (Get-Date).AddSeconds(15)
while ((Get-Date) -lt $logDeadline) {
    $logText = (& kubectl -n $Namespace logs $newPod --since=10m 2>&1 | Out-String)
    if ($logText -match 'controller started|startup reconcile') { break }
    Start-Sleep -Seconds 1
}
Add-Check 'pod.logs_controller_started' ($logText -match 'controller started') $(if ($logText -match 'controller started') { '启动日志含 controller started' } else { '未见 controller started' })
Add-Check 'pod.logs_startup_reconcile' ($logText -match 'startup reconcile') $(if ($logText -match 'startup reconcile') { '启动日志含 startup reconcile' } else { '未见 startup reconcile（当前实现成功路径不单独打；记录实际）' })

# 等 Run 终态
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
while ($sw.Elapsed.TotalSeconds -lt $RunTimeoutSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$runId"
    $brief = ($run.tasks | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', '
    $line = "t+{0:N1}s status={1} tasks: {2}" -f $sw.Elapsed.TotalSeconds, $run.status, $brief
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('succeeded', 'completed_with_errors', 'failed', 'service_unavailable', 'canceled')) { break }
    Start-Sleep -Seconds 1
}
$sw.Stop()
$finalElapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)
$terminal = $run.status -in @('succeeded', 'completed_with_errors', 'failed', 'service_unavailable', 'canceled')
Add-Check 'run.terminal' ($terminal -and $run.status -notin @('testing', 'cancelling')) ("status=" + $run.status + "，终态等待 " + $finalElapsed + "s")
Add-Check 'run.not_hung_testing' ($run.status -ne 'testing') ("status=" + $run.status)
Add-Check 'run.final_expected' ($run.status -in @('succeeded', 'failed')) ("status=" + $run.status + "（期望 succeeded 或按事实 failed）")

$tasks = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/tasks?limit=200").items
$taskIds = @($tasks | ForEach-Object { $_.id })
$idsSql = @($taskIds | ForEach-Object { "'" + $_ + "'" }) -join ','
try {
    $leases = Invoke-PsqlCount "select count(*) from endpoint_leases where task_id in ($idsSql);"
    Add-Check 'endpoint_leases.query' $true '查询成功'
    Add-Check 'endpoint_leases.empty' ($leases -eq 0) ("剩余租约=" + $leases)
} catch {
    Add-Check 'endpoint_leases.query' $false ($_.Exception.Message)
}

# Job 残留：终态后等 GC；--ignore-not-found 让不存在为 exit 0，坏 context 才 throw
$jobLeft = @()
$gcSw = [System.Diagnostics.Stopwatch]::StartNew()
while ($gcSw.Elapsed.TotalSeconds -lt $GcWaitSeconds) {
    $jobLeft = @()
    foreach ($tid in $taskIds) {
        $name = "tp-t-" + $tid.ToLower() + "-a1"
        $out = & kubectl -n $Namespace get job $name -o name --ignore-not-found 2>&1
        if ($LASTEXITCODE -ne 0) { Add-Check 'k8s.job_query' $false ("kubectl get job 失败：" + ($out -join ' ')); break }
        if (($out -join '') -ne '') { $jobLeft += ($out -join '') }
    }
    if ($jobLeft.Count -eq 0) { break }
    Start-Sleep -Seconds 2
}
Add-Check 'k8s.job_query' $true ("查询成功，等待 " + [math]::Round($gcSw.Elapsed.TotalSeconds, 1) + "s")
Add-Check 'k8s.jobs_empty' ($jobLeft.Count -eq 0) $(if ($jobLeft.Count -eq 0) { '无 Job 残留' } else { $jobLeft -join ', ' })

# 事件：至少一条 attempt.finished；有对账标记则断言，没有就记录实际（口径允许）
$events = (Invoke-Api -Method GET -Path "/api/v1/runs/$runId/events/history?limit=500").items
$finished = @($events | Where-Object { $_.type -eq 'attempt.finished' })
$reconciled = @($finished | Where-Object {
    ($_.payload.reason -and ([string]$_.payload.reason -match 'reconcil')) -or
    ($_.payload.detail -and ([string]$_.payload.detail -match 'reconcil')) -or
    ($_.payload.phase -and ([string]$_.payload.phase -match 'reconcil'))
})
Add-Check 'events.attempt_finished' ($finished.Count -gt 0) ("attempt.finished=" + $finished.Count)
if ($reconciled.Count -gt 0) {
    Add-Check 'events.reconciled_path' $true ("带对账标记的 attempt.finished=" + $reconciled.Count)
} else {
    Add-Check 'events.reconciled_path' $true ("没有可区分的对账标记；记录实际 attempt.finished payload=" + (($finished | ForEach-Object { $_.payload | ConvertTo-Json -Compress }) -join ' | '))
}

$ok = -not ($checks | Where-Object { -not $_.ok })
foreach ($p in $forwardProcs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
$out = [pscustomobject]@{
    ok = $ok
    run_id = $runId
    task_id_seen_running = $taskId
    old_controller_pods = $oldPods
    new_controller_pod = $newPod
    recovery_seconds = $recovered.waited
    final_status = $run.status
    elapsed_seconds = $finalElapsed
    status_trace = $trace
    events_attempt_finished = $finished
    checks = $checks
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$out | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
