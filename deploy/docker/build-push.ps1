<#
.SYNOPSIS
    构建 + 打标 + 推送 TestPlane 控制面镜像（运维与 CI 共用一条命令）。

.DESCRIPTION
    只走 deploy/docker/server.Dockerfile 这一个构建入口（W-8 / web-and-delivery §7、§8）。
    默认**同时**打两个 tag：

        <registry>/<image>:<tag>
        <registry>/<image>:sha-<12 位 git sha>      # 回滚按 sha 拉（W-8）

    digest 在推送后打印（Helm chart 的 image.digest 就填它）。

    多架构（-Platforms 含逗号）走 `docker buildx build --platform ... --push`；
    单架构走 `docker build` + `docker push`（本机 kind 也能直接用）。

    kind 的坑：Docker Desktop（containerd image store）+ buildx manifest list 会让
    `kind load docker-image` 失效，所以 -KindCluster 时走 `docker save → docker cp →
    ctr -n k8s.io images import`（不依赖 buildx 的本地镜像导出）。

.PARAMETER Registry
    镜像仓库前缀，默认 ghcr.io/ucm-system（GitHub Container Registry）。
    其它 registry 也支持，例如 `-Registry ccr.ccs.tencentyun.com/keriko`（腾讯云 CCR）。

    登录：
      GHCR（CI，repo 为 public 时用内置 token）：
        docker login ghcr.io -u ${{ github.actor }} -p ${{ secrets.GITHUB_TOKEN }}
      GHCR（本地）：
        gh auth token | docker login ghcr.io -u <你的 GitHub 用户名> --password-stdin
        （token 需 `write:packages`）
      CCR（可选）：docker login ccr.ccs.tencentyun.com

.PARAMETER Image
    镜像名。默认按 -Component 推导：testplane-server / testplane-web / testplane-tester-<name>。

.PARAMETER Tag
    主 tag；默认 `git describe --tags --always --dirty`，取不到时 `dev-<yyyymmdd>`。

.PARAMETER Platforms
    目标平台，逗号分隔，默认 linux/amd64（例如 linux/amd64,linux/arm64）。

.PARAMETER Push
    推送开关。不传 = 只构建（PR 校验用）。

.PARAMETER NoCache
    构建时不使用缓存（--no-cache / --no-cache-filter）。

.PARAMETER KindCluster
    额外的 kind 集群名：构建完成后把镜像导入该集群（本地验证用）。

.EXAMPLE
    pwsh deploy/docker/build-push.ps1 -Push

.EXAMPLE
    pwsh deploy/docker/build-push.ps1 -Component web -Tag v0.2.0 -Push

.EXAMPLE
    pwsh deploy/docker/build-push.ps1 -Component tester:endpoint-probe

.EXAMPLE
    pwsh deploy/docker/build-push.ps1 -Platforms linux/amd64,linux/arm64 -Push -Tag v0.2.0

.EXAMPLE
    # 本地 kind 验证：构建 amd64 后导入集群
    pwsh deploy/docker/build-push.ps1 -KindCluster testplane
#>
[CmdletBinding()]
param(
    [string]$Registry = 'ghcr.io/ucm-system',
    [string]$Image = '',
    [string]$Component = 'server',
    [string]$Tag = '',
    [string]$Platforms = 'linux/amd64',
    [switch]$Push,
    [switch]$NoCache,
    [string]$KindCluster = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# 仓库根 = 本脚本往上两级（deploy/docker/build-push.ps1）
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path

# ---- 组件解析：server / web / tester:<name> -------------------------------
# 三者的 Dockerfile 与**构建上下文**不同，这里集中决定（tester 的上下文是 Tester 目录，
# 不是仓库根——搞错会把整个仓库塞进镜像上下文）。
$ComponentSpec = $Component.Trim()
$BuildContext = $RepoRoot
switch -Regex ($ComponentSpec) {
    '^server$' {
        $Dockerfile = Join-Path $RepoRoot 'deploy/docker/server.Dockerfile'
        $DefaultImage = 'testplane-server'
    }
    '^web$' {
        $Dockerfile = Join-Path $RepoRoot 'deploy/docker/web.Dockerfile'
        $DefaultImage = 'testplane-web'
    }
    '^tester:(?<name>[\w.-]+)$' {
        $testerName = $Matches['name']
        $testerDir = Join-Path $RepoRoot "testers/$testerName"
        if (-not (Test-Path $testerDir)) {
            throw "找不到 Tester 目录：$testerDir"
        }
        $Dockerfile = Join-Path $testerDir 'build/Dockerfile'
        $BuildContext = $testerDir          # <-- 与 server/web 不同：上下文 = Tester 根目录
        $DefaultImage = "testplane-tester-$testerName"
    }
    default {
        throw "-Component 只能是 server | web | tester:<name>（收到 '$Component'）"
    }
}
if (-not $Image) { $Image = $DefaultImage }
if (-not (Test-Path $Dockerfile)) {
    throw "找不到 Dockerfile：$Dockerfile"
}

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Info([string]$msg) { Write-Host "    $msg" -ForegroundColor DarkGray }

# ---- git 元信息（构建前打印；dirty 只用于 tag 的默认值）-------------------
function Get-GitInfo {
    $info = [ordered]@{ Sha = ''; ShortSha = ''; Branch = ''; Dirty = $false; Describe = '' }
    # 优先用 git（CI 上一定有）；本机无 git 时退化为直接读 .git
    try {
        $info.Sha = (& git -C $RepoRoot rev-parse HEAD 2>$null).Trim()
        $info.Branch = (& git -C $RepoRoot rev-parse --abbrev-ref HEAD 2>$null).Trim()
        $status = & git -C $RepoRoot status --porcelain 2>$null
        $info.Dirty = [bool]($status | Where-Object { $_ })
        $info.Describe = (& git -C $RepoRoot describe --tags --always --dirty 2>$null).Trim()
    } catch {
        Write-Warning "git 不可用，退回读 .git 目录：$($_.Exception.Message)"
    }
    if (-not $info.Sha -and (Test-Path (Join-Path $RepoRoot '.git/HEAD'))) {
        $head = (Get-Content (Join-Path $RepoRoot '.git/HEAD') -Raw).Trim()
        if ($head -match '^ref:\s*(.+)$') {
            $ref = $Matches[1].Trim()
            $info.Branch = $ref -replace '^refs/heads/', ''
            $refFile = Join-Path $RepoRoot (Join-Path '.git' $ref)
            if (Test-Path $refFile) { $info.Sha = (Get-Content $refFile -Raw).Trim() }
        } else {
            $info.Sha = $head
            $info.Branch = '(detached)'
        }
    }
    if ($info.Sha) { $info.ShortSha = $info.Sha.Substring(0, [Math]::Min(12, $info.Sha.Length)) }
    if (-not $info.Describe -and $info.ShortSha) { $info.Describe = $info.ShortSha }
    return [pscustomobject]$info
}

$gitInfo = Get-GitInfo

$Tag = if ($Tag) { $Tag } elseif ($gitInfo.Describe) { $gitInfo.Describe } else { 'dev-' + (Get-Date -Format 'yyyyMMdd') }
if (-not $gitInfo.ShortSha) {
    throw '取不到 git sha：无法打 sha-<12> tag（W-8 要求回滚能按 sha 拉）。请在 git 仓库内运行，或用 -Tag 显式指定并把 sha 传进来。'
}
$ShaTag = "sha-$($gitInfo.ShortSha)"

$RefTag = "$Registry/$($Image):$Tag"
$RefSha = "$Registry/$($Image):$ShaTag"
# kind 流程用 `:kind-local`（values-kind.yaml 钉的就是它）：
# -KindCluster 时额外打一个本地 tag，保证"照 README 敲一条命令就能跑起来"，不用再手工 --set image.tag。
$KindLocalRef = if ($KindCluster) { "$Registry/$($Image):kind-local" } else { $null }

$platformList = @($Platforms.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ })
if ($platformList.Count -eq 0) { throw '-Platforms 不能为空' }
$multiArch = $platformList.Count -gt 1
$hasArm = $platformList -contains 'linux/arm64'

Write-Step "git"
Write-Info "sha    : $($gitInfo.Sha)"
Write-Info "short  : $($gitInfo.ShortSha)"
Write-Info "branch : $($gitInfo.Branch)"
Write-Info "dirty  : $($gitInfo.Dirty)"
Write-Info "describe: $($gitInfo.Describe)"

Write-Step "构建目标"
Write-Info "component  : $ComponentSpec"
Write-Info "image      : $Image"
Write-Info "dockerfile : $Dockerfile"
Write-Info "context    : $BuildContext"
Write-Info "platforms  : $($platformList -join ', ')"
Write-Info "tag        : $RefTag"
Write-Info "sha tag    : $RefSha"
Write-Info "push       : $([bool]$Push)"
Write-Info "no-cache   : $([bool]$NoCache)"

if ($multiArch -and -not $Push) {
    throw '多架构构建必须带 -Push：buildx 无法把 manifest list 载入本地 docker（kind 用单架构）。'
}
if ($multiArch -and $KindCluster) {
    throw '-KindCluster 需要单架构镜像（kind 节点只跑宿主架构）；请用 -Platforms linux/amd64。'
}

$commonArgs = @('--file', $Dockerfile)
if ($NoCache) { $commonArgs += '--no-cache' }

# ---- 构建 ----------------------------------------------------------------
$sw = [System.Diagnostics.Stopwatch]::StartNew()
if ($multiArch) {
    Write-Step "docker buildx build（多架构 + 直接推送）"
    $args = @('buildx', 'build', '--platform', ($platformList -join ','), '--provenance=false', '--sbom=false') +
            $commonArgs +
            @('--tag', $RefTag, '--tag', $RefSha, '--push', $BuildContext)
    Write-Info ("docker " + ($args -join ' '))
    & docker @args
    if ($LASTEXITCODE -ne 0) { throw "buildx build 失败（exit $LASTEXITCODE）" }
} else {
    Write-Step "docker build（单架构 $($platformList[0])）"
    # --provenance=false --sbom=false：否则 buildx 会额外塞一个 unknown/unknown 的
    # attestation manifest，`docker manifest inspect` 与部分集群的校验会看着别扭。
    $args = @('build', '--platform', $platformList[0], '--provenance=false', '--sbom=false') + $commonArgs +
            @('--tag', $RefTag, '--tag', $RefSha, $BuildContext)
    if ($KindLocalRef) { $args = $args[0..($args.Count - 2)] + @('--tag', $KindLocalRef, $args[-1]) }
    Write-Info ("docker " + ($args -join ' '))
    & docker @args
    if ($LASTEXITCODE -ne 0) { throw "docker build 失败（exit $LASTEXITCODE）" }

    if ($Push) {
        Write-Step "docker push（两个 tag）"
        foreach ($ref in @($RefTag, $RefSha)) {
            & docker push $ref
            if ($LASTEXITCODE -ne 0) { throw "docker push $ref 失败（exit $LASTEXITCODE）" }
        }
    }
}
$sw.Stop()

# ---- digest + 大小 -------------------------------------------------------
$digest = ''
if ($Push) {
    Write-Step "image digest（Helm chart 的 image.digest 填这个）"
    # 多架构：manifest list 的 digest（imagetools）；单架构：本地 RepoDigests
    if ($multiArch) {
        $out = & docker buildx imagetools inspect $RefSha 2>$null
        $line = $out | Select-String -Pattern '^Digest:\s+(sha256:[0-9a-f]+)$' | Select-Object -First 1
        if ($line -and $line.Matches.Count -gt 0) { $digest = $line.Matches[0].Groups[1].Value }
    } else {
        $out = & docker inspect --format '{{index .RepoDigests 0}}' $RefSha 2>$null
        if ($out) { $digest = ($out -split '@')[-1].Trim() }
    }
    if ($digest) {
        Write-Host "digest: $digest" -ForegroundColor Green
        Write-Host "引用（tag）   : $RefTag"
        Write-Host "引用（sha）   : $RefSha"
        Write-Host "引用（digest）: $Registry/$($Image)@$digest"
    } else {
        Write-Warning '没拿到 digest：检查 `docker inspect --format "{{index .RepoDigests 0}}"` / `docker buildx imagetools inspect`'
    }
    $size = & docker image inspect --format '{{.Size}}' $RefSha 2>$null
    if ($size) {
        $mb = [math]::Round(([double]$size) / 1MB, 1)
        Write-Host ("大小: {0} MB" -f $mb)
    }
} else {
    Write-Step "跳过推送（未传 -Push）：镜像只在本地 docker"
    $size = & docker image inspect --format '{{.Size}}' $RefSha 2>$null
    if ($size) { Write-Host ("大小: {0} MB" -f ([math]::Round(([double]$size) / 1MB, 1))) }
}

Write-Host ("耗时: {0:n1}s" -f $sw.Elapsed.TotalSeconds) -ForegroundColor DarkGray

# ---- kind 导入（可选）----------------------------------------------------
# `kind load docker-image` 在本机失效（Docker Desktop 用 containerd image store，
# 且 buildx 产出的是 manifest list，kind 看不到本地镜像）。改为：
#   docker save → docker cp → ctr -n k8s.io images import（走 containerd 的 import）
if ($KindCluster) {
    if ($KindLocalRef) { Write-Info "kind-local tag: $KindLocalRef（values-kind.yaml 默认就钉这个）" }
    Write-Step "导入 kind 集群：$KindCluster"
    $node = "$KindCluster-control-plane"
    & docker inspect $node *> $null
    if ($LASTEXITCODE -ne 0) { throw "找不到 kind 节点容器 $node（先 kind create cluster --name $KindCluster）" }

    $tar = Join-Path ([System.IO.Path]::GetTempPath()) ("$Image-kind-" + [guid]::NewGuid().ToString('N') + ".tar")
    try {
        Write-Info "docker save -o $tar $RefSha"
        & docker save -o $tar $RefSha
        if ($LASTEXITCODE -ne 0) { throw "docker save 失败（exit $LASTEXITCODE）" }

        $nodeTar = "/tmp/" + (Split-Path $tar -Leaf)
        Write-Info "docker cp $tar ${node}:$nodeTar"
        & docker cp $tar "${node}:$nodeTar"
        if ($LASTEXITCODE -ne 0) { throw "docker cp 失败（exit $LASTEXITCODE）" }

        Write-Info "ctr -n k8s.io images import $nodeTar"
        & docker exec $node ctr -n k8s.io images import $nodeTar
        if ($LASTEXITCODE -ne 0) { throw "ctr images import 失败（exit $LASTEXITCODE）" }

        & docker exec $node ctr -n k8s.io images rm "docker.io/library/$($Image):$Tag" *> $null
        Write-Host "已导入 $node：$RefSha（Pods 用 imagePullPolicy: IfNotPresent 即可）" -ForegroundColor Green
    } finally {
        if (Test-Path $tar) { Remove-Item $tar -Force -ErrorAction SilentlyContinue }
        & docker exec $node rm -f "/tmp/$(Split-Path $tar -Leaf)" *> $null
    }
}

# ---- 给 CI 用的机器可读输出 ---------------------------------------------
Write-Host ''
Write-Host "COMPONENT=$ComponentSpec"
Write-Host "DOCKERFILE=$Dockerfile"
Write-Host "CONTEXT=$BuildContext"
Write-Host "TAG=$Tag"
Write-Host "SHA_TAG=$ShaTag"
Write-Host "REF_TAG=$RefTag"
Write-Host "REF_SHA=$RefSha"
Write-Host "DIGEST=$digest"
Write-Host "PLATFORMS=$($platformList -join ',')"
Write-Host "PUSHED=$([bool]$Push)"

if ($hasArm) {
    Write-Host '  注意：本次含 linux/arm64；仓库里所有 Deploy 的 Pod 若无 arm 节点会把 arm 拉失败，按需在 chart 里限定 nodeSelector。' -ForegroundColor DarkYellow
}
