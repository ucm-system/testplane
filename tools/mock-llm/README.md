# mock-llm：OpenAI 兼容的假 LLM 服务

给 TestPlane 的 **Pre-Check（`testers/endpoint-probe`）** 和 **e2e** 喂一个可控的"被测 LLM 服务"：
不需要真模型、不需要 GPU，就能复现"就绪 / 就绪慢 / 500 / 401 / HTTP 200 包错误"这几种现场。

只用 Go 标准库，零依赖。默认监听 **`:18080`**（控制面是 8080，不要占用）。

## 为什么需要它

`endpoint-probe` 的判定逻辑不是"HTTP 200 就算过"，而是：

| 探针看到什么 | 判定 | 平台侧结果 |
|---|---|---|
| 2xx 且响应体出现 `data:` 帧（`stream:true`） | 就绪 | Pre-Check 通过 |
| 5xx / 404 / 429 / 连接拒绝 | 继续等，窗口用尽后失败（可重试） | Run `service_unavailable` |
| 400/405/… 或 401/403 | 立即失败（`retryable:false`） | Run `completed_with_errors` |
| 2xx 但响应体是 `{"error":...}` | 立即失败 | Run `completed_with_errors` |

这些分支没有真服务时无法验证，所以 mock 必须能精确伪造它们——包括"HTTP 200 但正文是错误对象"这种最容易被实现漏掉的形态。

## 快速开始

```bash
# 源码运行
go run ./tools/mock-llm

# 带默认故障启动（所有请求都 503，模拟服务还没起来）
go run ./tools/mock-llm -ready-after=10s

# 编译后运行
go build -o bin/mock-llm ./tools/mock-llm && ./bin/mock-llm -addr=:18080
```

启动后：

```bash
curl -sS http://127.0.0.1:18080/healthz                       # ok
curl -sS http://127.0.0.1:18080/v1/models                     # {"data":[{"id":"mock-llm"}]}
curl -sS -N -X POST 'http://127.0.0.1:18080/v1/chat/completions' \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-llm","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":true}'
# data: {"choices":[{"delta":{"content":"TestPlane ","role":"assistant"},...}]}
# data: {"choices":[{"delta":{},"finish_reason":"stop",...}]}
# data: [DONE]
```

## 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 进程存活，永远 200（**不受 `ready_after` 影响**） |
| `GET` | `/v1/models` | `{"object":"list","data":[{"id":"mock-llm",...}]}`；受故障注入影响 |
| `POST` | `/v1/chat/completions` | `stream:true` → SSE；`stream:false` → 普通 JSON |

路径按**后缀**匹配，`/models`、`/chat/completions`、`/openai/v1/models` 都能用——
`endpoint-probe` 会把 base URL 补成 `<url>/v1` 再拼路径，换个 base URL 写法不会 404。

行为细节：

- `stream:true`：每个 token 一帧 `data: {...}`（`object=chat.completion.chunk`），
  最后一帧 `finish_reason:"stop"`，然后 `data: [DONE]`；每帧都 flush，探针能立刻看到首包。
- `stream:false`：`choices[0].message.content` 非空；`max_tokens=1` 时至少输出一个 token。
- 响应体没带 `model` 时用启动参数 `-model`（默认 `mock-llm`）兜底。

## 故障注入

查询参数优先于启动参数/环境变量；参数写错（如 `fail=abc`）返回 **400**，
避免 e2e 拼错参数却悄悄测了个正常路径。

| 查询参数 | 含义 | 探针预期判定 |
|---|---|---|
| `?ready_after=3` | 服务启动后前 3 秒返回 `503` + `Retry-After: 1` | 继续等；窗口够就成功 |
| `?fail=500` | 直接返回 500（OpenAI 错误对象） | 可重试，窗口用尽后失败 |
| `?fail=401` | 401 `invalid_api_key` | **立即失败**，`retryable:false` |
| `?fail=403` / `?fail=429` | 其余常见码 | 403 立即失败；429 可重试 |
| `?error_in_200=1` | HTTP 200，正文是 `{"error":{...}}`，且没有 `data:` 帧 | **立即失败**（网关把错误包成 200） |
| `?slow_ms=2000` | 响应前延迟 2 秒 | 仍算就绪（只是慢） |
| `?model=other` | 临时改返回的模型名 | — |

同名环境变量设默认值：
`MOCK_LLM_ADDR` / `MOCK_LLM_MODEL` / `MOCK_LLM_READY_AFTER` / `MOCK_LLM_FAIL` /
`MOCK_LLM_ERROR_IN_200` / `MOCK_LLM_SLOW_MS` / `MOCK_LLM_QUIET`。

`ready_after` 的锚点是**进程启动时刻**（不是首次请求）：服务起来后的前 N 秒一直 503，
窗口过完自动转正常。这个语义让"就绪慢"用例可复现，不依赖请求顺序。

## 单测

```bash
cd tools/mock-llm
go vet ./...
go test ./... -v
```

覆盖：healthz、models、带/不带 `/v1` 的路径、stream（帧结构/数量/[DONE]）、non-stream（content 非空）、
503 就绪窗口、401/500 注入、`error_in_200`、`slow_ms`、非法 JSON、非法控制参数、405/404。
（18 个用例，全部通过。）

## 与真实 Tester 的联调记录（2026-11-05）

不 build Tester 镜像、不改 `testers/`：用一次性 alpine 容器装 `jq/bash/curl`，
只把 `testers/endpoint-probe/run.sh` 只读挂进去跑——这正是"契约同构"的验证。

```bash
# 宿主上先起 mock（一个场景一个端口）
mock-llm -addr=:18080
mock-llm -addr=:18081 -fail=401
mock-llm -addr=:18082 -fail=500
mock-llm -addr=:18083 -error-in-200
mock-llm -addr=:18085 -ready-after=15s

# 容器内跑真实探针（base URL 用 host.docker.internal 回指宿主）
docker run --rm --add-host=host.docker.internal:host-gateway \
  -v "$REPO/testers/endpoint-probe/run.sh:/tester/run.sh:ro" \
  -v "$REPO/.data/qa-probe/ready:/workspace/output" \
  alpine:3.20 sh -c \
  'apk add --no-cache jq bash curl >/dev/null && exec bash /tester/run.sh \
     --url=http://host.docker.internal:18080/v1 --model_name=mock-llm \
     --wait_timeout_seconds=20 --retry_interval_seconds=1'
```

实测结果（`exit` 是探针退出码，读自各场景的 `result.json`）：

| 场景 | mock 参数 | 探针参数 | exit | status | error.code | retryable | attempts |
|---|---|---|---|---|---|---|---|
| 正常就绪 | — | 窗口 20s / 间隔 1s | 0 | `succeeded` | — | — | 1 |
| 就绪慢 15s | `-ready-after=15s` | 窗口 30s / 间隔 2s | 0 | `succeeded` | — | — | **7** |
| 认证失败 | `-fail=401` | 窗口 30s / 间隔 1s | 1 | `failed` | `PRECHECK_FAILED` | `false` | 1 |
| 5xx | `-fail=500` | 窗口 2s / 间隔 1s | 1 | `failed` | `PRECHECK_FAILED` | `true` | 3 |
| 200 包错误 | `-error-in-200` | 窗口 30s / 间隔 1s | 1 | `failed` | `PRECHECK_FAILED` | `false` | 1 |

关键日志（就绪慢场景，7 次探测后成功）：

```text
第 1 次探测：未就绪 —— HTTP 503：{"error":{"code":"starting",...约 10.9 秒后就绪...}}
...
第 7 次探测：HTTP 200，收到 SSE 数据帧，服务可出 token
结论：status=succeeded HTTP 200，收到 SSE 数据帧，服务可出 token
```

401 场景第 1 次就结束（不等满 30s 窗口）：

```text
第 1 次探测：未就绪 —— HTTP 401：认证失败（检查 ENDPOINT_API_KEY）
判定为不可重试错误，立即结束，不再等待
结论：status=failed 不可重试：HTTP 401：认证失败（检查 ENDPOINT_API_KEY）
```

> 表格里的墙钟时间包含容器内 `apk add jq bash curl`（约 4–5 秒），探针本身的等待以日志时间为准。

## Docker / Kind

```bash
docker build -t testplane/mock-llm:dev tools/mock-llm
docker run --rm -p 18080:18080 testplane/mock-llm:dev
kind load docker-image testplane/mock-llm:dev --name testplane
```

在 Kind 里当被测服务用时，Pod 内访问地址是
`http://mock-llm.<namespace>.svc.cluster.local:18080/v1`（或同命名空间直接 `http://mock-llm:18080/v1`）。
镜像以 UID 10001 运行，无特权、无挂载需求。

## 边界

- 不做鉴权校验：`Authorization` 有没有都返回 200（Auth 失败用 `?fail=401` 显式注入，语义更清楚）。
- 不模拟真实模型能力：回复是固定 token 序列，`max_tokens` 只决定截断到几个 token。
- 单实例共享状态：`ready_after` 以进程启动时刻为准；要多个不同故障的实例，就多起几个端口。
