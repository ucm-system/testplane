# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## 布局结论：single-context

**本仓库是 single-context**：一个仓库共用一份根目录的 `GLOSSARY.md`，架构决定放 `docs/adr/`。

- 没有 `GLOSSARY-MAP.md`，也没有 `src/<context>/GLOSSARY.md` 这种分上下文布局——不要按多上下文去找。
- `GLOSSARY.md` 与 `docs/adr/` 目前**都还没有**：本次初始化只落配置，不预建空文件；等第一次真正写下词条或第一条 ADR 时再创建。

## Before exploring, read these

- **`GLOSSARY.md`** at the repo root, or
- **`GLOSSARY-MAP.md`** at the repo root if it exists: it points at one `GLOSSARY.md` per context. Read each one relevant to the topic.
- **`docs/adr/`**: read ADRs that touch the area you're about to work in. In multi-context repos, also check `src/<context>/docs/adr/` for context-scoped decisions.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## 本仓库的既有规格（先读它们，再谈代码）

TestPlane 是**设计先行**的仓库：`design/` 是唯一规格来源，代码尚未落地。动任何实现之前，按这个顺序读：

| 文件 | 回答什么 |
| --- | --- |
| [`design/ARCHITECTURE.md`](../../design/ARCHITECTURE.md) | 为什么这么做：原则、裁决（D-30…D-37）、附录错误码 |
| [`design/views/README.md`](../../design/views/README.md) | 4+1 视图索引、术语表、阅读路径 |
| [`design/views/02-logical-view.md`](../../design/views/02-logical-view.md) | 状态机与接口的**权威定义**（Run/Task/Attempt/Tester + Deploy 子状态） |
| [`design/views/03-data-view.md`](../../design/views/03-data-view.md) | 表结构、约束、JSON 形状的**权威定义** |
| [`design/views/07-api.md`](../../design/views/07-api.md) | REST 端点、错误模型、SSE 的**权威定义** |
| [`design/impl/`](../../design/impl/) | 三个实现方案：控制面 / 执行层 / Web 与交付 |

状态机、数据表、API 三处**同步改**：改一处就要同步 `design/views/02`、`03`、`07` 与 `api/openapi.yaml`、`schemas/`。

## File structure

Single-context repo (this repo):

```
/
├── GLOSSARY.md          ← 尚未创建（懒创建）
├── docs/adr/            ← 尚未创建（懒创建）
├── docs/agents/         ← 本目录
├── design/              ← 规格：ARCHITECTURE + 4+1 视图 + 实现方案
├── testers/             ← Tester 实现（每个目录一个 Tester）
└── server/ web/ ...     ← 实现待补
```

Multi-context repo (presence of `GLOSSARY-MAP.md` at the root) — **本仓库不是**:

```
/
├── GLOSSARY-MAP.md
├── docs/adr/                          ← system-wide decisions
└── src/
    ├── ordering/
    │   ├── GLOSSARY.md
    │   └── docs/adr/                  ← context-specific decisions
    └── billing/
        ├── GLOSSARY.md
        └── docs/adr/
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `GLOSSARY.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

术语尚未建表之前，以 [`design/views/README.md`](../../design/views/README.md) 的"术语"一节为临时词表；它的用词与 `design/` 全文一致（Run / Task / Attempt / Endpoint / Tester / Pre-Check / Deploy）。

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (event-sourced orders), but worth reopening because…_
