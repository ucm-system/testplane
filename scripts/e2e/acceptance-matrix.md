# 验收矩阵结论（票 #3 · QA 侧）

> 对 `design/IMPLEMENTATION.md` §9 / ARCHITECTURE §14.1 的 17 项，给出「做过 / mock / 未做」与证据。
> 本文件只提供结论，**不写** `docs/runbook/acceptance.md`（由 infra 收口）。
> 状态随测试推进更新；最后更新：2026-11-05。

| # | 验收项 | 结论 | 证据 / 备注 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | 做过 | e2e `r-20261105-5NH8`：`queued→testing→succeeded` 7.0s（集群实例 8091，真 K8s Job）；事件 seq/转移/终态全绿 |
| 2 | Task 独立提交 | 未做（MVP 排除） | Lead 裁决 `POST /tasks` 不在 MVP |
| 3 | Genet 适配三项 | 未做 | 依赖 backend 的 Fake Genet 与 deploy 编排 |
| 4 | Pre-Check 注入与阻断 | 部分 | 500/slow-ready → `service_unavailable`（retryable=true）✅；401/200包错误仍被误判 `service_unavailable`（应 `completed_with_errors`），根因 `outcome.go:56-65` / `status.go:44-55` 把 PRECHECK_FAILED 当可重试；已转 backend |
| 5 | Registry 发现与校验 | 做过（发现）/ 未做（非法 tester.yaml 单测） | `GET /testers` 返回 endpoint-probe；`tools/contract` 校验 required 字段 |
| 6 | Tester 契约（入口/argv/输出/result.json） | 做过 | 真实 `testers/endpoint-probe/run.sh` 在容器里对 mock-llm 跑通 5 种判定；`result.json` 11 字段通过校验 |
| 7 | Tester 独立运行 | 做过 | `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 result.json 均符合设计 |
| 8 | K8s Executor（超时/取消） | 部分 | 真 Job 创建/完成/产物/logs 已通（`r-5NH8` 7s）；超时/取消与重启恢复待补 |
| 9 | 状态机与重启恢复 | 部分 | 已见启动对账收口僵尸租约（Lead 实测 8QGE）；脚本化杀 Job 对账待补 |
| 10 | Attempt 与重试 | 未做 | 需要 `retry_on` 命中与失败注入 |
| 11 | 失败策略（abort_run） | 做过 | 失败注入中 precheck 失败后其余 Task 全部 `skipped(upstream_abort)` ✅ |
| 12 | Run 编排全链路 | 部分 | success 全链路 ✅；failed 归类 3/5 通过，401/200包错误待修 |
| 13 | 报告结构 | 做过 | `r-5NH8` report 663B / report.md 780B，终态后 ≤2s 出现 ✅ |
| 14 | 产物持久化 | 部分 | `result.json`、`raw/`、`logs/` 均落 PVC；`/logs` 返回 200 + X-Log-Offset；删 Pod 后下载待测 |
| 15 | 失败隔离 | 未做 | 两 Task 一失败一成功的 Run 待构造（需要第二个可执行 Tester） |
| 16 | Web UI | 部分 | `pnpm --dir web build` 通过；`GET /` 200 text/html；日志页接口已通；Playwright 冒烟未做 |
| 17 | API 契约与错误码 | 做过 | 规格侧 12/12；实现侧 32/32（集群实例 8091）；合计 44/44；warning 按扁平 Error（D-39） |

## 复跑命令

```bash
cd tools/contract && go test ./... && go run . --mode spec
cd tools/e2e      && go test ./...
pwsh scripts/e2e/probe-check.ps1
pwsh scripts/e2e/run-local.ps1                  # 需要控制面
pwsh scripts/e2e/failure-injection.ps1          # 需要控制面 + kind executor
```

## 已知阻塞 / 待办（2026-11-05 11:13 时点）

1. **等稳定窗口**：backend 的集成测试曾把 dev 库 `testplane` drop 重灌（11:04/11:05 两次，`relation "runs" does not exist`）；已定位为 `TESTPLANE_TEST_DATABASE_URL` 指向 dev 库，要求改用 `testplane_test`。
2. **backend-exec 修复中**：非零退出不读 `result.json`（违反 C-7.1 / 07 §3）。修好前 500/慢就绪会误判 `completed_with_errors`（应为 `service_unavailable`）。
3. **backend-core 修复中**：report hook（SLA ≤2s，当前 PPHF 终态后 30s 仍 404）；`/logs` 改为读 PVC `logs/`（当前恒 404）。
4. 修复后要**恢复严格断言**：report 必现、logs 必非空，不带 `-AllowKnownGaps`。
5. 窗口开始后复跑：`contract --mode all`、`e2e run`（success）、`failure-injection.ps1`（5 场景），结果写回 ticket #3。
