#!/bin/bash
# =============================================================================
# 本地便捷入口：跑一次 Runner（独立运行用）
#
# ⚠ 两件事说清楚：
#   1. 这不是契约入口。契约入口只有一个：容器内的 /runner/run.sh（ARCHITECTURE §5.2）。
#      v2 契约里平台执行 entry 时 argv 恒空（§5.3）；独立运行没有平台，按 §5.10 的
#      同构形态显式传：/runner/run.sh --params-file ... --url ... --model_name ...
#   2. **本脚本不 build**。镜像由 CI 产出，这里只 pull/run（§6.2）。
#
# 用法：
#   IMAGE=registry.dev.huawei.com/argus/aisbench-sla:0.5.0 ./run-local.sh \
#     --url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/runner/tool
#   业务参数走输入文件（JSONL）：.local/params.jsonl 存在时自动挂载为
#   /workspace/input/params.jsonl:ro，也可以用 --params-file 指别的路径：
#     echo '{"sla":["ttft.avg:100000"],"input_len":128,"output_len":8,"data_num":8,
#            "dataset":"/runner/fixtures/smoke_dataset.jsonl",
#            "aisbench_cmd":"/runner/tool/tests/stub_aisbench.py",
#            "work_path":"/runner/tool"}' > .local/params.jsonl
#   产物默认写到 ./.local/out（可用 AG_OUTPUT_DIR 覆盖）
#
# 环境变量：IMAGE / AG_OUTPUT_DIR / ENDPOINT_API_KEY / AG_TASK_ID 等
# 注意：要连宿主上的服务，Linux 加 --network host；Windows/macOS 用 host.docker.internal。
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-registry.dev.huawei.com/argus/aisbench-sla:0.5.0}"
OUT_DIR="${AG_OUTPUT_DIR:-${HERE}/.local/out}"
PARAMS_SRC="${PARAMS_FILE:-${HERE}/.local/params.jsonl}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。" >&2
  echo "  本地验证可先构建（**构建上下文 = Runner 目录本身**，不是仓库根）：" >&2
  echo "    cd runners/aisbench-sla && docker build -f build/Dockerfile -t ${IMAGE} ." >&2
  exit 2
fi

mkdir -p "${OUT_DIR}" || exit 2

ENVARGS=(-e "AG_TASK_ID=${AG_TASK_ID:-local}" \
         -e "AG_ATTEMPT=${AG_ATTEMPT:-1}" \
         -e "AG_RUNNER_SEQ=${AG_RUNNER_SEQ:-1}" \
         -e "AG_RUNNER_IMAGE=${IMAGE}")
if [ -n "${ENDPOINT_API_KEY:-}" ]; then
  ENVARGS+=(-e "ENDPOINT_API_KEY=${ENDPOINT_API_KEY}")
fi

MOUNTS=(-v "${OUT_DIR}:/workspace/output")
if [ -f "${PARAMS_SRC}" ]; then
  echo "==> 挂载输入文件 ${PARAMS_SRC} -> /workspace/input/params.jsonl:ro"
  MOUNTS+=(-v "${PARAMS_SRC}:/workspace/input/params.jsonl:ro")
fi

echo "==> docker run ${IMAGE} /runner/run.sh $*"
docker run --rm \
  -u "$(id -u):$(id -g)" \
  "${ENVARGS[@]}" \
  "${MOUNTS[@]}" \
  "${IMAGE}" /runner/run.sh "$@"
rc=$?

echo "==> exit=${rc}"
if [ -f "${OUT_DIR}/result.json" ]; then
  echo "==> ${OUT_DIR}/result.json"
  cat "${OUT_DIR}/result.json"
else
  echo "!! 没有产出 result.json（本 Runner 选择产出它；产物契约三选一 D-74）" >&2
fi
exit "${rc}"
