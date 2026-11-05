# 观测与排障（`/metrics` 指标清单 + Run 打包下载）

面向值班与排障：**怎么判断"是平台问题还是环境问题"**，以及**怎么把一次失败的 Run 打包带走**（人看 / 丢给 AI 分析）。

## 1. `/metrics`（Prometheus 文本 exposition）

不需要鉴权（与 `/healthz`、`/readyz` 同级），端口同控制面 8080。

### 请求与作业

| 指标 | 类型 | 用途 |
|---|---|---|
| `testplane_http_requests_total{method,path,status}` | counter | 请求量与错误率（`path` 是路由模板，不是原始 URL） |
| `testplane_http_request_duration_seconds{method,path}` | histogram | 延迟分位 |
| `testplane_jobs_created_total` / `..._deleted_total` | counter | 执行层 Job 创建/回收（GC 是否在跑） |
| `testplane_reconcile_runs_total` / `..._errors_total` | counter | 对账循环健康度 |

### 审计拒绝（接入层 401/403/CSRF）

拒绝类审计是**异步有界**的（容量 1024，满了丢弃不阻塞请求；401 按 `(来源 IP, 1min)` 聚合）。

| 指标 | 类型 | 含义 / 看什么 |
|---|---|---|
| `testplane_audit_rejected_written_total` | counter | 成功落库的拒绝审计条数（聚合后的条数，不是请求数） |
| `testplane_audit_rejected_dropped_total` | counter | 队列满被丢弃的条数。**持续 > 0 = 有人在刷接口或 DB 太慢**，两边都要查 |
| `testplane_audit_rejected_failed_total` | counter | 落库失败（DB 错误）；配合日志里的 `dropped=true` 采样定位 |
| `testplane_audit_rejected_suppressed_total` | counter | 被 401 聚合窗口压掉的条数（`suppressed_count` 的累计） |

### 典型判读

| 现象 | 先看哪 | 结论方向 |
|---|---|---|
| `/readyz` 503 | `testplane_http_requests_total{path="/readyz"}` + Pod 日志 `readyz` 原因 | DB 不可达 / 迁移版本不匹配（不是 K8s 问题：liveness 不含 DB，不会重启） |
| Run 卡在 `queued` | `GET /queue-status` 的 `volcano` 块 + `waiting_reason` | 队列满/配额不足 = 等资源；队列空还在排 = 卡平台槽位（`TESTPLANE_MAX_GLOBAL_CONCURRENCY`） |
| Job 堆积不回收 | `testplane_jobs_deleted_total` 长期不涨 + `TESTPLANE_JOB_GC_ENABLED` | GC 被关/无权限；注意 `GCTerminalAge=1h`（1 小时内不删，给读日志留窗口） |
| 登录失败但 IdP 正常 | Pod 日志 `oidc` / `callback`；`oidc.redirectURL` 是否与 IdP 登记逐字节一致 | 九成是 redirect_url 或 `session.secure` 配错（HTTPS 才能 `secure=true`） |
| 审计表疯狂增长 | `testplane_audit_rejected_suppressed_total` / `dropped_total` | 有人在刷 401；考虑在 Ingress 层限流 |

## 2. 把一次 Run 打包带走

```bash
# 细粒度：看清单再挑
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/runs/$RUN/artifacts" | jq
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/runs/$RUN/files?path=<path>" -o out.bin

# 一次性：打包（zip）
curl -sS -H "Authorization: Bearer $TOKEN" \
     "$BASE/api/v1/runs/$RUN/bundle" -o "run-$RUN.zip"
unzip -l "run-$RUN.zip"
```

包内固定布局（D-56）：

```
MANIFEST.json          ← 永远第一个条目；看 counts / files[] / missing[]（缺什么、为什么缺）
report.json  report.md  metadata.json
events.jsonl           ← 全量状态事件（按 seq），敏感键已打码
artifacts/index.json + artifacts/<path>
logs/tasks/<task>/attempts/<n>.log
logs/deploy/<pod>.log  ← 实时窗口（Pod 被 GC 回收后就没了，MANIFEST 会写进 missing）
```

**排障顺序建议**：先看 `MANIFEST.json` 的 `state` 与 `missing` → 再看 `report.json` 的 `counts/failures` → 最后才翻日志（日志量最大）。
给 AI 分析时，直接把这一个 zip 丢进去即可——它自带"缺了什么"，不需要额外上下文。

## 3. 对象存储（P1，可选）

`D-56` 的决定：**P0 的唯一真源是 PVC 文件系统**，S3 兼容存储（MinIO / rustFS 等）只作为 **P1 的可选镜像**，不做 P0 依赖、不改任何端点契约。要接的时候只增加"把 PVC 内容镜像出去"的旁路作业，`/bundle`、`/files`、`/artifacts` 的行为不变。
