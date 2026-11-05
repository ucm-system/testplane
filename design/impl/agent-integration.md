# Agent 接入实现（agent-integration）

> **范围：** P1 首批的 Agent 亲和入口：`tpctl` CLI 与 MCP Server（stdio / JSON-RPC 2.0），以及两者与 REST 的关系、退出码与错误映射、测试方式。
> **不在范围：** REST 契约本身（[`views/07-api.md`](../views/07-api.md)）、状态机与编排（[`control-plane.md`](control-plane.md)）、执行层（[`execution.md`](execution.md)）、Web UI。
> **基线：** [`server/api/openapi.yaml`](../../server/api/openapi.yaml)（MVP 25 operations）+ [`IMPLEMENTATION.md`](../IMPLEMENTATION.md) §13。
> **代码：** [`server/cmd/tpctl`](../../server/cmd/tpctl)、[`server/internal/mcp`](../../server/internal/mcp)。
> **定位：** **薄 adapter**。CLI 与 MCP 不引入新语义，只是 REST 的两种调用形态；控制面仍是唯一权威。

## 1 为什么做

TestPlane 的定位是 **AI Agent 友好**：Agent 要在**不写代码**的前提下完成「提交 → 等待 → 取报告 → 看日志 → 取消」闭环，并且能可靠地判断每一步的结局。控制面已有 25 个 REST 端点，但 REST 对 Agent 有三处摩擦：

| 摩擦 | 现象 | 本轮解法 |
|---|---|---|
| 工具调用 | Agent 运行时通常只会「调工具」，不会拼 HTTP、管 headers、解 SSE | MCP server 把 9 个高频动作注册成工具 |
| 结局判定 | HTTP 200 只表示「查到了」，不表示 Run 成功；Agent 需要解析状态枚举 | CLI 把 Run 结局映射成退出码；MCP 返回 `outcome` 字段 |
| 交互式排查 | 日志续读要靠 `X-Log-Offset`，报告未生成是 404 + `Retry-After` | CLI 把续读坐标打到 stderr，把 404 变成「尚未生成，N 秒后重试」 |

三条设计原则：

1. **一一对应**：每个子命令 / 工具都能追到一个 REST 端点，不发明控制面没有的动作（见 §3 映射表）。
2. **机器可读优先**：`--json` 时 stdout 只有一个 JSON 文档；进度、告警、日志续读坐标一律走 stderr，不污染结果。
3. **无状态**：CLI/MCP 不缓存、不落库、不写文件（只读 Manifest 输入），进程退出即无残留。

## 2 定位与边界

| 做 | 不做 |
|---|---|
| 把 REST 端点包装成命令 / 工具 | 新增状态、聚合或 Gate 判定（§13.1 的 Non-Goal 照旧） |
| 透传控制面错误信封（`code/message/detail/retryable/trace_id`） | 改写错误码，或把 4xx 说成成功 |
| 把 Run 终态映射成退出码 / `outcome` | 替 Agent 决定「这次测试算不算过」 |
| 读 `-f` 指定的 Manifest（也支持 stdin） | 写文件、改 Manifest、做 Matrix 展开 |
| 用 `--token` / `$TESTPLANE_TOKEN` 出站鉴权 | 代理/缓存凭据、把 token 写进输出或日志 |
| 依赖 Go 标准库（`flag` / `net/http` / `encoding/json`） | 引入 cobra、MCP SDK 或第二套 HTTP 客户端语义 |

**为什么不引入 MCP SDK / CLI 框架**：本轮协议面只有 4 个方法（`initialize` / `tools/list` / `tools/call` / `ping`）和 10 个子命令；引入框架只会把「命令 ↔ 端点」的对应关系藏进 DSL，反而妨碍 Agent 读源码。协议面约 200 行，测试可以直接用 `io.Pipe` 驱动。

**与 `internal/api` 的关系**：CLI 与 MCP **都不 import** `server/internal/api`（那是服务端 handler/DTO 包）。两者各有一份约 100 行的 HTTP 客户端，只做「拼 URL + Bearer + 超时 + 错误信封解析」。这与 §15.3 #7「MVP 手写 handler/DTO、未采用 codegen」保持一致。

## 3 与 REST 的关系

| CLI 命令 | MCP 工具 | REST | 说明 |
|---|---|---|---|
| `submit -f <file>` | `testplane_submit_run` | `POST /api/v1/runs`（`?dry_run=true`） | Manifest 原样转发；按内容首字符选 YAML/JSON `Content-Type` |
| `status <run_id>` | `testplane_get_run` | `GET /api/v1/runs/{id}` | 派生 `terminal` 字段 |
| `wait <run_id>` | `testplane_wait_run` | 轮询 `GET /api/v1/runs/{id}` | 派生 `outcome`；不新增等待端点 |
| `cancel <run_id>` | `testplane_cancel_run` | `POST /api/v1/runs/{id}/cancel` + 回读 `GET` | 202/200 后确认一次 |
| `cancel --task <task_id>` | — | `POST /api/v1/tasks/{id}/cancel` + 回读 `GET` | MCP 工具表按要求只保留 Run 级取消 |
| `report <run_id>` | `testplane_get_report` | `GET /api/v1/runs/{id}/report` | JSON 透传；`--json` 单行、默认缩进 |
| `report <run_id> --md` | `testplane_get_report`（`format=markdown`） | `GET /api/v1/runs/{id}/report.md` | 原样打印 Markdown，不做渲染 |
| `logs <task_id>` | `testplane_get_logs` | `GET /api/v1/tasks/{id}/attempts/{idx}/logs` | 透传 `offset/limit/tail/stream`；`X-Log-Offset` → `next_offset` |
| `testers` | `testplane_list_testers` | `GET /api/v1/testers` | `role` / `orphaned` 过滤 |
| `queue` | `testplane_queue_status` | `GET /api/v1/queue-status` | 三个视图**不可相加**（07 §5.3、D-49） |
| `profiles` | `testplane_list_profiles` | `GET /api/v1/profiles` | **端点尚未实现**（IMPLEMENTATION §15.2），见 §8 |

**不变的事实**（不随接入方式改变）：

- 提交**不做幂等**（D-37）：`submit` / `testplane_submit_run` 重复调用 = 多个 Run；
- 分页是**不透明游标**：CLI 的列表类命令只取服务端默认一页（`limit=50`），完整翻页需要 Agent 直接用 REST；
- 错误信封是**扁平** `{code,message,detail,retryable,trace_id}`（D-39），CLI/MCP 只投影不重编码；
- 响应**允许新增字段**：CLI 用结构体挑需要的字段，MCP 把整个 JSON 原样透传，都不会因未知字段失败。

## 4 CLI `tpctl`

### 4.1 全局选项

写在**命令之前**（与 `docker` / `kubectl` 一致）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `--base-url <url>` | `$TESTPLANE_BASE_URL` 或 `http://127.0.0.1:8080` | 只接受 http/https；**不要**带 `/api/v1` 后缀 |
| `--token <token>` | `$TESTPLANE_TOKEN` 或 `dev` | 出站 `Authorization: Bearer`；dev 模式下值即 subject |
| `--timeout <dur>` | `30s` | 单次 HTTP 超时；纯数字按秒（`5` / `0.5`），也可写 `30s` / `500ms` |
| `--json` | `false` | 机器可读输出；也可写在子命令之后 |
| `--stdio` | `false` | 等价于 `tpctl mcp` |

> `wait` 的 `--timeout 3600` 是**总等待时长**，与全局 HTTP 超时同名不同义；因为全局选项只解析到子命令为止，二者不会互相覆盖。

### 4.2 命令表

| 命令 | 用途 | 成功输出（`--json`） | 示例 |
|---|---|---|---|
| `submit -f <manifest.yaml> [--dry-run]` | 提交 Run / 只校验 | `{run_id,status,kind,submitted_at,tasks[]}`；dry-run 透传 `{valid,issues[],resolved_preview?}` | `tpctl submit -f run.yaml --json` |
| `status <run_id>` | Run 摘要 | `{run_id,name,kind,status,terminal,task_counts,error,tasks[]}` | `tpctl status r-20261105-0001 --json` |
| `wait <run_id> [--timeout 3600] [--interval 3]` | 轮询到终态 | `{...status 字段,outcome,elapsed_seconds}` | `tpctl wait r-20261105-0001 --timeout 600 --json` |
| `report <run_id> [--md]` | 报告 JSON / Markdown | JSON 原文（`--json` 单行；默认缩进） | `tpctl report r-20261105-0001 --md` |
| `logs <task_id> [--attempt 1] [--offset N] [--tail N] [--limit N] [--stream stdout\|stderr]` | Attempt 日志 | `{task_id,attempt,stream,offset,next_offset,coverage,text}` | `tpctl logs t-0002 --attempt 1 --tail 200` |
| `cancel <run_id>` / `cancel --task <task_id>` | 取消 + 回读确认 | `{target,id,accepted,http_status,status,confirmed}` | `tpctl cancel r-20261105-0001 --json` |
| `testers [--role test\|pre_check] [--orphaned]` | Tester 目录 | `GET /testers` 原样 | `tpctl testers --role pre_check` |
| `queue` | 槽位 / 等待原因 / Volcano | `GET /queue-status` 原样 | `tpctl queue --json` |
| `profiles` | Execution Profile | `GET /profiles` 原样（当前 404，见 §8） | `tpctl profiles --json` |
| `mcp` | stdio 上启动 MCP server | 协议流（见 §5） | `tpctl mcp` |
| `help` | 用法 | 帮助文本 | `tpctl help` |

实现末端的两个细节：

- `submit -f -` 从 stdin 读 Manifest（Agent 生成内容后直接管道提交）；
- `logs` 无论是否 `--json`，都会把 `X-Log-Offset`（以及非空的 `X-Log-Coverage`）打到 **stderr**；`--json` 时同一个值也在 `next_offset` 里。

### 4.3 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功。`wait`：Run `succeeded`；`status`/`report`/`testers` 等：读取成功（**不**表示 Run 成功） |
| `1` | 命令失败（HTTP 4xx/5xx、本地参数之外的处理失败）；`wait`：`failed` / `completed_with_errors` / `service_unavailable`；`submit --dry-run`：`valid=false` |
| `2` | `wait`：Run `canceled` |
| `3` | `wait`：总超时仍未终态 |
| `4` | 连不上控制面（DNS / 连接拒绝 / 传输超时） |
| `64` | 用法错误（未知命令、缺参数、非法枚举、读不到 Manifest 文件） |

对应关系：`outcome` 的 `success|failed|canceled|timeout` ↔ `0|1|2|3`；传输失败不产生 `outcome`，直接 `4`。

**只有 `wait` 把 Run 结局映射成退出码**。`status` 是查询命令：能读到 Run 就返回 `0`，Agent 应该看 `status` / `terminal` / `error` 字段判断结局——这样「查询失败」和「Run 失败」不会混在同一个退出码里。

### 4.4 输出约定

| 流 | 内容 |
|---|---|
| stdout | 成功结果：`--json` 时**恰好一个 JSON 文档**；否则给人读的摘要（或 `report --md` 的 Markdown 原文、`logs` 的日志原文） |
| stderr | 进度（`wait` 的状态变化）、错误原因、`X-Log-Offset` / `X-Log-Coverage`、`report` 未生成的重试提示 |

- `--json` 失败时 stdout 为空，stderr 是一个 `{"error":{http_status,code,message,retryable,trace_id,retry_after?}}` 文档（便于 Agent 解析）；
- 非 `--json` 失败时 stderr 是一行 `tpctl: HTTP 404 NOT_FOUND: ... trace_id=...`；
- `wait` 的状态变化只在**变化时**打一行，不刷屏。

### 4.5 Agent 视角的一次闭环

```bash
# 1) 提交（stdout 只给 machine-readable 结果）
run_id=$(tpctl submit -f run.yaml --json | jq -r .run_id)

# 2) 等待；退出码直接告诉 Agent 结局（0/1/2/3/4）
tpctl wait "$run_id" --timeout 3600 --json
case $? in
  0) tpctl report "$run_id" --json ;;
  1) tpctl report "$run_id" --md ;;
  2) echo "已取消" ;;
  3) echo "仍在跑，稍后再 wait" ;;
  4) echo "控制面不可达" ;;
esac

# 3) 续读日志：stdout 是原文，stderr 是下一次的 offset
tpctl logs t-0002 --attempt 1 2>offset.txt
```

### 4.6 实现约束

- 只用标准库 `flag`；子命令 flag 与位置参数**可交错**（`wait r-1 --timeout 10`）；
- Manifest 按首字符判断 `{`/`[` → `application/json`，否则 `application/yaml`；不解析内容；
- 单次应答读取上限 32 MiB（`report.json` 契约上限 20 MiB）；
- HTTP 客户端不复用服务端中间件：不做重试、不做连接池调参、不读环境代理配置以外的全局状态。

## 5 MCP Server

### 5.1 传输与生命周期

- **传输**：stdio，**一行一条** JSON-RPC 2.0 消息（UTF-8，`\n` 分隔），没有 `Content-Length` 帧；
- **方法**：`initialize`、`ping`、`tools/list`、`tools/call`；其余方法返回 `-32601`；
- **无会话状态**：`initialize` 只做协议版本与能力协商，不建立会话、不要求先 initialize 才能 `tools/list`；`notifications/initialized` 等通知**不应答**；
- **退出**：stdin EOF 或写失败即返回，进程退出码 `0`（协议错误不杀进程）；
- **单飞**：顺序处理请求（一次一条），不并发写 stdout。

### 5.2 `initialize` 应答

```json
{
  "protocolVersion": "2025-06-18",
  "capabilities": { "tools": {} },
  "serverInfo": { "name": "testplane", "version": "0.1.0" }
}
```

- 客户端请求的 `protocolVersion` 在 `2024-11-05` / `2025-03-26` / `2025-06-18` 内则回显，否则回本实现支持的最新版本；
- `serverInfo.version` 是 **agent 适配层版本**，不是控制面版本；控制面版本读 `GET /api/v1/meta`。

### 5.3 工具清单

| 工具 | 对应 REST | 备注 |
|---|---|---|
| `testplane_submit_run` | `POST /runs`（`?dry_run=true`） | `manifest` 是 YAML/JSON 原文；不做幂等 |
| `testplane_get_run` | `GET /runs/{run_id}` | 不等待，原样返回 |
| `testplane_wait_run` | 轮询 `GET /runs/{run_id}` | 返回 `outcome`；`timeout` 不是错误 |
| `testplane_cancel_run` | `POST /runs/{run_id}/cancel` | 202/200 后回读确认 |
| `testplane_list_testers` | `GET /testers` | `role` / `orphaned` |
| `testplane_get_report` | `GET /runs/{run_id}/report[.md]` | `format=json\|markdown` |
| `testplane_get_logs` | `GET /tasks/{id}/attempts/{idx}/logs` | 返回 `text` + `next_offset` |
| `testplane_queue_status` | `GET /queue-status` | 三视图不可相加 |
| `testplane_list_profiles` | `GET /profiles` | 端点未落地时返回 `isError`（§8） |

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
  "timeout_seconds":{"type":"number","default":3600,"minimum":1,"maximum":86400},
  "interval_seconds":{"type":"number","default":3,"minimum":0.01,"maximum":60}},
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

// testplane_queue_status / testplane_list_profiles
{"type":"object","properties":{},"additionalProperties":false}
```

### 5.5 `tools/call` 结果

成功（所有 JSON 类工具）：

```json
{"content":[{"type":"text","text":"{\"id\":\"r-20261105-0001\",\"status\":\"testing\"}"}]}
```

成功时省略 `isError`（MCP 默认 `false`）；失败时才写 `"isError": true`。

- `text` 是**一个 JSON 文档的字符串**；`testplane_get_report(format=markdown)` 例外——`text` 是 Markdown 原文，包进 JSON 只会双重转义、对 Agent 更差；
- `testplane_wait_run` 返回 `{run_id,status,terminal,outcome,task_counts,error,elapsed_seconds}`；
- `testplane_get_logs` 返回 `{task_id,attempt,stream,offset,tail?,next_offset,coverage,text}`，`text` 是日志原文。

执行失败（控制面返回 4xx/5xx）：

```json
{"content":[{"type":"text","text":"{\"error\":{\"http_status\":404,\"code\":\"NOT_FOUND\",\"message\":\"Run 不存在\",\"retryable\":false,\"trace_id\":\"01J...\"}}"}]}
```

`isError=true`，但 JSON-RPC 层仍是**成功应答**——MCP 规范把「工具执行失败」与「协议错误」分开，Agent 需要能拿到结构化原因继续决策。

连不上控制面：`isError=true`，`code=UNREACHABLE`、`retryable=true`。

### 5.6 协议错误码

| 场景 | code |
|---|---|
| 坏 JSON / 读不出消息 | `-32700` |
| `jsonrpc` 不是 `"2.0"`、缺 `method` | `-32600` |
| 未知方法（含未实现的 `resources/*`、`prompts/*`） | `-32601` |
| 未知工具、缺必填参数、参数类型/枚举错误、未知字段 | `-32602` |
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

## 6 错误映射

| 控制面 | CLI | MCP |
|---|---|---|
| 2xx | `0`（`wait` 再按终态细分） | `isError=false` + `content.text` |
| `400/404/409/413/429/500/5xx` | `1`，stderr 一行或 `{"error":...}` | `isError=true` + `{"error":{http_status,code,message,detail?,retryable,trace_id?,retry_after?}}` |
| 传输失败（DNS/连接拒绝/超时） | `4` | `isError=true`，`code=UNREACHABLE` |
| `report` 404（未生成） | `1`，stderr 提示「run.finished 后异步生成」+ `Retry-After` | `isError=true`，错误对象带 `retry_after` |
| `profiles` 404（端点未实现） | `1`，`ENDPOINT_NOT_IMPLEMENTED` + 指向 §15.2 | `isError=true`，`code=ENDPOINT_NOT_IMPLEMENTED` |
| 用法错误 | `64` | `-32602`（`tools/call` 的参数层） |

`trace_id` 与响应头 `X-Request-Id` 同源，原样透传给 Agent；CLI/MCP 都不吞掉它——这是 Agent 向人报障时唯一能把「它看到的现象」和「服务端日志」对上的字段。

## 7 测试与验收

| 包 | 测试文件 | 覆盖 |
|---|---|---|
| `server/internal/mcp` | `server_test.go` | `httptest` 假控制面 + `io.Pipe` 驱动 stdio：`initialize`（版本回显/回退）、`tools/list`（9 个工具 + schema）、`tools/call` 成功（get/submit dry-run/wait/logs/report.markdown/cancel）与失败（404 → `isError`）、`profiles` 未实现、未知方法 `-32601`、未知工具/缺参 `-32602`、坏 JSON `-32700`、通知不应答、传输失败 `UNREACHABLE` |
| `server/cmd/tpctl` | `main_test.go` | `httptest` 假控制面：submit（YAML/JSON/stdin/dry-run）、status、wait 的 5 种结局与退出码 0/1/2/3/4、report（md/pretty/compact）、logs（`X-Log-Offset` 进 stderr）、cancel（run/task）、testers/queue/profiles、全局选项位置、用法错误 64 |

```bash
cd server
go test ./internal/mcp/ ./cmd/tpctl/   # 本特性，不需要数据库/K8s
go build ./... && go vet ./... && go test ./...  # 全仓门禁（集成测试按 TESTPLANE_TEST_DATABASE_URL 跳过）
```

验收对应：[`IMPLEMENTATION.md`](../IMPLEMENTATION.md) §13「MCP Server / CLI `tpctl`」；不改变 `openapi.yaml`，因此不影响契约测试的 44/44。

## 8 已知限制（本轮）

1. **`GET /api/v1/profiles` 未实现**（IMPLEMENTATION §15.2「P1 首批」）：`tpctl profiles` / `testplane_list_profiles` 已按契约接好，但当前会明确返回 404/`ENDPOINT_NOT_IMPLEMENTED`，**不伪造数据**；端点落地后两个入口无需改动。
2. **无 SSE 订阅**：`wait` 是轮询（默认 3s），不建立 `GET /runs/{id}/events` 长连接；事件流留给二阶能力（§9）。
3. **列表类命令不分页**：`testers` 只取默认一页（`limit=50`），不暴露 `cursor`；需要翻页时直接用 REST。
4. **`wait` 不重试控制面错误**：轮询中遇到 4xx/5xx 立即失败（传输失败 → `4`）。这符合「错误要显式」的取舍，但服务端短暂 5xx 会让一次 wait 提前结束。
5. **`logs --json` 把日志原文放进 JSON `text` 字段**（转义会变长）；大日志建议用非 `--json` 模式。
6. **MCP 只做 tools**：没有 resources / prompts / sampling / elicitation，也不发 `notifications/progress`（单次 `tools/call` 可能阻塞到 wait 超时）。
7. **不处理 OIDC/CSRF**：token 由外部注入（`$TESTPLANE_TOKEN`）；dev 模式默认 `dev`。生产接 OIDC 后再补「如何拿 token」，但**不做凭据代理**。
8. **不复用 `internal/api` 的 DTO**：字段是本地最小投影，响应新增字段不会破坏解析，但也不会自动出现在 CLI 输出里。

## 9 P1 下一步（Agent 亲和的二阶能力，按 ROI）

| 优先级 | 能力 | 具体动作 |
|---|---|---|
| 1 | 结构化错误 + 下一步提示 | 在 CLI/MCP 的错误对象里补稳定 `hint` / `next_action`（例如 409 `CONFLICT` → 「已在执行，先 wait」；429 → 「带退避重试」）；错误码到动作的映射放一份表，CLI 与 MCP 共用 |
| 2 | SSE / 长任务进度 | `tpctl wait --follow` 走 `/events`，MCP 用 `notifications/progress` 推进度；断线按 `Last-Event-ID` 回放（07 §7）——避免 Agent 轮询 |
| 3 | 幂等提示（不引入幂等键） | D-37 不变：提交前用 `GET /runs?submitter=...` 查重、提交后返回 `submitted_by`；CLI 增加 `--if-none-similar` 之类的**提示性**检查，绝不静默去重 |
| 4 | 批处理 / Matrix | `tpctl submit --matrix matrix.yaml` 在客户端展开为多次 `POST /runs`，MCP 提供 `testplane_submit_batch`，返回 `{run_ids[]}`；展开规则与 `IMPLEMENTATION §13` 的 Matrix 一致 |
| 5 | 报告 / 产物完整 | 补 `GET /runs/{id}/artifacts`、`/files` 与 `result` 的 CLI/MCP 入口（P0 延后项，§15.2），让 Agent 能取原始产物 |
| 6 | 目录能力补全 | `GET /profiles`、`GET /testers/{name}/versions/{v}` 落地后，同步补 `profiles --json`、`testers --version` |
| 7 | MCP 元数据 | 给 9 个工具加 `annotations`（`readOnlyHint` / `destructiveHint` / `idempotentHint`）与 `resources`（报告/日志只读资源），让客户端能按危险级别编排 |
| 8 | 契约门禁 | 加一个「每个 CLI 子命令 / MCP 工具都映射到 openapi operationId」的单测（表驱动），防止后续有人只改 REST 不改 adapter |

## 10 文件清单

```text
server/cmd/tpctl/main.go         入口：全局选项、命令分发、退出码、usage
server/cmd/tpctl/client.go       最小 REST 客户端（Bearer/超时/错误信封）
server/cmd/tpctl/commands.go     submit/status/wait/report/logs/cancel/testers/queue/profiles
server/cmd/tpctl/main_test.go    httptest 假控制面 + 退出码/输出断言
server/internal/mcp/client.go    最小 REST 客户端（MCP 专用，与 CLI 各自独立）
server/internal/mcp/server.go    stdio 帧、JSON-RPC 2.0、initialize/tools/list/tools/call
server/internal/mcp/tools.go     9 个工具的 description + inputSchema + 转发实现
server/internal/mcp/server_test.go  io.Pipe 驱动 stdio 的协议面测试
```
