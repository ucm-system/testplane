# TestPlane Tester: aisbench-sla —— 镜像构建定义
#
# 唯一入口 /tester/run.sh（ARCHITECTURE §5.2 Single Entry Point）。
# 运行期依赖只有 python3 + requests（工具 requirements.txt 的唯一运行时依赖）；
# result.json 由 run.sh 内嵌的 python 写，因此**不需要 jq / curl**。
#
# 构建上下文 = **仓库根**（要从 testers/aisbench-sla 取 vendored 工具与 run.sh）：
#   cd <repo-root>
#   TAG=0.1.0
#   docker build -f testers/aisbench-sla/build/Dockerfile \
#     -t ghcr.io/ucm-system/testplane-tester-aisbench-sla:${TAG} .
#   docker push ghcr.io/ucm-system/testplane-tester-aisbench-sla:${TAG}
# 构建由 CI 执行（§6.2）；tester.yaml 的 image 写产物引用即可（tag / digest 均可）。
#
# vendored 来源与 commit 见 testers/aisbench-sla/tool/VENDORED.md。
# 这里刻意**不**联网 clone 上游仓库：构建输入必须只是本仓库的字节（§6.2 可追溯）。
#
# 本地一键构建 + 运行见 ../run-local.sh（不是契约入口，仅方便独立运行调试）。

# 建议钉 digest：FROM python:3.11-slim@sha256:<digest>
FROM python:3.11-slim

# bash：run.sh 用 bash（数组 / PIPESTATUS）；
# ca-certificates：将来接真实 ais_bench 时的出站 HTTPS；
# tzdata 不装：日志与 result.json 统一用 UTC（date -u），不假装本地时区。
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# 工具唯一的运行期依赖（见 testers/aisbench-sla/tool/requirements.txt）
RUN pip install --no-cache-dir requests

# 非 root 运行（§13.2）。挂载目录属主需由挂载方保证：
#   平台 K8s Executor -> securityContext.fsGroup: 10001
#   自己 docker run    -> -u "$(id -u):$(id -g)" 或 chown
RUN groupadd -g 10001 tester \
 && useradd -u 10001 -g 10001 -M -s /usr/sbin/nologin tester \
 && mkdir -p /workspace/output /tester \
 && chown -R 10001:10001 /workspace /tester

# ---- 执行入口 ----------------------------------------------------------------
COPY testers/aisbench-sla/run.sh /tester/run.sh
RUN chmod 0755 /tester/run.sh

# ---- vendored 工具（只读副本，不改上游代码）----------------------------------
COPY testers/aisbench-sla/tool /tester/tool

# ---- 离线冒烟用的小数据集（8 行 × 512 字符；stub 按 chars/4 估 ~128 token）----
COPY testers/aisbench-sla/tests/fixtures/smoke_dataset.jsonl /tester/fixtures/smoke_dataset.jsonl

# 可执行位：--aisbench_cmd 直接写 .py 路径时要求她有执行位。
# /tester/tool 必须对 10001 **可写**：工具会把每点的模型配置写进
# <work_path>/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py，
# 并把数据集软链到 <work_path>/ais_bench/datasets/gsm8k/test.jsonl。
RUN chmod 0755 /tester/tool/sla_search.py /tester/tool/tests/*.py \
 && chown -R 10001:10001 /tester /workspace

USER 10001:10001

WORKDIR /workspace

# 平台 Job Spec 的 command 也是 /tester/run.sh，二者等价。
ENTRYPOINT ["/tester/run.sh"]
