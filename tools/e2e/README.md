# e2e：TestPlane 端到端驱动脚本

独立 Go module，**不是 `go test`**：编译出一个 CLI，对运行中的控制面做
"提交 → 等终态 → 断言"，最后在 stdout 打一行 JSON、stderr 打人类摘要。
同时带 `go test`：用内存 fake 控制面把断言逻辑本身（含红/绿两条路）跑一遍，不需要真后端。

> 操作手册（起 mock-llm、Kind 跑法、失败注入）见 [`scripts/e2e/README.md`](../../scripts/e2e/README.md)。

## 命令

```bash
cd tools/e2e
go build -o ../../bin/e2e .          # 产物不入库（bin/ 已忽略）
../../bin/e2e run                    # 默认：submit + wait + assert
../../bin/e2e submit                 # 只提交，打印 run_id
../../bin/e2e wait --run-id r-...
../../bin/e2e assert --run-id r-...
```

连接参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--base-url` | `http://127.0.0.1:8080` | 控制面地址 |
| `--api-prefix` | `/api/v1` | MVP 定稿前缀（票 #3 裁决）；不支持 `auto` |
| `--user` | `dev` | dev 鉴权：`Authorization: Bearer <subject>` |
| `--token` | 空 | 显式 Bearer，优先于 `--user` |
| `--no-auth` | 关 | dev 模式允许匿名时用 |

等待/期望：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--expect` | `succeeded` | 逗号分隔；可选 `succeeded\|completed_with_errors\|service_unavailable\|failed\|canceled` |
| `--timeout` | `5m` | 等终态总时长 |
| `--poll` | `2s` | 轮询间隔 |
| `--sse-timeout` | `10s` | SSE 冒烟/回退抓取窗口 |
| `--report-wait` | `30s` | 报告异步生成的等待窗口（C-7.2 SLA：终态后 ≤2s；超时即判 hook 未触发） |
| `--expect-error-code` | 空 | 断言 `run.error.code` |
| `--expect-task-error-code` | 空 | 断言某个失败 Task 的 `error.code` |
| `--expect-retryable` | 空 | 断言失败对象的 `retryable=true\|false` |

提交内容（不传 `--manifest` 时用内置模板）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--manifest` | 空 | 已有 Manifest（YAML 或 JSON，按首字符判 Content-Type） |
| `--mode` | `deploy` | `deploy` 需要 Genet；`service` 直接指向 mock-llm |
| `--mock-url` | `http://127.0.0.1:18080/v1` | 内置模板里的 mock-llm 地址 |
| `--model` | `mock-llm` | 模型名 |
| `--profile` | `k8s-npu` | `execution.profile`；传空串则不写该字段 |
| `--name` | `e2e-<时间戳>` | Run 名 |
| `--deploy-provider` | `genet` | `deploy.provider` |
| `--dry-run-first` | 关 | 先 `POST /runs?dry_run=true`；MVP 未实现该参数时保持关闭 |

输出控制：`--json-out <file>`（另存缩进 JSON）、`--strict`（警告即失败）、
`--allow-empty-logs`（日志为空只警告）、`--verbose`（打印每个 HTTP 请求与 Manifest）。

退出码：

| 码 | 含义 |
|---|---|
| 0 | 全部检查通过 |
| 1 | 断言失败 / 控制面返回错误 / 等待超时 |
| 2 | 参数或配置错误 |
| 3 | 连不上控制面（消息里直接提示 `make dev` 与 8080） |

stdout 只有一行：

```json
{"ok":true,"command":"run","run_id":"r-20261105-0001","expected":["succeeded"],
 "actual":"succeeded","duration_ms":8234,"status_trace":["10:00:01 status=queued",...],
 "events":{"source":"history","count":118,"first_seq":1,"last_seq":118,"types":[...]},
 "checks":[{"name":"run.expected_status","ok":true,"detail":"期望 succeeded，实际 succeeded"},...],
 "warnings":[]}
```

## 断言清单

| 检查 | 断言 |
|---|---|
| `submit.http_status` / `submit.run_id` | `POST /runs` 返回 202 且响应含 `run.id`（07 §4.2） |
| `run.id_present` / `run.id_matches` | Run 详情 id 非空且与提交返回一致 |
| `run.terminal` / `run.expected_status` | 终态且命中 `--expect` |
| `run.status_token` | 状态 token 在设计集合内 |
| `run.tasks_present` / `run.tasks_valid` | 有 Task；id/name/状态 token 合法、名字不重复 |
| `run.skipped_without_attempt` | `skipped` 的 Task 没有 Attempt（C-5.3/C-5.5） |
| `run.all_tasks_terminal` | Run 终态时没有悬挂 Task |
| `run.succeeded_no_failed_tasks` / `run.succeeded_has_task` | `succeeded` 的构成合法 |
| `run.unavailable_tasks_skipped` | `service_unavailable` 时除 precheck 可重试失败外，其余 Task 全部 `skipped` 且无 Attempt |
| `run.tasks_list_complete` | `GET /runs/{id}/tasks` 覆盖 Run 详情里的全部 Task（含 seq=0 precheck，抓 `seq > cursor` 回归） |
| `run.abort_propagation` | precheck 失败后其余 Task 全部 `skipped`（`abort_run` 传播，不允许悬挂） |
| `run.precheck_retryable_consistent` | C-5.8 归类：retryable=true 的探活失败→`service_unavailable`；retryable=false→`completed_with_errors` |
| `run.has_failures` | `completed_with_errors` 至少一个失败/跳过 Task |
| `run.error_present` | `failed` 必须带 `run.error.code` |
| `precheck.failed_error` | precheck 失败必须带实例级 `error.code`/`retryable` |
| `expect.run_error_code` / `expect.task_error_code` / `expect.retryable` | 可选的失败码/可重试期望 |
| `report.present` / `report.json_valid` | `GET /runs/{id}/report` 非空且是 JSON 对象（404+Retry-After 会自动重试 30s） |
| `report.md` | 有内容时记录；没有只警告（Markdown 异步生成） |
| `task.<name>.attempts_continuous` | `attempts.idx` 从 1 连续递增（C-3.4） |
| `task.<name>.attempt.<idx>.logs` | `GET /tasks/{id}/attempts/{idx}/logs` 非空（stdout 空则看 stderr）；Job 根本没建起来的平台准备期失败（`EXECUTOR_UNAVAILABLE`/`IMAGE_PULL_FAILED`/`MOUNT_UNAVAILABLE`）只警告不判红 |
| `task.<name>.attempt.<idx>.result` | succeeded/failed 的 Attempt 有合法 `result.json`；平台准备期失败同理只警告 |
| `events.history` | `GET /runs/{id}/events/history` 可用且可解析 |
| `events.available` | 至少拿到一条事件 |
| `events.run_id_consistent` | 事件归属该 Run |
| `events.seq_monotonic` | `seq` 严格递增 |
| `events.transitions_legal` | 所有 `run/task.state_changed` 的 `from -> to` 在 C-3.1/C-3.2 转移表内 |
| `events.run_submitted` | 有 `run.submitted`（C-9.1） |
| `events.terminal_once` | Run 终态事件至多一次 |
| `events.final_state` | 最后一条 `run.state_changed` 的 `to` == Run 实际终态 |
| `sse.connect` | `GET /runs/{id}/events` 返回 200 + `text/event-stream`，且窗口内读到一行 |

软性项（默认警告，`--strict` 升级为失败）：缺 `run.finished`、缺 `attempt.started/finished` 覆盖、
未知事件类型、`report.md` 缺失、Attempt 列表不可读（按摘要合成 idx）。

## 内置 Manifest

`--mode service`（本地无 Genet 时用）：

```yaml
run:
  name: "e2e-20261105-100000"
  service:
    url: http://127.0.0.1:18080/v1
    model_name: mock-llm
  precheck:
    tester: endpoint-probe
    version: "1.0.0"
    args: "--wait_timeout_seconds=60 --retry_interval_seconds=1"
  cleanup: { policy: never }
  tests:
    - name: probe-a
      tester: endpoint-probe
      version: "1.0.0"
      execution: { profile: k8s-npu }
      args: "--wait_timeout_seconds=10 --retry_interval_seconds=1"
```

`--mode deploy`（默认）把 `service:` 换成 `deploy: {provider: genet, parameters: {model, endpoint}}`，
`cleanup.policy` 改为 `always`。`precheck` 写显式对象是为了把探活等待窗口压短
（默认 600s 会让失败注入用例跑十分钟）。

## 自测（不需要真后端）

```bash
cd tools/e2e
gofmt -l .          # 应无输出
go vet ./...
go test ./... -v
```

`e2e_test.go` 里的 fake 控制面覆盖：happy path（含报告 404 重试）、严格模式、
非法状态转移、seq 非单调、期望不符、`service_unavailable`（retryable 对/错）、
precheck 401（`completed_with_errors` + `retryable=false` 对/错）、空日志、
SSE 不可用、连接失败、提交被拒、三种鉴权、submit/wait/assert 分步、非终态 assert、自定义前缀。

## 还没做（等后端/契约落地）

- 契约测试（对 `server/api/openapi.yaml` 做实现侧校验）——等 Lead 通知文件就绪。
- 独立 Task 形态（`POST /tasks`）的 e2e——票 #3 第一阶段的断言聚焦 Run；端点已在 07 §5.2。
- 前端冒烟（`GET /` 返回 HTML、`pnpm build`）——第二阶段。
