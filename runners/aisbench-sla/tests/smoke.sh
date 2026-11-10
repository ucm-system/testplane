#!/bin/bash
# =============================================================================
# 离线冒烟（**在 Runner 镜像里**执行）：mock vLLM + stub ais_bench，不需要 NPU/权重/真 ais_bench。
#
# 它做四件事：
#   1. 在容器内起 tool/tests/mock_vllm.py（自带 /metrics 与 /__sim__/traffic）；
#   2. 用 stub_aisbench.py（假 ais_bench）跑一次真实的 /runner/run.sh，断言 result.json：
#      status=succeeded、指标名/统计量/单位/labels（附录 A 口径）、artifacts 齐全、退出码 0；
#   3. 用 stub 的可控故障参数跑出退出码 2（预检失败）与 3（无可行并发），断言
#      `result.json.error.code` 的映射（评审 #62 nit：这两条以前只有手工矩阵）；
#   4. 断言凭据不落任何可下载物：整个运行带 `ENDPOINT_API_KEY=<canary>`，
#      `grep -r` 产物目录与 work_path 必须 0 命中（评审 #62 硬问题 1 的红证）。
#
# 直接跑（在容器里）：
#   AG_OUTPUT_DIR=/workspace/output bash /tests/smoke.sh
# 从宿主机一键跑：
#   docker run --rm --entrypoint bash <stub-img> -lc "cd /runner && bash tests/smoke.sh"
#   Linux/macOS 等价命令见 README「离线冒烟」里的 docker run 一行。
#
# 环境变量：AG_OUTPUT_DIR（默认 /workspace/output）、MOCK_PORT（默认 18080）、
#           SMOKE_MAX_CONCURRENCY（默认 4）、SMOKE_MAX_PROBES（默认 2）、
#           SMOKE_API_KEY_CANARY（默认 sk-test-SECRET123，故意用明显的假 key）
# 退出码：0 全部断言通过；1 有断言失败（或 Runner 退出码非预期）；2 环境问题。
# =============================================================================
set -uo pipefail

OUT="${AG_OUTPUT_DIR:-/workspace/output}"
PORT="${MOCK_PORT:-18080}"
MAX_CONCURRENCY="${SMOKE_MAX_CONCURRENCY:-4}"
MAX_PROBES="${SMOKE_MAX_PROBES:-2}"
TOOL="${RUNNER_TOOL_DIR:-/runner/tool}"
DATASET="${SMOKE_DATASET:-/runner/fixtures/smoke_dataset.jsonl}"
MOCK_LOG="/tmp/mock-vllm.log"
CANARY="${SMOKE_API_KEY_CANARY:-sk-test-SECRET123}"
CASES="${OUT}/cases"

mkdir -p "${OUT}" || { echo "无法创建 ${OUT}" >&2; exit 2; }
# 每次从干净目录开始：上次的日志/产物会让「凭据零泄漏」扫描与断言读到旧内容。
rm -rf "${OUT}/logs" "${OUT}/raw" "${OUT}/sla_out" "${OUT}/result.json" "${CASES}" 2>/dev/null || true
mkdir -p "${OUT}" "${CASES}" || { echo "无法创建 ${OUT}" >&2; exit 2; }

echo "==> 启动 mock vLLM（127.0.0.1:${PORT}）"
python3 "${TOOL}/tests/mock_vllm.py" --host 127.0.0.1 --port "${PORT}" \
  --model-name mock-llm > "${MOCK_LOG}" 2>&1 &
MOCK_PID=$!
cleanup() { kill "${MOCK_PID}" 2>/dev/null || true; }
trap cleanup EXIT

ready=0
for _ in $(seq 1 60); do
  if python3 -c "import urllib.request,sys; urllib.request.urlopen('http://127.0.0.1:${PORT}/v1/models', timeout=1)" 2>/dev/null; then
    ready=1; break
  fi
  sleep 0.2
done
if [ "${ready}" -ne 1 ]; then
  echo "mock vLLM 起不来；日志：" >&2
  tail -n 40 "${MOCK_LOG}" >&2
  exit 2
fi

# v2：业务参数只走输入文件（JSONL，§5.3）；CLI 只留公共参数（§5.10）。
PARAMS_FILE="${TMPDIR:-/tmp}/sla-smoke-params.jsonl"
cat > "${PARAMS_FILE}" <<EOF
{"input_len":128,"output_len":8,"data_num":8,"sla":["ttft.avg:100000"],
 "max":${MAX_CONCURRENCY},"max_probes":${MAX_PROBES},
 "dataset":"${DATASET}","aisbench_cmd":"${TOOL}/tests/stub_aisbench.py",
 "work_path":"/tmp/aisbench-ws"}
EOF

BASE_ARGS=(
  --params-file="${PARAMS_FILE}"
  --url="http://127.0.0.1:${PORT}/v1"
  --model_name=mock-llm
  --model_path="/models/Qwen3-0.6B"
)

# run_case <输出目录> <说明> <start> <request_rate> <额外 env...>
# request_rate=0 时 stub 的「实测并发」等于目标并发；给一个很小的值就能复现
# 「频率成了瓶颈 → 并发压不满」的现场（评审 #62 的证据形态）。
# 退出码就是 /runner/run.sh 的退出码。
run_case() {
  local out="$1" desc="$2" start="$3" rate="$4"
  shift 4
  mkdir -p "${out}" || return 2
  echo "==> [${desc}] run.sh → ${out}（start=${start} rate=${rate}；额外 env：${*:-无}）"
  (
    export AG_OUTPUT_DIR="${out}" \
           AG_TASK_ID="${AG_TASK_ID:-smoke-task}" \
           AG_ATTEMPT="${AG_ATTEMPT:-1}" \
           AG_RUNNER_SEQ="${AG_RUNNER_SEQ:-1}" \
           AG_RUNNER_IMAGE="${AG_RUNNER_IMAGE:-local-smoke}" \
           ENDPOINT_API_KEY="${CANARY}"
    if [ "$#" -gt 0 ]; then
      env "$@" /runner/run.sh "${BASE_ARGS[@]}" --start="${start}" --request_rate="${rate}"
    else
      /runner/run.sh "${BASE_ARGS[@]}" --start="${start}" --request_rate="${rate}"
    fi
  )
}

run_case "${OUT}" "成功路径（不限速，并发达标）" 1 0
rc_ok=$?
echo "==> Runner 退出码 = ${rc_ok}"

# 请求频率 0.05 RPS：c>=2 的点在飞量被频率卡在 1，实测并发 < 目标 80% → degraded
run_case "${CASES}/degraded" "并发压不满 → 降级（request_rate=0.05）" 1 0.05
rc_degraded=$?
echo "==> Runner 退出码 = ${rc_degraded}（期望 0，但结论降级）"

run_case "${CASES}/exit2" "预检失败（STUB_FAIL_AT=1、start=2）" 2 0 STUB_FAIL_AT=1
rc_exit2=$?
echo "==> Runner 退出码 = ${rc_exit2}（期望 1 → error.code=SLA_PRECHECK_FAILED）"

run_case "${CASES}/exit3" "无可行并发（STUB_FAIL_AT=1、start=1）" 1 0 STUB_FAIL_AT=1
rc_exit3=$?
echo "==> Runner 退出码 = ${rc_exit3}（期望 1 → error.code=SLA_NO_FEASIBLE_CONCURRENCY）"

command -v grep >/dev/null 2>&1 || { echo "镜像里没有 grep（断言凭据泄漏需要它）" >&2; exit 2; }
LEAK_HITS_SEEN=0
for scope in "${OUT}" "${TOOL}"; do
  hits="$(grep -rl --binary-files=text -- "${CANARY}" "${scope}" 2>/dev/null | grep -v '^$' || true)"
  if [ -n "${hits}" ]; then
    LEAK_HITS_SEEN=1
    echo "!! 凭据出现在 ${scope} 下：" >&2
    printf '   %s\n' ${hits} >&2
  fi
done
echo "==> 凭据泄漏扫描：${CANARY} 在 ${OUT} 与 ${TOOL} 下命中 $( [ "${LEAK_HITS_SEEN}" -eq 0 ] && echo 0 || echo '>0' ) 次"

if [ ! -f "${OUT}/result.json" ]; then
  echo "!! 没有 result.json（endpoint-probe 选择产出它；产物契约三选一 D-74）" >&2
  exit 1
fi

python3 - "${OUT}" "${rc_ok}" "${CASES}/degraded" "${rc_degraded}" \
         "${CASES}/exit2" "${rc_exit2}" "${CASES}/exit3" "${rc_exit3}" "${LEAK_HITS_SEEN}" <<'PY'
import csv
import json
import os
import sys

out, rc = sys.argv[1], int(sys.argv[2])
degraded_dir, degraded_rc = sys.argv[3], int(sys.argv[4])
exit2_dir, exit2_rc = sys.argv[5], int(sys.argv[6])
exit3_dir, exit3_rc = sys.argv[7], int(sys.argv[8])
leaks = int(sys.argv[9])
problems = []


def load(path):
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


# ---------------------------------------------------------------- 成功路径
result = load(os.path.join(out, "result.json"))
if rc != 0:
    problems.append("Runner 退出码 %d（期望 0）" % rc)
if result.get("status") != "succeeded":
    problems.append("status=%r（期望 succeeded）" % result.get("status"))
if result.get("error") is not None:
    problems.append("error 非空：%r" % (result.get("error"),))

metrics = {m.get("name"): m for m in result.get("metrics") or []}
expected = {
    "sla_max_concurrency": ("count", "count"),
    "output_throughput": ("tokens_per_s", "max"),
    "ttft": ("ms", "mean"),
    "tpot": ("ms", "p90"),
    "probes_total": ("count", "count"),
}
for name, (unit, statistic) in expected.items():
    metric = metrics.get(name)
    if metric is None:
        problems.append("缺少指标 %s（实际：%s）" % (name, sorted(metrics)))
        continue
    if metric.get("unit") != unit:
        problems.append("%s.unit=%r（期望 %r）" % (name, metric.get("unit"), unit))
    if metric.get("statistic") != statistic:
        problems.append("%s.statistic=%r（期望 %r）" % (name, metric.get("statistic"), statistic))
    if not isinstance(metric.get("value"), (int, float)):
        problems.append("%s 不是数值：%r" % (name, metric))

# 名字里不得再编码统计量/单位（评审 #62 nit：跨 Run 可比较）
for legacy in ("ttft_avg_ms", "tpot_p90_ms", "hit_rate_percent", "max_output_throughput_tok_s"):
    if legacy in metrics:
        problems.append("仍在使用旧指标名 %s（名字里不该带统计量/单位）" % legacy)

if metrics.get("sla_max_concurrency", {}).get("labels", {}).get("scope") != "search":
    problems.append("sla_max_concurrency 缺 labels.scope=search")
if metrics.get("probes_total", {}).get("labels", {}).get("scope") != "search":
    problems.append("probes_total 缺 labels.scope=search")
if "concurrency" not in metrics.get("output_throughput", {}).get("labels", {}):
    problems.append("output_throughput 缺 labels.concurrency")
if "concurrency" not in metrics.get("ttft", {}).get("labels", {}):
    problems.append("ttft 缺 labels.concurrency")

workload = result.get("workload") or {}
if not isinstance(workload.get("concurrency"), (int, float)):
    problems.append("workload 缺 concurrency（附录 A 要求带并发）")

for required in ("sla_summary.json", "sla_points.csv", "run.log", "aisbench_all.log"):
    path = os.path.join(out, "sla_out", required)
    if not os.path.isfile(path):
        problems.append("缺少产物 %s" % path)
paths = [a.get("path") for a in result.get("artifacts") or []]
for required in ("sla_out/sla_summary.json", "sla_out/sla_points.csv"):
    if required not in paths:
        problems.append("artifacts 未列出 %s（实际 %r）" % (required, paths))

summary_path = os.path.join(out, "sla_out", "sla_summary.json")
if os.path.isfile(summary_path):
    summary = load(summary_path)
    if summary.get("status") not in ("ok", "max_unbounded", "none_passed"):
        problems.append("工具状态异常：%r" % summary.get("status"))
    print("==> sla_summary: status=%s max_ok=%s max_ok_measured=%s 点数=%d"
          % (summary.get("status"), summary.get("max_ok_concurrency"),
             summary.get("max_ok_concurrency_measured"),
             len(summary.get("points") or [])))
    max_ok = summary.get("max_ok_concurrency")
    reported = (metrics.get("sla_max_concurrency") or {}).get("value")
    if isinstance(max_ok, (int, float)) and reported != max_ok:
        problems.append("不限速的成功路径：sla_max_concurrency=%r 应等于 max_ok=%r"
                        % (reported, max_ok))
    if "并发点降级" in (result.get("notes") or ""):
        problems.append("不限速的成功路径不该出现降级点：%s" % (result.get("notes") or "")[:200])

# ------------------------------------------------- 降级路径（并发压不满）
deg_path = os.path.join(degraded_dir, "result.json")
if not os.path.isfile(deg_path):
    problems.append("没有降级用例的 result.json：%s" % deg_path)
else:
    deg = load(deg_path)
    deg_metrics = {m.get("name"): m for m in deg.get("metrics") or []}
    deg_summary_path = os.path.join(degraded_dir, "sla_out", "sla_summary.json")
    deg_summary = load(deg_summary_path) if os.path.isfile(deg_summary_path) else {}
    deg_max_ok = deg_summary.get("max_ok_concurrency")
    reported = (deg_metrics.get("sla_max_concurrency") or {}).get("value")
    if degraded_rc != 0:
        problems.append("降级用例：Runner 退出码 %d（期望 0：压不满不是执行失败）"
                        % degraded_rc)
    if deg.get("status") != "succeeded":
        problems.append("降级用例：status=%r（期望 succeeded）" % deg.get("status"))
    if not isinstance(deg_max_ok, (int, float)):
        problems.append("降级用例：工具没报 max_ok_concurrency（stub 配置有问题）")
    elif reported is None:
        problems.append("降级用例：没有 sla_max_concurrency（期望退回到达标的 c=1）")
    elif reported >= deg_max_ok:
        problems.append("降级用例：sla_max_concurrency=%r 等于/超过工具口径 %r —— "
                        "压不满的点被当成结论了" % (reported, deg_max_ok))
    if "并发点降级" not in (deg.get("notes") or ""):
        problems.append("降级用例：notes 没写降级说明：%s" % (deg.get("notes") or "")[:200])
    csv_path = os.path.join(degraded_dir, "sla_out", "sla_points.csv")
    if os.path.isfile(csv_path):
        with open(csv_path, encoding="utf-8", newline="") as handle:
            rows = list(csv.DictReader(handle))
        if not rows or "degraded" not in rows[0]:
            problems.append("降级用例：sla_points.csv 缺 degraded 旁注列")
        elif not any(str(r.get("degraded")).lower() == "true" for r in rows):
            problems.append("降级用例：sla_points.csv 里没有任何 degraded=true 的点")
    print("==> 降级用例：max_ok=%s → sla_max_concurrency=%s（notes 已说明）"
          % (deg_max_ok, reported))

# ---------------------------------------------------------------- 退出码 2 / 3
for label, case_dir, case_rc, want_code, want_summary in (
        ("退出码 2（预检失败）", exit2_dir, exit2_rc, "SLA_PRECHECK_FAILED", "preflight_failed"),
        ("退出码 3（无可行并发）", exit3_dir, exit3_rc, "SLA_NO_FEASIBLE_CONCURRENCY",
         "none_passed")):
    path = os.path.join(case_dir, "result.json")
    if not os.path.isfile(path):
        problems.append("[%s] 没有 result.json：%s" % (label, path))
        continue
    case = load(path)
    if case_rc != 1:
        problems.append("[%s] Runner 退出码 %d（期望 1）" % (label, case_rc))
    if case.get("status") != "failed":
        problems.append("[%s] status=%r（期望 failed）" % (label, case.get("status")))
    code = (case.get("error") or {}).get("code")
    if code != want_code:
        problems.append("[%s] error.code=%r（期望 %r）" % (label, code, want_code))
    tool_status = ((case.get("error") or {}).get("detail") or {}).get("tool_status")
    if tool_status != want_summary:
        problems.append("[%s] detail.tool_status=%r（期望 %r）"
                        % (label, tool_status, want_summary))
    print("==> %s：rc=%d status=%s code=%s" % (label, case_rc, case.get("status"), code))

# ---------------------------------------------------------------- 凭据不泄漏
if leaks != 0:
    problems.append("凭据 canary 出现在产物目录或 work_path 里（见上方扫描输出）")

print("==> metrics: " + ", ".join(
    "%s=%s(%s/%s)" % (name, m.get("value"), m.get("unit"), m.get("statistic"))
    for name, m in metrics.items()))
if problems:
    print("!! 断言失败：", file=sys.stderr)
    for problem in problems:
        print("   - " + problem, file=sys.stderr)
    sys.exit(1)
print("==> 冒烟通过：成功路径指标口径正确、退出码 2/3 映射正确、凭据零泄漏")
PY
assert_rc=$?

exit "${assert_rc}"
