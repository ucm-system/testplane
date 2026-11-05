# 管理面 P1 端点验收：profiles/clusters CRUD、secrets 登记与 rotate、
# audit 有记录、人工清理产物（终态 200 / 非终态 409）。
#
# 前置：
#   1) 控制面可访问（默认 http://127.0.0.1:8093），dev 模式角色 admin；
#   2) rotate 真集群用例要求 D-55 的窄授权已 apply（否则 verified 用例按设计判红）：
#        pwsh deploy/k8s/gen-secret-rotate-role.ps1 -Namespace testplane `
#          -SecretName tp-secret-endpoint-mock -Out deploy/k8s/rbac-secret-rotate.yaml
#        kubectl --context kind-testplane apply -f deploy/k8s/rbac-secret-rotate.yaml
#      该用例走的是**部署的 SA**（控制面 Pod 的 ServiceAccount），不是本机 admin kubeconfig。
#
# 用法：pwsh scripts/e2e/admin-check.ps1 -BaseUrl http://127.0.0.1:8093
param(
    [string]$BaseUrl = 'http://127.0.0.1:8093',
    [string]$User = 'dev',
    [string]$MockURL = 'http://mock-llm.default.svc:18080/v1',
    [string]$SlowMockURL = 'http://mock-llm-slow.default.svc:18080/v1',
    [string]$Namespace = 'testplane',
    # rotate 正向用例的目标 Secret（deploy 预置的 mock 端点密钥，key=api_key）。
    [string]$RotateSecretName = 'tp-secret-endpoint-mock',
    [string]$RotateSecretKey = 'api_key'
)
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$outDir = Join-Path $repo '.data\qa-admin'
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
function JsonOf($Resp) {
    try { return $Resp.Content | ConvertFrom-Json } catch { return $null }
}

$null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5
Write-Host "管理面验收：$BaseUrl（admin=$User）" -ForegroundColor Cyan

$cluster = "e2e-cluster-$stamp"
$profile = "e2e-profile-$stamp"
$secretRef = "e2e-secret-$stamp"

# ---- clusters + profiles CRUD -------------------------------------------
$r = Call POST '/api/v1/admin/clusters' @{ name = $cluster; api_server = 'https://127.0.0.1:6443'; kubeconfig_key = 'kind-testplane'; description = 'e2e admin check' }
$j = JsonOf $r
Add-Check 'cluster.create_201' ($r.StatusCode -eq 201 -and $j.name -eq $cluster) ("status=" + $r.StatusCode + " name=" + $j.name)
Add-Check 'cluster.no_kubeconfig_echo' ($r.Content -notmatch 'BEGIN |apiVersion:|clusters:') '响应不含 kubeconfig 内容'

$r = Call POST '/api/v1/admin/profiles' @{ name = $profile; cluster = $cluster; namespace = $Namespace; deploy_timeout_seconds = 3600; description = 'e2e' }
$j = JsonOf $r
Add-Check 'profile.create_201' ($r.StatusCode -eq 201 -and $j.name -eq $profile -and $j.cluster -eq $cluster) ("status=" + $r.StatusCode)
$r = Call PUT '/api/v1/admin/profiles' @{ name = $profile; cluster = $cluster; namespace = $Namespace; deploy_timeout_seconds = 7200; description = 'e2e-updated' }
$j = JsonOf $r
Add-Check 'profile.update_200' ($r.StatusCode -eq 200 -and $j.deploy_timeout_seconds -eq 7200) ("status=" + $r.StatusCode)

# 被 Profile 引用的 cluster 删除必须 409（负向）
$r = Call DELETE "/api/v1/admin/clusters?name=$cluster"
$j = JsonOf $r
Add-Check 'cluster.delete_referenced_409' ($r.StatusCode -eq 409 -and $j.code -eq 'CONFLICT') ("status=" + $r.StatusCode + " code=" + $j.code)
Add-Check 'cluster.delete_referenced_profiles_detail' (@($j.detail.profiles).Count -ge 1) ("profiles=" + (@($j.detail.profiles) -join ','))

# ---- secrets ------------------------------------------------------------
$r = Call GET '/api/v1/secrets?limit=100'
$j = JsonOf $r
# GET /secrets 是 operator 下界（Lead 裁决），dev 模式 admin 可以读；viewer 403 见 README 已知限制。
Add-Check 'secrets.list_200' ($r.StatusCode -eq 200 -and $null -ne $j.items) ("status=" + $r.StatusCode + " items=" + @($j.items).Count)
$leak = $false
foreach ($it in @($j.items)) {
    foreach ($k in @('namespace', 'secret_name', 'key', 'value')) {
        if ($null -ne $it.$k) { $leak = $true }
    }
}
Add-Check 'secrets.list_no_internal_or_value' (-not $leak) 'GET /secrets 只应有 name/description'

$r = Call POST '/api/v1/admin/secrets' @{ name = $secretRef; namespace = $Namespace; secret_name = "e2e-missing-$stamp"; key = 'token'; description = 'e2e' }
$j = JsonOf $r
Add-Check 'secret.create_201' ($r.StatusCode -eq 201 -and $j.name -eq $secretRef) ("status=" + $r.StatusCode)
Add-Check 'secret.create_no_value_echo' ($r.Content -notmatch '"value"|"secret_value"|"data"') '响应无 Secret 值字段'

$r = Call POST "/api/v1/admin/secrets/$secretRef/rotate" @{}
$j = JsonOf $r
# 目标 Secret 不存在：正确结果 404；若 D-55 的窄授权没 apply，只能是 503 且必须带 reason
# （评审 #5：不接受"裸 503"，也不接受把授权缺失当成正常）。
$rotateMissingOK = (($r.StatusCode -eq 404 -and $j.code -eq 'NOT_FOUND') -or
                    ($r.StatusCode -eq 503 -and $j.code -eq 'EXECUTOR_UNAVAILABLE' -and $j.detail.reason -in @('forbidden', 'k8s_unavailable')))
Add-Check 'secret.rotate_missing_negative' $rotateMissingOK ("status=" + $r.StatusCode + " code=" + $j.code + " reason=" + $j.detail.reason)

# ---- rotate 真集群正向：部署 SA + D-55 窄授权必须能返回 verified ----------
# 走的是控制面 Pod 的 ServiceAccount（不是本机 admin kubeconfig）；
# Role 没 apply 时这里会红——这正是要检出的问题。
$rotateRef = "e2e-rotate-$stamp"
$r = Call POST '/api/v1/admin/secrets' @{ name = $rotateRef; namespace = $Namespace; secret_name = $RotateSecretName; key = $RotateSecretKey; description = 'e2e rotate verified' }
Add-Check 'secret.rotate_ref_create_201' ($r.StatusCode -eq 201) ("status=" + $r.StatusCode)
$r = Call POST "/api/v1/admin/secrets/$rotateRef/rotate" @{}
$j = JsonOf $r
$rotateVerifiedOK = ($r.StatusCode -eq 200 -and $j.rotated -eq $true -and $j.verified -eq $true)
Add-Check 'secret.rotate_verified_200' $rotateVerifiedOK ("status=" + $r.StatusCode + " verified=" + $j.verified + " reason=" + $j.detail.reason + "（要求 D-55 的 resourceNames Role 已 apply）")

$r = Call GET '/api/v1/admin/audit?limit=100'
$j = JsonOf $r
$actions = @($j.items | ForEach-Object { $_.action })
Add-Check 'audit.list_200' ($r.StatusCode -eq 200 -and @($j.items).Count -gt 0) ("status=" + $r.StatusCode + " items=" + @($j.items).Count)
Add-Check 'audit.has_admin_writes' (($actions -contains 'register') -and ($actions -contains 'secret_rotated')) ("actions=" + (($actions | Select-Object -Unique) -join ','))
$rejected = @($j.items | Where-Object { $_.action -eq 'secret_rotated' -and $_.detail.result -eq 'rejected' }).Count
Add-Check 'audit.rotate_rejected_recorded' ($rejected -ge 1) ("secret_rotated rejected=" + $rejected)
$verified = @($j.items | Where-Object { $_.action -eq 'secret_rotated' -and $_.detail.result -eq 'ok' -and $_.detail.verified -eq $true }).Count
Add-Check 'audit.rotate_verified_recorded' ($verified -ge 1) ("secret_rotated verified=true 记录=" + $verified)


# ---- 人工清理产物：非终态 409 / 终态 200 + 列表清空 --------------------
$slowManifest = @{
    run = @{
        name = "admin-slow-$stamp"; service = @{ url = $SlowMockURL; model_name = 'mock-llm' }; precheck = 'off';
        cleanup = @{ policy = 'never' };
        tests = @(@{ name = 'slow-a'; tester = 'endpoint-probe'; version = '1.0.0'; execution = @{ profile = 'k8s-npu' }; args = '--wait_timeout_seconds=30 --retry_interval_seconds=1' })
    }
} | ConvertTo-Json -Depth 8 -Compress
$r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/runs" -Headers $headers -ContentType 'application/json' -Body $slowManifest
$slowRun = (JsonOf $r).run.id
Add-Check 'cleanup.nonterminal_submit' ($r.StatusCode -eq 202 -and $slowRun) ("run=" + $slowRun)
if ($slowRun) {
    $r = Call DELETE "/api/v1/admin/runs/$slowRun/artifacts"
    $j = JsonOf $r
    Add-Check 'cleanup.nonterminal_409' ($r.StatusCode -eq 409 -and $j.code -eq 'CONFLICT') ("status=" + $r.StatusCode + " code=" + $j.code)
    $null = Call POST "/api/v1/runs/$slowRun/cancel"
}

$manifest = @{
    run = @{
        name = "admin-artifact-$stamp"; service = @{ url = $MockURL; model_name = 'mock-llm' }; precheck = 'off';
        cleanup = @{ policy = 'never' };
        tests = @(@{ name = 'artifact-a'; tester = 'endpoint-probe'; version = '1.0.0'; execution = @{ profile = 'k8s-npu' }; args = '--wait_timeout_seconds=10 --retry_interval_seconds=1' })
    }
} | ConvertTo-Json -Depth 8 -Compress
$r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/runs" -Headers $headers -ContentType 'application/json' -Body $manifest
$artifactRun = (JsonOf $r).run.id
Add-Check 'cleanup.terminal_submit' ($r.StatusCode -eq 202 -and $artifactRun) ("run=" + $artifactRun)
if ($artifactRun) {
    $deadline = (Get-Date).AddSeconds(120)
    do {
        Start-Sleep -Seconds 1
        $run = JsonOf (Call GET "/api/v1/runs/$artifactRun")
    } while ($run.status -notin @('succeeded', 'failed', 'completed_with_errors', 'service_unavailable', 'canceled') -and (Get-Date) -lt $deadline)
    Add-Check 'cleanup.run_terminal' ($run.status -in @('succeeded', 'failed', 'completed_with_errors', 'service_unavailable', 'canceled')) ("status=" + $run.status)
    $r = Call DELETE "/api/v1/admin/runs/$artifactRun/artifacts"
    $j = JsonOf $r
    Add-Check 'cleanup.terminal_200' ($r.StatusCode -eq 200) ("status=" + $r.StatusCode)
    $r = Call GET "/api/v1/runs/$artifactRun/artifacts"
    $j = JsonOf $r
    Add-Check 'cleanup.artifacts_empty' ($r.StatusCode -eq 200 -and @($j.items).Count -eq 0) ("items=" + @($j.items).Count)
}

# ---- 清理本次创建的 profile/cluster ------------------------------------
$r = Call DELETE "/api/v1/admin/profiles?name=$profile"
Add-Check 'profile.delete_200' ($r.StatusCode -eq 200) ("status=" + $r.StatusCode)
$r = Call DELETE "/api/v1/admin/clusters?name=$cluster"
Add-Check 'cluster.delete_200' ($r.StatusCode -eq 200) ("status=" + $r.StatusCode)

# rotate 负向已写；profile/cluster 删除后再次删除 404（负向）
$r = Call DELETE "/api/v1/admin/clusters?name=$cluster"
Add-Check 'cluster.delete_missing_404' ($r.StatusCode -eq 404) ("status=" + $r.StatusCode)

$ok = -not ($checks | Where-Object { -not $_.ok })
$out = [pscustomobject]@{
    ok = $ok; cluster = $cluster; profile = $profile; secret_ref = $secretRef;
    artifact_run = $artifactRun; slow_run = $slowRun; checks = $checks
}
$jsonPath = Join-Path $outDir ("result-$stamp.json")
$out | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $jsonPath
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks | Where-Object { $_.ok }).Count, $checks.Count, $(if ($ok) { 'PASS' } else { 'FAIL' }), $jsonPath) -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
if (-not $ok) { exit 1 }
exit 0
