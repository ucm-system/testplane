# 07 API 视图：REST 契约

> **覆盖：** 约定、鉴权、错误模型、Manifest 提交、端点目录、SSE、分页/重复提交/限流、OpenAPI 与兼容规则。
> **不覆盖：** 状态语义（[02](02-logical-view.md)）、表结构（[03](03-data-view.md)）、页面交互（impl/web-and-delivery）。
> **唯一性：** 端点以本文件为权威；`impl/control-plane.md` C-8.2 是摘要。机器契约以 `server/api/openapi.yaml` 为准，本文解释为什么这样设计。
> **v2 术语（ARCHITECTURE §0.2 / D-66～D-73）：** Run→Task、runner（实例）取代旧 Task、Tester→Runner、
> attempt 为 Task 级计数；路径 `/runs→/tasks`、`/testers→/runners`；重试 = 整 Task 新 attempt。

---

## 1. 约定

| 项 | 规则 |
|---|---|
| Base | `/api/v1`（`/healthz`、`/readyz`、`/metrics` 不带版本） |
| 内容 | 请求/响应 `application/json; charset=utf-8`；Manifest 可传 YAML 字符串或 JSON 对象；日志 `text/plain`；SSE `text/event-stream` |
| 时间 | RFC3339，带时区；服务端统一返回 UTC（`Z`） |
| ID | 不透明字符串；客户端不得解析。Task `t-YYYYMMDD-XXXX`；runner 执行记录以 `(task_id, attempt_no, seq)` 定位 |
| 字段命名 | `snake_case`（与 ARCHITECTURE 附录一致） |
| 未知字段 | 请求：忽略；响应：允许新增（客户端不得因未知字段失败） |
| 大小 | 请求体 ≤ 1 MiB；单个 runner 的 `input` 原文 ≤ 64 KiB（更大走产物文件 + 路径入 input，§5.12）；**业务输入不经过 argv**（entry argv 恒空，D-66） |
| 重复提交 | **不做幂等**：每次 `POST` 创建新资源；重复提交 = 多个 Task（D-37） |
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

**与 Genet 的边界（D-48，2026-11-05 调研结论）**：Genet 只有两类身份——浏览器侧的企业 OAuth 会话 Cookie，与机器侧的 `Authorization: Bearer <Open API Key>`（打 `/api/open/*`，key 绑定唯一 namespace）。它**没有** client-credentials / PAT，其 OIDC Provider 默认 `enabled:false` 且只有两个硬编码 client。因此：

- **Argus 的 Web 用户**复用团队统一身份源，Argus 自建 OIDC client 与 `ag_session` / CSRF（上表形态不变），**不复用 Genet 会话、不把 Genet 当 IdP**；
- **Argus → Genet 的机器调用**不发生在控制面：部署与拆除由内置 service-deploy / service-clean **runner** 执行（D-70），Genet Open API Key 以 Task 级 secret 注入这两个 runner（`AG_GENET_API_KEY`，存 Secret），平台控制面自身不直连 Genet，key 也不进日志；
- Genet 的 API Key 与 namespace 1:1（同 namespace 同时只允许一个活跃写 key），因此一个 Argus 实例需要**独占一组 namespace 或独占一把 key**，这一点必须在部署文档里写明。

**部署形态与身份（D-51，2026-11-05 用户确认）**：Argus **纯内网部署**，与 Genet 共用**同一个企业统一身份源**。边界是「同一 IdP，不同 client」：

- Argus 是该 IdP 的**独立 OIDC client**（自己的 `client_id`/回调/scope），**不是** Genet 的 client，也不共享 Genet 的会话；
- Web：authorization code + PKCE → `ag_session`（`HttpOnly; Secure; SameSite=Lax`）+ `X-CSRF-Token` 双提交；
- 服务间（Jenkins / Agent / `argus` CLI / MCP）：用**同一个身份源签发的 JWT**，平台用 JWKS 校验（`aud == clientId`、`exp/nbf` 容差 60s），RBAC 与人类用户同一套，不设后门；
- 本地开发仍用 `AG_AUTH_MODE=dev`（默认角色 `admin`、`Authorization: Bearer <subject>` 即身份），生产镜像必须显式切 `oidc` 并在 `/healthz` 打警告（`impl/control-plane.md §C-10`）。

**`oidc` 模式的 Cookie / CSRF / 登录细节（实现口径，2026-11-05 落定）**

| 项 | 取值 / 行为 |
|---|---|
| 会话 Cookie | `ag_session`；`HttpOnly; Secure; SameSite=Lax; Path=/`（纯内网 http 部署可用 `AG_SESSION_SECURE=false` 关 Secure） |
| 会话值 | **HMAC-SHA256 签名令牌**（`base64url(payload).base64url(sig)`，密钥 `AG_SESSION_SECRET` ≥32B）。payload = `{sub, email, role, groups, csrf, exp}`；不做 JWT、不用库 |
| 会话 TTL | `AG_SESSION_TTL`（默认 `8h`）；过期或签名不符 → 401 `UNAUTHENTICATED`，前端跳 `/auth/login` |
| 角色 | claims 的 `groups` 经 `AG_ROLE_MAP`（`ag-admins=admin,ag-ops=operator`）映射，取**最高**命中；未命中 → `viewer`；`sub` 为空 → 401 |
| 登录中间态 | `ag_auth_tx`：`{state, nonce, code_verifier, return_to, exp}` 的签名 Cookie，`Path=/auth`、10 分钟；服务端不存任何会话态（多副本天然一致） |
| CSRF | 双提交：`X-CSRF-Token` 必须等于 `GET /me` 返回的 `csrf_token`（由 `HMAC(secret, "csrf|sub|exp")` 派生，与会话绑定）。**只对 Cookie 会话的写操作生效**；Bearer 调用方不需要该头 |
| 缺 CSRF | 403 `FORBIDDEN`（不是 401——身份是有效的，缺的是防伪令牌） |
| 登出 | `POST /auth/logout` 清 `ag_session`（`Max-Age=0`）；IdP 侧会话不主动登出（无 end_session 依赖） |
| `dev` 模式 | `AG_AUTH_MODE=dev` 时 `/auth/*` 返回 404，`Authorization: Bearer <subject>` 直接作为身份、角色固定 `admin`；`AG_ENV ∉ {dev, local}` 时**拒绝启动**（dev 模式等于任何人都是 admin，见 `06 §6.1`） |

`/auth/login`、`/auth/callback`、`/auth/logout` 与 `/healthz`、`/readyz`、`/metrics` 一样**不带 `/api/v1` 前缀**，且 `/auth/login|callback` 匿名（登录前不可能有凭据）。

### 2.2 角色

| 角色 | 能力 |
|---|---|
| `viewer` | 读 Task/attempt/runner/报告/日志/Runner 目录/Profile/队列 |
| `operator` | viewer + 提交 Task、取消、重试（整 Task 新 attempt）、重放 clean（泄漏兜底）、填 env/secrets |
| `admin` | operator + Profile/Cluster/Secret 维护、审计、人工清理产物 |

### 2.3 端点 × 角色

| 组 | viewer | operator | admin |
|---|---|---|---|
| `GET /tasks*`、`/runners*`、`/profiles`、`/clusters`、`/queue-status`、`/meta`、`/me` | ✅ | ✅ | ✅ |
| `POST /tasks`、`POST /tasks/{id}/cancel`、`POST /tasks/{id}/retry`、`POST /tasks/{id}/clean` | — | ✅ | ✅ |
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
  "code": "RUNNER_TIMEOUT",
  "message": "runner 执行超过 14400 秒被终止",
  "detail": { "task_id": "t-0007", "attempt_no": 1, "seq": 3, "timeout_seconds": 14400 },
  "retryable": false,
  "trace_id": "01J..."
}
```

`code` 只用 ARCHITECTURE 附录 D（v2）的枚举。HTTP 映射：

| HTTP | code（示例） |
|---|---|
| 400 | `VALIDATION_ERROR`、`RUNNER_NOT_FOUND`、`RUNNER_INVALID`、`EXECUTION_PROFILE_NOT_FOUND`、`TASK_TOO_LARGE` |
| 401 / 403 | `UNAUTHENTICATED` / `FORBIDDEN` |
| 404 | `NOT_FOUND` |
| 409 | `CONFLICT`、`RUNNER_VERSION_PINNED`、`ATTEMPT_EXHAUSTED`、`CANCELED` |
| 413 | `VALIDATION_ERROR`（体过大，detail 说明限制） |
| 429 | `RESOURCE_EXHAUSTED`（并发槽位/配额） |
| 500 | `INTERNAL_ERROR`、`RUNNER_NONZERO_EXIT`、`EXPORT_INVALID`、`RESULT_MISSING`、`RESULT_INVALID`、`CLEANUP_FAILED`、`SLA_PRECHECK_FAILED`、`SLA_NO_FEASIBLE_CONCURRENCY` |
| 502 | `IMAGE_PULL_FAILED`、`MOUNT_UNAVAILABLE`、`EXECUTOR_POD_ERROR`、`ENDPOINT_UNREACHABLE`、`PRECHECK_FAILED`、`DEPLOY_FAILED`、`DEPLOY_BLOCKED` |
| 503 | `EXECUTOR_UNAVAILABLE` |
| 504 | `RUNNER_TIMEOUT` |
| 507 | `ARTIFACT_QUOTA_EXCEEDED` |

校验失败时 `detail.issues[] = [{path, code, message}]`，`path` 用 `runners[2].input` 形式（jsonl 输入带
`line` 行号），供 UI 定位。

> **产物契约（D-74，三选一）**：test / pre_check 的 runner 产出 `result.json` / `result.html` / `result.csv`
> **至少其一**，由 Runner 决定，**不强求 `result.json`**。`RESULT_MISSING` 重定义为"退出码 0 但三选一产物
> 全部缺失"；`RESULT_INVALID` 仅在 **`result.json` 产出但不合法**（不符合附录 A）时适用。`result.html` /
> `result.csv` 为前端通用展示（iframe / 通用表格），平台不解析内容。失败时推荐仍产出 `result.json`
> （带 `error`），但为软要求。

> **Pre-Check 错误对象约定**：pre_check runner 的 `error` 必须带实例级 `retryable` 与 `detail.http_status`
> （4xx/认证失败置 `false`）。终态判定以实例字段为准，不只看码级默认值（01 UC-07、§5.11）。
> pre_check 以**不可重试**错误失败时 Task 终态归 `failed`；可重试（窗口内一直没就绪）归 `service_unavailable`（§7.5、D-35）。

---

## 4. 提交 Manifest

### 4.1 请求

```http
POST /api/v1/tasks?dry_run=false
Authorization: Bearer ...
Content-Type: application/yaml

task:
  name: qwen3-32b-daily
  deploy:                        # 与 service 二选一（互斥；都填或都不填 400）
    provider: genet
    spec: |
      model: Qwen3-32B
      replicas: 1
      resources: { gpu: 8 }
  precheck: auto
  env: { LOG_LEVEL: info }
  secrets: { ENDPOINT_API_KEY: endpoint/prod-qwen }
  runners:
    - name: accuracy-dataset-a
      runner: aisbench-acc
      version: "1.0.0"
      execution: { profile: k8s-npu }
      on_failure: continue
      input: |
        {"dataset":"/data/dataset-a","limit":100}
    - name: performance-sweep
      runner: aisbench-perf
      version: "1.0.0"
      execution: { profile: k8s-npu }
      on_failure: abort_task
      timeout_seconds: 14400
      input: |
        {"input":512,"output":128,"concurrency":1}
        {"input":512,"output":128,"concurrency":4}
```

也接受 JSON 等价对象。`dry_run=true` 只校验不落库，返回 `DryRunResult`。

**提交体字段**（嵌套 `task:`，D-40；ARCHITECTURE §10.1）：

- `name`：Task 名称，展示用，≤200 字符（D-58）。
- `deploy` XOR `service`：互斥，都填或都不填 → 400 `VALIDATION_ERROR`（D-27）。`deploy` 内容是 Genet 部署描述（`provider` + `spec` YAML 原文，平台不解析语义），由内置 service-deploy runner 消费；`service` 直接给 `url` / `model_name` / `model_path`（外接模式，静态值视为"序号 0 的导出"注入全部 runner）。
- `precheck`：`auto`（默认）/ `off` / 显式对象。`auto` 取注册表里唯一 `role: pre_check` 的 Runner 注入为第一个测试类 runner（0 个或多个都拒绝提交）。
- `env` / `secrets`：Task 级；`secrets` 只填引用名。
- `runners[]`：**至少 1 条、至多 200 条**（D-57、§12.7）；严格串行执行。字段只有 `name`（Task 内唯一）、`runner` + `version`（必须命中已注册白名单；`role: deploy/clean` 不可引用——它们由平台自动配对注入）、`execution.profile`、`input`（输入原文，格式按该 Runner 的 `input_format`）、`on_failure`（`continue|abort_task`，不能弱化 Runner 声明）、`timeout_seconds`。**没有** `args`、**没有** `retry`、**没有** `cleanup.policy`（D-66/D-69/D-70）。

**平台校验**（全部 400 `VALIDATION_ERROR`，错误带行号 + 字段名）：

1. 结构与互斥：`deploy` XOR `service`、`runners[]` 数量下限/上限；
2. 引用命中：`runner@version` / `execution.profile` / `secrets` 引用必须存在（`RUNNER_NOT_FOUND` / `EXECUTION_PROFILE_NOT_FOUND`）；
3. **jsonl 输入结构校验**（D-67）：逐行 `JSON.parse`、校验"是对象"、按 `runner.yaml.parameters` 校验**字段类型**与 **`required: true` 存在性**；枚举/范围/跨字段约束仍归 Runner；`yaml` / `text` 输入只做语法校验；
4. exports/consumes 闭合：每个 runner 声明的 `consumes` key 必须有上游导出、静态值或 secret 来源；任何两个 runner 的 `exports` 不允许同名 key（§5.12）；
5. `on_failure` 不得弱化 Runner 声明（§7.3）；必填 env 检查（§12.4）。

### 4.2 响应（202 Accepted）

异步编排：202 表示已受理并落库，后续状态由 SSE/轮询获取。注入的 service-deploy / precheck / service-clean
已在响应里给出最终执行序（`seq`）。

```json
{
  "task": { "id": "t-20261110-0001", "status": "queued",
            "submitted_at": "2026-11-10T01:00:00Z", "current_attempt": 0 },
  "runners": [
    { "seq": 1, "name": "service-deploy", "runner": "service-deploy", "role": "deploy", "status": "pending" },
    { "seq": 2, "name": "precheck", "runner": "endpoint-probe", "role": "pre_check", "status": "pending" },
    { "seq": 3, "name": "accuracy-dataset-a", "runner": "aisbench-acc", "role": "test", "status": "pending" },
    { "seq": 4, "name": "performance-sweep", "runner": "aisbench-perf", "role": "test", "status": "pending" },
    { "seq": 5, "name": "service-clean", "runner": "service-clean", "role": "clean", "status": "pending" }
  ]
}
```

### 4.3 外接模式与部署模式

同一提交端点、同一 `task:` 结构，差异只在 `deploy` XOR `service` 二选一：部署模式平台自动前置
service-deploy、追加 service-clean（用户不手动添加，不可注册，D-70）；外接模式不拉服务，三个公共参数写在
`service` 里，由平台注入环境变量。两者都走 `POST /tasks`，没有独立的"独立提交"端点。

> **重试形态**（§7.8）：`POST /tasks/{id}/retry` 产生**同一 Task 内的新 attempt**（`attempt_no + 1`，全量重跑含
> fresh deploy）；不是新 Task。跨 Task 续跑（`retry_of` 关联、复用未失败 runner）是 P1 能力，仅限外接模式（§7.7）。

### 4.4 重复提交：不做幂等（D-37）

**每次 `POST /tasks` 都创建新资源，没有 `Idempotency-Key`、没有提交指纹、不判重。**

| 情况 | 结果 |
|---|---|
| 客户端超时后重发同一份 Manifest | **两个 Task**；旧的需自行取消 |
| 两次独立提交，Manifest 完全一致 | **两个 Task，都执行** |
| 想避免重复 | 提交前用 `GET /tasks?submitter=...` 查；或提交后立即取消多余的那个 |

理由：平台无法判断两次提交是否等价——硬件型号、驱动、HDK 版本、集群负载、被测服务部署参数对平台透明；镜像 tag 可变，相同内容也可能已换代码。判定"是否同一次测试"是提交者的责任，平台只保证两个 Task 互不覆盖、可独立取消（D-37）。

镜像事实仍记录：`task_runners.image_digest`（K8s 实际拉取），供事后区分同 tag 不同代码（03 §4.9）。

---

## 5. 端点目录

> 统一前缀 `/api/v1`。`{id}` 为 Task ID；`{n}` 为 attempt 序号（从 1 开始）；`{seq}` 为 runner 执行序（从 1 开始，含注入的 deploy/precheck/clean）。
> Task 级 runner 端点（`/tasks/{id}/runners/...`）默认取**最新 attempt**（§7.2：报告默认取最后一个 attempt），
> `?attempt=<n>` 可显式指定；attempt 级端点（`/tasks/{id}/attempts/{n}/runners/...`）是权威形态。

### 5.1 Task

| 方法 | 路径 | 说明 | 成功 |
|---|---|---|---|
| `POST` | `/tasks` | 提交 Task（YAML/JSON）；`?dry_run=true` 只校验 | 202 / 200(dry) |
| `GET` | `/tasks` | 列表：`status`、`submitter`、`runner`、`cursor`、`limit`；**UI 另有 `page`/`page_size`（offset 分页，响应带 `total`）** | 200 |
| `GET` | `/tasks/facets` | Excel 式列筛选的候选值：`status` / `submitter` / `runner` 各值**及计数**（同一套过滤参数） | 200 |
| `GET` | `/tasks/{id}` | 详情 + attempt 列表 + 最新 attempt 的 runner 摘要 | 200 |
| `POST` | `/tasks/{id}/cancel` | 取消（终止当前 runner → 执行 service-clean → 落 `canceled`，§7.6） | 202 |
| `POST` | `/tasks/{id}/retry` | **整 Task 新 attempt**（全量重跑含 fresh deploy，D-69）；attempt 达上限 → 409 `ATTEMPT_EXHAUSTED` | 202 |
| `POST` | `/tasks/{id}/clean` | **重放 service-clean**（泄漏兜底的 REST 形态，`argus clean --task` 的底层；§9.3 / D-72）：用落档的 deploy 导出重新拉起一次内置 service-clean Job。请求体可选 `{attempt?, force?}`（`attempt` 缺省 = 最后一个 attempt）。无部署 / 已有成功 clean 且未 `force` / Task 非终态 / 重放已在进行 → 409 `CONFLICT`；重放失败**不改 Task 终态**（只发 `cleanup_failed` 事件，复用 `runner_started/runner_finished`，无新事件类型） | 202 |
| `GET` | `/tasks/{id}/events` | SSE（`Last-Event-ID`） | 200 |
| `GET` | `/tasks/{id}/events/history` | 事件历史 JSON（`since_seq`、`limit`） | 200 |
| `GET` | `/tasks/{id}/report` | `report.json`（Task 级汇总，默认取最新 attempt） | 200 / 404+Retry-After |
| `GET` | `/tasks/{id}/report.md` | `text/markdown` | 200 |
| `GET` | `/tasks/{id}/manifest` | Manifest 快照（"照上次再来一次"） | 200 |
| `GET` | `/tasks/{id}/deploy` | 部署快照（deploy-state 副本 / 排队水位 / Endpoint / Pods） | 200 |
| `GET` | `/tasks/{id}/deploy/logs` | `?attempt=&pod=`（**可选**）`&offset=&limit=&tail_lines=`。读 `attempts/<n>/runners/<deploy_seq>/logs/<pod>.log`。`pod` 缺省：0 Pod→200+`message`、1 Pod→直接服务、≥2 Pod→**409 `pod_ambiguous`**+`available_pods[]`。游标只在**单 Pod** 内；`tail_lines` 是一次性窗口（`window="tail"`、`truncated=false`、`next_offset=0`，不可续页），且与 `offset` 互斥 | 200 / 409 |
| `GET` | `/tasks/{id}/runners` | 最新 attempt 的 runner 列表（按 seq；`?attempt=` 切历史） | 200 |
| `GET` | `/tasks/{id}/runners/{seq}` | runner 执行详情（默认最新 attempt） | 200 |
| `GET` | `/tasks/{id}/runners/{seq}/result` | 该 runner 的机器可读结果 `result.json`（默认最新 attempt，`?attempt=` 切历史；产物三选一，D-74——未产出 `result.json`（仅 html/csv）或三选一全缺时 404，`detail.produced[]` 列实际产物） | 200 |
| `GET` | `/tasks/{id}/runners/{seq}/logs` | `?stream=stdout|stderr&offset=&limit=`（默认最新 attempt） | 200 |
| `GET` | `/tasks/{id}/runners/{seq}/artifacts` | 该 runner 的产物清单（默认最新 attempt） | 200 |
| `GET` | `/tasks/{id}/attempts` | attempt 列表 | 200 |
| `GET` | `/tasks/{id}/attempts/{n}/runners` | 指定 attempt 的 runner 列表（按 seq） | 200 |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}` | 指定 attempt 的 runner 执行详情 | 200 |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/result` | 该 runner 的机器可读结果 `result.json`（test/pre_check；产物三选一，D-74——未产出 `result.json` 时 404 + `detail.produced[]`） | 200 |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/logs` | `?stream=stdout|stderr&offset=&limit=` | 200 |
| `GET` | `/tasks/{id}/attempts/{n}/runners/{seq}/artifacts` | 该 runner 的产物清单 | 200 |
| `GET` | `/tasks/{id}/artifacts` | Task 级产物清单（含 exports 落档，`kind='export'`） | 200 |
| `GET` | `/tasks/{id}/bundle` | **一次性打包**（`application/zip`）：报告 / 元数据 / 全量事件 / 产物 / 各 attempt 与 deploy 日志 / exports 落档；缺失项进 `MANIFEST.json.missing`（D-56） | 200 |
| `GET` | `/tasks/{id}/files` | `?path=` 下载（Range），路径相对 `<storage-root>/<task_id>/` | 200/206 |

`GET /tasks/{id}` 响应要点：

```json
{
  "id": "t-20261110-0001", "name": "qwen3-32b-daily",
  "status": "running",
  "submitted_by": "zhangsan", "submitted_at": "...", "finished_at": null,
  "endpoint": null,
  "deploy": { "phase": "ready", "release_name": "arg-3f9c1a2b7d40",
              "queue": { "pending": 3, "inqueue": 2, "running": 12, "position": null },
              "queued_at": "..." },
  "error": null,
  "current_attempt": 1,
  "attempts": [ { "attempt_no": 1, "status": "running", "trigger": "initial" } ],
  "runners": [ { "seq": 1, "name": "service-deploy", "role": "deploy", "status": "succeeded" },
               { "seq": 2, "name": "precheck", "role": "pre_check", "status": "succeeded" },
               { "seq": 3, "name": "accuracy-dataset-a", "role": "test", "status": "running" } ]
}
```

### 5.2 Attempt / runner 语义

- Task 是稳定标识与**重试单位**；attempt 是 Task 的第 N 次全量执行；runner 在 attempt 内按序执行**恰好一次**，没有自己的重试端点（v1 的 runner 级 retry 已删除，D-69）。
- 取消也只有 Task 级（§7.6）：取消 = 终止当前 runner → 执行 service-clean → Task 落 `canceled`；没有 runner 级 cancel 端点。
- **产物三选一**（D-74）：test / pre_check 的 runner 产出 `result.json` / `result.html` / `result.csv` 至少其一，
  由 Runner 决定、不强求 `result.json`。`/result` 端点只服务机器可读的 `result.json`：未产出时 404 并在
  `detail.produced[]` 列出实际落档产物；`result.html` / `result.csv` 由前端经 `/artifacts` + `/files` 取用做
  通用展示（iframe / 通用表格），平台不解析其内容。`RESULT_MISSING` = 退出码 0 但三选一全部缺失；
  `RESULT_INVALID` 仅在 `result.json` 产出但不合法时适用（§3）。
- 历史 attempt 完全只读：产物路径按 `attempts/<n>/` 分层，重试不覆盖（§7.2）；UI 可切换查看。

日志/结果响应头：

```http
Content-Type: text/plain; charset=utf-8
X-Log-Offset: 1048576
X-Log-Coverage: 2026-11-10T01:00:00Z/2026-11-10T01:32:10Z
```

超过 10k 行由前端虚拟滚动；服务端单次返回 ≤ 64 KiB。

### 5.3 Runner 目录 / 环境（只读）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/runners` | 目录（`role` 过滤；**默认每个 name 只回最新版本**，`include_retired=true` 回全部版本；条目带 `latest` / `retired`，`orphaned` 为旧别名）。条目含 `entry`（exec form 数组）、`input_format`（jsonl/yaml/text）、`parameters`（jsonl 结构校验 schema：类型 + required，平台校验，D-67）、`exports[]` / `consumes[]`（§5.12 组合期校验依据） |
| `GET` | `/runners/{name}` | 该 name 的最新版本（带 `latest` / `retired`） |
| `GET` | `/runners/{name}/versions/{version}` | 指定版本（含 parameters/env 声明；退役版本仍可按 `name@version` 精确读取） |
| `GET` | `/profiles` | Execution Profile + 挂载能力 |
| `GET` | `/clusters` | 集群概要（不返回凭据） |
| `GET` | `/secrets` | Secret 名称与说明（operator） |
| `GET` | `/queue-status` | 平台槽位 + Volcano 等待原因 |
| `GET` | `/shared-mounts` | 平台级统一共享挂载的只读视图（viewer+；只回启用中的，回答"这次 Task 会挂什么"，D-65） |
| `GET` | `/meta` | `{version, api_version, features}` |

**`/queue-status` 的语义（D-49）**：`slots` 是平台自身的槽位（endpoint 独占 / 全局并发）；`waiting` 是平台侧排队（按 `waiting_reason` 分组）；`volcano` 是**集群调度器视角**的队列快照，直接读 CRD，**不经 Genet**。三者必须分开展示，不能相加成"一个排队数"。`volcano.available=false` 时 `detail` 取四态之一（`crd_missing`/`rbac_forbidden`/`scheduler_unavailable`/`disabled`），且 **不可达不等于 0 排队**；多集群时 `queues[].name` 用 `"<cluster>/<queue>"`。`volcano.queues[].pending_jobs` 是该队列下 `phase=Pending` 的 PodGroup 计数（不是 `queue.status.pending`），`longest_wait_seconds` 取 `now - min(podgroup.creationTimestamp)`（phase ∈ Pending/Inqueue）。

**`queues[].reason` 是自由文本，不是枚举**：取该队列里等得最久那条 PodGroup 的 `status.conditions[-1].message`（没有则 `.reason`），**原文透传**——它常是 K8s 调度器话术，硬映射成枚举会丢信息也会误判。结构化分类只在 `waiting.by_reason` 的键上；UI 展示队列原因时原样显示（等宽、可换行）。另：`volcano.stale`（**快照层**，不是每个队列一份）表示"上一次刷新成功、本次刷新失败"，**无时间阈值**；置位时仍 `available=false` + `queues=[]`，**降级绝不回放历史队列数字**。

> v2 的 `waiting_reason` 是 4 值枚举（`slot_waiting` / `queue_full` / `insufficient_resources` / `gang_pending`，§6.9）：
> v1 的 `precheck_gate` / `global_limit` 随门控+并发模型取消（D-32/D-69）——串行管道里等待前序 runner 的 runner
> 处于 `pending`（不属排队），平台并发上限并入 `slot_waiting`。部署排队可见性由 `/tasks/{id}/deploy` 的
> deploy-state 副本承载（D-70），不再有平台 `DEPLOYING` 阶段。

### 5.4 Admin

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/admin/shared-mounts` | 平台级统一共享挂载列表（含停用行，D-65） |
| `POST` | `/admin/shared-mounts` | 新建 `{name, mount_path, read_only?, claim_name, enabled?}`（默认只读、默认启用） |
| `PUT` | `/admin/shared-mounts/{name}` | 改 `mount_path` / `read_only` / `claim_name` / `enabled`（name 不可改） |
| `DELETE` | `/admin/shared-mounts/{name}` | 删除；只影响之后的渲染（已提交 Task 的执行记录快照不变） |
| `POST/PUT/DELETE` | `/admin/profiles` | Profile 维护 |
| `POST/PUT/DELETE` | `/admin/clusters` | 集群登记（`kubeconfig_key`） |
| `GET/POST` | `/admin/secrets` | 引用列表/登记 |
| `POST` | `/admin/secrets/{name}/rotate` | 轮换校验 + 审计 |
| `GET` | `/admin/audit` | 审计（`actor`、`action`、时间） |
| `DELETE` | `/admin/tasks/{id}/artifacts` | 人工清理产物 |

**`/shared-mounts` 的语义（D-63/D-65）**：这些挂载点由 executor 渲染进**所有** runner Job（含 precheck 与内置
deploy/clean），`shared_mounts` 表是运行时真源（`AG_SHARED_MOUNTS` 只是首次启动的种子）。平台**不创建** PV/PVC——
存储供给归集群运维，管理面只填一个已存在的 `claim_name`（内联 `volumes[].nfs` 会被 PodSecurity `restricted` 拒绝）。
校验失败 → 400 `VALIDATION_ERROR`，`detail.issues[].code` 为 `CONFLICT`（路径/名字冲突）或 `REQUIRED`（缺 `claim_name`）。

> Runner 注册**没有**运行时管理端点：目录自动发现（扫描 `runners/*/runner.yaml`），`deploy` / `clean` 角色仅内置、
> 用户注册拒绝（§5.1）；版本退役靠目录比对标 `orphaned`（`registry.Store.MarkRunnerOrphaned`，D-60）。

### 5.5 运维

`GET /healthz`（进程存活）、`GET /readyz`（DB + 存储可写 + 迁移版本；不含 K8s）、`GET /metrics`（Prometheus）。

---

## 6. 分页、过滤与排序

```http
GET /api/v1/tasks?status=running,queued&limit=50&cursor=eyJ0cyI6...
```

```json
{ "items": [ ... ], "next_cursor": "eyJ0cyI6..." , "has_more": true }
```

- `cursor` 不透明，服务端编码 `(submitted_at, id)` 倒序；客户端只回传。
- `limit` 默认 50、上限 200。
- 多值过滤用逗号；未知过滤字段返回 400。
- 排序固定为提交时间倒序；runner 列表按 `(task_id, attempt_no, seq)`。

---

## 7. SSE

```http
GET /api/v1/tasks/t-20261110-0001/events
Accept: text/event-stream
Last-Event-ID: 42
```

```text
id: 43
event: runner_finished
data: {"seq":43,"type":"runner_finished","task_id":"t-20261110-0001","attempt_no":1,"runner_seq":3,"at":"...","payload":{"status":"failed","error_code":"RUNNER_NONZERO_EXIT"}}
```

规则：

| 项 | 规则 |
|---|---|
| 回放 | 从 `Last-Event-ID+1` 开始；窗口 24h、单次 ≤1000 条；越窗发 `event: resync_required`，客户端整页重建 |
| 心跳 | 15s 一行 `: ping` |
| 断开 | 服务端 60s 无事件或客户端断开即释放；客户端指数退避重连（1/2/4/8/30s） |
| 并发 | 同一用户同一 Task 的连接上限（P1 限流位），超限返回 429 |
| 内容 | 只发状态事实，不发日志正文、不发凭据 |

事件类型（ARCHITECTURE §12.6）：`submitted`、`state_changed`、`runner_started`、`runner_finished`、
`exports_harvested`、`clean_finished`、`cleanup_failed`、`task_finished`、`report.written`、`resync_required`。

> `cleanup_failed`（D-54 沿用）：Task 已终态但 **service-clean 未通过判干净校验**（`GET /releases/{name}` 仍 200，
> 或 DELETE 失败）时发出，`payload = {release_name, clean_state, cleanup_errors[], verified_at}`。它**不改变 Task
> 终态**，只是把"资源没删干净"这件事暴露出来；失败项进 §9.3 重放清单，重放成功后事件与落档同步更新。

---

## 8. 取消、重试与重复调用语义

| 操作 | 前置 | 返回 | 重复调用 |
|---|---|---|---|
| `cancel task` | 非终态 | 202 + `{task_id,status:"cancelling"}` | 已是 `cancelling/canceled` 返回 200 同状态 |
| `retry task` | Task 终态且未达 attempt 上限 | 202 + `{task_id, attempt_no, status:"queued"}`（新 attempt 全量重跑含 fresh deploy） | attempt 达上限 → 409 `ATTEMPT_EXHAUSTED`；已在执行 → 409 `CONFLICT` |
| `replay clean`（`POST /tasks/{id}/clean`） | 目标 attempt 含部署、无成功 clean 记录（或显式 `force`）、Task 终态 | 202 + `{task_id, attempt_no, runner:{seq,name,status}}`（重放执行定位） | 无部署 `no_deploy` / 已有成功记录 `clean_already_verified`（未 force）/ 非终态 `task_running` / 重放进行中 `clean_replay_in_progress` → 409 `CONFLICT`；重放失败不改终态，可再次重放 |

取消与自然结束竞态：服务端以先落库状态为准，客户端以 SSE 推送为最终结果（不做乐观更新）。

> 取消语义（§7.6）：先终止当前 runner（Executor 删 Job），`pending`/`queued` 的 runner 标 `skipped(reason=canceled)`
> 传播到 clean 为止，然后**追加执行 service-clean**（若含部署），clean 结束后 Task 才落 `canceled`。
> clean 自身失败记 `cleanup_failed`，不改变终态。

---

## 9. 限流与配额

| 项 | P0 默认 |
|---|---|
| 单用户写请求 | 10 rps（令牌桶，超限 429 `RESOURCE_EXHAUSTED`） |
| SSE 连接 | 每用户 20、每 Task 5（P1 可配） |
| 单 Task runner 数 | 200（`TASK_TOO_LARGE`） |
| 单 Task attempt 数 | 默认 3（`POST /tasks/{id}/retry` 超限 409 `ATTEMPT_EXHAUSTED`，§12.7） |
| 全局并发 runner 数 | 20（排队不拒绝，返回 `waiting_reason`） |
| 请求体 | 1 MiB（413 + `VALIDATION_ERROR`） |

---

## 10. OpenAPI 与契约测试

- `server/api/openapi.yaml` 是唯一机器契约；`oapi-codegen` 生成服务端接口，`openapi-typescript` 生成前端类型（生成物入库）。
- CI：`spectral lint` + 重生成后 `git diff --exit-code`；生成失败或 diff 非空即阻断。
- 每个错误码至少一个 handler 测试断言 `code` 与 HTTP 状态；`tools/contract` 对错误码枚举做文档↔契约↔`domain/codes.go` 三方比对（本仓没有独立 `schemas/` 目录，样例校验直接走 openapi 定义的 schema）。
- 新增字段属兼容变更；删改字段、改枚举值属破坏性变更，走 ARCHITECTURE §15 并升版本。

---

## 11. 示例：一次完整调用

```bash
# 1) 提交
curl -sS -X POST "$API/api/v1/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @task.yaml

# 2) 轮询状态
curl -sS "$API/api/v1/tasks/t-20261110-0001" -H "Authorization: Bearer $TOKEN"

# 3) 订阅事件
curl -N "$API/api/v1/tasks/t-20261110-0001/events" \
  -H "Authorization: Bearer $TOKEN" -H "Last-Event-ID: 0"

# 4) 取结果（runner seq=3 的 result.json）
curl -sS "$API/api/v1/tasks/t-20261110-0001/attempts/1/runners/3/result" -H "Authorization: Bearer $TOKEN"

# 5) 下载原始产物
curl -OJ "$API/api/v1/tasks/t-20261110-0001/files?path=attempts/1/runners/3/raw/perf.xlsx" \
  -H "Authorization: Bearer $TOKEN"

# 6) 失败后整 Task 重试（新 attempt，全量重跑含重新部署）
curl -sS -X POST "$API/api/v1/tasks/t-20261110-0001/retry" -H "Authorization: Bearer $TOKEN"
```
