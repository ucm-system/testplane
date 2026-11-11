# Argus · 把宿主镜像灌进 kind 节点（docker save → docker cp → ctr import）
# 比 `kind load docker-image` 稳（kind.exe 拿不到 Docker Desktop 引擎的镜像列表）。
# 用法：pwsh deploy/kind/load-image.ps1 -Image argus-server:dev-logo [-Node argus-control-plane]
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Image,
    [string]$Node = 'argus-control-plane',
    [string]$TarPath = ''
)
$ErrorActionPreference = 'Stop'
if (-not $TarPath) { $TarPath = Join-Path $env:TEMP ('kindload-' + ($Image -replace '[/:]', '_') + '.tar') }

Write-Host "==> docker save $Image"
docker save $Image -o $TarPath
if ($LASTEXITCODE -ne 0) { throw "docker save 失败" }

$inNode = '/loadimg.tar'
Write-Host "==> docker cp -> ${Node}:${inNode}"
docker cp $TarPath "${Node}:$inNode"
if ($LASTEXITCODE -ne 0) { throw "docker cp 失败" }

Write-Host "==> ctr import（k8s.io 命名空间）"
docker exec $Node ctr --namespace=k8s.io images import $inNode
if ($LASTEXITCODE -ne 0) { throw "ctr import 失败" }
docker exec $Node sh -c ("rm -f $inNode")
Remove-Item -Force $TarPath -ErrorAction SilentlyContinue
Write-Host "==> 已导入 $Image"
