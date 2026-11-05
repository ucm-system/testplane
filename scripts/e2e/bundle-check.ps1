# Bundle 下载验收（issue #41）：zip 魔数 / MANIFEST.json / counts.artifacts 同源 /
# 非终态 Run 的 missing[] 必须显式列出 report.md。
param(
    [string]$BaseUrl = 'http://127.0.0.1:8170',
    [string]$User = 'dev',
    [string]$SlowMockURL = 'http://mock-llm-slow.default.svc:18080/v1',
    [string]$OutDir = ''
)
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) { $OutDir = Join-Path $repo '.data\qa-bundle' }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$checks = @()
function Add-Check([string]$Name, [bool]$Ok, [string]$Detail) {
    $script:checks += [pscustomobject]@{ name = $Name; ok = $Ok; detail = $Detail }
    Write-Host ("  [{0}] {1} —— {2}" -f $(if ($Ok) { 'OK' } else { 'FAIL' }), $Name, $Detail) -ForegroundColor $(if ($Ok) { 'Green' } else { 'Red' })
}
$headers = @{ Authorization = "Bearer $User"; Accept = 'application/json' }
function Call { param([string]$Method, [string]$Path, $Body = $null)
    $uri="$BaseUrl$Path"; if($null -ne $Body){ $json=$Body|ConvertTo-Json -Depth 8 -Compress; return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers -ContentType 'application/json; charset=utf-8' -Body $json }
    return Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method $Method -Uri $uri -Headers $headers
}
function JsonOf($r){ try{return $r.Content|ConvertFrom-Json}catch{return $null} }
Add-Type -AssemblyName System.IO.Compression.FileSystem
function Read-Bundle([string]$RunID) {
    $tmp = Join-Path $OutDir "bundle-$RunID.zip"
    $r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -PassThru -Uri "$BaseUrl/api/v1/runs/$RunID/bundle" -Headers @{ Authorization="Bearer $User" } -OutFile $tmp
    $bytes = [IO.File]::ReadAllBytes($tmp)
    $zip = [IO.Compression.ZipFile]::OpenRead($tmp)
    try {
        $manEntry = $zip.GetEntry('MANIFEST.json')
        $man = $null
        if ($manEntry) {
            $sr = New-Object IO.StreamReader($manEntry.Open())
            try { $man = $sr.ReadToEnd() | ConvertFrom-Json } finally { $sr.Dispose() }
        }
        return [pscustomobject]@{ status=[int]$r.StatusCode; content_type=[string]$r.Headers['Content-Type']; bytes=$bytes; zip=$zip; manifest=$man }
    } catch { $zip.Dispose(); throw }
}
try {
    $null = Invoke-WebRequest -UseBasicParsing "$BaseUrl/healthz" -TimeoutSec 5
    Write-Host "Bundle 验收：$BaseUrl" -ForegroundColor Cyan

    # 终态 Run：优先挑最近有 artifacts 的 succeeded/failed 终态 Run。
    $list = JsonOf (Call GET '/api/v1/runs?limit=50')
    $target = $null
    foreach ($r in @($list.items)) {
        if ($r.status -in @('succeeded','failed','completed_with_errors','service_unavailable','canceled')) {
            $arts = JsonOf (Call GET "/api/v1/runs/$($r.id)/artifacts")
            if (@($arts.items).Count -gt 0) { $target = $r; $targetArtifacts = @($arts.items); break }
        }
    }
    Add-Check 'terminal.found' ($null -ne $target) ("run=" + $(if($target){$target.id}else{'none'}))
    $b = Read-Bundle $target.id
    try {
        $ct = $b.content_type
        Add-Check 'bundle.200' ($b.status -eq 200) ("status=" + $b.status)
        Add-Check 'bundle.content_type' ($ct -like 'application/zip*') ("Content-Type=" + $ct)
        Add-Check 'bundle.zip_magic' ($b.bytes.Length -ge 2 -and $b.bytes[0] -eq 0x50 -and $b.bytes[1] -eq 0x4b) ("bytes=" + $b.bytes.Length)
        Add-Check 'bundle.manifest' ($null -ne $b.manifest -and $b.manifest.run_id -eq $target.id) ("MANIFEST.run_id=" + $b.manifest.run_id)
        Add-Check 'bundle.artifacts_count_matches' ($b.manifest.counts.artifacts -eq $targetArtifacts.Count) ("manifest=" + $b.manifest.counts.artifacts + " /artifacts=" + $targetArtifacts.Count)
        Add-Check 'bundle.terminal_true' ($b.manifest.terminal -eq $true) ("terminal=" + $b.manifest.terminal + " state=" + $b.manifest.state)
    } finally { $b.zip.Dispose() }

    # 非终态 Run：slow mock，bundle 必须 200 且 MANIFEST.missing 显式列 report.md。
    $manifest = @{ run = @{ name = "bundle-nonterminal-$stamp"; service = @{ url=$SlowMockURL; model_name='mock-llm' }; precheck='off'; cleanup=@{policy='never'}; tests=@(@{name='slow-a'; tester='endpoint-probe'; version='1.0.0'; execution=@{profile='k8s-npu'}; args='--wait_timeout_seconds=30 --retry_interval_seconds=1'}) } } | ConvertTo-Json -Depth 8 -Compress
    $r = Invoke-WebRequest -UseBasicParsing -SkipHttpErrorCheck -Method POST -Uri "$BaseUrl/api/v1/runs" -Headers $headers -ContentType 'application/json' -Body $manifest
    $slowID = (JsonOf $r).run.id
    Add-Check 'nonterminal.submit' ($r.StatusCode -eq 202 -and $slowID) ("run=" + $slowID)
    $nb = Read-Bundle $slowID
    try {
        $missing = @($nb.manifest.missing | ForEach-Object { $_.path })
        Add-Check 'nonterminal.bundle_200' ($nb.status -eq 200) ("status=" + $nb.status)
        Add-Check 'nonterminal.terminal_false' ($nb.manifest.terminal -eq $false) ("terminal=" + $nb.manifest.terminal + " state=" + $nb.manifest.state)
        Add-Check 'nonterminal.missing_report_md' ($missing -contains 'report.md') ("missing=" + ($missing -join ','))
    } finally { $nb.zip.Dispose(); $null = Call POST "/api/v1/runs/$slowID/cancel" }
} catch {
    Add-Check 'bundle.exception' $false $_.Exception.Message
}
$ok = -not ($checks | Where-Object { -not $_.ok })
$json = Join-Path $OutDir ("result-$stamp.json")
[pscustomobject]@{ ok=$ok; checks=$checks } | ConvertTo-Json -Depth 8 | Set-Content -Encoding utf8 $json
Write-Host ""
Write-Host ("检查：{0}/{1} 通过；结果：{2}（JSON: {3}）" -f @($checks|Where-Object{$_.ok}).Count, $checks.Count, $(if($ok){'PASS'}else{'FAIL'}), $json) -ForegroundColor $(if($ok){'Green'}else{'Red'})
if(-not $ok){exit 1}
exit 0
