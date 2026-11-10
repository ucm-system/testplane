# e2e：Argus 端到端驱动脚本

独立 Go module，**不是 `go test`**：编译出一个 CLI，对运行中的控制面做
"提交 Task → 等终态 → 断言"，最后在 stdout 打一行 JSON、stderr 打人类摘要。
同时带 `go test`：用内存 fake 控制面把断言逻辑本身（含红/绿两条路）跑一遍，不需要真后端。

> 操作手册（起 mock-llm、Kind 跑法、失败注入）见 `docs/runbook/`；
> Agent 全闭环口径见 `docs/agents/argus-playbook.md`。

## 命令

```bash
cd tools/e2e
go build -o ../../bin/e2e .          # 产物不入库（bin/ 已忽略）
../../bin/e2e run                    # 默认：submit + wait + assert
../../bin/e2e submit                 # 只提交，打印 task_id
../../bin/e2e wait --task-id t-...
../../bin/e2e assert --task-id t-...
```

连接参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--base-url` | `http://127.0.0.1:8080` | 控制面地址 |
| `--api-prefix` | `/api/v1` | 契约定稿前缀；不支持 `auto` |
| `--user` | `dev` | dev 鉴权：`Authorization: Bearer <subject>` |
| `--token` | 空 | 显式 Bearer，优先于 `--user` |
| `--no-auth` | 关 | dev 模式允许匿名时用 |

等待/期望（v2 终态：`succeeded|failed|service_unavailable|canceled`，没有 `completed_with_errors`，D-73）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--expect` | `succeeded` | 逗号分隔；可选 `succeeded\|failed\|service_unavailable\|canceled` |
| `--timeout` | `5m` | 等终态总时长 |
| `--poll` | `2s` | 轮询间隔 |
| `--sse-timeout` | `10s` | SSE 冒烟/回退抓取窗口 |
| `--report-wait` | `30s` | 报告异步生成的等待窗口（C-7.2 SLA：终态后 ≤2s；超时即判 hook 未触发） |
| `--expect-error-code` | 空 | 断言 `task.error.code` |
| `--expect-runner-error-code` | 空 | 断言某个失败 runner 的 `error.code` |
| `--expect-retryable` | 空 | 断言失败对象的 `retryable=true\|false` |

提交内容（不传 `--manifest` 时用内置模板；v2 提交体：嵌套 `task:`、deploy XOR service、`runners[].input`，无 `args`/`retry`/`cleanup.policy`）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--manifest` | 空 | 已有 Manifest（YAML 或 JSON，按首字符判 Content-Type） |
| `--mode` | `deploy` | `deploy` 需要 Genet；`service` 直接指向 mock-llm |
| `--mock-url` | `http://127.0.0.1:18080/v1` | 内置模板里的 mock-llm 地址 |
| `--model` | `mock-llm` | 模型名 |
| `--profile` | `k8s-npu` | `execution.profile`；传空串则不写该字段 |
| `--name` | `e2e-<时间戳>` | Task 名 |
| `--deploy-provider` | `genet` | `deploy.provider` |
| `--runner` | `aisbench-sla` | 测试类 runner 名称 |
| `--runner-version` | `0.5.0` | 测试类 runner 版本 |
| `--precheck-version` | `1.0.0` | 显式 precheck 的 endpoint-probe 版本 |
| `--dry-run-first` | 关 | 先 `POST /tasks?dry_run=true`（v2 契约已定稿该参数） |

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
{"ok":true,"command":"run","task_id":"t-20261110-0001","expected":["succeeded"],
 "actual":"succeeded","duration_ms":8234,"status_trace":["10:00:01 status=queued",...],
 "events":{"source":"history","count":118,"first_seq":1,"last_seq":118,"types":[...]},
 "checks":[{"name":"task.expected_status","ok":true,"detail":"期望 succeeded，实际 succeeded"},...],
 "warnings":[]}
```

## 断言清单

| 检查 | 断言 |
|---|---|
| `submit.http_status` / `submit.task_id` | `POST /tasks` 返回 202 且响应含 `task.id`（07 §4.2） |
| `task.id_present` / `task.id_matches` | Task 详情 id 非空且与提交返回一致 |
| `task.terminal` / `task.expected_status` | 终态且命中 `--expect` |
| `task.status_token` | 状态 token 在设计集合内 |
| `task.runners_present` / `task.runners_valid` | 有 runners[]（最新 attempt 摘要）；seq/name/状态 token 合法、`skipped` 必带 `upstream_abort/canceled`（D-06） |
| `task.all_runners_terminal` | Task 终态时没有悬挂 runner |
| `task.runners_list_complete` | `GET /tasks/{id}/runners` 覆盖 Task 详情里的全部 runner（含 seq=1 注入行，抓 `seq > cursor` 回归） |
| `task.succeeded_no_failed_runners` / `task.succeeded_has_runner` | `succeeded` 的构成合法 |
| `task.unavailable_runners_skipped` | `service_unavailable` 时除 precheck 可重试失败外，其余 runner 全部 `skipped` |
| `task.attempts_continuous` | `attempts[].attempt_no` 从 1 连续递增、`trigger` 合法（§7.2：重试只追加） |
| `task.abort_propagation` | precheck 失败后其余 runner 全部 `skipped`（`abort_task` 传播，不允许悬挂） |
| `task.precheck_retryable_consistent` | §7.5 归类：retryable=true 的探活失败→`service_unavailable`；retryable=false→`failed` |
| `task.error_present` | 平台侧 `failed` 必须带 `task.error.code` |
| `precheck.failed_error` | precheck 失败必须带实例级 `error.code`/`retryable` |
| `expect.task_error_code` / `expect.runner_error_code` / `expect.retryable` | 可选的失败码/可重试期望 |
| `report.present` / `report.json_valid` | `GET /tasks/{id}/report` 非空且是 JSON 对象（404+Retry-After 会自动重试 30s） |
| `report.md` | 有内容时记录；没有只警告（Markdown 异步生成） |
| `runner.<name>.logs` | `GET /tasks/{id}/attempts/{n}/runners/{seq}/logs` 非空（stdout 空则看 stderr）；Job 根本没建起来的平台准备期失败（`EXECUTOR_UNAVAILABLE`/`IMAGE_PULL_FAILED`/`MOUNT_UNAVAILABLE`）只警告不判红 |
| `runner.<name>.result` | succeeded/failed 的 test/pre_check runner 有合法 `result.json`（只产出 html/csv 时 404 属三选一 D-74 的合法形态，只警告）；平台准备期失败同理只警告 |
| `events.history` | `GET /tasks/{id}/events/history` 可用且可解析 |
| `events.available` | 至少拿到一条事件 |
| `events.task_id_consistent` | 事件归属该 Task |
| `events.seq_monotonic` | `seq` 严格递增 |
| `events.transitions_legal` | 所有 `state_changed` 的 `from -> to` 在 §2.1（Task，含终态→queued retry 边）/§2.2（runner，无重试回路）转移表内 |
| `events.submitted` | 有 `submitted` 事件 |
| `events.terminal_once` | Task 终态事件至多一次 |
| `events.final_state` | 最后一条终态事实（state_changed to=终态 / task_finished.status）== Task 实际终态 |
| `sse.connect` | `GET /tasks/{id}/events` 返回 200 + `text/event-stream`，且窗口内读到一行 |

软性项（默认警告，`--strict` 升级为失败）：缺 `task_finished`、终态 runner 缺 `runner_finished`、
未知事件类型、`report.md` 缺失、详情缺 `attempts[]`。

## 内置 Manifest

`--mode service`（本地无 Genet 时用）：

```yaml
task:
  name: "e2e-20261110-100000"
  service:
    url: http://127.0.0.1:18080/v1
    model_name: mock-llm
  precheck:                       # 显式对象：固定 runner/version，把探活等待窗口压短
    runner: endpoint-probe
    version: "1.0.0"
    execution: { profile: k8s-npu }
    input: |
      {"wait_timeout_seconds":10,"retry_interval_seconds":1}
  runners:
    - name: probe-a
      runner: aisbench-sla
      version: "0.5.0"
      execution: { profile: k8s-npu }
      input: |
        {"sla":["ttft.avg:999999"],"work_path":"/tmp/aisbench-ws",
         "aisbench_cmd":"/runner/tool/tests/stub_aisbench.py", ...}
```

`--mode deploy`（默认）把 `service:` 换成 `deploy: {provider: genet, spec: <Genet 部署描述原文>}`。
`precheck` 写显式对象是为了把探活等待窗口压短（endpoint-probe 默认 600s 会让失败注入用例跑十分钟）；
测试 runner 用 aisbench-sla 的 stub（`aisbench_cmd` 指向镜像内 stub）秒级返回。

## 自测（不需要真后端）

```bash
cd tools/e2e
gofmt -l .          # 应无输出
go vet ./...
go test ./... -v
```

`e2e_test.go` 里的 fake 控制面覆盖：happy path（含报告 404 重试）、严格模式、
非法 Task/runner 状态转移、retry 边合法性（终态→queued，§7.8）、seq 非单调、期望不符、
`--expect` 值域（拒绝 `completed_with_errors`）、`service_unavailable`（retryable 对/错）、
precheck 401（`failed` + `retryable=false` 对/错）、空日志、skipped 缺 reason（D-06）、
SSE 不可用、连接失败、提交被拒（VALIDATION_ERROR 带 issues 行号）、三种鉴权、
submit/wait/assert 分步、非终态 assert、自定义前缀。

## 已知边界

- 契约三方比对（spec ↔ 设计端点基线）由 `tools/contract` 负责，本工具不做 spec 解析。
- deploy 模式的端到端需要 fake-genet + service-deploy runner 在位；联调步骤见 runbook。
