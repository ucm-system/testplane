#!/bin/bash
# =============================================================================
# Argus Runner: endpoint-probe —— 服务就绪探测（Pre-Check）
#
# 契约：design/ARCHITECTURE.md §5.2（entry）、§5.3（输入文件 + env + 输出）、
#       §5.4.1（公共参数）、§5.11（Pre-Check 附加约束）、附录 A（result.json schema 2.0）
#
# 调用形态（§5.3）：业务输入只走输入文件；平台执行 entry 时 argv 恒空。
#   输入文件读取优先级：--params-file <path> > $AG_PARAMS_FILE > /workspace/input/params.jsonl
#   JSONL 逐行解析，每行必须是一个 JSON 对象；本 Runner 的参数（单行）：
#     {"wait_timeout_seconds":600,"retry_interval_seconds":15,"max_tokens":1,"stream":true}
#   公共参数走环境变量（§5.4.1）：AG_SERVED_URL / AG_SERVED_MODEL_NAME。
#
# 独立运行（§5.10）：公共参数可用 CLI（优先于环境变量），业务参数可用输入文件：
#   /runner/run.sh --params-file /tmp/params.jsonl \
#                  --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B
#   两者都不给输入文件时业务参数取默认值；显式给的路径不存在则报错退出（码 2）。
#
# 运行元信息走环境变量：AG_TASK_ID / AG_ATTEMPT / AG_RUNNER_SEQ（缺省 local / 1 / 1，§5.10）
# 产物写到 /workspace/output（可用 AG_OUTPUT_DIR 覆盖）：
#   result.json（本 Runner 产物；产物契约三选一 D-74：result.json/html/csv 至少其一，本 Runner 选 result.json）、logs/probe.log、raw/last-response.txt
#
# 判定（对齐 Jenkins E2E 的 probeCompletion）：
#   POST /v1/chat/completions，max_tokens=1、stream=true、temperature=0
#   - 2xx 且响应体出现 SSE 数据帧（data:）  -> 就绪
#   - 2xx 但无数据帧 / content 为空          -> 未就绪，继续等
#   - 401/403                                -> 立即失败（认证不会自愈）
#   - 其他状态码 / 连接失败                   -> 继续等，窗口用尽后失败
#
# 退出码：0 就绪；1 未就绪/不可达；2 参数或输入错误。
# =============================================================================
set -uo pipefail   # 不用 -e：探测失败也要把 result.json 写出来（本 Runner 的产物选择，D-74/§5.9）

VERSION="1.0.0"
OUT="${AG_OUTPUT_DIR:-/workspace/output}"
mkdir -p "${OUT}/logs" "${OUT}/raw" 2>/dev/null || { echo "无法创建 ${OUT}" >&2; exit 2; }

LOG="${OUT}/logs/probe.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}" >&2; }

# 元信息（§5.3）：平台注入；独立运行缺省 local / 1 / 1
TASK_ID="${AG_TASK_ID:-local}"
ATTEMPT_NO="${AG_ATTEMPT:-1}"
RUNNER_SEQ="${AG_RUNNER_SEQ:-1}"
IMAGE_REF="${AG_RUNNER_IMAGE:-unknown}"

STARTED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START="$(date +%s)"

command -v jq >/dev/null 2>&1 && command -v curl >/dev/null 2>&1 || { log "缺少 jq 或 curl"; exit 2; }

# --- result.json 写入与统一出口（先定义：输入解析阶段就可能调用）------------------
num() { case "$1" in ''|*[!0-9]*) printf '0' ;; *) printf '%s' "$(( 10#$1 ))" ;; esac; }

write_result() {   # $1=status  $2=error.code  $3=message  $4=retryable(true/false)
  local waited=$(( $(date +%s) - START ))
  jq -n --arg sv "2.0" --arg task "${TASK_ID}" \
     --argjson attempt "$(num "${ATTEMPT_NO}")" --argjson seq "$(num "${RUNNER_SEQ}")" \
     --arg ver "${VERSION}" --arg img "${IMAGE_REF}" --arg status "$1" \
     --arg started "${STARTED}" --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
     --arg url "${URL}" --arg model "${MODEL_NAME}" --arg msrc "${MODEL_SOURCE}" \
     --argjson mt "$(num "${MAX_TOKENS}")" \
     --argjson stream "$([ "${STREAM}" = "true" ] && printf true || printf false)" \
     --argjson attempts "${ATTEMPTS}" --argjson wait "${waited}" \
     --argjson http "$(num "${LAST_HTTP}")" --arg code "$2" --arg msg "$3" \
     --argjson retry "$([ "${4:-false}" = "true" ] && printf true || printf false)" \
     '{schema_version: $sv, task_id: $task, attempt_no: $attempt, runner_seq: $seq,
       runner: {name: "endpoint-probe", version: $ver, image_ref: $img},
       status: $status, started_at: $started, finished_at: $finished,
       workload: {base_url: $url, model_name: $model, model_source: $msrc, max_tokens: $mt, stream: $stream},
       metrics: [
         {name: "precheck_attempts",         value: $attempts, unit: "count", statistic: "count", labels: {}},
         {name: "precheck_wait_seconds",     value: $wait,     unit: "s",     statistic: "max",   labels: {}},
         {name: "precheck_last_http_status", value: $http,     unit: "code",  statistic: "max",   labels: {}}
       ],
       error: (if $code == "" then null else {code: $code, message: $msg, detail: {last_http_status: $http}, retryable: $retry} end)}' \
     > "${OUT}/result.json" || return 1
}
finish() {         # $1=exit  $2=status  $3=code  $4=message  $5=retryable
  log "结论：status=$2 ${4}"
  write_result "$2" "$3" "$4" "${5:-false}" || { log "result.json 写入失败"; exit 2; }
  exit "$1"
}

# --- 输入与公共参数 -------------------------------------------------------------
# URL / MODEL_NAME：CLI（独立运行，§5.10）优先，回落 AG_SERVED_URL / AG_SERVED_MODEL_NAME（§5.4.1）。
# 业务参数只来自输入文件（JSONL）；url / model_name 不是业务参数，不进文件。
PARAMS_FILE=""
URL=""
MODEL_NAME=""
UNKNOWN_ARGS=()

while [ $# -gt 0 ]; do
  arg="$1"; shift
  case "${arg}" in
    --params-file)   [ $# -ge 1 ] && { PARAMS_FILE="$1"; shift; } || { echo "--params-file 缺少路径" >&2; exit 2; } ;;
    --params-file=*) PARAMS_FILE="${arg#*=}" ;;
    --url)           [ $# -ge 1 ] && { URL="$1"; shift; } || { echo "--url 缺少值" >&2; exit 2; } ;;
    --url=*)         URL="${arg#*=}" ;;
    --model_name)    [ $# -ge 1 ] && { MODEL_NAME="$1"; shift; } || { echo "--model_name 缺少值" >&2; exit 2; } ;;
    --model_name=*)  MODEL_NAME="${arg#*=}" ;;
    --help|-h)
      printf '用法: %s [--params-file <jsonl>] [--url=<base URL>] [--model_name=<name>]\n' "$0" >&2
      printf '输入文件（JSONL，每行一个 JSON 对象）字段:\n' >&2
      printf '  {"wait_timeout_seconds":600,"retry_interval_seconds":15,"max_tokens":1,"stream":true}\n' >&2
      printf 'url 示例: --url=http://10.0.0.10:8000/v1 （不带 /v1 会自动补）\n' >&2
      exit 2 ;;
    --model_path|--) : ;;   # 公共参数保留字：平台可能注入（§5.4.1）；本 Runner 用不到，容忍
    *)       # 不是本 Runner 认识的参数：记下来，稍后统一报错
      UNKNOWN_ARGS+=("${arg}") ;;
  esac
done

# 输入文件解析（§5.3）：逐行 JSON 对象；未声明字段从严拒绝；多行时后面的值覆盖前面。
parse_params_file() {
  local file="$1" lineno=0 line key v
  [ -r "${file}" ] || { finish 2 failed VALIDATION_ERROR "输入文件不存在或不可读：${file}"; }
  while IFS= read -r line || [ -n "${line}" ]; do
    lineno=$(( lineno + 1 ))
    # 跳过空白行
    [ -z "$(printf '%s' "${line}" | tr -d ' \t\r')" ] && continue
    printf '%s' "${line}" | jq -e 'type == "object"' >/dev/null 2>&1 \
      || finish 2 failed VALIDATION_ERROR "输入第 ${lineno} 行不是 JSON 对象：${line}"
    while IFS= read -r key; do
      case "${key}" in
        wait_timeout_seconds|retry_interval_seconds|max_tokens|stream) : ;;
        *) finish 2 failed VALIDATION_ERROR "输入第 ${lineno} 行含未声明字段：${key}（本 Runner 只接受 wait_timeout_seconds / retry_interval_seconds / max_tokens / stream）" ;;
      esac
    done < <(printf '%s' "${line}" | jq -r 'keys[]')
    # tojson + 去引号：数字/布尔原样取出；字符串值去引号后按标量处理（类型不符由下方校验兜底）。
    # 不能用 `.x // empty`：jq 的 `//` 会把 false 当空值，{"stream": false} 会被静默丢掉。
    v="$(printf '%s' "${line}" | jq -r 'if has("wait_timeout_seconds") and .wait_timeout_seconds != null then .wait_timeout_seconds|tojson else empty end')"; [ -n "${v}" ] && WAIT="${v//\"/}"
    v="$(printf '%s' "${line}" | jq -r 'if has("retry_interval_seconds") and .retry_interval_seconds != null then .retry_interval_seconds|tojson else empty end')"; [ -n "${v}" ] && INTERVAL="${v//\"/}"
    v="$(printf '%s' "${line}" | jq -r 'if has("max_tokens") and .max_tokens != null then .max_tokens|tojson else empty end')"; [ -n "${v}" ] && MAX_TOKENS="${v//\"/}"
    v="$(printf '%s' "${line}" | jq -r 'if has("stream") and .stream != null then .stream|tojson else empty end')"; [ -n "${v}" ] && STREAM="${v//\"/}"
  done < "${file}"
}

WAIT="600"
INTERVAL="15"
MAX_TOKENS="1"
STREAM="true"

if [ -n "${PARAMS_FILE}" ]; then
  parse_params_file "${PARAMS_FILE}"           # 显式指定：必须存在且合法
elif [ -n "${AG_PARAMS_FILE:-}" ]; then
  parse_params_file "${AG_PARAMS_FILE}"        # 平台注入（§5.3）
elif [ -f /workspace/input/params.jsonl ]; then
  parse_params_file /workspace/input/params.jsonl   # 约定路径
#else 无输入文件：业务参数全部取默认值（独立运行场景，§5.10）
fi

# 公共参数回落（§5.4.1）：CLI 未给时读平台注入的环境变量
[ -n "${URL}" ] || URL="${AG_SERVED_URL:-}"
[ -n "${MODEL_NAME}" ] || MODEL_NAME="${AG_SERVED_MODEL_NAME:-}"

# --- url 归一化：业界统一的 OpenAI 兼容 base URL --------------------------------
# url = http://host:port/v1；不带 /v1 会自动补，末尾斜杠会被去掉。
# 探活路径由 url 推导（/chat/completions、/models），不单独暴露 path 参数。
case "${URL}" in
  "")      ;;
  *://*)   URL="${URL%/}" ;;
  *)       URL="http://${URL%/}" ;;
esac
if [ -n "${URL}" ]; then
  case "${URL}" in
    */v1) : ;;
    *)    URL="${URL}/v1"; log "url 未带 /v1，已按 OpenAI 兼容约定补全为 ${URL}" ;;
  esac
fi
CHAT_URL="${URL}/chat/completions"
MODELS_URL="${URL}/models"

ATTEMPTS=0
LAST_HTTP=""
LAST_MSG=""
MODEL_SOURCE=""

# --- 参数校验（平台已做类型 + required 校验（§5.4），这里做范围/枚举等语义复核）----
# 凭据与 curl 公共参数先就绪，下面的 model 推断也要用
CURL_BASE=(-sS --noproxy '*' --connect-timeout 10 --max-time 300)
AUTH=()
if [ -n "${ENDPOINT_API_KEY:-}" ]; then
  AUTH=(-H "Authorization: Bearer ${ENDPOINT_API_KEY}")
fi

if [ "${#UNKNOWN_ARGS[@]}" -gt 0 ]; then
  finish 2 failed VALIDATION_ERROR "无法识别的参数：${UNKNOWN_ARGS[*]}（只接受 --params-file / --url / --model_name）"
fi
[ -n "${URL}" ] || finish 2 failed VALIDATION_ERROR "缺少被测服务地址（--url 或环境变量 AG_SERVED_URL）"

# --- model_name 可省：省略时从 /v1/models 取第一个可用模型 --------------------
# 场景：平台拿不到模型名，但服务已部署。探活只需要"能出字"，用哪个模型并不重要。
# 代价：服务多模型时可能选中的不是你想要的，所以必须把选中的模型写进日志与结果。
MODEL_SOURCE="arg"
if [ -z "${MODEL_NAME}" ]; then
  MODELS_RAW="$(curl -sS --noproxy '*' --connect-timeout 5 --max-time 10 \
                 ${AUTH[@]+"${AUTH[@]}"} -w '\n%{http_code}' "${MODELS_URL}" 2>/dev/null || true)"
  MODELS_CODE="$(printf '%s' "${MODELS_RAW}" | tail -n1 | tr -dc '0-9')"
  MODELS_BODY="$(printf '%s' "${MODELS_RAW}" | sed '$d')"
  case "${MODELS_CODE:-000}" in
    2*) MODEL_NAME="$(printf '%s' "${MODELS_BODY}" | jq -r '(.data[0].id) // (.models[0].id) // (.data[0]) // empty' 2>/dev/null || true)" ;;
    *)  MODEL_NAME="" ;;
  esac
  if [ -z "${MODEL_NAME}" ]; then
    finish 2 failed VALIDATION_ERROR "缺少模型名（--model_name 或 AG_SERVED_MODEL_NAME），且无法从 ${MODELS_URL} 推断（HTTP ${MODELS_CODE:-000}，响应：$(printf '%s' "${MODELS_BODY}" | head -c 160 | tr '\n\r' '  ')）"
  fi
  MODEL_SOURCE="models_list"
  log "未提供模型名，从 ${MODELS_URL} 自动选用：${MODEL_NAME}"
fi
case "${WAIT}" in ''|*[!0-9]*) finish 2 failed VALIDATION_ERROR "wait_timeout_seconds 必须是非负整数" ;; esac
case "${INTERVAL}" in ''|*[!0-9]*) finish 2 failed VALIDATION_ERROR "retry_interval_seconds 必须是正整数" ;; esac
case "${MAX_TOKENS}" in ''|*[!0-9]*) finish 2 failed VALIDATION_ERROR "max_tokens 必须是正整数" ;; esac
[ "${INTERVAL}" -ge 1 ] || finish 2 failed VALIDATION_ERROR "retry_interval_seconds 必须 >= 1"
[ "${MAX_TOKENS}" -ge 1 ] || finish 2 failed VALIDATION_ERROR "max_tokens 必须 >= 1"
case "${STREAM}" in true|false) ;; *) finish 2 failed VALIDATION_ERROR "stream 只能是 true 或 false" ;; esac

# 直连被测服务，忽略代理环境变量（代理的 502 会被误判成服务未就绪）—— 见上方 CURL_BASE

log "探活 ${CHAT_URL} model_name=${MODEL_NAME}（source=${MODEL_SOURCE:-arg}）stream=${STREAM} max_tokens=${MAX_TOKENS} 窗口=${WAIT}s 间隔=${INTERVAL}s"

# --- 探测循环 -----------------------------------------------------------------
DEADLINE=$(( START + WAIT ))
while : ; do
  ATTEMPTS=$(( ATTEMPTS + 1 ))
  BODY="${OUT}/raw/last-response.txt"

  if [ "${STREAM}" = "true" ]; then
    PAYLOAD="$(jq -nc --arg m "${MODEL_NAME}" --argjson mt "${MAX_TOKENS}" \
      '{model:$m, messages:[{role:"user",content:"hello"}], max_tokens:$mt, temperature:0, stream:true}')"
  else
    PAYLOAD="$(jq -nc --arg m "${MODEL_NAME}" --argjson mt "${MAX_TOKENS}" \
      '{model:$m, messages:[{role:"user",content:"hello"}], max_tokens:$mt, temperature:0}')"
  fi

  CODE="$(curl "${CURL_BASE[@]}" ${AUTH[@]+"${AUTH[@]}"} -o "${BODY}" -w '%{http_code}' \
          -X POST -H 'Content-Type: application/json' --data-binary "${PAYLOAD}" \
          "${CHAT_URL}" 2>"${OUT}/raw/last-curl-error.txt")"
  RC=$?
  case "${CODE}" in ''|*[!0-9]*) CODE="000" ;; esac
  LAST_HTTP="${CODE}"

  NONRETRY=0
  if [ "${RC}" -ne 0 ]; then
    LAST_MSG="连接失败（curl exit=${RC}）：$(head -c 200 "${OUT}/raw/last-curl-error.txt" 2>/dev/null | tr '\n\r' '  ')"
    # DNS 解析失败、TLS 握手/证书错误属环境或配置问题，重试不会自愈
    case "${RC}" in 6|35|51|58|59|60|77) NONRETRY=1 ;; esac
  elif [ "${CODE}" -ge 200 ] && [ "${CODE}" -le 299 ]; then
    if [ "${STREAM}" = "true" ]; then
      if grep -q 'data:' "${BODY}" 2>/dev/null; then
        LAST_MSG="HTTP ${CODE}，收到 SSE 数据帧，服务可出 token"
        log "第 ${ATTEMPTS} 次探测：${LAST_MSG}"
        finish 0 succeeded "" "${LAST_MSG}"
      fi
      LAST_MSG="HTTP ${CODE} 但无 SSE 数据帧"
    else
      if [ -n "$(jq -r '.choices[0].message.content // empty' "${BODY}" 2>/dev/null)" ]; then
        LAST_MSG="HTTP ${CODE}，content 非空，服务可出 token"
        log "第 ${ATTEMPTS} 次探测：${LAST_MSG}"
        finish 0 succeeded "" "${LAST_MSG}"
      fi
      LAST_MSG="HTTP ${CODE} 但 content 为空"
    fi
    # 2xx 却带 error 对象：网关把错误包成了 200，属于配置问题，等下去不会变
    if grep -qi '"error"' "${BODY}" 2>/dev/null; then
      NONRETRY=1
      LAST_MSG="HTTP ${CODE} 但响应是错误对象：$(head -c 200 "${BODY}" 2>/dev/null | tr '\n\r' '  ')"
    fi
  else
    LAST_MSG="HTTP ${CODE}：$(head -c 200 "${BODY}" 2>/dev/null | tr '\n\r' '  ')"
    case "${CODE}" in
      401|403) LAST_MSG="HTTP ${CODE}：认证失败（检查 ENDPOINT_API_KEY）" ;;
    esac
    # 4xx 客户端类错误：请求本身不对，重试不会自愈
    # 404 除外——PD 分离下 model server 注册需要时间，属"尚未就绪"
    case "${CODE}" in 400|405|406|413|415|422|501) NONRETRY=1 ;; esac
    case "${CODE}" in 401|403) NONRETRY=1 ;; esac
  fi

  log "第 ${ATTEMPTS} 次探测：未就绪 —— ${LAST_MSG}"

  # Fail Fast（§1 原则）：请求或环境本身有问题时立刻结束，
  # 不占着独占槽位空等——否则后面所有正式测试都被这一条挡住。
  if [ "${NONRETRY}" -eq 1 ]; then
    log "判定为不可重试错误，立即结束，不再等待"
    case "$(num "${CODE}")" in
      0) finish 1 failed ENDPOINT_UNREACHABLE "不可重试：${LAST_MSG}" false ;;
      *) finish 1 failed PRECHECK_FAILED "不可重试：${LAST_MSG}" false ;;
    esac
  fi

  LEFT=$(( DEADLINE - $(date +%s) ))
  [ "${LEFT}" -le 0 ] && break
  SLEEP_FOR="${INTERVAL}"; [ "${SLEEP_FOR}" -gt "${LEFT}" ] && SLEEP_FOR="${LEFT}"
  [ "${SLEEP_FOR}" -lt 1 ] && SLEEP_FOR=1
  sleep "${SLEEP_FOR}"
done

# 窗口用尽：服务一直没就绪，但错误本身可重试（可能只是加载慢）
case "$(num "${LAST_HTTP}")" in
  0) finish 1 failed ENDPOINT_UNREACHABLE "${WAIT}s 内始终无法连接：${LAST_MSG}" true ;;
  *) finish 1 failed PRECHECK_FAILED "${WAIT}s 内未就绪：${LAST_MSG}" true ;;
esac
