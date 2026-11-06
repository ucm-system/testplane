# 失败 Run → retry-plan（复用/重跑）→ retry 新 Run → 新 Run 成功、旧成功 Task 复用。
#
# 用一个临时 mock-llm（host :18181，ready-after=4s）制造“一个 Task 先失败、一个后成功”：
#   retry-a: wait 1s  → 首次失败（服务还没 ready）
#   retry-b: wait 20s → 首次成功（4s 后服务 ready）
# retry 后 mock 已 ready，retry-a 成功；retry-b 直接从旧 Run 复用。
#
# 前置：控制面可访问（默认 http://127.0.0.1:8093），dev 模式。
# 用法：pwsh scripts/e2e/retry-check.ps1 -BaseUrl http://127.0.0.1:8126
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$MockURL = 'http://host.docker.internal:18181/v1',
    [int]$MockPort = 18181,
    [string]$MockExe = '',
    [switch]$SkipMockStart
)
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$outDir = Join-Path $repo '.data\qa-retry'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}
$headers = @{ Authorization = "Bearer $User"; Accept = 'application/json' }
function Call {
    param([string]$Method, [string]$Path, $Body = $null)
    $uri = "$BaseUrl$Path"
    if ($null -ne $Body) {
        $json = $Body | ConvertTo-Json -Depth 8 -Compress
        return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $json
    }
    return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers
}
function JsonOf($Resp) { try { return $Resp.Content | ConvertFrom-Json } catch { return $null } }
function Wait-Run([string]$RunID, [int]$Seconds = 120) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    do {
        Start-Sleep -Milliseconds 800
        $r = JsonOf (Call GET "/api/v1/runs/$RunID")
    } while ($r.status -notin @('succeeded', 'failed', 'completed_with_errors', 'service_unavailable', 'canceled') -and (Get-Date) -lt $deadline)
    return $r
}

$mockProc = $null
if (-not $SkipMockStart) {
    if ($MockExe -eq '') { $MockExe = Join-Path $repo '.data\mock-llm.exe' }
    if (-not (Test-Path -LiteralPath $MockExe)) {
        Push-Location (Join-Path $repo 'tools\mock-llm')
        try { go build -o $MockExe .; if ($LASTEXITCODE -ne 0) { throw 'go build mock-llm 失败' } } finally { Pop-Location }
    }
    $log = Join-Path $outDir "mock-$stamp.log"
    $mockProc = Start-Process -FilePath $MockExe -ArgumentList @('-addr', ":$MockPort", '-ready-after', '4s', '-quiet') -PassThru -WindowStyle Hidden -RedirectStandardOutput $log -RedirectStandardError "$log.err"
    $deadline = (Get-Date).AddSeconds(15)
    do {
        Start-Sleep -Milliseconds 300
        try { $h = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$MockPort/healthz" -TimeoutSec 2; if ($h.StatusCode -eq 200) { break } } catch { }
    } while ((Get-Date) -lt $deadline)
    Add-Check 'mock.started' ($null -ne $mockProc -and $mockProc.Id -gt 0) ("pid=" + $mockProc.Id + " url=" + $MockURL)
}

$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5
Write-Host "失败续跑验收：$BaseUrl（mock=$MockURL）" -ForegroundColor Cyan

$manifest = @{
    run = @{
        name = "retry-$stamp"; service = @{ url = $MockURL; model_name = 'mock-llm' }; precheck = 'auto';
        cleanup = @{ policy = 'never' };
        tests = @(
            @{ name = 'retry-a'; tester = 'endpoint-probe'; version = '1.0.0'; execution = @{ profile = 'k8s-npu' }; args = '--wait_timeout_seconds=10 --retry_interval_seconds=1' }
        )
    }
} | ConvertTo-Json -Depth 8 -Compress
$r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/runs" -Headers $headers -ContentType 'application/json' -Body $manifest
$accepted = JsonOf $r
$runID = $accepted.run.id
Add-Check 'run.submit_202' ($r.StatusCode -eq 202 -and $runID) ("status=" + $r.StatusCode + " run=" + $runID)

# 等 Pre-Check 成功：此时 service 已 ready；立刻停 mock，让随后的测试 Task 失败。
$precheckOK = $false
$deadline = (Get-Date).AddSeconds(90)
do {
    Start-Sleep -Milliseconds 800
    $run = JsonOf (Call GET "/api/v1/runs/$runID")
    $pc = @($run.tasks | Where-Object { $_.seq -eq 0 })[0]
    if ($pc -and $pc.status -eq 'succeeded') { $precheckOK = $true; break }
} while ((Get-Date) -lt $deadline)
Add-Check 'old.precheck_succeeded' $precheckOK ("status=" + $pc.status)
if ($mockProc) { Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue }
Add-Check 'mock.stopped_after_precheck' ($mockProc -ne $null) ("pid=" + $mockProc.Id)
$run = Wait-Run $runID
Add-Check 'run.terminal_not_succeeded' ($run.status -in @('failed', 'completed_with_errors') -and $run.status -ne 'succeeded') ("status=" + $run.status)
$tasks = @($run.tasks)
$failed = @($tasks | Where-Object { $_.name -eq 'retry-a' })[0]
$pc = @($tasks | Where-Object { $_.seq -eq 0 })[0]
Add-Check 'old.retry_a_failed' ($failed.status -eq 'failed') ("status=" + $failed.status)

# 重新起 mock（已 ready）；retry 才能成功。
$log2 = Join-Path $outDir "mock-retry-$stamp.log"
$mockProc = Start-Process -FilePath $MockExe -ArgumentList @('-addr', ":$MockPort", '-quiet') -PassThru -WindowStyle Hidden -RedirectStandardOutput $log2 -RedirectStandardError "$log2.err"
$deadline = (Get-Date).AddSeconds(15)
do {
    Start-Sleep -Milliseconds 300
    try { $h = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$MockPort/healthz" -TimeoutSec 2; if ($h.StatusCode -eq 200) { break } } catch { }
} while ((Get-Date) -lt $deadline)
Add-Check 'mock.restarted_ready' ($null -ne $mockProc -and $mockProc.Id -gt 0) ("pid=" + $mockProc.Id)

# retry-plan：reuse precheck / rerun retry-a
$r = Call GET "/api/v1/runs/$runID/retry-plan"
$plan = JsonOf $r
$reuseNames = @($plan.reuse | ForEach-Object { $_.name })
$rerunNames = @($plan.rerun | ForEach-Object { $_.name })
Add-Check 'plan.200_retryable' ($r.StatusCode -eq 200 -and $plan.retryable -eq $true) ("status=" + $r.StatusCode + " retryable=" + $plan.retryable)
Add-Check 'plan.reuse_precheck' ($reuseNames -contains 'precheck') ("reuse=" + ($reuseNames -join ','))
Add-Check 'plan.rerun_retry_a' ($rerunNames -contains 'retry-a') ("rerun=" + ($rerunNames -join ','))

# retry：202 + new_run_id；立即重复 retry 必须 409（负向）
$r = Call POST "/api/v1/runs/$runID/retry"
$retry = JsonOf $r
$newRunID = $retry.new_run_id
Add-Check 'retry.202_new_run_id' ($r.StatusCode -eq 202 -and $newRunID -and $newRunID -ne $runID) ("status=" + $r.StatusCode + " new=" + $newRunID)
$dup = Call POST "/api/v1/runs/$runID/retry"
$dupJ = JsonOf $dup
Add-Check 'retry.duplicate_409' ($dup.StatusCode -eq 409 -and $dupJ.code -eq 'CONFLICT') ("status=" + $dup.StatusCode + " code=" + $dupJ.code)

$newRun = Wait-Run $newRunID 120
$newTasks = @($newRun.tasks)
$newA = @($newTasks | Where-Object { $_.name -eq 'retry-a' })[0]
$newPC = @($newTasks | Where-Object { $_.seq -eq 0 })[0]
Add-Check 'new_run.succeeded' ($newRun.status -eq 'succeeded') ("status=" + $newRun.status)
Add-Check 'new.retry_a_succeeded' ($newA.status -eq 'succeeded') ("status=" + $newA.status)
Add-Check 'new.precheck_reused' ($newPC.status -eq 'succeeded' -and $newPC.attempts -eq 0) ("status=" + $newPC.status + " attempts=" + $newPC.attempts)

# 新 Run 已无失败 Task：plan 不应再 retryable（200 false 或 409 均接受）
$r = Call GET "/api/v1/runs/$newRunID/retry-plan"
$np = JsonOf $r
$blocked = ($r.StatusCode -eq 200 -and $np.retryable -eq $false) -or ($r.StatusCode -eq 409)
Add-Check 'new_plan.not_retryable' $blocked ("status=" + $r.StatusCode + " retryable=" + $np.retryable + " reason=" + $np.reason)

# Retry Task 单端点：**所属 Run 已终态 → 必须 409 `run_terminal`**（这正是造僵尸的那条路径，D-60）。
# 「非终态 Run 的 failed Task → 202」由 server 侧用例覆盖（TestRetryTask_*），这里只锁终态语义。
$r = Call POST "/api/v1/tasks/$($failed.id)/retry"
$tj = JsonOf $r
$reason = $tj.detail.reason
Add-Check 'task.retry_terminal_409' ($r.StatusCode -eq 409 -and $reason -eq 'run_terminal') ("status=" + $r.StatusCode + " reason=" + $reason)

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{ ok = $ok; run_id = $runID; new_run_id = $newRunID; old_tasks = $tasks; new_tasks = $newTasks; plan = $plan; checks = $checks }
$jsonPath = Join-Path $outDir ("result-$stamp.json")
$out | ConvertTo-Json -Depth 10 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if ($mockProc) { Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue }
if (-not $ok) { exit 1 }
exit 0
