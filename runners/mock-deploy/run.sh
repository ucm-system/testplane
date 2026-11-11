#!/bin/bash
# =============================================================================
# Argus 内置 Runner: mock-deploy —— Mock 服务接入（role: deploy，仅内置）
#
# 契约：design/ARCHITECTURE.md §5.2/§5.3/§5.8（entry、调用契约、退出码）、§9.1（内置
#       deploy 行为契约）、§5.12（导出契约）、附录 C；
#       server/internal/deploystate/deploystate.go（deploy-state.json 的解析 schema，
#       本 Runner 写出的字段必须能被 deploystate.Parse 通过）。
#
# 与 Genet 完全解耦（D-75）：本 Runner **不做任何部署动作**，只「接入」一个已经存在的
# OpenAI 兼容 mock 服务（联调环境是 chart 内置的 mock-llm Deployment，tools/mock-llm）：
#   1. 读参数（JSONL 单行，三通道：--params-file > $AG_PARAMS_FILE >
#      /workspace/input/params.jsonl，§5.3）：
#        base_url               可选，OpenAI 兼容基地址；缺省用 AG_MOCK_BASE_URL
#                               （runner.yaml env 声明 default → chart 内置 mock-llm）
#        model_name             可选，缺省 mock-llm
#        model_path             可选，权重路径（本地联调无权重，一般不填）
#        ready_timeout_seconds  可选，就绪探测总窗口，缺省 120
#   2. 就绪探测：GET {base_url}/models 每 AG_MOCK_POLL_INTERVAL 秒一次，
#      2xx 视为就绪；其余（5xx / 连接拒绝 / 503 未就绪）都是瞬态，窗口内继续探测
#      （mock-llm 的 ready_after 故障注入就是靠 503 表达「还没起来」）；
#      超窗 → failure{code:DEPLOY_BLOCKED, reason:timeout}，退出 1（D-47：部署五值
#      没有 failed 子态，统一收口 blocked）。
#   3. 就绪后：写 $AG_OUTPUT_DIR/deploy-state.json（phase=ready，原子写 temp+rename）
#      与 exports.env 四件套，退出 0。可用性由 pre_check runner 判定（§9.1）。
#
# mock-llm 是 chart 管理的常驻共享服务（mockLlm.enabled），Task 结束**不需要拆除**：
# 与 existing-deploy 同理，clean 阶段不注入（runner.yaml 不声明 clean_runner）。
# base_url 指向别的服务时也一样：本 Runner 从不创建集群对象，无可拆除物。
#
# 日志只到 stdout 与 logs/deploy.log；不处理凭据（mock-llm 不校验鉴权）。
# 退出码：0 就绪；1 超窗未就绪；2 参数或输入错误（§5.8）。
# =============================================================================
set -euo pipefail

VERSION="1.1.0"
OUT="${AG_OUTPUT_DIR:-/workspace/output}"
mkdir -p "${OUT}/logs" 2>/dev/null || { echo "无法创建输出目录 ${OUT}" >&2; exit 2; }

LOG="${OUT}/logs/deploy.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}"; }

command -v curl >/dev/null 2>&1 || { log "缺少 curl"; exit 2; }
command -v jq >/dev/null 2>&1   || { log "缺少 jq"; exit 2; }

START="$(date +%s)"
STATE_FILE="${OUT}/deploy-state.json"
EXPORTS_FILE="${OUT}/exports.env"

num() { case "${1:-}" in ''|*[!0-9]*) printf '0' ;; *) printf '%s' "$(( 10#$1 ))" ;; esac; }

# write_state PHASE REASON FCODE FREASON FMSG —— 原子写 deploy-state.json（temp+rename）。
# FCODE 为空字符串表示无 failure。写失败不中断主流程（平台按"下轮重读"处理，§9.5）。
write_state() {
  local phase="$1" reason="$2" fcode="${3:-}" freason="${4:-}" fmsg="${5:-}"
  local waited; waited="$(num "$(( $(date +%s) - START ))")"
  local tmp; tmp="$(mktemp "${STATE_FILE}.XXXXXX" 2>/dev/null)" || return 0
  if jq -n \
      --arg ph "$phase" --arg reason "$reason" \
      --arg url "$SERVED_URL" --arg model "$MODEL_NAME" \
      --argjson waited "$waited" \
      --arg fc "$fcode" --arg fr "$freason" --arg fm "$fmsg" \
      '{release_name:"mock-llm", namespace:"", genet_status:"", endpoint_status:"",
        phase:$ph, reason:$reason, waited_seconds:$waited, label_selector:"",
        pods:[], queue:null,
        exports:{"AG_SERVED_URL":$url, "AG_SERVED_MODEL_NAME":$model},
        failure:(if $fc == "" then null else {code:$fc, reason:$fr, message:$fm} end)}' \
      > "$tmp" 2>>"${LOG}" && mv -f "$tmp" "$STATE_FILE"; then
    return 0
  fi
  rm -f "$tmp" 2>/dev/null || true
  log "WARN: deploy-state.json 写入失败（下轮重写）"
  return 0
}

# --- 参数预检（任何执行动作前完成，§5.8）--------------------------------------
PARAMS_FILE="${AG_PARAMS_FILE:-/workspace/input/params.jsonl}"
line=""
if [ -s "$PARAMS_FILE" ]; then
  line="$(head -n 1 "$PARAMS_FILE")"
fi

# 空输入合法（全部走缺省）；给了就必须是单行 JSON 对象
if [ -n "$line" ] && ! printf '%s' "$line" | jq -e 'type == "object"' >/dev/null 2>&1; then
  log "参数不是合法的 JSON 对象（JSONL 单行）：${line:0:120}"
  exit 2
fi

# jget KEY DEFAULT —— 取字段（缺失 / null / **空输入**都回落缺省；布尔与数字转字符串）
jget() {
  if [ -z "$line" ]; then
    printf '%s' "$2"
    return 0
  fi
  printf '%s' "$line" | jq -r --arg key "$1" --arg d "$2" \
    'if has($key) and .[$key] != null then (.[$key]|tostring) else $d end' 2>/dev/null \
    || printf '%s' "$2"
}

BASE_URL="$(jget base_url "${AG_MOCK_BASE_URL:-http://mock-llm.argus.svc.cluster.local:18080/v1}")"
MODEL_NAME="$(jget model_name "mock-llm")"
MODEL_PATH="$(jget model_path "")"
READY_TIMEOUT="$(jget ready_timeout_seconds "${AG_MOCK_READY_TIMEOUT:-120}")"
POLL_INTERVAL="${AG_MOCK_POLL_INTERVAL:-2}"

case "$READY_TIMEOUT" in ''|*[!0-9]*) log "ready_timeout_seconds 不是非负整数：$READY_TIMEOUT"; exit 2 ;; esac
# base_url 去尾斜杠（models 路径拼接用）
BASE_URL="${BASE_URL%/}"
if [ -z "$BASE_URL" ]; then
  log "base_url 为空（参数与 AG_MOCK_BASE_URL 都没给）"
  exit 2
fi
PROBE_URL="${BASE_URL}/models"
SERVED_URL="$BASE_URL"

# --- 就绪探测（瞬态都继续等；超窗 DEPLOY_BLOCKED，D-47）------------------------
log "mock-deploy v${VERSION}：探测 ${PROBE_URL}（窗口 ${READY_TIMEOUT}s，间隔 ${POLL_INTERVAL}s）"
deadline=$(( START + READY_TIMEOUT ))
while :; do
  # curl 失败（连接拒绝等）时 -w 已输出 000，但 || 兜底会再补一个 —— 先吞掉退出码再判空
  code="$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 5 "$PROBE_URL" 2>>"${LOG}" || true)"
  [ -n "$code" ] || code="000"
  now="$(date +%s)"
  if [ "$code" = "200" ] || [ "$code" = "204" ]; then
    log "就绪（HTTP ${code}，等待 $(( now - START ))s）"
    break
  fi
  if [ "$now" -ge "$deadline" ]; then
    log "超窗未就绪（最后 HTTP ${code}）→ DEPLOY_BLOCKED(reason=timeout)"
    write_state "blocked" "timeout" "DEPLOY_BLOCKED" "timeout" "mock 服务 ${PROBE_URL} 在 ${READY_TIMEOUT}s 内未就绪（最后 HTTP ${code}）"
    printf 'AG_SERVED_URL=%s\nAG_SERVED_MODEL_NAME=%s\n' "$BASE_URL" "$MODEL_NAME" > "$EXPORTS_FILE" 2>/dev/null || true
    exit 1
  fi
  log "未就绪（HTTP ${code}），${POLL_INTERVAL}s 后重试（剩余 $(( deadline - now ))s）"
  sleep "$POLL_INTERVAL"
done

# --- 就绪：deploy-state + exports 四件套（§5.12），退出 0 ----------------------
write_state "ready" "ready"
cat > "$EXPORTS_FILE" <<EXPORTS
AG_SERVED_URL=$BASE_URL
AG_SERVED_MODEL_NAME=$MODEL_NAME
AG_MODEL_PATH=$MODEL_PATH
AG_DEPLOY_REF=mock
EXPORTS

log "mock-deploy：mock 服务已接入（url=$BASE_URL model=$MODEL_NAME），常驻共享服务、无需拆除"
exit 0
