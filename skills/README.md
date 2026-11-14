# Agent Skills

动手型技能（SKILL.md，含 frontmatter 可被 agent 工具直接加载；人也能当 checklist 读）。
契约与规格的**权威源**不在这里——`design/`（唯一规约）、`server/api/openapi.yaml`（唯一机读契约）、
`docs/agents/argus-playbook.md`（Agent 操作手册）。技能只收录「照着做」的路径与实测过的坑。

| 技能 | 什么时候用 |
|---|---|
| [`argus-submit-task`](argus-submit-task/SKILL.md) | 要在平台上提交/跟踪一个评测 Task（UI 或 API） |
| [`argus-develop-runner`](argus-develop-runner/SKILL.md) | 要开发/适配/修改一个 Runner（runner.yaml、环境变量、退出码、版本钉死） |
| [`argus-local-dev`](argus-local-dev/SKILL.md) | 本地 kind 环境重灌、排障、跑集成/E2E 测试 |
| [`argus-release`](argus-release/SKILL.md) | 要发一个 vX.Y.Z 版本（chart → tag → 镜像 → digest → Release） |

## 使用约定

- 技能内容与权威源冲突时，**以权威源为准**并回来修技能（技能是索引 + 坑实录，不是规格）。
- 每条坑必须是实翻过的车或实证过的命令；纯推断的内容不要写进来。
- 改了平台行为（契约/部署形态/发布流程）的 PR，**顺手核对受影响的技能**——它们比 runbook
  更容易被 agent 读到。
