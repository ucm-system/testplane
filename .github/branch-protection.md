# `main` 分支保护（已配置，改动请同步本文件）

规则由 GitHub Branch protection 执行，`.github/workflows/ci.yml` 提供检查项。**任何人对 `main` 都不能直接 push**，
包括管理员（`enforce_admins: true`）；发版靠 tag，合入靠 PR。

## 当前规则

| 项 | 值 | 为什么 |
|---|---|---|
| 必须走 PR | 是 | 直接 push `main` 关闭（含管理员） |
| 必需状态检查 | `go build / vet / test（含真 PG）`、`gofmt / 契约工具构建`、`契约（spec 侧：openapi ↔ design 50 端点）`、`web 类型检查 / 构建 / Playwright（mock）`、`Helm chart lint / 渲染`、`本地 kind 清单可渲染` | 六道门禁全绿才能合；**必须用 job 的 `name`（不是 job id）**，写错会导致所有 PR 永久 BLOCKED（已按实际 check 名应用） |
| 分支必须最新 | 是（`strict`） | 防止"绿了但基于旧 main"合并 |
| 必需审批 | 0（团队目前靠 PR 评论评审） | 需要人工审批时把 `required_approving_review_count` 调成 1 |
| 禁止 force push / 删除分支 | 是 | 保护历史 |
| 线性历史 | 是（squash/rebase） | 与现有 squash 合并习惯一致 |
| 对话解决 | 是 | 评审意见必须显式 resolve |

## 怎么改（管理员）

```bash
gh api -X PUT repos/ucm-system/testplane/branches/main/protection \
  -H 'Accept: application/vnd.github+json' --input - <<'JSON'
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "go build / vet / test（含真 PG）",
      "gofmt / 契约工具构建",
      "契约（spec 侧：openapi ↔ design 50 端点）",
      "web 类型检查 / 构建 / Playwright（mock）",
      "Helm chart lint / 渲染",
      "本地 kind 清单可渲染"
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": null,
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "required_conversation_resolution": true
}
JSON
```

查看现状：

```bash
gh api repos/ucm-system/testplane/branches/main/protection | jq
```

## 发版流程

```bash
git checkout main && git pull
git tag -a v0.1.0 -m "TestPlane v0.1.0"
git push origin v0.1.0          # → 触发 .github/workflows/release.yml
```

发布的镜像（**linux/amd64 + linux/arm64**，`ghcr.io/ucm-system/...`）：

| 镜像 | 源 |
|---|---|
| `testplane-server` | `deploy/docker/server.Dockerfile`（前端已内嵌进二进制） |
| `testplane-web` | `deploy/docker/web.Dockerfile`（纯静态 + 可选 `/api` 反代） |
| `testplane-tester-<name>` | `testers/<name>/build/Dockerfile`（矩阵自动发现，加 Tester 不用改 workflow） |

每个镜像三个 tag：`:<version>`（`v0.1.0` → `0.1.0`）、`:sha-<12>`、`:latest`；digest 写进 job summary，直接填进 Helm：

```yaml
image:
  repository: ghcr.io/ucm-system/testplane-server
  digest: sha256:...
```

> **首次发版后要做一次手工设置**：GitHub → 仓库 → Packages → 选中包 → Package settings →
> Change visibility → Public。否则匿名拉取会 401（私有包需要 `image.pullSecrets`）。

## 镜像发布的两个前置设置（一次性）

1. **`GHCR_TOKEN` 仓库 Secret**（PAT，scope 至少 `write:packages`）——本组织把 Actions 的默认 workflow 权限锁成了
   `read`（`PUT /repos/{repo}/actions/permissions/workflow` 返回 `409 Write permissions for workflows are disabled by the organization`），
   所以 `GITHUB_TOKEN` 拿不到 `packages: write`，release job 会报
   `denied: permission_denied: write_package`。workflow 里优先用这个 secret，缺失才退回 `GITHUB_TOKEN`。
2. **包可见性**：首次发版后到 *仓库 → Packages → 选包 → Package settings → Change visibility → Public*。
   默认是 `internal`，匿名拉取会 401（集群里要配 `image.pullSecrets` 才能拉）。
   用 API 改会 404（`PATCH /orgs/{org}/packages/container/{name}` 目前不可用），只能 UI 改。