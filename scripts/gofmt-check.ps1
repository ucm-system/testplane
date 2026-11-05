# gofmt-check.ps1 —— 把工作树里的 Go 文件复制成 **LF 形态**再跑 gofmt，绕开 Windows CRLF 假阳性。
#
# 为什么不用 `gofmt -l .`：`core.autocrlf=true` 时工作树是 CRLF，gofmt 把每一行都当需要重写，
# 于是全部文件都"未格式化"。而 `.gitattributes` 规定仓库内是 LF，所以这里先把内容统一成 LF
# （临时副本），再在副本上跑 `gofmt -l` —— 结论与"把仓库 clone 到 Linux 上跑 gofmt"一致。
#
# 用法：
#   pwsh scripts/gofmt-check.ps1          # 检查（CI 门禁用）
#   pwsh scripts/gofmt-check.ps1 -Fix     # 检查并对工作树里命中的文件 gofmt -w
#
# 防"空跑假绿"（PR #33 评审第 16 条）：扫到的文件数少于 -MinFiles 直接判**失败**。
# 另外排除规则只看**仓库根相对路径**，不看绝对路径——否则仓库放在 `...\.data\worktree\`
# 之类的目录下时，所有文件都会被排除，脚本会打印"没有 Go 文件"并 exit 0。
param(
    [switch]$Fix,
    [int]$MinFiles = 100
)

$ErrorActionPreference = 'Stop'
$repoRoot = Resolve-Path (Join-Path $PSScriptRoot '..')
Push-Location $repoRoot
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("tp-gofmt-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $root = (Get-Location).Path
    $files = Get-ChildItem -Recurse -Filter '*.go' -File |
        Where-Object {
            # 只看仓库根相对路径，避免"仓库本身在 .data/ 下"时把所有文件都排除掉
            $rel = ([System.IO.Path]::GetRelativePath($root, $_.FullName)) -replace '\\', '/'
            $rel -notmatch '^(\.data|\.tools|node_modules|\.git)/'
        }
    if (-not $files) {
        Write-Host "gofmt: FAIL（没有扫到任何 Go 文件；工作目录 $root，或过滤规则把源文件排除了）"
        exit 1
    }
    if ($files.Count -lt $MinFiles) {
        Write-Host "gofmt: FAIL（只扫到 $($files.Count) 个 Go 文件，少于下限 $MinFiles —— 目录/过滤规则不对时不允许当作通过）"
        exit 1
    }

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
