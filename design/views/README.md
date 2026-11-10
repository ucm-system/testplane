# Argus 设计视图（4+1）索引

> **定位：** 按 Kruchten **4+1 视图**组织的开发指导文档集。ARCHITECTURE 定裁决，`design/impl/` 定实现方案，本文集定**结构、接口、数据与运行时行为**——面向"要开始写代码的人"。
> **效力：** 不新增设计裁决；与 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)（v2.0）冲突时以它为准。发现缺口回写 ARCHITECTURE，不在这里静默改语义。
> **状态：** P0 开发基线（2026-11-10，随 Argus v2.0 更名与 Runner 化改造同步；v1 术语 Run/Task/Tester 已全面对调，映射见 ARCHITECTURE §0.2）。

---

## 1. 4+1 视图映射

| 视图 | 文档 | 回答的问题 | 主要读者 |
|---|---|---|---|
| **Scenarios（场景）** | [01-use-case-view.md](01-use-case-view.md) | 谁在什么情况下做什么，成功/失败路径长什么样 | 全员、测试 |
| **Logical（逻辑）** | [02-logical-view.md](02-logical-view.md) | 领域对象、Task / runner 状态机与 attempt 模型、状态机接口与不变量 | 后端、测试 |
| **Process（进程）** | [04-process-view.md](04-process-view.md) | 运行时有哪些循环、一次 Task 的时序、竞态与恢复 | 后端、SRE |
| **Development（开发）** | [05-development-view.md](05-development-view.md) | 代码怎么分层、依赖方向、构建与测试门禁 | 后端、前端 |
| **Physical（部署）** | [06-physical-view.md](06-physical-view.md) | 跑在哪些进程/节点、网络、存储、容量与高可用 | SRE、后端 |
| 数据视图（逻辑视图的细化） | [03-data-view.md](03-data-view.md) | 表、列、约束、索引、JSON 结构、迁移与保留 | 后端、DBA |
| 接口视图（逻辑视图的边界） | [07-api.md](07-api.md) | REST 端点、请求/响应、错误码、SSE、鉴权 | 前端、集成方、Agent |

> 4+1 的"1"是场景：01 是入口，其余五份是它依赖的结构与行为。03 与 07 是 4+1 之外的两份**细化视图**，因为"数据表"和"API"在本项目里是两条最容易各自漂移的边界，单独成文、单独评审。

## 2. 阅读路径

| 角色 | 顺序 |
|---|---|
| 新加入的后端 | 01（场景）→ 02（状态机/接口）→ 03（表）→ 04（时序）→ 07（API）→ 05（代码结构） |
| 前端 | 01 → 07（端点/SSE/错误码）→ 06（部署，只要知道同源） |
| 测试 | 01 → 02（转移表）→ 03（可观测字段）→ 04（故障注入点） |
| SRE | 06 → 04（循环与选主）→ 05（构建产物） |
| 评审/架构 | 01 → 02 → 03 → 07，重点看跨视图一致性 |

## 3. 与既有文档的关系

```text
ARCHITECTURE.md         设计裁决（权威，v2.0）
  └─ IMPLEMENTATION.md  实现方案：选型、进程形态、K 决策、G 缺口
       ├─ impl/control-plane.md   摘要级 DDL / 状态机 / API 表
       ├─ impl/execution.md       Job 渲染 / 日志 / Registry
       ├─ impl/web-and-delivery.md 前端 / CI / Helm
       └─ views/（本目录）        4+1 细化：可执行的结构与接口
```

冲突处理顺序：**ARCHITECTURE > 本目录 > impl/ 摘要**。本目录与 impl/ 的重复内容以本目录为准（impl 里标了"详见 views"）。impl/ 分册尚未随 v2 更新时，其 v1 术语（Run/Task/Tester）按 ARCHITECTURE §0.2 映射阅读。

## 4. 速查：全局术语与枚举

| 项 | 取值 / 规则 |
|---|---|
| Task 状态（平台层） | `submitted` `queued` `running` `cancelling` ｜ 终态：`succeeded` `failed` `service_unavailable` `canceled`（v1 的 `deploying`/`testing`/`cleanup`/`completed_with_errors` 已取消，见 §7.4、D-73） |
| runner 状态（实例层） | `pending` `queued` `preparing` `running` ｜ 终态：`succeeded` `failed` `canceled` `skipped`（无重试回路，`failed` 是终态） |
| attempt | Task 级计数（1 起）；无独立状态机；重试 = 新 attempt 全量重跑含 fresh deploy |
| 部署状态（`deploy-state.json`，service-deploy 产出） | `queued` `accepted` `deploying` `ready` `blocked` |
| `waiting_reason` | `slot_waiting`（仅外接模式）`global_limit` `queue_full` `insufficient_resources` `gang_pending`（v1 的 `precheck_gate` 已随门控模型取消） |
| ID | `t-YYYYMMDD-XXXX`（Task）、执行记录定位 `(task_id, attempt_no, seq)`、`j-<8hex>`（job_id，幂等键）、K8s Job 名 `argus-<task_id>-<attempt>-<seq>` |
| 事件类型 | `submitted` `state_changed` `runner_started` `runner_finished` `exports_harvested` `clean_finished` `task_finished` `resync_required` |
| 错误码 | 只用 ARCHITECTURE 附录 D（v2）枚举；无 runner 级重试，`retryable: true` 仅用于 §9.3 重放与告警分级 |
| 时间 | 库内 `timestamptz`（UTC）；展示 `Asia/Shanghai`；ID 与排序不用本地时间 |

## 5. 文档纪律

- 每份文档开头写"覆盖范围 / 不覆盖"，避免互相吃职责。
- 状态、表名、端点、事件名在本目录内**只允许一处定义**（02 定义状态机，03 定义表，07 定义端点）；其他文档引用编号，不复制细节。
- 改动契约（表、端点、事件、状态）必须同时改 02/03/07 与 `schemas/`、`api/openapi.yaml`；CI 校验生成物 diff（ARCHITECTURE §15⑤）。
- 每份文档随实现更新，不写"未来可能"；P1 内容单独标注。
