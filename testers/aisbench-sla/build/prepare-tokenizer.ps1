#!/usr/bin/env pwsh
# 把本机 D:\Models\Qwen3-0.6B 的 tokenizer 文件拷到 build/models/，
# 供 build/Dockerfile.aisbench 的 COPY 使用（**仅本地/kind 验证镜像**）。
#
#   pwsh testers/aisbench-sla/build/prepare-tokenizer.ps1
#   pwsh testers/aisbench-sla/build/prepare-tokenizer.ps1 -Source D:\Models\Qwen3-0.6B
#
# 生产镜像不带 tokenizer：Profile 挂载它，调用方用 --model_path 指容器内路径。
[CmdletBinding()]
param(
    [string]$Source = 'D:\Models\Qwen3-0.6B',
    [string]$Name = 'Qwen3-0.6B'
)

$ErrorActionPreference = 'Stop'
if (-not (Test-Path -LiteralPath $Source)) { throw "tokenizer 目录不存在：$Source" }

$dest = Join-Path $PSScriptRoot "models/$Name"
New-Item -ItemType Directory -Force -Path $dest | Out-Null

# 只需要 tokenizer 相关文件；权重（*.safetensors / *.bin）一概不带。
$keep = @('tokenizer.json', 'tokenizer_config.json', 'vocab.json', 'merges.txt',
          'config.json', 'generation_config.json', 'special_tokens_map.json',
          'added_tokens.json', 'chat_template.json', 'chat_template.jinja')
foreach ($file in $keep) {
    $src = Join-Path $Source $file
    if (Test-Path -LiteralPath $src) { Copy-Item -LiteralPath $src -Destination $dest -Force }
}

$bytes = (Get-ChildItem -LiteralPath $dest -File | Measure-Object -Property Length -Sum).Sum
if (-not $bytes) { throw "没有拷到任何 tokenizer 文件（源目录：$Source）" }
Write-Host ("→ {0}（{1:N1} MB）" -f $dest, ($bytes / 1MB))
