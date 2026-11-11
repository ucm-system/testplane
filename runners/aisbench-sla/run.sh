#!/bin/bash
# =============================================================================
# Argus Runner: aisbench-sla —— 前缀缓存 SLA 搜索（最大并发 / 最高吞吐）
#
# 契约：design/ARCHITECTURE.md §5.2（entry）、§5.3（输入文件 + env + 输出）、§5.4.1（公共参数）、
#       §6.6（超时）、§6.7（独占）、附录 A（result.json schema 2.0）
#
# 调用形态（§5.3）：业务输入只走输入文件；平台执行 entry 时 argv 恒空。
#   输入文件读取优先级：--params-file <path> > $AG_PARAMS_FILE > /workspace/input/params.jsonl
#   JSONL 逐行解析，每行必须是一个 JSON 对象，例如：
#     {"sla":["ttft.avg:1200","tpot.avg:30"],"input_len":2048,"output_len":32,
#      "data_num":160,"start":8,"max":256,"request_rate":0,"work_path":"/tmp/aisbench-ws"}
#   公共参数走环境变量（§5.4.1）：AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH。
#
# 独立运行（§5.10）：公共参数可用 CLI（优先于环境变量），业务参数可用输入文件：
#   /runner/run.sh --params-file /tmp/params.jsonl \
#                  --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B \
#                  --model_path=/models/Qwen3-32B
#
# 运行元信息走环境变量：AG_TASK_ID / AG_ATTEMPT / AG_RUNNER_SEQ（缺省 local / 1 / 1，§5.10）
#
# 凭据策略（§12.4，评审 #62 硬问题 1）——key **只走环境变量**：
#   * `ENDPOINT_API_KEY` 由平台/Task secrets 注入，本脚本**不把它拼进 argv**，
#     因此容器里 `ps` 看不到它；
#   * 日志里只出现 `--api-key ***` 这样的打码形态（真的万一出现 argv 也不会泄漏），
#     且本脚本自己的日志行从不打印 key；
#   * vendored 工具把 ais_bench 模型配置写成读 `ENDPOINT_API_KEY` 的表达式，
#     **不写明文 key**，退出时再删掉该临时配置 —— 所以 key 不落日志、不留临时配置。
#
# 产物写到 /workspace/output（可用 AG_OUTPUT_DIR 覆盖）：
#   result.json（本 Runner 产物；产物契约三选一 D-74：result.json/html/csv 至少其一，本 Runner 选 result.json）、logs/sla.log、raw/、sla_out/（工具原生产物：sla_summary.json、
#   sla_points.csv、run.log、aisbench_all.log、points/**）
#
# 指标口径（附录 A / openapi `ResultMetric`：名字只放指标，统计量与单位走字段）：
#   sla_max_concurrency（count/count, labels.scope=search）
#   output_throughput（tokens_per_s/max, labels.concurrency）
#   ttft（ms/mean, labels.concurrency）  tpot（ms/p90, labels.concurrency）
#   hit_rate（percent/mean, labels.concurrency）  probes_total（count/count）
#   sla_max_concurrency **只取实测并发达标（>= 目标 80%）的 PASS 点**；不达标点标
#   degraded、不参与取值；全部降级时不输出该指标并在 notes 说明「结论不可用」。
#
# 退出码（与 result.json.status 一致）：
#   0  succeeded                        工具退出码 0（找到可行并发；含 max_unbounded）
#   1  failed   SLA_PRECHECK_FAILED            工具退出码 2：预检（并发=1）不达标
#   1  failed   SLA_NO_FEASIBLE_CONCURRENCY    工具退出码 3：没有任何并发点达标
#   1  failed   RESULT_INVALID                 工具退出码 0 但摘要缺失/损坏/dry-run（结果不可用）
#   1  failed   RUNNER_NONZERO_EXIT            工具退出码 1 / 异常退出 / work_path 不可写
#   2  failed   VALIDATION_ERROR               本脚本参数错误（未知参数、缺必填、类型不对）
#
# 错误码只能用平台码表里有的（server/internal/domain/codes.go + openapi `ErrorCode`）：
# 自造一个码不会报错，而是被平台**静默降级成 RUNNER_NONZERO_EXIT**、丢掉区分信息
# （评审 #62 就是这么发现 SLA_* 码缺失的）。
# =============================================================================
set -uo pipefail   # 不用 -e：任何一步失败也要把 result.json 写出来（§5.9）

VERSION="0.5.0"
OUT="${AG_OUTPUT_DIR:-/workspace/output}"
LOG_DIR="${OUT}/logs"
RAW_DIR="${OUT}/raw"
SLA_OUT="${OUT}/sla_out"
RESULT="${OUT}/result.json"
# 与 tool/sla_tool/search.py 的 MEASURED_CONCURRENCY_MIN_RATIO 同一口径：
# PASS 点的实测并发 / 目标并发 低于它 → 该点 degraded，不参与 sla_max_concurrency。
MEASURED_RATIO_MIN="0.8"
# vendored 工具根（镜像里固定在 /runner/tool；测试时可用 RUNNER_TOOL_DIR 覆盖）
TOOL_DIR="${RUNNER_TOOL_DIR:-/runner/tool}"
TOOL_SLA="${TOOL_DIR}/sla_search.py"

mkdir -p "${LOG_DIR}" "${RAW_DIR}" 2>/dev/null || { echo "无法创建 ${LOG_DIR}" >&2; exit 2; }
LOG="${LOG_DIR}/sla.log"
log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "${LOG}" >&2; }

# 元信息（§5.3）：平台注入；独立运行缺省 local / 1 / 1
TASK_ID="${AG_TASK_ID:-local}"
ATTEMPT_NO="${AG_ATTEMPT:-1}"
RUNNER_SEQ="${AG_RUNNER_SEQ:-1}"
# 平台当前**不注入** AG_RUNNER_IMAGE（executor 渲染 Job 时没给设这个变量），
# 所以 result.json 的 runner.image_ref 恒为 "unknown" —— 这是预期行为，不是缺陷。
IMAGE_REF="${AG_RUNNER_IMAGE:-unknown}"

STARTED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
START_EPOCH="$(date +%s)"

is_uint() { case "$1" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
num_or() { if is_uint "$1"; then printf '%s' "$1"; else printf '%s' "$2"; fi; }

usage() {
  cat >&2 <<'EOF'
用法: /runner/run.sh [--params-file <jsonl>] [--url=<base URL>] [--model_name=<name>] [--model_path=<路径>]

输入文件（JSONL，每行一个 JSON 对象；平台内由 AG_PARAMS_FILE / /workspace/input/params.jsonl 提供）:
  公共参数不进文件（§5.4.1）：url / model_name / model_path 走 AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH

负载与搜索:
  {"sla":["ttft.avg:1200","tpot.avg:30"]}        必填，多条 AND（也可给单个字符串）
  {"input_len":2048,"output_len":32,"data_num":64}
  {"start":1,"max":256,"max_probes":20,"tolerance":1,"max_bisect":5}
  {"request_rate":3}                             0=不限速
  {"dataset_type":"prefix_cache"|"normal","prefix_num":1,"repeat_rate":"90%","seed":1,"dp":1}

数据与环境:
  {"dataset":"/path/a.jsonl","dataset_path":"/path/datasets"}
  {"work_path":"/tmp/aisbench-ws"}               必填且**可写**；平台上必须落在 /tmp 下
  {"aisbench_cmd":"ais_bench","aisbench_extra_args":"","point_timeout":0,
   "prefix_warmup":"per-point","no_stream":false}

独立运行 CLI（§5.10，公共参数优先于环境变量）:
  --url=<OpenAI 兼容 base URL>   形如 http://host:8000/v1（末段 /v1 会去掉再交给工具）
  --model_name=<服务端模型名>
  --model_path=<容器内模型/权重路径>   原样透传给 ais_bench；由平台统一挂载提供（默认 /models）

示例（离线冒烟）:
  # stub 只在 **tests/Dockerfile.stub** 构建的测试镜像里（发布镜像不带 stub，
  # 真镜像用镜像自带的 ais_bench）。测试镜像里：
  echo '{"sla":["ttft.avg:100000"],"input_len":128,"output_len":8,"data_num":8,
         "dataset":"/runner/fixtures/smoke_dataset.jsonl",
         "aisbench_cmd":"/runner/tool/tests/stub_aisbench.py","work_path":"/tmp/aisbench-ws"}' > /tmp/p.jsonl
  /runner/run.sh --params-file /tmp/p.jsonl \
    --url=http://127.0.0.1:18080/v1 --model_name=mock-llm --model_path=/models/Qwen3-0.6B
EOF
}

# --- result.json 写入（本 Runner 选择产出 result.json；失败也写，D-74 三选一 + §5.9 归一化）---
# 用 python3 写：镜像里没有 jq（只有 requests 一个 pip 依赖），而 python3 一定在。
write_result() {
  AG_OUT="${OUT}" AG_VERSION="${VERSION}" \
  AG_TASK_ID="${TASK_ID}" \
  AG_ATTEMPT_NO="$(num_or "${ATTEMPT_NO}" 1)" \
  AG_RUNNER_SEQ="$(num_or "${RUNNER_SEQ}" 1)" \
  AG_IMAGE="${IMAGE_REF}" AG_STARTED="${STARTED}" AG_START_EPOCH="${START_EPOCH}" \
  AG_URL="${URL}" AG_MODEL_NAME="${MODEL_NAME}" AG_MODEL_PATH="${MODEL_PATH}" \
  AG_INPUT_LEN="${INPUT_LEN}" AG_OUTPUT_LEN="${OUTPUT_LEN}" AG_DATA_NUM="${DATA_NUM}" \
  AG_START="${START_C}" AG_MAX="${MAX_C}" AG_MAX_PROBES="${MAX_PROBES}" \
  AG_REQUEST_RATE="${REQUEST_RATE}" AG_DATASET_TYPE="${DATASET_TYPE}" \
  AG_DATASET="${DATASET}" AG_DATASET_PATH="${DATASET_PATH}" AG_WORK_PATH="${WORK_PATH}" \
  AG_AISBENCH_CMD="${AISBENCH_CMD}" AG_SLA_LIST="$(printf '%s\n' ${SLA_ITEMS[@]+"${SLA_ITEMS[@]}"})" \
  python3 - <<'PYRESULT'
import hashlib
import json
import os
import sys
import time

OUT = os.environ.get("AG_OUT", "/workspace/output")
SLA_OUT = os.path.join(OUT, "sla_out")
RESULT = os.path.join(OUT, "result.json")
MAX_ARTIFACT_BYTES = 50 * 1024 * 1024


def env(name, default=""):
    value = os.environ.get(name)
    return default if value is None else value


def as_int(name, default=None):
    raw = env(name).strip()
    if raw == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def as_number(name, cast=float):
    raw = env(name).strip()
    if raw == "":
        return None
    try:
        return cast(raw)
    except ValueError:
        return None


status = env("AG_STATUS", "failed").strip() or "failed"
code = env("AG_CODE").strip()
reason = env("AG_REASON").strip()
extra_note = env("AG_EXTRA_NOTE").strip()
retryable = env("AG_RETRYABLE", "false").strip() == "true"
tool_rc = as_number("AG_RC", int)
tool_version = env("AG_TOOL_VERSION").strip()
version = env("AG_VERSION")
start_epoch = as_number("AG_START_EPOCH", int) or int(time.time())
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
    points = [p for p in (summary.get("points") or []) if isinstance(p, dict)]
    max_ok = summary.get("max_ok_concurrency")

# 「最大并发」必须是被测出来的（评审 #62 硬问题 2）：只认实测并发达到目标并发
# MEASURED_RATIO_MIN 的 PASS 点。低于门槛（或压根没测到）的点标 degraded，
# 不参与取值——它不是「服务能扛几并发」，而是「配置把在飞量卡在几」。
MEASURED_RATIO_MIN = float(env("AG_MEASURED_RATIO_MIN", "0.8") or 0.8)


def point_concurrency(point):
    try:
        return int(point.get("concurrency"))
    except (TypeError, ValueError):
        return None


def point_passed(point):
    verdict = str(point.get("verdict") or "").strip().upper()
    if verdict in ("PASS", "FAIL"):
        return verdict == "PASS"
    return bool(point.get("ok"))


def point_is_degraded(point):
    ratio = point.get("measured_concurrency_ratio")
    if isinstance(ratio, (int, float)) and not isinstance(ratio, bool):
        return bool(point.get("degraded")) or float(ratio) < MEASURED_RATIO_MIN
    measured = point.get("measured_concurrency")
    conc = point_concurrency(point)
    if not conc or not isinstance(measured, (int, float)) or isinstance(measured, bool) \
            or measured < 0:
        return True          # 测不到 = 不能证明并发真的达到了
    return bool(point.get("degraded")) or float(measured) / float(conc) < MEASURED_RATIO_MIN


def point_ratio(point):
    ratio = point.get("measured_concurrency_ratio")
    if isinstance(ratio, (int, float)) and not isinstance(ratio, bool):
        return float(ratio)
    measured = point.get("measured_concurrency")
    conc = point_concurrency(point)
    if conc and isinstance(measured, (int, float)) and not isinstance(measured, bool) \
            and measured >= 0:
        return float(measured) / float(conc)
    return None


passing = [p for p in points if point_passed(p) and str(p.get("kind")) != "warmup"]
degraded = [p for p in passing if point_is_degraded(p)]
verified = [p for p in passing if not point_is_degraded(p)]
effective_max = max((point_concurrency(p) for p in verified
                     if point_concurrency(p) is not None), default=None)
selected = None
if effective_max is not None:
    same = [p for p in verified if point_concurrency(p) == effective_max]
    probes = [p for p in same if str(p.get("kind")) != "preflight"]
    selected = (probes or same)[0] if (probes or same) else None

metrics = []
missing = []

# 1) SLA 内最大并发（只取实测并发达标的点）
if effective_max is not None:
    metrics.append({
        "name": "sla_max_concurrency",
        "value": int(effective_max),
        "unit": "count",
        "statistic": "count",
        "labels": {"scope": "search"},
    })
elif isinstance(max_ok, (int, float)) and not isinstance(max_ok, bool):
    missing.append(
        "sla_max_concurrency（工具报 max_ok_concurrency=%s，但没有任何 PASS 点的实测并发达到"
        "目标的 %d%%，并发没被真正压满 —— 结论不可用，不输出该指标）"
        % (int(max_ok), int(MEASURED_RATIO_MIN * 100)))
else:
    missing.append("sla_max_concurrency（sla_summary.json 里没有 max_ok_concurrency）")

# 2) 最高输出吞吐（可能出现在不达标的点上——工具把「峰值」与「SLA 内最高」分开报）
peak = {}
throughput = summary.get("throughput") if isinstance(summary, dict) else None
if isinstance(throughput, dict):
    peak = throughput.get("peak") or {}
peak_value = peak.get("value") if isinstance(peak, dict) else None
peak_degraded = False
if isinstance(peak_value, (int, float)) and not isinstance(peak_value, bool) and peak_value >= 0:
    labels = {}
    if peak.get("concurrency") is not None:
        labels["concurrency"] = int(peak["concurrency"])
        peak_degraded = any(point_concurrency(p) == int(peak["concurrency"]) and point_is_degraded(p)
                            for p in passing)
    metrics.append({
        "name": "output_throughput",
        "value": round(float(peak_value), 3),
        "unit": "tokens_per_s",
        "statistic": "max",
        "labels": labels,
    })
else:
    missing.append("output_throughput（没有任何点的输出吞吐可解析）")

# 3) 选中点（实测并发达标的最大并发点）的 TTFT AVG / TPOT P90 / 缓存命中率
if selected is None:
    if points:
        missing.append("ttft/tpot/hit_rate（没有实测并发达标、且在 sla_max_concurrency 上的探测点）")


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
if selected is not None:
    label = {"concurrency": int(effective_max)}
    if not point_metric(selected, "ttft_avg_ms", "ttft", "ms", "mean", label):
        missing.append("ttft（该点 TTFT 无数据）")
    if not point_metric(selected, "tpot_p90_ms", "tpot", "ms", "p90", label):
        missing.append("tpot（该点 TPOT 无数据）")
    hit_raw = selected.get("hbm_hit_rate")
    if isinstance(hit_raw, (int, float)) and not isinstance(hit_raw, bool):
        metrics.append({
            "name": "hit_rate",
            "value": round(float(hit_raw) * 100, 2),
            "unit": "percent",
            "statistic": "mean",
            "labels": label,
        })
    else:
        hit_note = str(selected.get("hit_rate_note") or "").strip()

# 4) 探测点数
if isinstance(summary, dict):
    probes = [p for p in points if isinstance(p, dict) and str(p.get("kind")) != "warmup"]
    metrics.append({
        "name": "probes_total",
        "value": len(probes),
        "unit": "count",
        "statistic": "count",
        "labels": {"scope": "search"},
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
if degraded:
    notes.append(
        "并发点降级（实测并发 < 目标并发的 %d%%，不参与 sla_max_concurrency）：%s"
        % (int(MEASURED_RATIO_MIN * 100),
           "，".join(
               "c=%s 实测 %s（%s）" % (
                   point_concurrency(p),
                   ("测不到" if p.get("measured_concurrency") is None
                    or not isinstance(p.get("measured_concurrency"), (int, float))
                    else "%.1f" % float(p.get("measured_concurrency"))),
                   ("%d%%" % int(point_ratio(p) * 100)) if point_ratio(p) is not None else "无实测值")
               for p in degraded)))
if effective_max is None and passing:
    notes.append(
        "并发未被真正测出：所有 PASS 点的实测并发都低于目标并发 —— 不输出 "
        "sla_max_concurrency，本次结论不可用（建议 request_rate=0 且 data_num >= 4×max 后重跑）")
if peak_degraded:
    notes.append("output_throughput 取自降级点（该点实测并发低于目标并发，吞吐对应的是更低的实际并发）")
if hit_note:
    notes.append("hit_rate 未提供：" + hit_note)
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
        detail["sla_max_concurrency"] = (effective_max if effective_max is not None
                                         else summary.get("max_ok_concurrency"))
        detail["degraded_points"] = [point_concurrency(p) for p in degraded]
    error_obj = {
        "code": code or "RUNNER_NONZERO_EXIT",
        "message": (reason or "aisbench-sla 执行失败")[:1000],
        "detail": detail,
        "retryable": retryable,
    }

workload = {
    "base_url": env("AG_URL"),
    "model_name": env("AG_MODEL_NAME"),
    "model_path": env("AG_MODEL_PATH"),
    "input_len": as_number("AG_INPUT_LEN", int),
    "output_len": as_number("AG_OUTPUT_LEN", int),
    "data_num": as_number("AG_DATA_NUM", int),
    "sla": [line for line in env("AG_SLA_LIST").splitlines() if line.strip()],
    "start": as_number("AG_START", int),
    "max_concurrency": as_number("AG_MAX", int),
    # 本次结论落在哪个并发上（实测达标的最大并发）；没测出来时退回配置上限，
    # 让下游至少知道这一轮压的是哪个形态（附录 A `workload` 要求带 concurrency）。
    "concurrency": (int(effective_max) if effective_max is not None
                    else as_number("AG_MAX", int)),
    "max_probes": as_number("AG_MAX_PROBES", int),
    "request_rate": as_number("AG_REQUEST_RATE", float),
    "dataset_type": env("AG_DATASET_TYPE"),
    "dataset": env("AG_DATASET"),
    "dataset_path": env("AG_DATASET_PATH"),
    "work_path": env("AG_WORK_PATH"),
    "aisbench_cmd": env("AG_AISBENCH_CMD"),
}
workload = {key: value for key, value in workload.items() if value not in (None, "", [])}

payload = {
    "schema_version": "2.0",
    "task_id": env("AG_TASK_ID", "local") or "local",
    "attempt_no": as_int("AG_ATTEMPT_NO", 1),
    "runner_seq": as_int("AG_RUNNER_SEQ", 1),
    "runner": {
        "name": "aisbench-sla",
        "version": version,
        "image_ref": env("AG_IMAGE", "unknown"),
    },
    "status": status,
    "started_at": env("AG_STARTED"),
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
        "tool_source": "vendored e271cb3bdddb714e526b0f8e5627c036e579d409 + 本地改动（见 tool/VENDORED.md）",
        "output_dir": "sla_out",
    },
}

tmp = RESULT + ".tmp"
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(payload, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
os.replace(tmp, RESULT)
print("[runner] result.json: status=%s metrics=%d artifacts=%d"
      % (status, len(metrics), len(artifacts)), file=sys.stderr)
PYRESULT
}

# 参数错误统一出口：写 result.json（status=failed / VALIDATION_ERROR）后退出 2
param_error() {
  local message="$1"
  log "参数错误：${message}"
  usage
  AG_STATUS=failed AG_CODE=VALIDATION_ERROR AG_REASON="${message}" \
  AG_RETRYABLE=false AG_RC="" AG_EXTRA_NOTE="本脚本参数校验未通过，未执行任何压测" \
    write_result || log "result.json 写入失败"
  exit 2
}

# 运行期硬失败出口（work_path 不可写、摘要损坏等）：写 result.json 后退出 1。
# 这些都不是参数错误（参数本身合法），所以退出码用 1、code 用 RUNNER_NONZERO_EXIT。
hard_fail() {
  local message="$1"
  local note="${2:-}"
  log "失败：${message}"
  AG_STATUS=failed AG_CODE=RUNNER_NONZERO_EXIT AG_REASON="${message}" \
  AG_RETRYABLE=false AG_RC="" AG_EXTRA_NOTE="${note}" \
    write_result || log "result.json 写入失败"
  exit 1
}

# --- 输入文件解析（§5.3）：JSONL 逐行 → shell 变量 -------------------------------
# 用 python3 解析（镜像里没有 jq）：逐行 json.loads、必须为对象、白名单字段，
# sla 可给字符串数组或单个字符串（逐条展开），其余字段必须是标量（类型由平台提交期
# 校验，这里做兜底）；未声明字段从严拒绝。多行时后面的标量覆盖前面。
# 白名单（与 runner.yaml.parameters 一一对应）：
PARSED_KEYS="input_len output_len data_num start max max_probes tolerance max_bisect \
request_rate dataset_type prefix_num repeat_rate seed dp dataset dataset_path work_path \
aisbench_cmd aisbench_extra_args point_timeout prefix_warmup no_stream"

parse_params_file() {
  local file="$1"
  [ -r "${file}" ] || { param_error "输入文件不存在或不可读：${file}"; }
  local parsed
  parsed="$(mktemp "${TMPDIR:-/tmp}/aisbench-sla-params.XXXXXX")" || { hard_fail "无法创建临时文件"; }
  PARSED_TMP="${parsed}"
  if ! python3 - "${file}" >"${parsed}" <<'PYPARSE'
import json
import sys

KNOWN = set("""sla input_len output_len data_num start max max_probes tolerance max_bisect
request_rate dataset_type prefix_num repeat_rate seed dp dataset dataset_path work_path
aisbench_cmd aisbench_extra_args point_timeout prefix_warmup no_stream""".split())

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as handle:
    for lineno, line in enumerate(handle, 1):
        text = line.strip()
        if not text:
            continue
        try:
            obj = json.loads(text)
        except ValueError as exc:
            print("输入第 %d 行不是合法 JSON：%s" % (lineno, exc), file=sys.stderr)
            raise SystemExit(2)
        if not isinstance(obj, dict):
            print("输入第 %d 行不是 JSON 对象：%s" % (lineno, text[:80]), file=sys.stderr)
            raise SystemExit(2)
        for key, value in obj.items():
            if key not in KNOWN:
                print("输入第 %d 行含未声明字段：%s" % (lineno, key), file=sys.stderr)
                raise SystemExit(2)
            if key == "sla":
                items = value if isinstance(value, list) else [value]
                if not items:
                    continue
                for item in items:
                    if not isinstance(item, str):
                        print("输入第 %d 行：sla 的每条约束必须是字符串" % lineno, file=sys.stderr)
                        raise SystemExit(2)
                    print("SLA\t%s" % item)
            elif isinstance(value, bool):
                print("%s\t%s" % (key.upper(), "true" if value else "false"))
            elif isinstance(value, (int, float, str)):
                # 键转大写：与脚本内的 shell 变量名一一对应（input_len → INPUT_LEN）。
                # aisbench_extra_args 例外，shell 侧叫 AISBENCH_EXTRA（由 read 循环处理）。
                print("%s\t%s" % (key.upper(), value))
            else:
                print("输入第 %d 行：%s 必须是标量（字符串/数字/布尔）" % (lineno, key), file=sys.stderr)
                raise SystemExit(2)
PYPARSE
  then
    rm -f "${PARSED_TMP}" 2>/dev/null || true
    param_error "输入文件解析失败（见上一行平台外报错，带行号）"
  fi
  local key val
  while IFS=$'\t' read -r key val; do
    [ -n "${key}" ] || continue
    val="${val%$'\r'}"   # 防御：Windows 上自测时 python 可能写出 CRLF
    case "${key}" in
      SLA)                 SLA_ITEMS+=("${val}") ;;
      AISBENCH_EXTRA_ARGS) AISBENCH_EXTRA="${val}" ;;   # shell 侧变量名与 jsonl 键不同
      *)                   printf -v "${key}" '%s' "${val}" ;;
    esac
  done < "${PARSED_TMP}"
  rm -f "${PARSED_TMP}" 2>/dev/null || true
}

# --- CLI 参数（独立运行通道，§5.10）：只有输入文件指针与公共参数 ------------------
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
PARSED_FILE=""

while [ $# -gt 0 ]; do
  arg="$1"; shift
  case "${arg}" in
    --help|-h) HELP_REQUESTED="true"; continue ;;
    --params-file)   [ $# -ge 1 ] && { PARSED_FILE="$1"; shift; continue; } || { echo "--params-file 缺少路径" >&2; exit 2; } ;;
    --params-file=*) PARSED_FILE="${arg#*=}"; continue ;;
    --url)           [ $# -ge 1 ] && { URL="$1"; shift; continue; } || { echo "--url 缺少值" >&2; exit 2; } ;;
    --url=*)         URL="${arg#*=}"; continue ;;
    --model_name)    [ $# -ge 1 ] && { MODEL_NAME="$1"; shift; continue; } || { echo "--model_name 缺少值" >&2; exit 2; } ;;
    --model_name=*)  MODEL_NAME="${arg#*=}"; continue ;;
    --model_path)    [ $# -ge 1 ] && { MODEL_PATH="$1"; shift; continue; } || { echo "--model_path 缺少值" >&2; exit 2; } ;;
    --model_path=*)  MODEL_PATH="${arg#*=}"; continue ;;
    *)      # v2：业务参数不走 argv（§5.3）——任何 --key=value 形式的业务参数都报错
      UNKNOWN+=("${arg}") ;;
  esac
done

if [ "${HELP_REQUESTED}" = "true" ]; then
  param_error "收到 --help，未执行任何测试（用法见上方）"
fi

if [ -n "${PARSED_FILE}" ]; then
  parse_params_file "${PARSED_FILE}"                 # 显式指定（优先级最高）
elif [ -n "${AG_PARAMS_FILE:-}" ]; then
  parse_params_file "${AG_PARAMS_FILE}"              # 平台注入（§5.3）
elif [ -f /workspace/input/params.jsonl ]; then
  parse_params_file /workspace/input/params.jsonl    # 约定路径
#else 无输入文件：业务参数全空，走下面的必填校验（work_path / sla / url 都会拦下）
fi

# 公共参数回落（§5.4.1）：CLI 未给时读平台注入的环境变量
[ -n "${URL}" ]        || URL="${AG_SERVED_URL:-}"
[ -n "${MODEL_NAME}" ] || MODEL_NAME="${AG_SERVED_MODEL_NAME:-}"
[ -n "${MODEL_PATH}" ] || MODEL_PATH="${AG_MODEL_PATH:-}"

if [ "${#UNKNOWN[@]}" -gt 0 ]; then
  param_error "无法识别的参数：${UNKNOWN[*]}（v2 业务参数只走输入文件 --params-file；公共参数用 --url / --model_name / --model_path）"
fi
[ -n "${URL}" ]        || param_error "缺少被测服务地址（--url 或环境变量 AG_SERVED_URL，公共参数 §5.4.1）"
[ -n "${WORK_PATH}" ]  || param_error "缺少 work_path（输入文件字段；ais_bench 源码根，工具要求存在且可写）"
[ "${#SLA_ITEMS[@]}" -gt 0 ] || param_error "缺少 sla（输入文件字段；SLA 搜索至少需要一条约束，如 {\"sla\":[\"ttft.avg:1200\"]}）"

is_uint "${INPUT_LEN:-2048}"   || param_error "input_len 必须是非负整数"
is_uint "${OUTPUT_LEN:-32}"    || param_error "output_len 必须是非负整数"
is_uint "${DATA_NUM:-64}"      || param_error "data_num 必须是非负整数"
is_uint "${START_C:-1}"        || param_error "start 必须是正整数"
is_uint "${MAX_C:-256}"        || param_error "max 必须是正整数"
is_uint "${MAX_PROBES:-20}"    || param_error "max_probes 必须是正整数"
is_uint "${TOLERANCE:-1}"      || param_error "tolerance 必须是正整数"
is_uint "${MAX_BISECT:-5}"     || param_error "max_bisect 必须是非负整数"
is_uint "${SEED:-1}"           || param_error "seed 必须是非负整数"
is_uint "${DP:-1}"             || param_error "dp 必须是正整数"
is_uint "${PREFIX_NUM:-1}"     || param_error "prefix_num 必须是正整数"
case "${REQUEST_RATE:-3}" in ''|*[!0-9.]|*.*.*) param_error "request_rate 必须是非负数（0=不限速）" ;; esac
case "${POINT_TIMEOUT:-0}" in ''|*[!0-9.]|*.*.*) param_error "point_timeout 必须是非负数（秒，0=不限）" ;; esac
case "${NO_STREAM}" in true|false|"") ;; *) param_error "no_stream 只能是 true 或 false" ;; esac
case "${DATASET_TYPE:-prefix_cache}" in prefix_cache|normal) ;; *) param_error "dataset_type 只能是 prefix_cache 或 normal" ;; esac
case "${PREFIX_WARMUP:-per-point}" in per-point|once|none) ;; *) param_error "prefix_warmup 只能是 per-point / once / none" ;; esac

# --- url 归一化：平台给 OpenAI base URL（.../v1），工具要服务根地址 -----------------
# 工具自己拼 /v1/models（探活）和 ais_bench 的 url 字段；直接传带 /v1 的地址会变成
# .../v1/v1/chat/completions。所以这里去掉末尾斜杠和末尾的 /v1 段。
TARGET="${URL}"
while [ "${TARGET%/}" != "${TARGET}" ]; do TARGET="${TARGET%/}"; done
case "${TARGET}" in
  */v1) TARGET="${TARGET%/v1}" ;;
esac

# --- work_path 兜底创建（建不了就**硬失败**）-------------------------------------
# 平台 Executor 把根文件系统设成只读（ReadOnlyRootFilesystem），只有 /workspace/output 与 /tmp
# 可写；所以平台上 work_path 通常得指 /tmp 下的目录。工具要求该目录存在且可写，这里不存在就建。
# 以前建不了只打一行 log 继续跑（评审 #62 硬问题 5），结果是工具在写模型配置时才炸、报错看不懂；
# 现在直接 failed + RUNNER_NONZERO_EXIT，并把「容器内请用 /tmp 下的路径」写进 message。
if [ ! -d "${WORK_PATH}" ]; then
  if mkdir -p "${WORK_PATH}" 2>/dev/null; then
    log "work_path 不存在，已创建：${WORK_PATH}"
  fi
fi
if [ ! -d "${WORK_PATH}" ]; then
  hard_fail "work_path 不可写：${WORK_PATH}（目录不存在且无法创建）。容器内请用 /tmp 下的路径，例如 /tmp/aisbench-ws —— 平台 Job 的根文件系统只读，/benchmark 之类不可写" \
    "未执行任何压测；改 work_path 后重跑"
fi
if ! { : > "${WORK_PATH}/.argus-write-probe" 2>/dev/null; }; then
  hard_fail "work_path 不可写：${WORK_PATH}（目录存在但不可写）。容器内请用 /tmp 下的路径，例如 /tmp/aisbench-ws" \
    "未执行任何压测；改 work_path 后重跑"
fi
rm -f "${WORK_PATH}/.argus-write-probe" 2>/dev/null || true

# --- ais_bench 侧接线：让 work_path 下的「影子源码根」被真实 ais_bench 认到 --------
# 背景见 build/Dockerfile 头部注释：平台 Job 的根文件系统只读，写不了
# /benchmark，所以 work_path 通常是 /tmp/aisbench-ws。但 ais_bench 有两处路径
# 默认锚在它自己的源码根上，必须各接一根线：
#   1) 数据集：ais_bench 把**相对**数据集路径拼在 AIS_BENCH_DATASETS_CACHE 下
#      （benchmark/datasets/utils/datasets.py get_cache_dir()，默认=源码根），
#      指到 work_path 即可让 path='ais_bench/datasets/gsm8k' 命中工具写的目录。
#   2) 模型配置：ais_bench 支持 --config-dir，会在 <config-dir>/models 下找模型配置；
#      work_path 不是镜像自带源码根时，把它补进 ais_bench 参数（不影响用户自带参数）。
# 只改本脚本，没有改 vendored 的 sla_tool/。
export AIS_BENCH_DATASETS_CACHE="${AIS_BENCH_DATASETS_CACHE:-${WORK_PATH}}"
if [ "${WORK_PATH}" != "/benchmark" ]; then
  AISBENCH_EXTRA="${AISBENCH_EXTRA:+${AISBENCH_EXTRA} }--config-dir ${WORK_PATH}/ais_bench/benchmark/configs"
fi

# --- 开跑前清掉上次残留的 sla_out ------------------------------------------------
# 本地复用同一个 AG_OUTPUT_DIR 时，上次的 sla_summary.json / points 会被当成这次的结果
# （评审 #62 nit）。平台每次是新的 emptyDir，这一步只是让两种运行同构。
if [ -d "${SLA_OUT}" ]; then
  rm -rf "${SLA_OUT}" 2>/dev/null && log "已清理上次残留的 ${SLA_OUT}"
fi
mkdir -p "${SLA_OUT}" 2>/dev/null || hard_fail "无法创建产物目录 ${SLA_OUT}（产物必须落在 /workspace/output 下）"

# --- 组装工具命令（逐项 argv，不经过 shell，§5.3）-------------------------------
CMD=(python3 "${TOOL_SLA}" search)
add_opt() { [ -n "$2" ] && CMD+=("$1" "$2"); return 0; }

add_opt --target      "${TARGET}"
add_opt --model       "${MODEL_NAME}"
add_opt --model-path  "${MODEL_PATH}"
# 未传的数值参数不进 argv，由工具用自己的默认值（与 runner.yaml 的 default 一致）
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
# dataset 与「每点换 seed 现生成数据集」互斥：工具在 seed_per_point（默认开）下会忽略 dataset
# 去重新生成数据集（要 transformers；本镜像没有）。给了现成数据集就复用同一份——代价是各点命中率
# 会逐点饱和，工具会在 warnings 里明说。见 README「已知限制」。
if [ -n "${DATASET}" ]; then
  CMD+=(--no-seed-per-point)
  log "给了 dataset：按复用同一份数据集运行（关闭每点换 seed，避免现生成）"
fi
CMD+=(--output-dir "${SLA_OUT}" --no-color --no-banner)

# 凭据只走环境变量（§12.4）：**不拼进 argv**（容器里 `ps` 看不见），工具自己从
# ENDPOINT_API_KEY 读（tool/sla_tool/config.py 的 normalize），模型配置里写的是读该变量的
# 表达式，子进程执行时在内存里注入 —— 磁盘、argv、日志三处都没有明文 key。
# 万一将来有人把 key 拼回 argv，打印时也会被 mask_argv 打码成 ***。
mask_argv() {
  local out="" prev="" arg
  for arg in "$@"; do
    if [ "${prev}" = "--api-key" ]; then
      out+="*** "
    else
      out+="$(printf '%q ' "${arg}")"
    fi
    prev="${arg}"
  done
  printf '%s' "${out}"
}

# stub_aisbench 用它定位被改写过的模型配置（真 ais_bench 不读这个变量，无害）。
# 见 tool/tests/stub_aisbench.py 的 STUB_WORK_DIR 说明。
export STUB_WORK_DIR="${WORK_PATH}"
export PYTHONUNBUFFERED=1

# 退出时删掉工具逐点写下的临时 ais_bench 模型配置。它本身**不含凭据**（api_key 写的是
# 读环境变量的表达式），删它是为了让 work_path 指到产物目录时不留下临时文件。
MODEL_CFG="${WORK_PATH}/ais_bench/benchmark/configs/models/vllm_api/vllm_api_chat_temp.py"
cleanup_model_cfg() {
  if [ -f "${MODEL_CFG}" ]; then
    rm -f "${MODEL_CFG}" 2>/dev/null && log "已删除临时模型配置（不含凭据）：${MODEL_CFG}"
  fi
}
trap cleanup_model_cfg EXIT

log "SLA 搜索：target=${TARGET} model=${MODEL_NAME:-<未指定>} work_path=${WORK_PATH}"
log "SLA 约束：$(printf '%s ' ${SLA_ITEMS[@]+"${SLA_ITEMS[@]}"})| 并发 ${START_C:-1}→${MAX_C:-256} 预算 ${MAX_PROBES:-20} 点"
printf '[%s] 命令：%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(mask_argv "${CMD[@]}")" >> "${LOG}"
if [ -n "${ENDPOINT_API_KEY:-}" ]; then
  log "已通过环境变量注入 ENDPOINT_API_KEY（不进 argv、不落日志、不写临时配置）"
fi

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
  1) STATUS="failed"; CODE="RUNNER_NONZERO_EXIT"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具退出码 1：参数或执行错误（详见 sla_out/run.log）" ;;
  *) STATUS="failed"; CODE="RUNNER_NONZERO_EXIT"; EXIT_CODE=1; RETRYABLE="false"
     REASON="工具异常退出（exit=${RC}，例如被信号终止、OOM 或解释器缺失）" ;;
esac

# 退出码 0 但摘要不可用 = 工具没跑通，不能算成功（§5.8 的"执行完成"要有结果）。
# **不能只检查文件存在**（评审 #62 nit）：截断/空文件/没有 points 都会让 result.json
# 记成 succeeded + 0 指标，下游拿不到任何可消费的结论。
if [ "${RC}" -eq 0 ]; then
  SUMMARY_PROBLEM="$(python3 - "${SLA_OUT}/sla_summary.json" <<'PYCHECK'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
except FileNotFoundError:
    print("没有产出 sla_out/sla_summary.json（工具没跑到写摘要那一步）")
    raise SystemExit(0)
except (OSError, ValueError) as exc:
    print("sla_out/sla_summary.json 无法解析：%s" % exc)
    raise SystemExit(0)
if not isinstance(data, dict):
    print("sla_out/sla_summary.json 不是 JSON 对象")
    raise SystemExit(0)
points = data.get("points")
if not isinstance(points, list) or not points:
    print("sla_out/sla_summary.json 的 points 为空（没有任何探测点）")
    raise SystemExit(0)
for key in ("status", "max_ok_concurrency", "sla", "tool"):
    if key not in data:
        print("sla_out/sla_summary.json 缺少关键字段 %s" % key)
        raise SystemExit(0)
if str(data.get("status") or "") == "dry_run":
    print("工具只做了 dry-run，没有真实压测结果")
    raise SystemExit(0)
if not any(isinstance(p, dict) and p.get("concurrency") is not None for p in points):
    print("sla_out/sla_summary.json 的 points 里没有可用的并发点")
    raise SystemExit(0)
PYCHECK
)"
  if [ -n "${SUMMARY_PROBLEM}" ]; then
    # 工具退出码 0 但结果不可用：RESULT_INVALID 比 RUNNER_NONZERO_EXIT 更贴切
    # （不是非零退出，是"拿到了结果但不合法"），且是平台码表里有的码。
    STATUS="failed"; CODE="RESULT_INVALID"; EXIT_CODE=1
    REASON="工具退出码 0 但 ${SUMMARY_PROBLEM}（结果不完整，按失败处理）"
  fi
fi

log "结论：status=${STATUS} code=${CODE:-<无>} rc=${RC} ${REASON}"
AG_TOOL_VERSION="$(sed -n 's/.*"tool": "aisbench-sla \([^"]*\)".*/\1/p' "${SLA_OUT}/sla_summary.json" 2>/dev/null | head -n1)"
export AG_TOOL_VERSION

if ! AG_STATUS="${STATUS}" AG_CODE="${CODE}" AG_REASON="${REASON}" \
     AG_RETRYABLE="${RETRYABLE}" AG_RC="${RC}" AG_EXTRA_NOTE="" write_result; then
  log "result.json 写入失败（本 Runner 的产物即 result.json，三选一 D-74），按参数错误退出"
  printf '{"schema_version":"2.0","task_id":"%s","attempt_no":%s,"runner_seq":%s,"runner":{"name":"aisbench-sla","version":"%s","image_ref":"%s"},"status":"failed","started_at":"%s","finished_at":"%s","metrics":[],"error":{"code":"RUNNER_NONZERO_EXIT","message":"result.json 写入失败","detail":{},"retryable":false},"artifacts":[]}\n' \
    "${TASK_ID}" "$(num_or "${ATTEMPT_NO}" 1)" "$(num_or "${RUNNER_SEQ}" 1)" "${VERSION}" "${IMAGE_REF}" "${STARTED}" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${RESULT}" 2>/dev/null || true
  exit 2
fi

log "产物：${SLA_OUT}（sla_summary.json / sla_points.csv / run.log / aisbench_all.log / points/**）"
exit "${EXIT_CODE}"
