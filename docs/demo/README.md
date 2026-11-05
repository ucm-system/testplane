# TestPlane · 演示截图

真实后端 + 真实 kind 集群数据截的图（不是设计稿）。**这些图会被提交进仓库**，用来在 PR 里直接看效果（评审 [PR #33](https://github.com/ucm-system/testplane/pull/33) 第 15 条）。

| 文件 | 界面 | 这一屏要证明什么 |
|---|---|---|
| `01-home.png` | 概览 | 圣杯三列（左 240 / 主内容自适应 / 右 288）；KPI 关键数字 30px `tabular-nums`；页头没有"新建 Run"主按钮 |
| `02-runs.png` | 运行列表 | 列表工具条里的"筛选本页 Run"（不是全局搜索）；右侧栏给终态语义，不是空白 |
| `03-run-succeeded.png` | Run 详情（成功） | 执行链路横向 + Task/Attempt 表 + 容器日志 + 右侧 SSE 事件流 |
| `04-retry-dialog.png` | 重试预览 | 复用/重跑分段、失败项带 `failure.code`；确认后建**新 Run**（`retry_of`），源 Run 不变 |
| `05-report.png` | 报告页 | `report.json` 的 counts / Task 明细 / 失败清单 |
| `06-clusters.png` | 集群与队列 | 平台槽位 vs Volcano：队列 `reason` 是 PodGroup 原文，`available=false` 有降级卡片 |
| `07-admin.png` | 管理台 | 四个 Tab（Profiles / Clusters / Secrets / Audit）；Secrets 只登记引用、不显示值 |
| `08-run-deploy.png` | Deploy 形态的 Run | 两根轴（helm `status` + `endpoint.status`）；"ready 只是地址可解析，可用性由 Pre-Check 判" |
| `09-submit.png` | 提交 | Deploy 卡片显式存在；不部署时同框填 `url` / `model_name` / `model_path`；环境变量区 |
| `10-deploy-logs.png` | Deploy 日志 | 按 Pod 分页签、`next_offset` 续读、`gaps` 显式「此处不连续」（mock 数据，展示形态） |
| `11-deploy-logs-multi-pod.png` | 多 Pod（PD 分离） | 4 个 Pod 页签 + gap 标记 |
| `12-three-columns-1280.png` / `13-two-columns-900.png` / `14-stacked-480.png` | 响应式 | >1024 三列 / 768–1024 收右栏 / <768 堆叠 |

## 怎么重新生成

```powershell
# 1) 布局审计（DOM 实测，35 条断言；不是"看着像"）
$env:DEMO_BASE='http://127.0.0.1:<port-forward>'
cd web; node tests/layout-audit.mjs      # 期望 35/35

# 2) 真实后端截图 + 录屏（需要 demo 数据：一个成功 Run、一个失败 Run、一个 deploy Run）
$env:DEMO_RUNS='{"green":"r-...","red":"r-...","deploy":"r-..."}'
$env:DEMO_OUT='..\.data\demo\shots'
$env:DEMO_VIDEO='..\.data\demo\video'
node tests/demo-capture.mjs

# 3) mock 形态（CI 友好，不依赖后端）
pnpm exec playwright test tests/demo.spec.ts
```

> 截图里出现的 Run ID（`r-20261105-*`）是当次演示的真实记录，集群重置后不会再出现；要看形态而非复现数据。
