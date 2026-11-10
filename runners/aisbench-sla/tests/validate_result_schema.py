#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""用平台契约（`server/api/openapi.yaml` 的 `components.schemas.Result`）校验 result.json。

为什么需要：`result.json` 的 `error.code` 必须是平台码表里有的（`ErrorCode` enum /
`server/internal/domain/codes.go`），否则平台不会报错，而是**静默降级成
`RUNNER_NONZERO_EXIT`**、丢掉区分信息（评审 #62 发现的正是这类问题）。
`metrics[]` 的 `name/unit/statistic/labels` 也走 `ResultMetric` 的约束。

跑法：
  py -3.11 runners/aisbench-sla/tests/validate_result_schema.py <result.json...>
  # 不给参数时校验冒烟/红证留下的已知产物（不存在就跳过）

依赖：`PyYAML` 与 `jsonschema`（只用于本测试，不进 Runner 镜像）。缺依赖时**跳过**并返回 0，
避免在没有这两个库的机器上把门禁搞红。
退出码：0 全部通过/跳过；1 有不通过；2 原始用法错误。
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

TESTS_DIR = Path(__file__).resolve().parent
RUNNER_DIR = TESTS_DIR.parent
REPO = RUNNER_DIR.parents[1]
SPEC = REPO / "server" / "api" / "openapi.yaml"

DEFAULT_TARGETS = [
    REPO / ".data" / "sla-runner" / "smoke" / "result.json",
    REPO / ".data" / "sla-runner" / "smoke" / "cases" / "degraded" / "result.json",
    REPO / ".data" / "sla-runner" / "smoke" / "cases" / "exit2" / "result.json",
    REPO / ".data" / "sla-runner" / "smoke" / "cases" / "exit3" / "result.json",
]


def _load_deps():
    try:
        import jsonschema  # noqa: F401
        import yaml  # noqa: F401
    except ImportError as exc:  # pragma: no cover - 环境相关
        print(f"SKIP 缺依赖（{exc}）：py -3.11 -m pip install pyyaml jsonschema")
        return None
    return jsonschema, yaml


def _resolve(node, schemas):
    if isinstance(node, dict):
        if "$ref" in node:
            name = node["$ref"].rsplit("/", 1)[-1]
            merged = dict(schemas[name])
            for key, value in node.items():
                if key != "$ref":
                    merged[key] = value
            return _resolve(merged, schemas)
        return {key: _resolve(value, schemas) for key, value in node.items()}
    if isinstance(node, list):
        return [_resolve(value, schemas) for value in node]
    return node


def main(argv: list) -> int:
    deps = _load_deps()
    if deps is None:
        return 0
    jsonschema, yaml = deps
    if not SPEC.is_file():
        print(f"SKIP 找不到契约文件：{SPEC}（本测试只在完整仓库里跑）")
        return 0

    targets = [Path(item) for item in argv] or DEFAULT_TARGETS
    targets = [path for path in targets if path.is_file()]
    if not targets:
        print("SKIP 没有可校验的 result.json（先跑 tests/smoke.ps1 或红证脚本）")
        return 0

    schemas = yaml.safe_load(SPEC.read_text(encoding="utf-8"))["components"]["schemas"]
    schema = _resolve(schemas["Result"], schemas)
    failed = 0
    for path in targets:
        data = json.loads(path.read_text(encoding="utf-8"))
        try:
            jsonschema.validate(data, schema)
        except jsonschema.ValidationError as exc:
            failed += 1
            print(f"FAIL {path}: {exc.message} @ {list(exc.absolute_path)}")
        else:
            print(f"PASS {path}: openapi Result schema 校验通过"
                  f"（{len(data.get('metrics') or [])} metrics / "
                  f"{len(data.get('artifacts') or [])} artifacts）")
    print(f"\nRESULT SCHEMA: {'PASS' if not failed else 'FAIL'} ({len(targets) - failed}/{len(targets)})")
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
