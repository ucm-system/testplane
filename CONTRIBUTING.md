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

## 当前**没有**强制力（重要）

本仓库是**私有仓库**，组织在 **GitHub Free** 计划。GitHub 的分支保护与 ruleset **对 Free 计划的私有库不开放**（实测 `GET /branches/main/protection` 与 `GET /rulesets` 均返回 403「Upgrade to GitHub Pro or make this repository public」）。

所以：

- 「只能 PR 合入」目前是**团队约定**，服务端不拦；
- 有写权限的人技术上可以直接 `git push origin main`——**请不要这样做**；
- 需要真正强制时，二选一，再由管理员开启保护：
  1. 仓库转 **public**（免费，但设计文档里的 `registry.dev.huawei.com` 等内部标识会公开，需先脱敏）；
  2. 组织升级 **GitHub Team**（付费），保持私有。

## 不要提交

运行产物、日志、凭据、临时文件——见 `.gitignore`。脚本与 Dockerfile 必须 LF（`.gitattributes`），否则容器内会 `bad interpreter`。
