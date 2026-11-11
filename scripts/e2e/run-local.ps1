# Argus · 本地 e2e 一把梭（Windows / pwsh）
#
# 做三件事：
#   1. 编译 mock-llm 与 e2e（产物放临时目录，不污染仓库）
#   2. 确保 mock-llm 在 :18080 活着（没有就起一个，脚本结束时回收）
#   3. 确认控制面 :8080 可达后，跑 `e2e run`
#
# 用法：
#   pwsh scripts/e2e/run-local.ps1
#   pwsh scripts/e2e/run-local.ps1 -BaseURL http://127.0.0.1:8080 -Expect succeeded
#   pwsh scripts/e2e/run-local.ps1 -Mode deploy -MockURL http://mock-llm.argus-svc.svc.cluster.local:18080/v1
#   pwsh scripts/e2e/run-local.ps1 -KeepMock
#
# MockURL 不传时自动探测 kind 里的 mock-llm Service（argus-mock / default /
# argus / argus-svc），探测不到才回退本机 http://127.0.0.1:18080/v1。
# 评审 #16 同类问题：工具默认值不该让集群场景必须带参数才通过。
# 前置：控制面已启动（PG + 迁移 + --role=all，监听 8080；见 deploy/README 或 Lead 的启动说明），
#       且 Registry 里能看到 endpoint-probe、存在可用的 execution profile。
#
# 退出码与 e2e 一致：0 通过；1 断言失败；2 参数错误；3 连不上控制面。

[CmdletBinding()]
param(
    [string]$BaseURL = 'http://127.0.0.1:8080',
    [string]$MockURL = '',
    [string]$Expect = 'succeeded',
    [string]$Profile = 'k8s-npu',
    [ValidateSet('service', 'deploy')]
    [string]$Mode = 'service',
    [switch]$KeepMock,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ExtraArgs = @()
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$tmp = [System.IO.Path]::GetTempPath()

if ([string]::IsNullOrWhiteSpace($MockURL)) {
    $MockURL = 'http://127.0.0.1:18080/v1'
    $kubectl = Get-Command kubectl -ErrorAction SilentlyContinue
    if ($kubectl) {
        foreach ($ns in @('argus-mock', 'default', 'argus', 'argus-svc')) {
            $svc = & kubectl -n $ns get svc mock-llm -o name --request-timeout=3s 2>$null
            if ($LASTEXITCODE -eq 0 -and (($svc | Out-String).Trim()) -ne '') {
                $MockURL = "http://mock-llm.$ns.svc:18080/v1"
                Write-Host "自动探测 mock-llm：namespace=$ns，MockURL=$MockURL"
                break
            }
        }
    }
}


function Wait-Healthy {
    param([string]$Url, [int]$TimeoutSeconds = 15)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -UseBasicParsing "$Url/healthz" -TimeoutSec 2
            if ($r.StatusCode -eq 200) { return $true }
        } catch { Start-Sleep -Milliseconds 300 }
    }
    return $false
}

$mockProc = $null
$e2eCode = 1
try {
    # ---- 1. 编译 ---------------------------------------------------------
    Push-Location (Join-Path $repo 'tools\mock-llm')
    $mockExe = Join-Path $tmp 'argus-mock-llm.exe'
    & go build -o $mockExe .
    if ($LASTEXITCODE -ne 0) { throw 'go build mock-llm 失败' }
    Pop-Location

    Push-Location (Join-Path $repo 'tools\e2e')
    $e2eExe = Join-Path $tmp 'argus-e2e.exe'
    & go build -o $e2eExe .
    if ($LASTEXITCODE -ne 0) { throw 'go build e2e 失败' }
    Pop-Location

    # ---- 2. mock-llm ----------------------------------------------------
    $mockBase = 'http://127.0.0.1:18080'
    if (Wait-Healthy -Url $mockBase -TimeoutSeconds 1) {
        Write-Host "mock-llm 已在运行：$mockBase"
    } else {
        Write-Host "启动 mock-llm：$mockBase"
        $mockProc = Start-Process -FilePath $mockExe -ArgumentList @('-addr=:18080') -PassThru -WindowStyle Hidden
        if (-not (Wait-Healthy -Url $mockBase -TimeoutSeconds 15)) {
            throw "mock-llm 起不来（18080 被占用？）"
        }
    }

    # ---- 3. 控制面 ------------------------------------------------------
    if (-not (Wait-Healthy -Url $BaseURL -TimeoutSeconds 2)) {
        Write-Host ""
        Write-Host "控制面不可达：$BaseURL" -ForegroundColor Red
        Write-Host "  先启动控制面（PG + 迁移 + --role=all，监听 8080），再重跑本脚本。"
        Write-Host "  参考：scripts/e2e/README.md 的『前置』与 deploy/kind/argus.yaml。"
        exit 3
    }

    # ---- 4. e2e ---------------------------------------------------------
    $e2eArgs = @(
        'run',
        '--base-url', $BaseURL,
        '--mode', $Mode,
        '--mock-url', $MockURL,
        '--profile', $Profile,
        '--expect', $Expect
    ) + $ExtraArgs

    Write-Host ""
    Write-Host ("e2e " + ($e2eArgs -join ' '))
    & $e2eExe @e2eArgs
    $e2eCode = $LASTEXITCODE
}
catch {
    Write-Host "run-local 失败：$($_.Exception.Message)" -ForegroundColor Red
    $e2eCode = 2
}
finally {
    if ($mockProc -and -not $KeepMock) {
        Write-Host "回收 mock-llm（PID $($mockProc.Id)）"
        Stop-Process -Id $mockProc.Id -Force -ErrorAction SilentlyContinue
    }
}

exit $e2eCode
