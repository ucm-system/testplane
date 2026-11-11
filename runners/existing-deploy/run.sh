#!/bin/bash
# existing-deploy：已有服务登记（内置 deploy 变体，0 秒完成）。
# 契约：读 service 三元组（JSONL 单行）→ 原样 exports → exit 0。无外部调用。
set -euo pipefail

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

OUT="${AG_OUTPUT_DIR:-/workspace/output}"
PARAMS_FILE="${AG_PARAMS_FILE:-/workspace/input/params.jsonl}"

command -v jq >/dev/null 2>&1 || { log "缺少 jq"; printf 'RUNNER_INVALID\n' > "$OUT/exports.env"; exit 2; }

# --- 读入 service 三元组（单行 JSON）--------------------------------------
line=""
if [ -s "$PARAMS_FILE" ]; then
  line="$(head -n 1 "$PARAMS_FILE")"
fi
[ -n "$line" ] || { log "缺少 service 三元组（url/model_name 必填）"; exit 2; }

url="$(printf '%s' "$line" | jq -r 'if has("url") and .url != null then .url else empty end')"
model_name="$(printf '%s' "$line" | jq -r 'if has("model_name") and .model_name != null then .model_name else empty end')"
model_path="$(printf '%s' "$line" | jq -r 'if has("model_path") and .model_path != null then .model_path else empty end')"

[ -n "$url" ] || { log "url 必填"; exit 2; }
[ -n "$model_name" ] || { log "model_name 必填"; exit 2; }

# --- 导出四件套（deploy_ref 指回自身：无可拆除对象，clean 阶段不注入）------
mkdir -p "$OUT"
cat > "$OUT/exports.env" <<EXPORTS
AG_SERVED_URL=$url
AG_SERVED_MODEL_NAME=$model_name
AG_MODEL_PATH=$model_path
AG_DEPLOY_REF=existing
EXPORTS

log "existing-deploy：service 已登记（url=$url model=$model_name），0 资源、无需拆除"
exit 0
