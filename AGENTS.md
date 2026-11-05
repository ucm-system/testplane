# AGENTS.md

TestPlane：面向 LLM 服务的云原生测试编排平台。**设计先行**——`design/` 是唯一规格来源，代码必须按它实现。

- 实现前先读 [`design/views/README.md`](design/views/README.md) 的阅读路径；状态机看 `design/views/02`，表结构看 `03`，API 看 `07`，实现方案看 `design/impl/`。
- 状态机、数据表、API 三处必须同步改（含 `api/openapi.yaml` 与 `schemas/`）。
- 分支与合入按 [`CONTRIBUTING.md`](CONTRIBUTING.md)：不直接提交 `main`，走分支 + PR。

## Agent skills

### Issue tracker

GitHub Issues（`ucm-system/testplane`），用 `gh` CLI 读写；wayfinder 地图与工单见 `docs/agents/issue-tracker.md`。

### Triage labels

五角色默认：`needs-triage` / `needs-info` / `ready-for-agent` / `ready-for-human` / `wontfix`；另有 `wayfinder:*` 五类地图标签。See `docs/agents/triage-labels.md`.

### Domain docs

single-context：一个仓库共用根目录一份 `GLOSSARY.md`，架构决定放 `docs/adr/`。See `docs/agents/domain.md`.
