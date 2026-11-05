# 验收矩阵结论（票 #3 · QA 侧）

> 对 `design/IMPLEMENTATION.md` §9 / ARCHITECTURE §14.1 的 17 项，给出「做过 / mock / 未做」与证据。
> 本文件只提供结论，**不写** `docs/runbook/acceptance.md`（由 infra 收口）。
> 状态随测试推进更新；最后更新：2026-11-05。

| # | 验收项 | 结论 | 证据 / 备注 |
|---|---|---|---|
| 1 | Run 提交与完整状态链 | mock（提交/状态推进已验证；终态链待失败注入） | e2e `submit` 成功 202 + run_id；事件历史 events/history 可读；当前单 controller 在修 watch/secure 问题 |
| 2 | Task 独立提交 | 未做（MVP 排除） | Lead 裁决 `POST /tasks` 不在 MVP；e2e 只覆盖 Run |
| 3 | Genet 适配三项 | 未做 | 依赖 backend 的 Fake Genet 与 deploy 编排 |
| 4 | Pre-Check 注入与阻断 | mock（注入已见；阻断待失败注入） | `GET /testers` 有 endpoint-probe；提交后 seq=0 自动注入；失败注入脚本已就绪 |
| 5 | Registry 发现与校验 | 做过（发现）/ 未做（非法 tester.yaml 单测） | `GET /testers` 返回 endpoint-probe（K-3 无运行时注册）；`tools/contract` 校验 required 字段 |
| 6 | Tester 契约（入口/argv/输出/result.json） | mock | 真实 `testers/endpoint-probe/run.sh` 在容器里对 mock-llm 跑通 5 种判定；e2e 校验 attempt logs/result.json |
| 7 | Tester 独立运行 | 做过 | `scripts/e2e/probe-check.ps1`：5/5 场景 exit code 与 result.json 均符合设计 |
| 8 | K8s Executor（超时/取消） | 未做 | 待 Job watch/reconcile 修好后执行；取消语义脚本已备 |
| 9 | 状态机与重启恢复 | 未做 | 需要杀 controller/Job 对账场景 |
| 10 | Attempt 与重试 | 未做 | 需要 `retry_on` 命中与失败注入 |
| 11 | 失败策略（abort_run） | mock | e2e 已断言 `run.abort_propagation`（precheck 失败→其余 skipped）；待真实失败注入验证 |
| 12 | Run 编排全链路 | 未做 | 四终态用例待执行 |
| 13 | 报告结构 | mock | e2e 断言 report 非空 + JSON 合法；真实 report 生成待 Run 终态 |
| 14 | 产物持久化 | 未做 | 需要删除 Pod/Job 后仍可下载 result.json 与 raw/ |
| 15 | 失败隔离 | 未做 | 两 Task 一失败一成功的 Run 待构造（需要第二个可执行 Tester） |
| 16 | Web UI | 部分 | `pnpm --dir web build` 通过；`GET /` 返回 200 text/html；Playwright 冒烟未做 |
| 17 | API 契约与错误码 | 做过（工具与结果） | 规格侧 12/12；实现侧 32/32（对最新源码旁路实例，含 `/runs/{id}/tasks` seq=0 完整性）；合计 44/44。`tools/contract` 可复跑 |

## 复跑命令

```bash
cd tools/contract && go test ./... && go run . --mode spec
cd tools/e2e      && go test ./...
pwsh scripts/e2e/probe-check.ps1
pwsh scripts/e2e/run-local.ps1                  # 需要控制面
pwsh scripts/e2e/failure-injection.ps1          # 需要控制面 + kind executor
```

## 已知阻塞（2026-11-05 10:55 时点）

1. 控制面 8080 正在按 Lead 的节奏重建/重启；单 controller（宿主 `role=all`）。
2. 后端的 Job watch/reconcile 查询（`attempts` 的 `created_at`）与 `seq > 0` 分页问题在修。
3. 失败注入依赖 Tester Pod 能跑起来（PVC / secret env 已修/在修）。
