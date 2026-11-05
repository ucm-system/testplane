# gofmt-check.ps1 —— 把工作树里的 Go 文件复制成 **LF 形态**再跑 gofmt，绕开 Windows CRLF 假阳性。
#
# 为什么不用 `gofmt -l .`：`core.autocrlf=true` 时工作树是 CRLF，gofmt 把每一行都当需要重写，
# 于是全部文件都"未格式化"。而 `.gitattributes` 规定仓库内是 LF，所以这里先把内容统一成 LF
# （临时副本），再在副本上跑 `gofmt -l` —— 结论与"把仓库 clone 到 Linux 上跑 gofmt"一致。
#
# 用法：
#   pwsh scripts/gofmt-check.ps1          # 检查（CI 门禁用）
#   pwsh scripts/gofmt-check.ps1 -Fix     # 检查并对工作树里命中的文件 gofmt -w
param(
    [switch]$Fix
)

$ErrorActionPreference = 'Stop'
$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..')
Push-Location $repoRoot
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("tp-gofmt-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $root = (Get-Location).Path
    $files = Get-ChildItem -Recurse -Filter '*.go' -File |
        Where-Object { $_.FullName -notmatch '[\\/](\.data|\.tools|node_modules|\.git)[\\/]' }
    if (-not $files) { Write-Host '没有 Go 文件'; exit 0 }

    $map = @{}   # 临时副本路径 -> 工作树相对路径
    foreach ($f in $files) {
        $rel = ([System.IO.Path]::GetRelativePath($root, $f.FullName)) -replace '\\', '/'
        $text = [System.IO.File]::ReadAllText($f.FullName)
        $lf = $text -replace "`r`n", "`n"
        $dest = Join-Path $tmp $rel
        $dir = Split-Path $dest -Parent
        if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
        [System.IO.File]::WriteAllText($dest, $lf, (New-Object System.Text.UTF8Encoding($false)))
        $map[$rel] = $f.FullName
    }

    Push-Location $tmp
    try {
        $bad = @(gofmt -l . 2>$null | ForEach-Object { $_ -replace '^\./', '' -replace '\\', '/' })
    }
    finally {
        Pop-Location
    }

    if (-not $bad) {
        Write-Host "gofmt: OK（$($files.Count) 个文件，按 LF 形态检查）"
        exit 0
    }

    if ($Fix) {
        foreach ($rel in $bad) {
            $target = $map[$rel]
            if ($target) { gofmt -w $target }
        }
        Write-Host "gofmt: 已格式化 $($bad.Count) 个文件（工作树）"
        exit 0
    }

    Write-Host "gofmt: FAIL（$($bad.Count)/$($files.Count) 个文件未格式化；按 LF 形态判定）"
    $bad | Sort-Object | ForEach-Object { Write-Host "  $_" }
    exit 1
}
finally {
    Pop-Location
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
