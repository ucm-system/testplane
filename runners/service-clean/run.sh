#!/bin/bash
# =============================================================================
# Argus 内置 Runner: service-clean —— 服务拆除（role: clean，仅内置，finally 语义）
#
# 契约：design/ARCHITECTURE.md §5.2/§5.3/§5.8、§7.6（finally）、§9.1（内置 clean 行为契约）；
#       design/impl/execution.md §9.2/§9.4（Genet 事实基线与 clean 行为契约，D-50/D-54）。
#
# 行为序列（execution §9.4）：
#   1. 取 releaseName（优先级）：
#        a. AG_DEPLOY_REF（deploy 导出；形态 "namespace/releaseName"，取最后一段）；
#        b. AG_DEPLOY_STATE_FILE 或 /workspace/input/deploy-state.json 副本的 .release_name
#           （execution §9.4 第 1 条：deploy-state 副本挂进 /workspace/input 或经 env 给路径）；
#        c. 都缺失 → 确定性兜底 "arg-" + sha256("${AG_TASK_ID}:${AG_ATTEMPT}")[:12]
#           （D-44，与 deploy 同式；**日志注明 fallback**）。
#      releaseName 形态校验 ^[a-z][a-z0-9-]{0,52}$，不合法 → 退出码 2（乱删兜底名会错杀资源）。
#   2. DELETE /api/open/helm/releases/{name}?keepHistory=false&force=true
#      （只能走 Genet OpenAPI：直接 helm uninstall 会被 admission webhook 拒绝，D-54）；
#      404 视为已干净（幂等）；200 继续验证；5xx/超时结果未知 → 交给验证循环判定。
#   3. 判干净 = GET /releases/{name} 404 **且** GET /releases/{name}/pods 的 items==[]
#      （两个条件同时满足、且两次查询本身成功——**查询失败 ≠ 为空，不判成功**，R9/D-50）。
#      验证循环：初始 AG_CLEAN_POLL_INITIAL_INTERVAL 秒起指数退避至上限
#      AG_CLEAN_POLL_MAX_INTERVAL 秒，总窗口 AG_CLEAN_VERIFY_TIMEOUT（缺省 600s）。
#   4. 幂等：资源已不存在 exit 0；判不干净 / 窗口内无法确认 exit 1
#      （平台记 cleanup_failed + CLEANUP_FAILED，不改 Task 终态，进 §9.6 重放清单）；
#      参数/凭据类错误 exit 2（§5.8）。
#
# 凭据（GENET_API_KEY）绝不打印（§12.4 / K-5）；错误信息带 releaseName、HTTP 码、Genet 原文。
# 退出码：0 已确认干净；1 未确认干净/调用失败；2 参数或输入错误。
# =============================================================================
set -euo pipefail

VERSION="1.0.0"
OUT="${AG_OUTPUT_DIR:-/workspace/output}"
mkdir -p "${OUT}/logs" "${OUT}/raw" 2>/dev/null || { echo "无法创建输出目录 ${OUT}" >&2; exit 2; }

LOG="${OUT}/logs/clean.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}"; }

command -v curl >/dev/null 2>&1 || { log "缺少 curl"; exit 2; }
command -v jq >/dev/null 2>&1   || { log "缺少 jq"; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { log "缺少 sha256sum"; exit 2; }

START="$(date +%s)"

num() { case "${1:-}" in ''|*[!0-9]*) printf '0' ;; *) printf '%s' "$(( 10#$1 ))" ;; esac; }
env_int() {
  local name="$1" def="$2" v="${!1:-}"
  case "$v" in
    '') printf '%s' "$def" ;;
    *[!0-9]*) log "WARN: 环境变量 ${name}=${v} 非法（须非负整数），使用缺省 ${def}"; printf '%s' "$def" ;;
    *) printf '%s' "$v" ;;
  esac
}

VERIFY_TIMEOUT="$(env_int AG_CLEAN_VERIFY_TIMEOUT 600)"
POLL_INTERVAL="$(env_int AG_CLEAN_POLL_INITIAL_INTERVAL 2)"
POLL_MAX="$(env_int AG_CLEAN_POLL_MAX_INTERVAL 15)"
[ "${POLL_INTERVAL}" -ge 1 ] || POLL_INTERVAL=1
[ "${POLL_MAX}" -ge "${POLL_INTERVAL}" ] || POLL_MAX="${POLL_INTERVAL}"

TASK_ID="${AG_TASK_ID:-local}"
ATTEMPT="${AG_ATTEMPT:-1}"

# --- 统一出口 ------------------------------------------------------------------
finish() { # $1=exit $2=结论（日志用）
  log "结论：exit=$1 $2"
  exit "$1"
}

# --- releaseName 解析（§9.4 第 1 条）--------------------------------------------
REL_PATTERN='^[a-z][a-z0-9-]{0,52}$'
RAW_REF="${AG_DEPLOY_REF:-}"
REF_SOURCE=""
NAMESPACE_HINT=""
if [ -n "${RAW_REF}" ]; then
  REF_SOURCE="AG_DEPLOY_REF"
  case "${RAW_REF}" in */*) NAMESPACE_HINT="${RAW_REF%%/*}" ;; esac
  REF="${RAW_REF##*/}"                     # "ns/name" 取最后一段；无斜杠时取整体
else
  # 兜底来源 2：deploy-state.json 副本（execution §9.4：挂进 /workspace/input 或经 env 给路径）
  STATE_COPY="${AG_DEPLOY_STATE_FILE:-}"
  [ -n "${STATE_COPY}" ] || STATE_COPY="/workspace/input/deploy-state.json"
  if [ -r "${STATE_COPY}" ]; then
    REF="$(jq -r '(.release_name // "") | tostring' "${STATE_COPY}" 2>/dev/null || printf '')"
    if [ -n "${REF}" ]; then
      REF_SOURCE="deploy-state copy: ${STATE_COPY}"
      NAMESPACE_HINT="$(jq -r '(.namespace // "") | tostring' "${STATE_COPY}" 2>/dev/null || printf '')"
    fi
  fi
  if [ -z "${REF:-}" ]; then
    # 兜底来源 3：确定性派生（与 deploy / domain.ReleaseName 同式；泄漏重放 §9.6 靠它）
    REF="arg-$(printf '%s' "${TASK_ID}:${ATTEMPT}" | sha256sum | cut -c1-12)"
    REF_SOURCE="fallback(deterministic D-44)"
    log "AG_DEPLOY_REF 缺失且无 deploy-state 副本 → 使用确定性兜底 releaseName=${REF}（fallback，来源=${REF_SOURCE}）"
  fi
fi
if ! printf '%s' "${REF}" | grep -Eq "${REL_PATTERN}"; then
  finish 2 "releaseName 不合法：'${REF}'（来自 ${REF_SOURCE}）不匹配 ${REL_PATTERN}；拒绝执行（用兜底名 DELETE 会错杀无关资源；重放时修正 AG_DEPLOY_REF/deploy-state 副本即可，§9.6）"
fi
log "service-clean v${VERSION} 启动：task=${TASK_ID} attempt=${ATTEMPT} releaseName=${REF}（来源=${REF_SOURCE}${NAMESPACE_HINT:+, ns=${NAMESPACE_HINT}}）"

GENET_BASE="${AG_GENET_BASE_URL:-}"
API_KEY="${GENET_API_KEY:-}"
[ -n "${GENET_BASE}" ] || finish 2 "缺少 AG_GENET_BASE_URL（Genet OpenAPI 基地址）"
[ -n "${API_KEY}" ]    || finish 2 "缺少 GENET_API_KEY（Genet Open API Key，凭据走 Task 级 secrets，§12.4）"

# --- Genet OpenAPI 客户端（connect 5s / 总 10s，§9.2；与 service-deploy 同款）----
RAW="${OUT}/raw"
GENET_CURL_RC=0
genet_request() { # $1=method $2=path $3=响应体落盘路径；stdout=HTTP code（000=连接失败）
  local method="$1" path="$2" outfile="${3:-${RAW}/body.json}" code
  GENET_CURL_RC=0
  code="$(curl -sS --noproxy '*' --connect-timeout 5 --max-time 10 \
            -H "Authorization: Bearer ${API_KEY}" \
            -o "${outfile}" -w '%{http_code}' -X "${method}" \
            "${GENET_BASE}${path}" 2>"${RAW}/curl-error.txt")" || GENET_CURL_RC=$?
  case "${code:-}" in ''|*[!0-9]*) code="000" ;; esac
  printf '%s' "${code}"
}
genet_err_msg() { jq -r '(.message // .error // "") | tostring' "${RAW}/body.json" 2>/dev/null | head -c 300 | tr '\r\n' '  ' || true; }
curl_err_msg()  { head -c 200 "${RAW}/curl-error.txt" 2>/dev/null | tr '\r\n' '  ' || true; }

window_left() { local l=$(( VERIFY_TIMEOUT - $(date +%s) + START )); [ "${l}" -lt 0 ] && l=0; printf '%s' "${l}"; }

# --- 第 2 步：卸载（DELETE；404 视为已干净；结果未知交给验证循环）----------------
DEL_PATH="/api/open/helm/releases/${REF}?keepHistory=false&force=true"
CODE="$(genet_request DELETE "${DEL_PATH}")"
case "${CODE}" in
  200)
    log "DELETE 200：$(genet_err_msg)（DELETE 的 200 不算数——以验证循环确认为准，R-G:218-221）"
    ;;
  404)
    log "DELETE 404：release 不存在，视为已干净（幂等，§9.4）"
    ;;
  401|403)
    finish 2 "Genet 认证失败（DELETE HTTP ${CODE}：$(genet_err_msg)；检查 GENET_API_KEY 是否与部署时同一把——namespace 绑定）"
    ;;
  *)
    log "DELETE ${CODE}（curl rc=${GENET_CURL_RC}: $(curl_err_msg) $(genet_err_msg)）→ 结果未知，交由验证循环判定"
    ;;
esac

# --- 第 3 步：判干净（GET 404 且 pods items==[]；查询失败 ≠ 为空，R9）------------
# clean 不写 deploy-state.json（契约只要求日志与退出码；产物目录留 logs/ 与 raw/）。
REL_PATH="/api/open/helm/releases/${REF}?light=true"     # R12：查询一律 light=true
PODS_PATH="/api/open/helm/releases/${REF}/pods"
INTERVAL="${POLL_INTERVAL}"
N=0
while :; do
  N=$(( N + 1 ))
  REL_CODE="$(genet_request GET "${REL_PATH}" "${RAW}/rel.json")"
  PODS_CODE="$(genet_request GET "${PODS_PATH}" "${RAW}/pods.json")"

  REL_QUERY_OK=1; PODS_QUERY_OK=1
  case "${REL_CODE}" in 000|5*) REL_QUERY_OK=0 ;; 401|403)
    finish 2 "Genet 认证失败（GET release HTTP ${REL_CODE}：$(genet_err_msg)）" ;; esac
  case "${PODS_CODE}" in 000|5*) PODS_QUERY_OK=0 ;; 401|403)
    finish 2 "Genet 认证失败（GET pods HTTP ${PODS_CODE}：$(genet_err_msg)）" ;; esac

  if [ "${REL_QUERY_OK}" -eq 1 ] && [ "${PODS_QUERY_OK}" -eq 1 ]; then
    # 两次查询本身都成功 → 才有资格判"干净/不干净"
    if [ "${REL_CODE}" = "404" ]; then
      if [ "${PODS_CODE}" = "404" ]; then
        log "GET pods 404（release 已消失，pods 亦无）→ 视为 items==[]"
        finish 0 "已确认干净：GET /releases/${REF} 404 且 pods 无（第 ${N} 轮；幂等成功，§9.4 第 3 条）"
      fi
      ITEMS="$(jq -r '(.items // []) | length' "${RAW}/pods.json" 2>/dev/null || printf '-1')"
      if [ "$(num "${ITEMS}")" -eq 0 ]; then
        finish 0 "已确认干净：GET /releases/${REF} 404 且 GET /pods items==[]（第 ${N} 轮；D-50/D-54）"
      fi
      log "第 ${N} 轮：release 已 404 但 pods items=${ITEMS}（残留 Pod 还在删）→ 不判干净"
    else
      STATUS="$(jq -r '(.status // "?") | tostring' "${RAW}/rel.json" 2>/dev/null || printf '?')"
      log "第 ${N} 轮：release 仍在（GET ${REL_CODE}，status=${STATUS}）→ 不判干净"
    fi
  else
    # 查询失败 ≠ 为空（R9）：任何一次查询失败都不得判成功，重试到窗口用尽
    REL_CURL_ERR=""
    if [ "${REL_QUERY_OK}" -eq 0 ]; then REL_CURL_ERR="curl_rc=${GENET_CURL_RC}: $(curl_err_msg)"; fi
    log "第 ${N} 轮：查询失败（release GET=${REL_CODE} ${REL_CURL_ERR}；pods GET=${PODS_CODE}）→ 不判成功，重试"
  fi

  if [ "$(window_left)" -le 0 ]; then
    finish 1 "窗口（AG_CLEAN_VERIFY_TIMEOUT=${VERIFY_TIMEOUT}s）内未能确认干净：releaseName=${REF}（来源=${REF_SOURCE}），最后观测 release GET=${REL_CODE} / pods GET=${PODS_CODE}"
  fi

  # 指数退避（不超过窗口剩余）
  INTERVAL=$(( INTERVAL * 2 ))
  if [ "${INTERVAL}" -gt "${POLL_MAX}" ]; then INTERVAL="${POLL_MAX}"; fi
  S="${INTERVAL}"
  L="$(window_left)"
  if [ "${S}" -gt "${L}" ]; then S="${L}"; fi
  if [ "${S}" -lt 1 ]; then S=1; fi
  sleep "${S}"
done
