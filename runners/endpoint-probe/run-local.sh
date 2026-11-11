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
#   IMAGE=registry.dev.huawei.com/argus/endpoint-probe:1.0.0 ./run-local.sh \
#     --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B
#   业务参数走输入文件（JSONL）：.local/params.jsonl 存在时自动挂载为
#   /workspace/input/params.jsonl:ro，也可以用 --params-file 指别的路径：
#     echo '{"wait_timeout_seconds":60}' > .local/params.jsonl
#   产物默认写到 ./.local/out（可用 AG_OUTPUT_DIR 覆盖）
#
# 环境变量：IMAGE / AG_OUTPUT_DIR / ENDPOINT_API_KEY / AG_TASK_ID 等
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-registry.dev.huawei.com/argus/endpoint-probe:1.0.0}"
OUT_DIR="${AG_OUTPUT_DIR:-${HERE}/.local/out}"
PARAMS_SRC="${PARAMS_FILE:-${HERE}/.local/params.jsonl}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。" >&2
  echo "  本地验证可指定 CI 产出的镜像：IMAGE=registry.dev.huawei.com/argus/endpoint-probe@sha256:... $0 ..." >&2
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
  if command -v jq >/dev/null 2>&1; then jq . "${OUT_DIR}/result.json"; else cat "${OUT_DIR}/result.json"; fi
else
  echo "!! 没有产出 result.json（本 Runner 选择产出它；产物契约三选一 D-74）" >&2
fi
exit "${rc}"
