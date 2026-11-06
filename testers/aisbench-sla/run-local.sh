#!/bin/bash
# =============================================================================
# 本地便捷入口：跑一次 Tester（独立运行用）
#
# ⚠ 两件事说清楚：
#   1. 这不是契约入口。契约入口只有一个：容器内的 /tester/run.sh（ARCHITECTURE §5.2）。
#      docker run 时不带 command，镜像的 ENTRYPOINT 就是它——所以下面的参数直接拼在镜像名后面。
#   2. **本脚本不 build**。镜像由 CI 产出，这里只 pull/run（§6.2）。
#
# 用法：
#   IMAGE=ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.1.0 ./run-local.sh \
#     --url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/tester/tool \
#     --input_len=128 --output_len=8 --data_num=8 --sla=ttft.avg:100000 \
#     --dataset=/tester/fixtures/smoke_dataset.jsonl \
#     --aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tester/tool
#   产物默认写到 ./.local/out（可用 OUTPUT_DIR 覆盖）
#
# 环境变量：IMAGE / OUTPUT_DIR / ENDPOINT_API_KEY / TESTPLANE_RUN_ID 等
# 注意：要连宿主上的服务，Linux 加 --network host；Windows/macOS 用 host.docker.internal。
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.1.0}"
OUT_DIR="${OUTPUT_DIR:-${HERE}/.local/out}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。" >&2
  echo "  本地验证可先构建：docker build -f build/Dockerfile -t ${IMAGE} ../.." >&2
  exit 2
fi

mkdir -p "${OUT_DIR}" || exit 2

ENVARGS=(-e "TESTPLANE_RUN_ID=${TESTPLANE_RUN_ID:-local}" \
         -e "TESTPLANE_TASK_ID=${TESTPLANE_TASK_ID:-local}" \
         -e "TESTPLANE_ATTEMPT_ID=${TESTPLANE_ATTEMPT_ID:-local}" \
         -e "TESTPLANE_TESTER_IMAGE=${IMAGE}")
if [ -n "${ENDPOINT_API_KEY:-}" ]; then
  ENVARGS+=(-e "ENDPOINT_API_KEY=${ENDPOINT_API_KEY}")
fi

echo "==> docker run ${IMAGE} $*"
docker run --rm \
  -u "$(id -u):$(id -g)" \
  "${ENVARGS[@]}" \
  -v "${OUT_DIR}:/workspace/output" \
  "${IMAGE}" "$@"
rc=$?

echo "==> exit=${rc}"
if [ -f "${OUT_DIR}/result.json" ]; then
  echo "==> ${OUT_DIR}/result.json"
  cat "${OUT_DIR}/result.json"
else
  echo "!! 没有产出 result.json，违反 §5.9" >&2
fi
exit "${rc}"
