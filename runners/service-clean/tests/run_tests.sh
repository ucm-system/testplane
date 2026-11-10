#!/bin/bash
# =============================================================================
# service-clean 内置 Runner 冒烟测试（stub Genet，不依赖真实集群 / genet-local）
#
# 覆盖路径（execution §9.4 / 任务验收三条路径 + 幂等补充）：
#   C1 已干净（幂等）  ：release 不存在 + AG_DEPLOY_REF 缺失 → 确定性兜底（日志注明 fallback）
#                        → DELETE 404 → GET 404 且 pods items==[] → exit 0
#   C2 需删除          ：release 存在 + AG_DEPLOY_REF=ns/name → DELETE 200 → 验证干净 → exit 0
#   C3 删除后验证      ：DELETE 受理但 release 延迟消失 → 验证循环重试到干净 → exit 0
#   C4 判不干净        ：release 一直删不掉 → 窗口（AG_CLEAN_VERIFY_TIMEOUT）用尽 → exit 1
#                        （平台记 cleanup_failed，不改 Task 终态）
#   C5 查询失败 ≠ 为空 ：release 已 404 但 pods 端点 500（R9）→ 不判成功 → exit 1
#   C6 参数错误        ：AG_DEPLOY_REF 形态不合法 → exit 2（拒绝执行，防错杀）
#
# 运行方式（自动选择）：
#   - native：宿主机有 bash+curl+jq → 直接跑 run.sh；
#   - docker：否则构建 build/Dockerfile（argus-service-clean:dev）并在容器内跑。
# 用法：bash runners/service-clean/tests/run_tests.sh
# =============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CLEAN_DIR="$(cd "${HERE}/.." && pwd)"
CLEAN_IMAGE="${CLEAN_IMAGE:-argus-service-clean:dev}"
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

WORK="$(mktemp -d "${TMPDIR:-/tmp}/svc-clean-tests.XXXXXX")"
PASS=0; FAIL=0; STUB_PID=""; STUB_PORT=""

cleanup() {
  [ -n "${STUB_PID}" ] && kill "${STUB_PID}" 2>/dev/null
  rm -rf "${WORK}" 2>/dev/null
}
trap cleanup EXIT

ok()  { printf '  [PASS] %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }
check_eq() {
  if [ "$3" = "$2" ]; then ok "$1"; else bad "$1（want=[$2] got=[$3]）"; fi
}
check_contains() {
  case "$3" in *"$2"*) ok "$1" ;; *) bad "$1（未包含 [$2]）" ;; esac
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
  local i2
  for i2 in $(seq 1 50); do
    curl -s -o /dev/null --max-time 1 "http://127.0.0.1:${STUB_PORT}/__counters" && break
    sleep 0.1
  done
}
stop_stub() { [ -n "${STUB_PID}" ] && kill "${STUB_PID}" 2>/dev/null && wait "${STUB_PID}" 2>/dev/null; STUB_PID=""; }

counters() { curl -s --max-time 3 "http://127.0.0.1:${STUB_PORT}/__counters"; }
counter()  { counters | "${PY}" -c 'import json,sys; print(json.load(sys.stdin)["counters"][sys.argv[1]])' "$1" 2>/dev/null; }

relname() { printf 'arg-%s' "$(printf '%s' "$1:$2" | sha256sum | cut -c1-12)"; }

BASE_URL=""   # runner 视角的 stub 地址（docker 用 host.docker.internal）

# run_clean <case名> <task_id> [KEY=VAL ...] —— 跑一次 runner，回传 RC
run_clean() {
  local case_name="$1" task_id="$2"; shift 2
  local out="${WORK}/${case_name}/out"
  mkdir -p "${out}"
  local rc=0
  if [ "${MODE}" = "docker" ]; then
    local args=(-e "AG_TASK_ID=${task_id}" -e "AG_ATTEMPT=1" -e "AG_RUNNER_SEQ=1"
                -e "AG_OUTPUT_DIR=/workspace/output"
                -e "AG_GENET_BASE_URL=${BASE_URL}" -e "GENET_API_KEY=${TOKEN}"
                -e "AG_CLEAN_POLL_INITIAL_INTERVAL=1")
    local kv
    for kv in "$@"; do args+=(-e "${kv}"); done
    # Git Bash 下 -v /tmp/... 会被 MSYS 路径改写吞掉：统一转成 Windows 混合形式（C:/...）
    local out_host="${out}"
    command -v cygpath >/dev/null 2>&1 && out_host="$(cygpath -m "${out}")"
    # MSYS 自动路径转换会破坏挂载参数 → 仅对 docker run 禁用（路径已 cygpath -m 转换）。
    MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" docker run --rm "${args[@]}" \
      -v "${out_host}:/workspace/output" "${CLEAN_IMAGE}" || rc=$?
  else
    local envs=(AG_TASK_ID="${task_id}" AG_ATTEMPT=1 AG_RUNNER_SEQ=1 AG_OUTPUT_DIR="${out}"
                AG_GENET_BASE_URL="${BASE_URL}" GENET_API_KEY="${TOKEN}"
                AG_CLEAN_POLL_INITIAL_INTERVAL=1)
    local kv
    for kv in "$@"; do envs+=("${kv}"); done
    env "${envs[@]}" bash "${CLEAN_DIR}/run.sh" || rc=$?
  fi
  RUN_OUT_DIR="${out}"
  return "${rc}"
}

echo "== service-clean 冒烟测试（mode=${MODE}）=="

if [ "${MODE}" = "docker" ]; then
  echo "-- 构建 ${CLEAN_IMAGE}（build/Dockerfile，上下文=Runner 根目录）"
  if ! docker build -q -f "${CLEAN_DIR}/build/Dockerfile" -t "${CLEAN_IMAGE}" "${CLEAN_DIR}" >/dev/null 2>&1; then
    echo "   默认源构建失败，改用清华 apk 镜像重试（网络环境旋钮 APK_MIRROR）"
    docker build -q --build-arg APK_MIRROR=https://mirrors.tuna.tsinghua.edu.cn/alpine \
      -f "${CLEAN_DIR}/build/Dockerfile" -t "${CLEAN_IMAGE}" "${CLEAN_DIR}" >/dev/null || {
      echo "镜像构建失败"; exit 1; }
  fi
fi

# ============== C1 已干净（幂等；AG_DEPLOY_REF 缺失 → fallback）==============
echo "-- C1 已干净：release 不存在 + 无 AG_DEPLOY_REF → 确定性兜底 + DELETE 404 → exit 0"
CASE="c1"; start_stub "${CASE}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
RN="$(relname t-c1 1)"
run_clean "${CASE}" "t-c1" "AG_CLEAN_VERIFY_TIMEOUT=30"; RC=$?
check_eq "C1: exit=0（资源已不存在=幂等成功）" "0" "${RC}"
check_contains "C1: 日志注明 fallback" "fallback" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
check_contains "C1: 兜底 releaseName=${RN}" "${RN}" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
check_eq "C1: DELETE 恰好 1 次" "1" "$(counter delete)"
check_eq "C1: pods 查询已发生（判干净两条件，D-50）" "1" "$(counter get_pods)"
stop_stub

# ============== C2 需删除（AG_DEPLOY_REF 优先）==============
echo "-- C2 需删除：release 存在 + AG_DEPLOY_REF → DELETE 200 → 验证干净 → exit 0"
CASE="c2"; RN="$(relname t-c2 1)"
start_stub "${CASE}" --preinstall "${RN}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_clean "${CASE}" "t-c2" "AG_DEPLOY_REF=argus-svc/${RN}" "AG_CLEAN_VERIFY_TIMEOUT=30"; RC=$?
check_eq "C2: exit=0" "0" "${RC}"
check_contains "C2: releaseName 来源=AG_DEPLOY_REF" "AG_DEPLOY_REF" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
check_eq "C2: DELETE 恰好 1 次" "1" "$(counter delete)"
check_eq "C2: stub 中 release 已消失（GET 会 404）" "0" \
  "$(counters | "${PY}" -c 'import json,sys; print(len(json.load(sys.stdin)["releases"]))')"
check_eq "C2: pods 端点确认 items==[]（查询成功）" "1" "$(counter get_pods)"
stop_stub

# ============== C3 删除后验证（DELETE 受理但延迟消失）==============
echo "-- C3 删除后验证：DELETE 200 但 release 延迟 2s 消失 → 循环重试 → exit 0"
CASE="c3"; RN="$(relname t-c3 1)"
start_stub "${CASE}" --preinstall "${RN}" --delete-pending 2
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_clean "${CASE}" "t-c3" "AG_DEPLOY_REF=argus-svc/${RN}" "AG_CLEAN_VERIFY_TIMEOUT=30"; RC=$?
check_eq "C3: exit=0" "0" "${RC}"
GETS="$(counter get_release)"
case "${GETS}" in ''|0|1) bad "C3: 验证循环确有重试（get_release=${GETS}）" ;; *) ok "C3: 验证循环确有重试（get_release=${GETS}）" ;; esac
check_contains "C3: 日志记录过 release 仍在" "release 仍在" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
stop_stub

# ============== C4 判不干净（窗口用尽）==============
echo "-- C4 判不干净：release 一直删不掉 → AG_CLEAN_VERIFY_TIMEOUT 用尽 → exit 1"
CASE="c4"; RN="$(relname t-c4 1)"
start_stub "${CASE}" --preinstall "${RN}" --delete-pending 9999
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_clean "${CASE}" "t-c4" "AG_DEPLOY_REF=argus-svc/${RN}" "AG_CLEAN_VERIFY_TIMEOUT=4"; RC=$?
check_eq "C4: exit=1（cleanup_failed 语义，不改 Task 终态）" "1" "${RC}"
check_contains "C4: 日志含窗口与最后观测" "AG_CLEAN_VERIFY_TIMEOUT=4" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
stop_stub

# ============== C5 查询失败 ≠ 为空（R9）==============
echo "-- C5 查询失败≠为空：release 已 404 但 pods 端点 500 → 不判成功 → exit 1"
CASE="c5"; RN="$(relname t-c5 1)"
start_stub "${CASE}" --preinstall "${RN}" --pods-500
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_clean "${CASE}" "t-c5" "AG_DEPLOY_REF=argus-svc/${RN}" "AG_CLEAN_VERIFY_TIMEOUT=4"; RC=$?
check_eq "C5: exit=1（绝不误报清理成功）" "1" "${RC}"
check_contains "C5: 日志记录查询失败" "查询失败" "$(cat "${RUN_OUT_DIR}/logs/clean.log" 2>/dev/null)"
check_eq "C5: stub 中 release 实际已删（说明 GET 404 为真、失败在 pods 端点）" "0" \
  "$(counters | "${PY}" -c 'import json,sys; print(len(json.load(sys.stdin)["releases"]))')"
stop_stub

# ============== C6 参数错误（AG_DEPLOY_REF 不合法）==============
echo "-- C6 参数错误：AG_DEPLOY_REF 形态不合法 → exit 2，未发任何请求"
CASE="c6"; start_stub "${CASE}"
if [ "${MODE}" = "docker" ]; then BASE_URL="http://host.docker.internal:${STUB_PORT}"; else BASE_URL="http://127.0.0.1:${STUB_PORT}"; fi
run_clean "${CASE}" "t-c6" "AG_DEPLOY_REF=argus-svc/Bad_Release!" "AG_CLEAN_VERIFY_TIMEOUT=10"; RC=$?
check_eq "C6: exit=2（参数错误，任何执行动作前）" "2" "${RC}"
check_eq "C6: 未发 DELETE（防错杀）" "0" "$(counter delete)"
stop_stub

echo "== 结果：PASS=${PASS} FAIL=${FAIL} =="
[ "${FAIL}" -eq 0 ]
