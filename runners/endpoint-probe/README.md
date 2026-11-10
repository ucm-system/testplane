# endpoint-probe：服务就绪探测（Pre-Check）

回答一个二值问题：**这个 LLM 端点此刻真的能出 token 吗？**

- 角色 `pre_check`：失败时平台强制终止整个 Task，后续 runner 标 `SKIPPED`、正在跑的 `CANCELED`（§7.3）
- 入口 `/runner/run.sh`（由 `runner.yaml` 的 `entry: ["/runner/run.sh"]` 声明，§5.2）
- 契约 [ARCHITECTURE.md](../../design/ARCHITECTURE.md) §5.2、§5.3、§5.4、§5.4.1、§5.11、附录 A

## 目录结构

```text
endpoint-probe/
├── runner.yaml       # 唯一权威：元数据 + entry + input_format + 参数 schema + 资源/并发
├── run.sh            # 唯一执行入口（平台调它，独立运行也跑它）
├── build/
│   └── Dockerfile    # 构建定义（构建上下文 = 本目录）
├── run-local.sh      # 本地便捷脚本（只 run，不 build）
└── README.md
```

**这里没有、也不该有：**

- `deployment.yaml` / `job.yaml` —— 部署由内置 service-deploy runner + Genet 负责（§9），特殊运行时需求走 `runner.yaml.k8s` 的受限字段（§6.8）。
- `results/` —— 容器内固定写 `/workspace/output`，由平台挂到最终存储路径；本地调试写到 `.local/out`。

## 怎么判定

`POST /v1/chat/completions`，`max_tokens=1`、`stream=true`、`temperature=0`：

| 响应 | 判定 | 是否继续等 |
|---|---|---|
| 2xx 且响应体出现 `data:` 帧 | **就绪**，首包 token 说明后端确实在出字 | — |
| 5xx / 404 / 429 / 408 / 连接被拒 / 超时 | 服务还没起来或正在加载 | 等到窗口用尽 |
| 400 / 405 / 406 / 413 / 415 / 422 / 501 | 请求本身不对，重试不会自愈 | **立即失败**（`retryable: false`） |
| 401 / 403 | 认证失败，重试不会自愈 | **立即失败** |
| 2xx 但带 `error` 对象 | 网关把错误包成了 200 | **立即失败** |
| DNS 解析失败 / TLS 证书错误 | 地址或证书配错了 | **立即失败** |

两个关键点来自现网 E2E 踩的坑：**限制 `max_tokens=1` 且默认流式**——否则服务一路生成到 EOS，慢模型几十分钟才返回，被误判成"没起来"；**404 不算致命**——PD 分离下 router 先可达、后端 model server 注册要等，这类错误应留在窗口内重试。

## 和两条平台约束的关系

**Fail Fast on Pre-Check（§1）**：`runner.yaml` 声明了 `on_failure: abort_task`——本 Runner 失败时，平台终止整个 Task（未开始的 runner `SKIPPED`、正在跑的 `CANCELED`），不再消耗测试资源。Runner 这一侧要保证不空等：不可重试的错误（4xx、认证、DNS/TLS、2xx 包错误）立即结束并置 `retryable: false`，只有"服务还没起来"才耗窗口。否则它是独占 runner，会一直占着该服务，把后面的测试全挡住。

**Isolation by Default（§1）**：默认隔离来自两件事——Runner 只写 `${AG_OUTPUT_DIR}`（本 attempt 的目录），不碰共享路径；影响扩散则**显式声明**：`on_failure: abort_task` 声明失败时要终止 Task，`concurrency.endpoint_exclusive: true` 声明运行期间要独占该服务（v2 中仅外接模式可能有并行测量，§6.7）。两条都不依赖平台的隐含规则。

## 怎么调用（v2：输入文件 + env，不走 argv）

业务输入只走输入文件，平台执行 entry 时 argv 恒空（§5.3）。输入读取优先级：
`--params-file <path>` > `$AG_PARAMS_FILE` > `/workspace/input/params.jsonl`。

输入是 **JSONL**（每行一个 JSON 对象；本 Runner 的参数放一行）：

```json
{"wait_timeout_seconds": 600, "retry_interval_seconds": 15, "max_tokens": 1, "stream": true}
```

```bash
# 平台内：entry=[​"/runner/run.sh"]，参数文件落盘在 /workspace/input/params.jsonl（只读挂载）
# 独立运行（§5.10）：CLI 显式传入口与公共参数
/runner/run.sh \
  --params-file /tmp/params.jsonl \
  --url=http://10.0.0.10:8000/v1 \
  --model_name=Qwen3-32B \
  # 业务参数也可以全走文件：输入里 {"wait_timeout_seconds":18000}
```

**url 是 OpenAI 兼容的 base URL**（业界统一形态）：写 `http://host:8000/v1`；不带 `/v1` 会自动补，末尾斜杠忽略。探活路径由 Runner 自己拼（`<url>/chat/completions`、`<url>/models`），**不单独暴露 path 参数**。

| 通道 | 内容 |
|---|---|
| 输入文件 | 全部业务参数（JSONL，见上）。平台做词法/结构校验后原样落盘（§5.4） |
| 环境变量（公共参数） | `AG_SERVED_URL` / `AG_SERVED_MODEL_NAME`（§5.4.1；独立运行可用 `--url` / `--model_name` 覆盖） |
| 环境变量（元信息） | `AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ`（缺省 `local` / `1` / `1`） |
| 凭据 | `ENDPOINT_API_KEY`（可选）。平台上由 Task 级 `secrets` 注入（§12.4）；独立运行时自己 `-e` |
| 输出目录 | `/workspace/output`（可用 `AG_OUTPUT_DIR` 覆盖）：`result.json`、`logs/probe.log`、`raw/` |

url / model_name 是**公共参数**（§5.4.1），来源看场景：**Task 里有 `deploy`** 时平台注入 deploy 导出；**外接模式**来自 Manifest 的 `service` 块；**独立运行**时用 CLI 或环境变量自己传。`AG_MODEL_PATH` 本 Runner 用不到，但会容忍。

## 参数清单

`runner.yaml.parameters` 是结构化 schema（§5.4）：平台提交期校验**类型**与 **required 存在性**；
范围/枚举/跨字段语义仍归本 Runner 运行时校验。url / model_name 不在其中（公共参数走环境变量）。

| 参数 | 默认 | 说明 |
|---|---|---|
| `wait_timeout_seconds` | `600` | 就绪等待总窗口，`0` 表示只探一次；PD/慢模型可到 `18000` |
| `retry_interval_seconds` | `15` | 轮询间隔，`>= 1` |
| `max_tokens` | `1` | 探活请求的 token 上限，**不要调大** |
| `stream` | `true` | 首包即通过；`false` 时要求 `content` 非空 |

## 退出码

> 产物契约（D-74）：test / pre_check Runner 产出 `result.json` / `result.html` / `result.csv` **至少其一**。
> 本 Runner 选择产出 `result.json`（结构见附录 A schema 2.0）。


| 码 | 含义 | `result.json.error.code` |
|---|---|---|
| `0` | 就绪 | — |
| `1` | 未就绪 / 不可达 | `PRECHECK_FAILED` / `ENDPOINT_UNREACHABLE` |
| `2` | 参数或输入错误 | `VALIDATION_ERROR` |

平台看到 `role: pre_check` 的 runner 失败，即按 `on_failure: abort_task` 终止 Task，正式测试 runner 标 `SKIPPED`。

## 独立运行

**与平台里完全同构**（§5.10）：镜像由 CI 产出，这里只 `docker run`，不 build。

```bash
export IMAGE=registry.dev.huawei.com/argus/endpoint-probe@sha256:...
echo '{"wait_timeout_seconds":60}' > /tmp/params.jsonl
docker run --rm \
  -e ENDPOINT_API_KEY=... -e AG_OUTPUT_DIR=/workspace/output \
  -v /tmp/out:/workspace/output \
  -v /tmp/params.jsonl:/workspace/input/params.jsonl:ro \
  ${IMAGE} \
  /runner/run.sh --url http://10.0.0.10:8000/v1 --model_name Qwen3-32B
# 或者用便捷脚本（.local/params.jsonl 存在时自动挂载为输入文件）：
#   ./run-local.sh --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B
jq . .local/out/result.json
```

不用容器也可以（需要 bash / curl / jq）：

```bash
AG_OUTPUT_DIR=.local/out bash run.sh \
  --params-file /tmp/params.jsonl --url=10.0.0.10:8000 --model_name=Qwen3-32B
```

## 构建（只发生在 CI）

```bash
cd runners/endpoint-probe
TAG=1.0.0-$(git rev-parse --short HEAD)
docker build -f build/Dockerfile -t registry.dev.huawei.com/argus/endpoint-probe:${TAG} .
docker push registry.dev.huawei.com/argus/endpoint-probe:${TAG}
# 把镜像引用回填 runner.yaml 的 image 字段（tag 或 digest 均可）
```

**构建方式不做规范**：没有 build 脚本也可以，能把构建上下文与命令写清楚就行（Dockerfile 注释或本节的这几行）。平台、SSH 主机、本地一律只 `docker pull` + `docker run`（§6.2）——在目标机器上 build 会让"跑的是哪个版本"不可追溯。

## 部署注意

- **网络可达性**：K8s ClusterIP 之类地址不保证控制端可达，所以 Pre-Check 也必须经 Executor 在集群内执行（§5.11）。
- **挂载权限**：镜像以 UID 10001 运行，挂载目录需 `fsGroup: 10001`（K8s）；自己 `docker run` 时用 `-u $(id -u):$(id -g)`（`run-local.sh` 已处理）。
- **代理**：脚本用 `--noproxy '*'` 直连，避免代理返回的 502 被当成"服务没起来"。
- **镜像引用**：`runner.yaml` 里写 CI 产出的镜像（tag 或 digest 均可，平台不校验形态）；拉取失败平台报 `IMAGE_PULL_FAILED`。
