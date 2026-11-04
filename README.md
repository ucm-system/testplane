# TestPlane

> 云原生、轻量化、AI Agent 友好的 **LLM 服务测试编排平台**。

本仓库存放 TestPlane 的**设计资料**、**可点击原型**，以及已按契约实现的 **Tester**。平台服务端代码不在本仓库。

## 仓库内容

| 路径 | 内容 |
|---|---|
| [`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) | 架构与设计约束（设计裁决集合）。系统边界、Tester 契约、Pre-Check 语义、并发与排队、存储与日志、错误码与终态语义 |
| [`design/prototype/`](design/prototype/) | 单文件可点击原型（圣杯布局），覆盖概览 / 运行列表 / 运行详情 / 提交运行 / Tester 目录 / 执行环境 |
| [`testers/endpoint-probe/`](testers/endpoint-probe/) | 已实现的 Pre-Check Tester：探测 LLM 端点此刻能否出 token |

## 设计文档：约束，不是需求

[`design/ARCHITECTURE.md`](design/ARCHITECTURE.md) 是**设计裁决**的集合：

- 写的是裁决（必须 / 不得 / 应该 / 可以），不是背景论证与用户画像；
- 与实现冲突时**以文档为准**；确需变更时按 §15 追加裁决条目并记录，不做静默修改；
- 显式列出 Non-Goals，防止实现蔓延。

它曾以 `PRD.md` 命名，现已停用——需求类文档另行维护，不用设计文档代替需求基线。

## 原型：双击就能看

无需构建、无需服务器、无需联网：

```text
双击 design/prototype/index.html
```

- 单文件 HTML + vanilla JS，数据是 mock，刷新即还原；
- 覆盖 6 个页面与完整状态谱：成功 / 部分失败 / 服务不可用 / 执行中 / 已取消 / 不部署跑已有服务；
- 截图在 [`design/prototype/screenshots/`](design/prototype/screenshots/)，设计说明在 [`design/prototype/README.md`](design/prototype/README.md)；
- 字体自托管（`assets/fonts/`），离线也不回落系统字体。

## Tester：契约的参考实现

[`testers/endpoint-probe/`](testers/endpoint-probe/) 既是可用的 Pre-Check Tester，也是新 Tester 的模板：

- 平台只认 `tester.yaml` + `run.sh`，用 argv 调它，不关心内部怎么实现；
- 输出统一落 `/workspace/output`，`result.json` 是唯一必产出文件；
- 本地可直接跑：`./run-local.sh --url ... --model_name ...`。

细节见[该目录的 README](testers/endpoint-probe/README.md)。

## 本仓库刻意不含

- 平台服务端 / Web UI / 调度器实现代码；
- AISBench、EvalScope 等测试工具本身（平台只调它们）；
- 部署逻辑——统一由 Genet 承担，TestPlane 不做部署平台；
- 需求文档、排期与里程碑。

## 协作

- 默认分支 `main`；
- 文档改动请在 PR 描述里指出对应章节号（如 §6.7），方便评审对照；
- 不要提交测试运行产物、日志与任何凭据（见 `.gitignore`）。
