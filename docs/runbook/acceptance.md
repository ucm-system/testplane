# TestPlane · P0 验收记录（快照 + 指针）

> **本文不是第二份验收数据。** 为避免"两处状态漂移"（评审 [#30](https://github.com/ucm-system/testplane/issues/30) 指出的问题），
> 验收的**唯一权威**是 [`scripts/e2e/acceptance-matrix.md`](../../scripts/e2e/acceptance-matrix.md)：
> 17 项逐项状态 + 证据（Run ID、断言数、脚本名、镜像 digest）。
>
> 本文只保留两件事：① 口径（哪 17 项、什么算通过）；② 当前快照（一行一项，明细指向矩阵）。

## 口径

- 验收项来自 `design/ARCHITECTURE.md` §14.1 的 **17 项 P0 验收**；
- 每项的证据形态见 `design/IMPLEMENTATION.md` §9；MVP 状态总览见 §15.1；
- 状态图例：**做过**（有可复跑证据）/ **🟡**（形态通、真依赖未接）/ **⬜**（未做；必须写明是否 MVP 明确排除）；
- 复跑方式见 [`scripts/e2e/README.md`](../../scripts/e2e/README.md)（脚本都支持 `-BaseUrl` 与 `-DryRun`）。

## 当前快照（明细与证据在矩阵）

| # | 验收项 | 状态 |
|---|---|---|
| 1 | Run 提交与完整状态链 | 做过（`run-local` 31/31） |
| 2 | Task 独立提交 | ⬜ **MVP 明确排除**（`IMPLEMENTATION §15.2`，非缺陷） |
| 3 | Genet 适配三项 | 做过（正常 9/9、blocked 7/7、500 4/4；超窗由单测覆盖） |
| 4 | Pre-Check 注入与阻断 | 做过（failure-injection 5/5） |
| 5 | Registry 发现与校验 | 做过（发现）；非法 `tester.yaml` 仅单测覆盖 |
| 6 | Tester 契约（入口/argv/输出/result.json） | 做过 |
| 7 | Tester 独立运行 | 做过（probe-check 5/5） |
| 8 | K8s Executor（超时/取消） | 做过（cancel 10/10、timeout 17/17） |
| 9 | 状态机与重启恢复 | 做过（restart-drill 13/13） |
| 10 | Attempt 与重试 | ⬜ **MVP 明确排除**（`retry_on` 不在 MVP） |
| 11 | 失败策略（abort_run） | 做过（5/5 场景一致） |
| 12 | Run 编排全链路 | 做过（五类终态均实测） |
| 13 | 报告结构 | 做过 |
| 14 | 产物持久化 | ✅（artifact-persistence 28/28：Pod 删除后与控制面重启后 sha256 一致） |
| 15 | 失败隔离 | 做过（mixed-result 11/11） |
| 16 | Web UI | 做过（Playwright 3 passed） |
| 17 | API 契约与错误码 | 做过（契约 59/59） |

> 快照时间：2026-11-05；镜像 `sha256:093622d5…`；环境：kind `testplane`（v1.33.1）+ 真 Genet（`genet` ns）+ fake-genet（宿主 `:18091`）+ Volcano 演示夹具。
>
> **改本文前先问一句**：这条信息该不该进矩阵？该进就只改矩阵，本文保持"快照 + 指针"角色。
