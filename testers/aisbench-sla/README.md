# aisbench-sla：前缀缓存 SLA 搜索（最大并发 / 最高吞吐）

回答一个问题：**给定负载形态与延迟约束，这套服务最多能扛到几并发、最高吞吐是多少？**

- 角色 `test`：失败不终止整个 Run（`on_failure: continue`）——单个并发点抖动只影响该点判定
- 入口 `/tester/run.sh`
- 独占 endpoint（`concurrency.endpoint_exclusive: true`）：工具会按并发阶梯持续打满服务，与任何其他测量并行都会互相污染
- 契约 [ARCHITECTURE.md](../../design/ARCHITECTURE.md) §5.3、§5.4、§5.4.1、§6.6、§6.7、附录 A
- 被测工具 **`aisbench-sla`**（vendored 在 [`tool/`](tool/)，来源与 commit 见 [`tool/VENDORED.md`](tool/VENDORED.md)）

## 干什么

工具的搜索策略是「预检 → 指数阶梯 → 二分收敛」：

```text
预检（并发=1）    不达标即判「阈值与能力矛盾」，退出码 2
阶梯（start ×2）  直到某点不达标或到 --max
二分（lo, hi）    收敛到 lo//8 或 --tolerance，最多 --max-bisect 次
预算              --max-probes 限制总探测点数
```

每个探测点都做三件事：把并发/输出长度写进 ais_bench 的模型配置 → 调 ais_bench 压测 →
解析汇总日志拿 TTFT / TPOT / 吞吐，并按 `--sla` 的多条约束（AND）判 PASS/FAIL。
命中率按目标服务 `/metrics` 的 `vllm:prefix_cache_*` 快照差分给出，**只作证据、不参与判定**。

本 Tester 只做「封装 + 归一化」：跑工具、把结论映射成 `result.json`（附录 A）、把工具产物留在
`/workspace/output/sla_out/`。它**不改**工具算法，也不判定「结果好不好」——达标与否由下游消费指标。

## 目录结构

```text
aisbench-sla/
├── tester.yaml            # 元数据 + 参数清单（文档语义） + 资源/并发声明
├── run.sh                 # 唯一执行入口（参数映射 + 跑工具 + 写 result.json）
├── build/Dockerfile       # 构建定义（构建上下文 = 仓库根）
├── run-local.sh           # 本地便捷脚本（只 run，不 build）
├── tool/                  # vendored 的上游 CLI（sla_search.py + sla_tool/ + tests/）
│   └── VENDORED.md        # 来源仓库、commit、版本、如何升级
├── tests/
│   ├── smoke.sh           # 容器内冒烟：mock vLLM + stub ais_bench + 断言 result.json
│   ├── smoke.ps1          # 宿主侧驱动：docker run 一次并断言（Windows）
│   └── fixtures/smoke_dataset.jsonl   # 8 行 × 512 字符（stub 估 ~128 token）
└── README.md
```

**这里没有、也不该有**：`job.yaml` / `deployment.yaml`（部署由平台渲染，§6.8）、`results/`（产物固定写
`/workspace/output`，§5.3）、凭据（只走环境变量，§12.4）。

## 参数映射：平台 → 工具

平台只做词法切分（§5.3），语义全部在 `run.sh` 里。下划线参数按工具习惯转成连字符：

| 平台参数（argv） | 工具参数 | 说明 |
|---|---|---|
| `--url` | `--target` | **公共参数**。平台给 OpenAI base URL（`.../v1`），工具要服务根地址，所以先去掉末尾 `/` 与末尾 `/v1` 再传。工具自己拼 `/v1/models`（探活）与 ais_bench 的 `url` 字段 |
| `--model_name` | `--model` | **公共参数**。服务端模型名 |
| `--model_path` | `--model-path` | **公共参数**。工具会校验该目录**存在**（ais_bench 模型配置要 tokenizer 路径，报 `UTILS-FILE-002`） |
| `--input_len` | `--input-len` | 输入 token 长度 |
| `--output_len` | `--output-len` | 输出 token 长度 |
| `--data_num` | `--data-num` | 数据集条数 / 每点请求数 |
| `--sla`（可多次） | 多个 `--sla` | `指标.统计量:阈值`，AND 生效 |
| `--start` | `--start` | 起始并发 |
| `--max` | `--max` | 并发上限 |
| `--max_probes` | `--max-probes` | 探测点预算 |
| `--tolerance` | `--tolerance` | 二分收敛容差 |
| `--max_bisect` | `--max-bisect` | 二分最大迭代 |
| `--request_rate` | `--request-rate` | RPS，0 = 不限速 |
| `--dataset_type` | `--dataset-type` | `prefix_cache` / `normal` |
| `--prefix_num` | `--prefix-num` | 前缀种类数 |
| `--repeat_rate` | `--repeat-rate` | `90%` 或 `0.9` |
| `--seed` | `--seed` | 随机种子 |
| `--dp` | `--dp` | dp 域数量 |
| `--dataset` | `--dataset` | 现成数据集 jsonl（跳过生成、跳过 tokenizer） |
| `--dataset_path` | `--dataset-path` | 生成数据集落盘目录 |
| `--work_path` | `--work-path` | ais_bench 源码根，**必须存在且可写**（工具往里写模型配置与数据集软链） |
| `--aisbench_cmd` | `--aisbench-cmd` | 替换 ais_bench 命令（可带参数） |
| `--aisbench_extra_args` | `--aisbench-extra-args` | 追加到 ais_bench 命令末尾 |
| `--point_timeout` | `--point-timeout` | 单点超时秒数（0 = 不限） |
| `--prefix_warmup` | `--prefix-warmup` | `per-point` / `once` / `none` |
| `--no_stream` | `--no-stream` | true 时不实时打印 ais_bench 输出（仍落盘） |
| 环境变量 `ENDPOINT_API_KEY` | `--api-key` | 凭据只走环境变量、不进 argv（§12.4），日志里也不打印值 |
| （Tester 固定追加） | `--output-dir <OUT>/sla_out --no-color --no-banner` | 产物目录固定、禁颜色、禁启动横幅 |

**没有映射的参数**（工具里存在但本 Tester 未建模）：`--config`、`--pods`、`--npu-num`、`--length_mean/std/min/max`、
`--gsm8k-source`、`--test-type`、`--enable-think`、`--reset-cache`、`--seed-per-point`/`--no-seed-per-point`、
`--dry-run`、`--verbose`。需要时用 `--aisbench_extra_args` 只能影响 ais_bench，不能影响本工具；这些属于后续版本。

另外三件事 `run.sh` 会替调用方做：

1. `export STUB_WORK_DIR=<work_path>`——stub_aisbench 用它定位被逐点改写的模型配置
   （真 ais_bench 不读这个变量，无害）。见 `tool/tests/stub_aisbench.py` 的 `STUB_WORK_DIR` 说明。
2. 给了 `--dataset` 就追加 `--no-seed-per-point`：工具在「每点换 seed」模式下会**忽略 `--dataset`**
   去重新生成数据集（要 transformers）。复用同一份数据集的代价是各点命中率会逐点饱和，工具会在
   `warnings` 里明说（本 Tester 把它写进 `result.json.notes`）。
3. 未传的数值参数不出现 argv 里，由工具用自己的默认值（与 `tester.yaml` 的 `default` 一致）。

## 退出码与 result.json 映射

| 场景 | Tester exit | `result.json.status` | `error.code` |
|---|---|---|---|
| 工具退出码 `0`（找到可行并发；含到上限仍达标的 `max_unbounded`） | `0` | `succeeded` | — |
| 工具退出码 `2`（预检：并发=1 就不达标） | `1` | `failed` | `SLA_PRECHECK_FAILED` |
| 工具退出码 `3`（没有任何并发点达标） | `1` | `failed` | `SLA_NO_FEASIBLE_CONCURRENCY` |
| 工具退出码 `1`（参数/执行错误，如缺 `--sla`、`--work-path` 不存在、ais_bench 起不来） | `1` | `failed` | `TESTER_ERROR` |
| 工具被信号终止 / OOM / 解释器缺失 | `1` | `failed` | `TESTER_ERROR` |
| 工具退出码 `0` 但没产出 `sla_summary.json`，或只做了 dry-run | `1` | `failed` | `TESTER_ERROR` |
| 本脚本参数错误（未知参数、缺 `--url`/`--sla`/`--work_path`、类型不对、`--help`） | `2` | `failed` | `TESTER_ERROR` |

`error.retryable` 恒为 `false`：参数错重试无用；预检/无可行并发是「阈值与服务能力矛盾」，重试同样无用
（改 SLA 约束或服务配置才有意义）。

> **和 §5.8 的取舍（重要）**：§5.8 说「测试执行成功与结果达标是两件事」，精度不达标也该 `exit 0`。
> 本 Tester 按上游工具的语义把「预检失败 / 无可行并发」当失败（`exit 1`），因为这两种情况下工具**没有**
> 产出一个可用的搜索结论（`max_ok_concurrency` 为 0/空），下游拿不到可消费的结果。如果团队要求严格按
> §5.8 上报，把 `run.sh` 里 `case "${RC}"` 的 `2)` / `3)` 两个分支改成 `STATUS=succeeded; EXIT_CODE=0`
> 即可（`error.code` 与 `notes` 仍会把工具结论写出来）。

## result.json 里的 metrics（从哪来）

全部取自 `<OUT>/sla_out/sla_summary.json`（工具自己写的摘要，字段见 `tool/sla_tool/report.py`）。
**取不到就不写**，并在 `notes` 说明缺了哪个、为什么——不编数。

| `metrics[].name` | 来源字段 | `unit` | `statistic` | `labels` |
|---|---|---|---|---|
| `sla_max_concurrency` | `max_ok_concurrency` | `count` | `count` | `{}` |
| `max_output_throughput_tok_s` | `throughput.peak.value`（最高输出吞吐；可能出现在不达标点） | `tokens_per_s` | `max` | `{concurrency}` |
| `ttft_avg_ms` | `points[]` 里 `concurrency == max_ok_concurrency` 那个点的 `ttft_avg_ms` | `ms` | `mean` | `{concurrency}` |
| `tpot_p90_ms` | 同上点的 `tpot_p90_ms` | `ms` | `p90` | `{concurrency}` |
| `hit_rate_percent` | 同上点的 `hbm_hit_rate × 100`（**有则给**：服务没暴露 `/metrics` 就没有） | `percent` | `mean` | `{concurrency}` |
| `probes_total` | `points[]` 里 `kind != "warmup"` 的条数（预热不计入预算） | `count` | `count` | `{}` |

`workload` 记录本次实际传了什么（base_url / model_name / model_path / input_len / output_len / data_num /
sla 列表 / start / max_concurrency / max_probes / request_rate / dataset_type / dataset / dataset_path /
work_path / aisbench_cmd）。`metadata` 记录工具名、工具版本（`aisbench-sla 0.7.0`）与 vendored commit。

## 产物

工具**直接**把产物写进 `<OUT>/sla_out/`（§6.8「产物直落最终路径」，平台不搬运字节）：

```text
/workspace/output/
├── result.json            # 必须（附录 A）
├── logs/sla.log           # 工具 stdout+stderr 的 tee（Tester 自己的时间线也在这里）
├── raw/                   # 自由区（当前为空，保留给 CONFIG_SNAPSHOT 之类）
└── sla_out/               # 工具原生产物
    ├── sla_summary.json   # 结论、SLA 口径、全部探测点
    ├── sla_points.csv     # 每点一行：并发/判定/统计量/吞吐/命中率/实测并发
    ├── run.log            # 工具自身日志
    ├── aisbench_all.log   # 工具消息 + 各阶段 ais_bench 原始输出（追加）
    ├── datasets/          # 生成的数据集（用 --dataset 时为空/不产生）
    └── points/c<N>/       # 每点的 warmup.log / full.log
```

`result.json.artifacts` 按附录 A 列出（`{name, path, size_bytes, sha256}`，`path` 相对 `/workspace/output`）：
`sla_out/sla_summary.json`、`sla_out/sla_points.csv`、`sla_out/run.log`、`sla_out/aisbench_all.log`、
`sla_out/points/**` 下全部文件。

**单个文件超过 50MB 时**保留末尾 50MB（日志的尾部才是现场），并在该 artifact 上加 `truncated: true`，
同时在 `notes` 里写明原大小。`datasets/**` 不进 artifacts（可能很大，且可用 `--dataset` 复用）。

## 构建与推送（只发生在 CI）

构建上下文是**仓库根**（要从 `testers/aisbench-sla/` 取 `run.sh`、`tool/`、fixtures）：

```bash
cd <repo-root>
TAG=0.1.1            # 或 0.1.1-$(git rev-parse --short HEAD)
docker build -f testers/aisbench-sla/build/Dockerfile \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:${TAG} .
docker push ghcr.io/ucm-system/testplane-tester-aisbench-sla:${TAG}
# 把镜像引用回填 tester.yaml 的 image 字段（tag 或 digest 均可）
```

真 ais_bench 模式（0.2.0）用**另一个 Dockerfile**：`testers/aisbench-sla/build/Dockerfile.aisbench`
（见下一节）。两个 Dockerfile 的构建上下文都是仓库根。

镜像内容：`python:3.11-slim` + `bash` + `ca-certificates` + `pip install requests` +
`/tester/run.sh` + `/tester/tool`（vendored）+ `/tester/fixtures/smoke_dataset.jsonl`；
非 root（uid 10001），`/tester` 与 `/workspace` 归 10001（工具要写 `<work_path>/ais_bench/...`）。

**平台、SSH 主机、本地一律只 `docker pull` + `docker run`**（§6.2）——不在目标机器上 build。

## 真 ais_bench 模式（0.2.0，`build/Dockerfile.aisbench`）

0.1.0 是 stub 模式（自带假 ais_bench，只证明链路）。0.2.0 把基镜像换成官方
`ghcr.io/aisbench/aisbench_benchmark:v3.1-20260330-master_x86_64_py_310`（6.39 GB，镜像里
`ais_bench` 是真命令、`transformers`/`datasets`/`requests` 都已装好），`run.sh` 与 `tester.yaml`
的契约零改动，只多两处「接线」（见下）。

### 构建（本地/CI 同一份）

```bash
cd <repo-root>
# 1) 只给**验证镜像**用的 tokenizer（15MB，生产镜像不带）
pwsh testers/aisbench-sla/build/prepare-tokenizer.ps1      # 从 D:\Models\Qwen3-0.6B 拷到 build/models/
TAG=0.2.0
docker build -f testers/aisbench-sla/build/Dockerfile.aisbench \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:${TAG} .
```

镜像内容：官方 ais_bench 基础镜像 + `/tester/run.sh` + `/tester/tool`（vendored，stub 也在）+
`/tester/fixtures` + `/models/Qwen3-0.6B`；非 root（10001），`/tmp`、`/models`、`/tester`、
`/workspace` 都归 10001；`HOME`/`HF_HOME` 收到 `/tmp`（非 root 没有 `/home`，否则 HF 缓存会炸）。

### tokenizer 怎么进容器：选「COPY 进镜像」（三选一里的第 1 种）

| 方案 | 结论 |
|---|---|
| ① COPY 进镜像 `/models/Qwen3-0.6B`（**选了它**） | Job 只挂产物 PVC，没有别的共享存储可挂；15MB 换来「一条 docker build 就能在 kind 里真跑」，可复现、不碰节点 |
| ② kind 节点 hostPath PV/PVC + `tester.yaml.mounts` | 更接近生产，但要在节点里放文件、依赖节点路径，且 `mounts` 是**执行 Profile** 的事，写死在 Tester 里会把某套目录布局钉住（tester.yaml 里已有这条说明） |
| ③ 塞进产物 PVC 再绝对路径挂 | 把「权重/数据集不由平台托管」（§10.4）这条破坏了，且每次 Run 得先准备 PVC |

**这只是验证镜像**：`build/models/` 在 `build/.gitignore` 里、不入库；生产镜像**不带** tokenizer，
tokenizer/权重由 Execution Profile 挂载，调用方用 `service.model_path`（公共参数）指容器内绝对路径。
为了让「带」和「不带」一眼可辨，Dockerfile 里那条 `COPY` 单独一行、带注释。

### 关键接线：work_path 走 `/tmp/aisbench-ws`，不写 `/benchmark`

官方镜像里 `/benchmark` 是 ais_bench 的源码根（root:root 0755）。而本工具要往 **work_path 下**写两样东西：
`<work_path>/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py`（模型配置）与
`<work_path>/ais_bench/datasets/gsm8k/{train,test}.jsonl`（数据集）。参考工具靠「work_path 恰好就是源码根」
让 ais_bench 找到它们——但平台 Executor 固定 `readOnlyRootFilesystem: true`（只有 `/workspace/output`
与 `/tmp` 可写），平台上不可能写 `/benchmark`。

`run.sh` 因此做了两处接线（**没有改 vendored 的 `sla_tool/`**）：

1. **数据集**：`export AIS_BENCH_DATASETS_CACHE="${WORK_PATH}"`。ais_bench 把**相对**数据集路径拼在
   这个环境变量下（`benchmark/datasets/utils/datasets.py: get_cache_dir()`，默认才是源码根），所以
   `--work_path=/tmp/aisbench-ws` 时 `path='ais_bench/datasets/gsm8k'` 正好落到工具写的位置。
2. **模型配置**：work_path 不是 `/benchmark` 时，自动给 ais_bench 追加
   `--config-dir <work_path>/ais_bench/benchmark/configs`（ais_bench 会在 `<config-dir>/models` 下找模型配置）。
   用户自己给的 `--aisbench_extra_args` 会保留，两者拼接。

另外镜像构建时把 `run.sh` 与 `tool/**/*.py` 的 CRLF 擦成 LF：Windows 检出带 CRLF 时，`stub_aisbench.py`
的 shebang 会变成 `python3\r`，直接把 stub 模式打死（本机真踩过）。

### 本地真跑（B）

```bash
# 目标可达性：sla-mock 在集群里，宿主机先做端口转发（pod IP 在 Docker Desktop 上不可直连）
kubectl --context kind-testplane -n testplane-mock port-forward --address 127.0.0.1 svc/sla-mock 18090:18080
docker run --rm --add-host host.docker.internal:host-gateway \
  -v "$PWD/.data/aisbench-real/out:/workspace/output" \
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.2.0 \
  --url=http://host.docker.internal:18090/v1 --model_name=mock-llm --model_path=/models/Qwen3-0.6B \
  --input_len=64 --output_len=4 --data_num=4 --sla=ttft.avg:100000 \
  --start=1 --max=1 --max_probes=1 --request_rate=0 \
  --work_path=/tmp/aisbench-ws --dataset_type=normal
```

真 ais_bench 结果（`result.json` metrics）：`sla_max_concurrency=1`、`max_output_throughput_tok_s=293.064`、
`ttft_avg_ms=6.6`、`tpot_p90_ms=2.3`、`probes_total=1`，`status=succeeded`；`sla_out/points/c1/**` 里有
ais_bench 自己写的 `aisbench_full.log` / `performances/vllm-api-stream-chat/{gsm8k.csv,gsm8k.json,gsm8k_details.jsonl,gsm8k_plot.html}`。

> 本地跑**不要**用 `--network container:testplane-control-plane` 去直连 Pod IP：节点 netns 里到
> `10.244.0.0/16` 的连接会挂着不回，ais_bench 卡在推理阶段；而往容器 `/etc/hosts` 里塞
> `sla-mock.testplane-mock.svc` 也没用——ais_bench 用 aiohttp，`aiodns`/c-ares **不读 `/etc/hosts`**。
> 端口转发 + `host.docker.internal` 最省事。

### 平台上真跑（C）

```yaml
run:
  name: aisbench-real-sla
  service:                      # 必须是 service，不能是 deploy（原因见下）
    url: http://sla-mock.testplane-mock.svc:18080/v1
    model_name: mock-llm
    model_path: /models/Qwen3-0.6B
  precheck: auto                # 注入 endpoint-probe（镜像要提前灌进节点，见下）
  tests:
    - name: sla-search-real
      tester: aisbench-sla
      version: "0.2.0"
      execution: { profile: k8s-npu }
      args: >-
        --input_len=64 --output_len=8 --data_num=32 --sla=ttft.avg:100000
        --start=1 --max=4 --max_probes=4 --max_bisect=0 --request_rate=0
        --work_path=/tmp/aisbench-ws --dataset_type=normal
```

跑之前两件事：

1. `precheck: auto` 会注入 `endpoint-probe`，它的镜像必须在节点里：
   ```bash
   cd testers/endpoint-probe && docker build -f build/Dockerfile -t registry.dev.huawei.com/testplane/endpoint-probe:1.0.0 .
   ```
   注意 endpoint-probe 的构建上下文是 **Tester 目录本身**（`testers/endpoint-probe`），不是仓库根；
   用仓库根做上下文会 `COPY run.sh: not found`。
2. 镜像要灌进 kind 节点（`docker save` → `docker exec -i <node> sh -c 'cat > /tmp/x.tar'` → `ctr -n k8s.io images import`）。
   **别用 `docker cp`**（本机会静默失败）。
   kind 节点里 `ctr` 的 import 会保留 `docker save` 里的 repo tag，tag 必须与 `tester.yaml.image` 逐字一致。

> **为什么这条链只有 precheck → 测试，没有 Deploy？**
> `deploy:` 与 `service:` 二选一（C-5.1 第 3 条）。本 Tester 的工具**硬要求** `--model_path`
> （`sla_tool/config.py: validate_for_run()`，缺了直接 `必须提供 --model-path`），而 `model_path` 是
> 公共参数（§5.4.1）：`service` 场景它来自 `service.model_path`，**deploy 场景的 Endpoint 只有
> BaseURL/ModelName，平台不从 deploy.parameters 推 model_path**，args 里又不许写公共参数。
> 所以 deploy 模式的 Run 一定跑不起来（本机实测：deploy 走假 Genet 还会卡在
> `release deployed but callInfo.endpoint is still pending`）。要让 aisbench-sla 也能 deploy，
> 得平台侧把 deploy 的 model_path 传下来（或 Tester 收一个非公共参数如 `--tokenizer_path`）。
> **precheck 也一样是「有没有」的问题**：之前那次 Run 没有 precheck，是因为 manifest 写了
> `precheck: "off"`，而本机 kind 当时没有 endpoint-probe 镜像。

### 日志去哪了（D）

平台日志框读的是 **attempt 目录下的 `logs/stdout.log`**（容器 stdout/stderr 全量落盘），
ais_bench 的逐点产物在 `sla_out/`。实测一次 3 点真跑（`--data_num=32 --max=4 --max_probes=4`）：

| 文件 | 字节 | 行数 |
|---|---|---|
| `logs/stdout.log`（平台日志框的来源） | 51 833 | 380 |
| `logs/sla.log`（同一份内容 + Tester 自己的 log 行） | 52 270 | 380 |
| `sla_out/aisbench_all.log`（工具消息 + 各点 ais_bench 输出，追加） | 50 334 | 360 |
| `sla_out/points/c1/full.log`（单点的 ais_bench 输出） | 15 060 | 106 |

结论：

* 「平台日志框只装容器 stdout」这句**对**，但**不等于看不到 ais_bench 日志**：`run.sh` 默认把 ais_bench
  输出实时 tee 到 stdout（`--no_stream=false`），所以 ais_bench 的逐点日志**基本都在**日志框里
  （51 833 B 里 50 334 B 是 ais_bench 原文）。真正只在产物里的，是逐点的结构化结果与附件：
  `points/c*/<exp>/aisbench_full.log`、`configs/*.py`、`performances/**/{gsm8k.csv,gsm8k.json,gsm8k_details.jsonl,gsm8k_plot.html,db_data/*.db}`
  —— 同一次 Run 的产物合计 512 763 B，是日志框那份的 ~10 倍。
* 日志 API（`GET /api/v1/tasks/{id}/attempts/{idx}/logs`）**没有 `truncated`/`next_offset` 字段**：
  它按 `offset`（字节）分页，响应头 `X-Log-Offset` 给出「本次读到哪」，`limit` 默认/上限 **64 KiB**
  （`maxLogChunk = 64<<10`），另有 `tail=N` 取最后 N 行。本次 `limit=65536` 一次取回全量
  51 833 B（`X-Log-Offset: 51833`）；`limit=4096` 只回 4096 B（`X-Log-Offset: 4096`），UI 的
  `useLogTail` 就是这么续读的。所以**只有当 stdout 超过 64 KiB 才需要翻页**（本次 ~51 KiB，
  再多一两个探测点就会超）。
* 想让「日志框里就有结论」的话，当前默认（`--stream` 开 + 结尾打印汇总表 + `sla_out` 全量入 artifacts）
  已经够；不建议再 `tail aisbench_all.log`——那会把同一批行打第二遍，且长跑时白占日志框额度。


## 离线冒烟（不需要 NPU / 权重 / 真 ais_bench）

`tool/tests/` 自带两个离线夹具：`mock_vllm.py`（带 `/metrics` 与 `/__sim__/traffic` 的假 vLLM）与
`stub_aisbench.py`（读模型配置、按 `ttft_p90 = 100 + 20×c` 打印 ais_bench 风格汇总的假 ais_bench）。
`tests/smoke.sh` 在容器里把它们串起来跑一次真实的 `/tester/run.sh`，然后断言 `result.json`：

```powershell
# Windows（宿主侧驱动，负责 docker run + 断言）
pwsh testers/aisbench-sla/tests/smoke.ps1
#   产物默认落到 .data\sla-tester\smoke\
```

```bash
# Linux/macOS 等价：
docker run --rm --entrypoint bash \
  -v "$PWD/.data/sla-tester/smoke:/workspace/output" \
  -v "$PWD/testers/aisbench-sla/tests:/tests:ro" \
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.1.0 /tests/smoke.sh
```

冒烟用的那串参数（与 `tests/smoke.*` 里一致）：

```text
--url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/tester/tool
--input_len=128 --output_len=8 --data_num=8 --sla=ttft.avg:100000
--start=1 --max=4 --max_probes=2
--dataset=/tester/fixtures/smoke_dataset.jsonl
--aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tester/tool
```

> `--model_path` 指 `/tester/tool`（镜像里存在的目录）而不是 `/weights/x`：工具会校验该目录存在
> （ais_bench 模型配置需要 tokenizer）。用 `--dataset` 时它不会被真正读取，但校验仍会跑。
> `--dataset` 是必需的：本镜像不装 `transformers`，不能现场生成数据集（见「已知限制」）。

## 平台上提交（Run）

`url` / `model_name` / `model_path` 是公共参数，**由平台注入**（`service:` 块或 Genet 部署），
`tests[].args` 里**不要**再写（同名会按 §5.4.1 拒绝提交）：

```yaml
run:
  name: sla-smoke-20261106
  service:
    url: http://sla-mock.testplane-mock.svc:18080/v1
    model_name: mock-llm
    model_path: /tester/tool
  cleanup: { policy: never }
  tests:
    - name: sla-search-stub
      tester: aisbench-sla
      version: "0.1.0"
      execution: { profile: k8s-npu }
      args: >-
        --input_len=128 --output_len=8 --data_num=8 --sla=ttft.avg:100000
        --start=1 --max=4 --max_probes=2
        --dataset=/tester/fixtures/smoke_dataset.jsonl
        --aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tmp/aisbench-ws
```

> 平台上 **`--work_path` 要用 `/tmp/...`**：Executor 把根文件系统设成只读（`ReadOnlyRootFilesystem`），
> 只有 `/workspace/output` 与 `/tmp`（emptyDir）可写，而工具要往 work_path 里写模型配置与数据集软链。
> `run.sh` 在 work_path 不存在时会 `mkdir -p`，所以 `/tmp/aisbench-ws` 可直接用。
> 离线冒烟（`docker run`，根文件系统可写）用 `/tester/tool`，两者只差这一个路径。

真实（非 stub）用法只需换掉 `--dataset`/`--aisbench_cmd`/`--work_path` 并给真 SLA 约束，
其余不变。

## 独立运行（脱离平台）

与平台里完全同构（§5.10）：只给 argv + 环境变量就能跑出同一份 `result.json`。

```bash
docker run --rm --add-host=host.docker.internal:host-gateway \
  -e TESTPLANE_RUN_ID=local \
  -v /tmp/sla-out:/workspace/output \
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.2.0 \
  --url=http://host.docker.internal:8000/v1 --model_name=Qwen3-32B \
  --model_path=/models/Qwen3-0.6B \
  --input_len=2048 --output_len=32 --data_num=160 \
  --sla=ttft.avg:1200 --sla=tpot.avg:30 --start=8 --max=256 --max_probes=12 \
  --work_path=/tmp/aisbench-ws --dataset_type=prefix_cache
```

或 `IMAGE=... ./run-local.sh <同样的参数>`（只 run，不 build）。

（0.2.0 里 `ais_bench` 就是真命令，不用再写 `--aisbench_cmd`；`--model_path` 换成你挂进来的 tokenizer 目录即可。）

## 已知限制

1. **`build/Dockerfile`（0.1.x，stub 镜像）不含 ais_bench**：它只证明封装与契约。
   真跑用 `build/Dockerfile.aisbench`（0.2.0，官方 ais_bench 基镜像），见上面「真 ais_bench 模式」。
   0.1.1 仍保留为「小镜像」用途：本机 kind 里的 `sla-mock` Deployment 就是用它跑
   `tool/tests/mock_vllm.py`（6.39 GB 的 0.2.0 镜像不适合跑一个 mock）。
2. **stub 镜像不能现场生成数据集**：0.2.0 的基镜像自带 `transformers`，可以
   （`--dataset_type=normal|prefix_cache` + `--model_path` 指 tokenizer 目录即可）；0.1.x 仍需
   `--dataset`。给了 `--dataset` 时工具跳过生成，但**仍会校验 `--model_path` 目录存在**。
   注意工具在 `--seed-per-point`（默认）下**会忽略 `--dataset`** 去重新生成——所以 `run.sh` 在 `--dataset`
   存在时自动加 `--no-seed-per-point`，各点复用同一份数据集（命中率会逐点饱和，工具会告警）。
3. **`hit_rate_percent` 依赖目标服务的 `/metrics`**：只有暴露 `vllm:prefix_cache_queries_total` /
   `vllm:prefix_cache_hits_total` 才有；没有（或抓取失败）就不写该指标，`notes` 里写明原因。
4. **`--work_path` 必须可写，且平台上要落在 `/tmp`**：工具要写
   `<work_path>/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py` 与数据集；
   平台 Executor 固定 `readOnlyRootFilesystem: true`，只有 `/workspace/output` 与 `/tmp` 可写。
   0.2.0 起 `run.sh` 用 `AIS_BENCH_DATASETS_CACHE` + `--config-dir` 把 `/tmp/aisbench-ws` 接给 ais_bench
   （见「真 ais_bench 模式」），**不要再写 `/benchmark`**：那里属 root 且平台只读。
5. **平台的 `--model_path` 不校验存在性**（§10.4 路径对平台不透明），而工具会校验；路径写错时的表现是
   `exit 1` + `TESTER_ERROR`，`notes` 里能看到工具的原始报错。
6. **`stub_aisbench.py` 只能证明链路，不能当结论**：它的 TTFT/TPOT 是确定性公式（`100 + 20×c` /
   `10 + 0.5×c`），与真实服务无关。真实结论必须用真 ais_bench + 真服务。
7. **未建模的工具开关**见「参数映射」末段；当前版本不做 dict 化透传（避免把未声明的参数悄悄带进平台语义）。
8. `run.sh` 用 `STUB_WORK_DIR` 让 stub 找到逐点改写的模型配置；真 ais_bench 不读该变量，但如果你换的是
   另一个自定义 `--aisbench_cmd`，它可能也需要类似的工作区提示（工具本身通过 `--work-dir` 传运行目录）。

## 维护者验证清单

```bash
# 1) 注册表校验（tester.yaml + run.sh）
cd server && go test ./internal/registry -run TestParse -v    # 或在仓库根跑 go test ./server/internal/registry
# 2) vendored 工具自身没坏
py -3.11 testers/aisbench-sla/tool/tests/test_cli_surface.py
py -3.11 testers/aisbench-sla/tool/tests/test_offline.py
# 3) 镜像离线冒烟
docker build -f testers/aisbench-sla/build/Dockerfile -t aisbench-sla:t1 .
pwsh testers/aisbench-sla/tests/smoke.ps1 -Image aisbench-sla:t1
```

## 本机验证记录（2026-11-06）

| 项 | 命令 | 结果 |
|---|---|---|
| 注册表（含本目录） | `go test ./internal/registry` + 用 `-overlay` 对真实 `testers/` 跑 `Scan` | `added=[aisbench-sla@0.1.0 endpoint-probe@1.0.0]`，`params=26` |
| vendored CLI | `py -3.11 tool/tests/test_cli_surface.py` | `PASS (34/34 OK)` |
| vendored 离线 e2e | `py -3.11 tool/tests/test_offline.py` | `Ran 17 tests ... OK`（含 mock+stub 全链路） |
| 容器冒烟 | `pwsh tests/smoke.ps1` | `status=succeeded`，6 个 metrics，退出码 0 |
| 退出码矩阵 | 容器内 4 个分支各跑一次 | `0/succeeded`、`2+1/SLA_PRECHECK_FAILED`、`3+1/SLA_NO_FEASIBLE_CONCURRENCY`、参数错 `2/TESTER_ERROR` |
| 平台真跑 | `POST /api/v1/runs`（stub 模式，`service` 指向集群内 mock） | Run `r-20261106-ZC7R` `succeeded`；`/report` 的 `result_ref` 非空；attempt result 的 metrics 与 `sla_points.csv`/`sla_summary.json` 都能从 `/bundle` 下到 |
| **平台真跑（真 ais_bench 0.2.0）** | `POST /api/v1/runs`，`precheck: auto`，`service.url=http://sla-mock.testplane-mock.svc:18080/v1` | Run `r-20261106-MWMY`（1 点）/ `r-20261106-9DTC` / `r-20261106-938F`（后两次 3 点：c=1,2,4）均 `succeeded`；链路 `precheck(succeeded) → sla-search-real(succeeded)`；metrics 来自真 ais_bench（938F：`sla_max_concurrency=4`、`max_output_throughput_tok_s=1253.612`、`ttft_avg_ms=1.8`、`tpot_p90_ms=2.3`、`probes_total=3`；c=4 那点 ais_bench 原始表 TTFT avg 1.8 / TPOT avg 2.2 / E2EL avg 17.2 ms、Total Generated Tokens 256）；artifacts 含 `sla_out/points/c{1,2,4}/<exp>/aisbench_full.log` |
| 日志体积（同上 Run `9DTC`） | PVC 上 `wc -c` / `wc -l` + logs API | `logs/stdout.log` 51 833 B / 380 行；`sla_out/aisbench_all.log` 50 334 B / 360 行；`points/c1/full.log` 15 060 B（c2/c4 各 ~15.2 KB）；产物合计 512 763 B；logs API `limit=65536` 一次取回全量（`X-Log-Offset: 51833`），无 `truncated`/`next_offset` |
| stub 回归（0.2.0 镜像内） | `--aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tester/tool` | `status=succeeded`，`sla_max_concurrency=2`、`最高输出吞吐 56.7 tok/s`（stub 公式值，仅证明链路） |

平台真跑用的 mock 是 vendored 的 `tool/tests/mock_vllm.py`（集群内一个 Deployment + Service），
不是仓库里的 `tools/mock-llm`——后者没有 `/metrics`，算不出命中率。夹具清单没有进仓库（放在被
gitignore 的 `.data/` 下）。该 Run 用 `precheck: "off"`：本机 kind 里只灌了本 Tester 的镜像，
`precheck: auto` 会注入 endpoint-probe 并卡在 `IMAGE_PULL_FAILED`。
