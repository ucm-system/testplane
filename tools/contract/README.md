# contract：TestPlane 契约校验器

以 [`server/api/openapi.yaml`](../../server/api/openapi.yaml) 为唯一机器契约（07 §10），
分两层校验：

- **规格侧（`--mode spec`）**：不需要控制面，离线可跑；端点集合以
  `design/views/07-api.md §5`（+ §2.4 认证表 + §5.5 运维段）为期望，与 openapi 做双向差集。
- **实现侧（`--mode live`）**：对运行中的控制面逐个打 MVP 的 25 个 operation。

> e2e 端到端（Run 生命周期）见 [`tools/e2e`](../e2e/README.md)；
> 起服务、mock、Kind 的操作手册见 [`scripts/e2e/README.md`](../../scripts/e2e/README.md)。

## 快速开始

```bash
cd tools/contract
go test ./...

go run . --mode spec              # 15 项规格检查，不需要控制面
go run . --mode live              # 25 个 operation 逐个打，默认 http://127.0.0.1:8080
go run . --mode all --json-out ../../.data/contract.json
go run . --mode spec --allow-missing none   # 严格比对：白名单里的 25 条也判红
```

`--allow-missing` 默认读 `tools/contract/allow-missing.txt`；白名单每行
`METHOD PATH | 理由`，理由为空/重复会直接报错。**只放行“设计有、openapi 无”**；
openapi 多出来的端点永远判红。

构建：

```bash
go build -o ../../bin/contract .
../../bin/contract --mode all
```

## 规格侧检查（15 项）

| 检查 | 断言 |
|---|---|
| `spec.openapi_3_1` | `openapi: 3.1.x` |
| `spec.design_endpoints_parsed` | 从 `07-api.md` §5（+§2.4/§5.5）解析出非空端点集合 |
| `spec.operation_count` | openapi operation 数 = 设计端点数 − 白名单放行数 |
| `spec.design_endpoints_present` | “设计有、openapi 无”的端点全部登记在白名单并写理由，否则判红 |
| `spec.openapi_endpoints_in_design` | openapi 的每个 method+path 都能在设计文档找到（多出来判红） |
| `spec.endpoints_match_design` | 上面两个方向的差集都为空（白名单放行不算空） |
| `spec.operation_ids` | `operationId` 非空且唯一 |
| `spec.excluded_endpoints_absent` | `POST /tasks`、`/retry`、`/profiles`、`/secrets`、`/admin/*` 等确实不存在 |
| `spec.security_coverage` | `/api/v1/*` 全部要求 bearerAuth；`/healthz`、`/readyz` 不要鉴权 |
| `spec.success_response_defined` | 每个 operation 至少一个 2xx |
| `spec.error_responses_use_error_schema` | 所有 JSON 错误响应引用 `components.schemas.Error`（healthz/readyz 的 text/plain 503 豁免） |
| `spec.error_schema_required` | `Error.required ⊇ {code,message,retryable,trace_id}` |
| `spec.path_params_declared` | 每个 `{param}` 都有 `in: path` 参数声明（支持 `$ref`） |
| `spec.refs_resolvable` | 全文档 `$ref` 可解析、无循环 |
| `spec.error_codes` | `ErrorCode` 枚举 ≥20 个且包含关键码 |

**差集报告**：每次都会把“设计有、openapi 无（白名单放行 N 条，带理由）”作为 warning
打印；白名单里登记了但设计文档已不存在的条目也会 warning 提示清理。

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
| 状态相关读 | `/report`、`/report.md`、`/deploy`、attempt `result`/`logs` | `/report`、`/report.md`：非终态 Run 允许 404，**终态 Run 必须 200**；`/deploy`：Run 无 deploy 时允许 404；attempt result/logs 允许尚未产出的 404；404 仍须是 Error |
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

## 当前状态（2026-11-05，PR #13 / 票 #16 修复后）

- 规格侧：**15/15 通过**（`go run . --mode spec`）；设计 50 个端点，openapi 25 个，
  白名单登记 25 条“设计有、实现无”（每条带理由）。
- 实现侧：**32/32 通过**，覆盖全部 25 个 operation、任务列表完整性（seq=0 precheck）、
  SSE 连通性、404/400 错误体、取消语义；`/report` 的 404 只在非终态 fixture 上放行。
- 合计 `--mode all`：**47/47 通过**。
- 负向证据：`--allow-missing none` 时 25 条未登记缺失端点全部判红（不再自证）；
  单测 `TestDesignChecksCatchUnregisteredMissing` / `TestArtifactResponseTerminalSemantics`
  覆盖“改前能过、改后必须红”。

## 设计取舍

- **为什么不用通用 JSON Schema 校验器**：spec 已由 redocly lint 把关；实现侧只关心
  "响应有没有被包一层 / 字段名拼错 / 错误体形状不对"这类契约漂移，校验 `required`
  足够，且不引入解释器差异带来的误报。
- **为什么从设计文档解析期望集合**：旧实现硬编码 25 个 operation 再跟 openapi 比，
  等于自己跟自己比，抓不到 `07-api.md §5` 里设计有、实现无的端点（PR #13 / 票 #16）。
  现在期望来自设计文档，差集双向输出；未登记缺失必须逐条写理由才能放行。
- **为什么允许状态相关 404 要区分状态**：`/report` 在 Run 未结束前按 07 §5.1 就是
  404 + `Retry-After`；但终态 Run 还 404 就是 bug。因此只在非终态 fixture 上放行 404，
  终态必须 200；404 本身仍要过 Error 形状校验。
