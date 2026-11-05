# 07 API 视图：REST 契约

> **覆盖：** 约定、鉴权、错误模型、Manifest 提交、端点目录、SSE、分页/重复提交/限流、OpenAPI 与兼容规则。
> **不覆盖：** 状态语义（[02](02-logical-view.md)）、表结构（[03](03-data-view.md)）、页面交互（impl/web-and-delivery）。
> **唯一性：** 端点以本文件为权威；`impl/control-plane.md` C-8.2 是摘要。机器契约以 `server/api/openapi.yaml` 为准，本文解释为什么这样设计。

---

## 1. 约定

| 项 | 规则 |
|---|---|
| Base | `/api/v1`（`/healthz`、`/readyz`、`/metrics` 不带版本） |
| 内容 | 请求/响应 `application/json; charset=utf-8`；Manifest 可传 YAML 字符串或 JSON 对象；日志 `text/plain`；SSE `text/event-stream` |
| 时间 | RFC3339，带时区；服务端统一返回 UTC（`Z`） |
| ID | 不透明字符串；客户端不得解析 |
| 字段命名 | `snake_case`（与 ARCHITECTURE 附录一致） |
| 未知字段 | 请求：忽略；响应：允许新增（客户端不得因未知字段失败） |
| 大小 | 请求体 ≤ 1 MiB；单参数 ≤ 64 KiB；`args` argv ≤ 1000 项 |
| 重复提交 | **不做幂等**：每次 `POST` 创建新资源；重复提交 = 多个 Run（D-37） |
| 并发 | 写操作用 `If-Match: <revision>` 可选；冲突返回 409 |
| Trace | 响应头 `X-Request-Id`；错误体带 `trace_id` |
| 版本 | URL 主版本；新增字段/端点不升版本；破坏性变更升 `/api/v2` + 至少一个版本兼容期 |

---

## 2. 鉴权与 RBAC

### 2.1 两种调用方

| 调用方 | 方式 | 说明 |
|---|---|---|
| Web UI | OIDC authorization code + PKCE → 会话 Cookie | `HttpOnly; Secure; SameSite=Lax; Path=/`；写操作要求 `X-CSRF-Token`（双提交，值来自 `GET /me`） |
| Jenkins / Agent / CLI | `Authorization: Bearer <JWT>` | 统一身份源签发；JWKS 校验；`aud == clientId`；`exp/nbf` 容差 60s |

无匿名端点（`/healthz`、`/metrics` 除外，按部署要求限制）。

### 2.2 角色

| 角色 | 能力 |
|---|---|
| `viewer` | 读 Run/Task/Attempt/报告/日志/Tester/Profile/队列 |
| `operator` | viewer + 提交 Run/Task、取消、重试、填 env/secrets |
| `admin` | operator + Profile/Cluster/Secret 维护、审计、人工清理产物 |

### 2.3 端点 × 角色

| 组 | viewer | operator | admin |
|---|---|---|---|
| `GET /runs*`、`/tasks*`、`/testers*`、`/profiles`、`/clusters`、`/queue-status`、`/meta`、`/me` | ✅ | ✅ | ✅ |
| `POST /runs`、`POST /tasks`、`POST /runs/{id}/cancel|retry`、`POST /tasks/{id}/cancel|retry` | — | ✅ | ✅ |
| `GET /secrets` | — | ✅ | ✅ |
| `/admin/*` | — | — | ✅ |
| 下载产物/日志 | ✅ | ✅ | ✅ |

`403` 由服务端统一判定，前端不推断角色（§11.4）。

### 2.4 认证端点

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/auth/login?return=<path>` | 302 到 OIDC；`return` 仅允许站内相对路径 |
| `GET` | `/auth/callback` | 换取会话，Set-Cookie，302 回 `return` |
| `POST` | `/auth/logout` | 清会话 |
| `GET` | `/me` | `{user, role, csrf_token, groups}` |

---

## 3. 错误模型

```json
{
  "code": "TASK_TIMEOUT",
  "message": "Task 执行超过 14400 秒被终止",
  "detail": { "task_id": "t-0007", "timeout_seconds": 14400 },
  "retryable": false,
  "trace_id": "01J..."
}
```

`code` 只用 ARCHITECTURE 附录 D 的枚举。HTTP 映射：

| HTTP | code（示例） |
|---|---|
| 400 | `VALIDATION_ERROR`、`TESTER_NOT_FOUND`、`EXECUTION_PROFILE_NOT_FOUND`、`RUN_TOO_LARGE` |
| 401 / 403 | `UNAUTHENTICATED` / `FORBIDDEN` |
| 404 | `NOT_FOUND` |
| 409 | `CONFLICT`、`TESTER_VERSION_PINNED`、`CANCELED` |
| 413 | `VALIDATION_ERROR`（体过大，detail 说明限制） |
| 429 | `RESOURCE_EXHAUSTED`（并发槽位/配额） |
| 500 | `INTERNAL_ERROR`、`TESTER_NONZERO_EXIT`、`RESULT_MISSING`、`RESULT_INVALID`、`CLEANUP_FAILED` |
| 502 | `IMAGE_PULL_FAILED`、`MOUNT_UNAVAILABLE`、`ENDPOINT_UNREACHABLE`、`PRECHECK_FAILED` |
| 503 | `EXECUTOR_UNAVAILABLE` |
| 504 | `TASK_TIMEOUT` |
| 507 | `ARTIFACT_QUOTA_EXCEEDED` |

校验失败时 `detail.issues[] = [{path, code, message}]`，`path` 用 `tests[2].args` 形式，供 UI 定位。

> **Pre-Check 错误对象约定**：Pre-Check Task 的 `error` 必须带实例级 `retryable` 与 `detail.http_status`（4xx/认证失败置 `false`）。终态判定以实例字段为准，不只看码级默认值（01 UC-07、§5.11）。

---

## 4. 提交 Manifest

### 4.1 请求

```http
POST /api/v1/runs?dry_run=false
Authorization: Bearer ...
Content-Type: application/yaml

run:
  name: qwen3-32b-daily
  deploy:
    provider: genet
    parameters: { model: Qwen3-32B }
  precheck: auto
  env: { LOG_LEVEL: info }
  secrets: { ENDPOINT_API_KEY: endpoint/prod-qwen }
  tests:
    - name: accuracy-a
      tester: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      args: "--dataset=/data/a"
      on_failure: continue
  cleanup: { policy: always }
```

也接受 JSON 等价对象。`dry_run=true` 只校验不落库，返回：

```json
{ "valid": false,
  "issues": [ { "path": "tests[0].args", "code": "PUBLIC_PARAM_CONFLICT",
                "message": "--url 由平台注入，args 中不得出现" } ],
  "resolved_preview": { "precheck": { "tester": "endpoint-probe", "seq": 0 } } }
```

### 4.2 响应（202 Accepted）

异步编排：202 表示已受理并落库，后续状态由 SSE/轮询获取。

```json
{
  "run": { "id": "r-20261105-0001", "status": "queued", "kind": "run",
           "submitted_at": "2026-11-05T01:00:00Z" },
  "tasks": [ { "id": "t-0001", "name": "precheck", "status": "pending" },
             { "id": "t-0002", "name": "accuracy-a", "status": "pending" } ]
}
```

### 4.3 独立 Task

`POST /tasks` 接受 `task:` 形态（§10.3），**禁止 `deploy`**，`service.url` 必填，返回 `{task_id, run_id}`（合成 `kind='task'` 的占位 Run）。

### 4.4 重复提交：不做幂等（D-37）

**每次 `POST /runs` / `POST /tasks` 都创建新资源，没有 `Idempotency-Key`、没有提交指纹、不判重。**

| 情况 | 结果 |
|---|---|
| 客户端超时后重发同一份 Manifest | **两个 Run**；旧的需自行取消 |
| 两次独立提交，Manifest 完全一致 | **两个 Run，都执行** |
| 想避免重复 | 提交前用 `GET /runs?submitter=...` 查；或提交后立即取消多余的那个 |

理由：平台无法判断两次提交是否等价——硬件型号、驱动、HDK 版本、集群负载、被测服务部署参数对平台透明；镜像 tag 可变，相同内容也可能已换代码。判定"是否同一次测试"是提交者的责任，平台只保证两个 Run 互不覆盖、可独立取消（D-37）。

镜像事实仍记录：`attempts.image_digest`（K8s 实际拉取），供事后区分同 tag 不同代码（03 §4.9）。

---

## 5. 端点目录

> 统一前缀 `/api/v1`。`{id}` 为 Run/Task ID；`{idx}` 为 Attempt 序号（从 1 开始）。

### 5.1 Run

| 方法 | 路径 | 说明 | 成功 |
|---|---|---|---|
| `POST` | `/runs` | 提交 Run（YAML/JSON）；`?dry_run=true` 只校验 | 202 / 200(dry) |
| `GET` | `/runs` | 列表：`status`、`kind`、`tester`、`submitter`、`cursor`、`limit` | 200 |
| `GET` | `/runs/{id}` | 详情 + Task 摘要 | 200 |
| `GET` | `/runs/{id}/tasks` | Task 列表（分页） | 200 |
| `POST` | `/runs/{id}/cancel` | 取消（含 Deploy 排队） | 202 |
| `POST` | `/runs/{id}/retry` | 同 Run 内重跑失败/跳过的 Task | 202 |
| `GET` | `/runs/{id}/retry-plan` | 预览：将复用 / 将重跑 | 200 |
| `GET` | `/runs/{id}/events` | SSE（`Last-Event-ID`） | 200 |
| `GET` | `/runs/{id}/events/history` | 事件历史 JSON（`since_seq`、`limit`） | 200 |
| `GET` | `/runs/{id}/report` | `report.json` | 200 / 404+Retry-After |
| `GET` | `/runs/{id}/report.md` | `text/markdown` | 200 |
| `GET` | `/runs/{id}/manifest` | Manifest 快照（"照上次再来一次"） | 200 |
| `GET` | `/runs/{id}/deploy` | 部署快照（状态/排队/Endpoint/Pods） | 200 |
| `GET` | `/runs/{id}/deploy/logs` | `?pod=&segment=&tail=` | 200 |
| `GET` | `/runs/{id}/artifacts` | Run 级产物清单 | 200 |
| `GET` | `/runs/{id}/files` | `?path=` 下载（Range） | 200/206 |

`GET /runs/{id}` 响应要点：

```json
{
  "id": "r-20261105-0001", "name": "qwen3-32b-daily", "kind": "run",
  "status": "deploying", "cleanup_policy": "always",
  "submitted_by": "zhangsan", "submitted_at": "...", "finished_at": null,
  "endpoint": null,
  "deploy": { "state": "queued", "queue_position": 12, "queued_at": "..." },
  "error": null,
  "tasks": [ { "id": "t-0001", "name": "precheck", "status": "pending",
               "waiting_reason": "precheck_gate", "attempts": 0 } ]
}
```

### 5.2 Task / Attempt

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/tasks` | 跨 Run 列表（`status`、`tester`、`cursor`） |
| `GET` | `/tasks/{id}` | 详情 + Attempt 列表 |
| `POST` | `/tasks/{id}/cancel` | 取消单个 Task |
| `POST` | `/tasks/{id}/retry` | 新建 Attempt |
| `GET` | `/tasks/{id}/attempts` | Attempt 列表 |
| `GET` | `/tasks/{id}/attempts/{idx}/result` | `result.json` |
| `GET` | `/tasks/{id}/attempts/{idx}/logs` | `?stream=stdout|stderr&offset=&limit=&follow=1` |
| `GET` | `/tasks/{id}/attempts/{idx}/artifacts` | 该 Attempt 产物清单 |

日志/结果响应头：

```http
Content-Type: text/plain; charset=utf-8
X-Log-Offset: 1048576
X-Log-Coverage: 2026-11-05T01:00:00Z/2026-11-05T01:32:10Z
```

超过 10k 行由前端虚拟滚动；服务端单次返回 ≤ 64 KiB。

### 5.3 Tester / 环境（只读）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/testers` | 目录（`role`、`orphaned` 过滤） |
| `GET` | `/testers/{name}` | 最新版本 |
| `GET` | `/testers/{name}/versions/{version}` | 指定版本（含 parameters/env 声明） |
| `GET` | `/profiles` | Execution Profile + 挂载能力 |
| `GET` | `/clusters` | 集群概要（不返回凭据） |
| `GET` | `/secrets` | Secret 名称与说明（operator） |
| `GET` | `/queue-status` | 平台槽位 + Volcano 等待原因 |
| `GET` | `/meta` | `{version, api_version, features}` |

### 5.4 Admin

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST/PUT/DELETE` | `/admin/profiles` | Profile 维护 |
| `POST/PUT/DELETE` | `/admin/clusters` | 集群登记（`kubeconfig_key`） |
| `GET/POST` | `/admin/secrets` | 引用列表/登记 |
| `POST` | `/admin/secrets/{name}/rotate` | 轮换校验 + 审计 |
| `GET` | `/admin/audit` | 审计（`actor`、`action`、时间） |
| `DELETE` | `/admin/runs/{id}/artifacts` | 人工清理产物 |

### 5.5 运维

`GET /healthz`（进程存活）、`GET /readyz`（DB + 存储可写 + 迁移版本；不含 K8s）、`GET /metrics`（Prometheus）。

---

## 6. 分页、过滤与排序

```http
GET /api/v1/runs?status=testing,deploying&kind=run&limit=50&cursor=eyJ0cyI6...
```

```json
{ "items": [ ... ], "next_cursor": "eyJ0cyI6..." , "has_more": true }
```

- `cursor` 不透明，服务端编码 `(submitted_at, id)` 倒序；客户端只回传。
- `limit` 默认 50、上限 200。
- 多值过滤用逗号；未知过滤字段返回 400。
- 排序固定为提交时间倒序；Task 列表按 `run_id, seq`。

---

## 7. SSE

```http
GET /api/v1/runs/r-20261105-0001/events
Accept: text/event-stream
Last-Event-ID: 42
```

```text
id: 43
event: task.state_changed
data: {"seq":43,"type":"task.state_changed","run_id":"r-...","task_id":"t-0002","at":"...","payload":{"from":"queued","to":"preparing"}}
```

规则：

| 项 | 规则 |
|---|---|
| 回放 | 从 `Last-Event-ID+1` 开始；窗口 24h、单次 ≤1000 条；越窗发 `event: resync_required`，客户端整页重建 |
| 心跳 | 15s 一行 `: ping` |
| 断开 | 服务端 60s 无事件或客户端断开即释放；客户端指数退避重连（1/2/4/8/30s） |
| 并发 | 同一用户同一 Run 的连接上限（P1 限流位），超限返回 429 |
| 内容 | 只发状态事实，不发日志正文、不发凭据 |

事件类型：`run.submitted`、`run.state_changed`、`task.state_changed`、`attempt.started`、`attempt.finished`、`deploy.state_changed`、`cleanup.started`、`run.finished`、`report.written`、`resync_required`。

---

## 8. 取消、重试与重复调用语义

| 操作 | 前置 | 返回 | 重复调用 |
|---|---|---|---|
| `cancel run` | 非终态 | 202 + `{run_id,status:"cancelling"}` | 已是 `cancelling/canceled` 返回 200 同状态 |
| `cancel task` | 非终态 | 202 | 同上 |
| `retry run` | 终态且存在失败/跳过 Task | 202 + 计划 | 已在执行返回 409 `CONFLICT` |
| `retry task` | 终态且非 `canceled` | 202 | 已在执行返回 409 |

取消与自然结束竞态：服务端以先落库状态为准，客户端以 SSE 推送为最终结果（不做乐观更新）。

---

## 9. 限流与配额

| 项 | P0 默认 |
|---|---|
| 单用户写请求 | 10 rps（令牌桶，超限 429 `RESOURCE_EXHAUSTED`） |
| SSE 连接 | 每用户 20、每 Run 5（P1 可配） |
| 单 Run Task 数 | 200（`RUN_TOO_LARGE`） |
| 全局并发 Task | 20（排队不拒绝，返回 `waiting_reason`） |
| 请求体 | 1 MiB（413 + `VALIDATION_ERROR`） |

---

## 10. OpenAPI 与契约测试

- `server/api/openapi.yaml` 是唯一机器契约；`oapi-codegen` 生成服务端接口，`openapi-typescript` 生成前端类型（生成物入库）。
- CI：`spectral lint` + 重生成后 `git diff --exit-code`；生成失败或 diff 非空即阻断。
- 每个错误码至少一个 handler 测试断言 `code` 与 HTTP 状态；`schemas/` 对样例做双向校验。
- 新增字段属兼容变更；删改字段、改枚举值属破坏性变更，走 ARCHITECTURE §15 并升版本。

---

## 11. 示例：一次完整调用

```bash
# 1) 提交
curl -sS -X POST "$API/api/v1/runs" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @run.yaml

# 2) 轮询状态
curl -sS "$API/api/v1/runs/r-20261105-0001" -H "Authorization: Bearer $TOKEN"

# 3) 订阅事件
curl -N "$API/api/v1/runs/r-20261105-0001/events" \
  -H "Authorization: Bearer $TOKEN" -H "Last-Event-ID: 0"

# 4) 取结果
curl -sS "$API/api/v1/tasks/t-0002/attempts/1/result" -H "Authorization: Bearer $TOKEN"

# 5) 下载原始产物
curl -OJ "$API/api/v1/runs/r-20261105-0001/files?path=tasks/t-0002/attempts/1/raw/perf.xlsx" \
  -H "Authorization: Bearer $TOKEN"
```
