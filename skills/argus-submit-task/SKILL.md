---
name: argus-submit-task
description: 在 Argus 平台端到端提交并跟踪一个评测 Task——发现 Runner、组装 manifest（deploy XOR service）、dry_run、提交 202、等待终态、取报告与产物；含 dev 鉴权与 jsonl 输入的硬规则。
---

# 提交 Task（端到端）

## 权威契约先读

`docs/agents/argus-playbook.md` 是**唯一**操作手册（公开契约、curl/MCP/CLI 三通道齐备）。
本技能只补「仓库里动手」的快速通道与易错点，契约细节一律以 playbook 与
`server/api/openapi.yaml` 为准。

## 30 秒快速通道（本地 kind 环境）

```bash
API=http://127.0.0.1:8080
TOKEN=dev            # authMode=dev：Bearer <subject>，subject 即用户名，人人都是 admin
# 1. 发现 Runner（提交体里的 runner@version 必须命中这里）
curl -sS "$API/api/v1/runners" -H "Authorization: Bearer $TOKEN"
# 2. 预检（不落库）→ 3. 提交 → 202
curl -sS -X POST "$API/api/v1/tasks?dry_run=true" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/yaml" --data-binary @task.yaml
curl -sS -X POST "$API/api/v1/tasks" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/yaml" --data-binary @task.yaml
```

等待终态与取结果（SSE / 轮询 / report / bundle / 日志）照 playbook §4–§5，不重复。

## Manifest 硬规则（错了就是 400，逐条都实测过）

- **deploy XOR service 二选一**：都给或都不给都拒绝；`service.url` = 外接已有服务，不部署。
- **runners 可为 0（D-77）**：只提交服务来源 + precheck（auto）/ clean，用于验证服务能否启动；
  上限 200 不变。管道永远非空——deploy/precheck/clean 由平台注入。
- **不幂等（D-37）**：没有幂等键，超时重发就是两个 Task；防重复先查
  `GET /api/v1/tasks?submitter=<你>`。
- **jsonl 输入（D-67）**：每行一个完整 JSON 对象；**行尾换行合法**（服务端逐行 trim 跳空行）；
  跨行美化 JSON 不是 JSONL（Web 端校验器会给「格式化」一键救回）。单条 ≤64 KiB。
  字段类型/required 按 Runner 的 `parameters` 校验，带行号报 `detail.issues[].line`。
- **argv 恒空（D-66）**：manifest 没有 `args`，业务输入全走 `input` 原文。
- 终态只有 `succeeded / failed / service_unavailable / canceled`；`retryable=true` 的错误码才可自动重试，
  整 Task `retry` 是唯一平台内重试入口（fresh deploy，新 attempt）。

## Web UI 口径

提交页服务来源三选一：**Genet（默认）/ Mock / 已有服务（外接直接填地址）**；
三种输入框（yaml/jsonl/text）语法实时校验，jsonl 是 CodeMirror 编辑器。

## 常见提交期错误

| 现象 | 原因 |
|---|---|
| `VALIDATION_ERROR` issues[].line 指着 input | jsonl 语法/类型错，按行号修 |
| `RUNNER_NOT_FOUND` | version 没钉对，回 `GET /runners` 对 latest |
| `EXPORT_KEY_CONFLICT` | 两个 runner 导出同名 key，改名或砍一个 |
| `TASK_TOO_LARGE` | runners > 200，拆 Task |
