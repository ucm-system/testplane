# Argus Runner: aisbench-sla —— **唯一发布镜像**（真实 AISBench benchmark 基础镜像）
#
# 唯一入口 /runner/run.sh（ARCHITECTURE §5.2 Single Entry Point）。
#
# 构建上下文 = **Runner 目录**（`runners/aisbench-sla`），不是仓库根：
#   docker build -f runners/aisbench-sla/build/Dockerfile runners/aisbench-sla
# 这是**发版路径**：.github/workflows/release.yml 的矩阵给每个 Runner 的 context 就是
# `runners/<name>`（与 endpoint-probe 同一约定），所以这里的 COPY 全部相对 Runner 目录。
# 历史上写的「context = 仓库根」已作废 —— 用仓库根做 context 会去找
# runners/aisbench-sla/runners/aisbench-sla/run.sh，构建必失败（评审 #62 硬问题 3）。
#
# 构建由 CI 执行（§6.2）；runner.yaml 的 image 写产物引用即可（tag / digest 均可）。
# vendored 来源与 commit 见 runners/aisbench-sla/tool/VENDORED.md。
# 这里刻意**不**联网 clone 上游仓库：构建输入必须只是本仓库的字节（§6.2 可追溯）。
#
# ---------------------------------------------------------------------------
# 只有一种发布镜像（用户 2026-11-09 拍板）
# ---------------------------------------------------------------------------
# 以前有两个 Dockerfile：这个 + `Dockerfile.aisbench`。结果是**发布出去的是 stub 假镜像**
# （python:3.11-slim + 自带 stub_aisbench.py），真镜像反而因为依赖本地 tokenizer 构建不出来。
# 现在合成一份：本文件就是真镜像，基础镜像是官方 AISBench benchmark 镜像。
# stub 只留在 `tests/Dockerfile.stub`（离线冒烟用）；发布矩阵只扫 `build/Dockerfile*`，
# 所以 tests/ 下的 stub 镜像**不会**进发布。
#
# ---------------------------------------------------------------------------
# 关键设计：为什么 ais_bench 的工作目录是 /tmp/aisbench-ws 而不是 /benchmark
# ---------------------------------------------------------------------------
# 官方镜像里 /benchmark 是 ais_bench 的源码根（pip 装的就是它，root:root 0755）。
# 本 Runner 的 vendored 工具会往 **work_path 下**写两样东西：
#   <work_path>/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py  （模型配置）
#   <work_path>/ais_bench/datasets/gsm8k/{train,test}.jsonl                        （数据集挂载）
# 参考工具是靠「<work_path> 恰好就是源码根」让 ais_bench 找到它们的。平台的 Executor
# 用 ReadOnlyRootFilesystem=true 跑 Job（只有 /workspace/output 与 /tmp 可写），所以
# 平台上不可能写 /benchmark；把 /benchmark chown 给 10001 也只解决本地、解决不了平台。
#
# 于是这里改成「/tmp 下的影子源码根」，用 ais_bench 自己的两个开关接上：
#   * 数据集：ais_bench 的相对数据集路径是拼在 AIS_BENCH_DATASETS_CACHE 下的
#     （datasets/utils/datasets.py: get_cache_dir() 读该环境变量，默认才是源码根），
#     所以 run.sh 导出 AIS_BENCH_DATASETS_CACHE=<work_path> 即可让
#     path='ais_bench/datasets/gsm8k' 落到 <work_path>/ais_bench/datasets/gsm8k。
#   * 模型配置：ais_bench 支持 --config-dir，会在 <config-dir>/models 下找模型配置，
#     run.sh 在 work_path 不是 /benchmark 时自动追加
#     --config-dir <work_path>/ais_bench/benchmark/configs。
# 这两处都在 run.sh（本仓库自己的入口）里做，**没有改 vendored 的 sla_tool/**。
#
# ---------------------------------------------------------------------------
# 镜像**不带** tokenizer / 数据集 / 权重（这是设计，不是缺陷）
# ---------------------------------------------------------------------------
# 真机需要 tokenizer 或权重时，走 D-63 的统一共享挂载（默认 `/models`，只读 PVC），
# 或用 `--model_path` 指一个**已经存在**的容器内路径（Profile 挂载 / 外部提供）。
# 镜像里没有 `/models/*` 内容 —— 平台不替 Runner 分发模型；缺了由 run.sh/Runner 明确报错。
#
# FROM 钉的是 README 里写的那一版（0330 master，py310），避免与 0630 那版行为漂移。
FROM ghcr.io/aisbench/aisbench_benchmark:v3.1-20260330-master_x86_64_py_310

ARG RUNNER_UID=10001
ARG RUNNER_GID=10001

# 非 root 运行时没有 /home，HF datasets 缓存会去 ~/.cache 炸掉：全部收进 /tmp（可写）。
ENV HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    HF_HOME=/tmp/hf \
    HF_DATASETS_CACHE=/tmp/hf/datasets \
    PYTHONUNBUFFERED=1 \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# 非 root 用户（§13.2 平台按 10001:10001 + fsGroup 挂产物卷）。
# /tmp 属于 10001：work_path 的默认位置就在这下面（平台只保证 /tmp 可写）。
# /models 建出来只是为了让"统一共享挂载点"有个落脚处（内容由 D-63 的 PVC 提供）。
RUN groupadd -g ${RUNNER_GID} runner \
 && useradd -u ${RUNNER_UID} -g ${RUNNER_GID} -M -d /tmp -s /bin/bash runner \
 && mkdir -p /runner /models /workspace/output /tmp/aisbench-ws \
 && chown -R ${RUNNER_UID}:${RUNNER_GID} /runner /models /workspace /tmp

# ---- 执行入口 ----------------------------------------------------------------
COPY run.sh /runner/run.sh

# ---- vendored 工具（只读副本；本地改动见 tool/VENDORED.md）--------------------
COPY tool /runner/tool

# ---- 离线冒烟用的小数据集（8 行 × 512 字符）-----------------------------------
COPY tests/fixtures /runner/fixtures

# CRLF 归一到 LF：本仓库在 Windows 上检出时 `tool/**/*.py` 可能带 CRLF，
# 带 CRLF 的 shebang 在 Linux 里会报 `/usr/bin/env: 'python3\r'`。
# 在镜像里擦一遍，比要求每个开发者配 .gitattributes 稳。
RUN sed -i 's/\r$//' /runner/run.sh \
 && find /runner/tool -name '*.py' -type f -exec sed -i 's/\r$//' {} +

RUN chmod 0755 /runner/run.sh /runner/tool/sla_search.py \
 && chown -R ${RUNNER_UID}:${RUNNER_GID} /runner

USER ${RUNNER_UID}:${RUNNER_GID}

# /benchmark 是镜像自带的源码根（只读也无所谓：本镜像不往里写）。
WORKDIR /workspace

# 平台 Job Spec 的 command 也是 /runner/run.sh，二者等价。
ENTRYPOINT ["/runner/run.sh"]
