# TestPlane v0.1.2

这一版没有单一"主题"——是**界面重做 + 契约扩展 + 存储能力**三件事并行推进，外带两个把"看起来对其实不对"的问题修掉。

## 镜像（linux/amd64 + linux/arm64）

| 镜像 | index digest |
|---|---|
| `ghcr.io/ucm-system/testplane-server:0.1.2` | `sha256:f91cef0daaa7d0d60d2b26a3da6546dd824e3a7261b34ac001d2a815c9165dee` |
| `ghcr.io/ucm-system/testplane-web:0.1.2` | `sha256:16d169d77f207c31dba63a9493243a02f19b09ad98e8c3c202580063d4e985b3` |
| `ghcr.io/ucm-system/testplane-tester-endpoint-probe:0.1.2` | `sha256:0bbb95922e75338dd838aff39dccf67896dbe7296dfbc85aa89e1a2a67a6fbc0` |
| `ghcr.io/ucm-system/testplane-tester-aisbench-sla:0.1.2` | `sha256:2b2f9fbf3878b43b765b6ba9e538f5530fc222a9b4e5da5de934e060851c7023` |

## 1. 运行列表：真分页 + Excel 式列筛选（D-61）

原来只有"加载更多"的游标分页，没有页码、没有总数，筛选是四档 chip。这一版按用户要求重做：

- **UI 走 offset 分页**（`page`/`page_size`/`total`），agent 与 CLI 继续走 `cursor`，两者在**同一端点上互斥**；
- 新增 **`GET /runs/facets`**：`status`/`submitter`/`kind`/`tester` 四维 `{value,count}`，**按维度独立计数**（算"状态"时应用提交人过滤、不应用状态自己），否则勾完一个值其他的会凭空消失；
- 前端落 `@tanstack/react-table`（v8）+ Radix Popover：表头漏斗（搜索 + `值(计数)` 复选 + 清除本列）、页码栏（10/20/50）、**条件全进 URL**、改筛选取第 1 页；
- **真机抓到并修掉一个让分页变假的 bug**：归一化层把 `total`/`page`/`page_size` 丢了，UI 只能用 `items.length` 兜底，显示成"共 20 条 · 第 1 / 1 页"。

## 2. 概览与运行列表合并（D-62）

概览直接承载**全量 Run 表**（KPI 四卡 + 表格 + 分页同屏），`/runs` 退化为 `/?<query>` 兼容重定向（7 类参数全保留），右栏全站取消。

## 3. 界面重做（票 #72）

- **tokens 全量迁 oklch**：近白底、半透明边框、`--radius:0.625rem` 派生、两级阴影、统一焦点环；
- **字体切 Inter**（中文仍 Noto Sans SC），`--text-sm` 13 → 14px；
- **去色噪**：卡头 31 处染底 → 透明、KPI 数字 → 近黑、状态徽标 → 描边胶囊 + 彩点 + `data-tone`；
- **外壳换 shadcn `dashboard-01` 骨架**：inset 侧栏 256px ↔ 48px icon rail（`Ctrl/Cmd+B` + cookie 记忆）、sticky 页头面包屑、<768 抽屉；
- **可重复 UI 审计** `web/tests/ui-audit.mjs`：**P0 从 162 清到 0**（裸 hex / 半档字号 / 可点元素 / 对比度 / 圆角 / 阴影 / 焦点环），改前改后各 24 张截图；
- 两个"字发虚"的机械原因也修了：**非整数行高 → 整数 px**、品牌图按**显示尺寸 2×** 出图。

## 4. Tester 统一共享挂载（D-63）

给所有 Tester 统一挂集群共享存储读模型权重：

- 研究实测钉死形态：**内联 `volumes[].nfs` 被 PodSecurity `restricted` 直接拒绝建 Pod**，只能走 PVC；
- 平台级 `sharedMounts: [{name, mountPath, readOnly, claimName}]`，默认 `/models` 与 `/mnt/model`，**Profile 可关**，**未配置不阻断**；
- 提交期同路径冲突 → 400 `VALIDATION_ERROR` + `CONFLICT`；**唯一保留路径常量**修掉 `/tester` 与 `/workspace/input` 两套集合不一致；
- chart 提供**通用 `extraObjects`**（原样渲染任意 YAML）让运维贴 PV/PVC，平台仍不理解 NFS 协议；
- 权限必读：导出侧**目录 0755 / 文件 0644**，`fsGroup` 对普通 NFS 卷**无效**。

## 5. 结果/报告页分工 + 文档页 + API 页（D-64）

- 产物清单只留结果页，三组默认折叠；报告页瘦身；`report.md` 用 `react-markdown` 渲染；
- 侧栏底部新增 `文档` / `API` / `GitHub` 入口；`/api` 页**只消费 `openapi.yaml`**（52 端点 / 14 分组），不会与实现漂移。

## 6. 其它修复

- 页头滚动"顶一下"：改固定高外壳 + `main` 内滚（并修掉改容器时表格被裁的真 bug）；
- 删除三处"教用户看界面"的说明文字；
- `asPage` 透传 `total`/`page`/`page_size`；
- Tester 详情抽屉 448px → 1024px；`aisbench-sla` 0.4.0（密钥零泄漏 / 并发必须测出来 / SLA 码进码表）；
- YAML 编辑器改 `InputGroup` 形态（文件名 + 实时行列 + 复制 + 解析状态），**可拖拽改高度**。

## 升级注意

- **`/runs` 现在是重定向**：旧书签会跳转一次，`?status=&page=` 等参数保留；
- **chart 版本首次从 0.1.0 跳到 0.1.2**（v0.1.1 未改 chart 版本，属于历史遗留）；
- 生产请钉 **digest** 而不是 tag（`values.yaml` 已按此注释）。
