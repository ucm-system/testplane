#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""result.json 映射回归（评审 #62 硬问题 2 / 5 的红证）。

这个用例**不重写** run.sh 里那段 Python：它从 run.sh 里把 `<<'PYRESULT' ... PYRESULT`
heredoc 原样抽出来，喂不同的 `sla_summary.json` fixture 跑一遍，断言 result.json 的：

* `sla_max_concurrency` 只取**实测并发 >= 目标 80%** 的 PASS 点（压不满的点 degraded、
  不参与取值）；全部点都降级时**不输出**该指标，并在 notes 说明结论不可用；
* 指标名不再编码统计量/单位（`ttft` / `tpot` / `hit_rate` / `output_throughput`），
  `unit` / `statistic` / `labels` 符合附录 A 与 openapi `ResultMetric`；
* `workload.concurrency` 记下本次结论落在哪个并发。

跑法：
  py -3.11 testers/aisbench-sla/tests/test_result_mapping.py
退出码：0 全绿；1 有断言失败；2 找不到 run.sh 的写结果块（结构性错误）。
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

TESTS_DIR = Path(__file__).resolve().parent
TESTER_DIR = TESTS_DIR.parent
RUN_SH = TESTER_DIR / "run.sh"

_results: list[tuple[bool, str, str]] = []


def check(ok: bool, title: str, detail: str = "") -> None:
    _results.append((bool(ok), title, detail))
    print(f"{'PASS' if ok else 'FAIL'}  {title}" + (f"  [{detail}]" if detail else ""))


def extract_result_writer() -> str:
    """从 run.sh 里抽出写 result.json 的 Python 块（唯一实现，测试不复制逻辑）。"""
    text = RUN_SH.read_text(encoding="utf-8")
    marker = "python3 - <<'PYRESULT'"
    start = text.find(marker)
    if start < 0:
        raise SystemExit(f"run.sh 里找不到 {marker}")
    start = text.find("\n", start) + 1
    end = text.find("\nPYRESULT\n", start)
    if end < 0:
        end = text.find("\nPYRESULT", start)
    if end < 0:
        raise SystemExit("run.sh 里找不到 PYRESULT 结束标记")
    return text[start:end]


def point(concurrency: int, measured: float, *, kind: str = "probe",
          verdict: str = "PASS", degraded: bool = False, ttft: float = 10.0,
          tpot: float = 2.0, hit: float = 0.75) -> dict:
    return {
        "kind": kind,
        "concurrency": concurrency,
        "verdict": verdict,
        "reason": "SLA 满足" if verdict == "PASS" else "ttft 超阈值",
        "degraded": degraded,
        "degraded_reason": ("实测并发远低于目标" if degraded else ""),
        "measured_concurrency": measured,
        "measured_concurrency_ratio": round(measured / concurrency, 3),
        "ttft_avg_ms": ttft,
        "tpot_p90_ms": tpot,
        "hbm_hit_rate": hit,
        "hit_rate_note": "",
        "max_running": measured,
        "output_token_throughput": 100.0 * concurrency,
    }


def summary(points: list, max_ok, peak_concurrency: int, peak_value: float = 400.0) -> dict:
    return {
        "tool": "aisbench-sla 0.7.0",
        "status": "ok" if max_ok else "none_passed",
        "max_ok_concurrency": max_ok,
        "note": "SLA 内最大并发 = %s" % max_ok,
        "error": "",
        "sla": {"ttft_avg": 1200},
        "sla_labels": ["ttft AVG <= 1200 ms"],
        "warnings": [],
        "throughput": {"peak": {"value": peak_value, "concurrency": peak_concurrency,
                                "ok": True}},
        "points": points,
    }


def run_writer(writer_path: Path, fixture: dict) -> dict:
    out = Path(tempfile.mkdtemp(prefix="tp_result_"))
    (out / "sla_out").mkdir(parents=True, exist_ok=True)
    (out / "sla_out" / "sla_summary.json").write_text(
        json.dumps(fixture, ensure_ascii=False, indent=2), encoding="utf-8")
    env = {
        **os.environ,
        "TP_OUT": str(out),
        "TP_VERSION": "0.4.0",
        "TP_RUN_ID": "r-test",
        "TP_TASK_ID": "t-test",
        "TP_ATTEMPT_ID": "a-test",
        "TP_IMAGE": "unknown",
        "TP_STARTED": "2026-11-06T00:00:00Z",
        "TP_START_EPOCH": "1",
        "TP_URL": "http://127.0.0.1:1/v1",
        "TP_MODEL_NAME": "mock",
        "TP_MODEL_PATH": "/tmp",
        "TP_INPUT_LEN": "128",
        "TP_OUTPUT_LEN": "8",
        "TP_DATA_NUM": "32",
        "TP_START": "1",
        "TP_MAX": "8",
        "TP_MAX_PROBES": "4",
        "TP_REQUEST_RATE": "0",
        "TP_DATASET_TYPE": "normal",
        "TP_DATASET": "/tmp/d.jsonl",
        "TP_DATASET_PATH": "/tmp/datasets",
        "TP_WORK_PATH": "/tmp/aisbench-ws",
        "TP_AISBENCH_CMD": "ais_bench",
        "TP_SLA_LIST": "ttft.avg:1200\n",
        "TP_STATUS": "succeeded",
        "TP_CODE": "",
        "TP_REASON": "工具退出码 0：已得到 SLA 搜索结论",
        "TP_EXTRA_NOTE": "",
        "TP_RETRYABLE": "false",
        "TP_RC": "0",
        "TP_TOOL_VERSION": "0.7.0",
        "TP_MEASURED_RATIO_MIN": "0.8",
        "PYTHONIOENCODING": "utf-8",
        "PYTHONUTF8": "1",
    }
    proc = subprocess.run([sys.executable, str(writer_path)], env=env,
                          capture_output=True, text=True, encoding="utf-8",
                          errors="replace")
    if proc.returncode != 0:
        raise AssertionError(f"写 result.json 的块退出码 {proc.returncode}：{proc.stderr[-2000:]}")
    return json.loads((out / "result.json").read_text(encoding="utf-8"))


def by_name(result: dict) -> dict:
    return {m.get("name"): m for m in result.get("metrics") or []}


def extract_summary_checker() -> str:
    """抽出 run.sh 里校验 sla_summary.json 内容的那段 python（同样不复制逻辑）。"""
    text = RUN_SH.read_text(encoding="utf-8")
    marker = "python3 - \"${SLA_OUT}/sla_summary.json\" <<'PYCHECK'"
    start = text.find(marker)
    if start < 0:
        raise SystemExit("run.sh 里找不到摘要校验块（PYCHECK）")
    start = text.find("\n", start) + 1
    end = text.find("\nPYCHECK\n", start)
    if end < 0:
        end = text.find("\nPYCHECK", start)
    if end < 0:
        raise SystemExit("run.sh 里找不到 PYCHECK 结束标记")
    return text[start:end]


def run_summary_check(checker_path: Path, summary_path: Path) -> str:
    # 子进程的 stdout 是管道：Windows 上默认按 GBK 编码，这里强制 UTF-8，否则
    # 中文消息解不出来（会变成空串，断言全假失败）。
    proc = subprocess.run([sys.executable, str(checker_path), str(summary_path)],
                          capture_output=True, text=True, encoding="utf-8",
                          errors="replace",
                          env={**os.environ, "PYTHONIOENCODING": "utf-8",
                               "PYTHONUTF8": "1"})
    if proc.returncode != 0:
        raise AssertionError(f"摘要校验块自身崩溃：{proc.returncode} {proc.stderr[-800:]}")
    return (proc.stdout or "").strip()


def check_summary_validation(checker_path: Path) -> None:
    """nit：「摘要损坏/为空也要 failed」，不能只判文件存在。"""
    work = Path(tempfile.mkdtemp(prefix="tp_summary_"))
    good = work / "good.json"
    good.write_text(json.dumps(summary([point(1, 1.0)], max_ok=1, peak_concurrency=1)),
                    encoding="utf-8")
    check(run_summary_check(checker_path, good) == "",
          "摘要校验：正常摘要通过")

    missing = work / "missing.json"
    check("没有产出" in run_summary_check(checker_path, missing),
          "摘要校验：文件缺失 → 报错")

    broken = work / "broken.json"
    broken.write_text("{not json", encoding="utf-8")
    check("无法解析" in run_summary_check(checker_path, broken),
          "摘要校验：JSON 损坏 → 报错")

    empty = work / "empty.json"
    empty.write_text("{}", encoding="utf-8")
    check("points 为空" in run_summary_check(checker_path, empty),
          "摘要校验：空对象/没有 points → 报错")

    no_points = work / "no_points.json"
    payload = summary([point(1, 1.0)], max_ok=1, peak_concurrency=1)
    payload["points"] = []
    no_points.write_text(json.dumps(payload), encoding="utf-8")
    check("points 为空" in run_summary_check(checker_path, no_points),
          "摘要校验：points 是空数组 → 报错")

    no_key = work / "no_key.json"
    payload = summary([point(1, 1.0)], max_ok=1, peak_concurrency=1)
    payload.pop("max_ok_concurrency")
    no_key.write_text(json.dumps(payload), encoding="utf-8")
    check("缺少关键字段" in run_summary_check(checker_path, no_key),
          "摘要校验：缺少关键字段 → 报错")

    dry = work / "dry.json"
    payload = summary([point(1, 1.0)], max_ok=1, peak_concurrency=1)
    payload["status"] = "dry_run"
    dry.write_text(json.dumps(payload), encoding="utf-8")
    check("dry-run" in run_summary_check(checker_path, dry),
          "摘要校验：dry-run 不算成功")


def main() -> int:
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except Exception:  # noqa: BLE001
        pass

    writer = Path(tempfile.mkdtemp(prefix="tp_writer_")) / "write_result.py"
    writer.write_text(extract_result_writer(), encoding="utf-8")
    checker = Path(tempfile.mkdtemp(prefix="tp_check_")) / "check_summary.py"
    checker.write_text(extract_summary_checker(), encoding="utf-8")

    check_summary_validation(checker)

    # ---------------------------------------------------------------- 正常 fixture
    normal = summary([point(1, 1.0), point(2, 2.0), point(4, 4.0, ttft=3.5, tpot=2.3)],
                     max_ok=4, peak_concurrency=4, peak_value=1253.612)
    result = run_writer(writer, normal)
    metrics = by_name(result)
    check(metrics.get("sla_max_concurrency", {}).get("value") == 4,
          "正常 fixture：sla_max_concurrency 取实测并发达标的 c=4",
          repr(metrics.get("sla_max_concurrency")))
    expected = {
        "sla_max_concurrency": ("count", "count"),
        "output_throughput": ("tokens_per_s", "max"),
        "ttft": ("ms", "mean"),
        "tpot": ("ms", "p90"),
        "hit_rate": ("percent", "mean"),
        "probes_total": ("count", "count"),
    }
    for name, (unit, statistic) in expected.items():
        metric = metrics.get(name) or {}
        check(metric.get("unit") == unit and metric.get("statistic") == statistic,
              f"正常 fixture：{name} 的 unit/statistic = {unit}/{statistic}",
              f"{metric.get('unit')}/{metric.get('statistic')}")
    for legacy in ("ttft_avg_ms", "tpot_p90_ms", "hit_rate_percent",
                   "max_output_throughput_tok_s"):
        check(legacy not in metrics, f"正常 fixture：不再输出旧指标名 {legacy}")
    check((metrics.get("sla_max_concurrency") or {}).get("labels", {}).get("scope") == "search",
          "正常 fixture：sla_max_concurrency 带 labels.scope=search")
    check((metrics.get("probes_total") or {}).get("labels", {}).get("scope") == "search",
          "正常 fixture：probes_total 带 labels.scope=search")
    check((metrics.get("ttft") or {}).get("labels", {}).get("concurrency") == 4,
          "正常 fixture：ttft 取自 c=4 且带 labels.concurrency=4",
          repr((metrics.get("ttft") or {}).get("labels")))
    check((metrics.get("ttft") or {}).get("value") == 3.5,
          "正常 fixture：ttft 的值来自选中点（3.5）")
    check((metrics.get("output_throughput") or {}).get("value") == 1253.612,
          "正常 fixture：output_throughput = 吞吐峰值")
    check((result.get("workload") or {}).get("concurrency") == 4,
          "正常 fixture：workload.concurrency = 4",
          repr((result.get("workload") or {}).get("concurrency")))
    check("并发点降级" not in (result.get("notes") or ""),
          "正常 fixture：notes 里没有降级告警")

    # ---------------------------------------------------------------- 降级 fixture
    degraded_fixture = summary(
        [point(1, 1.0), point(2, 1.0, degraded=True, ttft=40.0)],
        max_ok=2, peak_concurrency=2, peak_value=56.66)
    result = run_writer(writer, degraded_fixture)
    metrics = by_name(result)
    check(metrics.get("sla_max_concurrency", {}).get("value") == 1,
          "降级 fixture：sla_max_concurrency 不取自降级点（=1，而不是 2）",
          repr(metrics.get("sla_max_concurrency")))
    check((metrics.get("ttft") or {}).get("labels", {}).get("concurrency") == 1,
          "降级 fixture：ttft 也不取自降级点（labels.concurrency=1）",
          repr((metrics.get("ttft") or {}).get("labels")))
    notes = result.get("notes") or ""
    check("并发点降级" in notes and "c=2" in notes,
          "降级 fixture：notes 写明哪个点降级", notes[:120])
    check("output_throughput 取自降级点" in notes,
          "降级 fixture：notes 提示吞吐峰值来自降级点", notes[:160])

    # ---------------------------------------------- 只有配置、实测不达标（无 degraded 字段）
    unverified = summary([point(1, 1.0), point(2, 1.0), point(4, 1.0)],
                         max_ok=4, peak_concurrency=4, peak_value=300.0)
    for row in unverified["points"]:
        row.pop("degraded", None)
        row.pop("degraded_reason", None)
    result = run_writer(writer, unverified)
    metrics = by_name(result)
    check(metrics.get("sla_max_concurrency", {}).get("value") == 1,
          "无 degraded 标记也按 0.8 阈值复核（实测 1.0/目标 4 → 不取 4）",
          repr(metrics.get("sla_max_concurrency")))

    # ---------------------------------------------------------------- 全降级 fixture
    all_degraded = summary(
        [point(2, 1.0, degraded=True), point(4, 1.0, degraded=True)],
        max_ok=4, peak_concurrency=4, peak_value=200.0)
    result = run_writer(writer, all_degraded)
    metrics = by_name(result)
    check("sla_max_concurrency" not in metrics,
          "全降级 fixture：不输出 sla_max_concurrency（不编一个数）",
          repr(sorted(metrics)))
    notes = result.get("notes") or ""
    check("结论不可用" in notes, "全降级 fixture：notes 说明「结论不可用」", notes[:200])
    check("ttft" not in metrics and "tpot" not in metrics,
          "全降级 fixture：没有可用的选中点，就不写 ttft/tpot",
          repr(sorted(metrics)))

    failed = [r for r in _results if not r[0]]
    print("\n" + "=" * 70)
    print(f"RESULT MAPPING: {'PASS' if not failed else 'FAIL'} "
          f"({len(_results) - len(failed)}/{len(_results)} OK)")
    print("=" * 70)
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
