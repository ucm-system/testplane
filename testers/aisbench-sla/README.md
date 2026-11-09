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

## ⚠️ 硬警告：并发点可能根本没被压满

这不是「调优建议」，是**结论有效性**的前提，配置错了会得到一个看起来合理、其实什么也没测出来的数。

**`request_rate` × 时延 < 目标并发时，该并发点是无效的。**

在飞请求数由「到达速率 × 服务时延」封顶。比如 `--request_rate=3`（默认）配 500 ms 时延，
在飞并发最多约 1.5：此时 `--max=256` 的阶梯里，`c=2/4/8/…` 的**实测并发都会停在 1~2**，
你测到的是「这个频率下服务什么样」，**不是**「服务能扛几并发」。

0.3.0 及以前的实测证据里就有这个形态（两个并发点除 `concurrency` 列外逐字节相同，
`measured_concurrency` 恒为 1）——那种 `sla_max_concurrency` 是配置给的，不是测出来的。

0.4.0 起本 Tester 会**拒绝**这种结论：PASS 点的 `measured_concurrency / concurrency` 必须
≥ 0.8（`MEASURED_CONCURRENCY_MIN_RATIO`，`run.sh` 与 `tool/sla_tool/search.py` 同一阈值），
否则该点标 `degraded`（写进 `sla_points.csv` 与 `result.json.notes`）、**不参与**
`sla_max_concurrency`；所有点都降级时 `sla_max_concurrency` **不输出**，`notes` 直接写
「并发未被真正测出，结论不可用」。

**推荐值（要测并发上限就照这个配）**：

```text
--request_rate=0                 # 不限速，让并发只由服务能力决定
--data_num >= 4 × --max          # 每个点都要有足够的请求把并发压满（如 --max=16 时给 64+）
--start=1 --max=<你怀疑的上限×2>  # 阶梯 ×2；别一上来就 256 浪费预算
--max_probes >= 8                # 阶梯 + 二分至少需要这么多点
```

（工具自带的 `--help` 对 `--data-num` 写的是「2~4 倍」；本 Tester 按 **≥4 倍** 给建议——
二分探测点会消耗额外请求，2 倍在边界点常常压不满。）

只想测「某个固定频率下的延迟」时才用非 0 的 `request_rate`——那时请把结论当频率场景看，
不要当并发上限。

## 目录结构

```text
aisbench-sla/
├── tester.yaml            # 元数据 + 参数清单（文档语义） + 资源/并发声明
├── run.sh                 # 唯一执行入口（参数映射 + 跑工具 + 写 result.json）
├── build/Dockerfile       # **唯一发布镜像**（FROM 官方 ais_bench 基础镜像，上下文 = Tester 目录）
├── run-local.sh           # 本地便捷脚本（只 run，不 build）
├── tool/                  # vendored 的上游 CLI（sla_search.py + sla_tool/ + tests/）
│   └── VENDORED.md        # 来源仓库、commit、版本、**本地改动**、如何升级
├── tests/
│   ├── smoke.sh           # 容器内冒烟（**唯一入口**）：成功路径 + 退出码 2/3 + 凭据零泄漏
│   ├── Dockerfile.stub    # 离线冒烟用的 stub 镜像（python slim + tool/tests/stub_aisbench.py）
│   ├── test_result_mapping.py  # summary fixture → result.json 映射（降级/正常/全降级）
│   ├── validate_result_schema.py  # result.json 按 openapi Result schema 校验（含 ErrorCode）
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
| `--work_path` | `--work-path` | ais_bench 的工作根，**必须存在且可写**（工具往里写模型配置与数据集软链）。平台上必须落在 `/tmp` 下；不可写时直接 failed + `TESTER_NONZERO_EXIT` |
| `--aisbench_cmd` | `--aisbench-cmd` | 替换 ais_bench 命令（可带参数） |
| `--aisbench_extra_args` | `--aisbench-extra-args` | 追加到 ais_bench 命令末尾 |
| `--point_timeout` | `--point-timeout` | 单点超时秒数（0 = 不限） |
| `--prefix_warmup` | `--prefix-warmup` | `per-point` / `once` / `none` |
| `--no_stream` | `--no-stream` | true 时不实时打印 ais_bench 输出（仍落盘） |
| 环境变量 `ENDPOINT_API_KEY` | **工具直接读同名环境变量**（`sla_tool/config.py` 的 `normalize()`） | 凭据**只走环境变量**（§12.4）：`run.sh` 不把它拼进 argv（容器里 `ps` 看不到），日志只出现 `--api-key ***` 这种打码形态；工具把 ais_bench 模型配置写成读该变量的表达式，**明文 key 不落盘**，退出时临时配置还会被删掉 |
| （Tester 固定追加） | `--output-dir <OUT>/sla_out --no-color --no-banner` | 产物目录固定、禁颜色、禁启动横幅 |

**没有映射的参数**（工具里存在但本 Tester 未建模）：`--config`、`--pods`、`--npu-num`、`--length_mean/std/min/max`、
`--gsm8k-source`、`--test-type`、`--enable-think`、`--reset-cache`、`--seed-per-point`/`--no-seed-per-point`、
`--dry-run`、`--verbose`。需要时用 `--aisbench_extra_args` 只能影响 ais_bench，不能影响本工具；这些属于后续版本。

另外四件事 `run.sh` 会替调用方做：

1. `export STUB_WORK_DIR=<work_path>`——stub_aisbench 用它定位被逐点改写的模型配置
   （真 ais_bench 不读这个变量，无害）。见 `tool/tests/stub_aisbench.py` 的 `STUB_WORK_DIR` 说明。
2. 给了 `--dataset` 就追加 `--no-seed-per-point`：工具在「每点换 seed」模式下会**忽略 `--dataset`**
   去重新生成数据集（要 transformers）。复用同一份数据集的代价是各点命中率会逐点饱和，工具会在
   `warnings` 里明说（本 Tester 把它写进 `result.json.notes`）。
3. 未传的数值参数不出现 argv 里，由工具用自己的默认值（与 `tester.yaml` 的 `default` 一致）。
4. **开跑前清空 `<OUT>/sla_out`**、退出时删掉 `<work_path>/ais_bench/…/vllm_api_chat_temp.py`
   （生成的临时模型配置，本身不含凭据）——本地复用同一个输出目录时不会读到上次的残留。

## 退出码与 result.json 映射

| 场景 | Tester exit | `result.json.status` | `error.code` |
|---|---|---|---|
| 工具退出码 `0`（找到可行并发；含到上限仍达标的 `max_unbounded`） | `0` | `succeeded` | — |
| 工具退出码 `2`（预检：并发=1 就不达标） | `1` | `failed` | `SLA_PRECHECK_FAILED` |
| 工具退出码 `3`（没有任何并发点达标） | `1` | `failed` | `SLA_NO_FEASIBLE_CONCURRENCY` |
| 工具退出码 `1`（参数/执行错误，如缺 `--sla`、`--work-path` 不存在、ais_bench 起不来） | `1` | `failed` | `TESTER_NONZERO_EXIT` |
| 工具被信号终止 / OOM / 解释器缺失 | `1` | `failed` | `TESTER_NONZERO_EXIT` |
| 工具退出码 `0` 但 `sla_summary.json` **缺失/损坏/空/没有 points**，或只做了 dry-run | `1` | `failed` | `RESULT_INVALID` |
| `--work_path` 不存在且建不了、或存在但不可写 | `1` | `failed` | `TESTER_NONZERO_EXIT` |
| 本脚本参数错误（未知参数、缺 `--url`/`--sla`/`--work_path`、类型不对、`--help`） | `2` | `failed` | `VALIDATION_ERROR` |

> 「摘要损坏」这一行不是理论情况：以前只检查文件是否存在，`sla_summary.json` 被截断/写成空文件时
> 会记成 `succeeded` + 0 指标，下游拿到一个「成功但没有结论」的 Run。0.4.0 起 `run.sh` 会真正
> **解析**摘要（能 parse、含 `status`/`max_ok_concurrency`/`sla`/`tool`、`points` 非空、不是 dry-run），
> 不满足就 failed。

> **错误码只能是平台码表里有的**（`server/internal/domain/codes.go` + openapi `ErrorCode`）。
> 自造一个码不会报错，而是被平台**静默降级成 `TESTER_NONZERO_EXIT`**、丢掉区分信息——评审 #62 就是
> 这么发现 `SLA_*` 两个码缺失的。0.4.0 因此把早先的 `TESTER_ERROR`（不在码表里）换成
> `TESTER_NONZERO_EXIT` / `RESULT_INVALID` / `VALIDATION_ERROR`。
> `result.json` 现在可以用 `py -3.11 tests/validate_result_schema.py` 按 openapi `Result` schema 校验
> （含 `ErrorCode` enum 与 `ResultMetric`；缺 `pyyaml`/`jsonschema` 时它会跳过）。

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

指标名只放**指标**：统计量与单位走 `statistic` / `unit`，维度走 `labels`（附录 A、`openapi.yaml`
的 `ResultMetric`：「`name` 不得编码维度」）。这样同名指标跨 Run 才能比较。

| `metrics[].name` | 来源字段 | `unit` | `statistic` | `labels` |
|---|---|---|---|---|
| `sla_max_concurrency` | **实测并发达标**（实测/目标 ≥ 0.8）的 PASS 点里最大的 `concurrency` | `count` | `count` | `{"scope":"search"}` |
| `output_throughput` | `throughput.peak.value`（最高输出吞吐；可能出现在不达标的点） | `tokens_per_s` | `max` | `{concurrency}` |
| `ttft` | 选中点（上表那个并发点）的 `ttft_avg_ms` | `ms` | `mean` | `{concurrency}` |
| `tpot` | 同上点的 `tpot_p90_ms` | `ms` | `p90` | `{concurrency}` |
| `hit_rate` | 同上点的 `hbm_hit_rate × 100`（**有则给**：服务没暴露 `/metrics` 就没有） | `percent` | `mean` | `{concurrency}` |
| `probes_total` | `points[]` 里 `kind != "warmup"` 的条数（预热不计入预算） | `count` | `count` | `{"scope":"search"}` |

0.3.0 → 0.4.0 的改名对照（旧名字把统计量/单位编码进了名字，已废弃）：

| 旧名（≤0.3.0） | 新名（0.4.0） | unit / statistic |
|---|---|---|
| `sla_max_concurrency` | `sla_max_concurrency`（+ `labels.scope`） | `count` / `count` |
| `max_output_throughput_tok_s` | `output_throughput` | `tokens_per_s` / `max` |
| `ttft_avg_ms` | `ttft` | `ms` / `mean` |
| `tpot_p90_ms` | `tpot` | `ms` / `p90` |
| `hit_rate_percent` | `hit_rate` | `percent` / `mean` |
| `probes_total` | `probes_total`（+ `labels.scope`） | `count` / `count` |

**`sla_max_concurrency` 的口径（0.4.0 起）**：只认「PASS 且实测并发 ≥ 目标并发 × 0.8」的点。
压不满的点在 `sla_points.csv` 里有旁注（`degraded=true` + `degraded_reason` +
`measured_concurrency_ratio`），在 `result.json.notes` 里也会列出来；这些点**不参与**取值。
如果所有 PASS 点都降级，`sla_max_concurrency` 直接**不输出**，`notes` 写「并发未被真正测出，
本次结论不可用」——宁可不给数，也不给一个配置凑出来的数。

`workload` 记录本次实际传了什么（base_url / model_name / model_path / input_len / output_len /
data_num / sla 列表 / start / max_concurrency / **concurrency**（结论落在哪个并发；没测出来时是
配置上限）/ max_probes / request_rate / dataset_type / dataset / dataset_path / work_path /
aisbench_cmd）。`metadata` 记录工具名、工具版本（`aisbench-sla 0.7.0`）、vendored commit 与
「有本地改动」的说明。

> `tester.image_ref` 恒为 `"unknown"` 是**预期**：平台目前不注入 `TESTPLANE_TESTER_IMAGE`
> （`executor/render.go` 没给 Job 设这个变量）。离开平台跑（`run-local.sh` / `docker run`）时
> 可自己设该环境变量让溯源信息完整。

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

**构建上下文是 Tester 目录本身**（`testers/aisbench-sla`），不是仓库根。这是**发版路径**：
`.github/workflows/release.yml` 的矩阵给每个 Tester 的 `context` 就是 `testers/<name>`、
dockerfile 是 `testers/<name>/build/Dockerfile`（与 `endpoint-probe` 同一约定）。Dockerfile
的 `COPY` 全部相对这个目录：

```bash
# 发版路径（= release.yml 矩阵的 context）
docker build -f testers/aisbench-sla/build/Dockerfile \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0 \
  testers/aisbench-sla
docker push ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0
# 把镜像引用回填 tester.yaml 的 image 字段（tag 或 digest 均可）
```

> **不要再用仓库根做 context**：0.3.0 的 Dockerfile 写的是 `COPY testers/aisbench-sla/run.sh`，
> 配合矩阵给的 `context: testers/aisbench-sla` 会去找
> `testers/aisbench-sla/testers/aisbench-sla/run.sh` → 发版时构建失败（评审 #62 硬问题 3；
> PR 的 CI 只构建 server/web，所以一直绿着）。0.4.0 已改成相对 Tester 目录。

**只有一种发布镜像**（用户 2026-11-09 拍板）：`build/Dockerfile` 就是真镜像，基础镜像为
`ghcr.io/aisbench/aisbench_benchmark:v3.1-20260330-master_x86_64_py_310`（6.39 GB，镜像里
`ais_bench` 是真命令）。曾经的 `Dockerfile.aisbench` 已删除——它和 `build/Dockerfile` 并存时，
发布矩阵只构建后者，于是**发出去的是 stub 假镜像**，真镜像反而因为依赖本地 tokenizer 构建不出来。

| 产物 | 来自 | 用途 |
|---|---|---|
| `:<version>` | `build/Dockerfile` | **唯一发布镜像**：真 `ais_bench`，生产/真实 SLA 结论用它 |
| `:<version>-stub`（自建，不进矩阵） | `tests/Dockerfile.stub` | 离线冒烟：python slim + `tool/tests/stub_aisbench.py`，不需要 6.4 GB 基镜像 |

发版矩阵只扫 `testers/<name>/build/Dockerfile*`，所以 `tests/Dockerfile.stub` **不会**进发布
（这是有意的：stub 只证明入口与结果映射，不证明模型能力）。

镜像内容：官方 ais_bench 基础镜像 + `/tester/run.sh` + `/tester/tool`（vendored）+ `/tester/fixtures`；
非 root（uid 10001），`/tmp`、`/models`、`/tester`、`/workspace` 都归 10001；
`HOME`/`HF_HOME` 收到 `/tmp`（非 root 没有 `/home`，否则 HF 缓存会炸）。

> **镜像不带 tokenizer / 数据集 / 权重，这是设计不是缺陷**：模型与 tokenizer 由平台统一共享挂载
> （D-63）提供，默认挂到 `/models`（必要时还有 `/mnt/model`）；`--model_path` 只是**容器内路径**，
> run.sh **原样透传**给 `ais_bench`，不做存在性预检、不猜 fallback、不提示"请先准备 tokenizer"。

**平台、SSH 主机、本地一律只 `docker pull` + `docker run`**（§6.2）——不在目标机器上 build。

## 构建与运行真镜像（唯一发布镜像）

```bash
# 构建（context = Tester 目录，与发版矩阵一致）
docker build -f testers/aisbench-sla/build/Dockerfile \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:<version> \
  testers/aisbench-sla

# 关键断言：镜像里真的有 ais_bench，且跑在 uid 10001
docker run --rm <img> bash -lc 'command -v ais_bench && id'
```

### 运行前提：`/models` 由平台挂载（D-63），不是 Tester 的职责

- 平台级 `sharedMounts`（Helm values → 平台配置）会把共享 PVC 挂进**所有** Tester Pod，
  默认路径 `/models` 与 `/mnt/model`，默认只读；
- 调用方（或平台注入的公共参数）给的 `--model_path` 就是**容器内路径**，约定形如
  `/models/<模型名>`；Tester 只把它透传给 `ais_bench`；
- 路径不存在时由 `ais_bench` 自己报错（`UTILS-FILE-002` 之类），**Tester 不做预检**；
- 镜像里没有 `/models/*` 的任何内容——平台不替 Tester 分发模型（§10.4 路径不透明）。
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
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0 \
  --url=http://host.docker.internal:18090/v1 --model_name=mock-llm --model_path=/models/Qwen3-0.6B \
  --input_len=64 --output_len=4 --data_num=4 --sla=ttft.avg:100000 \
  --start=1 --max=1 --max_probes=1 --request_rate=0 \
  --work_path=/tmp/aisbench-ws --dataset_type=normal
```

真 ais_bench 的产出形态：`sla_max_concurrency=1`、`output_throughput`（旧名
`max_output_throughput_tok_s`）、`ttft` / `tpot`、`probes_total=1`，`status=succeeded`；
`sla_out/points/c1/**` 里有 ais_bench 自己写的 `aisbench_full.log` /
`performances/vllm-api-stream-chat/{gsm8k.csv,gsm8k.json,gsm8k_details.jsonl,gsm8k_plot.html}`。

> ⚠️ 本 README 的「真实 Run」证据**只引用同一次 Run `r-20261106-938F`**（见文末「本机验证记录」）。
> 那次跑的是 **mock 服务（`tool/tests/mock_vllm.py`），不是真模型**——它证明的是链路与产物形态，
> 不是任何真实模型的性能数字。数字来自 0.2.0 镜像，指标名按当时的旧口径，0.4.0 改名后同名指标换新名。

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
      version: "0.4.0"
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
`tests/smoke.sh` 在容器里把它们串起来，跑**四次**真实的 `/tester/run.sh` 并断言：

| 用例 | 构造方式 | 断言 |
|---|---|---|
| 成功路径 | `--request_rate=0`，`start=1 max=4` | 退出码 0、`status=succeeded`、六项指标的 name/unit/statistic/labels、artifacts 齐全、`sla_max_concurrency == max_ok_concurrency`（没有降级点） |
| 并发压不满 → 降级 | `--request_rate=0.05`（在飞量被频率卡在 1） | 退出码 0 但 `sla_max_concurrency < max_ok_concurrency`、`notes` 有「并发点降级」、`sla_points.csv` 里有 `degraded=true` 的行 |
| 退出码 2 | `--start=2` + `STUB_FAIL_AT=1`（预检点失败） | Tester 退出码 1、`error.code=SLA_PRECHECK_FAILED`、`detail.tool_status=preflight_failed` |
| 退出码 3 | `--start=1` + `STUB_FAIL_AT=1`（首个探测点失败） | Tester 退出码 1、`error.code=SLA_NO_FEASIBLE_CONCURRENCY`、`detail.tool_status=none_passed` |

另外整轮用 `ENDPOINT_API_KEY=sk-test-SECRET123` 跑，最后 `grep -r` 产物目录与 `work_path`：
**必须 0 命中**（凭据零泄漏，评审 #62 硬问题 1 的常驻红证）。

```bash
# 唯一冒烟入口（**只有 shell，没有 ps1**；Windows 用 WSL 跑同一份）：
bash testers/aisbench-sla/tests/smoke.sh
#   产物默认落到 .data/sla-tester/smoke/（含 cases/exit2、cases/exit3、cases/degraded）
```

```bash
# 在 stub 镜像里跑（发布镜像不带 stub，所以冒烟用 tests/Dockerfile.stub 构建的镜像）：
docker build -f testers/aisbench-sla/tests/Dockerfile.stub \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:stub testers/aisbench-sla
docker run --rm --entrypoint bash \
  -v "$PWD/.data/sla-tester/smoke:/workspace/output" \
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:stub /tester/tests/smoke.sh
```

冒烟用的那串参数（与 `tests/smoke.sh` 里一致）：

```text
--url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/models/Qwen3-0.6B
--input_len=128 --output_len=8 --data_num=8 --sla=ttft.avg:100000
--start=1 --max=4 --max_probes=2 --request_rate=0
--dataset=/tester/fixtures/smoke_dataset.jsonl
--aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tmp/aisbench-ws
```

> `--model_path` 是**容器内路径**，由平台统一共享挂载（D-63，默认 `/models/<模型名>`）提供；
> Tester 只透传、**不校验存在性**。stub 镜像里没有真的 `/models` 内容（stub 不读权重），
> 所以 stub 冒烟直接用约定的 `/models/Qwen3-0.6B` 字符串即可。
> `--dataset` 在 stub 镜像里是必需的：stub 不装 `transformers`，不能现场生成数据集（见「已知限制」）。

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
      version: "0.4.0"
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
  ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0 \
  --url=http://host.docker.internal:8000/v1 --model_name=Qwen3-32B \
  --model_path=/models/Qwen3-0.6B \
  --input_len=2048 --output_len=32 --data_num=160 \
  --sla=ttft.avg:1200 --sla=tpot.avg:30 --start=8 --max=256 --max_probes=12 \
  --request_rate=0 \
  --work_path=/tmp/aisbench-ws --dataset_type=prefix_cache
```

或 `IMAGE=... ./run-local.sh <同样的参数>`（只 run，不 build）。

（`ais_bench` 就是镜像里的真命令，不用再写 `--aisbench_cmd`；`--model_path` 写平台挂进来的
容器内路径即可，默认形如 `/models/<模型名>`。要凭据就 `-e ENDPOINT_API_KEY=...`——它不会进 argv、不会落日志。）

## 已知限制

1. **`build/Dockerfile` 是唯一发布镜像（真 `ais_bench`）**；stub 镜像只是 `tests/Dockerfile.stub` 的产物（不进发布）。
   stub 仍有用：本机 kind 里的 `sla-mock` Deployment 就是用它跑 `tool/tests/mock_vllm.py`。

   （6.39 GB 的真镜像不适合跑一个 mock）
2. **stub 镜像不能现场生成数据集**：真镜像自带 `transformers`，可以
   （`--dataset_type=normal|prefix_cache` + `--model_path` 指 tokenizer 目录即可）；stub 镜像仍需
   `--dataset`。给了 `--dataset` 时工具跳过生成；`--model_path` **不做存在性校验**（路径由平台统一挂载提供）。
   注意工具在 `--seed-per-point`（默认）下**会忽略 `--dataset`** 去重新生成——所以 `run.sh` 在 `--dataset`
   存在时自动加 `--no-seed-per-point`，各点复用同一份数据集（命中率会逐点饱和，工具会告警）。
3. **`hit_rate` 依赖目标服务的 `/metrics`**：只有暴露 `vllm:prefix_cache_queries_total` /
   `vllm:prefix_cache_hits_total` 才有；没有（或抓取失败）就不写该指标，`notes` 里写明原因。
4. **`--work_path` 必须可写，且平台上要落在 `/tmp`**：工具要写
   `<work_path>/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py` 与数据集；
   平台 Executor 固定 `readOnlyRootFilesystem: true`，只有 `/workspace/output` 与 `/tmp` 可写。
   写不了时 0.4.0 **直接 failed + TESTER_NONZERO_EXIT**（不再打一行 log 硬着头皮跑）。
   0.2.0 起 `run.sh` 用 `AIS_BENCH_DATASETS_CACHE` + `--config-dir` 把 `/tmp/aisbench-ws` 接给 ais_bench
   （见上面「构建与运行真镜像」里的 work_path 接线），**不要再写 `/benchmark`**：那里属 root 且平台只读。
5. **平台的 `--model_path` 不校验存在性**（§10.4 路径对平台不透明），而工具会校验；路径写错时的表现是
   `exit 1` + `TESTER_NONZERO_EXIT`，`notes` 里能看到工具的原始报错。
6. **`stub_aisbench.py` 只能证明链路，不能当结论**：它的 TTFT/TPOT 是确定性公式（`100 + 20×c` /
   `10 + 0.5×c`），与真实服务无关。真实结论必须用真 ais_bench + 真服务；本 README 引用的那次
   平台 Run 打的也是 **mock 服务**，不是真模型。
7. **`sla_max_concurrency` 可能不出现**：所有 PASS 点都没被压满（实测并发 < 目标 80%）时它**不输出**，
   `notes` 写「并发未被真正测出，结论不可用」。这是刻意的——宁可没有数，也不给一个配置凑出来的数。
   参见开头「⚠️ 硬警告」。
8. **`tester.image_ref` 恒为 `unknown` 是预期**：平台不注入 `TESTPLANE_TESTER_IMAGE`
   （`executor/render.go` 没给 Job 设这个变量），离开平台跑时可以自己设。
9. **未建模的工具开关**见「参数映射」末段；当前版本不做 dict 化透传（避免把未声明的参数悄悄带进平台语义）。
10. `run.sh` 用 `STUB_WORK_DIR` 让 stub 找到逐点改写的模型配置；真 ais_bench 不读该变量，但如果你换的是
    另一个自定义 `--aisbench_cmd`，它可能也需要类似的工作区提示（工具本身通过 `--work-dir` 传运行目录）。

## 维护者验证清单

```bash
# 1) 注册表校验（tester.yaml + run.sh）
cd server && go test ./internal/registry -run TestParse -v    # 或在仓库根跑 go test ./server/internal/registry
# 2) vendored 工具自身没坏
py -3.11 testers/aisbench-sla/tool/tests/test_cli_surface.py
py -3.11 testers/aisbench-sla/tool/tests/test_offline.py
# 3) summary → result.json 映射（降级 / 正常 / 全降级 三种 fixture，不需要 docker）
py -3.11 testers/aisbench-sla/tests/test_result_mapping.py
# 4) result.json 按平台契约校验（需要 pyyaml + jsonschema；缺依赖会跳过）
py -3.11 testers/aisbench-sla/tests/validate_result_schema.py
# 5) 镜像离线冒烟（构建上下文 = Tester 目录，与发版矩阵一致）
docker build -f testers/aisbench-sla/build/Dockerfile \
  -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.4.0 testers/aisbench-sla
bash testers/aisbench-sla/tests/smoke.sh
```

## 本机验证记录（2026-11-06，0.4.0）

| 项 | 命令 | 结果 |
|---|---|---|
| 注册表（含本目录） | `cd server && go test ./internal/registry` | `ok`；`tester.yaml` 仍按 26 个参数解析，`version/image = 0.4.0` |
| vendored CLI | `py -3.11 tool/tests/test_cli_surface.py` | `PASS (34/34 OK)` |
| vendored 离线 e2e | `py -3.11 tool/tests/test_offline.py` | `Ran 22 tests ... OK`（含 mock+stub 全链路与 `DegradedAndCredentialTests`） |
| summary→result.json 映射 | `py -3.11 tests/test_result_mapping.py` | `PASS (26/26 OK)`：正常 fixture 出 4、降级 fixture 只出 1、全降级不输出该指标 |
| 容器冒烟（stub 镜像） | `bash tests/smoke.sh`（在 `tests/Dockerfile.stub` 构建的镜像里） | 退出码 0；成功路径 6 个 metrics（新名字）；`cases/degraded` 的 `sla_max_concurrency=1 < max_ok=2`；退出码 2/3 的 `error.code` 正确；`grep sk-test-SECRET123` 在产物与 work_path 下 **0 命中** |
| 契约校验 | `py -3.11 tests/validate_result_schema.py` | RESULT SCHEMA: PASS (5/5)：冒烟/红证/降级/退出码 2/3 的 result.json 全过 openapi Result（含 ErrorCode enum） |
| 单一发布镜像按发版 context 构建 | `docker build -f build/Dockerfile … testers/aisbench-sla` | 成功；`:0.4.0`（官方 ais_bench 基镜像，约 6.4 GB）；`command -v ais_bench && id` 输出真命令与 `uid=10001` |
| **平台真跑（真 ais_bench，mock 服务）** | `POST /api/v1/runs`，`precheck: auto`，`service.url=http://sla-mock.testplane-mock.svc:18080/v1` | **本 README 只引用这一次：Run `r-20261106-938F`**（3 点 c=1,2,4，`succeeded`；链路 `precheck(succeeded) → sla-search-real(succeeded)`）。⚠️ 目标是 **mock 服务（`tool/tests/mock_vllm.py`），不是真模型**——它证明链路与产物形态，不是模型性能。该次用 0.2.0 镜像，metrics 按旧名：`sla_max_concurrency=4`、`max_output_throughput_tok_s=1253.612`、`ttft_avg_ms=1.8`、`tpot_p90_ms=2.3`、`probes_total=3`（0.4.0 对应 `output_throughput` / `ttft` / `tpot`）；c=4 那点 ais_bench 原始表 TTFT avg 1.8 / TPOT avg 2.2 / E2EL avg 17.2 ms、Total Generated Tokens 256；artifacts 含 `sla_out/points/c{1,2,4}/<exp>/aisbench_full.log` |
| 日志体积（同 manifest 的 3 点真跑，0.2.0 镜像；体积只取决于点数与负载，与 938F 同构） | PVC 上 `wc -c` / `wc -l` + logs API | `logs/stdout.log` 51 833 B / 380 行；`sla_out/aisbench_all.log` 50 334 B / 360 行；`points/c1/full.log` 15 060 B（c2/c4 各 ~15.2 KB）；产物合计 512 763 B；logs API `limit=65536` 一次取回全量（`X-Log-Offset: 51833`），无 `truncated`/`next_offset` |

> **证据口径**：上表所有「平台真跑」都指同一次 Run `r-20261106-938F`，早期另一次同 manifest 的
> `r-20261106-ZC7R` 不再单独引用（两份文档指同一次 Run，避免数字一样但对象不同）。
> 那次跑的 mock 是 vendored 的 `tool/tests/mock_vllm.py`（集群内一个 Deployment + Service），
> 不是仓库里的 `tools/mock-llm`——后者没有 `/metrics`，算不出命中率。夹具清单没有进仓库（放在被
> gitignore 的 `.data/` 下）。