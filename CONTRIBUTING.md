# 贡献流程

## 分支

- `main` 是唯一长期分支，**所有改动经 PR 合入**；
- 分支命名：`docs/简短描述`、`feat/…`、`fix/…`、`chore/…`。

```bash
git switch -c docs/workflow main
```

## 提 PR 之前自测

| 改动类型 | 自测 |
|---|---|
| 设计文档 | 章节号引用是否仍然成立（`§x.y`）；改裁决要按 §15 记录 |
| 原型 | 双击 `design/prototype/index.html` 过一遍相关页面；**有 UI 变更就重出截图** |
| Tester | `./run-local.sh --url … --model_name …` 能跑通，`result.json` 符合附录 A |
| 仓库杂项 | 没有混入无关格式化 |

## PR 要求

- 标题：`<类型>: <一句话>`，类型取 `docs` / `feat` / `fix` / `chore`；
- 描述写清楚**改了什么、为什么**；
- 涉及设计的标出章节号（如 §6.7），涉及实现的对应到 §14.1 的 P0 验收项；
- **一个 PR 只做一件事**。

## 合入方式

- 仓库只开了 **squash** 与 **rebase**，merge commit 已关闭 → `main` 保持线性历史；
- 合并后源分支**自动删除**；
- 需要 review 时 `@Potterluo`。

## `main` 已开分支保护（服务端强制）

仓库已转为 **public**，GitHub 的分支保护因此可用，**已启用**（详见 [`.github/branch-protection.md`](.github/branch-protection.md)）：

- 任何人对 `main` 都不能直接 `push`，**包括管理员**（`enforce_admins: true`）——只能走 PR；
- 只接受 **squash / rebase**（线性历史），禁止 force push 与删除分支；
- 评审意见必须先 **resolve** 才能合；
- CI 的必需检查项在 [`ci.yml`](.github/workflows/ci.yml) 落地后启用（见该文件里的 job `name`）。

## 自动评审

每 15 分钟轮询一次打开的 PR，有新提交就起一个独立 agent 按
[`docs/agents/pr-review.md`](docs/agents/pr-review.md) 评审，把意见贴成 PR 评论，
并给 head 打一个 `agent-review` 的 commit status。

```bash
pwsh scripts/agents/pr-review-dispatch.ps1 -DryRun   # 看会评哪几张
pwsh scripts/agents/pr-review-dispatch.ps1           # 立刻评一轮
```

> 机制说明：GitHub 的 webhook 打不到本机，所以用「定时轮询 + 按 head SHA 去重」代替
> webhook；语义相同——同一份提交只评一次。评审需要模型凭据，若 headless profile 里没有
> （`MISSING_CREDENTIAL`），任务会落到 `.data/pr-review/pending/`，由有凭据的会话接手。

## 不要提交

运行产物、日志、凭据、临时文件——见 `.gitignore`。脚本与 Dockerfile 必须 LF（`.gitattributes`），否则容器内会 `bad interpreter`。

