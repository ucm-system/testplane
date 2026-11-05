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
$deadline=(Get-Date).AddSeconds(150); $pods=@(); $noPod=$null; $d=$null
do {
    Start-Sleep -Seconds 1
    $noPod=Call GET "/api/v1/runs/$runID/deploy/logs"
    $noPod=Call GET "/api/v1/runs/$runID/deploy/logs"; $nj=JsonOf $noPod
    $pods=@($nj.pods)
    $d=JsonOf (Call GET "/api/v1/runs/$runID/deploy")
} while ($noPod.StatusCode -eq 200 -and $pods.Count -eq 0 -and $d.deploy.state -notin @('failed','blocked') -and (Get-Date) -lt $deadline)
$ns=[string]$d.deploy.namespace
# Pod 列表以 deploy/logs 的实时结果为准（/deploy 的 pods 字段在本环境可能为空）。
$noPod=Call GET "/api/v1/runs/$runID/deploy/logs"; $nj=JsonOf $noPod
for($i=0;$i -lt 15 -and $noPod.StatusCode -ne 200 -and $noPod.StatusCode -ne 409;$i++){
    Start-Sleep -Seconds 1
    $noPod=Call GET "/api/v1/runs/$runID/deploy/logs"; $nj=JsonOf $noPod
}
$pods=@($nj.pods)
Add-Check 'deploy.pods_present' ($pods.Count -ge 1) ("namespace="+$ns+" pods="+($pods.name -join ',')+" state="+$d.deploy.state)
Add-Check 'logs.no_pod' (($pods.Count -eq 1 -and $noPod.StatusCode -eq 200) -or ($pods.Count -ge 2 -and $noPod.StatusCode -eq 409)) ("pods="+$pods.Count+" status="+$noPod.StatusCode)

if($pods.Count -eq 1){
    $pod=$pods[0].name
    # 等 Pod Ready（exec 需要容器可执行），再在 Pod 内打 60 次 /healthz 制造 >50 行。
    for($i=0;$i -lt 20;$i++){
        $nj2=JsonOf (Call GET "/api/v1/runs/$runID/deploy/logs")
        $pp=@($nj2.pods | Where-Object { $_.name -eq $pod })[0]
        if($pp -and $pp.ready){ break }
        Start-Sleep -Seconds 1
    }
    $cmd='i=0; while [ $i -lt 60 ]; do wget -qO- http://127.0.0.1:18080/healthz >/dev/null; i=$((i+1)); done'
    $lines=0
    for($i=0;$i -lt 6;$i++){
        kubectl -n $ns exec $pod -- sh -c $cmd 2>&1 | Out-Null
        Start-Sleep -Seconds 2
        $r=Call GET "/api/v1/runs/$runID/deploy/logs?pod=$([uri]::EscapeDataString($pod))&tail_lines=50&limit=10"
        $j=JsonOf $r
        $lines=@($j.entries|ForEach-Object{$_.line_count}|Measure-Object -Sum).Sum
        if($lines -ge 50){ break }
    }
    $tailOK=($r.StatusCode -eq 200 -and $j.window -eq 'tail' -and $j.truncated -eq $false -and $j.next_offset -eq 0 -and $lines -eq 50)
    Add-Check 'tail.50_lines' $tailOK ("status="+$r.StatusCode+" window="+$j.window+" truncated="+$j.truncated+" next="+$j.next_offset+" lines="+$lines)
} else {
    Add-Check 'tail.50_lines' $false '没有拿到单个 Deploy Pod，无法测 tail'
}
$null=Call POST "/api/v1/runs/$runID/cancel"
$ok=-not($checks|Where-Object{-not $_.ok});$json=Join-Path $OutDir ("result-$stamp.json")
[pscustomobject]@{ok=$ok;run_id=$runID;checks=$checks}|ConvertTo-Json -Depth 8|Set-Content -Encoding utf8 $json
Write-Host "";Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks|Where-Object{$_.ok}).Count,$checks.Count,$(if($ok){'PASS'}else{'FAIL'}),$json) -ForegroundColor $(if($ok){'Green'}else{'Red'})
if(-not $ok){exit 1};exit 0

