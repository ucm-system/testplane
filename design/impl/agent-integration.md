# Agent 接入实现（agent-integration）

> **范围：** P1 的 Agent 亲和入口：`tpctl` CLI 与 MCP Server（stdio / JSON-RPC 2.0），以及与 REST 的关系、`wait --follow` 的 SSE 策略、`next_action` 错误提示、退出码、测试方式。
> **不在范围：** REST 契约本身（[`views/07-api.md`](../views/07-api.md)）、状态机与编排（[`control-plane.md`](control-plane.md)）、执行层（[`execution.md`](execution.md)）、Web UI。
> **基线：** [`server/api/openapi.yaml`](../../server/api/openapi.yaml)（P1 本轮补到 34 operations：新增 `/profiles`、`/runs/{id}/artifacts`、`/runs/{id}/files`、`/tasks/{id}/attempts/{idx}/artifacts`、`/testers/{name}/versions/{v}` 及认证/指标端点）+ [`IMPLEMENTATION.md`](../IMPLEMENTATION.md) §13。
> **代码：** [`server/cmd/tpctl`](../../server/cmd/tpctl)、[`server/internal/mcp`](../../server/internal/mcp)。
> **定位：** **薄 adapter**。CLI 与 MCP 不引入新语义，只是 REST 的两种调用形态；控制面仍是唯一权威。

## 1 为什么做

TestPlane 的定位是 **AI Agent 友好**：Agent 要在**不写代码**的前提下完成「提交 → 等待 → 取报告 → 看产物/日志 → 取消」闭环，并且能可靠判断结局、知道失败后该做什么。REST 对 Agent 有四处摩擦：

| 摩擦 | 现象 | 本轮解法 |
|---|---|---|
| 工具调用 | Agent 运行时通常只会「调工具」，不会拼 HTTP、管 headers、解 SSE | MCP server 把 11 个高频动作注册成工具 |
| 结局判定 | HTTP 200 只表示「查到了」，不表示 Run 成功；Agent 需要解析状态枚举 | CLI 把 Run 结局映射成退出码；MCP 返回 `outcome` 字段 |
| 等待成本 | 轮询有延迟，长 Run 浪费调用次数 | `wait --follow` 用 SSE（`Last-Event-ID` 续传、断线重连），不可用时自动降级轮询 |
| 失败后怎么办 | 错误码只说明「哪里错」，不说明「下一步做什么」 | 共享 `next_action` 映射（CLI stderr / MCP `error.next_action`） |

三条设计原则：

1. **一一对应**：每个子命令 / 工具都能追到一个 REST 端点，不发明控制面没有的动作（见 §3）。
2. **机器可读优先**：`--json` 时 stdout 只有一个 JSON 文档；进度、告警、日志续读坐标、SSE 降级说明一律走 stderr。
3. **无状态**：CLI/MCP 不缓存、不落库；除 `files --out` 的显式落盘外不写文件，进程退出即无残留。

## 2 定位与边界

| 做 | 不做 |
|---|---|
| 把 REST 端点包装成命令 / 工具 | 新增状态、聚合或 Gate 判定（§13.1 的 Non-Goal 照旧） |
| 透传控制面错误信封（`code/message/detail/retryable/trace_id`） | 改写错误码，或把 4xx 说成成功 |
| 把 Run 终态映射成退出码 / `outcome` | 替 Agent 决定「这次测试算不算过」 |
| 读 `-f` 指定的 Manifest（也支持 stdin）；`files --out` 显式落盘 | 改 Manifest、做 Matrix 展开、默认写任何文件 |
| 用 `--token` / `$TESTPLANE_TOKEN` 出站鉴权 | 代理/缓存凭据、把 token 写进输出或日志 |
| 用 SSE 等服务端事件（`wait --follow`） | 自建状态、缓存事件、把日志塞进 SSE |
| 依赖 Go 标准库（`flag` / `net/http` / `encoding/json`） | 引入 cobra、MCP SDK 或第二套 HTTP 客户端语义 |

**为什么不引入 MCP SDK / CLI 框架**：协议面只有 4 个方法（`initialize` / `tools/list` / `tools/call` / `ping`）和 12 个子命令；引入框架只会把「命令 ↔ 端点」的对应关系藏进 DSL，反而妨碍 Agent 读源码。SSE 解析器约 70 行、等待循环约 200 行，测试可以直接用 `httptest` + `io.Pipe` 驱动。

**与 `internal/api` 的关系**：CLI 与 MCP **都不 import** `server/internal/api`（那是服务端 handler/DTO 包）。两者各有一份约 120 行的 HTTP 客户端，只做「拼 URL + Bearer + 超时 + 错误信封解析 + SSE 打开」。这与 §15.3 #7「MVP 手写 handler/DTO、未采用 codegen」保持一致。

**共享的等待内核**：`server/internal/mcp/sse.go` 同时被 MCP 工具与 CLI 使用（`mcp.Wait`）。CLI 的 client 与 MCP 的 client 各实现一个 3 方法窄接口 `RunReader`（`GetRun` / `OpenRunEvents`），因此等待/降级/重连逻辑只有一份实现、一份测试。

## 3 与 REST 的关系

| CLI 命令 | MCP 工具 | REST | 说明 |
|---|---|---|---|
| `submit -f <file>` | `testplane_submit_run` | `POST /api/v1/runs`（`?dry_run=true`） | Manifest 原样转发；按内容首字符选 YAML/JSON `Content-Type` |
| `status <run_id>` | `testplane_get_run` | `GET /api/v1/runs/{id}` | 派生 `terminal`；失败终态带 `next_action` |
| `wait <run_id> [--follow]` | `testplane_wait_run`（`follow`） | 轮询 `GET /runs/{id}`；`follow` 时 `GET /runs/{id}/events`（SSE） | SSE 不可用自动降级轮询；不新增等待端点 |
| `cancel <run_id>` | `testplane_cancel_run` | `POST /runs/{id}/cancel` + 回读 `GET` | 202/200 后确认一次 |
| `cancel --task <task_id>` | — | `POST /tasks/{id}/cancel` + 回读 `GET` | MCP 工具表按要求只保留 Run 级取消 |
| `report <run_id>` | `testplane_get_report` | `GET /runs/{id}/report` | JSON 透传；`--json` 单行、默认缩进 |
| `report <run_id> --md` | `testplane_get_report`（`format=markdown`） | `GET /runs/{id}/report.md` | 原样打印 Markdown，不做渲染 |
| `logs <task_id>` | `testplane_get_logs` | `GET /tasks/{id}/attempts/{idx}/logs` | 透传 `offset/limit/tail/stream`；`X-Log-Offset` → `next_offset` |
| `artifacts <run_id>` | `testplane_list_artifacts`（`run_id`） | `GET /runs/{id}/artifacts` | 按 `kind` 分组；`path` 可直接给 `files` |
| `artifacts --task <task_id> --attempt N` | `testplane_list_artifacts`（`task_id`） | `GET /tasks/{id}/attempts/{idx}/artifacts` | 与 Run 级同信封 |
| `files <run_id> <path>` | `testplane_get_file`（摘要） | `GET /runs/{id}/files?path=` | `--range` 透传 `Range`；`--out` 落盘并打印 sha256 |
| `testers` | `testplane_list_testers` | `GET /api/v1/testers` | `role` / `orphaned` 过滤 |
| `queue` | `testplane_queue_status` | `GET /api/v1/queue-status` | 三个视图**不可相加**（07 §5.3、D-49） |
| `profiles` | `testplane_list_profiles` | `GET /api/v1/profiles` | P1 已实现，返回 Profile + `mounts` |

**不变的事实**（不随接入方式改变）：

- 提交**不做幂等**（D-37）：`submit` / `testplane_submit_run` 重复调用 = 多个 Run；
- 分页是**不透明游标**：CLI 的列表类命令只取服务端默认一页（`limit=50`），完整翻页需要 Agent 直接用 REST；
- 错误信封是**扁平** `{code,message,detail,retryable,trace_id}`（D-39），CLI/MCP 只投影不重编码；
- 响应**允许新增字段**：CLI 用结构体挑需要的字段，MCP 把整个 JSON 原样透传，都不会因未知字段失败；
- 日志**不走 SSE**（C-9.2）：`wait --follow` 只消费状态事件，日志仍走 `logs` 端点。

## 4 CLI `tpctl`

### 4.1 全局选项

写在**命令之前**（与 `docker` / `kubectl` 一致）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `--base-url <url>` | `$TESTPLANE_BASE_URL` 或 `http://127.0.0.1:8080` | 只接受 http/https；**不要**带 `/api/v1` 后缀 |
| `--token <token>` | `$TESTPLANE_TOKEN` 或 `dev` | 出站 `Authorization: Bearer`；dev 模式下值即 subject |
| `--timeout <dur>` | `30s` | 单次 HTTP 超时；纯数字按秒（`5` / `0.5`），也可写 `30s` / `500ms` |
| `--json` | `false` | 机器可读输出；也可写在子命令之后（两种位置等价） |
| `--stdio` | `false` | 等价于 `tpctl mcp` |

> `wait` 的 `--timeout 3600` 是**总等待时长**；因为全局选项只解析到子命令为止，二者不会互相覆盖。

### 4.2 命令表

| 命令 | 用途 | 成功输出（`--json`） | 示例 |
|---|---|---|---|
| `submit -f <manifest.yaml> [--dry-run]` | 提交 Run / 只校验 | `{run_id,status,kind,submitted_at,tasks[]}`；dry-run 透传 `{valid,issues[],resolved_preview?}` | `tpctl submit -f run.yaml --json` |
| `status <run_id>` | Run 摘要 | `{run_id,name,kind,status,terminal,task_counts,error,tasks[],next_action?,hint?}` | `tpctl status r-20261105-0001 --json` |
| `wait <run_id> [--follow] [--timeout 3600] [--interval 3]` | 等待终态 | `{...status 字段,outcome,elapsed_seconds,wait_mode,fallback_reason?,timed_out?}` | `tpctl wait r-20261105-0001 --follow --json` |
| `report <run_id> [--md]` | 报告 JSON / Markdown | JSON 原文（`--json` 单行；默认缩进） | `tpctl report r-20261105-0001 --md` |
| `logs <task_id> [--attempt 1] [--offset N] [--tail N] [--limit N] [--stream stdout\|stderr]` | Attempt 日志 | `{task_id,attempt,stream,offset,next_offset,coverage,text}` | `tpctl logs t-0002 --attempt 1 --tail 200` |
| `artifacts <run_id>` / `artifacts --task <task_id> [--attempt 1]` | 产物清单 | `GET .../artifacts` 原样 | `tpctl artifacts r-20261105-0001 --json` |
| `files <run_id> <path> [--out <file>] [--range <range>]` | 下载产物 | `{run_id,path,api_path,http_status,content_type,size_bytes,total_size_bytes,sha256,sha256_scope,out?,range?,content_range?}` | `tpctl files r-1 tasks/t-2/attempts/1/result.json --out r.json` |
| `cancel <run_id>` / `cancel --task <task_id>` | 取消 + 回读确认 | `{target,id,accepted,http_status,status,confirmed}` | `tpctl cancel r-20261105-0001 --json` |
| `testers [--role test\|pre_check] [--orphaned]` | Tester 目录 | `GET /testers` 原样 | `tpctl testers --role pre_check` |
| `queue` | 槽位 / 等待原因 / Volcano | `GET /queue-status` 原样 | `tpctl queue --json` |
| `profiles` | Execution Profile | `GET /profiles` 原样 | `tpctl profiles --json` |
| `mcp` | stdio 上启动 MCP server | 协议流（见 §5） | `tpctl mcp` |
| `help` | 用法 | 帮助文本 | `tpctl help` |

细节：

- `submit -f -` 从 stdin 读 Manifest（Agent 生成内容后直接管道提交）；
- `logs` 无论是否 `--json`，都把 `X-Log-Offset`（以及非空的 `X-Log-Coverage`）打到 **stderr**；`--json` 时同一个值也在 `next_offset` 里；
- `artifacts` 无 `--json` 时按 `result → raw → log → report → other` 分组，每组打印条数、总大小；`--json` 原样输出控制面 JSON；
- `files` 的语义见 §4.3；`--json` 必须配合 `--out`（不把二进制写进 JSON stdout）；
- `--json` 无论写在命令前还是命令后，错误输出也统一为 `{"error":{...}}` 到 stderr。

### 4.3 artifacts / files

**`artifacts`**（只读）：

```bash
tpctl artifacts r-20261105-0001                 # Run 级，按 kind 分组
tpctl artifacts --task t-0002 --attempt 1       # Attempt 级
tpctl artifacts r-20261105-0001 --json          # 原样 JSON（items + by_kind + total_size_bytes）
```

**`files`**（下载）：

```bash
# 1) 落盘 + 打印 sha256（默认下载全文，≤1 GiB；超过服务端 413，需 --range）
tpctl files r-20261105-0001 tasks/t-0002/attempts/1/result.json --out result.json

# 2) 只取前 1KiB（Range 透传；--range 可写 0-1023 或 bytes=0-1023）
tpctl files r-20261105-0001 tasks/t-0002/attempts/1/logs/stdout.log --range 0-1023 --out head.txt

# 3) 不落盘：body 原样写 stdout，摘要写 stderr（可管道给 jq / 文件）
tpctl files r-20261105-0001 report.json | jq .
```

- `sha256` 是**本次收到的字节**的哈希；`sha256_scope` 为 `full`（status 200 或整文件）或 `partial`（206 且未取全），不要把它当整文件指纹；
- `path` 直接接受 `artifacts` 返回的写法（`<run_id>/tasks/...`）或 run 内相对路径（`tasks/...`）；CLI 自动剥离 `<run_id>/` 前缀，JSON 里的 `api_path` 是实际发给 `/files?path=` 的值；
- `--out` 会自动创建父目录（`MkdirAll`）；`--out -` 表示 stdout；
- `--json` 只输出元数据，**必须**配 `--out`，否则 64 用法错误——避免把二进制混进 JSON。

### 4.4 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功。`wait`：Run `succeeded`；`status`/`report`/`artifacts` 等：读取成功（**不**表示 Run 成功） |
| `1` | 命令失败（HTTP 4xx/5xx、本地处理失败）；`wait`：`failed` / `completed_with_errors` / `service_unavailable`；`submit --dry-run`：`valid=false` |
| `2` | `wait`：Run `canceled` |
| `3` | `wait`：总超时仍未终态 |
| `4` | 连不上控制面（DNS / 连接拒绝 / 传输超时） |
| `64` | 用法错误（未知命令、缺参数、非法枚举、读不到 Manifest、`files --json` 缺 `--out` 等） |

对应关系：`outcome` 的 `success|failed|canceled|timeout` ↔ `0|1|2|3`；传输失败不产生 `outcome`，直接 `4`。

**只有 `wait` 把 Run 结局映射成退出码**。`status` 是查询命令：能读到 Run 就返回 `0`，Agent 应看 `status` / `terminal` / `error` / `next_action` 判断结局。

### 4.5 `wait --follow`：SSE 优先，自动降级

```bash
tpctl wait r-20261105-0001 --follow --timeout 3600 --json
```

流程（`server/internal/mcp/sse.go` 的 `Wait`）：

1. 先 `GET /runs/{id}` 拿当前状态；已终态直接输出并退出（不订阅 SSE）；
2. 未终态且 `--follow`：`GET /runs/{id}/events`（`Accept: text/event-stream`）；
   - 按 `id:` 记 `Last-Event-ID`，重连时带上，服务端从 `seq+1` 回放（07 §7）；
   - `run.state_changed` 更新状态；`run.finished` 触发一次 `GET /runs/{id}` 拿权威终态后退出；
   - `: ping` 心跳直接忽略；
3. **断线重连 3 次**，指数退避 1s → 2s → 4s（受总 timeout 约束）；
4. **SSE 不可用**时自动降级为轮询：
   - 404 / 405 / 5xx、连接失败、`Content-Type` 不是 `text/event-stream`、或重连 3 次仍失败；
   - CLI 打一行 stderr：`tpctl: SSE 不可用（原因），已降级为轮询`；JSON 里 `wait_mode=poll`、`fallback_reason=原因`；
   - **401/403 不降级**（轮询也会失败，直接按退出码 1 报错）；
5. 总超时退出码 3，输出最后一次已知状态；`timed_out=true`；
6. 不订阅 SSE 时（默认 `wait`）仍是最简单的轮询，行为与上一版一致。

> **回放噪声**：服务端会从 `seq=1` 回放历史事件。`Wait` 会跳过「初始 GET 状态之前」的历史转移，避免终端出现 `testing → queued → testing` 的倒序噪声；`run.finished` 与窗口起点之后的事件正常展示。

> **为什么降级**：Agent 不应该因为服务端没开 SSE / 网关缓冲 / 老版本控制面就失败；`wait --follow` 是**优化路径**，不是硬依赖。

### 4.6 输出约定

| 流 | 内容 |
|---|---|
| stdout | 成功结果：`--json` 时**恰好一个 JSON 文档**；否则给人读的摘要（或 `report --md` 的 Markdown 原文、`logs`/`files` 的原文） |
| stderr | 进度（`wait` 的状态变化）、SSE 降级说明、`next_action`、错误原因、`X-Log-Offset`、下载摘要、`report` 未生成的重试提示 |

- `--json` 失败时 stdout 为空，stderr 是 `{"error":{http_status,code,message,retryable,trace_id,retry_after?,next_action?,hint?}}`；
- 非 `--json` 失败时 stderr 是 `tpctl: HTTP 404 NOT_FOUND: ... trace_id=...`，已知错误码再加一行 `tpctl: 下一步: ...`；
- `wait` 的状态变化只在**变化时**打一行，不刷屏。

### 4.7 Agent 推荐序列

```bash
# 1) 提交（stdout 只给 machine-readable 结果）
run_id=$(tpctl submit -f run.yaml --json | jq -r .run_id)

# 2) 等终态：SSE 优先；退码直接告诉 Agent 结局（0/1/2/3/4）
tpctl wait "$run_id" --follow --timeout 3600 --json
case $? in
  0) tpctl report "$run_id" --md ;;
  1) tpctl report "$run_id" --md; tpctl artifacts "$run_id" --json ;;
  2) echo "已取消" ;;
  3) echo "仍在跑，稍后再 wait" ;;
  4) echo "控制面不可达" ;;
esac

# 3) 产物：先列清单，再按 path 下载
tpctl artifacts "$run_id" --json | jq -r '.items[] | select(.kind=="raw") | .path' |
  while read -r p; do tpctl files "$run_id" "$p" --out "out/$(basename "$p")"; done

# 4) 失败时看日志（stdout 原文，stderr 是下一次 offset）
tpctl logs t-0002 --attempt 1 --tail 200
```

### 4.8 实现约束

- 只用标准库 `flag`；子命令 flag 与位置参数**可交错**（`wait r-1 --follow --timeout 10`）；
- Manifest 按首字符判断 `{`/`[` → `application/json`，否则 `application/yaml`；不解析内容；
- 单次**普通**应答读取上限 32 MiB；`files` 下载走流式（不整段入内存，契约单文件 ≤1 GiB）；SSE 用独立 `http.Client{}`（无 TotalTimeout，由 ctx deadline 控制）；
- HTTP 客户端不复用服务端中间件：不做重试、不做连接池调参、不读环境代理配置以外的全局状态。

## 5 MCP Server

### 5.1 传输与生命周期

- **传输**：stdio，**一行一条** JSON-RPC 2.0 消息（UTF-8，`\n` 分隔），没有 `Content-Length` 帧；
- **方法**：`initialize`、`ping`、`tools/list`、`tools/call`；其余方法返回 `-32601`；
- **无会话状态**：`initialize` 只做版本与能力协商，不建立会话；`notifications/initialized` 等通知**不应答**；
- **同步 request/response**：`tools/call` 一次调用只产生一次应答；`wait_run(follow=true)` 也是服务端侧等完再返回，**不发 progress notification**（见 §5.5）；
- **退出**：stdin EOF 或写失败即返回，进程退出码 `0`（协议错误不杀进程）。

### 5.2 `initialize` 应答

```json
{
  "protocolVersion": "2025-06-18",
  "capabilities": { "tools": {} },
  "serverInfo": { "name": "testplane", "version": "0.1.0" }
}
```

- 客户端请求的 `protocolVersion` 在 `2024-11-05` / `2025-03-26` / `2025-06-18` 内则回显，否则回最新支持版本；
- `serverInfo.version` 是 **agent 适配层版本**，不是控制面版本；控制面版本读 `GET /api/v1/meta`。

### 5.3 工具清单（11 个）

| 工具 | 对应 REST | 备注 |
|---|---|---|
| `testplane_submit_run` | `POST /runs`（`?dry_run=true`） | `manifest` 是 YAML/JSON 原文；不做幂等 |
| `testplane_get_run` | `GET /runs/{run_id}` | 不等待，原样返回 |
| `testplane_wait_run` | 轮询或 SSE（`follow`） | 返回 `outcome`、`wait_mode`、`fallback_reason`；`timeout` 不是错误 |
| `testplane_cancel_run` | `POST /runs/{run_id}/cancel` | 202/200 后回读确认 |
| `testplane_list_testers` | `GET /testers` | `role` / `orphaned` |
| `testplane_get_report` | `GET /runs/{run_id}/report[.md]` | `format=json\|markdown` |
| `testplane_get_logs` | `GET /tasks/{id}/attempts/{idx}/logs` | 返回 `text` + `next_offset` |
| `testplane_list_artifacts` | `GET /runs/{id}/artifacts` 或 `GET /tasks/{id}/attempts/{idx}/artifacts` | `run_id` 与 `task_id` **二选一** |
| `testplane_get_file` | `GET /runs/{id}/files?path=` | 返回**摘要**（path/size/sha256/前 2KB），不塞二进制 |
| `testplane_queue_status` | `GET /queue-status` | 三视图不可相加 |
| `testplane_list_profiles` | `GET /profiles` | 已实现；返回 Profile + `mounts` |

### 5.4 `inputSchema`

所有 Schema 都是 `type=object` + `additionalProperties=false`：拼错的参数会被 `-32602` 拒绝，而不是被静默忽略。下面按工具分列（`//` 后是工具名，不是 JSON 内容；实际应答里没有注释）：

```jsonc
// testplane_submit_run
{"type":"object","properties":{
  "manifest":{"type":"string","description":"Run Manifest 原文（YAML 或 JSON 对象，顶层是 run:）"},
  "dry_run":{"type":"boolean","description":"true 时只校验、不落库","default":false}},
 "required":["manifest"],"additionalProperties":false}

// testplane_get_run
{"type":"object","properties":{
  "run_id":{"type":"string","description":"Run ID（不透明字符串）"}},
 "required":["run_id"],"additionalProperties":false}

// testplane_wait_run
{"type":"object","properties":{
  "run_id":{"type":"string"},
  "timeout_seconds":{"type":"number","default":3600,"minimum":0.1,"maximum":86400},
  "interval_seconds":{"type":"number","default":3,"minimum":0.01,"maximum":60},
  "follow":{"type":"boolean","default":false,"description":"true 时优先用 SSE；不可用自动降级轮询"}},
 "required":["run_id"],"additionalProperties":false}

// testplane_cancel_run
{"type":"object","properties":{"run_id":{"type":"string"}},
 "required":["run_id"],"additionalProperties":false}

// testplane_list_testers
{"type":"object","properties":{
  "role":{"type":"string","enum":["test","pre_check"]},
  "orphaned":{"type":"boolean","default":false}},
 "additionalProperties":false}

// testplane_get_report
{"type":"object","properties":{
  "run_id":{"type":"string"},
  "format":{"type":"string","enum":["json","markdown"]}},
 "required":["run_id"],"additionalProperties":false}

// testplane_get_logs
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "attempt":{"type":"integer","default":1,"minimum":1,"maximum":1000},
  "stream":{"type":"string","enum":["stdout","stderr"]},
  "offset":{"type":"integer","minimum":0},
  "limit":{"type":"integer","minimum":1,"maximum":65536},
  "tail":{"type":"integer","minimum":1,"maximum":1000000}},
 "required":["task_id"],"additionalProperties":false}

// testplane_list_artifacts（run_id XOR task_id）
{"type":"object","properties":{
  "run_id":{"type":"string"},
  "task_id":{"type":"string"},
  "attempt":{"type":"integer","default":1,"minimum":1,"maximum":1000}},
 "additionalProperties":false}

// testplane_get_file
{"type":"object","properties":{
  "run_id":{"type":"string"},
  "path":{"type":"string"},
  "range":{"type":"string","description":"可选 HTTP Range；缺省按 max_bytes 取前缀"},
  "max_bytes":{"type":"integer","default":1048576,"minimum":1024,"maximum":33554432}},
 "required":["run_id","path"],"additionalProperties":false}

// testplane_queue_status / testplane_list_profiles
{"type":"object","properties":{},"additionalProperties":false}
```

### 5.5 `tools/call` 结果

成功（所有 JSON 类工具）：

```json
{"content":[{"type":"text","text":"{\"id\":\"r-20261105-0001\",\"status\":\"testing\"}"}]}
```

成功时省略 `isError`（MCP 默认 `false`）；失败时才写 `"isError": true`。

- `text` 是**一个 JSON 文档的字符串**；`testplane_get_report(format=markdown)` 例外——`text` 是 Markdown 原文；
- `testplane_wait_run` 返回 `{run_id,status,terminal,outcome,task_counts,error,elapsed_seconds,wait_mode,fallback_reason?,timed_out?,next_action?}`；
  - `wait_mode`: `sse` 或 `poll`；`fallback_reason` 只在降级时出现；
  - `outcome`: `success | failed | canceled | timeout`；不是 HTTP 状态；
  - `follow=true` 仍只返回**一次**结果（服务端侧等完），不发 progress notification——保持与未声明 `notifications/progress` 能力的客户端兼容；
- `testplane_get_logs` 返回 `{task_id,attempt,stream,offset,tail?,next_offset,coverage,text}`；
- `testplane_get_file` 返回**摘要**（不返回完整二进制）：

```json
{
  "run_id": "r-1", "path": "r-1/tasks/t-1/attempts/1/logs/stdout.log",
  "api_path": "tasks/t-1/attempts/1/logs/stdout.log",
  "http_status": 206, "content_type": "text/plain; charset=utf-8",
  "size_bytes": 1024, "total_size_bytes": 10485760,
  "sha256": "…", "sha256_scope": "partial",
  "range": "bytes=0-1023", "content_range": "bytes 0-1023/10485760",
  "encoding": "text", "preview": "前 2KB 文本…", "truncated": true
}
```

- `path` 接受 `testplane_list_artifacts` 返回的写法（含 `<run_id>/` 前缀）或 run 内相对路径；`api_path` 是实际发给 `/files?path=` 的值；
- 默认用 `Range: bytes=0-1048575` 取前 1 MiB；请求大小受 `max_bytes`（≤32 MiB）约束；
- `sha256` 是**收到字节**的哈希，`sha256_scope=full|partial` 标明范围；完整文件请走 HTTP 下载（CLI `files`）；
- 文本（合法 UTF-8、无控制字符）→ `encoding=text` + `preview`；否则 `encoding=base64` + `preview_base64`（前 2KB）。

执行失败（控制面返回 4xx/5xx）：

```json
{"content":[{"type":"text","text":"{\"error\":{\"http_status\":404,\"code\":\"NOT_FOUND\",\"message\":\"Run 不存在\",\"retryable\":false,\"trace_id\":\"01J...\",\"next_action\":\"核对 ID 与资源是否存在…\"}}"}]}
```

`isError=true`，但 JSON-RPC 层仍是**成功应答**——MCP 规范把「工具执行失败」与「协议错误」分开，Agent 需要能拿到结构化原因继续决策。`error.next_action` / `error.hint` 来自 §6 的共享映射；`notifications` 不用于报错。

连不上控制面：`isError=true`，`code=UNREACHABLE`、`retryable=true`。

### 5.6 协议错误码

| 场景 | code |
|---|---|
| 坏 JSON / 读不出消息 | `-32700` |
| `jsonrpc` 不是 `"2.0"`、缺 `method` | `-32600` |
| 未知方法（含未实现的 `resources/*`、`prompts/*`） | `-32601` |
| 未知工具、缺必填参数、参数类型/枚举错误、未知字段、`artifacts` 的 XOR 不满足 | `-32602` |
| 服务端内部编码失败 | `-32603` |

### 5.7 启动方式

```bash
# 方式一：子命令
tpctl --base-url http://127.0.0.1:8080 --token dev mcp

# 方式二：全局开关（等价）
tpctl --base-url http://127.0.0.1:8080 --token dev --stdio
```

任一种方式下，**stdout 只写协议消息**，任何诊断信息都写 stderr。MCP 客户端配置示例：

```json
{
  "mcpServers": {
    "testplane": {
      "command": "tpctl",
      "args": ["--base-url", "http://127.0.0.1:8080", "--token", "dev", "mcp"]
    }
  }
}
```

### 5.8 SSE 与 MCP 的关系

- MCP 的 `tools/call` 是同步 request/response，**不能流式推**；`testplane_wait_run(follow=true)` 的 SSE 是**服务端侧**等待，工具调用仍一次返回；
- 不发送 `notifications/progress`：客户端未声明该能力时发通知会破坏兼容；未来若要发，先看 `initialize` 的 `capabilities.progress` 再决定（§9）；
- SSE 降级原因写进结果字段 `fallback_reason`（不是 stderr，MCP 没有 stderr 语义）。

## 6 错误映射与 `next_action`

### 6.1 HTTP → 入口

| 控制面 | CLI | MCP |
|---|---|---|
| 2xx | `0`（`wait` 再按终态细分） | `isError=false` + `content.text` |
| `400/404/409/413/429/500/5xx` | `1`，stderr 一行或 `{"error":...}` | `isError=true` + `{"error":{...}}` |
| 传输失败（DNS/连接拒绝/超时） | `4` | `isError=true`，`code=UNREACHABLE` |
| `report` 404（未生成） | `1`，stderr 提示「run.finished 后异步生成」+ `Retry-After` | `isError=true`，错误对象带 `retry_after` |
| 用法错误 | `64` | `-32602`（`tools/call` 的参数层） |

`trace_id` 与响应头 `X-Request-Id` 同源，原样透传给 Agent；CLI/MCP 都不吞掉它。

### 6.2 共享 `next_action` 映射

实现在 `server/internal/mcp/hints.go`（CLI 与 MCP 共用同一份）。字段：`{code, retryable, next_action, hint}`；`retryable` 是码级默认，实例级以错误信封的 `retryable` 为准。

| code | 可重试（码级） | next_action | hint |
|---|---|---|---|
| `VALIDATION_ERROR` | 否 | 按 `detail.issues[]` 修正字段后重新提交 | issue 带 `path`（如 `tests[2].args`） |
| `UNAUTHENTICATED` | 否 | 重新获取 token（`$TESTPLANE_TOKEN` / `--token`） | 控制面无匿名端点 |
| `FORBIDDEN` | 否 | 确认角色：viewer 只读、operator 可提交/取消、admin 可维护 | 403 由服务端统一判定 |
| `NOT_FOUND` | 否 | 核对 ID 与资源是否存在；先 list 再操作 | ID 不透明 |
| `CONFLICT` | 是 | 先 wait 到终态再重试；不要并发改同一 Run | 以先落库状态为准 |
| `TESTER_NOT_FOUND` | 否 | 用 testers 看目录，修正名称/版本 | 注册表自发现（K-3） |
| `TESTER_VERSION_PINNED` | 否 | 发布新 version 后改 Manifest 引用 | 同 name+version 不可覆盖 |
| `TESTER_INVALID` | 否 | 修 `tester.yaml` / `run.sh`，等重扫 | 附录 C |
| `EXECUTION_PROFILE_NOT_FOUND` | 否 | 用 profiles 看已登记 Profile，改 `tests[].execution.profile` | 引导期例外已移除 |
| `RUN_TOO_LARGE` | 否 | 拆分 Run（单 Run Task ≤200） | 含 precheck |
| `RESOURCE_EXHAUSTED` | 是 | 指数退避后重试，或降低提交频率 | 429 |
| `ARTIFACT_QUOTA_EXCEEDED` | 否 | 清理旧 Run 产物后重试 | 平台级配额 |
| `EXECUTOR_UNAVAILABLE` | 是 | 等 30–60s 重试 wait / 重新提交 | 执行器暂不可用 |
| `EXECUTOR_POD_ERROR` | 否 | 看 `k8s_reason`，调 `resources` 后重跑 | OOM/Evicted/NodeLost |
| `IMAGE_PULL_FAILED` | 否 | 核对镜像/registry 凭据后提交 | 看 Attempt 与事件 |
| `MOUNT_UNAVAILABLE` | 否 | 对照 profile.mounts 补挂载能力 | 不是 Tester 参数问题 |
| `ENDPOINT_UNREACHABLE` | 是 | 等服务就绪后重试；看 `deploy.queue_reason` | 先看 precheck 日志 |
| `PRECHECK_FAILED` | 否 | 检查健康检查/端口/模型名；看 precheck Attempt 日志 | precheck 是 seq=0 |
| `TASK_TIMEOUT` | 否 | 调大 `tests[].timeout_seconds` / `TESTPLANE_TASK_TIMEOUT` | 先确认没卡外部资源 |
| `TESTER_NONZERO_EXIT` | 否 | 看 Tester 日志与 result.json | 通常是测试失败 |
| `RESULT_MISSING` | 否 | 看日志：Tester 没写 result.json | 唯一必产出文件 |
| `RESULT_INVALID` | 否 | 对照契约修 Tester 输出 | 字段不限、必须合法 JSON |
| `CLEANUP_FAILED` | 是 | 看 deploy/cleanup 日志，必要时人工清理 | 不影响日志/产物 |
| `CANCELED` | 否 | 重新 submit（不做幂等，D-37） | 重复取消返回 200 |
| `INTERNAL_ERROR` | 是 | 带 trace_id 报平台；可退避重试一次 | 与 `X-Request-Id` 同源 |
| `DEPLOY_FAILED` | 否 | 平台调用 Genet 失败，**不要重试**同样参数；带 trace_id 找平台 | 502 |
| `DEPLOY_BLOCKED` | 是 | 看 `/queue-status` 的 volcano 块，等队列前进后重试 | reason 在 `deploy.failure.detail.reason` |
| `SERVICE_UNAVAILABLE`（终态） | 是 | 等服务就绪后重新 submit；或改用 `service.url` | Deploy 收口 |
| `UNREACHABLE`（合成） | 是 | 检查 `--base-url` / 控制面 / 网络后重试 | 请求没到服务端 |

CLI 非 `--json` 时打印 `tpctl: 下一步: <next_action>`；`--json` 时放在 `error.next_action`。MCP 放在 `error.next_action`（另带 `error.hint`）。Run 的**失败终态**也会带提示：`status` / `wait` 的输出里有顶层 `next_action` / `hint`（取 `Run.error.code`，没有则按 `canceled` / `service_unavailable` 终态）。

## 7 测试与验收

| 包 | 测试文件 | 覆盖 |
|---|---|---|
| `server/internal/mcp` | `server_test.go` | `httptest` 假控制面 + `io.Pipe` 驱动 stdio：`initialize`、`tools/list`（11 工具 + schema）、`tools/call` 成功（get/submit/wait/logs/report/profile/artifacts/get_file/cancel）、失败（404 → `isError` + `next_action`）、`-32601`/`-32602`/`-32700`、通知不应答、传输失败 `UNREACHABLE`；SSE 解析（注释/多行 data/`id`）；`Wait(follow)` 成功、**404 降级轮询**、**断线重连并携带 `Last-Event-ID`**、**回放噪声抑制**；`hints` 全码与抽检语义；`get_file` 文本/二进制预览、`sha256_scope`、`<run_id>/` 前缀剥离 |
| `server/cmd/tpctl` | `main_test.go` | `httptest` 假控制面：submit（YAML/JSON/stdin/dry-run）、status、wait（5 种结局 + 退出码 0/1/2/3/4）、`wait --follow`（SSE 成功 + 降级 + stderr 说明）、report（md/pretty/compact）、logs（`X-Log-Offset` 进 stderr）、artifacts（run/task/分组/原样 JSON）、files（`--out`/`--range`/sha256/`--json` 需 `--out`/stdout 原文/前缀剥离）、cancel（run/task）、testers/queue/profiles、全局选项位置、`next_action`（错误响应与失败终态）、用法错误 64 |

```bash
cd server
go test ./internal/mcp/ ./cmd/tpctl/   # 本特性，不需要数据库/K8s
go build ./... && go vet ./... && go test ./...  # 全仓门禁（集成测试按 TESTPLANE_TEST_DATABASE_URL 跳过）
```

真控制面验收（Kind + port-forward）：

```bash
tpctl --base-url http://127.0.0.1:8080 submit -f run.yaml --json
tpctl --base-url http://127.0.0.1:8080 wait <run_id> --follow --json
tpctl --base-url http://127.0.0.1:8080 report <run_id> --md
tpctl --base-url http://127.0.0.1:8080 artifacts <run_id>
```

## 8 已知限制

1. **MCP 不返回完整文件**：`testplane_get_file` 只给摘要（默认前 1 MiB、最多 32 MiB）；完整产物走 HTTP / CLI `files`。
2. **`sha256` 可能是局部哈希**：`sha256_scope=partial` 时是「收到的字节」的哈希，不是整文件指纹；MCP 与 CLI 都明确标注。
3. **SSE 降级不重试 401/403**：鉴权错误直接失败（轮询也会失败）；其余不可用一律降级。
4. **SSE 事件不打印任务粒度**：`wait --follow` 只用事件维护 Run 状态、等 `run.finished`；task 级进度不在终端逐条打印（避免 1000 条回放刷屏）。需要 task 明细走 `status`。
5. **列表类命令不分页**：`testers` / `artifacts` / `profiles` 只取默认页（后两者本身不分页）；需要翻页直接用 REST。
6. **`wait` 遇控制面 4xx/5xx 立即失败**：不无限重试；只有 SSE 断线做 3 次短退避重连，之后降级轮询。
7. **MCP 只做 tools**：没有 resources / prompts / sampling / elicitation，也不发 `notifications/progress`。
8. **不处理 OIDC/CSRF**：token 由外部注入（`$TESTPLANE_TOKEN`）；dev 模式默认 `dev`。生产接 OIDC 后再补「如何拿 token」，但**不做凭据代理**。
9. **不复用 `internal/api` 的 DTO**：字段是本地最小投影，响应新增字段不会破坏解析，但也不会自动出现在 CLI 输出里。
10. **`files` 不校验服务端 sha256**：控制面不返回整文件哈希；Agent 若需完整性校验，应在下载后自行比对产物清单里的 `sha256`（若已计算）。
11. **产物路径有前缀差异**：`/runs/{id}/artifacts` 返回的 `path` 是 storage-root 相对（含 `<run_id>/`），而 `/runs/{id}/files?path=` 要 run 目录内相对路径（openapi 描述与实现不一致）。CLI/MCP 统一在客户端剥离 `<run_id>/` 前缀并回显 `api_path`；理想修复在 API 侧，届时 adapter 可直接透传。

## 9 下一步（按 ROI）

| 优先级 | 能力 | 具体动作 |
|---|---|---|
| 1 | 进度通知（MCP） | `initialize` 协商 `capabilities.progress`；`wait_run(follow=true)` 按 `run.state_changed` / `task.state_changed` 发 `notifications/progress`（带 `progressToken`），同时保留同步返回 |
| 2 | 分页与批量 | 列表类命令补 `--cursor/--limit`；`submit --matrix` 客户端展开；MCP `testplane_submit_batch` 返回 `{run_ids[]}` |
| 3 | 幂等提示（不引入幂等键） | D-37 不变：提交前 `GET /runs?submitter=...` 查重提示、返回 `submitted_by`；绝不静默去重 |
| 4 | 更完整的产物 | `artifacts --verify`（比对清单 sha256）；`files --dir` 按 kind 批量下载；MCP `testplane_get_file` 支持 `format=full`（base64 上限内） |
| 5 | SSE 事件时间线 | `tpctl events <run_id>`（`events/history` + `--follow`），供 Agent 解释「为什么失败」而不只是结局 |
| 6 | 契约门禁 | 表驱动单测：每个 CLI 子命令 / MCP 工具 ↔ openapi operationId，防止只改 REST 不改 adapter |
| 7 | 鉴权体验 | device flow / token 刷新说明（仍不做凭据代理） |

## 10 文件清单

```text
server/cmd/tpctl/main.go             入口：全局选项、命令分发、退出码、usage
server/cmd/tpctl/client.go           最小 REST 客户端 + SSE 打开（Bearer/超时/错误信封/Range）
server/cmd/tpctl/commands.go         submit/status/wait/report/logs/artifacts/files/cancel/testers/queue/profiles
server/cmd/tpctl/main_test.go        httptest 假控制面 + 退出码/输出/SSE 降级断言
server/internal/mcp/client.go        MCP 专用最小 REST 客户端（含 OpenRunEvents）
server/internal/mcp/server.go        stdio 帧、JSON-RPC 2.0、initialize/tools/list/tools/call
server/internal/mcp/tools.go         11 个工具的 description + inputSchema + 转发实现
server/internal/mcp/sse.go           SSE 帧解析 + 共享 Wait（SSE 优先、降级轮询、断线重连）
server/internal/mcp/hints.go         共享错误码 → {retryable,next_action,hint} 映射
server/internal/mcp/server_test.go   协议面 + SSE/wait + 工具 + hints 测试
```
