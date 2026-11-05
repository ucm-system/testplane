# TestPlane · P0 验收记录（MVP）

> **口径**：`design/ARCHITECTURE.md` §14.1 的 **17 项 P0 验收** + `design/IMPLEMENTATION.md` §9（每项的证据形态）
> 与 §15.1（MVP 状态总览）。本文只记录"**做到哪一步 + 证据在哪**"，不改验收标准。
>
> **状态图例**：✅ 做过（有可复跑证据） / 🟡 部分或 mock（形态通、真依赖未接） / ⬜ 未做。
>
> **来源**：QA 侧结论见 [`scripts/e2e/acceptance-matrix.md`](../../scripts/e2e/acceptance-matrix.md)；
> 带 🔧 的条目是 infra 在 kind 集群里本轮实跑补的证据（2026-11-05）。
> 最后更新：**2026-11-05**。状态会变——改这里时同步改时间戳和对应行，别只改字。

---

## 1 十七项总表

| # | 验收项（§14.1） | MVP 状态 | 证据 / 备注 | 复跑 |
|---|---|---|---|---|
| 1 | Run 提交与完整状态链 | 🟡 部分 | 🔧 service 形态全链路实跑通：`r-20261105-KWYE` `queued→testing→succeeded`，`state_events` 17 条（`run.submitted` / `task.state_changed` / `attempt.started/finished` / `cleanup.started` / `run.finished`），2/2 Task succeeded。缺：失败/服务不可用两条终态 | 见 §2.1 |
| 2 | Task 独立提交（`POST /tasks`） | ⬜ 未做 | Lead 裁决不在 MVP（`IMPLEMENTATION` §15.1 第 13 行）；`/api/v1/tasks` 只有查询 | — |
| 3 | Genet 适配三项（部署/状态/Endpoint） | 🟡 mock | 🔧 `deploy` 形态实跑通：`r-20261105-PRBV` `queued→deploying→testing→succeeded`（约 10s），`deploy.state_changed` 事件带 `deployment_id`，Endpoint 由 mock 注入（`TESTPLANE_MOCK_ENDPOINT_URL`）。真实 Genet 未接（D-14） | 见 §2.2 |
| 4 | Pre-Check 注入与阻断 | 🟡 部分 | 🔧 注入实跑：每次提交自动生成 `seq=0` 的 `precheck` Task（`role: pre_check`），`r-20261105-KWYE` 的 `t-DXVJBD`。缺：失败 → 其余 `SKIPPED` + 终态 `SERVICE_UNAVAILABLE` 的失败注入 | `scripts/e2e/failure-injection.ps1` |
| 5 | Registry 发现与校验 | ✅ 做过 | 🔧 启动扫描 `Tester 注册表就绪 dir=/app/testers count=1`；`GET /api/v1/testers` 返回 `endpoint-probe` 全量字段（含 `entry: /tester/run.sh`、`role: pre_check`、`parameters`）。校验 15 条由 `internal/registry` 单测覆盖（`registry_test.go`）。缺：一个非法 `tester.yaml` 的端到端拒绝样例 | `go test ./internal/registry/...` |
| 6 | Tester 契约（入口/argv/输出/result.json） | ✅ 做过 | 🔧 真实镜像 `registry.dev.huawei.com/testplane/endpoint-probe:1.0.0` 在 kind 里跑通；`injected_argv` 见 `--url=` / `--model_name=`；`result.json` 符合附录 A（`schema_version/run_id/task_id/attempt_id/status/workload/metrics`），落在 PVC 最终路径 | 见 §2.1 |
| 7 | Tester 独立运行（不依赖平台） | ✅ 做过 | QA `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 `result.json` 均符合设计 | `pwsh scripts/e2e/probe-check.ps1` |
| 8 | K8s Executor（超时/取消） | 🟡 部分 | 🔧 Job 创建/完成/观测已通：`tp-t-dxvjbd-a1`、`tp-t-f5w42q-a1` 均 `Complete 1/1`；渲染出 `subPath=<run>/tasks/<task>/attempts/<n>`、`/tmp` emptyDir、`fsGroup 10001`。缺：`TASK_TIMEOUT` 与"取消后 30s 内 Job/Pod 消失"未验证（当前有几个 `cancelling` 卡住的 Run） | `scripts/e2e/run-local.ps1` |
| 9 | 状态机与重启恢复 | 🟡 部分 | 转移表单测全绿（`go test ./internal/domain/...`）；缺：杀 controller 后状态最终一致的对账演练 | 需手工：`kubectl delete pod` + 观察 |
| 10 | Attempt 与重试 | ⬜ 未做 | `retry_on` 命中才重试、历史 Attempt 不被覆盖——未跑 | 需失败注入 |
| 11 | 失败策略（`abort_run`） | 🟡 mock | QA e2e 已断言 `abort_propagation`（precheck 失败 → 其余 `skipped`）；真实失败注入未跑 | `scripts/e2e/failure-injection.ps1` |
| 12 | Run 编排全链路 | 🟡 部分 | 🔧 两条链都贯通到 `succeeded`：service 形态（`r-20261105-KWYE`）、deploy 形态（`r-20261105-PRBV`，含 cleanup 阶段）。缺：四终态（成功/部分失败/服务不可用/取消）只覆盖"成功" | §2.1、§2.2 |
| 13 | 报告结构（report / result / 错误码） | ✅ 做过 | 🔧 终态后异步生成齐全：`r-20261105-JW8Z` 的 `<run_id>/` 下有 `report.json`(653B) / `report.md`(770B) / `metadata.json` / `events.jsonl`；`GET /runs/{id}/report` 与 `/report.md` 均 200；`artifacts` 表 12 行（result 2 / log 2 / raw 4 / report 4）。`report.json` 含 `schema_version` / `counts` / `tasks[].duration_ms` / `failures[]`。⚠️ 之前 404 是**镜像落后于代码**（修复 11:03 落地，重灌后正常），见 §2.3 | 见 §2.3 |
| 14 | 产物持久化 | ✅ 做过 | 🔧 删除 Job+Pod 后（`kubectl delete job tp-t-dxvjbd-a1 tp-t-f5w42q-a1`），宿主 `.data/storage/r-20261105-KWYE/.../attempts/1/` 的 `result.json`、`logs/probe.log`、`raw/*` 仍在且可读——产物直落最终路径（06 §3.3）。Run 级报告缺失见 #13 | 见 §2.4 |
| 15 | 失败隔离（一败一成功 → `COMPLETED_WITH_ERRORS`） | ⬜ 未做 | 需要第二个可执行 Tester 构造混合结果 | 需构造 Run |
| 16 | Web UI（列表/详情/报告/日志/提交/取消） | 🟡 部分 | `pnpm --dir web build` 通过；🔧 `GET /` 返回 `200 text/html`（控制面单端口同时服务静态资源与 API）。缺：Playwright 冒烟 | `pnpm --dir web build` |
| 17 | API 契约与错误码 | ✅ 做过 | QA：规格侧 12/12、实现侧 32/32，合计 **44/44**；`tools/contract` 可复跑 | `cd tools/contract && go test ./... && go run . --mode spec` |

**合计**：✅ 6 项（#5 #6 #7 #13 #14 #17）· 🟡 8 项（#1 #3 #4 #8 #9 #11 #12 #16）· ⬜ 3 项（#2 #10 #15）。
其中 #2 是 Lead 裁决的 MVP 排除项，其余 2 项（重试、失败隔离）是真实缺口。

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

---

## 3 已知缺口（按优先级）

1. **取消语义未验证**（#8）：集群里有若干 `cancelling` 卡住的 Run（Job/Pod 未在 30s 内消失）。
2. **失败注入类验收未跑**（#4 #10 #11 #15）：需要"precheck 必失败"与"两 Task 一败一成"两类样本。
3. **重启恢复**（#9）：杀 controller 后按 DB 对账的演练未做。
4. **Web UI 交互验收**（#16）：Playwright 冒烟未做（构建与静态资源已验证）。

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
| 镜像 | `testplane-server:dev`、`testplane/endpoint-probe:1.0.0`（同时打 `registry.dev.huawei.com/…` tag 供 tester.yaml 引用） |
| RBAC | namespace 级 `Role`（jobs/pods/pods-log/pvc/events/leases）+ 只读 `ClusterRole testplane-view`；**不授 secrets** |
