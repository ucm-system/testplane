# Deploy（deploy runner）日志验收（v2）
#
# v2 语义（D-70）：平台 Deploy 阶段取消，部署由内置 deploy runner（genet-deploy 等）承载——
# 它是 Task 里 seq=1 的 runner（§7.2：seq 从 1 递增，含注入的 deploy/precheck/clean）。
# 日志走统一的 runner 日志端点：GET /api/v1/tasks/{id}/attempts/{n}/runners/{seq}/logs，
# 不再有 v1 的 /runs/{id}/deploy/logs（?pod= 多 Pod 409 语义随平台 Deploy 阶段一并取消）。
#
# 验证点：
#   1. 提交 deploy 形态 Task（deploy.provider=genet + spec 文本）→ 202；
#   2. deploy runner（seq=1）出现并走到终态（succeeded 或 failed/blocked 归类）；
#   3. 该 runner 的日志端点 200 且 entries 非空；tail_lines 窗口生效（≤ tail_lines 行）。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8170',
    [string]$User = 'dev',
    [string]$MockURL = 'http://mock-llm.default.svc:18080/v1',
    [string]$OutDir = ''
)
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-deploy-logs' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$checks = @()
function Add-Check([string]$Name,[bool]$Ok,[string]$Detail){$script:checks += [pscustomobject]@{name=$Name;ok=$Ok;detail=$Detail};Write-Host ("  [{0}] {1} —— {2}" -f $(if($Ok){'OK'}else{'FAIL'}),$Name,$Detail) -ForegroundColor $(if($Ok){'Green'}else{'Red'})}
$headers=@{Authorization="Bearer $User";Accept='application/json'}
function Call{param([string]$Method,[string]$Path,$Body=$null);$uri="$BaseUrl$Path";if($null -ne $Body){$json=$Body|ConvertTo-Json -Depth 8 -Compress;return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json' -Body $json};return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers}
function JsonOf($r){try{return $r.Content|ConvertFrom-Json}catch{return $null}}
$null=Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5
Write-Host "deploy runner 日志验收：$BaseUrl" -ForegroundColor Cyan

# v2 提交体（固定形态）：deploy.provider=genet + spec（yaml 文本）；deploy runner 由平台自动注入。
$manifest=@{task=@{name="deploy-logs-$stamp";deploy=@{provider='genet';spec='model_name: mock-llm'};precheck='off';runners=@(@{name='probe';runner='endpoint-probe';version='1.0.0';execution=@{profile='k8s-npu'};input='{"wait_timeout_seconds":60}';on_failure='continue'})}}|ConvertTo-Json -Depth 8 -Compress
$r=Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/tasks" -Headers $headers -ContentType 'application/json' -Body $manifest
$taskID=(JsonOf $r).task.id
Add-Check 'submit.202' ($r.StatusCode -eq 202 -and $taskID) ("task="+$taskID)

# 等 deploy runner（seq=1）出现并终态
$deploySeq=1
$deadline=(Get-Date).AddSeconds(150); $deployRunner=$null
do {
    Start-Sleep -Seconds 1
    $runners=(JsonOf (Call GET "/api/v1/tasks/$taskID/runners?limit=50")).items
    $deployRunner=@($runners | Where-Object { $_.seq -eq $deploySeq })[0]
} while ((Get-Date) -lt $deadline -and (-not $deployRunner -or $deployRunner.status -notin @('succeeded','failed','canceled')))
Add-Check 'deploy.runner_present' ($null -ne $deployRunner) ("seq=$deploySeq status=" + $(if ($deployRunner) { $deployRunner.status } else { '-' }))

# runner 日志端点：200 且有内容（日志按 runner 落盘，§8.6）
$logResp=Call GET "/api/v1/tasks/$taskID/attempts/1/runners/$deploySeq/logs"
$logJson=JsonOf $logResp
$entryCount=@($logJson.entries).Count
Add-Check 'logs.200' ($logResp.StatusCode -eq 200) ("status="+$logResp.StatusCode)
Add-Check 'logs.entries_nonempty' ($entryCount -gt 0) ("entries="+$entryCount)

# tail_lines 窗口：请求最后 5 行，返回不超过 5 行（分页字段名以 openapi 为准：limit/tail_lines）
$tailResp=Call GET "/api/v1/tasks/$taskID/attempts/1/runners/$deploySeq/logs?tail_lines=5&limit=10"
$tailJson=JsonOf $tailResp
$tailLines=@($tailJson.entries).Count
Add-Check 'logs.tail_window' ($tailResp.StatusCode -eq 200 -and $tailLines -le 5 -and $tailLines -gt 0) ("status="+$tailResp.StatusCode+" lines="+$tailLines)

$null=Call POST "/api/v1/tasks/$taskID/cancel"
$ok=-not($checks|Where-Object{-not $_.ok});$json=Join-Path $OutDir ("result-$stamp.json")
[pscustomobject]@{ok=$ok;task_id=$taskID;deploy_seq=$deploySeq;checks=$checks}|ConvertTo-Json -Depth 8|Set-Content -Encoding utf8 $json
Write-Host "";Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks|Where-Object{$_.ok}).Count,$checks.Count,$(if($ok){'PASS'}else{'FAIL'}),$json) -ForegroundColor $(if($ok){'Green'}else{'Red'})
if(-not $ok){exit 1};exit 0
