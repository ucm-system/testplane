# TestPlane · P0 验收记录（MVP）

> **口径**：`design/ARCHITECTURE.md` §14.1 的 **17 项 P0 验收** + `design/IMPLEMENTATION.md` §9（每项的证据形态）
> 与 §15.1（MVP 状态总览）。本文只记录"**做到哪一步 + 证据在哪**"，不改验收标准。
>
> **状态图例**：✅ 做过（有可复跑证据） / 🟡 部分或 mock（形态通、真依赖未接） / ⬜ 未做。
>
> **来源**：QA 最终验收数据见 §2.5（镜像 `b61a1a23…`，集群实例，严格模式）；
> QA 侧过程结论见 [`scripts/e2e/acceptance-matrix.md`](../../scripts/e2e/acceptance-matrix.md)；
> 带 🔧 的条目是 infra 在 kind 集群里实跑补的证据（2026-11-05）。
>
> ⚠️ **编号对齐**：本文编号 = ARCHITECTURE §14.1 的 17 行。口径里说的"**#14 日志采集**"对应本表
> **#14 产物持久化**（日志是产物的一种：`logs/stdout.log` 落 PVC，api 直读回放）；
> 本表没有独立的"日志采集"行，因为 §14.1 没有这一项。
>
> 最后更新：**2026-11-05**（QA 最终数据 + 取消/超时路径并入，收口版）。
> 状态会变——改这里时同步改时间戳和对应行，别只改字。

---

## 1 十七项总表

| # | 验收项（§14.1） | MVP 状态 | 证据 / 备注 | 复跑 |
|---|---|---|---|---|
| 1 | Run 提交与完整状态链 | ✅ 做过 | QA e2e 生命周期 **31/31 PASS**：`r-20261105-EPRN` `queued→testing→succeeded` **7.0s**，18 条事件（含 `report.written`）；三条终态均有实例——`succeeded`（EPRN）、`completed_with_errors`（fail-401 / error-in-200，各 4.1s）、`service_unavailable`（fail-500，12.1s）。🔧 另测 service 形态 `r-20261105-KWYE` 17 条事件链完整 | 见 §2.5、§2.1 |
| 2 | Task 独立提交（`POST /tasks`） | ⬜ 未做 | Lead 裁决不在 MVP（`IMPLEMENTATION` §15.1）；`/api/v1/tasks` 只有查询，无提交 | — |
| 3 | Genet 适配三项（部署/状态/Endpoint） | 🟡 mock | 🔧 `deploy` 形态实跑通：`r-20261105-PRBV` `queued→deploying→testing→succeeded`（~10s），`deploy.state_changed` 带 `deployment_id`，Endpoint 由 mock 注入（`TESTPLANE_MOCK_ENDPOINT_URL`）。真实 Genet 未接（D-14） | 见 §2.2 |
| 4 | Pre-Check 注入与阻断 | ✅ 做过 | 🔧 注入：每次提交自动生成 `seq=0` 的 `precheck`（`role: pre_check`）。阻断：QA 失败注入 **5/5**——`fail-500`→`service_unavailable` 12.1s（retryable=true）、`fail-401`→`completed_with_errors` 4.1s（retryable=false）、`error-in-200`→`completed_with_errors` 4.1s；precheck 失败后其余 Task `skipped/upstream_abort` | `scripts/e2e/failure-injection.ps1` |
| 5 | Registry 发现与校验 | ✅ 做过 | 🔧 启动扫描 `Tester 注册表就绪 dir=/app/testers count=1`；`GET /api/v1/testers` 返回 `endpoint-probe` 全量字段（`entry: /tester/run.sh`、`role: pre_check`、`parameters`）。校验 15 条由 `internal/registry` 单测覆盖 | `go test ./internal/registry/...` |
| 6 | Tester 契约（入口/argv/输出/result.json） | ✅ 做过 | 🔧 真实镜像在 kind 里跑通；`injected_argv` 见 `--url=`/`--model_name=`；`result.json` 符合附录 A（`schema_version/run_id/task_id/attempt_id/status/workload/metrics`），落 PVC 最终路径 | 见 §2.1 |
| 7 | Tester 独立运行（不依赖平台） | ✅ 做过 | QA `probe-check.ps1`：5/5 场景 exit code 与 `result.json` 均符合设计 | `pwsh scripts/e2e/probe-check.ps1` |
| 8 | K8s Executor（超时/取消） | ✅ 做过 | **取消 8/8**：`r-20261105-HX8M` → `canceled`（实测 submitted→finished **48.8s**），Task 全 `skipped/upstream_abort`，**无 Pod / 无 Job / 无租约残留**（`endpoint_leases`=0）。**超时 14/14**：`r-20261105-04K3` → `failed` **16.9s**，Task `status_reason=TASK_TIMEOUT`、`error.retryable=true`，Attempt span 14.96s ≤ `timeout+30s`，事件 `task.state_changed#9`，Job/Pod/租约全清。🔧 另验 Job 创建/完成/观测：`Complete 1/1`、`subPath=<run>/tasks/<task>/attempts/<n>`、`/tmp` emptyDir、`fsGroup 10001`。备注：取消收口走对账循环，最坏约一个 `ReconcileInterval`（60s）——**设计取舍，不是缺陷** | `scripts/e2e/run-local.ps1` |
| 9 | 状态机与重启恢复 | 🟡 部分 | 转移表单测全绿（`go test ./internal/domain/...`）；启动 +60s 对账循环已加入，取消/超时两条路径都靠它收口（无残留）。缺：**杀 controller 后状态最终一致**的正式演练未做 | 需手工：`kubectl delete pod` + 观察 |
| 10 | Attempt 与重试 | ⬜ 未做 | 失败注入只验证了 `retryable` 标记（true/false），**`retry_on` 命中后新建 Attempt、历史不被覆盖**未跑；属 P1（失败续跑） | 需专门用例 |
| 11 | 失败策略（`abort_run`） | ✅ 做过 | QA 失败注入 5/5：`abort_run` 生效——precheck 失败 → 其余 Task `skipped/upstream_abort`，Run 终态按可重试性分流（`service_unavailable` / `completed_with_errors`） | `scripts/e2e/failure-injection.ps1` |
| 12 | Run 编排全链路 | 🟡 部分 | 🔧 service 链（`r-20261105-KWYE`）与 deploy 链（`r-20261105-PRBV`，含 `cleanup.started`）都贯通到 `succeeded`；**五条终态全部有实例**（`succeeded` / `completed_with_errors` / `service_unavailable` / `canceled` / `failed`）。缺：deploy 分支**只在 mock Genet 上**贯通，接真实 Genet 后整条链需重跑（与 #3 同源） | §2.1、§2.2、§2.5、§2.7 |
| 13 | 报告结构（report / result / 错误码） | ✅ 做过 | QA 实测：`r-20261105-EPRN` 的 `report.json` **662B**、`report.md` **780B**、18 条事件含 `report.written`；**终态后 ≤2s 生成**（7.0s 的 Run 结束即 200 可达）。🔧 `r-20261105-JW8Z`：`/report` 与 `/report.md` 200，`metadata.json`/`events.jsonl` 齐全，`artifacts` 12 行（result 2 / log 2 / raw 4 / report 4），`report.json` 含 `schema_version`/`counts`/`tasks[].duration_ms`/`failures[]`。⚠️ 早前 404 是**镜像落后于代码**（见 §2.3） | 见 §2.3、§2.5 |
| 14 | 产物持久化 | ✅ 做过 | 🔧 **日志**：`GET /api/v1/tasks/{id}/attempts/{idx}/logs` 读 PVC 的 `logs/stdout.log`，实测 200 + **363B** + `X-Log-Offset: 363`；`offset=5&limit=4`→体 4B / 头 `X-Log-Offset: 9`；`tail=2`→200（§2.6）。QA 侧 precheck/probe 日志 363B / 362B。**产物**：删掉 Job+Pod 后 `result.json`/`logs`/`raw` 仍在（§2.4）。实现方式：**executor 落盘 + api 直读**，`collector` 角色仍未拆，但"能取到日志与产物"这条验收已成立 | 见 §2.4、§2.6 |
| 15 | 失败隔离（一败一成功 → `COMPLETED_WITH_ERRORS`） | 🟡 部分 | `completed_with_errors` 终态已出现（fail-401 / error-in-200），但那两条是 precheck 失败 → 后续 `skipped`，**不是"一个 Task 成功 + 另一个失败"**。真正的混合结果需要第二个可执行 Tester | 需构造混合 Run |
| 16 | Web UI（列表/详情/报告/日志/提交/取消） | 🟡 部分 | `pnpm --dir web build` 通过；🔧 `GET /` 返回 `200 text/html`（单端口同时服务静态资源与 API）。缺：Playwright 冒烟（提交→详情→日志→取消） | `pnpm --dir web build` |
| 17 | API 契约与错误码 | ✅ 做过 | QA 最终：**44/44 PASS**（spec 12 + live 32），`tools/contract` 可复跑；错误码覆盖附录 D | `cd tools/contract && go test ./... && go run . --mode spec` |

**最终计分：✅ 10 · 🟡 5 · ⬜ 2（共 17）**

- ✅ 10：**#1 #4 #5 #6 #7 #8 #11 #13 #14 #17**
- 🟡 5：**#3（Genet 仅 mock）· #9（重启演练未做）· #12（deploy 分支仅 mock）· #15（一成一败混合）· #16（Playwright）**
- ⬜ 2：**#2 Task 独立提交（Lead 裁决排除，不是缺口）· #10 Attempt 与重试（P1 失败续跑）**

> 实现侧真正没做的只有 **#10 重试**；#2 是范围裁决。其余 5 项是"做了但没做满"，
> 差距都写在上面各自的"缺"里，不藏。

---

## 2 本轮实跑证据（infra · kind 集群）

环境：kind `v0.29.0` / k8s `v1.33.1` / PostgreSQL `16.13` / 镜像 `testplane-server:dev`（`kind load`）。
控制面：`testplaned-*` 1/1 Running，`/healthz`=`ok`、`/readyz`=`ok`。

### 2.1 service 形态全链路（#1 #6 #12）

```powershell
# 提交（嵌套 run: 是唯一契约形态，D-40）
POST /api/v1/runs   {"run":{"name":"infra-nested-smoke","service":{...},"secrets":{"ENDPOINT_API_KEY":"tp-secret-endpoint-mock/api_key"},"tests":[...]}}
→ 202 {"run":{"id":"r-20261105-KWYE","status":"queued"},"tasks":[{"name":"precheck",...},{"name":"probe",...}]}
```

| 时点 | 观测 |
|---|---|
| submit | `queued`，自动注入 `precheck`（seq=0） |
| +30s | `testing`，precheck 先跑 |
| +35s | `succeeded`，`task_counts: {total:2, succeeded:2}` |

```powershell
kubectl --context kind-testplane -n testplane get jobs -l testplane.io/run=r-20261105-KWYE
# tp-t-dxvjbd-a1   Complete 1/1   tp-t-f5w42q-a1   Complete 1/1

docker exec testplane-pg psql -U testplane -d testplane -c `
  "select seq,type,payload->>'to' from state_events where run_id='r-20261105-KWYE' order by seq;"
# 17 行：run.submitted → run.state_changed(submitted→queued→testing) →
#        task.state_changed / attempt.started / attempt.finished → cleanup.started → run.finished(succeeded)
```

`result.json` 关键字段（`tasks/t-DXVJBD/attempts/1/result.json`）：`schema_version=1.0`、`status=succeeded`、
`workload.base_url=http://mock-llm.default.svc:18080/v1`、`metrics[precheck_last_http_status]=200`。

### 2.2 deploy 形态全链路（#3 #12）

```powershell
POST /api/v1/runs   {"run":{"name":"infra-deploy-smoke","deploy":{"provider":"genet","parameters":{...}},...}}
→ r-20261105-PRBV
```

| 时点 | 观测 |
|---|---|
| t+0s | `deploying`（`deploy.state_changed`：`deployment_id=d-r-20261105-PRBV`） |
| t+4s | `testing`（mock 部署 ready，Endpoint 由 `TESTPLANE_MOCK_ENDPOINT_URL` 注入） |
| t+10s | `succeeded`，2/2 Task |

前提是 ConfigMap 有 `TESTPLANE_MOCK_ENDPOINT_URL` / `MODEL_NAME` / `DEPLOY_MOCK_DURATION_SECONDS`
（见 [`local-kind.md` §3.1](local-kind.md)），否则 Endpoint 指向不存在的地址。

### 2.3 报告与产物索引（#13）

终态后异步写，实测齐全（`r-20261105-JW8Z`，succeeded）：

```powershell
kubectl --context kind-testplane -n testplane exec deploy/testplaned -- `
  wget -qO- http://127.0.0.1:8080/api/v1/runs/r-20261105-JW8Z/report      # 200，report.json
kubectl --context kind-testplane -n testplane exec deploy/testplaned -- `
  wget -qO- http://127.0.0.1:8080/api/v1/runs/r-20261105-JW8Z/report.md   # 200，"# Run r-… / - 状态：**SUCCEEDED**"
Get-ChildItem .data\storage\r-20261105-JW8Z -File
# events.jsonl 4375B / metadata.json 2507B / report.json 653B / report.md 770B
docker exec testplane-pg psql -U testplane -d testplane -c `
  "select kind,count(*) from artifacts where run_id='r-20261105-JW8Z' group by kind;"
# log 2 / raw 4 / report 4 / result 2
```

`report.json` 形状（ARCHITECTURE §8.4）：`schema_version=1.0`、`status=succeeded`、`counts{total,succeeded,failed,canceled,skipped}`、
`tasks[]{task_id,name,status,attempts,duration_ms}`、`failures[]`。

> **踩过的坑（值得记）**：同一批 Run 之前 `/report` 一直 404、`artifacts` 表 0 行、run 目录里一个报告文件都没有，
> 当时以为是 `WriteRunReport` 的终态钩子没接。真实原因是**集群里的镜像比代码旧**（报告钩子的修复 11:03 才落地，
> 而运行中的 Pod 是 10:57 的镜像）。重灌后立刻正常。
> 教训：**任何"功能没生效"的结论，先确认运行中的 Pod 镜像 digest 与最新构建一致**（`local-kind.md` §0 第 4 条判据）。

### 2.4 产物持久化（#14）

```powershell
kubectl --context kind-testplane -n testplane delete job tp-t-dxvjbd-a1 tp-t-f5w42q-a1
Get-ChildItem -Recurse .data\storage\r-20261105-KWYE\tasks\t-DXVJBD\attempts\1
# result.json (946B) / logs/probe.log / raw/last-response.txt / raw/last-curl-error.txt —— 全在
```

Pod 删除后产物仍可读：产物直落共享存储最终路径（06 §3.3），不依赖容器生命周期。

### 2.5 QA 最终验收数据（集群实例，严格模式）

> 本轮数据采集时的镜像：契约 / 生命周期 / 失败注入在 `b61a1a23…`，取消 / 超时（§2.7）在收口构建 `7a2f2c12…`；
> 两者是同一次提交的连续构建，收口时集群实际运行的 `imageID` 见 §5。

| 套件 | 结果 | 说明 |
|---|---|---|
| 契约 | **44/44 PASS** | spec 12 + live 32 |
| e2e 生命周期 | **31/31 PASS** | `r-20261105-EPRN`：`queued→testing→succeeded` **7.0s**；`report.json` **662B** / `report.md` **780B**；precheck 日志 363B / probe 日志 362B；**18 条事件**（含 `report.written`） |
| 失败注入 | **5/5** | 见下表 |

| 场景 | 终态 | 耗时 | retryable |
|---|---|---|---|
| success | `succeeded` | 7.1s | — |
| fail-500 | `service_unavailable` | 12.1s | true |
| fail-401 | `completed_with_errors` | 4.1s | false |
| error-in-200 | `completed_with_errors` | 4.1s | false |
| slow-ready | `service_unavailable` | 12.1s | — |

> 这组数据把 #1（终态链）、#4（pre-check 阻断与可重试性分流）、#11（`abort_run` 传播）、#17（契约）从 🟡/⬜ 抬到 ✅。
> 报告"终态后 ≤2s 生成"来自这里：7.0s 的 Run 结束时 `/report` 已 200 可达。

### 2.6 日志读取（#14 的日志部分）

实现方式是 **executor 把 stdout/stderr 落 PVC 的 `logs/`，api 直读回放**（`collector` 角色仍未拆，D-38 的日志采集由 executor 承担）。
infra 在集群内实测（同一镜像，读 `t-BKH6NC` 的 attempt 1）：

```powershell
# offset=0&limit=2000
kubectl -n testplane exec deploy/testplaned -- wget -qS -O - `
  "http://127.0.0.1:8080/api/v1/tasks/t-BKH6NC/attempts/1/logs?offset=0&limit=2000"
# HTTP/1.1 200 OK · Content-Length: 363 · X-Log-Offset: 363

# offset=5&limit=4  → 200 · Content-Length: 4 · X-Log-Offset: 9
# tail=2            → 200 · X-Log-Offset: 363（读到的就是本次结束处）
```

`X-Log-Offset` = 本次响应结束处的字节偏移，客户端拿它做续读游标；`offset` 越界/尾部读取语义与设计一致。

### 2.7 取消与超时路径（#8）

两条路径都是"平台权威判定 + 对账循环收口"，不是靠 K8s 自己杀：

| 路径 | Run | 结果 | 关键事实 |
|---|---|---|---|
| 取消 8/8 | `r-20261105-HX8M` | `canceled`（**48.8s**） | Task 全 `skipped / upstream_abort`；Pod、Job、`endpoint_leases` 全部为 0；事件链收尾到 `run.finished` |
| 超时 14/14 | `r-20261105-04K3` | `failed`（**16.9s**） | Task `status_reason=TASK_TIMEOUT`、`error.retryable=true`；Attempt span **14.96s** ≤ `timeout+30s`；事件 `task.state_changed#9`；Job/Pod/租约全清 |

**取舍说明（写清免得被当成缺陷）**：取消的终态推进走控制面对账循环，
所以最坏要等**一个 `ReconcileInterval`（60s）**才看到 `canceling → canceled`。
这是"平台权威 + 可恢复"换来的延迟，不是遗漏；实测 48.8s 落在窗口内。

---

## 3 已知缺口（按优先级）

1. **重试/失败续跑**（#10）：`retry_on` 命中后新建 Attempt、历史不被覆盖——P1。
2. **混合结果**（#15）：需要第二个可执行 Tester 构造"一 Task 成功 + 一 Task 失败"的 `completed_with_errors`
   （现有的 `completed_with_errors` 都是 precheck 失败 → 后续 `skipped`，不是混合结果）。
3. **重启恢复演练**（#9）：杀 controller 后按 DB 对账最终一致——未做正式演练（对账循环本身已在取消/超时路径上验证过）。
4. **Web UI 交互**（#16）：Playwright 冒烟未做（构建与静态资源已验证）。
5. **deploy 链的真实依赖**（#3 #12）：只有 mock Genet；换真实 Genet 后整条链要重跑。

> 本轮曾出现过 4 条 `cancelling` 不推进（`queued` 阶段取消、无 Attempt 分支），已由对账循环修复：
> 最终 `runs` 表里 **`cancelling` / `queued` / `running` 残留 = 0**。

**不在 P0 缺口内**（范围裁决，写清免得被当成漏做）：
Task 独立提交 `POST /tasks`（#2，Lead 裁决排除）、Volcano 队列/快照（P1）、OIDC + RBAC + CSRF（P1）、
真实 Genet 适配（#3，MVP 用 mock，D-14）。

---

## 4 复跑命令汇总

```powershell
# 单元 / 契约（不需要集群）
Push-Location server; go build ./...; go vet ./...; go test ./...; Pop-Location
cd tools/contract; go test ./...; go run . --mode spec
cd tools/e2e; go test ./...

# Tester 独立运行（不需要集群）
pwsh scripts/e2e/probe-check.ps1

# 集群链路（需要 kind + 控制面，见 docs/runbook/local-kind.md §0）
pwsh scripts/e2e/run-local.ps1
pwsh scripts/e2e/failure-injection.ps1
```

---

## 5 环境侧事实（供复核）

| 项 | 值 |
|---|---|
| 集群 / context | `testplane` / `kind-testplane`（k8s `v1.33.1`，1 control-plane） |
| namespace | `testplane`（PSA `restricted`） |
| PostgreSQL | 容器 `testplane-pg`，`16.13`，宿主 `5432`，库/用户=testplane |
| 存储 | PV/PVC `testplane-storage`，10Gi `ReadWriteMany`（hostPath `/storage-root` ↔ 宿主 `.data\storage`） |
| 镜像（数据基线） | `testplane-server:dev` —— 集群实际 `imageID = sha256:7a2f2c1298ce77b93650214bfd96fadb228dccfa3d095f352f51477cedf7f057`（以 `kubectl -n testplane get pod … -o jsonpath='{.status.containerStatuses[0].imageID}'` 为准）。**二进制比对**：Pod 内 `/testplaned` 与最新本地构建的 `sha256sum` 一致（`869843fa…`），即集群跑的就是当前源码。`testplane/endpoint-probe:1.0.0`（同时打 `registry.dev.huawei.com/…` tag 供 `tester.yaml` 引用） |
| RBAC | namespace 级 `Role`（jobs/pods/pods-log/pvc/events/leases）+ 只读 `ClusterRole testplane-view`；**不授 secrets** |
| 验收用的本地入口 | 取消 / 超时两条路径分别在 **port-forward `8093` 与 `8095`** 上完成（避免与 E2E 并发用例抢端口）；`kubectl -n testplane port-forward svc/testplane 8093:8080` |
| 终态分布（收口时） | `service_unavailable 9 / succeeded 9 / canceled 8 / completed_with_errors 4 / failed 1`，**无 `cancelling` / `queued` / `running` 残留**；`endpoint_leases` = 0 |
