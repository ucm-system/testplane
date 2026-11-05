# 验收矩阵结论（票 #3 · QA 侧）

> 对 `design/IMPLEMENTATION.md` §9 / ARCHITECTURE §14.1 的 17 项，给出「做过 / mock / 未做」与证据。
> 本文件只提供结论，**不写** `docs/runbook/acceptance.md`（由 infra 收口）。
> 状态随测试推进更新；最后更新：2026-11-05。

| # | 验收项 | 结论 | 证据 / 备注 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | 做过 | e2e `r-20261105-EPRN`：`queued→testing→succeeded` 7.0s（集群实例 8092，真 K8s Job）；31/31 断言全绿 |
| 2 | Task 独立提交 | 未做（MVP 排除） | Lead 裁决 `POST /tasks` 不在 MVP |
| 3 | Genet 适配三项 | 未做 | 依赖 backend 的 Fake Genet 与 deploy 编排 |
| 4 | Pre-Check 注入与阻断 | 做过 | 失败注入 5/5：500/slow → `service_unavailable`(retryable=true)；401/200包错误 → `completed_with_errors`(retryable=false)；其余 Task 全 `skipped(upstream_abort)` |
| 5 | Registry 发现与校验 | 做过（发现）/ 未做（非法 tester.yaml 单测） | `GET /testers` 返回 endpoint-probe；`tools/contract` 校验 required 字段 |
| 6 | Tester 契约（入口/argv/输出/result.json） | 做过 | 真实 `testers/endpoint-probe/run.sh` 在容器里对 mock-llm 跑通 5 种判定；`result.json` 11 字段通过校验 |
| 7 | Tester 独立运行 | 做过 | `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 result.json 均符合设计 |
| 8 | K8s Executor（超时/取消） | 部分 | 真 Job 创建/完成/产物/logs 已通（`r-EPRN` 7s）；超时/取消与重启恢复待补 |
| 9 | 状态机与重启恢复 | 部分 | 已见启动对账收口僵尸租约（Lead 实测 8QGE）；脚本化杀 Job 对账待补 |
| 10 | Attempt 与重试 | 未做 | 需要 `retry_on` 命中与失败注入 |
| 11 | 失败策略（abort_run） | 做过 | 失败注入中 precheck 失败后其余 Task 全部 `skipped(upstream_abort)` ✅ |
| 12 | Run 编排全链路 | 做过 | success + 4 类失败全链路：失败注入 5/5，终态与 C-5.8 一致 |
| 13 | 报告结构 | 做过 | `r-EPRN` report 662B / report.md 780B，终态后 ≤2s 出现 ✅ |
| 14 | 产物持久化 | 部分 | `result.json`、`raw/`、`logs/` 均落 PVC；`/logs` 返回 200 + X-Log-Offset；删 Pod 后下载待测 |
| 15 | 失败隔离 | 未做 | 两 Task 一失败一成功的 Run 待构造（需要第二个可执行 Tester） |
| 16 | Web UI | 部分 | `pnpm --dir web build` 通过；`GET /` 200 text/html；日志页接口已通；Playwright 冒烟未做 |
| 17 | API 契约与错误码 | 做过 | 规格侧 12/12；实现侧 32/32（集群实例 8092）；合计 44/44；warning 按扁平 Error（D-39） |

## 复跑命令

```bash
cd tools/contract && go test ./... && go run . --mode spec
cd tools/e2e      && go test ./...
pwsh scripts/e2e/probe-check.ps1
pwsh scripts/e2e/run-local.ps1                  # 需要控制面
pwsh scripts/e2e/failure-injection.ps1          # 需要控制面 + kind executor
```

## 最终结果（2026-11-05 11:31，镜像 `b61a1a23…` / 8092，严格模式）

- 契约 `--mode all`：**44/44 PASS**（spec 12 + live 32）。
- e2e 生命周期：`r-20261105-EPRN` **31/31 PASS**，succeeded 7.0s；report 662B、report.md 780B、logs 363/362B、事件 18 条。
- 失败注入 **5/5 全绿**：
  | 场景 | Run | 实际终态 | 关键断言 |
  |---|---|---|---|
  | success | `r-9DBT` | succeeded | 7.1s |
  | fail-500 | `r-R1VP` | service_unavailable | PRECHECK_FAILED retryable=true |
  | fail-401 | `r-YAJG` | completed_with_errors | PRECHECK_FAILED retryable=false |
  | error-in-200 | `r-8G9W` | completed_with_errors | PRECHECK_FAILED retryable=false |
  | slow-ready | `r-Z47Q` | service_unavailable | PRECHECK_FAILED retryable=true |

严格断言，无 report/logs 豁免。
