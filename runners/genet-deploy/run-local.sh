#!/bin/bash
# =============================================================================
# 本地便捷入口：跑一次 service-deploy（独立运行 / 本地联调用）
#
# ⚠ 两件事说清楚：
#   1. 这不是契约入口。契约入口只有一个：容器内的 /runner/run.sh（ARCHITECTURE §5.2）。
#      v2 契约里平台执行 entry 时 argv 恒空（§5.3）；独立运行按 §5.10 同构形态：
#      部署描述走输入文件（--params-file 优先于 /workspace/input/params.yaml）。
#   2. **本脚本不 build**。镜像由 CI 产出，这里只 pull/run（§6.2）；本地联调可用
#      build/Dockerfile 构出的 argus-service-deploy:dev。
#
# 用法：
#   # 1) 准备部署描述（Genet 部署描述原文，values 透传）
#   cat > .local/params.yaml <<'EOF'
#   replicaCount: 1
#   model: Qwen3-32B
#   model_path: /models/Qwen3-32B
#   package_type: stack
#   EOF
#   # 2) 指向 genet-local（或真实 Genet）并带上 API Key
#   AG_GENET_BASE_URL=http://host.docker.internal:18091 \
#   GENET_API_KEY=tp-local-2026 \
#   ./run-local.sh
#
# 输入文件：.local/params.yaml 存在时自动挂载为 /workspace/input/params.yaml:ro，
# 也可以用 PARAMS_FILE=<路径> 指别的文件。产物默认写到 ./.local/out（AG_OUTPUT_DIR 可覆盖）。
#
# 环境变量：IMAGE / AG_OUTPUT_DIR / AG_GENET_BASE_URL / GENET_API_KEY /
#           AG_DEPLOY_TIMEOUT / AG_DEPLOY_POLL_* / AG_TASK_ID / AG_ATTEMPT
#
# ⚠ 凭据只经环境变量注入容器，本脚本不落盘、不回显 GENET_API_KEY（§12.4）。
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${IMAGE:-argus-service-deploy:dev}"
OUT_DIR="${AG_OUTPUT_DIR:-${HERE}/.local/out}"
PARAMS_SRC="${PARAMS_FILE:-${HERE}/.local/params.yaml}"

: "${AG_GENET_BASE_URL:?需要 AG_GENET_BASE_URL（Genet OpenAPI 基地址，如 http://host.docker.internal:18091）}"
: "${GENET_API_KEY:?需要 GENET_API_KEY（Genet Open API Key；只注入环境变量，不回显）}"

if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  echo "镜像 ${IMAGE} 不存在。" >&2
  echo "  镜像由 CI 产出，本脚本不 build（§6.2）。本地联调可先构建 dev tag：" >&2
  echo "    cd ${HERE} && docker build -f build/Dockerfile -t argus-service-deploy:dev ." >&2
  echo "  或指定 CI 产出的引用：IMAGE=registry.dev.huawei.com/argus/service-deploy@sha256:... $0" >&2
  exit 2
fi

mkdir -p "${OUT_DIR}" || exit 2

# Git Bash/MSYS 下 -v /d/... 会被路径改写吞掉：挂载前转成 Windows 混合形式（D:/...）
OUT_HOST="${OUT_DIR}"; PARAMS_HOST="${PARAMS_SRC}"
if command -v cygpath >/dev/null 2>&1; then
  OUT_HOST="$(cygpath -m "${OUT_DIR}")"
  PARAMS_HOST="$(cygpath -m "${PARAMS_SRC}")"
fi

ENVARGS=(-e "AG_TASK_ID=${AG_TASK_ID:-local}" \
         -e "AG_ATTEMPT=${AG_ATTEMPT:-1}" \
         -e "AG_RUNNER_SEQ=${AG_RUNNER_SEQ:-1}" \
         -e "AG_OUTPUT_DIR=/workspace/output" \
         -e "AG_GENET_BASE_URL=${AG_GENET_BASE_URL}" \
         -e "GENET_API_KEY=${GENET_API_KEY}")
for v in AG_DEPLOY_TIMEOUT AG_DEPLOY_POLL_FAST_INTERVAL AG_DEPLOY_POLL_FAST_WINDOW \
         AG_DEPLOY_POLL_SLOW_INTERVAL AG_DEPLOY_POLL_NOTFOUND_GRACE; do
  [ -n "${!v:-}" ] && ENVARGS+=(-e "${v}=${!v}")
done

MOUNTS=(-v "${OUT_HOST}:/workspace/output")
if [ -f "${PARAMS_SRC}" ]; then
  echo "==> 挂载输入文件 ${PARAMS_SRC} -> /workspace/input/params.yaml:ro"
  MOUNTS+=(-v "${PARAMS_HOST}:/workspace/input/params.yaml:ro")
else
  echo "!! 未找到部署描述 ${PARAMS_SRC}（runner 会因三通道皆无而以退出码 2 失败，§5.3）" >&2
fi

echo "==> docker run ${IMAGE}（entry /runner/run.sh，argv 恒空）"
# Git Bash/MSYS 的自动路径转换会破坏 docker run 的挂载参数，禁用之（挂载已显式转换）。
MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" docker run --rm \
  "${ENVARGS[@]}" \
  "${MOUNTS[@]}" \
  "${IMAGE}"
rc=$?

echo "==> exit=${rc}（0=ready；1=DEPLOY_BLOCKED/DEPLOY_FAILED；2=参数或凭据错误，§5.8）"
if [ -f "${OUT_DIR}/deploy-state.json" ]; then
  echo "==> ${OUT_DIR}/deploy-state.json"
  if command -v jq >/dev/null 2>&1; then jq . "${OUT_DIR}/deploy-state.json"; else cat "${OUT_DIR}/deploy-state.json"; fi
else
  echo "!! 没有产出 deploy-state.json" >&2
fi
if [ -f "${OUT_DIR}/exports.env" ]; then
  echo "==> ${OUT_DIR}/exports.env（platform 收割注入下游，§5.12）"
  grep -v '^#' "${OUT_DIR}/exports.env" | sed 's/=.*/=<value>/'
fi
exit "${rc}"
