# contract：TestPlane 契约校验器

以 [`server/api/openapi.yaml`](../../server/api/openapi.yaml) 为唯一机器契约（07 §10），
分两层校验：

- **规格侧（`--mode spec`）**：不需要控制面，离线可跑。
- **实现侧（`--mode live`）**：对运行中的控制面逐个打 MVP 的 25 个 operation。

> e2e 端到端（Run 生命周期）见 [`tools/e2e`](../e2e/README.md)；
> 起服务、mock、Kind 的操作手册见 [`scripts/e2e/README.md`](../../scripts/e2e/README.md)。

## 快速开始

```bash
cd tools/contract
go test ./...

go run . --mode spec              # 12 项规格检查，不需要控制面
go run . --mode live              # 25 个 operation 逐个打，默认 http://127.0.0.1:8080
go run . --mode all --json-out ../../.data/contract.json
```

构建：

```bash
go build -o ../../bin/contract .
../../bin/contract --mode all
```

## 规格侧检查（12 项）

| 检查 | 断言 |
|---|---|
| `spec.openapi_3_1` | `openapi: 3.1.x` |
| `spec.operation_count` | 25 个 operation（MVP 定稿数） |
| `spec.operations_match_mvp` | method+path 与 Lead 定稿集合逐条一致（多/少都红） |
| `spec.operation_ids` | `operationId` 非空且唯一 |
| `spec.excluded_endpoints_absent` | `POST /tasks`、`/retry`、`/profiles`、`/secrets`、`/admin/*` 等确实不存在 |
| `spec.security_coverage` | `/api/v1/*` 全部要求 bearerAuth；`/healthz`、`/readyz` 不要鉴权 |
| `spec.success_response_defined` | 每个 operation 至少一个 2xx |
| `spec.error_responses_use_error_schema` | 所有 JSON 错误响应引用 `components.schemas.Error`（healthz/readyz 的 text/plain 503 豁免） |
| `spec.error_schema_required` | `Error.required ⊇ {code,message,retryable,trace_id}` |
| `spec.path_params_declared` | 每个 `{param}` 都有 `in: path` 参数声明（支持 `$ref`） |
| `spec.refs_resolvable` | 全文档 `$ref` 可解析、无循环 |
| `spec.error_codes` | `ErrorCode` 枚举 ≥20 个且包含关键码 |

**已知口径**：spec 的 `Error` 是**扁平** `{code,message,detail,retryable,trace_id}`，
不是 `{error:{...}}`。工具每次都会打一条 warning 提醒；实现若返回嵌套形态，
`live.error_envelope_*` 会判红。

## 实现侧检查（25 个 operation）

实现侧从 spec 取"成功状态码 + 响应 schema + Content-Type"，不在代码里硬编码。
期望行为：

| 类别 | 端点 | 断言 |
|---|---|---|
| 运维 | `/healthz`、`/readyz` | 200 + `text/plain` |
| 身份/元信息 | `/meta`、`/me` | 200 + schema required |
| 列表 | `/runs`、`/tasks`、`/testers`、`/clusters`、`/queue-status` | 200 + 分页信封 required（`items`/`has_more`） |
| Registry | `/testers`、`/testers/endpoint-probe` | `endpoint-probe` 必须在目录里（K-3：无运行时注册） |
| 提交 | `POST /runs`、`?dry_run=true` | 202 `RunAccepted`（`run.id`/`tasks[]` 非空）；dry-run 200 `DryRunResult` |
| Run 读 | `/runs/{id}`、`/tasks`、`/events/history`、`/manifest` | 200 + schema required |
| 状态相关读 | `/report`、`/report.md`、`/deploy`、attempt `result`/`logs` | 200，**或** spec 声明的 404（报告异步生成 / 还没 Attempt），404 仍须是 Error |
| SSE | `/runs/{id}/events` | 200 + `text/event-stream`，5s 内至少一行 |
| 取消 | `POST /runs/{id}/cancel`、`POST /tasks/{id}/cancel` | 202（幂等时 200）+ `CancelAccepted`/`TaskCancelAccepted` |
| 错误体 | 不存在的 Run（404）、deploy+service 同时出现（400） | 扁平 Error 的 required 齐全、`code` 非空、Content-Type JSON |

实现侧会创建 **2 个 fixture Run**（A 用于读、B 用于取消），结束时尽力取消 A。
fixture Manifest 默认是 `service` 模式 + `endpoint-probe` + `profile: k8s-npu`，
指向 `--mock-url`（默认 `http://127.0.0.1:18080/v1`；Kind 用
`http://mock-llm.<ns>.svc.cluster.local:18080/v1`）。

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部检查通过 |
| 1 | 契约违约（spec 或实现） |
| 2 | 参数/spec 文件错误 |
| 3 | 连不上控制面 |

stdout 一行 JSON（`checks[]` 逐项给 detail），stderr 人类摘要；`--json-out` 另存缩进 JSON。

## 当前状态（2026-11-05）

- 规格侧：**12/12 通过**（`go run . --mode spec`）。
- 实现侧（最新源码旁路实例）：**32/32 通过**，覆盖全部 25 个 operation、
  任务列表完整性（seq=0 precheck）、SSE 连通性、404/400 错误体、取消语义。
- 合计 `--mode all`：**44/44 通过**。
- 对旧构建跑会看到 `live.listRunTasks_complete` 红（`seq > cursor` 的回归），
  这正是该检查存在的意义；对最新源码已全绿。

## 设计取舍

- **为什么不用通用 JSON Schema 校验器**：spec 已由 redocly lint 把关；实现侧只关心
  "响应有没有被包一层 / 字段名拼错 / 错误体形状不对"这类契约漂移，校验 `required`
  足够，且不引入解释器差异带来的误报。
- **为什么允许状态相关 404**：`/report` 在 Run 未结束前按 07 §5.1 就是 404 + `Retry-After`；
  把它判红会把"正确的异步语义"当成违约。404 本身仍要过 Error 形状校验。
- **为什么硬编码 25 个 operation**：契约测试的价值在于"多一个/少一个都红"；
  从 spec 反推只能验证 spec 自洽，抓不到端点漂移。
