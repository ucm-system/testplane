# e2e：mock LLM + 端到端脚本操作手册

> 票 #3（测试与端到端验证）。目标：**证明它能跑**，而不是"看起来能跑"。
> 工具实现与断言清单见 [`tools/e2e/README.md`](../../tools/e2e/README.md)；
> mock 的端点与故障参数见 [`tools/mock-llm/README.md`](../../tools/mock-llm/README.md)。

## 现状（2026-11-05）

| 项 | 状态 |
|---|---|
| `tools/mock-llm`（OpenAI 兼容假服务 + 故障注入） | ✅ 完成，18 个单测通过；真实 `endpoint-probe` 五种场景验证通过 |
| `tools/e2e`（submit / wait / assert CLI） | ✅ 完成，22 个 fake 控制面测试通过；对真控制面 **31/31** 通过 |
| 无后端时干净报错 | ✅ 退出码 3，提示 `make dev` 与 8080，不崩 |
| 契约测试（`tools/contract`） | ✅ 规格 15/15 + 实现 32/32 = **47/47**（设计端点差集 + 白名单） |
| 前端冒烟（`pnpm build`、`GET /`） | ✅ build 通过、`GET /` 200 text/html |
| 失败注入（经平台，`service_unavailable` 路径） | ✅ 5/5（success/500/401/200包错误/slow-ready） |
| 一成一败混合结果（#15） | ✅ `mixed-result-check.ps1` 11/11 |
| 重启恢复演练（#9） | ✅ `restart-drill.ps1` 13/13（新镜像；Run `r-Q0VT`） |
| Web UI Playwright 冒烟（#16） | ✅ `web/tests/ui-smoke.spec.ts` 1 passed |
| deploy 形态（PR #18） | ✅ 正常 9/9；blocked 7/7；500 4/4（超窗仅单测） |
| 验收矩阵 17 项结论 | ✅ 见 [`acceptance-matrix.md`](acceptance-matrix.md) |

## 目录

```text
tools/mock-llm/          # OpenAI 兼容假服务（独立 module，只用标准库）
tools/e2e/               # e2e CLI（独立 module）+ fake 控制面单测
scripts/e2e/
├── README.md            # 本文件
├── run-local.ps1        # 一键：起 mock-llm + 跑 e2e
├── probe-check.ps1      # 用真实 endpoint-probe 反验 mock-llm（五种场景）
└── kind/mock-llm.yaml   # Kind 里的 mock-llm Deployment + Service
```

## 前置

- Go 1.25（`GOPROXY=https://goproxy.cn,direct`）；Docker；kind（`.tools\kind.exe`）。
- **控制面已启动**：PG（5432）+ 迁移 + `--role=all`，监听 **8080**。
  Windows 没有 `make`，按 `deploy/README`（Lead 提供）或 infra 的命令起；不要占用 5433。
- 控制面的 Registry 能看到 `endpoint-probe`，且存在可用的 `execution_profile`
  （内置模板默认 `k8s-npu`，用 `--profile` 改）。
- dev 鉴权（MVP 裁决）：`TESTPLANE_AUTH_MODE=dev`，`Authorization: Bearer <subject>` 即用户名，
  不带则用 `dev`、角色 `admin`；`GET /api/v1/auth/me` 可自检。

## 1. 起 mock-llm（宿主，:18080）

```powershell
cd D:\Project\TestPalne
go run ./tools/mock-llm                     # 或 go build -o bin/mock-llm ./tools/mock-llm
```

```bash
curl -sS http://127.0.0.1:18080/healthz      # ok
curl -sS http://127.0.0.1:18080/v1/models    # {"data":[{"id":"mock-llm"}]}
```

故障注入是**进程级**的（探针只会拼 `/chat/completions`，不会带你的查询参数）：

```powershell
go run ./tools/mock-llm -ready-after=12s     # 前 12 秒 503
go run ./tools/mock-llm -fail=500            # 所有请求 500
go run ./tools/mock-llm -fail=401            # 认证失败（不可重试）
go run ./tools/mock-llm -error-in-200        # HTTP 200 但正文是 error 对象
```

一个场景一个实例、不同端口即可同时存在：

```powershell
.\bin\mock-llm -addr=:18081 -fail=500
.\bin\mock-llm -addr=:18082 -fail=401
```

> 别用 `?fail=401` 这种查询参数去配 `service.url`：`run.sh` 会在 base URL 后拼
> `/chat/completions`，查询串会被拼坏。查询参数只适合直接 curl mock 时用。

## 2. 跑 e2e（宿主）

一键（会编译、起/收 mock、检查控制面、跑 `e2e run`）：

```powershell
pwsh scripts/e2e/run-local.ps1
pwsh scripts/e2e/run-local.ps1 -Expect succeeded -Profile k8s-npu
```

控制面没起时它**明确报错并退出 3**，不会留下 mock 进程：

```text
控制面不可达：http://127.0.0.1:8080
  先启动控制面（PG + 迁移 + --role=all，监听 8080），再重跑本脚本。
回收 mock-llm（PID 25208）
exit=3
```

手动分步：

```powershell
cd tools/e2e
go build -o ..\..\bin\e2e .

..\..\bin\e2e run  --mode service            # 提交 + 等待 + 断言（一条命令）
..\..\bin\e2e submit --mode service          # 只提交，打印 run_id
..\..\bin\e2e wait   --run-id r-...
..\..\bin\e2e assert --run-id r-...
```

- 默认 `--base-url http://127.0.0.1:8080`、`--api-prefix /api/v1`、`--user dev`。
- `--mode deploy`（默认）需要 Genet；本地没有 Genet 时用 `--mode service`。
- stdout 只有**一行 JSON**；人类摘要、状态轨迹、失败项在 stderr。
- 退出码：`0` 通过 / `1` 断言失败 / `2` 参数错误 / `3` 连不上控制面。

## 3. 失败注入（后端可跑后执行）

一键脚本（会部署 4 个故障 mock 变体、生成嵌套 Manifest、逐场景断言并记录实际终态）：

```powershell
pwsh scripts/e2e/failure-injection.ps1
pwsh scripts/e2e/failure-injection.ps1 -Only fail-401,success
pwsh scripts/e2e/failure-injection.ps1 -SkipApply -Cleanup
pwsh scripts/e2e/failure-injection.ps1 -DryRun            # 只生成 Manifest/打表，不连后端
pwsh scripts/e2e/failure-injection.ps1 -AllowKnownGaps    # 仅修复期临时豁免 report/logs
```

取消场景单独验证（严格断言 Run/Task/租约/Pod）：

```powershell
pwsh scripts/e2e/cancel-check.ps1 -BaseUrl http://127.0.0.1:8093
pwsh scripts/e2e/cancel-check.ps1 -DryRun          # 只打 Manifest/阈值，不连后端
```

票 #16 起：轮询上界 180s 与断言阈值 60s **分离**（`-AssertCancelSeconds` / `-LoopLimitSeconds`），
`docker exec`/`kubectl` 查询失败一律 fail-fast，并拆成 `*.query` 与 `*.empty` 两条断言，
不会再把“查询挂了”当成“无残留”。

超时路径（平台 watchdog 超时）同样有独立脚本：

```powershell
pwsh scripts/e2e/timeout-check.ps1 -BaseUrl http://127.0.0.1:8095
pwsh scripts/e2e/timeout-check.ps1 -DryRun
```

期望：`precheck:"off"` + 单个 test Task 超时 → Run `failed`（TASK_TIMEOUT 属平台 C 类）；
Attempt 容差 `timeout + 30s`（独立于轮询变量）；事件接受
`task.state_changed + payload.error.code=TASK_TIMEOUT`；kubectl/docker 查询失败同样 fail-fast。

`mock-llm-slow` 用 `-ready-after=86400s`（24h）而不是 1h：否则跑满 1h 后会真的就绪，
超时用例会“意外成功”。

## 3.1 重启恢复演练（#9）

```powershell
pwsh scripts/e2e/restart-drill.ps1 -BaseUrl http://127.0.0.1:8098 -DryRun
pwsh scripts/e2e/restart-drill.ps1 -BaseUrl http://127.0.0.1:8098
pwsh scripts/e2e/restart-drill.ps1 -RecoveryTimeoutSeconds 1   # 负向：必须 FAIL
```

- 先用 `kind/mock-llm-delayed.yaml`（15s 后才就绪）制造 running 窗口；
- 提交 2 个 test，等 testing + Attempt running，然后
  `kubectl delete pod -l app.kubernetes.io/name=testplaned --grace-period=0 --force`；
- 脚本自己管理 port-forward（8100 → 恢复后 8101），断言新 Pod Ready、Run 终态、
  租约/Job 清空、`attempt.finished` 有对账痕迹；`-RecoveryTimeoutSeconds 1` 时
  必须 exit 1（验证不会把“没恢复”当 PASS）。

## 3.2 一成一败混合结果（#15）

```powershell
pwsh scripts/e2e/mixed-result-check.ps1 -BaseUrl http://127.0.0.1:8096 -DryRun
pwsh scripts/e2e/mixed-result-check.ps1 -BaseUrl http://127.0.0.1:8096 -SkipApply
```

- `kind/mixed-router.yaml` 按 `Authorization` 分流：Task A `ENDPOINT_API_KEY=ok` →
  mock-llm（成功），Task B `ENDPOINT_API_KEY=fail` → mock-llm-500（失败）；
- 首次用 `kind load docker-image python:3.12-alpine --name testplane` 把 router 镜像装进集群；
- 断言 Run `completed_with_errors`、A succeeded/B failed、report counts 1/1、
  failures 非空、Run.error 取首个失败 Task（Pre-Check 优先）。

## 3.3 Web UI Playwright 冒烟（#16）

```powershell
cd web
pnpm add -D @playwright/test    # 仅首次
pnpm exec playwright test       # 自动起 dev server（5173），system Chrome
```

也可以先起服务再跑：`$env:PLAYWRIGHT_BASE_URL='http://127.0.0.1:8098'; pnpm exec playwright test`。
Spec 覆盖 `/runs` → 第一条 → 详情 Task 表 → `/submit` dry-run 报错可见。

## 3.4 deploy 形态（PR #18）

```powershell
pwsh scripts/e2e/deploy-check.ps1 -BaseUrl http://127.0.0.1:8107 -DryRun
pwsh scripts/e2e/deploy-check.ps1 -BaseUrl http://127.0.0.1:8107
pwsh scripts/e2e/deploy-check.ps1 -BaseUrl http://127.0.0.1:8107 -BlockedOnly  # fake-genet -sim-blocked
pwsh scripts/e2e/deploy-check.ps1 -BaseUrl http://127.0.0.1:8107 -FailureOnly  # fake-genet -sim-error 500
```

- 正常：`deploy:{provider:"genet"}` → `deploy.state` 依次 `queued → accepted/deploying → ready`，
  `endpoint.source=genet`，precheck `succeeded`，Run `succeeded`；终态后
  `helm list -n testplane-mock` 为空（本机无 helm CLI 时用 Helm release Secret 等价判断）。
- blocked：`-BlockedOnly` 断言 Run `service_unavailable`、`DEPLOY_BLOCKED` retryable=true、
  `deploy.failure.detail.reason=blocked`、`uninstall.state=verified`。
- 500：`-FailureOnly` 断言 Run `failed`、`DEPLOY_FAILED` retryable=false，
  并最多等 90s 让对账写出 `cleanup.failed` state event。
- **超窗 blocked 不做端到端**：配置下限 1h（`03 §3.2` CHECK），端到端构造不现实；
  由 genet 72h 单测 + 编排层 blocked 单测覆盖。

默认**严格**：report 必现（终态后 ≤2s，30s 等待足够）、`/logs` 必非空；只有显式
`-AllowKnownGaps` 才把当前已知的 report/logs 缺口从失败项里剔除，修复后不要带这个开关。

期望终态以 **C-5.8 终态判定优先级**为准（Lead 2026-11-05 已拍板）：

- **可重试**的 precheck 失败（窗口耗尽的 `PRECHECK_FAILED` / `ENDPOINT_UNREACHABLE`）→ Run `service_unavailable`；
- **不可重试**的 precheck 失败（401/403、200 包错误、DNS/TLS）→ Run `completed_with_errors`，
  precheck Task 是 `failed`（不是 `skipped`），其余 Task `skipped(upstream_abort)`；
- 平台准备期失败（如 Job 创建失败 `EXECUTOR_UNAVAILABLE`）→ `completed_with_errors`，**不是** `failed`。

| 注入 | mock 启动参数 | 探针行为 | Run 终态 | 断言命令要点 |
|---|---|---|---|---|
| 5xx 持续 | `-fail=500` | 窗口内重试，用尽后 `PRECHECK_FAILED(retryable=true)` | `service_unavailable` | `--expect service_unavailable --expect-task-error-code PRECHECK_FAILED --expect-retryable=true`；其余 Task 必须 `skipped(upstream_abort)`、无 Attempt |
| 认证失败 | `-fail=401` | **立即**失败，`retryable=false`，不耗满窗口 | `completed_with_errors` | `--expect completed_with_errors --expect-task-error-code PRECHECK_FAILED --expect-retryable=false` |
| 200 包错误 | `-error-in-200` | **立即**失败，`retryable=false` | `completed_with_errors` | 同上 |
| 就绪慢（窗口够） | `-ready-after=12s` | 多次 503 后成功 | `succeeded` | `--expect succeeded`；事件里能看到多次 `attempt` 才成功 |
| 就绪慢（窗口不够） | `-ready-after=3600s` | 窗口用尽，`retryable=true` | `service_unavailable` | 把 precheck 的 `--wait_timeout_seconds` 压到 5–10s（见下） |

**关键前置**：内置 Manifest 的 precheck 等待窗口是 60s，失败注入会跑很久。
要么用 `--manifest` 传自己写的 Manifest，把 precheck 的 `args` 压短：

```yaml
precheck:
  tester: endpoint-probe
  version: "1.0.0"
  args: "--wait_timeout_seconds=5 --retry_interval_seconds=1"
```

要么直接改 `--manifest` 里的 `deploy.parameters.endpoint` / `service.url` 指向注入端的端口。
断言 `abort_run` 时注意：其余 Task 是 `skipped`（`reason=upstream_abort`），**不是 `canceled`**。

## 4. Kind 里跑

```powershell
# 0) 集群（注意：已有同名集群先删再建；extraMounts 只在建集群时生效）
#    集群配置是模板，先渲染出本机路径，再建集群（见 deploy/README.md §2）
pwsh deploy\kind\render.ps1
.\.tools\kind.exe create cluster --config .data\kind-testplane.yaml

# 1) mock-llm 镜像：本地 build + load（不 push 任何仓库）
docker build -t testplane/mock-llm:dev tools/mock-llm
.\.tools\kind.exe load docker-image testplane/mock-llm:dev --name testplane

# 2) 部署 mock-llm（默认 namespace；要换就加 -n）
kubectl apply -f scripts\e2e\kind\mock-llm.yaml
kubectl rollout status deployment/mock-llm

# 3) 控制面：按 infra 的 Helm/compose 起；宿主访问用 port-forward
kubectl -n <testplane-ns> port-forward svc/testplane 8080:8080

# 4) 跑 e2e（集群内地址，不是 127.0.0.1）
bin\e2e run --mode deploy `
  --mock-url http://mock-llm.default.svc.cluster.local:18080/v1 `
  --base-url http://127.0.0.1:8080
# 或 service 模式（不经过 Genet，直接指向集群里的 mock）：
bin\e2e run --mode service `
  --mock-url http://mock-llm.default.svc.cluster.local:18080/v1
```

- `--mock-url` 是**给 Tester Pod 用**的地址：Pod 里没有宿主 `127.0.0.1`，必须用 Service DNS。
- `deploy` 模式最终 Endpoint 由 Genet 返回；`deploy.parameters.endpoint` 只是给 dev 假 Genet 的提示。
  假 Genet 不认这个参数时，用 `--mode service` 或自己的 `--manifest`。
- Tester Pod 需要拉得到 `endpoint-probe` 镜像（本地 kind 用 `kind load`，否则平台会 `IMAGE_PULL_FAILED`）。

## 5. 用真实探针反验 mock（可重复）

```powershell
pwsh scripts/e2e/probe-check.ps1
```

它会起 5 个不同故障的 mock，把 `testers/endpoint-probe/run.sh` 只读挂进一次性 alpine 容器运行
（不 build Tester 镜像、不改 `testers/`），逐场景比对 `result.json`。实测输出：

```text
scenario         ok exit status    code            retryable attempts seconds
slow-ready-12s True    0 succeeded -               -                6   10.70
ready          True    0 succeeded -               -                1    0.50
fail-401       True    1 failed    PRECHECK_FAILED False            1    0.50
fail-500       True    1 failed    PRECHECK_FAILED True             3    2.60
error-in-200   True    1 failed    PRECHECK_FAILED False            1    0.50
全部场景符合预期；result.json 与探针日志在 D:\Project\TestPalne\.data\qa-probe
```

产物在 `.data/qa-probe/<场景>/`（`result.json` + `container.log`，`.data/` 不入库）。

## 6. 排查

| 现象 | 先看什么 |
|---|---|
| `e2e` 退出码 3 | 控制面没起或端口不是 8080；`--base-url` 是否指对 |
| 所有接口 404 | `--api-prefix` 与后端挂载的前缀不一致（MVP 定稿 `/api/v1`） |
| `TESTER_NOT_FOUND` | `GET /api/v1/testers` 里有没有 `endpoint-probe`；镜像内 Registry 扫描是否成功 |
| `EXECUTION_PROFILE_NOT_FOUND` | `GET /api/v1/profiles` 的实际名字，用 `--profile` 传 |
| `report.present` 失败 | 报告是异步写的；e2e 会在 404 上重试 30s，仍 404 才是真问题 |
| `events.transitions_legal` 失败 | 详情里直接给出 `seq=… from -> to 不在转移表内`，对照 C-3.1/C-3.2 |
| `sse.connect` 失败 | `GET /api/v1/runs/{id}/events` 没返回 `text/event-stream`（或路由缺失） |
| Tester Pod 拉不到镜像 | `kind load docker-image ...` 或平台报 `IMAGE_PULL_FAILED` |

## 7. 第二阶段待办（依赖别人）

1. 契约测试：对 `server/api/openapi.yaml` 校验端点存在、成功状态码、错误体
   `{error:{code,message,detail,retryable}}`；每个错误码至少一例。
2. 经平台的失败注入：跑上面第 3 节的五行，记录实际终态与耗时。
3. 前端冒烟：`web/` 的 `pnpm build` 通过；`GET /` 返回 HTML。
4. 验收矩阵 17 项填「做过 / mock / 未做」，结论交 infra 写 `docs/runbook/acceptance.md`。
