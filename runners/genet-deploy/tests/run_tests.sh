#!/bin/bash
# =============================================================================
# service-deploy 内置 Runner 冒烟测试（stub Genet，不依赖真实集群 / genet-local）
#
# 覆盖路径（execution §9.3 / 任务验收）：
#   S1 ready           ：GET 404 → POST 202 → 轮询 → deployed+endpoint ready
#                        → exports 四件套 + deploy-state phase=ready，exit 0
#   S2 幂等恢复        ：GET 200（release 已存在）→ 直接轮询，绝不重发 POST
#   S3 POST 400 冲突   ：GET 404 → POST 400(protected metadata) → GET 复核当幂等成功
#                        → 404 宽限窗内 accepted → 可见后 ready
#   S4 blocked（helm） ：release failed（D-47 折叠）→ DEPLOY_BLOCKED，exit 1
#   S5 blocked（endpoint）：deployed 但 endpoint.status=blocked → DEPLOY_BLOCKED，exit 1
#   —   S6 窗口用尽        ：Genet 不可达/未就绪 + AG_DEPLOY_TIMEOUT 用尽 → DEPLOY_BLOCKED(timeout)，exit 1
#   S7 缺部署描述      ：三通道皆无 → 退出码 2（§5.3/§5.8）
#   S8 空部署描述      ：values/templatePath 全空 → 退出码 2（参数预检，不发请求）
#
# 每个 case 断言 deploy-state.json：
#   1. python -c "import json;json.load(...)" 可解析（任务验收口径）；
#   2. tests/validate_deploystate.py 通过（逐条对齐 deploystate.Parse/validate）。
#
# 运行方式（自动选择）：
#   - native：宿主机有 bash+curl+jq（jq>=1.6，--rawfile）→ 直接跑 run.sh；
#   - docker：否则构建 build/Dockerfile（argus-service-deploy:dev）并在容器内跑
#     （容器经 host.docker.internal 访问宿主上的 stub）。
# 用法：bash runners/service-deploy/tests/run_tests.sh
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="$(cd "${HERE}/.." && pwd)"
DEPLOY_IMAGE="${DEPLOY_IMAGE:-argus-service-deploy:dev}"
TOKEN="stub-genet-key"

# ---- 依赖与模式 --------------------------------------------------------------
# 注意：Windows 的 python3 可能是 Microsoft Store 别名（存在但不可用），必须实际验证。
PY=""
for cand in python python3 py; do
  cand_path="$(command -v "${cand}" 2>/dev/null || true)"
  if [ -n "${cand_path}" ] && "${cand_path}" -c "import json" >/dev/null 2>&1; then PY="${cand_path}"; break; fi
done
[ -n "${PY}" ] || { echo "需要可用的 python（stub 与断言用）"; exit 1; }
HAVE_JQ=0; HAVE_CURL=0
command -v jq   >/dev/null 2>&1 && HAVE_JQ=1
command -v curl >/dev/null 2>&1 && HAVE_CURL=1
MODE=""
if [ "${HAVE_JQ}" -eq 1 ] && [ "${HAVE_CURL}" -eq 1 ]; then
  MODE="native"
elif command -v docker >/dev/null 2>&1; then
  MODE="docker"
else
  echo "需要（bash+curl+jq）原生运行，或 docker 运行；二者皆缺。" >&2
  exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/svc-deploy-tests.XXXXXX")"
PASS=0; FAIL=0; STUB_PID=""; STUB_PORT=""

cleanup() {
  [ -n "${STUB_PID}" ] && kill "${STUB_PID}" 2>/dev/null
  rm -rf "${WORK}" 2>/dev/null
}
trap cleanup EXIT

ok()  { printf '  [PASS] %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
check_eq() { # desc want got
  if [ "$3" = "$2" ]; then ok "$1"; else bad "$1（want=[$2] got=[$3]）"; fi
}
check_contains() { # desc needle haystack
  case "$3" in *"$2"*) ok "$1" ;; *) bad "$1（未包含 [$2]）" ;; esac
}

# jf FILE EXPR —— 用 python 当断言用 jq（宿主机可能没有 jq）
jf() {
  "${PY}" -c '
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
x = eval(sys.argv[2])
if isinstance(x, (dict, list)):
    print(json.dumps(x, ensure_ascii=False))
elif x is None:
    print("null")
else:
    print(x)
' "$1" "$2" 2>/dev/null
}

start_stub() { # start_stub <case名> [stub 参数...]
  local case_name="$1"; shift
  "$PY" "${HERE}/stub_genet.py" --token "${TOKEN}" "$@" >"${WORK}/stub-${case_name}.log" 2>&1 &
  STUB_PID=$!
  STUB_PORT=""
  local i port
  for i in $(seq 1 50); do
    port="$(sed -n 's/^STUB_PORT=//p' "${WORK}/stub-${case_name}.log" | head -n 1)"
    if [ -n "${port}" ]; then STUB_PORT="${port}"; break; fi
    sleep 0.1
  done
  if [ -z "${STUB_PORT}" ]; then
    echo "stub 启动失败（case=${case_name}）"; cat "${WORK}/stub-${case_name}.log"; exit 1
  fi
  for i in $(seq 1 50); do
    curl -s -o /dev/null --max-time 1 "http://127.0.0.1:${STUB_PORT}/__counters" && break
    sleep 0.1
  done
}
stop_stub() { [ -n "${STUB_PID}" ] && kill "${STUB_PID}" 2>/dev/null && wait "${STUB_PID}" 2>/dev/null; STUB_PID=""; }

counters() { curl -s --max-time 3 "http://127.0.0.1:${STUB_PORT}/__counters"; }
counter()  { counters | "${PY}" -c 'import json,sys; print(json.load(sys.stdin)["counters"][sys.argv[1]])' "$1" 2>/dev/null; }

# releaseName 派生（与 run.sh / domain.ReleaseName 同式，断言确定性）
relname() { printf 'arg-%s' "$(printf '%s' "$1:$2" | sha256sum | cut -c1-12)"; }

BASE_URL=""   # runner 视角的 stub 地址（docker 用 host.docker.internal）

# run_deploy <case名> <params文件|空> <task_id> [KEY=VAL ...] —— 跑一次 runner，回传 RC
run_deploy() {
  local case_name="$1" params="$2" task_id="$3"; shift 3
  local out="${WORK}/${case_name}/out"
  mkdir -p "${out}"
  local rc=0
  if [ "${MODE}" = "docker" ]; then
    local args=(-e "AG_TASK_ID=${task_id}" -e "AG_ATTEMPT=1" -e "AG_RUNNER_SEQ=1"
                -e "AG_OUTPUT_DIR=/workspace/output"
                -e "AG_GENET_BASE_URL=${BASE_URL}" -e "GENET_API_KEY=${TOKEN}"
                -e "AG_DEPLOY_POLL_FAST_INTERVAL=1")
    local kv
    for kv in "$@"; do args+=(-e "${kv}"); done
    # Git Bash 下 -v /tmp/... 会被 MSYS 路径改写吞掉：统一转成 Windows 混合形式（C:/...）
    local out_host="${out}" params_host="${params}"
    if command -v cygpath >/dev/null 2>&1; then
      out_host="$(cygpath -m "${out}")"
      [ -n "${params_host}" ] && params_host="$(cygpath -m "${params_host}")"
    fi
    [ -n "${params_host}" ] && args+=(-v "${params_host}:/workspace/input/params.yaml:ro")
    # MSYS 自动路径转换会破坏挂载参数（容器写进被调包的目标）→ 仅对 docker run 禁用；
    # 挂载路径已用 cygpath -m 显式转成 Windows 形式，build 等其他 docker 调用不受影响。
    MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" docker run --rm "${args[@]}" \
      -v "${out_host}:/workspace/output" "${DEPLOY_IMAGE}" || rc=$?
  else
    local envs=(AG_TASK_ID="${task_id}" AG_ATTEMPT=1 AG_RUNNER_SEQ=1 AG_OUTPUT_DIR="${out}"
                AG_GENET_BASE_URL="${BASE_URL}" GENET_API_KEY="${TOKEN}"
                AG_DEPLOY_POLL_FAST_INTERVAL=1)
    local kv
    for kv in "$@"; do envs+=("${kv}"); done
    [ -n "${params}" ] && envs+=(AG_PARAMS_FILE="${params}")
    env "${envs[@]}" bash "${DEPLOY_DIR}/run.sh" || rc=$?
  fi
  RUN_OUT_DIR="${out}"
  return "${rc}"
}

# assert_state <case名> <期望phase> <期望failure.code|none> —— json.load + Parse 等价校验
assert_state() {
  local case_name="$1" want_phase="$2" want_failure="$3"
  local f="${WORK}/${case_name}/out/deploy-state.json"
  if [ ! -s "$f" ]; then bad "${case_name}: deploy-state.json 缺失或为空"; return 1; fi
  # 任务验收口径：python -c "import json;json.load(...)" 可解析（路径走 argv，勿内嵌字符串——
  # 内嵌时 Git Bash 的 MSYS 路径转换不会生效，Windows python 会打不开 /tmp/... 形态的路径）。
  if "${PY}" -c "import json,sys;json.load(open(sys.argv[1],encoding='utf-8'))" "$f" >/dev/null 2>&1; then
    ok "${case_name}: deploy-state.json 可被 json.load 解析"
  else
    bad "${case_name}: deploy-state.json 不是合法 JSON"
  fi
  local vout; vout="$("${PY}" "${HERE}/validate_deploystate.py" "$f" 2>&1)"
  if [ $? -eq 0 ]; then ok "${case_name}: ${vout}"; else bad "${case_name}: ${vout}"; fi
  check_eq "${case_name}: phase=${want_phase}" "${want_phase}" "$(jf "$f" 'd["phase"]')"
  local got_failure
  got_failure="$(jf "$f" '(d.get("failure") or {}).get("code", "none")')"
  check_eq "${case_name}: failure.code=${want_failure}" "${want_failure}" "${got_failure}"
}

echo "== service-deploy 冒烟测试（mode=${MODE}）=="

if [ "${MODE}" = "docker" ]; then
  echo "-- 构建 ${DEPLOY_IMAGE}（build/Dockerfile，上下文=Runner 根目录）"
  if ! docker build -q -f "${DEPLOY_DIR}/build/Dockerfile" -t "${DEPLOY_IMAGE}" "${DEPLOY_DIR}" >/dev/null 2>&1; then
    echo "   默认源构建失败，改用清华 apk 镜像重试（网络环境旋钮 APK_MIRROR）"
    docker build -q --build-arg APK_MIRROR=https://mirrors.tuna.tsinghua.edu.cn/alpine \
      -f "${DEPLOY_DIR}/build/Dockerfile" -t "${DEPLOY_IMAGE}" "${DEPLOY_DIR}" >/dev/null || {
      echo "镜像构建失败"; exit 1; }
  fi
fi

# ---- 部署描述 fixture（Genet 部署描述原文；values 透传）------------------------
PARAMS="${WORK}/params.yaml"
cat > "${PARAMS}" <<'EOF'
# Genet 部署描述（原文透传为 values）
replicaCount: 1
model: Qwen3-32B
model_path: /models/Qwen3-32B
package_type: stack
EOF
EMPTY_PARAMS="${WORK}/params-empty.yaml"
: > "${EMPTY_PARAMS}"

# ============== S1 ready ==============
echo "-- S1 ready：POST 202 → 轮询 → ready（exports 四件套）"
CASE="s1"; start_stub "${CASE}" --ready-after 0
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
RN="$(relname t-s1 1)"
run_deploy "${CASE}" "${PARAMS}" "t-s1" "AG_DEPLOY_TIMEOUT=60"; RC=$?
check_eq "S1: exit=0" "0" "${RC}"
E="${RUN_OUT_DIR}/exports.env"
for k in AG_SERVED_URL AG_SERVED_MODEL_NAME AG_MODEL_PATH AG_DEPLOY_REF; do
  grep -q "^${k}=" "${E}" 2>/dev/null && ok "S1: exports.env 含 ${k}" || bad "S1: exports.env 缺 ${k}"
done
check_eq "S1: AG_SERVED_URL（Genet 返回值原样）" \
  "http://${RN}-mock-llm.argus-svc.svc:18080/v1" "$(sed -n 's/^AG_SERVED_URL=//p' "${E}" 2>/dev/null)"
check_eq "S1: AG_SERVED_MODEL_NAME" "Qwen3-32B" "$(sed -n 's/^AG_SERVED_MODEL_NAME=//p' "${E}" 2>/dev/null)"
check_eq "S1: AG_MODEL_PATH（描述字段）" "/models/Qwen3-32B" "$(sed -n 's/^AG_MODEL_PATH=//p' "${E}" 2>/dev/null)"
check_eq "S1: AG_DEPLOY_REF（ns/releaseName）" "argus-svc/${RN}" "$(sed -n 's/^AG_DEPLOY_REF=//p' "${E}" 2>/dev/null)"
assert_state "${CASE}" "ready" "none"
F="${RUN_OUT_DIR}/deploy-state.json"
check_eq "S1: deploy-state.release_name" "${RN}" "$(jf "$F" 'd["release_name"]')"
check_eq "S1: deploy-state.genet_status=deployed" "deployed" "$(jf "$F" 'd["genet_status"]')"
check_eq "S1: deploy-state.endpoint_status=ready" "ready" "$(jf "$F" 'd["endpoint_status"]')"
check_eq "S1: queue.position 恒 null（D-46）" "null" "$(jf "$F" 'd["queue"]["position"]')"
check_eq "S1: deploy-state.exports 与 exports.env 四件套一致" "4" "$(jf "$F" 'len(d["exports"])')"
check_contains "S1: label_selector" "helm-release-name=${RN}" "$(jf "$F" 'd["label_selector"]')"
check_eq "S1: POST 恰好 1 次" "1" "$(counter post_install)"
stop_stub

# ============== S2 幂等恢复（GET 200，不重发 POST）==============
echo "-- S2 幂等恢复：release 已存在（GET 200）→ 直接轮询，绝不 POST"
CASE="s2"; RN="$(relname t-s2 1)"
start_stub "${CASE}" --preinstall "${RN}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_deploy "${CASE}" "${PARAMS}" "t-s2" "AG_DEPLOY_TIMEOUT=60"; RC=$?
check_eq "S2: exit=0" "0" "${RC}"
check_eq "S2: 未发 POST（幂等恢复）" "0" "$(counter post_install)"
assert_state "${CASE}" "ready" "none"
stop_stub

# ============== S3 POST 400 冲突 → GET 复核幂等 ==============
echo "-- S3 POST 400(protected metadata) → GET 复核当幂等 → 宽限窗后 ready"
CASE="s3"; RN="$(relname t-s3 1)"
start_stub "${CASE}" --preinstall "${RN}" --delayed-visible 4
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_deploy "${CASE}" "${PARAMS}" "t-s3" "AG_DEPLOY_TIMEOUT=60"; RC=$?
check_eq "S3: exit=0" "0" "${RC}"
check_eq "S3: POST 恰好 1 次（400 后不再重发）" "1" "$(counter post_install)"
assert_state "${CASE}" "ready" "none"
stop_stub

# ============== S4 blocked（helm failed，D-47 折叠）==============
echo "-- S4 blocked：release failed → DEPLOY_BLOCKED"
CASE="s4"; start_stub "${CASE}" --fail-after 3
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
RN="$(relname t-s4 1)"
run_deploy "${CASE}" "${PARAMS}" "t-s4" "AG_DEPLOY_TIMEOUT=60"; RC=$?
check_eq "S4: exit=1" "1" "${RC}"
assert_state "${CASE}" "blocked" "DEPLOY_BLOCKED"
F="${RUN_OUT_DIR}/deploy-state.json"
check_eq "S4: genet_status=failed（原值保留）" "failed" "$(jf "$F" 'd["genet_status"]')"
check_contains "S4: failure.message 带 releaseName 上下文" "${RN}" "$(jf "$F" 'd["failure"]["message"]')"
stop_stub

# ============== S5 blocked（endpoint.status=blocked）==============
echo "-- S5 blocked：deployed 但 endpoint blocked → DEPLOY_BLOCKED"
CASE="s5"; start_stub "${CASE}" --ready-after 1 --endpoint-blocked
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_deploy "${CASE}" "${PARAMS}" "t-s5" "AG_DEPLOY_TIMEOUT=60"; RC=$?
check_eq "S5: exit=1" "1" "${RC}"
assert_state "${CASE}" "blocked" "DEPLOY_BLOCKED"
F="${RUN_OUT_DIR}/deploy-state.json"
check_eq "S5: genet_status=deployed 且 endpoint_status=blocked" "deployed|blocked" \
  "$(jf "$F" 'd["genet_status"]+"|"+d["endpoint_status"]')"
stop_stub

# ============== S6 窗口用尽 → DEPLOY_BLOCKED(timeout) ==============
echo "-- S6 窗口用尽：AG_DEPLOY_TIMEOUT 用尽 → DEPLOY_BLOCKED(reason=timeout)"
CASE="s6"
run_deploy "${CASE}" "${PARAMS}" "t-s6" "AG_DEPLOY_TIMEOUT=3" "AG_GENET_BASE_URL=http://127.0.0.1:9"; RC=$?
check_eq "S6: exit=1" "1" "${RC}"
assert_state "${CASE}" "blocked" "DEPLOY_BLOCKED"
F="${RUN_OUT_DIR}/deploy-state.json"
check_eq "S6: failure.reason=timeout" "timeout" "$(jf "$F" 'd["failure"]["reason"]')"
stop_stub

# ============== S7 缺部署描述 → 退出码 2 ==============
echo "-- S7 缺部署描述（三通道皆无）→ exit 2"
CASE="s7"; start_stub "${CASE}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_deploy "${CASE}" "" "t-s7" "AG_DEPLOY_TIMEOUT=10"; RC=$?
check_eq "S7: exit=2（参数错误，任何执行动作前）" "2" "${RC}"
check_eq "S7: 未发任何 POST" "0" "$(counter post_install)"
stop_stub

# ============== S8 空部署描述 → 退出码 2 ==============
echo "-- S8 空部署描述（values/templatePath 全空）→ exit 2"
CASE="s8"; start_stub "${CASE}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_deploy "${CASE}" "${EMPTY_PARAMS}" "t-s8" "AG_DEPLOY_TIMEOUT=10"; RC=$?
check_eq "S8: exit=2" "2" "${RC}"
check_eq "S8: 未发任何 POST（参数预检拦截，§9.2）" "0" "$(counter post_install)"
stop_stub

echo "== 结果：PASS=${PASS} FAIL=${FAIL} =="
[ "${FAIL}" -eq 0 ]
