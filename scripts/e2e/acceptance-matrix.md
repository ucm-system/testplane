# 验收矩阵结论（票 #3 · QA 侧）

> 对 `design/IMPLEMENTATION.md` §9 / ARCHITECTURE §14.1 的 17 项，给出「做过 / mock / 未做」与证据。
> 环境：kind `testplane`；真 Genet（`genet` ns）；fake-genet（宿主 `:18091`）；Volcano 演示夹具；最终镜像 `sha256:492ff780…`。
> 本文件只提供结论，**不写** `docs/runbook/acceptance.md`（由 infra 收口）。
> 状态随测试推进更新；最后更新：2026-11-05。

| # | 验收项 | 结论 | 证据 / 备注 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | 做过 | `run-local.ps1` 默认 MockURL 自动探测 kind 的 mock-llm；`r-20261105-7RTG` `queued→testing→succeeded` 8.1s，**31/31** 断言全绿 |
| 2 | Task 独立提交 | 未做（MVP 明确排除） | `IMPLEMENTATION §15.2` 延后表；`POST /tasks` 不在 MVP，非缺陷 |
| 3 | Genet 适配三项 | 做过 | 正常 `96QQ` **9/9**：`queued→accepted→deploying→ready`、endpoint.source=genet、precheck succeeded、Run succeeded、helm 0<br>blocked `DVNS` **7/7**：`service_unavailable` / `DEPLOY_BLOCKED` / retryable=true / `detail.reason=blocked` / uninstall=verified / helm 0<br>500 `70V5` **4/4**：`failed` / `DEPLOY_FAILED` / retryable=false / `cleanup.failed=2`<br>超窗：单测覆盖（genet 72h 用例 + 编排层 blocked 用例）；端到端不做（配置下限 1h） |
| 4 | Pre-Check 注入与阻断 | 做过 | failure-injection **5/5**：success `EHW7`、500 `B2PB`、401 `ZMMG`、200包错误 `AY4Q`、slow `2NQR`；可重试 → service_unavailable，不可重试 → completed_with_errors，其余 Task 全 `skipped(upstream_abort)` |
| 5 | Registry 发现与校验 | 做过（发现）/ 未做（非法 tester.yaml 单测） | `GET /testers` 返回 endpoint-probe；`tools/contract` 校验 required 字段 |
| 6 | Tester 契约（入口/argv/输出/result.json） | 做过 | 真实 `testers/endpoint-probe/run.sh` 在容器里对 mock-llm 跑通 5 种判定；`result.json` 11 字段通过校验 |
| 7 | Tester 独立运行 | 做过 | `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 result.json 均符合设计 |
| 8 | K8s Executor（超时/取消） | 做过 | 取消 **10/10**：`r-20261105-1YZW` 立即 cancel → `cancelling` → `canceled` 18.2s，Task 全 skipped、租约 0、Pod 0。超时 **17/17**：`r-20261105-G9D5` `timeout_seconds=5` → Run `failed`、Task/Attempt `TASK_TIMEOUT retryable=true`、attempt span 14.97s ≤ timeout+30s、Pod/Job 删除、租约 0 |
| 9 | 状态机与重启恢复 | 做过 | `restart-drill.ps1` **13/13**：`r-20261105-DD92` 在 Attempt running 时 kill controller，旧 Pod `testplaned-67b58ff5b7-x84v7` → 新 Pod `testplaned-67b58ff5b7-7klbm`，3.3s Ready；启动日志含 `startup reconcile`；Run succeeded，租约/Job 0；`attempt.finished` 2 条，按实际 `phase=succeeded` + `exit_code=0` 断言 |
| 10 | Attempt 与重试 | 未做（MVP 明确排除） | `IMPLEMENTATION §15.2` 延后表；`retry_on` 不在 MVP，非缺陷 |
| 11 | 失败策略（abort_run） | 做过 | failure-injection 中 precheck 失败后其余 Task 全部 `skipped(upstream_abort)`，5/5 场景一致 |
| 12 | Run 编排全链路 | 做过 | succeeded `6H0Y/7RTG`、service_unavailable `B2PB/2NQR/DVNS`、completed_with_errors `ZMMG/AY4Q/PA16`、canceled `1YZW`、failed `G9D5/70V5` 均实测 |
| 13 | 报告结构 | 做过 | `7RTG` report 662B / report.md 779B，终态后生成；混合 `PA16` counts/failures、deploy `96QQ` 报告均校验 |
| 14 | 产物持久化 | ✅ | `artifact-persistence-check.ps1` **44/44 PASS**：Run `r-20261105-GEEX`（14 条/12999B/result 2 条）；Job/Pod **删前按名记录并断言存在**，hold-until 期间不被删，删后按名复查；两种 `/files` path 形态均 200 且 sha256 一致；控制面 restart 后 sha256 仍一致；Range 206+10B；404/400（含带前缀穿越）负向通过；`kubectl` 失败时 `k8s.pods_query`/`pod.gone` FAIL，不再假通过 |
| 15 | 失败隔离 | 做过 | `r-20261105-PA16` **11/11**：A 打 mock-llm succeeded、B 打 mock-llm-500 failed；Run `completed_with_errors`；report counts `{total:2,succeeded:1,failed:1}`、failures 非空；Run.error=首个失败 Task（PRECHECK_FAILED） |
| 16 | Web UI | 做过 | `pnpm --dir web build` 通过；Playwright **3 passed**（`ui-smoke` + 新增 viewer / operator 取消用例；frontend 已提交） |
| 17 | API 契约与错误码 | 做过 | 规格 **17/17**、实现 **43/43**、合计 **60/60**；设计 50 / openapi 34 / 白名单 16（每条带 `IMPLEMENTATION.md:439` 指针）/ openapi 多出设计 0；设计基线 50 + 端点集合 sha256；白名单已实现条目判红；fixture 等终态后 `/report` 200、终态 404 必红 |

## PR #18 收口（2026-11-05 14:20，最终镜像 `sha256:492ff780…`）

- **#15 一成一败**：`mixed-result-check.ps1` → **11/11 PASS**，Run `r-20261105-PA16`；A→mock-llm succeeded、B→mock-llm-500 failed；report counts 1/1、failures 非空、Run.error 取首个失败 Task。
- **#16 Playwright**：`ui-smoke.spec.ts` + viewer/operator 取消用例 → **3 passed**，system Chrome（`channel: chrome`）。
- **#9 重启演练**：`restart-drill.ps1` → **13/13 PASS**，Run `r-20261105-DD92`，3.3s 恢复；`startup reconcile` 日志出现；Run succeeded、租约/Job 0；负向 `-RecoveryTimeoutSeconds 1` → exit 1。
- **#3 Genet deploy**：正常 `96QQ` 9/9；blocked `DVNS` 7/7；500 `70V5` 4/4；超窗单测覆盖（genet 72h + 编排层 blocked），端到端不做。
- **#17 契约**：`go test ./...` PASS；`--mode all` **60/60**（spec 17 + live 43）；设计 50 / openapi 34 / 白名单 16（全带指针）/ openapi 多出设计 0；设计基线 50 + sha256，终态 `/report` 200 强制、终态 404 必红。
- **#1 生命周期**：`run-local.ps1` 默认 MockURL 自动探测（namespace=default）→ `r-20261105-7RTG` **31/31**。
- **#14 产物持久化**：`artifact-persistence-check.ps1` → **44/44 PASS**，Run `r-20261105-GEEX`；Job/Pod 删前按名存在、hold-until 生效、删后按名复查；两种 path 形态 200 且 sha256 一致；restart 后仍一致；Range 206+10B、404/400（含带前缀穿越）负向通过；fake kubectl 时 `pod.gone` FAIL。
- **环境收尾**：configmap 已还原 `TESTPLANE_GENET_BASE_URL=http://host.docker.internal:18091`、`TESTPLANE_DEPLOY_TIMEOUT=72h`；fake-genet `:18095` 已停。

## 复跑命令

```bash
cd tools/contract && go test ./... && go run . --mode spec
cd tools/e2e      && go test ./...
pwsh scripts/e2e/probe-check.ps1
pwsh scripts/e2e/run-local.ps1                  # 需要控制面
pwsh scripts/e2e/failure-injection.ps1          # 需要控制面 + kind executor
```

## 历史结果（2026-11-05 11:31，镜像 `b61a1a23…` / 8092，严格模式）

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

## 超时路径补充（2026-11-05 11:48，镜像 `0eade6b9…` 后续修复版 / 8093）

- `scripts/e2e/timeout-check.ps1`：**14/14 PASS**。
- `r-20261105-04K3`：`precheck:"off"` + `timeout_seconds=5` + mock-llm-slow。
  - Run 终态 **`failed`**（平台 C 类），16.9s ≤ 30s；
  - Task `t-9DSH8Z`：`failed`，`error.code=TASK_TIMEOUT`，`retryable=true`；
  - Attempt `t-9DSH8Z-a1`：`failed/phase=timeout`，span **14.96s ≤ timeout+30s**；
  - 事件：`task.state_changed#9` `running→failed` + `payload.error.code=TASK_TIMEOUT`（无 `task.timeout`，按口径接受）；
  - Job/Pod 已删除，`endpoint_leases` 0。
- DB 状态分布（收口后）：`service_unavailable 9 / succeeded 9 / canceled 8 / completed_with_errors 4 / failed 1`，无 `cancelling/queued/running` 残留。
- 判据：`resolved.precheck.injected` 为唯一依据（`precheck:"off"` → false → 全部 Task 走测试语义）；`seq==0` 与 tester role 都不能单独判 Pre-Check。

## 本轮修复回归（2026-11-05 15:10–15:26，镜像 `sha256:093622d5…`）

- **#20 重启/GC**：`restart-drill.ps1` **13/13 PASS**，Run `r-20261105-THES`；控制器恢复 3.4s，终态 succeeded 60.1s，无 Job 残留。GC 真实观察：`r-20261105-GEEX` 的 Job `tp-t-60azqr-a1` 年龄 16s（<1h），终态后仍存在；`testplane.io/hold-until` 打到未来 10min 时未被 GC/正常清理删除；25h 分支由 `gc_test.go` 单测覆盖。
- **#21 契约门禁**：`--mode all` **60/60**（spec 17 + live 43）；`r-20261105-BKH5` fixture 终态 **succeeded 7.1s**，`/report`、`/report.md` 均 200；设计 50 / openapi 34 / 白名单 16，基线 count=50 + sha256。
- **#22 Deploy 错误码**：正常 `r-20261105-R4KG` **9/9**；blocked `r-20261105-K96J` **7/7**（`DEPLOY_BLOCKED`）；failure `r-20261105-5FBY` **4/4**（`DEPLOY_FAILED`、`retryable=false`、`cleanup.failed=2`）。
- **#29 脚本门禁**：
  - `artifact-persistence-check.ps1` 默认（delayed mock）**44/44 PASS**，Run `r-20261105-GEEX`；Job `tp-t-60azqr-a1` 删除前按名存在、hold-until 生效、删除后按名消失，Pod 按名复查；
  - fake `kubectl`（exit 1）时 `k8s.pods_query`/`pod.gone` **FAIL**，脚本 exit 1，不再“查询失败反而 PASS”；
  - 基线 hash 被改成 `deadbeef` 时 spec **16/17 FAIL**；白名单塞入已实现 `GET /api/v1/runs` 时 `spec.allow_missing_not_implemented` **FAIL**；
  - Playwright 真实后端 `tests/real-api.spec.ts`（`VITE_USE_MOCK=0`）**1 passed**：`/healthz` 200、`/api/v1/runs` 200 且非空、`/runs` 列表渲染非空。

## P1 端点收口（2026-11-05 16:39，镜像 `df0be9d4…`）

- `allow-missing.txt` **16 → 0**；openapi **50** operation，设计 50 / openapi-only 0；`spec.design_endpoints_present` = “0 个（已登记 0）”，`spec.operation_count` 从实际推导 50。
- 契约 `--mode all`：**80/80 PASS**（spec 17/17 + live 63/63）；retry 3、deploy/logs（终态 + 排队）、`GET /secrets`、`/admin/*` 12 个 operation 全部被 live 覆盖；admin 写只验 400/404/409 错误契约。
- 新增 `retry-check.ps1`：**17/17 PASS**（Run `r-20261105-CC2Q` → retry `r-20261105-7SEC`；新 Run 再取 plan 为 `retryable=false, reason=nothing_to_retry`）。
- 新增 `admin-check.ps1`：**26/26 PASS**；`secret.rotate_verified_200` 走 D-55 窄 Role；`it-forbidden-ref` 稳定 503 `reason=forbidden`。
- 老回归：run-local 31/31、probe 5/5、failure-injection 5/5、cancel 10/10、timeout 17/17、mixed 11/11；deploy 正常 9/9 / blocked 7/7 / failure 4/4；artifact 44/44；restart 13/13；Playwright **9 passed / 2 skipped**。
- 评审红确认：DELETE 改路径 → `client-contract.spec.ts` 1 failed；`deploy/logs` 忽略 pod → `TestDeployLogs_TwoPodsPaging` FAIL；审计改提交后补记 → `TestAuditFailure_RollsBackAdminWrite` FAIL；均已还原，恢复后全绿。




