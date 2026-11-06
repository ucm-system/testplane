#!/bin/bash
# =============================================================================
# TestPlane Tester: aisbench-sla —— 前缀缓存 SLA 搜索（最大并发 / 最高吞吐）
#
# 契约：design/ARCHITECTURE.md §5.3（argv + env + 输出）、§5.4.1（公共参数）、
#       §6.6（超时）、§6.7（独占）、附录 A（result.json）
#
# 参数全部来自命令行（平台与独立运行完全同构）：
#   /tester/run.sh --input_len=2048 --output_len=32 --data_num=160 \
#                  --sla=ttft.avg:1200 --start=8 --max=256 \
#                  --work_path=/benchmark \
#                  --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B \
#                  --model_path=/models/Qwen3-32B
#   平台会把 --url / --model_name / --model_path 追加在末尾（§5.4.1），人手工跑时自己写。
#
# 运行元信息走环境变量：TESTPLANE_RUN_ID / TESTPLANE_TASK_ID / TESTPLANE_ATTEMPT_ID
#                        TESTPLANE_TESTER_IMAGE
# 凭据走环境变量：ENDPOINT_API_KEY（不进 argv、不落日志）
# 产物写到 /workspace/output（可用 OUTPUT_DIR 覆盖）：
#   result.json（必须）、logs/sla.log、raw/、sla_out/（工具原生产物：sla_summary.json、
#   sla_points.csv、run.log、aisbench_all.log、points/**）
#
# 退出码（与 result.json.status 一致）：
#   0  succeeded                        工具退出码 0（找到可行并发；含 max_unbounded）
#   1  failed   SLA_PRECHECK_FAILED            工具退出码 2：预检（并发=1）不达标
#   1  failed   SLA_NO_FEASIBLE_CONCURRENCY    工具退出码 3：没有任何并发点达标
#   1  failed   TESTER_ERROR                   工具退出码 1 / 异常 / 本脚本参数错误
#   2  failed   TESTER_ERROR                   本脚本参数错误（未知参数、缺必填、类型不对）
# =============================================================================
set -uo pipefail   # 不用 -e：任何一步失败也要把 result.json 写出来（§5.9）

VERSION="0.1.0"
OUT="${OUTPUT_DIR:-/workspace/output}"
LOG_DIR="${OUT}/logs"
RAW_DIR="${OUT}/raw"
SLA_OUT="${OUT}/sla_out"
RESULT="${OUT}/result.json"
# vendored 工具根（镜像里固定在 /tester/tool；测试时可用 TESTER_TOOL_DIR 覆盖）
TOOL_DIR="${TESTER_TOOL_DIR:-/tester/tool}"
TOOL_SLA="${TOOL_DIR}/sla_search.py"

mkdir -p "${LOG_DIR}" "${RAW_DIR}" 2>/dev/null || { echo "无法创建 ${LOG_DIR}" >&2; exit 2; }
LOG="${LOG_DIR}/sla.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}" >&2; }

RUN_ID="${TESTPLANE_RUN_ID:-local}"
TASK_ID="${TESTPLANE_TASK_ID:-local}"
ATTEMPT_ID="${TESTPLANE_ATTEMPT_ID:-local}"
IMAGE_REF="${TESTPLANE_TESTER_IMAGE:-unknown}"

STARTED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START_EPOCH="$(date +%s)"

usage() {
  cat >&2 <<'EOF'
用法: /tester/run.sh [--key=value ...]

公共参数（§5.4.1，平台会自动追加）:
  --url=<OpenAI 兼容 base URL>   必填，形如 http://host:8000/v1（末段 /v1 会去掉再交给工具）
  --model_name=<服务端模型名>
  --model_path=<容器内 tokenizer/权重路径>   工具要求该目录存在

负载与搜索:
  --input_len=2048 --output_len=32 --data_num=64
  --sla=ttft.avg:1200 [--sla=tpot.avg:30 ...]   必填，可重复，且 AND
  --start=1 --max=256 --max_probes=20 --tolerance=1 --max_bisect=5
  --request_rate=3（0=不限速） --dataset_type=prefix_cache|normal
  --prefix_num=1 --repeat_rate=90% --seed=1 --dp=1

数据与环境:
  --dataset=<现成 jsonl> --dataset_path=<数据集落盘目录>
  --work_path=<ais_bench 源码根，必填且可写> --aisbench_cmd=ais_bench
  --aisbench_extra_args=<追加参数> --point_timeout=0
  --prefix_warmup=per-point|once|none --no_stream=true|false

示例（离线冒烟，用镜像自带的 stub_aisbench）:
  --url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/tester/tool \
  --input_len=128 --output_len=8 --data_num=8 --sla=ttft.avg:100000 \
  --start=1 --max=4 --max_probes=2 --dataset=/tester/fixtures/smoke_dataset.jsonl \
  --aisbench_cmd=/tester/tool/tests/stub_aisbench.py --work_path=/tester/tool
EOF
}

# --- 参数（全部 --key=value；平台只做词法切分，语义解释在这里，§5.3）------------
URL=""; MODEL_NAME=""; MODEL_PATH=""
INPUT_LEN=""; OUTPUT_LEN=""; DATA_NUM=""
SLA_ITEMS=()
START_C=""; MAX_C=""; MAX_PROBES=""; TOLERANCE=""; MAX_BISECT=""
REQUEST_RATE=""; DATASET_TYPE=""; PREFIX_NUM=""; REPEAT_RATE=""; SEED=""; DP=""
DATASET=""; DATASET_PATH=""
WORK_PATH=""; AISBENCH_CMD=""; AISBENCH_EXTRA=""; POINT_TIMEOUT=""; PREFIX_WARMUP=""
NO_STREAM="false"
HELP_REQUESTED="false"
UNKNOWN=()

while [ $# -gt 0 ]; do
  arg="$1"; shift
  case "${arg}" in
    --help|-h) HELP_REQUESTED="true"; continue ;;
    --*=*)
      key="${arg#--}"; key="${key%%=*}"; val="${arg#*=}" ;;
    --*)
      # 布尔开关允许裸写：--no_stream 等价 --no_stream=true
      key="${arg#--}"; val=""
      case "${key}" in
        no_stream) val="true" ;;
        *) UNKNOWN+=("${arg}"); continue ;;
      esac ;;
    *)
      # 不是 --key=value 形式：记下来，稍后统一报错（保持 Tester 对参数形态严格）
      UNKNOWN+=("${arg}"); continue ;;
  esac

  case "${key}" in
    # 公共参数（平台注入或调用方填写）
    url)                  URL="${val}" ;;
    model_name)           MODEL_NAME="${val}" ;;
    model_path)           MODEL_PATH="${val}" ;;
    # 负载形态
    input_len)            INPUT_LEN="${val}" ;;
    output_len)           OUTPUT_LEN="${val}" ;;
    data_num)             DATA_NUM="${val}" ;;
    # SLA（可重复）
    sla)                  SLA_ITEMS+=("${val}") ;;
    # 搜索控制
    start)                START_C="${val}" ;;
    max)                  MAX_C="${val}" ;;
    max_probes)           MAX_PROBES="${val}" ;;
    tolerance)            TOLERANCE="${val}" ;;
    max_bisect)           MAX_BISECT="${val}" ;;
    request_rate)         REQUEST_RATE="${val}" ;;
    dataset_type)         DATASET_TYPE="${val}" ;;
    prefix_num)           PREFIX_NUM="${val}" ;;
    repeat_rate)          REPEAT_RATE="${val}" ;;
    seed)                 SEED="${val}" ;;
    dp)                   DP="${val}" ;;
    # 数据与环境
    dataset)              DATASET="${val}" ;;
    dataset_path)         DATASET_PATH="${val}" ;;
    work_path)            WORK_PATH="${val}" ;;
    aisbench_cmd)         AISBENCH_CMD="${val}" ;;
    aisbench_extra_args)  AISBENCH_EXTRA="${val}" ;;
    point_timeout)        POINT_TIMEOUT="${val}" ;;
    prefix_warmup)        PREFIX_WARMUP="${val}" ;;
    no_stream)            NO_STREAM="${val:-true}" ;;
    *)                    UNKNOWN+=("${arg}") ;;
  esac
done

is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
is_num()  { case "$1" in ''|*[!0-9.]*|*.*.*) return 1 ;; *) return 0 ;; esac; }
as_bool() {
  case "$1" in
    true|1|yes|on|"") printf 'true' ;;
    false|0|no|off)   printf 'false' ;;
    *) return 1 ;;
  esac
}

# --- result.json 写入（无论成败都必须产出，§5.9）--------------------------------
# 用 python3 写：镜像里没有 jq（只有 requests 一个 pip 依赖），而 python3 一定在。
write_result() {
  TP_OUT="${OUT}" TP_VERSION="${VERSION}" \
  TP_RUN_ID="${RUN_ID}" TP_TASK_ID="${TASK_ID}" TP_ATTEMPT_ID="${ATTEMPT_ID}" \
  TP_IMAGE="${IMAGE_REF}" TP_STARTED="${STARTED}" TP_START_EPOCH="${START_EPOCH}" \
  TP_URL="${URL}" TP_MODEL_NAME="${MODEL_NAME}" TP_MODEL_PATH="${MODEL_PATH}" \
  TP_INPUT_LEN="${INPUT_LEN}" TP_OUTPUT_LEN="${OUTPUT_LEN}" TP_DATA_NUM="${DATA_NUM}" \
  TP_START="${START_C}" TP_MAX="${MAX_C}" TP_MAX_PROBES="${MAX_PROBES}" \
  TP_REQUEST_RATE="${REQUEST_RATE}" TP_DATASET_TYPE="${DATASET_TYPE}" \
  TP_DATASET="${DATASET}" TP_DATASET_PATH="${DATASET_PATH}" TP_WORK_PATH="${WORK_PATH}" \
  TP_AISBENCH_CMD="${AISBENCH_CMD}" TP_SLA_LIST="$(printf '%s\n' ${SLA_ITEMS[@]+"${SLA_ITEMS[@]}"})" \
  python3 - <<'PYRESULT'
import hashlib
import json
import os
import sys
import time

OUT = os.environ.get("TP_OUT", "/workspace/output")
SLA_OUT = os.path.join(OUT, "sla_out")
RESULT = os.path.join(OUT, "result.json")
MAX_ARTIFACT_BYTES = 50 * 1024 * 1024


def env(name, default=""):
    value = os.environ.get(name)
    return default if value is None else value


def as_number(name, cast=float):
    raw = env(name).strip()
    if raw == "":
        return None
    try:
        return cast(raw)
    except ValueError:
        return None


status = env("TP_STATUS", "failed").strip() or "failed"
code = env("TP_CODE").strip()
reason = env("TP_REASON").strip()
extra_note = env("TP_EXTRA_NOTE").strip()
retryable = env("TP_RETRYABLE", "false").strip() == "true"
tool_rc = as_number("TP_RC", int)
tool_version = env("TP_TOOL_VERSION").strip()
version = env("TP_VERSION")
start_epoch = as_number("TP_START_EPOCH", int) or int(time.time())
finished = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
duration_ms = max(0, int((time.time() - start_epoch) * 1000))

# ---------------------------------------------------------------- 工具摘要
summary = None
summary_problem = ""
summary_path = os.path.join(SLA_OUT, "sla_summary.json")
try:
    with open(summary_path, "r", encoding="utf-8") as handle:
        summary = json.load(handle)
except FileNotFoundError:
    summary_problem = "没有 sla_out/sla_summary.json（工具没跑到写摘要那一步）"
except (OSError, ValueError) as exc:
    summary_problem = "sla_out/sla_summary.json 无法解析：%s" % exc

if not tool_version and isinstance(summary, dict):
    tool_version = str(summary.get("tool") or "").replace("aisbench-sla ", "").strip()

points = []
max_ok = None
if isinstance(summary, dict):
    points = summary.get("points") or []
    max_ok = summary.get("max_ok_concurrency")

metrics = []
missing = []

# 1) SLA 内最大并发
if isinstance(max_ok, (int, float)) and not isinstance(max_ok, bool):
    metrics.append({
        "name": "sla_max_concurrency",
        "value": int(max_ok),
        "unit": "count",
        "statistic": "count",
        "labels": {},
    })
else:
    missing.append("sla_max_concurrency（sla_summary.json 里没有 max_ok_concurrency）")

# 2) 最高输出吞吐（可能出现在不达标的点上——工具把「峰值」与「SLA 内最高」分开报）
peak = {}
throughput = summary.get("throughput") if isinstance(summary, dict) else None
if isinstance(throughput, dict):
    peak = throughput.get("peak") or {}
peak_value = peak.get("value") if isinstance(peak, dict) else None
if isinstance(peak_value, (int, float)) and not isinstance(peak_value, bool) and peak_value >= 0:
    labels = {}
    if peak.get("concurrency") is not None:
        labels["concurrency"] = int(peak["concurrency"])
    metrics.append({
        "name": "max_output_throughput_tok_s",
        "value": round(float(peak_value), 3),
        "unit": "tokens_per_s",
        "statistic": "max",
        "labels": labels,
    })
else:
    missing.append("max_output_throughput_tok_s（没有任何点的输出吞吐可解析）")

# 3) 最大并发点的 TTFT AVG / TPOT P90 / HBM 命中率
max_point = None
if isinstance(max_ok, (int, float)) and not isinstance(max_ok, bool):
    same = [p for p in points if isinstance(p, dict)
            and p.get("concurrency") is not None
            and int(p.get("concurrency")) == int(max_ok)]
    probes = [p for p in same if str(p.get("kind")) != "preflight"]
    max_point = (probes or same or [None])[0]

if max_point is None:
    if points:
        missing.append("ttft_avg_ms/tpot_p90_ms/hit_rate_percent"
                       "（没有与 max_ok_concurrency 对应的探测点）")


def point_metric(point, key, name, unit, statistic, labels, scale=1.0):
    raw = point.get(key) if isinstance(point, dict) else None
    if isinstance(raw, (int, float)) and not isinstance(raw, bool) and raw >= 0:
        metrics.append({
            "name": name,
            "value": round(float(raw) * scale, 3),
            "unit": unit,
            "statistic": statistic,
            "labels": labels,
        })
        return True
    return False


hit_note = ""
if max_point is not None:
    label = {"concurrency": int(max_ok)}
    if not point_metric(max_point, "ttft_avg_ms", "ttft_avg_ms", "ms", "mean", label):
        missing.append("ttft_avg_ms（该点 TTFT 无数据）")
    if not point_metric(max_point, "tpot_p90_ms", "tpot_p90_ms", "ms", "p90", label):
        missing.append("tpot_p90_ms（该点 TPOT 无数据）")
    hit_raw = max_point.get("hbm_hit_rate")
    if isinstance(hit_raw, (int, float)) and not isinstance(hit_raw, bool):
        metrics.append({
            "name": "hit_rate_percent",
            "value": round(float(hit_raw) * 100, 2),
            "unit": "percent",
            "statistic": "mean",
            "labels": label,
        })
    else:
        hit_note = str(max_point.get("hit_rate_note") or "").strip()

# 4) 探测点数
if isinstance(summary, dict):
    probes = [p for p in points if isinstance(p, dict) and str(p.get("kind")) != "warmup"]
    metrics.append({
        "name": "probes_total",
        "value": len(probes),
        "unit": "count",
        "statistic": "count",
        "labels": {},
    })
else:
    missing.append("probes_total（没有探测点记录）")

# ---------------------------------------------------------------- 产物
def sha256_of(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def tail_truncate(path, limit):
    size = os.path.getsize(path)
    with open(path, "rb") as handle:
        handle.seek(size - limit)
        data = handle.read()
    tmp = path + ".truncated"
    with open(tmp, "wb") as handle:
        handle.write(data)
    os.replace(tmp, path)
    return size


candidates = [
    "sla_out/sla_summary.json",
    "sla_out/sla_points.csv",
    "sla_out/run.log",
    "sla_out/aisbench_all.log",
]
points_dir = os.path.join(SLA_OUT, "points")
for root, _dirs, files in os.walk(points_dir):
    for name in sorted(files):
        rel = os.path.relpath(os.path.join(root, name), OUT).replace(os.sep, "/")
        candidates.append(rel)

artifacts = []
truncated = []
for rel in candidates:
    full = os.path.join(OUT, rel)
    if not os.path.isfile(full):
        continue
    size = os.path.getsize(full)
    was_truncated = False
    if size > MAX_ARTIFACT_BYTES:
        original = tail_truncate(full, MAX_ARTIFACT_BYTES)
        truncated.append("%s（%d 字节 → 保留末尾 %d 字节）" % (rel, original, MAX_ARTIFACT_BYTES))
        size = os.path.getsize(full)
        was_truncated = True
    entry = {
        "name": rel[len("sla_out/"):] if rel.startswith("sla_out/") else rel,
        "path": rel,
        "size_bytes": size,
        "sha256": sha256_of(full),
    }
    if was_truncated:
        entry["truncated"] = True
    artifacts.append(entry)

# ---------------------------------------------------------------- 结论与说明
notes = []
if reason:
    notes.append(reason)
if extra_note:
    notes.append(extra_note)
if isinstance(summary, dict):
    slas = summary.get("sla_labels") or []
    if slas:
        notes.append("SLA 口径：" + "；".join(str(s) for s in slas))
    note = str(summary.get("note") or "").strip()
    if note:
        notes.append(note)
    error = str(summary.get("error") or "").strip()
    if error:
        notes.append("工具报错：" + error)
    for warning in (summary.get("warnings") or [])[:3]:
        notes.append("注意：" + str(warning))
if summary_problem:
    notes.append(summary_problem)
if hit_note:
    notes.append("hit_rate_percent 未提供：" + hit_note)
if missing:
    notes.append("未写入的指标：" + "；".join(missing))
if truncated:
    notes.append("产物超 50MB 已截断（保留末尾）：" + "；".join(truncated))
notes_text = "；".join(n for n in notes if n)
if len(notes_text) > 4000:
    notes_text = notes_text[:3999] + "…"

error_obj = None
if status != "succeeded":
    detail = {"tool_exit_code": tool_rc, "summary": "sla_out/sla_summary.json"}
    if isinstance(summary, dict):
        detail["tool_status"] = summary.get("status")
        detail["sla_max_concurrency"] = summary.get("max_ok_concurrency")
    error_obj = {
        "code": code or "TESTER_ERROR",
        "message": (reason or "aisbench-sla 执行失败")[:1000],
        "detail": detail,
        "retryable": retryable,
    }

workload = {
    "base_url": env("TP_URL"),
    "model_name": env("TP_MODEL_NAME"),
    "model_path": env("TP_MODEL_PATH"),
    "input_len": as_number("TP_INPUT_LEN", int),
    "output_len": as_number("TP_OUTPUT_LEN", int),
    "data_num": as_number("TP_DATA_NUM", int),
    "sla": [line for line in env("TP_SLA_LIST").splitlines() if line.strip()],
    "start": as_number("TP_START", int),
    "max_concurrency": as_number("TP_MAX", int),
    "max_probes": as_number("TP_MAX_PROBES", int),
    "request_rate": as_number("TP_REQUEST_RATE", float),
    "dataset_type": env("TP_DATASET_TYPE"),
    "dataset": env("TP_DATASET"),
    "dataset_path": env("TP_DATASET_PATH"),
    "work_path": env("TP_WORK_PATH"),
    "aisbench_cmd": env("TP_AISBENCH_CMD"),
}
workload = {key: value for key, value in workload.items() if value not in (None, "", [])}

payload = {
    "schema_version": "1.0",
    "run_id": env("TP_RUN_ID", "local") or "local",
    "task_id": env("TP_TASK_ID", "local") or "local",
    "attempt_id": env("TP_ATTEMPT_ID", "local") or "local",
    "tester": {
        "name": "aisbench-sla",
        "version": version,
        "image_ref": env("TP_IMAGE", "unknown"),
    },
    "status": status,
    "started_at": env("TP_STARTED"),
    "finished_at": finished,
    "duration_ms": duration_ms,
    "workload": workload,
    "metrics": metrics,
    "error": error_obj,
    "artifacts": artifacts,
    "notes": notes_text,
    "metadata": {
        "tool": "aisbench-sla",
        "tool_version": tool_version or "unknown",
        "tool_source": "vendored e271cb3bdddb714e526b0f8e5627c036e579d409",
        "output_dir": "sla_out",
    },
}

tmp = RESULT + ".tmp"
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(payload, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
os.replace(tmp, RESULT)
print("[tester] result.json: status=%s metrics=%d artifacts=%d"
      % (status, len(metrics), len(artifacts)), file=sys.stderr)
PYRESULT
}

# 参数错误统一出口：写 result.json（status=failed / TESTER_ERROR）后退出 2
param_error() {
  local message="$1"
  log "参数错误：${message}"
  usage
  TP_STATUS=failed TP_CODE=TESTER_ERROR TP_REASON="${message}" \
  TP_RETRYABLE=false TP_RC="" TP_EXTRA_NOTE="本脚本参数校验未通过，未执行任何压测" \
    write_result || log "result.json 写入失败"
  exit 2
}

# --- 参数校验（运行时才校验，平台不校验，§5.4）--------------------------------
if [ "${HELP_REQUESTED}" = "true" ]; then
  param_error "收到 --help，未执行任何测试（用法见上方）"
fi
if [ "${#UNKNOWN[@]}" -gt 0 ]; then
  param_error "无法识别的参数：${UNKNOWN[*]}（只接受 --key=value）"
fi
[ -n "${URL}" ]        || param_error "缺少 --url（公共参数，平台会自动注入）"
[ -n "${WORK_PATH}" ]  || param_error "缺少 --work_path（ais_bench 源码根，工具要求存在且可写）"
[ "${#SLA_ITEMS[@]}" -gt 0 ] || param_error "缺少 --sla（SLA 搜索至少需要一条约束，如 --sla=ttft.avg:1200）"

is_uint "${INPUT_LEN:-2048}"   || param_error "--input_len 必须是非负整数"
is_uint "${OUTPUT_LEN:-32}"    || param_error "--output_len 必须是非负整数"
is_uint "${DATA_NUM:-64}"      || param_error "--data_num 必须是非负整数"
is_uint "${START_C:-1}"        || param_error "--start 必须是正整数"
is_uint "${MAX_C:-256}"        || param_error "--max 必须是正整数"
is_uint "${MAX_PROBES:-20}"    || param_error "--max_probes 必须是正整数"
is_uint "${TOLERANCE:-1}"      || param_error "--tolerance 必须是正整数"
is_uint "${MAX_BISECT:-5}"     || param_error "--max_bisect 必须是非负整数"
is_uint "${SEED:-1}"           || param_error "--seed 必须是非负整数"
is_uint "${DP:-1}"             || param_error "--dp 必须是正整数"
is_uint "${PREFIX_NUM:-1}"     || param_error "--prefix_num 必须是正整数"
is_num  "${REQUEST_RATE:-3}"   || param_error "--request_rate 必须是非负数（0=不限速）"
is_num  "${POINT_TIMEOUT:-0}"  || param_error "--point_timeout 必须是非负数（秒，0=不限）"
NO_STREAM="$(as_bool "${NO_STREAM}")" || param_error "--no_stream 只能是 true 或 false"
case "${DATASET_TYPE:-prefix_cache}" in prefix_cache|normal) ;; *) param_error "--dataset_type 只能是 prefix_cache 或 normal" ;; esac
case "${PREFIX_WARMUP:-per-point}" in per-point|once|none) ;; *) param_error "--prefix_warmup 只能是 per-point / once / none" ;; esac

# --- url 归一化：平台给 OpenAI base URL（.../v1），工具要服务根地址 -----------------
# 工具自己拼 /v1/models（探活）和 ais_bench 的 url 字段；直接传带 /v1 的地址会变成
# .../v1/v1/chat/completions。所以这里去掉末尾斜杠和末尾的 /v1 段。
TARGET="${URL}"
while [ "${TARGET%/}" != "${TARGET}" ]; do TARGET="${TARGET%/}"; done
case "${TARGET}" in
  */v1) TARGET="${TARGET%/v1}" ;;
esac

# --- work_path 兜底创建 ---------------------------------------------------------
# 平台 Executor 把根文件系统设成只读（ReadOnlyRootFilesystem），只有 /workspace/output 与 /tmp
# 可写；所以平台上 --work_path 通常得指 /tmp 下的目录。工具要求该目录存在，这里不存在就建。
# 真实 ais_bench 场景下 work_path 应指向镜像里已存在的 /benchmark，不会走到这个分支。
if [ ! -d "${WORK_PATH}" ]; then
  if mkdir -p "${WORK_PATH}" 2>/dev/null; then
    log "work_path 不存在，已创建：${WORK_PATH}"
  else
    log "work_path 不存在且无法创建：${WORK_PATH}（只读根文件系统？请改指 /tmp 下的可写目录）"
  fi
fi

# --- 组装工具命令（逐项 argv，不经过 shell，§5.3）-------------------------------
CMD=(python3 "${TOOL_SLA}" search)
add_opt() { [ -n "$2" ] && CMD+=("$1" "$2"); return 0; }

add_opt --target      "${TARGET}"
add_opt --model       "${MODEL_NAME}"
add_opt --model-path  "${MODEL_PATH}"
add_opt --input-len   "${INPUT_LEN}"
add_opt --output-len  "${OUTPUT_LEN}"
add_opt --data-num    "${DATA_NUM}"
add_opt --start       "${START_C}"
add_opt --max         "${MAX_C}"
add_opt --max-probes  "${MAX_PROBES}"
add_opt --tolerance   "${TOLERANCE}"
add_opt --max-bisect  "${MAX_BISECT}"
add_opt --request-rate "${REQUEST_RATE}"
add_opt --dataset-type "${DATASET_TYPE}"
add_opt --prefix-num  "${PREFIX_NUM}"
add_opt --repeat-rate "${REPEAT_RATE}"
add_opt --seed        "${SEED}"
add_opt --dp          "${DP}"
add_opt --dataset     "${DATASET}"
add_opt --dataset-path "${DATASET_PATH}"
add_opt --work-path   "${WORK_PATH}"
add_opt --aisbench-cmd "${AISBENCH_CMD}"
add_opt --aisbench-extra-args "${AISBENCH_EXTRA}"
add_opt --point-timeout "${POINT_TIMEOUT}"
add_opt --prefix-warmup "${PREFIX_WARMUP}"
for item in ${SLA_ITEMS[@]+"${SLA_ITEMS[@]}"}; do CMD+=(--sla "${item}"); done
if [ "${NO_STREAM}" = "true" ]; then CMD+=(--no-stream); fi
# --dataset 与「每点换 seed 现生成数据集」互斥：工具在 seed_per_point（默认开）下会忽略 --dataset
# 去重新生成数据集（要 transformers；本镜像没有）。给了现成数据集就复用同一份——代价是各点命中率
# 会逐点饱和，工具会在 warnings 里明说。见 README「已知限制」。
if [ -n "${DATASET}" ]; then
  CMD+=(--no-seed-per-point)
  log "给了 --dataset：按复用同一份数据集运行（关闭每点换 seed，避免现生成）"
fi
CMD+=(--output-dir "${SLA_OUT}" --no-color --no-banner)

# 凭据只走环境变量、不进 argv 日志（§12.4）：追加在最后，且不写进日志。
if [ -n "${ENDPOINT_API_KEY:-}" ]; then CMD+=(--api-key "${ENDPOINT_API_KEY}"); fi

# stub_aisbench 用它定位被改写过的模型配置（真 ais_bench 不读这个变量，无害）。
# 见 tool/tests/stub_aisbench.py 的 STUB_WORK_DIR 说明。
export STUB_WORK_DIR="${WORK_PATH}"
export PYTHONUNBUFFERED=1

log "SLA 搜索：target=${TARGET} model=${MODEL_NAME:-<未指定>} work_path=${WORK_PATH}"
log "SLA 约束：$(printf '%s ' ${SLA_ITEMS[@]+"${SLA_ITEMS[@]}"})| 并发 ${START_C:-1}→${MAX_C:-256} 预算 ${MAX_PROBES:-20} 点"
printf '[%s] 命令：%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(printf '%q ' "${CMD[@]}")" >> "${LOG}"
if [ -n "${ENDPOINT_API_KEY:-}" ]; then log "已从环境变量注入 ENDPOINT_API_KEY（不打印）"; fi

# --- 执行：stdout/stderr 同时落盘（tee），退出码取工具自己的 ---------------------
"${CMD[@]}" 2>&1 | tee -a "${LOG}"
RC="${PIPESTATUS[0]}"

case "${RC}" in
  0) STATUS="succeeded"; CODE=""; EXIT_CODE=0; RETRYABLE="false"
     REASON="工具退出码 0：已得到 SLA 搜索结论" ;;
  2) STATUS="failed"; CODE="SLA_PRECHECK_FAILED"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具退出码 2：预检（并发=1）不满足 SLA，阈值与服务实际能力矛盾" ;;
  3) STATUS="failed"; CODE="SLA_NO_FEASIBLE_CONCURRENCY"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具退出码 3：没有任何并发点满足 SLA" ;;
  1) STATUS="failed"; CODE="TESTER_ERROR"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具退出码 1：参数或执行错误（详见 sla_out/run.log）" ;;
  *) STATUS="failed"; CODE="TESTER_ERROR"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具异常退出（exit=${RC}，例如被信号终止、OOM 或解释器缺失）" ;;
esac

# 退出码 0 但没产出摘要 = 工具本身没跑通，不能算成功（§5.8 的"执行完成"要有结果）
if [ "${RC}" -eq 0 ] && [ ! -f "${SLA_OUT}/sla_summary.json" ]; then
  STATUS="failed"; CODE="TESTER_ERROR"; EXIT_CODE=1
  REASON="工具退出码 0 但没有产出 sla_out/sla_summary.json（结果不完整）"
fi
if [ "${RC}" -eq 0 ] && [ -f "${SLA_OUT}/sla_summary.json" ]; then
  # dry-run 之类的"没真正压测"的形态不应被当成成功
  if grep -q '"status": "dry_run"' "${SLA_OUT}/sla_summary.json" 2>/dev/null; then
    STATUS="failed"; CODE="TESTER_ERROR"; EXIT_CODE=1
    REASON="工具只做了 dry-run，没有真实压测结果"
  fi
fi

log "结论：status=${STATUS} code=${CODE:-<无>} rc=${RC} ${REASON}"
TP_TOOL_VERSION="$(sed -n 's/.*"tool": "aisbench-sla \([^"]*\)".*/\1/p' "${SLA_OUT}/sla_summary.json" 2>/dev/null | head -n1)"
export TP_TOOL_VERSION

if ! TP_STATUS="${STATUS}" TP_CODE="${CODE}" TP_REASON="${REASON}" \
     TP_RETRYABLE="${RETRYABLE}" TP_RC="${RC}" TP_EXTRA_NOTE="" write_result; then
  log "result.json 写入失败（违反 §5.9），按参数错误退出"
  printf '{"schema_version":"1.0","run_id":"%s","task_id":"%s","attempt_id":"%s","tester":{"name":"aisbench-sla","version":"%s","image_ref":"%s"},"status":"failed","started_at":"%s","finished_at":"%s","metrics":[],"error":{"code":"TESTER_ERROR","message":"result.json 写入失败","detail":{},"retryable":false},"artifacts":[]}\n' \
    "${RUN_ID}" "${TASK_ID}" "${ATTEMPT_ID}" "${VERSION}" "${IMAGE_REF}" "${STARTED}" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${RESULT}" 2>/dev/null || true
  exit 2
fi

log "产物：${SLA_OUT}（sla_summary.json / sla_points.csv / run.log / aisbench_all.log / points/**）"
exit "${EXIT_CODE}"
