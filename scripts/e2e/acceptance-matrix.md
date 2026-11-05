# 验收矩阵结论（票 #3 · QA 侧）

> 对 `design/IMPLEMENTATION.md` §9 / ARCHITECTURE §14.1 的 17 项，给出「做过 / mock / 未做」与证据。
> 本文件只提供结论，**不写** `docs/runbook/acceptance.md`（由 infra 收口）。
> 状态随测试推进更新；最后更新：2026-11-05。

| # | 验收项 | 结论 | 证据 / 备注 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | 做过 | e2e `r-20261105-EPRN`：`queued→testing→succeeded` 7.0s（集群实例 8092，真 K8s Job）；31/31 断言全绿 |
| 2 | Task 独立提交 | 未做（MVP 排除） | Lead 裁决 `POST /tasks` 不在 MVP |
| 3 | Genet 适配三项 | 未做 | 依赖 backend 的 Fake Genet 与 deploy 编排 |
| 4 | Pre-Check 注入与阻断 | 做过 | 失败注入 5/5：500/slow → `service_unavailable`(retryable=true)；401/200包错误 → `completed_with_errors`(retryable=false)；其余 Task 全 `skipped(upstream_abort)` |
| 5 | Registry 发现与校验 | 做过（发现）/ 未做（非法 tester.yaml 单测） | `GET /testers` 返回 endpoint-probe；`tools/contract` 校验 required 字段 |
| 6 | Tester 契约（入口/argv/输出/result.json） | 做过 | 真实 `testers/endpoint-probe/run.sh` 在容器里对 mock-llm 跑通 5 种判定；`result.json` 11 字段通过校验 |
| 7 | Tester 独立运行 | 做过 | `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 result.json 均符合设计 |
| 8 | K8s Executor（超时/取消） | 做过 | 取消 8/8：`r-HX8M` 立即 cancel → `cancelling` → 49.6s 后 `canceled`（60s reconcile tick），Task 全 skipped、租约 0、Pod 0。超时 14/14：`r-04K3` `timeout_seconds=5` → Run `failed`、Task/Attempt `TASK_TIMEOUT retryable=true`、attempt 14.96s ≤ timeout+30s、Pod/Job 删除、租约 0、16.9s 终态 |
| 9 | 状态机与重启恢复 | 做过 | `scripts/e2e/restart-drill.ps1` **13/13**：`r-20261105-Q0VT` 在 Attempt running 时 kill controller，旧 Pod `testplaned-db565b6db-ckt9f` → 新 Pod `testplaned-db565b6db-44krc`，3.3s Ready；启动日志含 `startup reconcile`；Run 终态 `succeeded`，租约/Job 0；`attempt.finished` 2 条（payload 无对账标记，按实际记录） |
| 10 | Attempt 与重试 | 未做 | 需要 `retry_on` 命中与失败注入 |
| 11 | 失败策略（abort_run） | 做过 | 失败注入中 precheck 失败后其余 Task 全部 `skipped(upstream_abort)` ✅ |
| 12 | Run 编排全链路 | 做过 | success / service_unavailable / completed_with_errors / canceled / failed（平台超时）五类终态均实测 |
| 13 | 报告结构 | 做过 | `r-EPRN` report 662B / report.md 780B，终态后 ≤2s 出现 ✅ |
| 14 | 产物持久化 | 部分 | `result.json`、`raw/`、`logs/` 均落 PVC；`/logs` 返回 200 + X-Log-Offset；删 Pod 后下载待测 |
| 15 | 失败隔离 | 做过 | `r-29YJ` 11/11：A 打 mock-llm succeeded、B 打 mock-llm-500 failed；Run `completed_with_errors`；report counts `{total:2,succeeded:1,failed:1}`、failures 非空；Run.error=首个失败 Task（PRECHECK_FAILED） |
| 16 | Web UI | 做过 | `pnpm --dir web build` 通过；Playwright `ui-smoke.spec.ts` **1 passed**（/runs → 详情 Task 表 → /submit dry-run 报错可见） |
| 17 | API 契约与错误码 | 做过 | PR #13/#16 后：规格 15/15（设计端点双向差集 + 白名单）、实现 32/32、合计 **47/47**；warning 按扁平 Error（D-39） |

## PR #18 三项收口（2026-11-05 13:40）

- **#15 一成一败**：`pwsh scripts/e2e/mixed-result-check.ps1 -BaseUrl ... -SkipApply` → **11/11 PASS**，Run `r-29YJ`；router 按 `ENDPOINT_API_KEY` 把 A 转到 mock-llm、B 转到 mock-llm-500；report counts 1/1、failures 非空、Run.error 取首个失败 Task。
- **#16 Playwright**：`web/tests/ui-smoke.spec.ts` → **1 passed (2.1s)**，system Chrome（`channel: chrome`），dev server 5173 + `TESTPLANE_BACKEND=http://127.0.0.1:8103`。
- **#9 重启演练**：`scripts/e2e/restart-drill.ps1` → **13/13 PASS**，Run `r-20261105-Q0VT`，旧 Pod `testplaned-db565b6db-ckt9f` → 新 Pod `testplaned-db565b6db-44krc`，3.3s Ready；`startup reconcile` 日志已出现；Run `succeeded`，租约/Job 0。负向 `-RecoveryTimeoutSeconds 1` → exit 1。

## 复跑命令

```bash
cd tools/contract && go test ./... && go run . --mode spec
cd tools/e2e      && go test ./...
pwsh scripts/e2e/probe-check.ps1
pwsh scripts/e2e/run-local.ps1                  # 需要控制面
pwsh scripts/e2e/failure-injection.ps1          # 需要控制面 + kind executor
```

## 最终结果（2026-11-05 11:31，镜像 `b61a1a23…` / 8092，严格模式）

- 契约 `--mode all`：**44/44 PASS**（spec 12 + live 32）。
- e2e 生命周期：`r-20261105-EPRN` **31/31 PASS**，succeeded 7.0s；report 662B、report.md 780B、logs 363/362B、事件 18 条。
- 失败注入 **5/5 全绿**：
  | 场景 | Run | 实际终态 | 关键断言 |
  |---|---|---|---|
  | success | `r-9DBT` | succeeded | 7.1s |
  | fail-500 | `r-R1VP` | service_unavailable | PRECHECK_FAILED retryable=true |
  | fail-401 | `r-YAJG` | completed_with_errors | PRECHECK_FAILED retryable=false |
  | error-in-200 | `r-8G9W` | completed_with_errors | PRECHECK_FAILED retryable=false |
  | slow-ready | `r-Z47Q` | service_unavailable | PRECHECK_FAILED retryable=true |

严格断言，无 report/logs 豁免。

## 取消收口补充（2026-11-05 11:40，镜像 `0162d0c6…` / 8093）

- `contract --mode all`：**44/44 PASS**（确认取消改动未碰契约）。
- e2e 生命周期：`r-20261105-ZTBF` **31/31 PASS**，succeeded 7.1s。
- 取消场景 `scripts/e2e/cancel-check.ps1`：`r-20261105-HX8M` 立即 `POST /cancel` → `cancelling` → **49.6s** 后 `canceled`，**8/8 断言全绿**：
  - Run 终态 canceled、≤60s；
  - Task 全 skipped，reason=`upstream_abort`；
  - 无 Attempt 残留；
  - `endpoint_leases` 0 行；
  - `job-name=tp-t-*` Pod 0 个。
- DB 状态分布（收口后）：`succeeded 9 / canceled 8 / service_unavailable 6 / completed_with_errors 4`，**无 cancelling/queued/running 残留**。

取消时延说明：`cancelling → canceled` 走 C-3.5 对账循环（`ReconcileInterval=60s`），最坏约 60s；本次 49.6s 属 tick 相位，不是卡死。若产品要求更快，需要把取消收口挪到 watchdog 或调小 ReconcileInterval。

## 超时路径补充（2026-11-05 11:48，镜像 `0eade6b9…` 后续修复版 / 8095）

- `scripts/e2e/timeout-check.ps1`：**14/14 PASS**。
- `r-20261105-04K3`：`precheck:"off"` + `timeout_seconds=5` + mock-llm-slow。
  - Run 终态 **`failed`**（平台 C 类），16.9s ≤ 30s；
  - Task `t-9DSH8Z`：`failed`，`error.code=TASK_TIMEOUT`，`retryable=true`；
  - Attempt `t-9DSH8Z-a1`：`failed/phase=timeout`，span **14.96s ≤ timeout+30s**；
  - 事件：`task.state_changed#9` `running→failed` + `payload.error.code=TASK_TIMEOUT`（无 `task.timeout`，按口径接受）；
  - Job/Pod 已删除，`endpoint_leases` 0。
- DB 状态分布（收口后）：`service_unavailable 9 / succeeded 9 / canceled 8 / completed_with_errors 4 / failed 1`，无 `cancelling/queued/running` 残留。
- 判据：`resolved.precheck.injected` 为唯一依据（`precheck:"off"` → false → 全部 Task 走测试语义）；`seq==0` 与 tester role 都不能单独判 Pre-Check。
