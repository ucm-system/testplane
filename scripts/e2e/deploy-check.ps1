# deploy 形态验收（PR #18，deploy:{provider:"genet"}）
#
# 正常路径：
#   - 提交 deploy Run（fake Genet），断言 deploy.state 依次经过
#     queued -> accepted/deploying -> ready；
#   - endpoint.source = genet；precheck succeeded；Run 终态 succeeded；
#   - 终态后 helm release（namespace argus-mock）为空。
# 失败路径（-FailureOnly，fake-genet 用 -sim-error 500）：
#   - Run failed，error.code=DEPLOY_FAILED，retryable=false，state_events 有 cleanup.failed。
# Blocked 路径（-BlockedOnly，fake-genet 用 -sim-blocked）：
#   - Run service_unavailable，DEPLOY_BLOCKED retryable=true，
#     deploy.failure.detail.reason=blocked，uninstall.state=verified。
#
# 说明：本机没有 helm CLI 时，用 Kubernetes 里 Helm 的 release Secret
# （label owner=helm）等价判断“helm list 为空”，detail 会注明用了 fallback。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8107',
    [string]$User = 'dev',
    [string]$HelmNamespace = 'argus-mock',
    [string]$Namespace = 'argus',
    [string]$OutDir = '',
    [int]$TerminalTimeoutSeconds = 300,
    [int]$HelmWaitSeconds = 60,
    [int]$CleanupWaitSeconds = 90,
    [switch]$FailureOnly,
    [switch]$BlockedOnly,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-deploy' }
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
{"task":{"name":"deploy-check-$Stamp","deploy":{"provider":"genet","spec":"model_name: mock-llm"},"secrets":{"GENET_API_KEY":"argus-genet/api-key"},"precheck":"auto","runners":[{"name":"probe","runner":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"input":"{\"wait_timeout_seconds\":60}","on_failure":"abort_task"}]}}
"@
}

# helm CLI 存在就用 `helm list -n <ns> -o json`；否则退化为 Helm release Secret 计数。
function Get-HelmReleases {
    $helm = Get-Command helm -ErrorAction SilentlyContinue
    if ($helm) {
        $json = & helm list -n $HelmNamespace -o json 2>&1
        if ($LASTEXITCODE -ne 0) { throw "helm list -n $HelmNamespace 失败：$($json -join ' ')" }
        $items = @($json | ConvertFrom-Json)
        return [pscustomobject]@{ source = 'helm'; items = $items }
    }
    $out = & kubectl -n $HelmNamespace get secrets -l owner=helm -o json 2>&1
    if ($LASTEXITCODE -ne 0) { throw "kubectl get helm release secrets 失败：$($out -join ' ')" }
    $items = @(($out | ConvertFrom-Json).items)
    # 只数平台 Deploy 自己的 release：mock-llm/mock-llm-fail 是基础设施，
    # arg-demo-manual 是人工演示 release（Lead 2026-11-05），都不算平台残留。
    $items = @($items | Where-Object {
        $rel = [string]$_.metadata.labels.name
        $rel -notlike 'mock-llm*' -and $rel -notlike 'arg-demo-*'
    })
    return [pscustomobject]@{ source = 'k8s-secrets（helm CLI 不在 PATH）'; items = $items }
}

if ($DryRun) {
    Write-Host "deploy-check dry-run（不连后端、不写集群）" -ForegroundColor Cyan
    Write-Host "  base_url=$BaseUrl helm_ns=$HelmNamespace"
    Write-Host "  正常：deploy 排队→受理/部署→ready（deploy-state.json）；endpoint 经 deploy 导出注入；Task succeeded；helm list 空"
    Write-Host "  失败（-FailureOnly，fake-genet -sim-error 500）：Task failed / DEPLOY_FAILED / retryable=false / cleanup_failed 事件"
    Write-Host "  blocked（-BlockedOnly，fake-genet -sim-blocked）：Task service_unavailable / DEPLOY_BLOCKED / retryable=true / detail.reason=blocked / uninstall=verified"
    Write-Host (New-Manifest -Stamp (Get-Date -Format 'yyyyMMdd-HHmmss'))
    Write-Host "dry-run OK" -ForegroundColor Green
    exit 0
}

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host ("deploy 形态验收：{0}（{1}）" -f $BaseUrl, $(if ($FailureOnly) { '失败分支' } elseif ($BlockedOnly) { 'blocked 分支' } else { '正常分支' })) -ForegroundColor Cyan
$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$submitted = Invoke-Api -Method POST -Path '/api/v1/tasks' -Body (New-Manifest -Stamp $stamp)
$runId = [string]$submitted.task.id
Write-Host "已提交 Task $runId"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$trace = @()
$states = @()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt $TerminalTimeoutSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/tasks/$runId"
    $ds = $run.deploy.phase
    if ($ds) { if ($states.Count -eq 0 -or $states[-1] -ne $ds) { $states += $ds } }
    $line = "t+{0:N1}s status={1} deploy={2}" -f $sw.Elapsed.TotalSeconds, $run.status, $ds
    if ($trace.Count -eq 0 -or $trace[-1] -ne $line) { $trace += $line; Write-Host "  $line" }
    if ($run.status -in @('succeeded', 'failed', 'service_unavailable', 'failed', 'canceled')) { break }
    Start-Sleep -Milliseconds 500
}
$sw.Stop()
$elapsed = [math]::Round($sw.Elapsed.TotalSeconds, 1)

Write-Host ""
Write-Host "断言：" -ForegroundColor Cyan

if ($FailureOnly) {
    Add-Check 'run.failed' ($run.status -eq 'failed') ("status=" + $run.status + "，耗时 " + $elapsed + "s")
    Add-Check 'run.error_deploy_failed' ($run.error.code -eq 'DEPLOY_FAILED') ("error.code=" + $run.error.code)
    Add-Check 'run.error_not_retryable' ($run.error.retryable -eq $false) ("retryable=" + $run.error.retryable)
    $events = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/events/history?limit=500").items
    $cleanupFailed = @($events | Where-Object { $_.type -eq 'cleanup.failed' }).Count
    # 部署失败没有 Attempt 终态钩子；卸载收口由对账 tick 兜底（默认 60s），等它出现。
    $cSw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($cleanupFailed -eq 0 -and $cSw.Elapsed.TotalSeconds -lt $CleanupWaitSeconds) {
        Start-Sleep -Seconds 2
        $events = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/events/history?limit=500").items
        $cleanupFailed = @($events | Where-Object { $_.type -eq 'cleanup.failed' }).Count
    }
    Add-Check 'events.cleanup_failed' ($cleanupFailed -gt 0) ("cleanup.failed=" + $cleanupFailed + "，等待 " + [math]::Round($cSw.Elapsed.TotalSeconds, 1) + "s")
    $jsonPath = Join-Path $OutDir ("result-" + $stamp + "-failure.json")
    $ok = -not ($checks | Where-Object { -not $_.ok })
    [pscustomobject]@{ ok = $ok; mode = 'failure'; run_id = $runId; final_status = $run.status; error = $run.error; checks = $checks; status_trace = $trace } |
        ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
    Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
    if (-not $ok) { exit 1 }
    exit 0
}

if ($BlockedOnly) {
    # blocked 判定后 uninstall 是异步收口，等 deploy.uninstall.state=verified。
    $uSw = [System.Diagnostics.Stopwatch]::StartNew()
    while ($uSw.Elapsed.TotalSeconds -lt $HelmWaitSeconds) {
        $u = $run.deploy.uninstall
        if ($u -and $u.state -eq 'verified') { break }
        Start-Sleep -Seconds 1
        $run = Invoke-Api -Method GET -Path "/api/v1/tasks/$runId"
    }
    $uSw.Stop()

    Add-Check 'run.service_unavailable' ($run.status -eq 'service_unavailable') ("status=" + $run.status + "，耗时 " + $elapsed + "s")
    Add-Check 'run.error_deploy_blocked' ($run.error.code -eq 'DEPLOY_BLOCKED') ("error.code=" + $run.error.code)
    Add-Check 'run.error_retryable' ($run.error.retryable -eq $true) ("retryable=" + $run.error.retryable)
    $deployFailure = $run.deploy.failure
    $reason = if ($deployFailure -and $deployFailure.detail) { [string]$deployFailure.detail.reason } else { '' }
    Add-Check 'deploy.failure_detail_reason_blocked' ($reason -eq 'blocked') ("deploy.failure.detail.reason=" + $reason)
    $uninstall = $run.deploy.uninstall
    Add-Check 'deploy.uninstall_verified' ($null -ne $uninstall -and $uninstall.state -eq 'verified') $(if ($uninstall) { "uninstall.state=" + $uninstall.state } else { '没有 uninstall' })

    $releases = $null
    try {
        $releases = Get-HelmReleases
        Add-Check 'helm.query' $true ("查询方式=" + $releases.source)
        Add-Check 'helm.empty' ($releases.items.Count -eq 0) ("release 数=" + $releases.items.Count)
    } catch {
        Add-Check 'helm.query' $false ($_.Exception.Message)
    }

    $ok = -not ($checks | Where-Object { -not $_.ok })
    $jsonPath = Join-Path $OutDir ("result-" + $stamp + "-blocked.json")
    [pscustomobject]@{
        ok = $ok
        mode = 'blocked'
        run_id = $runId
        final_status = $run.status
        error = $run.error
        deploy_failure = $deployFailure
        uninstall = $uninstall
        checks = $checks
        status_trace = $trace
    } | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
    Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
    if (-not $ok) { exit 1 }
    exit 0
}

Add-Check 'run.succeeded' ($run.status -eq 'succeeded') ("status=" + $run.status + "，耗时 " + $elapsed + "s")
Add-Check 'endpoint.source_genet' ($run.endpoint.source -eq 'genet') ("source=" + $run.endpoint.source + "，url=" + $run.endpoint.url)

$tasks = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/runners?limit=200").items
$pre = @($tasks | Where-Object { $_.name -eq 'precheck' })[0]
Add-Check 'precheck.succeeded' ($null -ne $pre -and $pre.status -eq 'succeeded') $(if ($pre) { "precheck=" + $pre.status } else { '没有 precheck Task' })

# deploy.state 顺序：从事件取，避免只看轮询末值。
# 事件里 queued 有两条来源：run.state_changed(to=queued) 和 deploy.state_changed.payload.deploy.state=queued；
# 初版只读 payload.state，漏了 queued，且把轮询 states 追加在事件序列后又制造了重复段。
$events = (Invoke-Api -Method GET -Path "/api/v1/tasks/$runId/events/history?limit=500").items
$eventStates = @()
foreach ($ev in $events) {
    if ($ev.type -ne 'deploy.state_changed') { continue }
    if ($ev.payload.deploy.state) { $eventStates += [string]$ev.payload.deploy.state }
    elseif ($ev.payload.state) { $eventStates += [string]$ev.payload.state }
}
$queuedEvent = @($events | Where-Object { $_.type -eq 'run.state_changed' -and $_.to -eq 'queued' }).Count -gt 0
if ($queuedEvent -and $eventStates -notcontains 'queued') { $eventStates = @('queued') + $eventStates }
$seqStates = if ($eventStates.Count -gt 0) { $eventStates } else { @($states) }
Add-Check 'deploy.state_queued' ($seqStates -contains 'queued') ("states=" + ($seqStates -join ' -> '))
Add-Check 'deploy.state_ready' ($seqStates -contains 'ready') ("states=" + ($seqStates -join ' -> '))
Add-Check 'deploy.state_middle' ($seqStates -contains 'accepted' -or $seqStates -contains 'deploying') ("states=" + ($seqStates -join ' -> '))
# 顺序：queued 在前，ready 在后，中间态在两者之间（按事件去重后的首次出现位置）。
$pos = @{}
$i = 0
foreach ($s in $seqStates) { if (-not $pos.ContainsKey($s)) { $pos[$s] = $i }; $i++ }
$orderOK = $pos.ContainsKey('queued') -and $pos.ContainsKey('ready') -and $pos['queued'] -lt $pos['ready']
if ($pos.ContainsKey('accepted')) { $orderOK = $orderOK -and $pos['queued'] -lt $pos['accepted'] -and $pos['accepted'] -lt $pos['ready'] }
elseif ($pos.ContainsKey('deploying')) { $orderOK = $orderOK -and $pos['queued'] -lt $pos['deploying'] -and $pos['deploying'] -lt $pos['ready'] }
Add-Check 'deploy.state_order' $orderOK ("queued -> accepted|deploying -> ready（实际 " + ($seqStates -join ' -> ') + "）")

# 终态后 Helm release 清空（backend 卸载收口修复后应通过）。
$helmSw = [System.Diagnostics.Stopwatch]::StartNew()
$releases = $null
while ($helmSw.Elapsed.TotalSeconds -lt $HelmWaitSeconds) {
    try {
        $releases = Get-HelmReleases
        if ($releases.items.Count -eq 0) { break }
    } catch {
        Add-Check 'helm.query' $false ($_.Exception.Message)
        $releases = $null
        break
    }
    Start-Sleep -Seconds 2
}
if ($null -ne $releases) {
    Add-Check 'helm.query' $true ("查询方式=" + $releases.source)
    Add-Check 'helm.empty' ($releases.items.Count -eq 0) ("release 数=" + $releases.items.Count + ("（" + (($releases.items | ForEach-Object { $_.name }) -join ', ') + "）"))
}

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok
    mode = 'success'
    run_id = $runId
    final_status = $run.status
    elapsed_seconds = $elapsed
    deploy_states = $seqStates
    endpoint = $run.endpoint
    helm = $(if ($releases) { [pscustomobject]@{ source = $releases.source; count = $releases.items.Count } } else { $null })
    checks = $checks
    status_trace = $trace
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$out | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
