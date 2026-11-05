# TestPlane · 演示截图

**01–09 是真实后端 + 真实 kind 集群数据**；**10–14 带 `-mock` 后缀，是用 mock 数据截的**（只展示形态，不代表真实执行）。文件名与下表**一一对应**，改文件名必须同步本表。

| 文件 | 界面 | 数据来源 | 这一屏要证明什么 |
|---|---|---|---|
| `01-home.png` | 概览 | 真后端 | 圣杯三列（左 240 / 主内容自适应 / 右 288）；关键数字 30px `tabular-nums`；页头无「新建 Run」 |
| `02-runs.png` | 运行列表 | 真后端 | 列表工具条里是「筛选本页 Run」（不是全局搜索）；右栏给终态语义 |
| `03-run-succeeded.png` | Run 详情（成功） | 真后端 | 执行链路横向 + Task/Attempt 表 + 日志 + 右侧 SSE 事件流 |
| `04-retry-dialog.png` | 重试预览 | 真后端 | 复用/重跑/不可复用三段；失败项带 `failure.code`；确认后建**新 Run**（`retry_of`） |
| `05-report.png` | 报告页 | 真后端 | `report.json` 的 counts / Task 明细 / 失败清单 |
| `06-clusters.png` | 集群与队列 | 真后端 | 平台槽位 vs Volcano；队列 `reason` 是 PodGroup 原文；`available=false` 有降级卡片 |
| `07-admin.png` | 管理台 | 真后端 | 四个 Tab；Secrets 只登记引用不显示值；Audit 记录写与拒绝 |
| `08-run-deploy.png` | Deploy 形态的 Run | 真后端 | 两根轴（helm `status` + `endpoint.status`）；「ready 只是地址解析完成」 |
| `09-submit.png` | 提交 | 真后端 | Deploy 卡片显式存在；不部署时填 `url`/`model_name`/`model_path`；环境变量区 |
| `10-deploy-logs-mock.png` | Deploy 日志（单 Pod） | mock | Pod 页签、`next_offset` 续读、`gaps` 显式「此处不连续」 |
| `11-deploy-logs-multi-pod-mock.png` | Deploy 日志（PD 分离 4 Pod） | mock | 多 Pod 页签 + gap 标记 |
| `12-three-columns-1280-mock.png` | 1280 三列 | mock | 页头分隔线正对 240 / vw−288，三列等高 |
| `13-two-columns-900-mock.png` | 900 收右栏 | mock | 768–1024 隐右栏，左栏仍钉住 |
| `14-stacked-480-mock.png` | 480 堆叠 | mock | <768 单列 + 抽屉导航 |

> 截图里的 Run ID（`r-20261105-*`）是当次演示的真实记录，集群重置后无法复现；看形态而非复现数据。

## 怎么重新生成

```powershell
# 1) 布局审计（DOM 实测，不是"看着像"）
$env:DEMO_BASE='http://127.0.0.1:<port-forward>'
cd web; node tests/layout-audit.mjs      # 期望 35/35（另含"页头分隔线只能是 240 / vw-288"）

# 2) 真后端截图 + 录屏（需要 demo 数据：成功 / 失败 / deploy 各一个 Run）
$env:DEMO_RUNS='{"green":"r-...","red":"r-...","deploy":"r-..."}'
$env:DEMO_OUT='..\.data\demo\shots'
node tests/demo-capture.mjs

# 3) mock 形态（CI 友好，不依赖后端）
pnpm exec playwright test tests/demo.spec.ts
```
