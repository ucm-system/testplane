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
#   IMAGE=registry.../endpoint-probe:1.0.0 ./run-local.sh --url=... --model_name=... [其他参数]
#   产物默认写到 ./.local/out（可用 OUTPUT_DIR 覆盖）
#
# 环境变量：IMAGE / OUTPUT_DIR / ENDPOINT_API_KEY / TESTPLANE_RUN_ID 等
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-endpoint-probe:local}"
OUT_DIR="${OUTPUT_DIR:-${HERE}/.local/out}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。" >&2
  echo "  本地验证可指定 CI 产出的镜像：IMAGE=registry.../endpoint-probe@sha256:... $0 ..." >&2
  exit 2
fi

mkdir -p "${OUT_DIR}" || exit 2

ENVARGS=(-e "TESTPLANE_RUN_ID=${TESTPLANE_RUN_ID:-local}" \
         -e "TESTPLANE_TASK_ID=${TESTPLANE_TASK_ID:-local}" \
         -e "TESTPLANE_ATTEMPT_ID=${TESTPLANE_ATTEMPT_ID:-local}")
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
  if command -v jq >/dev/null 2>&1; then jq . "${OUT_DIR}/result.json"; else cat "${OUT_DIR}/result.json"; fi
else
  echo "!! 没有产出 result.json，违反 §5.9" >&2
fi
exit "${rc}"
