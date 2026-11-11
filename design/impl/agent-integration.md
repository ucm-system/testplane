# Agent 接入实现（agent-integration）

> **范围：** P1 的 Agent 亲和入口：`argus` CLI 与 MCP Server（stdio / JSON-RPC 2.0），以及与 REST 的关系、`wait --follow` 的 SSE 策略、`next_action` 错误提示、退出码、测试方式。
> **不在范围：** REST 契约本身（[`views/07-api.md`](../views/07-api.md)）、状态机与编排（[`control-plane.md`](control-plane.md)）、执行层（[`execution.md`](execution.md)）、Web UI。
> **基线：** [`server/api/openapi.yaml`](../../server/api/openapi.yaml)（**v2 契约**：路径全面 `/tasks` / `/runners` 化，D-73）+ [`IMPLEMENTATION.md`](../IMPLEMENTATION.md) §13。
> **代码：** [`server/cmd/argus`](../../server/cmd/argus)、[`server/internal/mcp`](../../server/internal/mcp)。
> **定位：** **薄 adapter**。CLI 与 MCP 不引入新语义，只是 REST 的两种调用形态；控制面仍是唯一权威。

## 1 为什么做

Argus 的定位是 **AI Agent 友好**：Agent 要在**不写代码**的前提下完成「提交 → 等待 → 取报告 → 看产物/日志 → 取消/重试」闭环，并且能可靠判断结局、知道失败后该做什么。REST 对 Agent 有四处摩擦：

| 摩擦 | 现象 | 本轮解法 |
|---|---|---|
| 工具调用 | Agent 运行时通常只会「调工具」，不会拼 HTTP、管 headers、解 SSE | MCP server 把 11 个高频动作注册成工具 |
| 结局判定 | HTTP 200 只表示「查到了」，不表示 Task 成功；Agent 需要解析状态枚举 | CLI 把 Task 结局映射成退出码；MCP 返回 `outcome` 字段 |
| 等待成本 | 轮询有延迟，长 Task（部署模式可达数小时）浪费调用次数 | `wait --follow` 用 SSE（`Last-Event-ID` 续传、断线重连），不可用时自动降级轮询 |
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
| 把 Task 终态映射成退出码 / `outcome`（`retry` / `clean` 不做结局映射） | 替 Agent 决定「这次测试算不算过」 |
| 读 `-f` 指定的 Manifest（也支持 stdin）；`files --out` 显式落盘 | 改 Manifest、做 Matrix 展开、默认写任何文件 |
| 用 `--token` / `$AG_TOKEN` 出站鉴权 | 代理/缓存凭据、把 token 写进输出或日志 |
| 用 SSE 等服务端事件（`wait --follow`） | 自建状态、缓存事件、把日志塞进 SSE |
| 依赖 Go 标准库（`flag` / `net/http` / `encoding/json`） | 引入 cobra、MCP SDK 或第二套 HTTP 客户端语义 |

**为什么不引入 MCP SDK / CLI 框架**：协议面只有 4 个方法（`initialize` / `tools/list` / `tools/call` / `ping`）和子命令表（§4.2）；引入框架只会把「命令 ↔ 端点」的对应关系藏进 DSL，反而妨碍 Agent 读源码。SSE 解析器约 70 行、等待循环约 200 行，测试可以直接用 `httptest` + `io.Pipe` 驱动。

**与 `internal/api` 的关系**：CLI 与 MCP **都不 import** `server/internal/api`（那是服务端 handler/DTO 包）。两者各有一份约 120 行的 HTTP 客户端，只做「拼 URL + Bearer + 超时 + 错误信封解析 + SSE 打开」。这与 §15.3 #7「MVP 手写 handler/DTO、未采用 codegen」保持一致。

**共享的等待内核**：`server/internal/mcp/sse.go` 同时被 MCP 工具与 CLI 使用（`mcp.Wait`）。CLI 的 client 与 MCP 的 client 各实现一个 3 方法窄接口 `TaskReader`（`GetTask` / `OpenTaskEvents`），因此等待/降级/重连逻辑只有一份实现、一份测试。

## 3 与 REST 的关系

> v2 起只有一种顶层对象（Task）与一种执行单位（runner）；`/runs` 系列端点全部并入 `/tasks`（D-73）。

| CLI 命令 | MCP 工具 | REST | 说明 |
|---|---|---|---|
| `submit -f <file>` | `argus_submit_task` | `POST /api/v1/tasks`（`?dry_run=true`） | Manifest（顶层 `task:`）原样转发；按内容首字符选 YAML/JSON `Content-Type` |
| `status <task_id>` | `argus_get_task` | `GET /api/v1/tasks/{id}` | 派生 `terminal`；失败终态带 `next_action` |
| `wait <task_id> [--follow]` | `argus_wait_task`（`follow`） | 轮询 `GET /tasks/{id}`；`follow` 时 `GET /tasks/{id}/events`（SSE） | SSE 不可用自动降级轮询；不新增等待端点 |
| `cancel <task_id>` | `argus_cancel_task` | `POST /tasks/{id}/cancel` + 回读 `GET` | 202/200 后确认一次；取消会先执行 service-clean 再落 `canceled`（§7.6） |
| `retry <task_id>` | `argus_retry_task` | `POST /tasks/{id}/retry` + 回读 `GET` | **整 Task 新 attempt 全量重跑含 fresh deploy**（§7.8）；超限 409 `ATTEMPT_EXHAUSTED`；输出提示"将重新部署" |
| `clean --task <task_id> [--attempt N]` | `argus_clean_task` | `POST /tasks/{id}/clean?attempt=N` | **泄漏重放**（§9.3）：用落档导出重拉一次 service-clean Job；幂等 |
| `report <task_id>` | `argus_get_report` | `GET /tasks/{id}/report` | JSON 透传；`--json` 单行、默认缩进 |
| `report <task_id> --md` | `argus_get_report`（`format=markdown`） | `GET /tasks/{id}/report.md` | 原样打印 Markdown，不做渲染 |
| `logs <task_id> --runner-seq N` | `argus_get_logs` | `GET /tasks/{id}/attempts/{n}/runners/{seq}/logs` | 透传 `offset/limit/tail/stream`；`X-Log-Offset` → `next_offset`；`--runner-seq` 缺省取管道第一个失败 runner |
| `artifacts <task_id>` | `argus_list_artifacts`（`task_id`） | `GET /tasks/{id}/artifacts` | 按 `kind` 分组；`path` 可直接给 `files` |
| `artifacts <task_id> --attempt N --runner-seq S` | `argus_list_artifacts`（runner 级） | `GET /tasks/{id}/attempts/{n}/runners/{seq}/artifacts` | 与 Task 级同信封 |
| `files <task_id> <path>` | `argus_get_file`（摘要） | `GET /tasks/{id}/files?path=` | `--range` 透传 `Range`；`--out` 落盘并打印 sha256 |
| `runners` | `argus_list_runners` | `GET /api/v1/runners` | `role` / `orphaned` 过滤 |
| `queue` | `argus_queue_status` | `GET /api/v1/queue-status` | 三个视图**不可相加**（07 §5.3、D-49） |
| `profiles` | `argus_list_profiles` | `GET /api/v1/profiles` | P1 已实现，返回 Profile + `mounts` |

**不变的事实**（不随接入方式改变）：

- 提交**不做幂等**（D-37）：`submit` / `argus_submit_task` 重复调用 = 多个 Task；
- **重试语义唯一**（§7.8 / D-69）：`retry` = 新 attempt 全量重跑（含重新部署），不存在 runner 级或部分复用重试；
- 分页是**不透明游标**：CLI 的列表类命令只取服务端默认一页（`limit=50`），完整翻页需要 Agent 直接用 REST；
- 错误信封是**扁平** `{code,message,detail,retryable,trace_id}`（D-39），CLI/MCP 只投影不重编码；
- 响应**允许新增字段**：CLI 用结构体挑需要的字段，MCP 把整个 JSON 原样透传，都不会因未知字段失败；
- 日志**不走 SSE**（C-9.2）：`wait --follow` 只消费状态事件，日志仍走 `logs` 端点。

## 4 CLI `argus`

### 4.1 全局选项

写在**命令之前**（与 `docker` / `kubectl` 一致）：

| 选项 | 默认 | 说明 |
|---|---|---|
| `--base-url <url>` | `$AG_BASE_URL` 或 `http://127.0.0.1:8080` | 只接受 http/https；**不要**带 `/api/v1` 后缀 |
| `--token <token>` | `$AG_TOKEN` 或 `dev` | 出站 `Authorization: Bearer`；dev 模式下值即 subject |
| `--timeout <dur>` | `30s` | 单次 HTTP 超时；纯数字按秒（`5` / `0.5`），也可写 `30s` / `500ms` |
| `--json` | `false` | 机器可读输出；也可写在子命令之后（两种位置等价） |
| `--stdio` | `false` | 等价于 `argus mcp` |

> `wait` 的 `--timeout 3600` 是**总等待时长**；因为全局选项只解析到子命令为止，二者不会互相覆盖。

### 4.2 命令表

| 命令 | 用途 | 成功输出（`--json`） | 示例 |
|---|---|---|---|
| `submit -f <manifest.yaml> [--dry-run]` | 提交 Task / 只校验 | `{task_id,status,submitted_at,attempt_no,runners[]}`；dry-run 透传 `{valid,issues[],resolved_preview?}` | `argus submit -f task.yaml --json` |
| `status <task_id>` | Task 摘要 | `{task_id,name,status,terminal,attempt_no,error,runners[],next_action?,hint?}` | `argus status t-20261110-0001 --json` |
| `wait <task_id> [--follow] [--timeout 3600] [--interval 3]` | 等待终态 | `{...status 字段,outcome,elapsed_seconds,wait_mode,fallback_reason?,timed_out?}` | `argus wait t-20261110-0001 --follow --json` |
| `retry <task_id>` | **整 Task 重试**（新 attempt 全量重跑含 fresh deploy） | `{task_id,accepted,attempt_no,status}`；stderr 提示"将重新部署" | `argus retry t-20261110-0001 --json` |
| `clean --task <task_id> [--attempt N]` | 泄漏重放：重拉一次 service-clean Job（§9.3） | `{task_id,attempt_no,accepted,status}` | `argus clean --task t-20261110-0001 --json` |
| `report <task_id> [--md]` | 报告 JSON / Markdown | JSON 原文（`--json` 单行；默认缩进） | `argus report t-20261110-0001 --md` |
| `logs <task_id> [--attempt 1] [--runner-seq N] [--offset N] [--tail N] [--limit N] [--stream stdout\|stderr]` | runner 日志（`--runner-seq` 缺省 = 管道第一个失败 runner） | `{task_id,attempt_no,runner_seq,stream,offset,next_offset,coverage,text}` | `argus logs t-0002 --attempt 1 --runner-seq 3 --tail 200` |
| `artifacts <task_id>` / `artifacts <task_id> [--attempt 1] [--runner-seq N]` | 产物清单 | `GET .../artifacts` 原样 | `argus artifacts t-20261110-0001 --json` |
| `files <task_id> <path> [--out <file>] [--range <range>]` | 下载产物 | `{task_id,path,api_path,http_status,content_type,size_bytes,total_size_bytes,sha256,sha256_scope,out?,range?,content_range?}` | `argus files t-1 attempts/1/runners/3/result.json --out r.json` |
| `cancel <task_id>` | 取消 + 回读确认（先 clean 后终态） | `{target,id,accepted,http_status,status,confirmed}` | `argus cancel t-20261110-0001 --json` |
| `runners [--role test\|pre_check] [--orphaned]` | Runner 目录 | `GET /runners` 原样 | `argus runners --role pre_check` |
| `queue` | 槽位 / 等待原因 / Volcano | `GET /queue-status` 原样 | `argus queue --json` |
| `profiles` | Execution Profile | `GET /profiles` 原样 | `argus profiles --json` |
| `mcp` | stdio 上启动 MCP server | 协议流（见 §5） | `argus mcp` |
| `help` | 用法 | 帮助文本 | `argus help` |

细节：

- `submit -f -` 从 stdin 读 Manifest（Agent 生成内容后直接管道提交）；`retry` / `clean` 成功后建议紧跟 `wait`（重试会重新走完整管道，含部署）；
- `logs` 无论是否 `--json`，都把 `X-Log-Offset`（以及非空的 `X-Log-Coverage`）打到 **stderr**；`--json` 时同一个值也在 `next_offset` 里；
- `artifacts` 无 `--json` 时按 `result → raw → log → report → other` 分组，每组打印条数、总大小；`--json` 原样输出控制面 JSON；
- `files` 的语义见 §4.3；`--json` 必须配合 `--out`（不把二进制写进 JSON stdout）；
- `--json` 无论写在命令前还是命令后，错误输出也统一为 `{"error":{...}}` 到 stderr。

### 4.3 artifacts / files

**`artifacts`**（只读）：

```bash
argus artifacts t-20261110-0001                          # Task 级，按 kind 分组
argus artifacts t-20261110-0001 --attempt 1 --runner-seq 3   # runner 执行记录级
argus artifacts t-20261110-0001 --json                   # 原样 JSON（items + by_kind + total_size_bytes）
```

**`files`**（下载）：

```bash
# 1) 落盘 + 打印 sha256（默认下载全文，≤1 GiB；超过服务端 413，需 --range）
argus files t-20261110-0001 attempts/1/runners/3/result.json --out result.json

# 2) 只取前 1KiB（Range 透传；--range 可写 0-1023 或 bytes=0-1023）
argus files t-20261110-0001 attempts/1/runners/3/logs/stdout.log --range 0-1023 --out head.txt

# 3) 不落盘：body 原样写 stdout，摘要写 stderr（可管道给 jq / 文件）
argus files t-20261110-0001 report.json | jq .
```

- `sha256` 是**本次收到的字节**的哈希；`sha256_scope` 为 `full`（status 200 或整文件）或 `partial`（206 且未取全），不要把它当整文件指纹；
- `path` 直接接受 `artifacts` 返回的写法（`<task_id>/attempts/...`）或 Task 目录内相对路径（`attempts/...`，相对 `<storage-root>/<task_id>/`）；CLI 自动剥离 `<task_id>/` 前缀，JSON 里的 `api_path` 是实际发给 `/files?path=` 的值；
- `--out` 会自动创建父目录（`MkdirAll`）；`--out -` 表示 stdout；
- `--json` 只输出元数据，**必须**配 `--out`，否则 64 用法错误——避免把二进制混进 JSON。

### 4.4 退出码

| 码 | 含义 |
|---|---|
| `0` | 成功。`wait`：Task `succeeded`；`status`/`report`/`artifacts` 等：读取成功（**不**表示 Task 成功） |
| `1` | 命令失败（HTTP 4xx/5xx、本地处理失败）；`wait`：`failed` / `service_unavailable`（v1 的 `completed_with_errors` 已并入 `failed`，D-73）；`submit --dry-run`：`valid=false` |
| `2` | `wait`：Task `canceled` |
| `3` | `wait`：总超时仍未终态 |
| `4` | 连不上控制面（DNS / 连接拒绝 / 传输超时） |
| `64` | 用法错误（未知命令、缺参数、非法枚举、读不到 Manifest、`files --json` 缺 `--out`、`clean` 未给 `--task` 等） |

对应关系：`outcome` 的 `success|failed|canceled|timeout` ↔ `0|1|2|3`；传输失败不产生 `outcome`，直接 `4`。

**只有 `wait` 把 Task 结局映射成退出码**。`status` 是查询命令：能读到 Task 就返回 `0`，Agent 应看 `status` / `terminal` / `error` / `next_action` 判断结局。`retry` / `clean` 是动作命令：202/200 返回 `0`，超限 `ATTEMPT_EXHAUSTED` 返回 `1`（错误信封带 `next_action`）。

### 4.5 `wait --follow`：SSE 优先，自动降级

```bash
argus wait t-20261110-0001 --follow --timeout 3600 --json
```

流程（`server/internal/mcp/sse.go` 的 `Wait`）：

1. 先 `GET /tasks/{id}` 拿当前状态；已终态直接输出并退出（不订阅 SSE）；
2. 未终态且 `--follow`：`GET /tasks/{id}/events`（`Accept: text/event-stream`）；
   - 按 `id:` 记 `Last-Event-ID`，重连时带上，服务端从 `seq+1` 回放（07 §7）；
   - `task.state_changed` 更新状态；`task.finished` 触发一次 `GET /tasks/{id}` 拿权威终态后退出（`runner.state_changed` 不打印逐条明细，§8 限制 4）；
   - `: ping` 心跳直接忽略；
3. **断线重连 3 次**，指数退避 1s → 2s → 4s（受总 timeout 约束）；
4. **SSE 不可用**时自动降级为轮询：
   - 404 / 405 / 5xx、连接失败、`Content-Type` 不是 `text/event-stream`、或重连 3 次仍失败；
   - CLI 打一行 stderr：`argus: SSE 不可用（原因），已降级为轮询`；JSON 里 `wait_mode=poll`、`fallback_reason=原因`；
   - **401/403 不降级**（轮询也会失败，直接按退出码 1 报错）；
5. 总超时退出码 3，输出最后一次已知状态；`timed_out=true`；
6. 不订阅 SSE 时（默认 `wait`）仍是最简单的轮询，行为与上一版一致。

> **回放噪声**：服务端会从 `seq=1` 回放历史事件。`Wait` 会跳过「初始 GET 状态之前」的历史转移，避免终端出现倒序噪声；`task.finished` 与窗口起点之后的事件正常展示。

> **为什么降级**：Agent 不应该因为服务端没开 SSE / 网关缓冲 / 老版本控制面就失败；`wait --follow` 是**优化路径**，不是硬依赖。

### 4.6 输出约定

| 流 | 内容 |
|---|---|
| stdout | 成功结果：`--json` 时**恰好一个 JSON 文档**；否则给人读的摘要（或 `report --md` 的 Markdown 原文、`logs`/`files` 的原文） |
| stderr | 进度（`wait` 的状态变化）、SSE 降级说明、`next_action`、错误原因、`X-Log-Offset`、下载摘要、`report` 未生成的重试提示 |

- `--json` 失败时 stdout 为空，stderr 是 `{"error":{http_status,code,message,retryable,trace_id,retry_after?,next_action?,hint?}}`；
- 非 `--json` 失败时 stderr 是 `argus: HTTP 404 NOT_FOUND: ... trace_id=...`，已知错误码再加一行 `argus: 下一步: ...`；
- `wait` 的状态变化只在**变化时**打一行，不刷屏。

### 4.7 Agent 推荐序列

```bash
# 1) 提交（stdout 只给 machine-readable 结果）
task_id=$(argus submit -f task.yaml --json | jq -r .task_id)

# 2) 等终态：SSE 优先；退码直接告诉 Agent 结局（0/1/2/3/4）
argus wait "$task_id" --follow --timeout 3600 --json
case $? in
  0) argus report "$task_id" --md ;;
  1) argus report "$task_id" --md; argus artifacts "$task_id" --json ;;
  2) echo "已取消" ;;
  3) echo "仍在跑，稍后再 wait" ;;
  4) echo "控制面不可达" ;;
esac

# 2b) 失败且需要重试：整 Task 新 attempt 全量重跑（部署模式会重新部署，Agent 需二次确认场景由调用方决定）
argus retry "$task_id" --json && argus wait "$task_id" --follow --json

# 3) 产物：先列清单，再按 path 下载
argus artifacts "$task_id" --json | jq -r '.items[] | select(.kind=="raw") | .path' |
  while read -r p; do argus files "$task_id" "$p" --out "out/$(basename "$p")"; done

# 4) 失败时看日志（stdout 原文，stderr 是下一次 offset）
argus logs "$task_id" --attempt 1 --runner-seq 3 --tail 200
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
- **同步 request/response**：`tools/call` 一次调用只产生一次应答；`argus_wait_task(follow=true)` 也是服务端侧等完再返回，**不发 progress notification**（见 §5.5）；
- **退出**：stdin EOF 或写失败即返回，进程退出码 `0`（协议错误不杀进程）。

### 5.2 `initialize` 应答

```json
{
  "protocolVersion": "2025-06-18",
  "capabilities": { "tools": {} },
  "serverInfo": { "name": "argus", "version": "0.1.0" }
}
```

- 客户端请求的 `protocolVersion` 在 `2024-11-05` / `2025-03-26` / `2025-06-18` 内则回显，否则回最新支持版本；
- `serverInfo.version` 是 **agent 适配层版本**，不是控制面版本；控制面版本读 `GET /api/v1/meta`。

### 5.3 工具清单（13 个）

| 工具 | 对应 REST | 备注 |
|---|---|---|
| `argus_submit_task` | `POST /tasks`（`?dry_run=true`） | `manifest` 是 YAML/JSON 原文（顶层 `task:`，D-40/D-66）；不做幂等 |
| `argus_get_task` | `GET /tasks/{task_id}` | 不等待，原样返回 |
| `argus_wait_task` | 轮询或 SSE（`follow`） | 返回 `outcome`、`wait_mode`、`fallback_reason`；`timeout` 不是错误 |
| `argus_cancel_task` | `POST /tasks/{task_id}/cancel` | 202/200 后回读确认；取消先执行 service-clean 再落 `canceled` |
| `argus_retry_task` | `POST /tasks/{task_id}/retry` | **整 Task 新 attempt 全量重跑含 fresh deploy**（§7.8）；`ATTEMPT_EXHAUSTED` 以 `isError=true` 返回 |
| `argus_clean_task` | `POST /tasks/{task_id}/clean`（`?attempt=`） | 泄漏重放（§9.3）；幂等 |
| `argus_list_runners` | `GET /runners` | `role` / `orphaned` |
| `argus_get_report` | `GET /tasks/{task_id}/report[.md]` | `format=json\|markdown` |
| `argus_get_logs` | `GET /tasks/{id}/attempts/{n}/runners/{seq}/logs` | 返回 `text` + `next_offset` |
| `argus_list_artifacts` | `GET /tasks/{id}/artifacts` 或 `GET /tasks/{id}/attempts/{n}/runners/{seq}/artifacts` | Task 级 / runner 级二选一 |
| `argus_get_file` | `GET /tasks/{id}/files?path=` | 返回**摘要**（path/size/sha256/前 2KB），不塞二进制 |
| `argus_queue_status` | `GET /queue-status` | 三视图不可相加 |
| `argus_list_profiles` | `GET /profiles` | 已实现；返回 Profile + `mounts` |

### 5.4 `inputSchema`

所有 Schema 都是 `type=object` + `additionalProperties=false`：拼错的参数会被 `-32602` 拒绝，而不是被静默忽略。下面按工具分列（`//` 后是工具名，不是 JSON 内容；实际应答里没有注释）：

```jsonc
// argus_submit_task
{"type":"object","properties":{
  "manifest":{"type":"string","description":"Task Manifest 原文（YAML 或 JSON 对象，顶层是 task:；deploy XOR service）"},
  "dry_run":{"type":"boolean","description":"true 时只校验、不落库","default":false}},
 "required":["manifest"],"additionalProperties":false}

// argus_get_task
{"type":"object","properties":{
  "task_id":{"type":"string","description":"Task ID（不透明字符串）"}},
 "required":["task_id"],"additionalProperties":false}

// argus_wait_task
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "timeout_seconds":{"type":"number","default":3600,"minimum":0.1,"maximum":86400},
  "interval_seconds":{"type":"number","default":3,"minimum":0.01,"maximum":60},
  "follow":{"type":"boolean","default":false,"description":"true 时优先用 SSE；不可用自动降级轮询"}},
 "required":["task_id"],"additionalProperties":false}

// argus_cancel_task
{"type":"object","properties":{"task_id":{"type":"string"}},
 "required":["task_id"],"additionalProperties":false}

// argus_retry_task（整 Task 新 attempt 全量重跑，部署模式将重新部署）
{"type":"object","properties":{"task_id":{"type":"string"}},
 "required":["task_id"],"additionalProperties":false}

// argus_clean_task（泄漏重放，§9.3）
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "attempt":{"type":"integer","default":0,"minimum":0,"description":"0 = 最后一个 attempt"}},
 "required":["task_id"],"additionalProperties":false}

// argus_list_runners
{"type":"object","properties":{
  "role":{"type":"string","enum":["test","pre_check"]},
  "orphaned":{"type":"boolean","default":false}},
 "additionalProperties":false}

// argus_get_report
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "format":{"type":"string","enum":["json","markdown"]}},
 "required":["task_id"],"additionalProperties":false}

// argus_get_logs
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "attempt":{"type":"integer","default":1,"minimum":1,"maximum":1000},
  "runner_seq":{"type":"integer","minimum":1,"maximum":1000,"description":"缺省取管道第一个失败 runner"},
  "stream":{"type":"string","enum":["stdout","stderr"]},
  "offset":{"type":"integer","minimum":0},
  "limit":{"type":"integer","minimum":1,"maximum":65536},
  "tail":{"type":"integer","minimum":1,"maximum":1000000}},
 "required":["task_id"],"additionalProperties":false}

// argus_list_artifacts（runner 级 = task_id + attempt + runner_seq）
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "attempt":{"type":"integer","default":1,"minimum":1,"maximum":1000},
  "runner_seq":{"type":"integer","minimum":1,"maximum":1000}},
 "required":["task_id"],"additionalProperties":false}

// argus_get_file
{"type":"object","properties":{
  "task_id":{"type":"string"},
  "path":{"type":"string"},
  "range":{"type":"string","description":"可选 HTTP Range；缺省按 max_bytes 取前缀"},
  "max_bytes":{"type":"integer","default":1048576,"minimum":1024,"maximum":33554432}},
 "required":["task_id","path"],"additionalProperties":false}

// argus_queue_status / argus_list_profiles
{"type":"object","properties":{},"additionalProperties":false}
```

### 5.5 `tools/call` 结果

成功（所有 JSON 类工具）：

```json
{"content":[{"type":"text","text":"{\"task_id\":\"t-20261110-0001\",\"status\":\"running\"}"}]}
```

成功时省略 `isError`（MCP 默认 `false`）；失败时才写 `"isError": true`。

- `text` 是**一个 JSON 文档的字符串**；`argus_get_report(format=markdown)` 例外——`text` 是 Markdown 原文；
- `argus_wait_task` 返回 `{task_id,status,terminal,outcome,attempt_no,error,elapsed_seconds,wait_mode,fallback_reason?,timed_out?,next_action?}`；
  - `wait_mode`: `sse` 或 `poll`；`fallback_reason` 只在降级时出现；
  - `outcome`: `success | failed | canceled | timeout`；不是 HTTP 状态；
  - `follow=true` 仍只返回**一次**结果（服务端侧等完），不发 progress notification——保持与未声明 `notifications/progress` 能力的客户端兼容；
- `argus_retry_task` / `argus_clean_task` 返回 `{task_id,accepted,attempt_no,status}`；
- `argus_get_logs` 返回 `{task_id,attempt_no,runner_seq,stream,offset,tail?,next_offset,coverage,text}`；
- `argus_get_file` 返回**摘要**（不返回完整二进制）：

```json
{
  "task_id": "t-1", "path": "t-1/attempts/1/runners/3/logs/stdout.log",
  "api_path": "attempts/1/runners/3/logs/stdout.log",
  "http_status": 206, "content_type": "text/plain; charset=utf-8",
  "size_bytes": 1024, "total_size_bytes": 10485760,
  "sha256": "…", "sha256_scope": "partial",
  "range": "bytes=0-1023", "content_range": "bytes 0-1023/10485760",
  "encoding": "text", "preview": "前 2KB 文本…", "truncated": true
}
```

- `path` 接受 `argus_list_artifacts` 返回的写法（含 `<task_id>/` 前缀）或 Task 目录内相对路径；`api_path` 是实际发给 `/files?path=` 的值；
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
argus --base-url http://127.0.0.1:8080 --token dev mcp

# 方式二：全局开关（等价）
argus --base-url http://127.0.0.1:8080 --token dev --stdio
```

任一种方式下，**stdout 只写协议消息**，任何诊断信息都写 stderr。MCP 客户端配置示例：

```json
{
  "mcpServers": {
    "argus": {
      "command": "argus",
      "args": ["--base-url", "http://127.0.0.1:8080", "--token", "dev", "mcp"]
    }
  }
}
```

### 5.8 SSE 与 MCP 的关系

- MCP 的 `tools/call` 是同步 request/response，**不能流式推**；`argus_wait_task(follow=true)` 的 SSE 是**服务端侧**等待，工具调用仍一次返回；
- 不发送 `notifications/progress`：客户端未声明该能力时发通知会破坏兼容；未来若要发，先看 `initialize` 的 `capabilities.progress` 再决定（§9）；
- SSE 降级原因写进结果字段 `fallback_reason`（不是 stderr，MCP 没有 stderr 语义）。

## 6 错误映射与 `next_action`

### 6.1 HTTP → 入口

| 控制面 | CLI | MCP |
|---|---|---|
| 2xx | `0`（`wait` 再按终态细分） | `isError=false` + `content.text` |
| `400/404/409/413/429/500/5xx` | `1`，stderr 一行或 `{"error":...}` | `isError=true` + `{"error":{...}}` |
| 传输失败（DNS/连接拒绝/超时） | `4` | `isError=true`，`code=UNREACHABLE` |
| `report` 404（未生成） | `1`，stderr 提示「task.finished 后异步生成」+ `Retry-After` | `isError=true`，错误对象带 `retry_after` |
| 用法错误 | `64` | `-32602`（`tools/call` 的参数层） |

`trace_id` 与响应头 `X-Request-Id` 同源，原样透传给 Agent；CLI/MCP 都不吞掉它。

### 6.2 共享 `next_action` 映射

实现在 `server/internal/mcp/hints.go`（CLI 与 MCP 共用同一份）。字段：`{code, retryable, next_action, hint}`；`retryable` 是码级默认，实例级以错误信封的 `retryable` 为准。

| code | 可重试（码级） | next_action | hint |
|---|---|---|---|
| `VALIDATION_ERROR` | 否 | 按 `detail.issues[]` 修正字段后重新提交 | issue 带 `path`（如 `runners[2].input`，jsonl 带 `line`） |
| `UNAUTHENTICATED` | 否 | 重新获取 token（`$AG_TOKEN` / `--token`） | 控制面无匿名端点 |
| `FORBIDDEN` | 否 | 确认角色：viewer 只读、operator 可提交/取消/重试/泄漏重放、admin 可维护 | 403 由服务端统一判定 |
| `NOT_FOUND` | 否 | 核对 ID 与资源是否存在；先 list 再操作 | ID 不透明 |
| `CONFLICT` | 是 | 先 wait 到终态再重试；不要并发改同一 Task | 以先落库状态为准 |
| `ATTEMPT_EXHAUSTED` | 否 | attempt 已达上限（默认 3，§12.7）；换新 Task 或修正输入 | v2 新增（D-73） |
| `RUNNER_NOT_FOUND` | 否 | 用 runners 看目录，修正名称/版本 | 注册表自发现（K-3） |
| `RUNNER_VERSION_PINNED` | 否 | 发布新 version 后改 Manifest 引用 | 同 name+version 不可覆盖 |
| `RUNNER_INVALID` | 否 | 修 `runner.yaml` / entry，等重扫 | 附录 C v2 |
| `EXECUTION_PROFILE_NOT_FOUND` | 否 | 用 profiles 看已登记 Profile，改 `runners[].execution.profile` | 引导期例外已移除 |
| `TASK_TOO_LARGE` | 否 | 拆分 Task（单 Task runner ≤200） | 含注入的 deploy/precheck/clean |
| `RESOURCE_EXHAUSTED` | 是 | 指数退避后重试，或降低提交频率 | 429 |
| `ARTIFACT_QUOTA_EXCEEDED` | 否 | 清理旧 Task 产物后重试 | 平台级配额 |
| `EXECUTOR_UNAVAILABLE` | 是 | 等 30–60s 后重试 wait / 重新提交 | 执行器暂不可用 |
| `EXECUTOR_POD_ERROR` | 否 | 看 `k8s_reason`，调 `resources` 后整 Task retry | OOM/Evicted/NodeLost |
| `IMAGE_PULL_FAILED` | 否 | 核对镜像/registry 凭据后提交 | 看执行记录与事件 |
| `MOUNT_UNAVAILABLE` | 否 | 对照 profile.mounts 补挂载能力 | 不是 Runner 参数问题 |
| `ENDPOINT_UNREACHABLE` | 是 | 等服务就绪后重试；部署模式看 `/deploy-state` 的 phase 与水位 | 先看 pre_check runner 日志 |
| `PRECHECK_FAILED` | 否 | 检查健康检查/端口/模型名；看 pre_check runner 日志 | pre_check 是管道第一个测试类 runner（串行） |
| `RUNNER_TIMEOUT` | 否 | 调大 `runners[].timeout_seconds` / `AG_RUNNER_TIMEOUT` | 先确认没卡外部资源 |
| `RUNNER_NONZERO_EXIT` | 否 | 看 Runner 日志与 result.json 的 error（若有） | 通常是测试失败 |
| `RESULT_MISSING` | 否 | 看日志：runner 退出 0 但三选一产物全缺（D-74） | 应产出 result.json/html/csv 至少其一 |
| `RESULT_INVALID` | 否 | 对照契约修 Runner 的 result.json | 仅当 result.json 产出但不合法（D-74） |
| `EXPORT_INVALID` | 否 | 修 Runner 的 exports.env（每行 `KEY=VALUE`，值不含换行） | v2 新增（D-71） |
| `CLEANUP_FAILED` | 是 | 看 service-clean 日志；可用 `argus clean --task` 重放（§9.3） | 不改变 Task 终态 |
| `CANCELED` | 否 | 重新 submit（不做幂等，D-37） | 重复取消返回 200 |
| `INTERNAL_ERROR` | 是 | 带 trace_id 报平台；可退避重试一次 | 与 `X-Request-Id` 同源 |
| `DEPLOY_FAILED` | 否 | service-deploy 调 Genet 失败，**不要**直接重试同样参数；带 trace_id 找平台；需要重试走 `argus retry`（fresh deploy） | 502 |
| `DEPLOY_BLOCKED` | 是 | 看 `/deploy-state` 的 phase 与水位（或 `/queue-status` volcano 块），等队列前进；需要重试走 `argus retry` | reason 在 deploy-state `failure.detail.reason` |
| `SERVICE_UNAVAILABLE`（终态） | 是 | 等服务就绪后重新 submit；或改用 `service` 块外接 | 部署收口 |
| `UNREACHABLE`（合成） | 是 | 检查 `--base-url` / 控制面 / 网络后重试 | 请求没到服务端 |

CLI 非 `--json` 时打印 `argus: 下一步: <next_action>`；`--json` 时放在 `error.next_action`。MCP 放在 `error.next_action`（另带 `error.hint`）。Task 的**失败终态**也会带提示：`status` / `wait` 的输出里有顶层 `next_action` / `hint`（取 `tasks.error.code`，没有则按 `canceled` / `service_unavailable` 终态）。

## 7 测试与验收

| 包 | 测试文件 | 覆盖 |
|---|---|---|
| `server/internal/mcp` | `server_test.go` | `httptest` 假控制面 + `io.Pipe` 驱动 stdio：`initialize`、`tools/list`（13 工具 + schema）、`tools/call` 成功（get/submit/wait/logs/report/profile/artifacts/get_file/cancel/retry/clean）、失败（404 → `isError` + `next_action`；`ATTEMPT_EXHAUSTED`）、`-32601`/`-32602`/`-32700`、通知不应答、传输失败 `UNREACHABLE`；SSE 解析（注释/多行 data/`id`）；`Wait(follow)` 成功、**404 降级轮询**、**断线重连并携带 `Last-Event-ID`**、**回放噪声抑制**；`hints` 全码与抽检语义；`get_file` 文本/二进制预览、`sha256_scope`、`<task_id>/` 前缀剥离 |
| `server/cmd/argus` | `main_test.go` | `httptest` 假控制面：submit（YAML/JSON/stdin/dry-run）、status、wait（5 种结局 + 退出码 0/1/2/3/4）、`wait --follow`（SSE 成功 + 降级 + stderr 说明）、retry（`ATTEMPT_EXHAUSTED`）、clean（缺 `--task` 用法错误 64）、report（md/pretty/compact）、logs（`X-Log-Offset` 进 stderr；`--runner-seq`）、artifacts（task/runner 级/分组/原样 JSON）、files（`--out`/`--range`/sha256/`--json` 需 `--out`/stdout 原文/前缀剥离）、cancel、runners/queue/profiles、全局选项位置、`next_action`（错误响应与失败终态）、用法错误 64 |

```bash
cd server
go test ./internal/mcp/ ./cmd/argus/   # 本特性，不需要数据库/K8s
go build ./... && go vet ./... && go test ./...  # 全仓门禁（集成测试按 AG_TEST_DATABASE_URL 跳过）
```

真控制面验收（Kind + port-forward）：

```bash
argus --base-url http://127.0.0.1:8080 submit -f task.yaml --json
argus --base-url http://127.0.0.1:8080 wait <task_id> --follow --json
argus --base-url http://127.0.0.1:8080 report <task_id> --md
argus --base-url http://127.0.0.1:8080 artifacts <task_id>
```

## 8 已知限制

1. **MCP 不返回完整文件**：`argus_get_file` 只给摘要（默认前 1 MiB、最多 32 MiB）；完整产物走 HTTP / CLI `files`。
2. **`sha256` 可能是局部哈希**：`sha256_scope=partial` 时是「收到的字节」的哈希，不是整文件指纹；MCP 与 CLI 都明确标注。
3. **SSE 降级不重试 401/403**：鉴权错误直接失败（轮询也会失败）；其余不可用一律降级。
4. **SSE 事件不打印 runner 粒度**：`wait --follow` 只用事件维护 Task 状态、等 `task.finished`；runner 级管道进度不在终端逐条打印（避免 1000 条回放刷屏）。需要明细走 `status`。
5. **列表类命令不分页**：`runners` / `artifacts` / `profiles` 只取默认页（后两者本身不分页）；需要翻页直接用 REST。
6. **`wait` 遇控制面 4xx/5xx 立即失败**：不无限重试；只有 SSE 断线做 3 次短退避重连，之后降级轮询。
7. **MCP 只做 tools**：没有 resources / prompts / sampling / elicitation，也不发 `notifications/progress`。
8. **不处理 OIDC/CSRF**：token 由外部注入（`$AG_TOKEN`）；dev 模式默认 `dev`。生产接 OIDC 后再补「如何拿 token」，但**不做凭据代理**。
9. **不复用 `internal/api` 的 DTO**：字段是本地最小投影，响应新增字段不会破坏解析，但也不会自动出现在 CLI 输出里。
10. **`files` 不校验服务端 sha256**：控制面不返回整文件哈希；Agent 若需完整性校验，应在下载后自行比对产物清单里的 `sha256`（若已计算）。
11. **产物路径有前缀差异**：`/tasks/{id}/artifacts` 返回的 `path` 是 storage-root 相对（含 `<task_id>/`），而 `/tasks/{id}/files?path=` 要 Task 目录内相对路径（openapi 描述与实现不一致）。CLI/MCP 统一在客户端剥离 `<task_id>/` 前缀并回显 `api_path`；理想修复在 API 侧，届时 adapter 可直接透传。

## 9 下一步（按 ROI）

| 优先级 | 能力 | 具体动作 |
|---|---|---|
| 1 | 进度通知（MCP） | `initialize` 协商 `capabilities.progress`；`argus_wait_task(follow=true)` 按 `task.state_changed` / `runner.state_changed` 发 `notifications/progress`（带 `progressToken`），同时保留同步返回 |
| 2 | 分页与批量 | 列表类命令补 `--cursor/--limit`；`submit --matrix` 客户端展开；MCP `argus_submit_batch` 返回 `{task_ids[]}` |
| 3 | 幂等提示（不引入幂等键） | D-37 不变：提交前 `GET /tasks?submitted_by=...` 查重提示；绝不静默去重 |
| 4 | 更完整的产物 | `artifacts --verify`（比对清单 sha256）；`files --dir` 按 kind 批量下载；MCP `argus_get_file` 支持 `format=full`（base64 上限内） |
| 5 | SSE 事件时间线 | `argus events <task_id>`（`events/history` + `--follow`），供 Agent 解释「为什么失败」而不只是结局（含 runner 管道粒度） |
| 6 | 契约门禁 | 表驱动单测：每个 CLI 子命令 / MCP 工具 ↔ openapi operationId，防止只改 REST 不改 adapter |
| 7 | 鉴权体验 | device flow / token 刷新说明（仍不做凭据代理） |

## 10 文件清单

```text
server/cmd/argus/main.go             入口：全局选项、命令分发、退出码、usage
server/cmd/argus/client.go           最小 REST 客户端 + SSE 打开（Bearer/超时/错误信封/Range）
server/cmd/argus/commands.go         submit/status/wait/retry/clean/report/logs/artifacts/files/cancel/runners/queue/profiles
server/cmd/argus/main_test.go        httptest 假控制面 + 退出码/输出/SSE 降级断言
server/internal/mcp/client.go        MCP 专用最小 REST 客户端（含 OpenTaskEvents）
server/internal/mcp/server.go        stdio 帧、JSON-RPC 2.0、initialize/tools/list/tools/call
server/internal/mcp/tools.go         13 个工具的 description + inputSchema + 转发实现
server/internal/mcp/sse.go           SSE 帧解析 + 共享 Wait（SSE 优先、降级轮询、断线重连）
server/internal/mcp/hints.go         共享错误码 → {retryable,next_action,hint} 映射
server/internal/mcp/server_test.go   协议面 + SSE/wait + 工具 + hints 测试
```
