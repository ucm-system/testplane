#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
deploy-state.json 独立校验器 —— 逐条对齐 server/internal/deploystate/deploystate.go 的
Parse/validate（不依赖 Go 工具链，供冒烟测试在 bash 里调用）：

  - 必须是合法 JSON 对象（Go encoding/json 拒绝 NaN/Infinity，这里同样拒绝）；
  - phase 必填且 ∈ {queued, accepted, deploying, ready, blocked}；
  - release_name 非空时必须匹配 ^[a-z][a-z0-9-]{0,52}$；
  - waited_seconds / pods[].restarts / queue.pending|inqueue|running 非负；
  - exports 键匹配 ^[A-Za-z_][A-Za-z0-9_]*$ 且值不含换行（§5.12）；
  - 未知字段放行（前向兼容，execution §9.5）——本工具同样不拒绝未知字段。

用法：python validate_deploystate.py <deploy-state.json>
退出码：0 = deploystate.Parse 可通过；1 = 会被 Parse 拒绝（打印原因）。
"""
import json
import re
import sys

PHASES = {"queued", "accepted", "deploying", "ready", "blocked"}
REL_RE = re.compile(r"^[a-z][a-z0-9-]{0,52}$")
ENVKEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def fail(msg):
    print(f"INVALID: {msg}")
    sys.exit(1)


def main():
    if len(sys.argv) != 2:
        print("usage: validate_deploystate.py <deploy-state.json>")
        sys.exit(1)
    try:
        with open(sys.argv[1], "rb") as f:
            raw = f.read()
    except OSError as e:
        fail(f"read: {e}")
    if not raw.strip():
        fail("deploy-state.json is empty")
    try:
        doc = json.loads(raw.decode("utf-8"),
                         parse_constant=lambda c: (_ for _ in ()).throw(ValueError(c)))
    except (ValueError, UnicodeDecodeError) as e:
        fail(f"decode: {e}")
    if not isinstance(doc, dict):
        fail("not a JSON object")

    phase = doc.get("phase")
    if phase not in PHASES:
        fail(f"phase {phase!r} not in {sorted(PHASES)}")

    rel = doc.get("release_name", "")
    if rel and not REL_RE.match(rel):
        fail(f"release_name {rel!r} does not match ^[a-z][a-z0-9-]{{0,52}}$")

    waited = doc.get("waited_seconds", 0)
    if not isinstance(waited, int) or waited < 0:
        fail(f"waited_seconds {waited!r} is negative / not an integer")

    pods = doc.get("pods") or []
    if not isinstance(pods, list):
        fail("pods is not a list")
    for i, p in enumerate(pods):
        if not str(p.get("name", "")).strip():
            fail(f"pods[{i}].name is empty")
        if p.get("restarts", 0) < 0:
            fail(f"pods[{i}].restarts is negative")

    q = doc.get("queue")
    if q is not None:
        if not isinstance(q, dict):
            fail("queue is not an object")
        for k in ("pending", "inqueue", "running"):
            if q.get(k, 0) < 0:
                fail(f"queue.{k} is negative")

    exports = doc.get("exports") or {}
    if not isinstance(exports, dict):
        fail("exports is not an object")
    for k, v in exports.items():
        if not ENVKEY_RE.match(k):
            fail(f"exports key {k!r} is not a valid env name")
        if "\n" in v or "\r" in v:
            fail(f"exports[{k}] contains a newline")

    failure = doc.get("failure")
    if failure is not None:
        if not isinstance(failure, dict):
            fail("failure is not an object")
        if failure.get("code") not in ("DEPLOY_FAILED", "DEPLOY_BLOCKED"):
            fail(f"failure.code {failure.get('code')!r} not in {{DEPLOY_FAILED, DEPLOY_BLOCKED}}")

    print(f"OK: deploy-state.json passes deploystate.Parse-equivalent validation "
          f"(phase={phase}, release_name={rel or '-'}, waited={waited}s, "
          f"pods={len(pods)}, failure={failure.get('code') if failure else 'none'})")


if __name__ == "__main__":
    main()
