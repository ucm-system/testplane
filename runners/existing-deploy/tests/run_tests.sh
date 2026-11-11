#!/bin/bash
# 冒烟：三元组 → exports 四件套；缺 url → exit 2
set -u
cd "$(dirname "$0")/.."
OUT=$(mktemp -d)
mkdir -p "$OUT/input/1" "$OUT/runners/1"
printf '{"url":"http://10.0.0.10:8000/v1","model_name":"Qwen3-32B"}\n' > "$OUT/input/1/params.jsonl"
AG_OUTPUT_DIR="$OUT/runners/1" AG_PARAMS_FILE="$OUT/input/1/params.jsonl" bash run.sh && \
grep -q "AG_SERVED_URL=http://10.0.0.10:8000/v1" "$OUT/runners/1/exports.env" && \
grep -q "AG_DEPLOY_REF=existing" "$OUT/runners/1/exports.env" && echo "T1 PASS: 正常登记" || echo "T1 FAIL"
printf '{"model_name":"x"}\n' > "$OUT/input/1/params.jsonl"
AG_OUTPUT_DIR="$OUT/runners/1" AG_PARAMS_FILE="$OUT/input/1/params.jsonl" bash run.sh 2>/dev/null && echo "T2 FAIL: 缺 url 应 exit 2" || [ $? -eq 2 ] && echo "T2 PASS: 缺 url exit 2"
rm -rf "$OUT"
