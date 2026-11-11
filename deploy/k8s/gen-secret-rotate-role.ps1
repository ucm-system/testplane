<#
.SYNOPSIS
  生成 D-55 的窄授权清单：控制面 SA 只对**一条**已登记 Secret 有 get 权限。

.DESCRIPTION
  `POST /admin/secrets/{name}/rotate` 需要确认目标 Secret 存在且 key 非空（C-10、D-55）。
  `06-physical-view.md §4.4` 仍然**不授予**通配的 `secrets get/list/watch`；
  唯一例外是 `resourceNames` 限定到 `secret_refs` 里登记的那一条 Secret 的 `get`。

  本脚本按「登记项」生成一对 Role + RoleBinding（Role 在**目标 Secret 所在的 namespace**，
  RoleBinding 的 subject 是**控制面 SA**）。不要手写 YAML：resourceNames 必须与
  `secret_refs.secret_name` 一致，生成脚本会做 DNS-1123 校验并在名字过长时加哈希。

.EXAMPLE
  # 本地 kind（kustomize 安装，SA = argus）：为 mock 端点 Secret 生成并 apply
  pwsh deploy/k8s/gen-secret-rotate-role.ps1 `
    -Namespace argus -SecretName argus-secret-endpoint-mock `
    -ServiceAccount argus `
    -Out deploy/k8s/rbac-secret-rotate.yaml
  kubectl --context kind-argus apply -f deploy/k8s/rbac-secret-rotate.yaml

.EXAMPLE
  # 只打印到 stdout（生产按登记项逐个生成、走 GitOps）；注意必须显式给 -ServiceAccount
  pwsh deploy/k8s/gen-secret-rotate-role.ps1 -Namespace argus -SecretName prod-qwen-key `
    -ServiceAccount argus
#>
param(
    # Secret 所在的 namespace（等于 secret_refs.namespace）。
    [Parameter(Mandatory = $true)][string]$Namespace,
    # 已登记的 Secret 名（等于 secret_refs.secret_name）；resourceNames 只放这一个。
    [Parameter(Mandatory = $true)][string]$SecretName,
    # 控制面 ServiceAccount 名（**必填**，不给就报错）。
    #
    # 为什么不给默认值：两种安装方式的 SA 名不同——
    #   kustomize（deploy/k8s/serviceaccount.yaml）：argus
    #   Helm（deploy/charts/argus）       ：<release>-argus（release = helm install 的名字）
    # 默认成 argus 会让 Helm 装的环境"生成成功、rotate 却 403/503"（评审 #40-3）。
    [string]$ServiceAccount,
    # 控制面 ServiceAccount 所在 namespace。
    [string]$ServiceAccountNamespace = 'argus',
    # 输出文件；省略则打印到 stdout。
    [string]$Out
)

$ErrorActionPreference = 'Stop'

function Assert-Dns1123([string]$Value, [string]$What, [int]$MaxLen) {
    if ([string]::IsNullOrWhiteSpace($Value)) { throw "$What 不能为空" }
    if ($Value.Length -gt $MaxLen) { throw "$What 过长（> $MaxLen）：$Value" }
    if ($Value -notmatch '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$') {
        throw "$What 必须是 DNS-1123 label（小写字母/数字/'-'）：$Value"
    }
}

if ([string]::IsNullOrWhiteSpace($ServiceAccount)) {
    throw @"
-ServiceAccount 必填：RoleBinding 的 subject 必须与控制面实际 SA 名一致，两种安装方式不同：
    kustomize（deploy/k8s/serviceaccount.yaml）：-ServiceAccount argus
    Helm（deploy/charts/argus）          ：-ServiceAccount <release>-argus
  Helm 装的环境按下面这条查实际名字（<release> 换成 helm install 时用的名字）：
    kubectl -n argus get sa -l app.kubernetes.io/instance=<release> -o name
"@
}

Assert-Dns1123 $Namespace 'Namespace' 63
Assert-Dns1123 $ServiceAccount 'ServiceAccount' 63
Assert-Dns1123 $ServiceAccountNamespace 'ServiceAccountNamespace' 63
# Secret 名是 DNS-1123 subdomain（允许点）。
if ($SecretName.Length -gt 253 -or $SecretName -notmatch '^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$') {
    throw "SecretName 必须是 DNS-1123 subdomain（小写字母/数字/'-'/'.'）：$SecretName"
}

# Role 名要可重复生成（确定性）：argus-secret-rotate-<sanitized>[-<hash>]。
$slug = ($SecretName.ToLower() -replace '[^a-z0-9]+', '-').Trim('-')
$prefix = 'argus-secret-rotate'
$roleName = "$prefix-$slug"
if ($roleName.Length -gt 63) {
    # 名字过长时截断 + 原始 SecretName 的短哈希，保证同一输入稳定生成同一名字。
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $hash = -join ($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($SecretName))[0..3] |
        ForEach-Object { $_.ToString('x2') })
    $keep = 63 - $prefix.Length - 1 - $hash.Length - 1
    $roleName = "$prefix-" + $slug.Substring(0, [Math]::Min($keep, $slug.Length)) + "-$hash"
}

$labelBlock = @"
    app.kubernetes.io/name: argus
    app.kubernetes.io/part-of: argus
    argus.io/component: secret-rotate
"@

$yaml = @"
# 由 deploy/k8s/gen-secret-rotate-role.ps1 生成，**不要手改**（改参数后重新生成）。
# 设计依据：06-physical-view.md §4.4、ARCHITECTURE.md D-55。
# 范围：只授 get，只对 resourceNames 里这一条 Secret；不授 list/watch。
# 目标 Secret：$Namespace/$SecretName
# 控制面 SA：$ServiceAccountNamespace/$ServiceAccount
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: $roleName
  namespace: $Namespace
  labels:
$labelBlock
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["$SecretName"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $roleName
  namespace: $Namespace
  labels:
$labelBlock
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: $roleName
subjects:
  - kind: ServiceAccount
    name: $ServiceAccount
    namespace: $ServiceAccountNamespace
"@

if ($Out) {
    $dir = Split-Path $Out -Parent
    if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    Set-Content -Path $Out -Value $yaml -Encoding utf8
    Write-Host "已生成 $Out（Role/RoleBinding=$roleName，namespace=$Namespace，secret=$SecretName）"
} else {
    Write-Output $yaml
}
