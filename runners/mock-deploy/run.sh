#!/bin/bash
# =============================================================================
# Argus 内置 Runner: service-deploy —— 服务部署（role: deploy，仅内置）
#
# 契约：design/ARCHITECTURE.md §5.2/§5.3/§5.8（entry、调用契约、退出码）、§9.1（内置
#       deploy/clean 行为契约）、§5.12（导出契约）、附录 C；
#       design/impl/execution.md §9.2/§9.3（Genet 事实基线与行为契约，D-44/D-45/D-46/D-47）；
#       server/internal/deploystate/deploystate.go（deploy-state.json 的解析 schema，
#       本 Runner 写出的字段必须能被 deploystate.Parse 通过）。
#
# 行为序列（execution §9.3）：
#   1. 读部署描述（三通道：--params-file > $AG_PARAMS_FILE > /workspace/input/params.yaml，
#      §5.3）；语义原样透传 Genet（values），只解析少量展示/路由字段（package_type、
#      template_path、model、model_path），不修改、不校验业务语义；
#   2. releaseName = "arg-" + sha256("${AG_TASK_ID}:${AG_ATTEMPT}")[:12]（D-44，确定性；
#      与 server/internal/domain.ReleaseName 同式，同 Task 同 attempt 恒等）；
#   3. 幂等拉起：GET /api/open/helm/releases/{name}
#        200          → 恢复场景，直接进入轮询；
#        404          → POST /api/open/helm/uc-packages/{stack|pd}/releases
#                       body = {releaseName, noWait:true, values:<params.yaml 原文>}
#                       （values/templatePath/templateId 不能全空，参数预检期拦截 → 退出 2）；
#        POST 202     → 进入轮询；
#        POST 400/409 → GET 复核当幂等成功（文案含 already has protected metadata 视为已存在；
#                       复核仍 404 → 请求被真拒，DEPLOY_FAILED）；
#        POST 401/403 → 凭据类错误，退出码 2（execution §9.3 第 6 步）；
#        POST 5xx/超时→ **不断定"未创建"**（Genet 202 后台异步 apply），进入轮询。
#   4. 轮询（单飞）：前 AG_DEPLOY_POLL_FAST_WINDOW 秒每 AG_DEPLOY_POLL_FAST_INTERVAL 秒，
#      之后指数退避至上限 AG_DEPLOY_POLL_SLOW_INTERVAL 秒；单请求 connect 5s / 总 10s；
#      每轮把状态**原子写** $AG_OUTPUT_DIR/deploy-state.json（mktemp + mv，temp+rename）；
#      队列水位低频查询（每 5 轮一次，best-effort，无位次——D-46：queue.position 恒 null）；
#      GET 404 在宽限窗（AG_DEPLOY_POLL_NOTFOUND_GRACE，缺省 180s）内 → phase=accepted；
#      5xx / 连接失败 / 超时 → 瞬态，**不得断定失败**，窗口内继续轮询。
#   5. 就绪（两根轴，D-45）：genet_status=deployed 且 callInfo.endpoint.status=ready
#      且 endpoint.url 非空 → 写 exports.env 四件套 + deploy-state phase=ready，退出 0。
#      （ready 只代表地址可解析，可用性由 pre_check runner 判定。）
#   6. 失败：release status ∈ {failed,superseded,uninstalling,uninstalled}（D-47 折叠为
#      phase=blocked，平台五值没有 deploy failed 子态）或 endpoint.status=blocked
#        → failure{code:DEPLOY_BLOCKED, reason, message=Genet 原文}，退出 1；
#      Genet 调不通 / 5xx / 超窗（AG_DEPLOY_TIMEOUT，缺省 259200s）
#        → 超窗/blocked：failure{code:DEPLOY_BLOCKED, reason:timeout|blocked}；Genet 调用被拒/认证失败：failure{code:DEPLOY_FAILED, reason:...}，退出 1；
#      参数/凭据类错误 → 退出码 2（§5.8：输入/环境错误在任何执行动作前退出）。
#
# deploy-state.json 字段（与 deploystate.Parse 对齐）：release_name / namespace /
# genet_status / endpoint_status / phase(五值) / reason / waited_seconds /
# label_selector / values_digest(前向兼容扩展，Parse 放行未知字段) / pods[] /
# queue{phase,pending,inqueue,running,position:null} / exports / failure。
#
# 日志只到 stdout 与 logs/deploy.log；凭据（GENET_API_KEY）绝不打印（§12.4 / K-5）。
# 退出码：0 就绪；1 部署未就绪 / Genet 调用失败；2 参数或输入错误。
# =============================================================================
set -euo pipefail

VERSION="1.0.0"
OUT="${AG_OUTPUT_DIR:-/workspace/output}"
mkdir -p "${OUT}/logs" "${OUT}/raw" 2>/dev/null || { echo "无法创建输出目录 ${OUT}" >&2; exit 2; }

LOG="${OUT}/logs/deploy.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}"; }

# 凭据不出现在任何日志/exports（§12.4）：API_KEY 只进 Authorization 头。
command -v curl >/dev/null 2>&1 || { log "缺少 curl"; exit 2; }
command -v jq >/dev/null 2>&1   || { log "缺少 jq"; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { log "缺少 sha256sum"; exit 2; }

START="$(date +%s)"

# --- 退出与状态写入（先定义：参数预检阶段就会用到）------------------------------
STATE_FILE="${OUT}/deploy-state.json"
RELEASE_NAME=""     # 在下方派生
LABEL_SELECTOR=""   # 派生后置值
NAMESPACE=""        # 来自 Genet 返回（API Key 绑定的 ns），不自行推导（R7）
GENET_STATUS=""     # helm release.status 原值
ENDPOINT_STATUS=""  # callInfo.endpoint.status 原值
PODS_JSON="[]"      # PodRef[]
QUEUE_JSON="null"   # QueueWatermark | null
EXPORTS_JSON="null" # ready 时的四件套副本 | null
VALUES_DIGEST=""

num() { case "${1:-}" in ''|*[!0-9]*) printf '0' ;; *) printf '%s' "$(( 10#$1 ))" ;; esac; }

# write_state PHASE REASON FCODE FREASON FMSG —— 原子写 deploy-state.json（temp+rename）。
# FCODE 为空字符串表示无 failure（exports/queue 等 See 全局变量）。
# 写失败不中断主流程：坏文件平台按"下轮重读"处理（execution §9.5）。
write_state() {
  local phase="$1" reason="$2" fcode="${3:-}" freason="${4:-}" fmsg="${5:-}"
  local waited; waited="$(num "$(( $(date +%s) - START ))")"
  local tmp; tmp="$(mktemp "${STATE_FILE}.XXXXXX" 2>/dev/null)" || return 0
  if jq -n \
      --arg rn "$RELEASE_NAME" --arg ns "$NAMESPACE" \
      --arg gs "$GENET_STATUS" --arg es "$ENDPOINT_STATUS" \
      --arg ph "$phase" --arg reason "$reason" \
      --arg ls "$LABEL_SELECTOR" --arg vd "$VALUES_DIGEST" \
      --argjson waited "$waited" \
      --argjson pods "$PODS_JSON" --argjson queue "$QUEUE_JSON" \
      --argjson exports "$EXPORTS_JSON" \
      --arg fc "$fcode" --arg fr "$freason" --arg fm "$fmsg" \
      '{release_name:$rn, namespace:$ns, genet_status:$gs, endpoint_status:$es,
        phase:$ph, reason:$reason, waited_seconds:$waited, label_selector:$ls,
        values_digest:$vd, pods:$pods, queue:$queue, exports:$exports,
        failure:(if $fc == "" then null else {code:$fc, reason:$fr, message:$fm} end)}' \
      > "$tmp" 2>>"${LOG}" && mv -f "$tmp" "$STATE_FILE"; then
    return 0
  fi
  rm -f "$tmp" 2>/dev/null || true
  log "WARN: deploy-state.json 写入失败（下轮重写）"
  return 0
}

# finish EXIT PHASE REASON FCODE FREASON FMSG —— 统一出口：写终态 + 退出码表达结果（§5.8）。
finish() {
  local rc="$1" phase="$2" reason="$3"
  log "结论：exit=${rc} phase=${phase} ${reason}${4:+ [failure.code=$4]}"
  write_state "$phase" "$reason" "${4:-}" "${5:-}" "${6:-}"
  exit "$rc"
}

# --- 环境变量与参数预检（任何执行动作前完成，§5.8）------------------------------
env_int() { # $1=变量名 $2=缺省值：可选调优项，非法值告警后回落缺省（容忍，不致命）
  local name="$1" def="$2" v="${!1:-}"
  case "$v" in
    '') printf '%s' "$def" ;;
    *[!0-9]*) log "WARN: 环境变量 ${name}=${v} 非法（须非负整数），使用缺省 ${def}"; printf '%s' "$def" ;;
    *) printf '%s' "$v" ;;
  esac
}

GENET_BASE="${AG_GENET_BASE_URL:-}"
API_KEY="${GENET_API_KEY:-}"
[ -n "${GENET_BASE}" ] || finish 2 blocked "缺少 AG_GENET_BASE_URL（Genet OpenAPI 基地址）" \
  DEPLOY_FAILED invalid_input "env AG_GENET_BASE_URL is required"
[ -n "${API_KEY}" ]    || finish 2 blocked "缺少 GENET_API_KEY（Genet Open API Key，凭据走 Task 级 secrets，§12.4）" \
  DEPLOY_FAILED invalid_input "env GENET_API_KEY is required"

WINDOW="$(env_int AG_DEPLOY_TIMEOUT 259200)"
FAST_INTERVAL="$(env_int AG_DEPLOY_POLL_FAST_INTERVAL 2)"
FAST_WINDOW="$(env_int AG_DEPLOY_POLL_FAST_WINDOW 30)"
SLOW_INTERVAL="$(env_int AG_DEPLOY_POLL_SLOW_INTERVAL 15)"
GRACE="$(env_int AG_DEPLOY_POLL_NOTFOUND_GRACE 180)"
[ "${FAST_INTERVAL}" -ge 1 ] || FAST_INTERVAL=1
[ "${SLOW_INTERVAL}" -ge "${FAST_INTERVAL}" ] || SLOW_INTERVAL="${FAST_INTERVAL}"

# 元信息（§5.3）：平台注入；独立运行缺省 local / 1 / 1
TASK_ID="${AG_TASK_ID:-local}"
ATTEMPT="${AG_ATTEMPT:-1}"

# releaseName（D-44，确定性）：与平台 domain.ReleaseName(taskID, attempt) 同式。
RELEASE_NAME="arg-$(printf '%s' "${TASK_ID}:${ATTEMPT}" | sha256sum | cut -c1-12)"
LABEL_SELECTOR="helm-release-name=${RELEASE_NAME}"

# 输入文件三通道（§5.3）；部署描述是本 Runner 的必要业务输入 → 三通道皆无 = 退出 2。
PARAMS_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --params-file)   [ $# -ge 2 ] || { log "--params-file 缺少路径"; exit 2; }
                     PARAMS_FILE="$2"; shift 2 ;;
    --params-file=*) PARAMS_FILE="${1#*=}"; shift ;;
    --help|-h) printf '用法: %s [--params-file <params.yaml>]\n（平台内 entry argv 恒空，§5.3）\n' "$0"; exit 0 ;;
    *) log "无法识别的参数：$1（只接受 --params-file）"; exit 2 ;;
  esac
done
[ -n "${PARAMS_FILE}" ] || PARAMS_FILE="${AG_PARAMS_FILE:-}"
[ -n "${PARAMS_FILE}" ] || { [ -f /workspace/input/params.yaml ] && PARAMS_FILE=/workspace/input/params.yaml; }
[ -n "${PARAMS_FILE}" ] || finish 2 blocked "缺少部署描述（--params-file / AG_PARAMS_FILE / /workspace/input/params.yaml 三通道皆无，§5.3）" \
  DEPLOY_FAILED invalid_input "deploy spec (params.yaml) is missing"
[ -r "${PARAMS_FILE}" ] || finish 2 blocked "部署描述不可读：${PARAMS_FILE}" \
  DEPLOY_FAILED invalid_input "deploy spec not readable: ${PARAMS_FILE}"

# values 体量约束（R11）：≤100KB，超限在参数预检期拒绝（不动 Genet）。
BYTES="$(wc -c < "${PARAMS_FILE}" | tr -d '[:space:]')"
[ "$(num "${BYTES}")" -le 102400 ] || finish 2 blocked "部署描述超过 100KB（${BYTES}B，R11：values ≤100KB）" \
  DEPLOY_FAILED invalid_input "deploy spec exceeds 100KiB"

# 空描述拦截（Genet 实测：values/templatePath/templateId 全空 → 400，§9.2）：
# 参数预检期发现全空 → 退出码 2，不发请求（execution §9.2）。
TEMPLATE_PATH="$(sed -n -E 's/^template_path[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\1/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"
if [ -z "${TEMPLATE_PATH}" ]; then
  TEMPLATE_PATH="$(sed -n -E 's/^templatePath[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\1/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"
fi
# 内容行 = 非空白、非纯注释行（逐行判定；不能先 tr——会把整份文件并成一行、
# 让"文件以注释开头"被误判为空）。
NONWS="$(grep -cvE '^[[:space:]]*(#|$)' "${PARAMS_FILE}" || true)"
if [ "$(num "${NONWS}")" -eq 0 ] && [ -z "${TEMPLATE_PATH}" ]; then
  finish 2 blocked "部署描述为空（values / templatePath 不能全空，Genet 会 400；§9.2）" \
    DEPLOY_FAILED invalid_input "deploy spec is empty (values/templatePath/templateId must not all be empty)"
fi

# 展示/路由字段（只读，不透传语义改动；解析失败不致命）：
PACKAGE_TYPE="$(sed -n -E 's/^package_type[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\1/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"
case "${PACKAGE_TYPE}" in
  '') PACKAGE_TYPE="stack" ;;   # D-53：本地/CI 一律 stack
  stack|pd) : ;;
  *) finish 2 blocked "package_type 仅支持 stack|pd（D-53），得到：${PACKAGE_TYPE}" \
       DEPLOY_FAILED invalid_input "package_type must be stack|pd, got: ${PACKAGE_TYPE}" ;;
esac
SPEC_MODEL="$(sed -n -E 's/^model(_name|Name)?[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\2/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"
MODEL_PATH="$(sed -n -E 's/^model_path[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\1/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"
[ -n "${MODEL_PATH}" ] || MODEL_PATH="$(sed -n -E 's/^modelPath[[:space:]]*:[[:space:]]*([^#]*)[[:space:]]*#?.*$/\1/p' "${PARAMS_FILE}" | head -n 1 | sed -E 's/^[[:space:]]+|[[:space:]]+$//g; s/^["'"'"']//; s/["'"'"']$//')"

VALUES_DIGEST="$(sha256sum "${PARAMS_FILE}" | cut -c1-16)"

log "service-deploy v${VERSION} 启动：task=${TASK_ID} attempt=${ATTEMPT} releaseName=${RELEASE_NAME}"
log "Genet=${GENET_BASE} package_type=${PACKAGE_TYPE} spec=${PARAMS_FILE}（${BYTES}B, digest=${VALUES_DIGEST}）"

# --- Genet OpenAPI 客户端（单飞；connect 5s / 总 10s，§9.2）----------------------
RAW="${OUT}/raw"
# genet_request METHOD PATH [BODY]：stdout=HTTP code（000=连接失败）；
# 响应体 → ${RAW}/body.json；curl 退出码 → GENET_CURL_RC。函数恒返回 0（配合 set -e）。
GENET_CURL_RC=0
genet_request() {
  local method="$1" path="$2" body="${3:-}" code
  local args=(-sS --noproxy '*' --connect-timeout 5 --max-time 10
              -H "Authorization: Bearer ${API_KEY}"
              -o "${RAW}/body.json" -w '%{http_code}' -X "${method}")
  [ -n "${body}" ] && args+=(-H 'Content-Type: application/json' --data-binary "${body}")
  GENET_CURL_RC=0
  code="$(curl "${args[@]}" "${GENET_BASE}${path}" 2>"${RAW}/curl-error.txt")" || GENET_CURL_RC=$?
  case "${code:-}" in ''|*[!0-9]*) code="000" ;; esac
  printf '%s' "${code}"
}

# Genet 错误响应里的 reason 原文（截断 300B；带上下文进日志与 failure.message，D-47）。
genet_err_msg() {
  jq -r '(.message // .error // "") | tostring' "${RAW}/body.json" 2>/dev/null \
    | head -c 300 | tr '\r\n' '  ' || true
}

curl_err_msg() { head -c 200 "${RAW}/curl-error.txt" 2>/dev/null | tr '\r\n' '  ' || true; }

# 轮询间隔：窗口到期前允许的剩余睡眠
window_left() { local l=$(( WINDOW - $(date +%s) + START )); [ "${l}" -lt 0 ] && l=0; printf '%s' "${l}"; }
window_expired() { [ "$(window_left)" -le 0 ]; }

# --- 队列水位（D-46：只有水位、没有位次；低频 best-effort，失败不改旧值）---------
fetch_queue() {
  local code
  code="$(genet_request GET /api/open/queue)"
  [ "${code}" = "200" ] || return 0
  QUEUE_JSON="$(jq -c 'def nn: if type == "number" and . >= 0 then . else 0 end;
    {phase: ((.queue.state // .queue.phase // "") | tostring),
     pending: ((.queue.pending // 0) | nn),
     inqueue: ((.queue.inqueue // 0) | nn),
     running: ((.queue.running // 0) | nn),
     position: null}' "${RAW}/body.json" 2>/dev/null || printf 'null')" || QUEUE_JSON="null"
}

# --- release 响应解析（只取 Genet 返回值，禁止自行推导，R8）----------------------
# 解析结果写入全局：NAMESPACE / GENET_STATUS / ENDPOINT_STATUS / EP_MSG / EP_URL / EP_MODEL / REL_DESC / PODS_JSON
parse_release() {
  NAMESPACE="$(jq -r '(.namespace // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  GENET_STATUS="$(jq -r '(.status // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  ENDPOINT_STATUS="$(jq -r '(.callInfo.endpoint.status // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  EP_MSG="$(jq -r '(.callInfo.endpoint.message // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  EP_URL="$(jq -r '(.callInfo.endpoint.url // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  EP_MODEL="$(jq -r '(.callInfo.model // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  REL_DESC="$(jq -r '(.description // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
  # PodRef[]：name/role/node/restarts/ready；role 是展示性提示（按名字约定，非权威）；
  # restarts 防负（deploystate 校验 pods[].restarts >= 0）。
  PODS_JSON="$(jq -c '[(.pods // [])[] | {
      name: ((.name // "") | tostring),
      role: (if ((.name // "") | tostring | test("engine")) then "engine"
             elif ((.name // "") | tostring | test("router")) then "router"
             else "" end),
      node: ((.nodeName // "") | tostring),
      restarts: (if ((.restarts // .restartCount // 0) | type == "number" and . >= 0)
                 then (.restarts // .restartCount // 0) else 0 end),
      ready: (.ready // false)}]' "${RAW}/body.json" 2>/dev/null || printf '[]')" || PODS_JSON="[]"
}

# 是否"未调度"（启发式，R3）：没有任何 Pod 带 nodeName。
pods_unscheduled() {
  [ "$(printf '%s' "${PODS_JSON}" | jq '[.[]? | select((.node // "") != "")] | length' 2>/dev/null || printf 0)" -eq 0 ]
}

# --- exports（§5.12：KV 行、值不含换行、无凭据；platform 收割注入下游）------------
write_exports() { # $1=url $2=model $3=model_path
  local url model mpath ref
  url="$(printf '%s' "$1" | tr -d '\r\n')"
  model="$(printf '%s' "$2" | tr -d '\r\n')"
  mpath="$(printf '%s' "$3" | tr -d '\r\n')"
  ref="$(printf '%s' "${NAMESPACE:+${NAMESPACE}/}${RELEASE_NAME}" | tr -d '\r\n')"
  cat > "${OUT}/exports.env" <<EOF
# service-deploy exports（§5.12）—— ready 时写入，平台按管道序收割注入下游 runner
AG_SERVED_URL=${url}
AG_SERVED_MODEL_NAME=${model}
AG_MODEL_PATH=${mpath}
AG_DEPLOY_REF=${ref}
EOF
  EXPORTS_JSON="$(jq -n --arg u "${url}" --arg m "${model}" --arg p "${mpath}" --arg r "${ref}" \
    '{AG_SERVED_URL:$u, AG_SERVED_MODEL_NAME:$m, AG_MODEL_PATH:$p, AG_DEPLOY_REF:$r}')" || EXPORTS_JSON="null"
  log "exports：AG_SERVED_URL=${url} AG_SERVED_MODEL_NAME=${model} AG_MODEL_PATH=${mpath} AG_DEPLOY_REF=${ref}"
}

# --- 状态合成（研究基线映射表：research/genet-lifecycle.md「状态映射表」#1-#8）----
ACCEPTED_AT=""   # 受理锚点（POST 202 / 400-protected / 恢复 GET 200 的时刻）
LAST_PHASE="queued"
LAST_REASON=""

set_phase() { LAST_PHASE="$1"; LAST_REASON="$2"; write_state "${LAST_PHASE}" "${LAST_REASON}" "" "" ""; }

classify_release() {  # 前提：parse_release 已执行
  case "${GENET_STATUS}" in
    failed|superseded|uninstalling|uninstalled)
      # D-47：Genet 终态失败折叠为 blocked（平台五值没有 deploy failed 子态）。
      finish 1 blocked "release ${GENET_STATUS}（D-47 折叠）：${REL_DESC:-${EP_MSG}}" \
        DEPLOY_BLOCKED blocked "helm release status=${GENET_STATUS}: ${REL_DESC:-${EP_MSG}} (releaseName=${RELEASE_NAME})"
      ;;
  esac
  case "${ENDPOINT_STATUS}" in
    blocked)
      finish 1 blocked "endpoint blocked：${EP_MSG}" \
        DEPLOY_BLOCKED blocked "callInfo.endpoint.status=blocked: ${EP_MSG} (releaseName=${RELEASE_NAME})"
      ;;
    ready)
      if [ "${GENET_STATUS}" = "deployed" ] && [ -n "${EP_URL}" ]; then
        # 就绪两根轴（D-45）。AG_SERVED_MODEL_NAME：callInfo.model → 描述里的 model → releaseName。
        local model_src="genet"
        [ -n "${EP_MODEL}" ] || { EP_MODEL="${SPEC_MODEL}"; model_src="spec"; }
        [ -n "${EP_MODEL}" ] || { EP_MODEL="${RELEASE_NAME}"; model_src="release_name"; }
        log "服务就绪：status=deployed 且 endpoint.status=ready（D-45：仅地址可解析，可用性由 pre_check 判定；model 来源=${model_src}）"
        write_exports "${EP_URL}" "${EP_MODEL}" "${MODEL_PATH}"
        finish 0 ready "endpoint ready (address resolved; availability needs pre_check, D-45)"
      fi
      set_phase deploying "endpoint.status=ready but release status=${GENET_STATUS:-unknown}/url empty（不满足两根轴，继续轮询）"
      return 0
      ;;
  esac
  case "${GENET_STATUS}" in
    pending-*)
      if pods_unscheduled; then
        set_phase queued "release ${GENET_STATUS} 且无已调度 Pod（启发式判据，R3；queue.phase=$(printf '%s' "${QUEUE_JSON}" | jq -r '(.phase // "")' 2>/dev/null || true)）"
      else
        set_phase deploying "release ${GENET_STATUS} 且 Pod 已调度（启发式判据，R3）"
      fi ;;
    deployed)
      # deployed + endpoint pending **不可**当 ready（D-45 / 映射表 #8）。
      set_phase deploying "release deployed but callInfo.endpoint.status=${ENDPOINT_STATUS:-pending}（不可当 ready，D-45）" ;;
    *)
      set_phase deploying "release status=${GENET_STATUS:-unknown}（未识别，继续轮询）" ;;
  esac
}

# --- 幂等拉起（§9.3 第 2 步）----------------------------------------------------
POLL_INTERVAL="${FAST_INTERVAL}"
backoff() { # 指数退避：快速窗之后 interval 翻倍至上限
  if [ "$(( $(date +%s) - START ))" -ge "${FAST_WINDOW}" ]; then
    POLL_INTERVAL=$(( POLL_INTERVAL * 2 ))
    if [ "${POLL_INTERVAL}" -gt "${SLOW_INTERVAL}" ]; then
      POLL_INTERVAL="${SLOW_INTERVAL}"
    fi
  fi
  return 0
}
nap() { # 睡眠不超过窗口剩余
  local s="${POLL_INTERVAL}" left; left="$(window_left)"
  [ "${s}" -gt "${left}" ] && s="${left}"
  [ "${s}" -lt 1 ] && s=1
  sleep "${s}"
}

# 先落状态再发起任何 Genet 调用（POST 超时不代表没创建；execution §9.2）。
write_state queued "preparing：spec 已受理（digest=${VALUES_DIGEST}），尚未与 Genet 确认 release" "" "" ""

ACCEPTED_AT=""          # 非空 = 已进入轮询资格（受理或恢复）
POLL_N=0

while :; do
  if window_expired; then
    # 超窗（AG_DEPLOY_TIMEOUT 缺省 259200s，K-6）：排队计入窗口。
    # 超窗归类（文档三方口径，D-47/§7.5）：DEPLOY_BLOCKED(reason=timeout) → 平台映射
    # service_unavailable（可重试）；DEPLOY_FAILED 仅保留给 Genet 调用本身失败。
    finish 1 blocked "超窗：${WINDOW}s 内未就绪（AG_DEPLOY_TIMEOUT；releaseName=${RELEASE_NAME}，最后 phase=${LAST_PHASE}）" \
      DEPLOY_BLOCKED timeout "deploy did not become ready within AG_DEPLOY_TIMEOUT=${WINDOW}s (releaseName=${RELEASE_NAME})"
  fi

  if [ -z "${ACCEPTED_AT}" ]; then
    # ---- 幂等拉起前置：先 GET（恢复场景直接进轮询）----
    CODE="$(genet_request GET "/api/open/helm/releases/${RELEASE_NAME}")"
    case "${CODE}" in
      200)
        parse_release
        ACCEPTED_AT="$(date +%s)"
        log "恢复场景：release 已存在（GET 200，status=${GENET_STATUS}，ns=${NAMESPACE}），直接进入轮询"
        classify_release
        ;;
      404)
        # 未存在 → POST（noWait=true 恒真：timeoutSec 是 helm --wait deadline，排队会误判 failed，§9.2）。
        log "GET 404（release 不存在）→ POST /api/open/helm/uc-packages/${PACKAGE_TYPE}/releases（noWait=true）"
        jq -n --arg rn "${RELEASE_NAME}" --rawfile values "${PARAMS_FILE}" --arg tp "${TEMPLATE_PATH}" \
          '{releaseName:$rn, noWait:true, values:$values}
           + (if $tp != "" then {templatePath:$tp} else {} end)' \
          > "${RAW}/post-body.json" 2>/dev/null || { log "POST body 构造失败"; exit 2; }
        CODE="$(genet_request POST "/api/open/helm/uc-packages/${PACKAGE_TYPE}/releases" "$(cat "${RAW}/post-body.json")")"
        case "${CODE}" in
          202)
            NAMESPACE="$(jq -r '(.namespace // "") | tostring' "${RAW}/body.json" 2>/dev/null || printf '')"
            ACCEPTED_AT="$(date +%s)"
            log "POST 202 受理（ns=${NAMESPACE}）；落 accept 锚点，进入轮询"
            set_phase accepted "POST /api/open/helm/uc-packages/${PACKAGE_TYPE}/releases accepted (202)"
            ;;
          400|409)
            MSG="$(genet_err_msg)"
            case "${MSG}" in
              *"already has protected metadata"*)
                # 同名已存在（Genet 同名再 POST → 400，不是幂等成功；§9.2）→ 视为已存在，进轮询。
                log "POST ${CODE}：同名 release 已存在（protected metadata）→ 幂等成功，进入轮询"
                ACCEPTED_AT="$(date +%s)"
                set_phase accepted "POST ${CODE} but release already exists (protected metadata); treating as recovered"
                ;;
              *)
                # 其他 400：GET 复核当幂等成功；复核仍 404 = 请求被真拒。
                local_code="$(genet_request GET "/api/open/helm/releases/${RELEASE_NAME}")"
                case "${local_code}" in
                  200)
                    parse_release
                    log "POST ${CODE}（${MSG}）但 GET 复核 200 → 幂等成功，进入轮询"
                    ACCEPTED_AT="$(date +%s)"
                    classify_release
                    ;;
                  404)
                    finish 1 blocked "POST 被 Genet 拒绝（HTTP ${CODE}：${MSG}）且复核 GET 404（release 未创建）" \
                      DEPLOY_FAILED genet_rejected "POST /api/open/helm/uc-packages/${PACKAGE_TYPE}/releases ${CODE}: ${MSG}; recheck GET 404 (releaseName=${RELEASE_NAME})"
                    ;;
                  *)
                    log "POST ${CODE}（${MSG}）且复核 GET ${local_code} → 状态未知，按受理处理进入轮询（404 宽限窗兜底）"
                    ACCEPTED_AT="$(date +%s)"
                    set_phase accepted "POST ${CODE} (${MSG}); recheck GET ${local_code}; polling with 404 grace"
                    ;;
                esac
                ;;
            esac
            ;;
          401|403)
            finish 2 blocked "Genet 认证失败（HTTP ${CODE}：$(genet_err_msg)；检查 GENET_API_KEY，凭据不会自愈）" \
              DEPLOY_FAILED genet_auth_failed "POST releases ${CODE}: $(genet_err_msg)"
            ;;
          000|5*)
            # 超时 / 5xx：**不得断定未创建**（202 后台异步，R-G:320）→ 按受理处理进轮询。
            log "POST ${CODE}（curl rc=${GENET_CURL_RC}: $(curl_err_msg) $(genet_err_msg)）→ 不断定未创建，进入轮询"
            ACCEPTED_AT="$(date +%s)"
            set_phase accepted "POST outcome unknown (${CODE}, curl rc=${GENET_CURL_RC}); polling with 404 grace"
            ;;
          *)
            finish 1 blocked "POST 意外响应 HTTP ${CODE}（$(genet_err_msg)）" \
              DEPLOY_FAILED genet_call_failed "POST releases unexpected HTTP ${CODE}: $(genet_err_msg)"
            ;;
        esac
        ;;
      401|403)
        finish 2 blocked "Genet 认证失败（GET HTTP ${CODE}：$(genet_err_msg)；检查 GENET_API_KEY）" \
          DEPLOY_FAILED genet_auth_failed "GET releases ${CODE}: $(genet_err_msg)"
        ;;
      *)
        # 5xx / 连接失败 / 其他：瞬态，重试 GET（不盲 POST——release 可能已存在）。
        log "GET ${CODE}（curl rc=${GENET_CURL_RC}: $(curl_err_msg) $(genet_err_msg)）→ 瞬态，重试"
        set_phase queued "genet unreachable before POST (GET ${CODE}, curl rc=${GENET_CURL_RC}); retrying"
        backoff; nap
        continue
        ;;
    esac
  else
    # ---- 轮询（单飞）----
    POLL_N=$(( POLL_N + 1 ))
    if [ $(( POLL_N % 5 )) -eq 1 ]; then fetch_queue; fi   # 低频水位（每 5 轮，best-effort）
    CODE="$(genet_request GET "/api/open/helm/releases/${RELEASE_NAME}")"
    case "${CODE}" in
      200)
        parse_release
        classify_release
        ;;
      404)
        if [ -n "${ACCEPTED_AT}" ] && [ "$(( $(date +%s) - ACCEPTED_AT ))" -le "${GRACE}" ]; then
          set_phase accepted "release not yet visible (GET 404 within ${GRACE}s grace, §9.3)"
        else
          finish 1 blocked "release 不可见：GET 404 超出宽限窗 ${GRACE}s（受理于 ${ACCEPTED_AT}；releaseName=${RELEASE_NAME}）" \
            DEPLOY_FAILED release_not_visible "GET /api/open/helm/releases/${RELEASE_NAME} 404 beyond ${GRACE}s grace after acceptance"
        fi
        ;;
      401|403)
        finish 2 blocked "Genet 认证失败（GET HTTP ${CODE}：$(genet_err_msg)）" \
          DEPLOY_FAILED genet_auth_failed "GET releases ${CODE}: $(genet_err_msg)"
        ;;
      *)
        # 5xx / 000 / 其他 4xx：瞬态，不得断定失败，继续轮询到窗口用尽。
        log "GET ${CODE}（curl rc=${GENET_CURL_RC}: $(curl_err_msg) $(genet_err_msg)）→ 瞬态，继续轮询"
        set_phase "${LAST_PHASE}" "genet transient failure (GET ${CODE}, curl rc=${GENET_CURL_RC}); polling continues"
        ;;
    esac
  fi

  backoff
  nap
done
