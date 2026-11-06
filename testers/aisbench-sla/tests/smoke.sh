#!/bin/bash
# =============================================================================
# 离线冒烟（**在 Tester 镜像里**执行）：mock vLLM + stub ais_bench，不需要 NPU/权重/真 ais_bench。
#
# 它做三件事：
#   1. 在容器内起 tool/tests/mock_vllm.py（自带 /metrics 与 /__sim__/traffic）；
#   2. 用 stub_aisbench.py（假 ais_bench）跑一次真实的 /tester/run.sh；
#   3. 断言 result.json：status=succeeded、metrics 有 sla_max_concurrency、
#      artifacts 含 sla_summary.json / sla_points.csv，退出码 0。
#
# 直接跑（在容器里）：
#   OUTPUT_DIR=/workspace/output bash /tests/smoke.sh
# 从宿主机一键跑：
#   pwsh testers/aisbench-sla/tests/smoke.ps1        # 负责 docker run + 宿主侧断言
#   Linux/macOS 等价命令见 README「离线冒烟」里的 docker run 一行。
#
# 环境变量：OUTPUT_DIR（默认 /workspace/output）、MOCK_PORT（默认 18080）、
#           SMOKE_MAX_CONCURRENCY（默认 4）、SMOKE_MAX_PROBES（默认 2）
# 退出码：0 全部断言通过；1 有断言失败（或 Tester 退出码非 0）；2 环境问题。
# =============================================================================
set -uo pipefail

OUT="${OUTPUT_DIR:-/workspace/output}"
PORT="${MOCK_PORT:-18080}"
MAX_CONCURRENCY="${SMOKE_MAX_CONCURRENCY:-4}"
MAX_PROBES="${SMOKE_MAX_PROBES:-2}"
TOOL="${TESTER_TOOL_DIR:-/tester/tool}"
DATASET="${SMOKE_DATASET:-/tester/fixtures/smoke_dataset.jsonl}"
MOCK_LOG="/tmp/mock-vllm.log"

mkdir -p "${OUT}" || { echo "无法创建 ${OUT}" >&2; exit 2; }

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

echo "==> 调 /tester/run.sh（stub ais_bench，离线）"
TESTPLANE_RUN_ID="${TESTPLANE_RUN_ID:-smoke}" \
TESTPLANE_TASK_ID="${TESTPLANE_TASK_ID:-smoke-task}" \
TESTPLANE_ATTEMPT_ID="${TESTPLANE_ATTEMPT_ID:-smoke-a1}" \
TESTPLANE_TESTER_IMAGE="${TESTPLANE_TESTER_IMAGE:-local-smoke}" \
  /tester/run.sh \
    --url="http://127.0.0.1:${PORT}/v1" \
    --model_name=mock-llm \
    --model_path="${TOOL}" \
    --input_len=128 --output_len=8 --data_num=8 \
    --sla=ttft.avg:100000 \
    --start=1 --max="${MAX_CONCURRENCY}" --max_probes="${MAX_PROBES}" \
    --dataset="${DATASET}" \
    --aisbench_cmd="${TOOL}/tests/stub_aisbench.py" \
    --work_path="${TOOL}"
rc=$?

echo "==> Tester 退出码 = ${rc}"
if [ ! -f "${OUT}/result.json" ]; then
  echo "!! 没有 result.json（违反 §5.9）" >&2
  exit 1
fi

python3 - "$OUT" "$rc" <<'PY'
import json
import os
import sys

out, rc = sys.argv[1], int(sys.argv[2])
problems = []

with open(os.path.join(out, "result.json"), encoding="utf-8") as handle:
    result = json.load(handle)

if rc != 0:
    problems.append("Tester 退出码 %d（期望 0）" % rc)
if result.get("status") != "succeeded":
    problems.append("status=%r（期望 succeeded）" % result.get("status"))
if result.get("error") is not None:
    problems.append("error 非空：%r" % (result.get("error"),))

metrics = {m.get("name"): m for m in result.get("metrics") or []}
for name in ("sla_max_concurrency", "max_output_throughput_tok_s", "probes_total"):
    if name not in metrics:
        problems.append("缺少指标 %s" % name)
if "sla_max_concurrency" in metrics and not isinstance(metrics["sla_max_concurrency"].get("value"), (int, float)):
    problems.append("sla_max_concurrency 不是数值：%r" % (metrics["sla_max_concurrency"],))

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
    with open(summary_path, encoding="utf-8") as handle:
        summary = json.load(handle)
    if summary.get("status") not in ("ok", "max_unbounded", "none_passed"):
        problems.append("工具状态异常：%r" % summary.get("status"))
    print("==> sla_summary: status=%s max_ok=%s 点数=%d"
          % (summary.get("status"), summary.get("max_ok_concurrency"),
             len(summary.get("points") or [])))

keys = ["sla_max_concurrency", "max_output_throughput_tok_s", "ttft_avg_ms",
        "tpot_p90_ms", "hit_rate_percent", "probes_total"]
print("==> metrics: " + ", ".join("%s=%s" % (k, metrics[k]["value"]) for k in keys if k in metrics))
if problems:
    print("!! 断言失败：", file=sys.stderr)
    for problem in problems:
        print("   - " + problem, file=sys.stderr)
    sys.exit(1)
print("==> 冒烟通过：status=succeeded，metrics 与产物齐全")
PY
assert_rc=$?

exit "${assert_rc}"
