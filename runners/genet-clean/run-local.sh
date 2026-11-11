#!/bin/bash
# =============================================================================
# 本地便捷入口：跑一次 service-clean（独立运行 / 本地联调用）
#
# ⚠ 两件事说清楚：
#   1. 这不是契约入口。契约入口只有一个：容器内的 /runner/run.sh（ARCHITECTURE §5.2）。
#      平台执行 entry 时 argv 恒空（§5.3）；本 Runner 无业务参数，releaseName 全走环境变量。
#   2. **本脚本不 build**。镜像由 CI 产出，这里只 pull/run（§6.2）；本地联调可用
#      build/Dockerfile 构出的 argus-service-clean:dev。
#
# 用法（释放某次部署；AG_DEPLOY_REF 是 deploy 导出的 "namespace/releaseName"）：
#   AG_GENET_BASE_URL=http://host.docker.internal:18091 \
#   GENET_API_KEY=tp-local-2026 \
#   AG_DEPLOY_REF=argus-svc/arg-9f3a1c2b7d40 \
#   ./run-local.sh
#
# AG_DEPLOY_REF 缺失时 runner 按确定性兜底派生 releaseName（日志注明 fallback，§9.4）。
# 可选：DEPLOY_STATE_FILE=<deploy-state.json 副本> 挂到 /workspace/input/deploy-state.json:ro。
# 产物默认写到 ./.local/out（AG_OUTPUT_DIR 可覆盖）；本 Runner 产出 logs/clean.log 与 raw/。
#
# ⚠ 凭据只经环境变量注入容器，本脚本不落盘、不回显 GENET_API_KEY（§12.4）。
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-argus-service-clean:dev}"
OUT_DIR="${AG_OUTPUT_DIR:-${HERE}/.local/out}"
STATE_SRC="${DEPLOY_STATE_FILE:-}"

: "${AG_GENET_BASE_URL:?需要 AG_GENET_BASE_URL（Genet OpenAPI 基地址，如 http://host.docker.internal:18091）}"
: "${GENET_API_KEY:?需要 GENET_API_KEY（Genet Open API Key；只注入环境变量，不回显）}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。本地联调可先构建 dev tag：" >&2
  echo "    cd ${HERE} && docker build -f build/Dockerfile -t argus-service-clean:dev ." >&2
  echo "  或指定 CI 产出的引用：IMAGE=registry.dev.huawei.com/argus/service-clean@sha256:... $0" >&2
  exit 2
fi

mkdir -p "${OUT_DIR}" || exit 2

# Git Bash/MSYS 下 -v /d/... 会被路径改写吞掉：挂载前转成 Windows 混合形式（D:/...）
OUT_HOST="${OUT_DIR}"; STATE_HOST="${STATE_SRC}"
if command -v cygpath >/dev/null 2>&1; then
  OUT_HOST="$(cygpath -m "${OUT_DIR}")"
  [ -n "${STATE_HOST}" ] && STATE_HOST="$(cygpath -m "${STATE_HOST}")"
fi

ENVARGS=(-e "AG_TASK_ID=${AG_TASK_ID:-local}" \
         -e "AG_ATTEMPT=${AG_ATTEMPT:-1}" \
         -e "AG_RUNNER_SEQ=${AG_RUNNER_SEQ:-1}" \
         -e "AG_OUTPUT_DIR=/workspace/output" \
         -e "AG_GENET_BASE_URL=${AG_GENET_BASE_URL}" \
         -e "GENET_API_KEY=${GENET_API_KEY}")
[ -n "${AG_DEPLOY_REF:-}" ] && ENVARGS+=(-e "AG_DEPLOY_REF=${AG_DEPLOY_REF}")
for v in AG_CLEAN_VERIFY_TIMEOUT AG_CLEAN_POLL_INITIAL_INTERVAL AG_CLEAN_POLL_MAX_INTERVAL; do
  [ -n "${!v:-}" ] && ENVARGS+=(-e "${v}=${!v}")
done

MOUNTS=(-v "${OUT_HOST}:/workspace/output")
if [ -n "${STATE_SRC}" ] && [ -f "${STATE_SRC}" ]; then
  echo "==> 挂载 deploy-state 副本 ${STATE_SRC} -> /workspace/input/deploy-state.json:ro"
  MOUNTS+=(-v "${STATE_HOST}:/workspace/input/deploy-state.json:ro")
fi

echo "==> docker run ${IMAGE}（entry /runner/run.sh，argv 恒空）"
# Git Bash/MSYS 的自动路径转换会破坏 docker run 的挂载参数，禁用之（挂载已显式转换）。
MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" docker run --rm \
  "${ENVARGS[@]}" \
  "${MOUNTS[@]}" \
  "${IMAGE}"
rc=$?

echo "==> exit=${rc}（0=已确认干净；1=cleanup_failed（平台不改 Task 终态，§9.1）；2=参数或凭据错误）"
if [ -f "${OUT_DIR}/logs/clean.log" ]; then
  echo "==> ${OUT_DIR}/logs/clean.log（尾部）"
  tail -n 12 "${OUT_DIR}/logs/clean.log"
fi
exit "${rc}"
