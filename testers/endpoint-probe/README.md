# endpoint-probe：服务就绪探测（Pre-Check）

回答一个二值问题：**这个 LLM 端点此刻真的能出 token 吗？**

- 角色 `pre_check`：失败时平台强制终止后续测试，Run 终态 `SERVICE_UNAVAILABLE`
- 入口 `/tester/run.sh`
- 契约 [ARCHITECTURE.md](../../design/ARCHITECTURE.md) §5.3、§5.4、§5.4.1、§7.2、附录 A

## 目录结构

```text
endpoint-probe/
├── tester.yaml       # 元数据 + 参数清单（文档语义） + 资源需求
├── run.sh            # 唯一执行入口（平台调它，独立运行也跑它）
├── build/
│   └── Dockerfile    # 构建定义（构建上下文 = 本目录）
├── run-local.sh      # 本地便捷脚本（只 run，不 build）
└── README.md
```

**这里没有、也不该有：**

- `deployment.yaml` / `job.yaml` —— 部署由平台渲染（§6.8）。
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

**Fail Fast on Pre-Check（§1）**：`tester.yaml` 声明了 `on_failure: abort_run`——本 Tester 失败时，平台终止整个 Run（未开始的 `SKIPPED`、正在跑的 `CANCELED`），不再消耗测试资源。Tester 这一侧要保证不空等：不可重试的错误（4xx、认证、DNS/TLS、2xx 包错误）立即结束并置 `retryable: false`，只有"服务还没起来"才耗窗口。否则它是独占 Task，会一直占着该 Endpoint，把后面的测试全挡住。

**Isolation by Default（§1）**：默认隔离来自两件事——Tester 只写 `${OUT}`（本次 Attempt 的目录），不碰共享路径；影响扩散则**显式声明**：`on_failure: abort_run` 声明失败时要终止别人的 Task，`concurrency.endpoint_exclusive: true` 声明运行期间要独占该 Endpoint。两条都不依赖平台的隐含规则。

## 怎么调用

参数**全走命令行**，没有参数文件、没有输入目录（§5.3）：

```bash
/tester/run.sh \
  --url=http://10.0.0.10:8000/v1 \
  --model_name=Qwen3-32B \
  --wait_timeout_seconds=18000
```

**`--url` 是 OpenAI 兼容的 base URL**（业界统一形态）：写 `http://host:8000/v1`；不带 `/v1` 会自动补，末尾斜杠忽略。探活路径由 Tester 自己拼（`<url>/chat/completions`、`<url>/models`），**不单独暴露 path 参数**。

| 通道 | 内容 |
|---|---|
| argv | 全部参数，形如 `--key=value`。平台只做词法切分，不解释语义 |
| 环境变量 | `TESTPLANE_RUN_ID` / `TESTPLANE_TASK_ID` / `TESTPLANE_ATTEMPT_ID`（缺省 `local`） |
| 凭据 | `ENDPOINT_API_KEY`（可选）。平台上由 Run 级 `secrets` 注入（§12.4）；独立运行时自己 `-e` |
| 输出目录 | `/workspace/output`（可用 `OUTPUT_DIR` 覆盖）：`result.json`、`logs/probe.log`、`raw/` |

`--url` / `--model_name` / `--model_path` 是**公共参数**（§5.4.1），来源看场景：**Run 里有 `deploy`** 时平台注入（`args` 里别重复写）；**跑已有服务**时你必须自己在 `args` 里写；**独立运行**时同理由你传。`--model_path` 本 Tester 用不到，但会容忍。

## 参数清单

以下是**文档语义**（§5.4）：平台不渲染、不校验，写错只会在运行时暴露。

| 参数 | 来源 | 默认 | 说明 |
|---|---|---|---|
| `--url` | 平台注入 / 自己传 | — | **必填**。OpenAI 兼容 base URL，`http://host:8000/v1`；不带 `/v1` 自动补 |
| `--model_name` | 平台注入 / 自己传 | — | 服务端模型名；省略时从 `/models` 取第一个并在结果里标注来源 |
| `--wait_timeout_seconds` | 调用方 | `600` | 就绪等待总窗口，`0` 表示只探一次；PD/慢模型可到 `18000` |
| `--retry_interval_seconds` | 调用方 | `15` | 轮询间隔 |
| `--max_tokens` | 调用方 | `1` | 探活请求的 token 上限，**不要调大** |
| `--stream` | 调用方 | `true` | 首包即通过；`false` 时要求 `content` 非空 |

`--url` / `--model_name` / `--model_path` 是**公共参数保留字**（§5.4.1）：平台会自动追加，`--model_path` 本 Tester 用不到但**会容忍**（不报错）。独立运行时前两个由你自己传。

## 退出码

| 码 | 含义 | `result.json.error.code` |
|---|---|---|
| `0` | 就绪 | — |
| `1` | 未就绪 / 不可达 | `PRECHECK_FAILED` / `ENDPOINT_UNREACHABLE` |
| `2` | 参数或输入错误 | `VALIDATION_ERROR` |

平台看到 `role: pre_check` 的 Task 失败，即把 Run 置为 `SERVICE_UNAVAILABLE`，正式测试 Task 标 `SKIPPED`。

## 独立运行

**与平台里完全同构**（§5.10）：镜像由 CI 产出，这里只 `docker run`，不 build。

```bash
export IMAGE=registry.dev.huawei.com/testplane/endpoint-probe@sha256:...
./run-local.sh --wait_timeout_seconds=60 --url=10.0.0.10:8000 --model_name=Qwen3-32B
jq . .local/out/result.json
```

不用容器也可以（需要 bash / curl / jq）：

```bash
OUTPUT_DIR=.local/out bash run.sh --url=10.0.0.10:8000 --model_name=Qwen3-32B --wait_timeout_seconds=60
```

## 构建（只发生在 CI）

```bash
cd testers/endpoint-probe
TAG=1.0.0-$(git rev-parse --short HEAD)
docker build -f build/Dockerfile -t registry.dev.huawei.com/testplane/endpoint-probe:${TAG} .
docker push registry.dev.huawei.com/testplane/endpoint-probe:${TAG}
# 把镜像引用回填 tester.yaml 的 image 字段（tag 或 digest 均可）
```

**构建方式不做规范**：没有 build 脚本也可以，能把构建上下文与命令写清楚就行（Dockerfile 注释或本节的这几行）。平台、SSH 主机、本地一律只 `docker pull` + `docker run`（§6.2）——在目标机器上 build 会让"跑的是哪个版本"不可追溯。

## 部署注意

- **网络可达性**：K8s ClusterIP 之类地址不保证控制端可达，所以 Pre-Check 也必须走 Executor 执行（§7.2）。
- **挂载权限**：镜像以 UID 10001 运行，挂载目录需 `fsGroup: 10001`（K8s）；自己 `docker run` 时用 `-u $(id -u):$(id -g)`（`run-local.sh` 已处理）。
- **代理**：脚本用 `--noproxy '*'` 直连，避免代理返回的 502 被当成"服务没起来"。
- **镜像引用**：`tester.yaml` 里写 CI 产出的镜像（tag 或 digest 均可，平台不校验形态）；拉取失败平台报 `IMAGE_PULL_FAILED`。
