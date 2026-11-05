# TestPlane · PR 自动评审 dispatcher
#
# 做什么：轮询打开的 PR，发现"新的 PR / PR 有新提交"（按 head SHA 去重）就起一个
# **独立 agent 进程**评审，然后：
#   1. 把评审意见贴成 PR 评论（sticky：同一条反复更新，不刷屏）；
#   2. 给 head 打一个 `agent-review` 的 **commit status**（success / failure），
#      分支保护把 `agent-review` 列为必需检查后，就能按它拦合入。
#
# 为什么用轮询而不是 GitHub webhook：webhook 只能打公网地址，打不到本机；
# 而"评审"必须在本机跑（要模型凭据、要 gh 身份、要真跑测试）。所以用
# 定时轮询 + 状态文件代替 webhook，语义等价：同一 SHA 只评一次。
#
# 用法：
#   pwsh scripts/agents/pr-review-dispatch.ps1                 # 评所有待评 PR
#   pwsh scripts/agents/pr-review-dispatch.ps1 -DryRun         # 只看要评谁
#   pwsh scripts/agents/pr-review-dispatch.ps1 -PR 34 -Force   # 强制重评某张
#
# 定时（每 15 分钟）：
#   schtasks /Create /TN "TestPlane PR 自动评审" /SC MINUTE /MO 15 /F `
#     /TR "pwsh -NoProfile -File D:\Project\TestPalne\scripts\agents\pr-review-dispatch.ps1"
#
# 退出码：0 正常（含无可评）；1 有 PR 被判 changes；2 执行失败（含 agent 没凭据）。

[CmdletBinding()]
param(
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path,
    [string]$GitHubRepo = 'ucm-system/testplane',
    [int]$PR = 0,
    [switch]$Force,
    [switch]$DryRun,
    [int]$TimeoutMinutes = 45,
    [string]$RubricPath = '',
    # agent 命令：末尾的 `-` 表示从 stdin 读任务（headless 的约定）
    [string]$AgentCmd = 'dsh --profile headless -'
)

# 5.1 下 'Stop' 会把原生命令（git/gh）写到 stderr 的内容当成终止错误，
# 所以用 Continue，并在需要的地方显式检查 $LASTEXITCODE。
$ErrorActionPreference = 'Continue'

# Windows PowerShell 5.1 默认用系统 ANSI 代码页解码外部命令的输出，gh 返回的 UTF-8 JSON
# 会变成乱码、ConvertFrom-Json 直接失败（PR 描述里的中文尤其明显）。显式钉死 UTF-8。
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }
try { $OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }
$stateDir = Join-Path $RepoRoot '.data/pr-review'
$statePath = Join-Path $stateDir 'state.json'
$lockPath = Join-Path $stateDir '.lock'
$pendingDir = Join-Path $stateDir 'pending'
$marker = '<!-- dsh-pr-review -->'
$statusContext = 'agent-review'

New-Item -ItemType Directory -Force -Path $stateDir | Out-Null

$rubric = if ($RubricPath) { $RubricPath } else { Join-Path $RepoRoot 'docs/agents/pr-review.md' }
if (-not (Test-Path $rubric)) { $rubric = Join-Path $PSScriptRoot 'pr-review.md' }
if (-not (Test-Path $rubric)) { $rubric = Join-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) 'docs/agents/pr-review.md' }
if (-not (Test-Path $rubric)) { throw "找不到评审标准：$rubric" }

# Windows PowerShell 5.1 的 `-Encoding utf8` 会写 BOM，BOM 会污染评论正文与 state.json。
# 计划任务可能用 5.1 跑（本机没有独立 pwsh 7 二进制），所以统一走 .NET 的 UTF-8 无 BOM。
function Save-Utf8([string]$Path, [string]$Text) {
    $dir = Split-Path $Path -Parent
    if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    [System.IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Add-Utf8([string]$Path, [string]$Text) {
    [System.IO.File]::AppendAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Write-Log([string]$Message) {
    $line = '[{0}] {1}' -f (Get-Date -Format 'HH:mm:ss'), $Message
    Write-Host $line
    Add-Utf8 (Join-Path $stateDir 'dispatch.log') ($line + "`r`n")
}

# ---- 单实例 ----
if (Test-Path $lockPath) {
    $age = (Get-Date) - (Get-Item $lockPath).LastWriteTime
    if ($age.TotalMinutes -lt $TimeoutMinutes) {
        Write-Log "已有实例在跑（锁 $([int]$age.TotalMinutes) 分钟前），跳过"
        exit 0
    }
    Remove-Item $lockPath -Force
}
Save-Utf8 $lockPath (Get-Date -Format o)

try {
    # ---- 状态：PR 号 -> 已评的 head SHA ----
    $state = @{}
    if (Test-Path $statePath) {
        $raw = Get-Content $statePath -Raw -Encoding utf8
        if ($raw.Trim()) {
            (ConvertFrom-Json $raw).PSObject.Properties | ForEach-Object {
                $state[[string]$_.Name] = [string]$_.Value.sha
            }
        }
    }

    # ---- 取 PR ----
    if ($PR -gt 0) {
        $prs = @(gh pr view $PR --repo $GitHubRepo --json number,title,body,headRefOid,isDraft,baseRefName | ConvertFrom-Json)
    } else {
        $prs = gh pr list --repo $GitHubRepo --state open --limit 30 `
            --json number,title,body,headRefOid,isDraft,baseRefName | ConvertFrom-Json
    }
    if (-not $prs) { Write-Log '没有打开的 PR'; exit 0 }

    $pending = @()
    foreach ($p in $prs) {
        if ($p.isDraft) { continue }
        $key = [string]$p.number
        if (-not $Force -and $state.ContainsKey($key) -and $state[$key] -eq $p.headRefOid) { continue }
        $pending += $p
    }

    if ($pending.Count -eq 0) {
        Write-Log '没有待评的 PR（都按当前 head 评过了）'
        exit 0
    }
    Write-Log ('待评 {0} 张：{1}' -f $pending.Count, (($pending | ForEach-Object { "#$($_.number)" }) -join ' '))

    if ($DryRun) {
        $pending | ForEach-Object { Write-Host ('  #{0} {1} @ {2}' -f $_.number, $_.title, $_.headRefOid.Substring(0, 7)) }
        exit 0
    }

    $rubricText = Get-Content $rubric -Raw -Encoding utf8
    $exitCode = 0

    foreach ($p in $pending) {
        $num = [int]$p.number
        $sha = [string]$p.headRefOid
        $short = $sha.Substring(0, 7)
        $wt = Join-Path $stateDir ('wt-{0}' -f $num)
        Write-Log ("`n==== PR #{0} @ {1} ====" -f $num, $short)

        try {
            # ---- 隔离 worktree（绝不碰主工作区里别人的改动）----
            git -C $RepoRoot worktree remove --force $wt 2>$null | Out-Null
            Remove-Item $wt -Recurse -Force -ErrorAction SilentlyContinue
            git -C $RepoRoot fetch origin "pull/$num/head" --quiet 2>&1 | Out-Null
            $fetched = (git -C $RepoRoot rev-parse FETCH_HEAD).Trim()
            git -C $RepoRoot worktree add --detach $wt $fetched --quiet 2>&1 | Out-Null
            if (-not (Test-Path $wt)) { throw "worktree 创建失败：$wt" }

            # ---- 提示词 = 评审标准 + 本次上下文 ----
            $baseRef = "origin/$($p.baseRefName)"
            $prompt = @"
$rubricText

---

## 本次任务

仓库：$GitHubRepo
主工作区（**不要在里面 checkout**）：$RepoRoot
评审用隔离 worktree（**在这里看代码**）：$wt

PR #$num：$($p.title)
base 分支：$($p.baseRefName)（diff 用 ``git -C '$wt' diff $baseRef...HEAD``）
head SHA：$sha

PR 描述：

$($p.body)

### 步骤

1. 看 diff：``git -C '$wt' diff $baseRef...HEAD``；需要上下文时直接读 worktree 里的完整文件。
2. 要跑命令就在 worktree 里跑（例如 ``cd '$wt/server'``），不要污染主工作区。
3. 按五类检查逐条过，每条结论带 ``文件:行号``；没有发现就写"未发现"，不要编。
4. 严格按「输出契约」结尾给 VERDICT。
"@
            $promptFile = Join-Path $stateDir ('prompt-{0}-{1}.md' -f $num, $short)
            Save-Utf8 $promptFile $prompt

            # ---- 起 agent（独立进程，超时保护）----
            $logFile = Join-Path $stateDir ('review-{0}-{1}.md' -f $num, $short)
            Write-Log "  → 启动 agent：$AgentCmd"
            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            $job = Start-Job -ScriptBlock {
                param($root, $promptFile, $logFile, $agentCmd)
                Set-Location $root
                $text = Get-Content $promptFile -Raw -Encoding utf8
                $parts = $agentCmd -split '\s+'
                $exe = Get-Command $parts[0] -ErrorAction SilentlyContinue
                if (-not $exe) { return "MISSING_AGENT: 找不到 $($parts[0])" }
                $rest = @()
                if ($parts.Length -gt 1) { $rest = @($parts[1..($parts.Length - 1)]) }
                $text | & $exe.Source @rest 2>&1 | Tee-Object -FilePath $logFile
            } -ArgumentList $RepoRoot, $promptFile, $logFile, $AgentCmd

            if (-not (Wait-Job $job -Timeout ($TimeoutMinutes * 60))) {
                Stop-Job $job
                Remove-Job $job -Force
                throw "agent 超时（$TimeoutMinutes 分钟）"
            }
            $out = ((Receive-Job $job) -join "`n")
            Remove-Job $job -Force
            $sw.Stop()
            Write-Log ('  ← agent 结束，{0:N1} 分钟，输出 {1} 字符' -f $sw.Elapsed.TotalMinutes, $out.Length)

            # ---- 结论：没有 VERDICT 一律按"评审没完成"处理，绝不默认成 changes ----
            $m = [regex]::Match($out, 'VERDICT:\s*(approve|changes)')
            if (-not $m.Success) {
                $head = if ($out.Length -gt 200) { $out.Substring(0, 200) } else { $out }
                throw "agent 未给出 VERDICT（输出开头：$head）"
            }
            $verdict = $m.Groups[1].Value
            $rm = [regex]::Match($out, 'REASON:\s*(.+)')
            $reason = if ($rm.Success) { $rm.Groups[1].Value.Trim() } else { '' }

            $body = ($out -replace '(?s)\s*VERDICT:.*$', '').Trim()
            if (-not $body) { $body = '（agent 没有产出正文，见 .data/pr-review 下的 log）' }
            $stamp = Get-Date -Format 'yyyy-MM-dd HH:mm'
            $label = if ($verdict -eq 'approve') { '✅ 可以合入' } else { '❌ 需要修改' }
            $reasonLine = if ($reason) { "`n理由：$reason" } else { '' }
            $comment = @"
$marker
## 自动评审 · PR #$num @ $short

**结论：$label**$reasonLine

$body

<sub>自动评审（docs/agents/pr-review.md）· $stamp · head $sha</sub>
"@

            # ---- 贴评论（sticky）----
            $commentFile = Join-Path $stateDir ('comment-{0}-{1}.md' -f $num, $short)
            Save-Utf8 $commentFile $comment
            $comments = gh api "repos/$GitHubRepo/issues/$num/comments" --paginate 2>$null | ConvertFrom-Json
            $existing = ($comments | Where-Object { $_.body -like "*$marker*" } | Select-Object -First 1).id
            if ($existing) {
                gh api -X PATCH "repos/$GitHubRepo/issues/comments/$existing" -F "body=@$commentFile" 2>&1 | Out-Null
                if ($LASTEXITCODE -ne 0) { throw "更新评论失败（id=$existing）" }
                Write-Log "  → 评论已更新（id=$existing）"
            } else {
                gh pr comment $num --repo $GitHubRepo --body-file $commentFile 2>&1 | Out-Null
                if ($LASTEXITCODE -ne 0) { throw '发评论失败' }
                Write-Log '  → 评论已发布'
            }

            # ---- commit status（check run 需要 GitHub App 身份，用户 token 会 403）----
            $st = if ($verdict -eq 'approve') { 'success' } else { 'failure' }
            $desc = if ($reason) { $reason } else { $label }
            # GitHub 的 status description 上限 140 字符；超了整条 status 会创建失败
            # （表现是"评论发出去了但门禁没动"，很容易漏）。
            if ($desc.Length -gt 130) { $desc = $desc.Substring(0, 127) + '...' }
            gh api -X POST "repos/$GitHubRepo/statuses/$sha" `
                -f "state=$st" -f "context=$statusContext" -f "description=$desc" 2>&1 | Out-Null
            if ($LASTEXITCODE -ne 0) { Write-Log '  ! status 创建失败' } else { Write-Log "  → status: $statusContext = $st" }

            # ---- 只有拿到结论才记状态 ----
            $state[[string]$num] = $sha
            $stateObj = @{}
            foreach ($k in $state.Keys) { $stateObj[$k] = @{ sha = $state[$k] } }
            Save-Utf8 $statePath ($stateObj | ConvertTo-Json -Depth 4)

            if ($verdict -ne 'approve') { $exitCode = 1 }
        }
        catch {
            $msg = $_.Exception.Message
            Write-Log ("  ✗ PR #{0} 失败：{1}" -f $num, $msg)
            if ($msg -match 'MISSING_CREDENTIAL|no API key|MISSING_AGENT') {
                New-Item -ItemType Directory -Force -Path $pendingDir | Out-Null
                $src = Join-Path $stateDir ('prompt-{0}-{1}.md' -f $num, $short)
                if (Test-Path $src) {
                    Copy-Item $src (Join-Path $pendingDir ('PR-{0}-{1}.md' -f $num, $short)) -Force
                }
                Write-Log ("  → agent 起不来（多半是模型凭据不在 headless profile 里）。已把任务落到 $pendingDir，可在有凭据的会话里评。")
            }
            $exitCode = 2
        }
        finally {
            git -C $RepoRoot worktree remove --force $wt 2>$null | Out-Null
            Remove-Item $wt -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    exit $exitCode
}
finally {
    Remove-Item $lockPath -Force -ErrorAction SilentlyContinue
}
