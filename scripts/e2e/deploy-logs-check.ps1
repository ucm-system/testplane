# Deploy 日志新语义（#40）：?pod= 缺省 0/1 Pod → 200；tail_lines 窗口不可续页。
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
Write-Host "Deploy 日志新语义验收：$BaseUrl" -ForegroundColor Cyan

$manifest=@{run=@{name="deploy-logs-$stamp";deploy=@{provider='genet';parameters=@{model_name='mock-llm'}};precheck='off';cleanup=@{policy='never'};tests=@(@{name='probe';tester='endpoint-probe';version='1.0.0';execution=@{profile='k8s-npu'};args='--wait_timeout_seconds=60'})}}|ConvertTo-Json -Depth 8 -Compress
$r=Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/runs" -Headers $headers -ContentType 'application/json' -Body $manifest
$runID=(JsonOf $r).run.id
Add-Check 'submit.202' ($r.StatusCode -eq 202 -and $runID) ("run="+$runID)
$deadline=(Get-Date).AddSeconds(120); $pods=@()
do { Start-Sleep -Seconds 1; $d=JsonOf (Call GET "/api/v1/runs/$runID/deploy"); $pods=@($d.pods); $state=$d.deploy.state } while ($pods.Count -eq 0 -and $state -notin @('ready','failed','blocked') -and (Get-Date) -lt $deadline)
Add-Check 'deploy.pods_present' ($pods.Count -ge 1) ("pods="+($pods.name -join ',')+" state="+$state)

$r=Call GET "/api/v1/runs/$runID/deploy/logs"
$j=JsonOf $r
Add-Check 'logs.no_pod_single_200' (($pods.Count -eq 1 -and $r.StatusCode -eq 200) -or ($pods.Count -gt 1 -and $r.StatusCode -eq 409)) ("pods="+$pods.Count+" status="+$r.StatusCode+" reason="+$j.detail.reason)

if($pods.Count -eq 1){
    $pod=$pods[0].name
    $r=Call GET "/api/v1/runs/$runID/deploy/logs?pod=$pod&tail_lines=50&limit=10"
    $j=JsonOf $r
    $lines=@($j.entries|ForEach-Object{$_.line_count}|Measure-Object -Sum).Sum
    Add-Check 'tail.status_200' ($r.StatusCode -eq 200) ("status="+$r.StatusCode)
    Add-Check 'tail.window_tail' ($j.window -eq 'tail') ("window="+$j.window)
    Add-Check 'tail.truncated_false' ($j.truncated -eq $false) ("truncated="+$j.truncated)
    Add-Check 'tail.next_offset_0' ($j.next_offset -eq 0) ("next_offset="+$j.next_offset)
    Add-Check 'tail.50_lines' ($lines -eq 50) ("line_count_sum="+$lines+" entries="+@($j.entries).Count)
} else {
    Add-Check 'tail.multi_pod_409' ($false) '当前 fake-genet 只造出 0/1 Pod，无法覆盖多 Pod ambiguous；已知限制'
}
$null=Call POST "/api/v1/runs/$runID/cancel"
$ok=-not($checks|Where-Object{-not $_.ok});$json=Join-Path $OutDir ("result-$stamp.json")
[pscustomobject]@{ok=$ok;run_id=$runID;checks=$checks}|ConvertTo-Json -Depth 8|Set-Content -Encoding utf8 $json
Write-Host "";Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks|Where-Object{$_.ok}).Count,$checks.Count,$(if($ok){'PASS'}else{'FAIL'}),$json) -ForegroundColor $(if($ok){'Green'}else{'Red'})
if(-not $ok){exit 1};exit 0
