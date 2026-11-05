# 产物持久化验收（#14）：Pod/Job 消失后产物仍可获取
#
# 三段：
#   1) 基线：service Run（2 个 test）→ succeeded；记录 /artifacts 条目数/总大小/result 数，
#      以及 tasks/<task>/attempts/1/result.json 的字节与 sha256；
#   2) 删除 Job/Pod 后：/artifacts 条目数/总大小/result 数/路径集合/文件 sha256 必须与基线完全一致；
#      /tasks/{id}/attempts/1/artifacts 仍可用；/files 的 Range 仍为 206 + 10 字节；
#   3) 控制面 rollout restart 后：再取 /files，sha256 仍一致。
# 负向：不存在 path → 404；`../../etc/passwd` → 400。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8120',
    [string]$User = 'dev',
    [string]$Namespace = 'testplane',
    [string]$MockURL = '',
    [string]$OutDir = '',
    [int]$ForwardPort = 8120,
    [int]$RestartPort = 8121,
    [int]$TerminalTimeoutSeconds = 240,
    [int]$PodGoneTimeoutSeconds = 600,
    [int]$RestartTimeoutSeconds = 240,
    [switch]$SkipForward,
    [switch]$SkipRestart,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-artifacts' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$h = @{ Authorization = "Bearer $User"; Accept = 'application/json' }
$script:ApiBase = $BaseUrl
$script:ForwardProcs = @()
$script:NextForwardPort = 8125

if ([string]::IsNullOrWhiteSpace($MockURL)) {
    $MockURL = 'http://mock-llm.default.svc:18080/v1'
    foreach ($ns in @('testplane-mock', 'default', 'testplane', 'testplane-svc')) {
        $svc = & kubectl -n $ns get svc mock-llm -o name --request-timeout=3s 2>$null
        if ($LASTEXITCODE -eq 0 -and (($svc | Out-String).Trim()) -ne '') {
            $MockURL = "http://mock-llm.$ns.svc:18080/v1"
            break
        }
    }
}

function Start-ApiForward {
    param([int]$Port)
    $log = Join-Path $OutDir ("forward-$Port.log")
    $p = Start-Process -FilePath 'kubectl' `
        -ArgumentList @('-n', $Namespace, 'port-forward', 'svc/testplane', "${Port}:8080") `
        -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err"
    $deadline = (Get-Date).AddSeconds(25)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$Port/healthz" -TimeoutSec 2
            if ($r.StatusCode -eq 200) { return $p }
        } catch { Start-Sleep -Milliseconds 500 }
    }
    throw "port-forward $Port 在 25s 内不可用（见 $log）"
}

function Restart-ApiForward {
    foreach ($p in $script:ForwardProcs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    $script:ForwardProcs = @()
    for ($i = 0; $i -lt 5; $i++) {
        $port = $script:NextForwardPort
        $script:NextForwardPort++
        try {
            $p = Start-ApiForward -Port $port
            $script:ForwardProcs += $p
            $script:ApiBase = "http://127.0.0.1:$port"
            return
        } catch { }
    }
    throw '无法重建 API port-forward'
}

function Invoke-Api {
    param([string]$Method, [string]$Path, [string]$Body = '')
    $uri = "$script:ApiBase$Path"
    if ($Body -ne '') {
        return Invoke-RestMethod -Method $Method -Uri $uri -Headers $h -ContentType 'application/json; charset=utf-8' -Body $Body
    }
    return Invoke-RestMethod -Method $Method -Uri $uri -Headers $h
}

function Get-FileBytes {
    param([string]$RelativePath, [hashtable]$ExtraHeaders = @{})
    $tmp = Join-Path $OutDir ("file-" + [guid]::NewGuid().ToString('N') + ".bin")
    $headers = @{ Authorization = "Bearer $User" } + $ExtraHeaders
    $u = "$script:ApiBase/api/v1/runs/$script:RunId/files?path=" + [uri]::EscapeDataString($RelativePath)
    $r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -PassThru -Uri $u -Headers $headers -OutFile $tmp
    $bytes = if (Test-Path -LiteralPath $tmp) { [System.IO.File]::ReadAllBytes($tmp) } else { [byte[]]@() }
    Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
    $hex = if ($bytes.Length -gt 0) {
        $sha = [System.Security.Cryptography.SHA256]::Create()
        (($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') }) -join '')
    } else { '' }
    return [pscustomobject]@{ status = [int]$r.StatusCode; bytes = $bytes; sha256 = $hex }
}

function New-Manifest {
    param([string]$Stamp)
    return @"
{"run":{"name":"artifact-$Stamp","service":{"url":"$MockURL","model_name":"mock-llm"},"precheck":"off","cleanup":{"policy":"never"},"tests":[{"name":"artifact-a","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=15 --retry_interval_seconds=1"},{"name":"artifact-b","tester":"endpoint-probe","version":"1.0.0","execution":{"profile":"k8s-npu"},"args":"--wait_timeout_seconds=15 --retry_interval_seconds=1"}]}}
"@
}

if ($DryRun) {
    Write-Host "artifact-persistence-check dry-run（不连后端、不删 Job、不重启）" -ForegroundColor Cyan
    Write-Host "  base=$BaseUrl mock=$MockURL"
    Write-Host "  1) 2-test service Run → 基线 artifacts/result.json sha256"
    Write-Host "  2) 删除 Job/Pod → artifacts 条目数/大小/result 数/sha256 完全一致；attempt artifacts + Range(0-9) 可用"
    Write-Host "  3) rollout restart testplaned → /files sha256 仍一致"
    Write-Host "  负向：不存在 path → 404；../../etc/passwd → 400"
    Write-Host (New-Manifest -Stamp (Get-Date -Format 'yyyyMMdd-HHmmss'))
    Write-Host "dry-run OK" -ForegroundColor Green
    exit 0
}

$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}

Write-Host "产物持久化验收：$BaseUrl（mock=$MockURL）" -ForegroundColor Cyan
if (-not $SkipForward) {
    $script:ForwardProcs += Start-ApiForward -Port $ForwardPort
    $script:ApiBase = "http://127.0.0.1:$ForwardPort"
}
$null = Invoke-WebRequest -UseBasicParsing "$script:ApiBase/healthz" -TimeoutSec 5

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$submitted = Invoke-Api -Method POST -Path '/api/v1/runs' -Body (New-Manifest -Stamp $stamp)
$script:RunId = [string]$submitted.run.id
Write-Host "已提交 Run $($script:RunId)"

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$run = $null
while ($sw.Elapsed.TotalSeconds -lt $TerminalTimeoutSeconds) {
    $run = Invoke-Api -Method GET -Path "/api/v1/runs/$script:RunId"
    if ($run.status -in @('succeeded', 'failed', 'completed_with_errors', 'service_unavailable', 'canceled')) { break }
    Start-Sleep -Milliseconds 500
}
$sw.Stop()
Write-Host ("终态：{0}，耗时 {1:N1}s" -f $run.status, $sw.Elapsed.TotalSeconds)

Write-Host ""
Write-Host "基线断言：" -ForegroundColor Cyan
Add-Check 'run.succeeded' ($run.status -eq 'succeeded') ("status=" + $run.status)
$tasks = @((Invoke-Api -Method GET -Path "/api/v1/runs/$script:RunId/tasks?limit=50").items)
Add-Check 'run.two_tests' ($tasks.Count -eq 2) ("tasks=" + $tasks.Count)
Add-Check 'tasks.succeeded' (@($tasks | Where-Object { $_.status -ne 'succeeded' }).Count -eq 0) (($tasks | ForEach-Object { "$($_.name)=$($_.status)" }) -join ', ')
$task = @($tasks | Where-Object { $_.name -eq 'artifact-a' })[0]
if (-not $task) { $task = $tasks[0] }
$taskId = [string]$task.id
$fileRel = "tasks/$taskId/attempts/1/result.json"
$taskDetail = Invoke-Api -Method GET -Path "/api/v1/tasks/$taskId"
$attempts = @($taskDetail.attempts)
$attemptTerminal = $attempts.Count -gt 0 -and @($attempts | Where-Object { $_.status -notin @('succeeded', 'failed', 'canceled') }).Count -eq 0
Add-Check 'attempt.terminal_before_delete' $attemptTerminal (($attempts | ForEach-Object { "idx=$($_.idx) status=$($_.status)" }) -join ', ')

# 产物索引是终态后异步写入；基线等它出现 result 行（最多 60s），
# 但后面仍然严格比较条目数/大小/sha256，不放宽核心断言。
$baseDeadline = (Get-Date).AddSeconds(60)
while ($true) {
    $baseArtifacts = Invoke-Api -Method GET -Path "/api/v1/runs/$script:RunId/artifacts"
    $baseItems = @($baseArtifacts.items)
    $baseResultCount = @($baseItems | Where-Object { $_.kind -eq 'result' }).Count
    if ($baseResultCount -gt 0 -or (Get-Date) -ge $baseDeadline) { break }
    Start-Sleep -Seconds 1
}
$baseCount = $baseItems.Count
$baseTotal = [long]$baseArtifacts.total_size_bytes
$basePaths = @($baseItems | ForEach-Object { $_.path } | Sort-Object) -join "`n"
$baseFile = Get-FileBytes -RelativePath $fileRel
Add-Check 'baseline.artifact_result_exists' ($baseResultCount -gt 0) ("result=" + $baseResultCount + " / items=" + $baseCount + " / total=" + $baseTotal)
Add-Check 'baseline.file_200' ($baseFile.status -eq 200) ("status=" + $baseFile.status + " / bytes=" + $baseFile.bytes.Length + " / sha=" + $baseFile.sha256)
Add-Check 'baseline.file_nonempty' ($baseFile.bytes.Length -gt 0) ("bytes=" + $baseFile.bytes.Length)
Write-Host ("基线：artifacts={0}, total={1}, result={2}, fileBytes={3}, sha={4}" -f $baseCount, $baseTotal, $baseResultCount, $baseFile.bytes.Length, $baseFile.sha256)

# 确认 Attempt 已终态后删除 Job（不等 GC，加速；语义与 GC 删除后的读取一致）。
$jobName = "tp-t-" + $taskId.ToLower() + "-a1"
$jobFound = & kubectl -n $Namespace get job $jobName -o name --ignore-not-found 2>&1
if ($LASTEXITCODE -ne 0) { throw "kubectl get job 失败：$($jobFound -join ' ')" }
if (($jobFound | Out-String).Trim() -ne '') {
    Write-Host "删除 Job $jobName（Attempt 已终态）…"
    kubectl -n $Namespace delete job $jobName --wait=true --timeout=60s 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "kubectl delete job $jobName 失败" }
} else {
    Write-Host "Job $jobName 已被 GC"
}
$podSw = [System.Diagnostics.Stopwatch]::StartNew()
$pods = @()
while ($podSw.Elapsed.TotalSeconds -lt $PodGoneTimeoutSeconds) {
    $pods = @(& kubectl -n $Namespace get pods -o name 2>&1 | Where-Object { $_ -match [regex]::Escape("tp-t-$($taskId.ToLower())") })
    if ($pods.Count -eq 0) { break }
    Start-Sleep -Seconds 2
}
Add-Check 'pod.gone' ($pods.Count -eq 0) $(if ($pods.Count -eq 0) { "无 tp-t-$($taskId.ToLower()) Pod" } else { $pods -join ', ' })

Write-Host ""
Write-Host "Pod 消失后断言：" -ForegroundColor Cyan
$afterArtifacts = Invoke-Api -Method GET -Path "/api/v1/runs/$script:RunId/artifacts"
$afterItems = @($afterArtifacts.items)
$afterPaths = @($afterItems | ForEach-Object { $_.path } | Sort-Object) -join "`n"
$afterFile = Get-FileBytes -RelativePath $fileRel
Add-Check 'after.artifact_count_same' ($afterItems.Count -eq $baseCount) ("before=" + $baseCount + " after=" + $afterItems.Count)
Add-Check 'after.artifact_total_same' ([long]$afterArtifacts.total_size_bytes -eq $baseTotal) ("before=" + $baseTotal + " after=" + $afterArtifacts.total_size_bytes)
Add-Check 'after.result_count_same' (@($afterItems | Where-Object { $_.kind -eq 'result' }).Count -eq $baseResultCount) ("before=" + $baseResultCount + " after=" + @($afterItems | Where-Object { $_.kind -eq 'result' }).Count)
Add-Check 'after.path_set_same' ($afterPaths -eq $basePaths) '产物路径集合一致'
Add-Check 'after.file_200' ($afterFile.status -eq 200) ("status=" + $afterFile.status + " / bytes=" + $afterFile.bytes.Length)
Add-Check 'after.sha256_same' ($afterFile.sha256 -eq $baseFile.sha256) ("before=" + $baseFile.sha256 + " after=" + $afterFile.sha256)

$attemptArtifacts = Invoke-Api -Method GET -Path "/api/v1/tasks/$taskId/attempts/1/artifacts"
$attemptItems = @($attemptArtifacts.items)
Add-Check 'after.attempt_artifacts' ($attemptItems.Count -gt 0) ("items=" + $attemptItems.Count)
$range = Get-FileBytes -RelativePath $fileRel -ExtraHeaders @{ Range = 'bytes=0-9' }
Add-Check 'after.range_206' ($range.status -eq 206) ("status=" + $range.status)
Add-Check 'after.range_10_bytes' ($range.bytes.Length -eq 10) ("bytes=" + $range.bytes.Length)

# 负向：不存在 404、路径穿越 400。
$bad404 = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Uri ("$script:ApiBase/api/v1/runs/$script:RunId/files?path=" + [uri]::EscapeDataString("no-such-artifact-$stamp.bin")) -Headers @{ Authorization = "Bearer $User" }
Add-Check 'negative.missing_404' ($bad404.StatusCode -eq 404) ("status=" + $bad404.StatusCode)
$bad400 = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Uri ("$script:ApiBase/api/v1/runs/$script:RunId/files?path=" + [uri]::EscapeDataString("../../etc/passwd")) -Headers @{ Authorization = "Bearer $User" }
Add-Check 'negative.traversal_400' ($bad400.StatusCode -eq 400) ("status=" + $bad400.StatusCode)

if (-not $SkipRestart) {
    Write-Host ""
    Write-Host "控制面重启后断言：" -ForegroundColor Cyan
    kubectl -n $Namespace rollout restart deploy/testplaned 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'rollout restart 失败' }
    kubectl -n $Namespace rollout status deploy/testplaned --timeout="$($RestartTimeoutSeconds)s" 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'rollout status 超时' }
    if (-not $SkipForward) { Restart-ApiForward }
    $null = Invoke-WebRequest -UseBasicParsing "$script:ApiBase/healthz" -TimeoutSec 10
    $restartFile = Get-FileBytes -RelativePath $fileRel
    Add-Check 'restart.file_200' ($restartFile.status -eq 200) ("status=" + $restartFile.status + " / bytes=" + $restartFile.bytes.Length)
    Add-Check 'restart.sha256_same' ($restartFile.sha256 -eq $baseFile.sha256) ("before=" + $baseFile.sha256 + " after=" + $restartFile.sha256)
}

foreach ($p in $script:ForwardProcs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok
    run_id = $script:RunId
    task_id = $taskId
    file = $fileRel
    baseline = [pscustomobject]@{ artifacts = $baseCount; total_size_bytes = $baseTotal; result_count = $baseResultCount; file_bytes = $baseFile.bytes.Length; sha256 = $baseFile.sha256 }
    after_pod_gone = [pscustomobject]@{ artifacts = $afterItems.Count; total_size_bytes = [long]$afterArtifacts.total_size_bytes; file_bytes = $afterFile.bytes.Length; sha256 = $afterFile.sha256 }
    restart = $(if ($restartFile) { [pscustomobject]@{ file_bytes = $restartFile.bytes.Length; sha256 = $restartFile.sha256 } } else { $null })
    checks = $checks
}
$jsonPath = Join-Path $OutDir ("result-" + $stamp + ".json")
$out | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0

