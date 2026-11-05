# TestPlane · 用真实 endpoint-probe 反验 mock-llm（Windows / pwsh）
#
# 这不是"再写一个探针"，而是让 testers/endpoint-probe/run.sh 原封不动跑在
# 一次性 alpine 容器里，对准 mock-llm，验证五种现场下 Tester 的判定与设计一致：
#
#   ready            -> exit 0 / succeeded / 1 次探测
#   slow-ready-15s   -> exit 0 / succeeded / 多次探测（窗口内重试）
#   fail-401         -> exit 1 / PRECHECK_FAILED / retryable=false，且不耗满窗口
#   fail-500         -> exit 1 / PRECHECK_FAILED / retryable=true（窗口用尽）
#   error-in-200     -> exit 1 / PRECHECK_FAILED / retryable=false
#
# 用法：
#   pwsh scripts/e2e/probe-check.ps1
#   pwsh scripts/e2e/probe-check.ps1 -OutputRoot D:\tmp\qa-probe
#
# 前置：Docker 可用（首次会 build 一个带 jq/bash/curl 的 runner 镜像，之后走缓存）。
# 不改 testers/、不 build Tester 镜像；run.sh 只读挂载进容器。
#
# 退出码：0 全部符合预期；1 有场景不符合；2 环境/构建问题。

[CmdletBinding()]
param(
    [string]$OutputRoot = ''
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutputRoot)) {
    $OutputRoot = Join-Path $repo '.data\qa-probe'
}
$probeScript = Join-Path $repo 'testers\endpoint-probe\run.sh'
if (-not (Test-Path -LiteralPath $probeScript)) {
    Write-Host "找不到 $probeScript" -ForegroundColor Red
    exit 2
}
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null

# ---- 一次性 runner 镜像（jq/bash/curl）-------------------------------------
$runnerImage = 'testplane/probe-runner:qa'
& docker image inspect $runnerImage *> $null
if ($LASTEXITCODE -ne 0) {
    $ctx = Join-Path ([System.IO.Path]::GetTempPath()) ("testplane-probe-runner-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $ctx | Out-Null
    @'
FROM alpine:3.20
RUN apk add --no-cache bash curl jq ca-certificates tzdata
'@ | Set-Content -Encoding ascii (Join-Path $ctx 'Dockerfile')
    Write-Host "构建 runner 镜像 $runnerImage …"
    & docker build -q -t $runnerImage $ctx | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "docker build 失败" -ForegroundColor Red
        exit 2
    }
    Remove-Item -Recurse -Force $ctx -ErrorAction SilentlyContinue
}

# ---- 编译并启动 5 个不同故障的 mock-llm ------------------------------------
$tmp = [System.IO.Path]::GetTempPath()
Push-Location (Join-Path $repo 'tools\mock-llm')
$mockExe = Join-Path $tmp 'testplane-mock-llm.exe'
& go build -o $mockExe .
Pop-Location
if ($LASTEXITCODE -ne 0) {
    Write-Host "go build mock-llm 失败" -ForegroundColor Red
    exit 2
}

$scenarios = @(
    @{ Name = 'slow-ready-12s'; Port = 18105; MockArgs = @('-addr=:18105', '-ready-after=12s'); ProbeArgs = '--wait_timeout_seconds=30 --retry_interval_seconds=2'; Exit = 0; Status = 'succeeded'; Code = ''; Retryable = $null;  MinAttempts = 2; MaxSeconds = 60 }
    @{ Name = 'ready';          Port = 18101; MockArgs = @('-addr=:18101');               ProbeArgs = '--wait_timeout_seconds=20 --retry_interval_seconds=1'; Exit = 0; Status = 'succeeded'; Code = ''; Retryable = $null;  MinAttempts = 1; MaxSeconds = 30 }
    @{ Name = 'fail-401';       Port = 18102; MockArgs = @('-addr=:18102', '-fail=401');  ProbeArgs = '--wait_timeout_seconds=30 --retry_interval_seconds=1'; Exit = 1; Status = 'failed';    Code = 'PRECHECK_FAILED'; Retryable = $false; MinAttempts = 1; MaxSeconds = 20 }
    @{ Name = 'fail-500';       Port = 18103; MockArgs = @('-addr=:18103', '-fail=500');  ProbeArgs = '--wait_timeout_seconds=2 --retry_interval_seconds=1';  Exit = 1; Status = 'failed';    Code = 'PRECHECK_FAILED'; Retryable = $true;  MinAttempts = 2; MaxSeconds = 30 }
    @{ Name = 'error-in-200';   Port = 18104; MockArgs = @('-addr=:18104', '-error-in-200'); ProbeArgs = '--wait_timeout_seconds=30 --retry_interval_seconds=1'; Exit = 1; Status = 'failed'; Code = 'PRECHECK_FAILED'; Retryable = $false; MinAttempts = 1; MaxSeconds = 20 }
)

$procs = @()
function Wait-MockReady {
    param([int]$Port)
    $deadline = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$Port/healthz" -TimeoutSec 1
            if ($r.StatusCode -eq 200) { return $true }
        } catch { Start-Sleep -Milliseconds 200 }
    }
    return $false
}

$rows = @()
$failed = 0
try {
    foreach ($s in $scenarios) {
        $p = Start-Process -FilePath $mockExe -ArgumentList $s.MockArgs -PassThru -WindowStyle Hidden
        $procs += $p
        if (-not (Wait-MockReady -Port $s.Port)) {
            throw "mock-llm:$($s.Port) 起不来"
        }
    }

    foreach ($s in $scenarios) {
        $outDir = Join-Path $OutputRoot $s.Name
        $null = New-Item -ItemType Directory -Force -Path $outDir
        $inner = "bash /tester/run.sh --url=http://host.docker.internal:$($s.Port)/v1 --model_name=mock-llm $($s.ProbeArgs)"
        $dockerArgs = @(
            'run', '--rm',
            '--add-host=host.docker.internal:host-gateway',
            '-v', "${probeScript}:/tester/run.sh:ro",
            '-v', "${outDir}:/workspace/output",
            $runnerImage, 'sh', '-c', $inner
        )

        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $log = & docker @dockerArgs 2>&1 | Out-String
        $exit = $LASTEXITCODE
        $sw.Stop()
        $log | Set-Content -Encoding utf8 (Join-Path $outDir 'container.log')

        $resultPath = Join-Path $outDir 'result.json'
        $r = $null
        if (Test-Path -LiteralPath $resultPath) {
            $r = Get-Content -Raw -Encoding utf8 $resultPath | ConvertFrom-Json
        }
        $attempts = 0
        if ($r) {
            $m = $r.metrics | Where-Object { $_.name -eq 'precheck_attempts' }
            if ($m) { $attempts = [int]$m.value }
        }
        $retryable = $null
        if ($r -and $r.error) { $retryable = [bool]$r.error.retryable }

        $problems = @()
        if ($exit -ne $s.Exit) { $problems += "exit=$exit（期望 $($s.Exit)）" }
        if (-not $r) { $problems += '没有产出 result.json' }
        else {
            if ($r.status -ne $s.Status) { $problems += "status=$($r.status)（期望 $($s.Status)）" }
            $code = ''
            if ($r.error) { $code = [string]$r.error.code }
            if ($code -ne $s.Code) { $problems += "error.code=$code（期望 $($s.Code)）" }
            if ($null -ne $s.Retryable -and $retryable -ne $s.Retryable) {
                $problems += "retryable=$retryable（期望 $($s.Retryable)）"
            }
            if ($attempts -lt $s.MinAttempts) { $problems += "attempts=$attempts（至少 $($s.MinAttempts)）" }
        }
        if ($sw.Elapsed.TotalSeconds -gt $s.MaxSeconds) {
            $problems += ("耗时 {0:N1}s > {1}s（该场景应立即结束或按窗口结束）" -f $sw.Elapsed.TotalSeconds, $s.MaxSeconds)
        }

        $ok = ($problems.Count -eq 0)
        if (-not $ok) { $failed++ }
        $rows += [pscustomobject]@{
            scenario = $s.Name
            ok       = $ok
            exit     = $exit
            status   = if ($r) { $r.status } else { '-' }
            code     = if ($r -and $r.error) { $r.error.code } else { '-' }
            retryable = if ($null -ne $retryable) { $retryable } else { '-' }
            attempts = $attempts
            seconds  = [math]::Round($sw.Elapsed.TotalSeconds, 1)
            problem  = ($problems -join '；')
        }
    }
}
catch {
    Write-Host "probe-check 失败：$($_.Exception.Message)" -ForegroundColor Red
    $failed++
}
finally {
    foreach ($p in $procs) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
}

$rows | Format-Table -AutoSize | Out-String -Width 220 | Write-Host
if ($failed -gt 0) {
    Write-Host "$failed 个场景不符合预期；result.json 在 $OutputRoot" -ForegroundColor Red
    exit 1
}
Write-Host "全部场景符合预期；result.json 与探针日志在 $OutputRoot" -ForegroundColor Green
exit 0
