# 生成设计端点基线（count + 端点集合 sha256）。
# 用法：pwsh tools/contract/design-baseline.ps1
# 设计文档变化时必须显式重跑并 review diff；CI 只校验，不自动改基线。
param(
    [string]$OutFile = (Join-Path $PSScriptRoot 'design-baseline.txt')
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Push-Location (Join-Path $root 'tools\contract')
try {
    $lines = & go run . --print-design-baseline
    if ($LASTEXITCODE -ne 0) { throw "contract --print-design-baseline 失败（exit $LASTEXITCODE）" }
    $body = @(
        '# 设计端点基线（反向锚点）',
        '# 生成：pwsh tools/contract/design-baseline.ps1；校验：contract --mode spec',
        '# 统计：design/views/07-api.md §5 端点表 + §2.4 认证表 + §5.5 运维段。',
        '# 删除/新增设计端点后本文件必须显式更新，且 diff 要 review。',
        ''
    ) + @($lines)
    Set-Content -LiteralPath $OutFile -Value $body -Encoding utf8
    Write-Host "写入 $OutFile"
    $lines | ForEach-Object { Write-Host "  $_" }
} finally {
    Pop-Location
}
