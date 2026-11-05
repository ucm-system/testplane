# TestPlane infra · 打包 chart 并部署「假 ChartMuseum」（供 Genet 的 Helm 管理功能拉 chart）
#
# 为什么需要：Genet 的 chartRepo 只支持 harbor-oci（要真 Harbor artifacts API）与
# harbor-chartmuseum（形如 GET /api/chartrepo/<project>/charts[/name[/version]] 与
# GET /chartrepo/<project>/charts/<name>-<version>.tgz）。后者可以用静态 nginx 假冒。
#
# 用法（仓库根执行）：
#   pwsh deploy/genet-local/build-chartrepo.ps1
#
# 打包哪些 chart：
#   1) deploy/charts/mock-llm          —— 我们自己的被测服务 chart
#   2) .data/packages/unified-cache-stack —— 把 mock-llm 改名冒充 Genet 固定 stack chart
#      （Genet 的 Open API 只能装 helm.ucPackages.stack.chart 指定的 chart，
#        要让它「装我们的服务」只能反过来冒充；该目录是本地生成物，不入库）
#
# 产物：ConfigMap/Deployment/Service `genet-chartrepo`（namespace genet）。
#       挂载的 tgz 与列表 JSON 全部按 .data/chartrepo 下实际打出的 tgz 现算。
[CmdletBinding()]
param(
    [string]$Namespace = 'genet',
    [string]$RepoRoot
)

$ErrorActionPreference = 'Stop'
if (-not $RepoRoot) { $RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path }
$helm = Join-Path $RepoRoot '.tools\helm.exe'
if (-not (Test-Path -LiteralPath $helm)) { throw "找不到 $helm（先按 deploy/README.md 第 1 步装 helm）" }

$chartDirs = @(
    (Join-Path $RepoRoot 'deploy\charts\mock-llm')
)
# `.data/packages/*` 是 make-chartrepo.ps1 生成的「冒充件」（Genet 固定 chart 的替身）；
# 目录里有什么就打包什么，脚本不写死具体名字，换冒充目标不用改这里。
$pkgRoot = Join-Path $RepoRoot '.data\packages'
if (Test-Path -LiteralPath $pkgRoot) {
    $chartDirs += @(Get-ChildItem -LiteralPath $pkgRoot -Directory | ForEach-Object { $_.FullName })
}
$outDir = Join-Path $RepoRoot '.data\chartrepo'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
Get-ChildItem $outDir -Filter '*.tgz' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

Write-Host '== 1/3 打包 chart =='
$packed = @()
foreach ($dir in $chartDirs) {
    if (-not (Test-Path -LiteralPath $dir)) { Write-Host "   跳过（不存在）：$dir"; continue }
    & $helm package $dir -d $outDir | Write-Host
    $tgz = Get-ChildItem $outDir -Filter '*.tgz' | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    # 文件名形如 <name>-<version>.tgz；version 从 Chart.yaml 读，避免正则猜错
    $cy = Get-Content (Join-Path $dir 'Chart.yaml') -Raw
    $name = ([regex]::Match($cy, '(?m)^name:\s*(\S+)')).Groups[1].Value
    $ver = ([regex]::Match($cy, '(?m)^version:\s*(\S+)')).Groups[1].Value
    if ($tgz.BaseName -ne "$name-$ver") { throw "打包产物名与 Chart.yaml 不一致：$($tgz.BaseName) vs $name-$ver" }
    $packed += [pscustomobject]@{ File = $tgz; Name = $name; Version = $ver }
}
if (-not $packed) { throw '没有打包出任何 chart' }
$packed | ForEach-Object { Write-Host "   $($_.Name) $($_.Version)  ($($_.File.Name), $($_.File.Length) B)" }

Write-Host '== 2/3 生成清单 =='
$entries = @(); $binary = @(); $mounts = @(); $items = @()
foreach ($p in $packed) {
    $urlPath = "chartrepo/charts/charts/$($p.File.Name)"
    $entries += "  {`n    `"name`": `"$($p.Name)`",`n    `"version`": `"$($p.Version)`",`n    `"urls`": [`"$urlPath`"]`n  }"
    $binary += "  $($p.File.Name): `"$([Convert]::ToBase64String([IO.File]::ReadAllBytes($p.File.FullName)))`""
    $mounts += "            - { name: html, mountPath: /usr/share/nginx/html/$urlPath, subPath: $($p.File.Name) }"
    $items += "              - { key: $($p.File.Name), path: $($p.File.Name) }"
}
$chartsJson = "[`n" + ($entries -join ",`n") + "`n]"

$manifest = @"
apiVersion: v1
kind: ConfigMap
metadata:
  name: genet-chartrepo
  namespace: $Namespace
data:
  nginx.conf: |
    server {
      listen 80;
      server_name _;
      root /usr/share/nginx/html;
      location / { }
      location ^~ /api/ { default_type application/json; }
    }
  charts: |
$(($chartsJson -split "`n" | ForEach-Object { "    $_" }) -join "`n")
binaryData:
$($binary -join "`n")
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: genet-chartrepo
  namespace: $Namespace
  labels: { app: genet-chartrepo }
spec:
  replicas: 1
  selector:
    matchLabels: { app: genet-chartrepo }
  template:
    metadata:
      labels: { app: genet-chartrepo }
    spec:
      automountServiceAccountToken: false
      containers:
        - name: nginx
          image: nginx:alpine
          imagePullPolicy: IfNotPresent
          ports: [{ containerPort: 80, name: http }]
          readinessProbe:
            httpGet: { path: /api/chartrepo/charts/charts, port: http }
            initialDelaySeconds: 2
            periodSeconds: 5
          volumeMounts:
            - { name: conf, mountPath: /etc/nginx/conf.d/default.conf, subPath: nginx.conf }
            - { name: html, mountPath: /usr/share/nginx/html/api/chartrepo/charts/charts, subPath: charts }
$($mounts -join "`n")
          resources:
            requests: { cpu: 10m, memory: 16Mi }
            limits: { cpu: 100m, memory: 64Mi }
      volumes:
        - name: conf
          configMap: { name: genet-chartrepo, items: [{ key: nginx.conf, path: nginx.conf }] }
        - name: html
          configMap:
            name: genet-chartrepo
            items:
              - { key: charts, path: charts }
$($items -join "`n")
---
apiVersion: v1
kind: Service
metadata:
  name: genet-chartrepo
  namespace: $Namespace
  labels: { app: genet-chartrepo }
spec:
  type: ClusterIP
  selector: { app: genet-chartrepo }
  ports:
    - name: http
      port: 80
      targetPort: http
"@

$manifestPath = Join-Path $RepoRoot '.data\genet-local\chartrepo.rendered.yaml'
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $manifestPath) | Out-Null
[IO.File]::WriteAllText($manifestPath, $manifest, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "   → $manifestPath"

Write-Host '== 3/3 apply + 重启（ConfigMap 内容变了要重建 Pod 才生效）=='
& kubectl --context kind-testplane apply -f $manifestPath | Write-Host
& kubectl --context kind-testplane -n $Namespace rollout restart deploy/genet-chartrepo | Write-Host
& kubectl --context kind-testplane -n $Namespace rollout status deploy/genet-chartrepo --timeout=120s | Write-Host

Write-Host ''
Write-Host '自检：'
Write-Host "  kubectl --context kind-testplane -n $Namespace exec deploy/genet-chartrepo -- wget -qO- http://127.0.0.1/api/chartrepo/charts/charts"
Write-Host '  Genet 的 chart 缓存 TTL 300s；想立刻刷新就重启 genet-backend：'
Write-Host "  kubectl --context kind-testplane -n $Namespace rollout restart deploy/genet-backend"
