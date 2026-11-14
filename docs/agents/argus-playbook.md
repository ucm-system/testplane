# Argus Agent 全闭环操作手册（playbook）

> 读者：要在 Argus 上自主提交评测、等待结果、取报告、处理失败的 Agent（LLM 或自动化脚本）。
> 本手册只使用**公开契约**：`server/api/openapi.yaml`（站点静态镜像 `/openapi.yaml`）、
> `GLOSSARY.md`（术语与 v1→v2 映射）、`design/ARCHITECTURE.md`（附录 A result.json / 附录 D 错误码）。
> **Agent 没有特权路径**——与 Web UI / CLI 走同一套 REST 与鉴权（ARCHITECTURE §11.5）。
>
> 所有 curl 示例假定：`$API` 是控制面地址（默认 `http://127.0.0.1:8080`）、`$TOKEN` 是 Bearer 令牌
> （dev 鉴权口径：`Authorization: Bearer <subject>`，subject 即用户名）。
> 每个步骤末尾给出对应的 MCP 工具与 `argus` CLI 子命令。

## 0. 自举入口

| 入口 | 地址 | 用途 |
|---|---|---|
| 机器契约 | `GET /openapi.yaml`（静态）或仓库 `server/api/openapi.yaml` | 全部端点、schema、错误码 |
| Agent 索引 | `GET /llms.txt`（静态） | 文档导航（playbook / GLOSSARY / runbook） |
| 平台能力开关 | `GET /api/v1/meta` | 版本与 features |
| 当前身份 | `GET /api/v1/me` | 确认鉴权生效 |

```bash
curl -sS "$API/api/v1/meta" -H "Authorization: Bearer $TOKEN"
```

## 1. 发现 Runner（提交前必做）

Runner 注册表由目录扫描落库（K-3：无运行时注册），**只读**。提交体里每个 `runners[].runner@version`
都必须命中这里，`execution.profile` 必须命中 `GET /api/v1/profiles`。

```bash
curl -sS "$API/api/v1/runners" -H "Authorization: Bearer $TOKEN"
curl -sS "$API/api/v1/runners/endpoint-probe" -H "Authorization: Bearer $TOKEN"
curl -sS "$API/api/v1/profiles" -H "Authorization: Bearer $TOKEN"
```

一个 Runner 投影里 Agent 要读的字段（附录 C v2）：

| 字段 | 怎么用 |
|---|---|
| `input_format` | `jsonl`：平台做语法 + 结构校验（`parameters` 是类型 + required schema）；`yaml` / `text`：只做语法校验，语义归 Runner，此时 `parameters` 必为 `[]` |
| `parameters` | jsonl 输入的字段 schema：`name` / `type` / `required`（提交期校验）；`default` / `minimum` / `description` 只是文档语义，平台不校验 |
| `placeholder` | 与 `input_format` 同格式的合法输入示例片段（UI 占位用，平台不解析）——Agent 照着它构造 `input` 最稳 |
| `exports` / `consumes` | 组合期校验依据：下游 runner 的 `consumes` 必须能被上游 `exports` 或 `service` / deploy 导出闭合（§5.12），否则提交 400 |
| `env` | Runner 声明读取的环境变量；`required: true` 且 `secret: true` 的要在 Task 级 `secrets` 给引用名 |
| `entry` / `image` | 参考信息：argv 恒空（D-66），业务参数全部走 `input` |
| `latest` / `retired` / `orphaned` | 默认列表只回每个 name 的最新版本；`retired/orphaned` 行仍可按 name@version 引用历史 Task |

MCP：`argus_list_runners`。CLI：`argus runners`。

## 2. 组合提交体（Manifest v2）

顶层是嵌套 `task:`（D-40）；`deploy` 与 `service` **二选一**（都填或都不填都拒绝，D-27）；
`runners[]` 可为 0（D-77：只跑 deploy/precheck/clean，验证服务能否启动）、至多 200 条（§12.7）。字段只有
`name / runner / version / execution / input / on_failure / timeout_seconds`——
**没有** `args`（argv 恒空）、**没有** runner 级 `retry`（D-69）、**没有** `cleanup.policy`（D-70）。

```yaml
# task.yaml —— 部署模式（deploy XOR service）
task:
  name: qwen3-32b-daily
  deploy:
    provider: genet
    spec: |
      model: Qwen3-32B
      replicas: 1
      resources: { gpu: 8 }
  precheck: auto          # auto（默认）/ off / 显式对象（C-5.2）
  env:
    LOG_LEVEL: info
  secrets:
    ENDPOINT_API_KEY: endpoint/prod-qwen   # 只填引用名，永不回显（C-10）
  runners:
    - name: accuracy-dataset-a
      runner: aisbench-acc
      version: 1.0.0
      execution:
        profile: k8s-npu
      on_failure: continue
      input: |
        {"dataset":"/data/dataset-a","limit":100}
    - name: performance-sweep
      runner: aisbench-perf
      version: 1.0.0
      execution:
        profile: k8s-npu
      on_failure: abort_task
      timeout_seconds: 14400
      input: |
        {"input":512,"output":128,"concurrency":1}
        {"input":512,"output":128,"concurrency":4}
```

外接模式（不部署，已有服务直接测）：把 `deploy` 换成

```yaml
  service:
    url: http://10.0.0.10:8000/v1
    model_name: Qwen3-32B
    model_path: /models/Qwen3-32B
```

`input` 是**输入原文**：jsonl 按 Runner 的 `parameters` 逐行校验（类型 + required，带行号，D-67），
单条 ≤64 KiB；`on_failure` 只能取更严者（pre_check 必须 `abort_task`）。

## 3. 提交（不幂等！）

```bash
curl -sS -X POST "$API/api/v1/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/yaml" \
  --data-binary @task.yaml
```

- 成功：**202** `TaskAccepted`：`{task:{id,status,submitted_at,current_attempt}, runners[]}`
  ——`runners[]` 是**含注入行**（service-deploy / precheck / service-clean）的最终执行序。记住 `task.id`（形如 `t-20261110-0001`）。
- **不做幂等（D-37）**：没有幂等键、没有提交指纹；超时重发就是两个 Task。想防重复先
  `GET /api/v1/tasks?submitter=<你>&limit=...` 查一下再决定。
- 可选预检：`POST /api/v1/tasks?dry_run=true` → **200** `DryRunResult`（`valid` + `issues[]` + `resolved_preview`），不落库。

### 400 VALIDATION_ERROR：按 issues 修复重试

```json
{ "code": "VALIDATION_ERROR", "message": "Manifest 校验未通过", "retryable": false,
  "detail": { "issues": [ { "path": "runners[1].input", "code": "INPUT_TYPE_MISMATCH",
             "line": 2, "message": "第 2 行字段 input 期望 integer，实际 string" } ] },
  "trace_id": "..." }
```

处理循环：读 `detail.issues[].path`（`runners[2].input` 形式）与 `line`（jsonl 行号，从 1 起）→
对照第 1 步拿到的 `parameters` 修 Manifest → 重发。issue 级码常见：
`INPUT_JSON_SYNTAX`（逐行 parse 失败）、`INPUT_TYPE_MISMATCH`、`INPUT_REQUIRED_MISSING`、
`EXPORT_KEY_CONFLICT`（两个 runner 导出同名 key）。

MCP：`argus_submit_task`（支持 dry_run=true）。CLI：`argus submit -f task.yaml --json`。

## 4. 等待终态

终态集合：`succeeded` / `failed` / `service_unavailable` / `canceled`（v2 **没有**
`completed_with_errors`，其语义并入 `failed`，D-73）。

### 方式 A：轮询（简单可靠）

```bash
while :; do
  st=$(curl -sS "$API/api/v1/tasks/$TASK_ID" -H "Authorization: Bearer $TOKEN" | jq -r .status)
  case "$st" in succeeded|failed|service_unavailable|canceled) break ;; esac
  sleep 2
done
```

`TaskDetail` 同时给 `runner_counts`（`{total,pending,running,succeeded,failed,skipped,canceled}`）、
`attempts[]`（attempt 聚合行）与最新 attempt 的 `runners[]`（每个 runner 的状态、`status_reason` /
`waiting_reason`、`error`）——进度汇报与失败定位都用它，不必再查列表。

### 方式 B：SSE（推荐长等待）

```bash
curl -N "$API/api/v1/tasks/$TASK_ID/events" \
  -H "Authorization: Bearer $TOKEN" -H "Last-Event-ID: 0"
```

- 帧格式：`id: <seq>` + `event: <type>` + `data: {json}`；心跳 15s 一行 `: ping`。
- 事件类型：`submitted`、`state_changed`、`runner_started`、`runner_finished`、
  `exports_harvested`、`clean_finished`、`cleanup_failed`、`task_finished`、
  `report.written`、`resync_required`。
- **断线续传**：重连时带上最后收到的 `id` 作 `Last-Event-ID`，服务端从 `seq+1` 回放；
  越过 24h 窗口会先发 `resync_required`——此时整页重建（改用 `GET /tasks/{id}` + `events/history`）。
- 只发状态事实，不发日志正文、不发凭据（C-9.1）；日志走第 5 步的独立端点。
- JSON 形态补齐：`GET /api/v1/tasks/{id}/events/history?since_seq=<seq>&limit=200`（≤1000）。

MCP：`argus_wait_task`（SSE 优先、自动降级轮询，返回 `success|failed|canceled|timeout`，timeout 不是错误）。CLI：`argus wait --task-id $TASK_ID`。

## 5. 取结果

```bash
# 机器可读报告（终态后异步生成；未就绪 404 + Retry-After，照它重试）
curl -sS "$API/api/v1/tasks/$TASK_ID/report"     -H "Authorization: Bearer $TOKEN"
# 人类可读 Markdown
curl -sS "$API/api/v1/tasks/$TASK_ID/report.md"  -H "Authorization: Bearer $TOKEN"
# 一次性打包全部产物（zip，带 MANIFEST.json；counts 与 /artifacts 同源）
curl -OJ "$API/api/v1/tasks/$TASK_ID/bundle"     -H "Authorization: Bearer $TOKEN"
```

细粒度（按 runner 定位，键 `(task_id, attempt_no, seq)`）：

```bash
# runner 日志（text/plain；?stream=stdout|stderr，offset/limit 翻页，响应头 X-Log-Offset 续读）
curl -sS "$API/api/v1/tasks/$TASK_ID/attempts/1/runners/3/logs?stream=stdout&limit=65536" -H "Authorization: Bearer $TOKEN"
# runner 的 result.json（产物三选一 D-74：只产出 html/csv 时本端点 404，detail.produced[] 列实际产物）
curl -sS "$API/api/v1/tasks/$TASK_ID/attempts/1/runners/3/result" -H "Authorization: Bearer $TOKEN"
# runner / Task 级产物清单与文件下载
curl -sS "$API/api/v1/tasks/$TASK_ID/attempts/1/runners/3/artifacts" -H "Authorization: Bearer $TOKEN"
curl -OJ "$API/api/v1/tasks/$TASK_ID/files?path=attempts/1/runners/3/raw/perf.xlsx" -H "Authorization: Bearer $TOKEN"
```

`result.json` 是附录 A schema 2.0：`schema_version / task_id / attempt_no / runner_seq / runner / status / metrics` 必填；
`status ∈ {succeeded, failed}`（字段级 `error` 在 runner 自报失败时出现）。历史 attempt 用
`GET /tasks/{id}/attempts` + `/attempts/{n}/runners` 回看。

MCP：`argus_get_report`（json/markdown）、`argus_get_logs`、`argus_list_artifacts`、`argus_get_file`。
CLI：`argus report` / `logs` / `artifacts` / `files`。

## 6. 后续动作：重试 / 取消 / 泄漏重放

```bash
# 整 Task 重试：新 attempt 全量重跑，含 fresh deploy（§7.8——不得复用上次已部署服务）
curl -sS -X POST "$API/api/v1/tasks/$TASK_ID/retry" -H "Authorization: Bearer $TOKEN"
#   → 202 {task_id, attempt_no: current_attempt+1, status: "queued"}；canceled 不可重试；
#     attempt 达上限 409 ATTEMPT_EXHAUSTED。

# 取消：先执行 service-clean（finally）再落 canceled（C-6.1）；已在 cancelling/canceled 幂等 200
curl -sS -X POST "$API/api/v1/tasks/$TASK_ID/cancel" -H "Authorization: Bearer $TOKEN"

# 泄漏重放（§9.3）：用落档导出重拉一次 service-clean Job（集群里有残留时用）
curl -sS -X POST "$API/api/v1/tasks/$TASK_ID/clean" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d '{}'
#   请求体可选 {attempt: <n>}（缺省最后一个 attempt）、{force: true}（已有成功 clean 记录时才需要）；
#   409 时 detail.reason ∈ {no_deploy, clean_already_verified, task_running, clean_replay_in_progress}
```

MCP：`argus_retry_task` / `argus_cancel_task` / `argus_clean_task`。CLI：`argus retry` / `cancel` / `clean`。

## 7. 错误处理表（附录 D v2 → next_action）

顶层错误码全集见 `design/ARCHITECTURE.md` 附录 D；Agent 的自动分诊口径：

| code | HTTP | retryable | next_action |
|---|---|---|---|
| `VALIDATION_ERROR` | 400 | 否 | 按 `detail.issues[].path/line` 修复 Manifest 后**重发提交**（见 §3） |
| `UNAUTHENTICATED` / `FORBIDDEN` | 401/403 | 否 | 检查令牌 / 角色，不要原样重试 |
| `NOT_FOUND` | 404 | 否 | id 打错或对象不存在；列表核对后再查 |
| `RUNNER_NOT_FOUND` | 400 | 否 | 回到 §1 发现 Runner，换 `runner@version` |
| `EXECUTION_PROFILE_NOT_FOUND` | 400 | 否 | `GET /api/v1/profiles` 换已登记 profile |
| `TASK_TOO_LARGE` | 400 | 否 | 拆分 Task（runners ≤200，§12.7） |
| `ATTEMPT_EXHAUSTED` | 409 | 否 | 重试次数用尽——升级人工，别再打 retry |
| `RESOURCE_EXHAUSTED` | 429 | 是 | 退避后重试；先看 `GET /api/v1/queue-status`（三层水位不可相加） |
| `EXECUTOR_UNAVAILABLE` | 503 | 是 | 平台侧 K8s 不可达；退避重试/告警 |
| `IMAGE_PULL_FAILED` | 502 | 是 | 镜像引用/DNS 问题；核对 `runner.yaml` image 与集群可见性 |
| `MOUNT_UNAVAILABLE` | 502 | 否 | PVC / 挂载配置问题；人工处理 |
| `EXECUTOR_POD_ERROR` | 502 | 是 | Pod 异常终止；`error.detail.k8s_reason` 给原因 |
| `EXPORT_INVALID` | 500 | 否 | runner 退出码 0 但 exports.env 不合法——改 Runner 产物，或换版本 |
| `DEPLOY_FAILED` | 502 | 否 | Genet 调用失败；`detail.reason` 是 Genet 原文（D-47）——按 reason 修 deploy.spec |
| `DEPLOY_BLOCKED` | 502 | 是 | 部署 blocked/超窗；`detail.reason ∈ {blocked,timeout,…}`；可整 Task retry（fresh deploy） |
| `ENDPOINT_UNREACHABLE` / `PRECHECK_FAILED` | 502 | 是 | pre_check 探活失败；retryable=true 归 `service_unavailable`，可 retry |
| `RUNNER_TIMEOUT` | 504 | 按策略 | 超过 `timeout_seconds`；酌情调大或排查 runner |
| `RUNNER_NONZERO_EXIT` | 500 | 否 | Runner 业务失败；读该 runner 的 logs 与 result.json 定位 |
| `SLA_PRECHECK_FAILED` / `SLA_NO_FEASIBLE_CONCURRENCY` | 500 | 否 | Runner 自报 SLA 不达标；属测量结论不是平台故障 |
| `RESULT_MISSING` | 500 | 否 | 退出码 0 但 `result.json/html/csv` 三选一全缺——Runner 侧修复 |
| `RESULT_INVALID` | 500 | 否 | `result.json` 不符合附录 A——Runner 侧修复 |
| `CLEANUP_FAILED` | 500 | 是 | 拆除失败不改 Task 终态；进 §9.3 重放清单，用 `POST /tasks/{id}/clean` |
| `CANCELED` | 409 | 否 | 操作对象已取消 |
| `INTERNAL_ERROR` | 500 | 视情况 | 带 trace_id 找人；不要盲目重试 |

规律：`retryable=true` 才允许自动重试类动作（整 Task `retry` 是唯一平台内重试入口）；
`409 CONFLICT/ATTEMPT_EXHAUSTED/CANCELED` 是终局拒绝；提交类 400 一律先修 Manifest 再重发。

## 8. MCP 工具 ↔ REST 端点对照（13 工具）

| MCP 工具 | REST 端点 | 对应本手册 |
|---|---|---|
| `argus_list_runners` | `GET /api/v1/runners`（+`/{name}`、`/{name}/versions/{version}`） | §1 |
| `argus_list_profiles` | `GET /api/v1/profiles` | §1 |
| `argus_queue_status` | `GET /api/v1/queue-status` | §7（RESOURCE_EXHAUSTED 分诊） |
| `argus_submit_task` | `POST /api/v1/tasks`（`?dry_run=true` 可选） | §2–§3 |
| `argus_get_task` | `GET /api/v1/tasks/{task_id}` | §4 方式 A |
| `argus_wait_task` | `GET /api/v1/tasks/{task_id}/events`（SSE）→ 轮询降级 | §4 方式 B |
| `argus_get_report` | `GET /api/v1/tasks/{task_id}/report`、`/report.md` | §5 |
| `argus_get_logs` | `GET /api/v1/tasks/{task_id}/attempts/{n}/runners/{seq}/logs` | §5 |
| `argus_list_artifacts` | `GET /api/v1/tasks/{task_id}/artifacts`、`…/runners/{seq}/artifacts` | §5 |
| `argus_get_file` | `GET /api/v1/tasks/{task_id}/files?path=` | §5 |
| `argus_cancel_task` | `POST /api/v1/tasks/{task_id}/cancel` | §6 |
| `argus_retry_task` | `POST /api/v1/tasks/{task_id}/retry` | §6 |
| `argus_clean_task` | `POST /api/v1/tasks/{task_id}/clean` | §6 |

MCP 走 stdio JSON-RPC 2.0：`argus mcp`（CLI 内嵌）或独立 MCP Server 二进制；
`tools/list` 的 `inputSchema` 是每工具参数的唯一权威。bundle 无对应 MCP 工具时用 CLI
`argus files` / HTTP 下载兜底；所有工具的语义与 REST 严格同源，不引入私有端点。

## 9. 最小闭环伪代码

```text
runners  = GET /api/v1/runners                      # §1 校准 runner@version + parameters
manifest = compose(task:, deploy|service, runners[].input)   # §2
dry      = POST /api/v1/tasks?dry_run=true          # 可选预检
task     = POST /api/v1/tasks                       # §3；VALIDATION_ERROR → 修 issues 重试
wait     = SSE /tasks/{id}/events（Last-Event-ID 续传）或轮询 GET /tasks/{id}   # §4
report   = GET /tasks/{id}/report（404+Retry-After → 等待重试）                 # §5
bundle   = GET /tasks/{id}/bundle                   # §5 一次性取走
on failure = 分诊附录 D（§7）→ retry（终态非 canceled）/ clean（泄漏）/ 人工
```
