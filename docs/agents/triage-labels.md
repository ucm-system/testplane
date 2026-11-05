# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker.

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

Edit the "Label in our tracker" column to match whatever vocabulary you actually use.

## 已在本仓库创建的标签

| 标签 | 用途 |
| --- | --- |
| `needs-triage` | 新票默认入口，待维护者评估 |
| `needs-info` | 等报告人补充信息；补齐后回到 `needs-triage` |
| `ready-for-agent` | 规格已完整，可交给 AFK agent 直接实现 |
| `ready-for-human` | 需要人工实现或人工判断 |
| `wontfix` | 不做（含"已实现""已拒绝"两类结论） |
| `wayfinder:map` | 探索地图本体（一张图一个 issue） |
| `wayfinder:research` | 地图工单：查证一个问题 |
| `wayfinder:prototype` | 地图工单：做一次性原型验证 |
| `wayfinder:grilling` | 地图工单：拷问人做决策 |
| `wayfinder:task` | 地图工单：具体实施任务 |

规则：`wayfinder:*` 票只带 `wayfinder:*` 标签，**不带 triage 标签**——它们是决策而不是实施工作；除上面这些之外，不再强制任何标签。
