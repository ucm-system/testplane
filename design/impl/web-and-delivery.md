# Web UI 与交付实现方案

> 本文是 `design/ARCHITECTURE.md`（下称 ARCHITECTURE，**v2.0**）在**前端与交付**两个面上的落地细化：只回答"怎么做"，不重述裁决。本文已按 D-66～D-73 同步（/tasks 路由、runner 管道时间线、部署 XOR 外接提交卡、整 Task 重试）。与 ARCHITECTURE 冲突时以它为准；本文给出的端口、超时、阈值都是**可调整的默认值**，改的时候同步 `control-plane.md` / `execution.md`。既定选型不再论证。

---

## W 部分 · 前端

### 1 技术栈与目录结构

| 关注点 | 选型 | 结论与理由 |
|---|---|---|
| 框架 | React 19 + TypeScript 5.x（`strict`，`noUncheckedIndexedAccess`） | 无 SSR 需求，SPA 足够（§11.4） |
| 构建 | Vite 7，`build.target=chrome110` | 内网 Chrome，无需 polyfill |
| 路由 | TanStack Router（文件式路由 + `beforeLoad` 取会话） | 类型安全 search params：`?node=runner:3&attempt=1` 可分享、可前进后退 |
| 服务端状态 | TanStack Query v5，`staleTime` 30s，终态对象 `Infinity` | 不做全局 store：Architecture 要求 UI 不持有 API 之外的状态（§2） |
| API 客户端 | `openapi-typescript` 生成 `types.d.ts` + `openapi-fetch`（~4KB gz） | 生成物入库，CI 校验"重生成后 diff 为空"；不用 orval（不引入第二个 schema 源） |
| 样式 / 组件 | **Tailwind CSS v4 + shadcn/ui**（Radix 底座，CSS 变量主题） | 决策 K-2：原型只作风格参考；主题色板从原型 tokens 映射，交互与可访问性用 shadcn 现成实现 |
| 图标 | `lucide-react`（shadcn 默认） | 与组件库同源，避免第二套图标集 |
| 虚拟滚动 | `@tanstack/react-virtual` | 日志与长表格，见 §6 |
| YAML 编辑 | CodeMirror 6（`@codemirror/lang-yaml`），**仅提交路由懒加载** | dry-run 要按行标错，需要带 range 的解析器；不用 textarea 叠色层（IME 与滚动同步问题） |
| 测试 | Vitest + MSW 单测、Playwright e2e（见 §13） | 与后端共用同一份 OpenAPI 声明，不手写 mock 类型 |

```text
web/
├── index.html / vite.config.ts / components.json      # shadcn 配置
├── src/
│   ├── main.tsx                  # QueryClient / Router / 全局样式
│   ├── routes/                   # 只放路由装配，不写业务逻辑；__root 即 AppShell
│   │   ├── tasks.index.tsx  tasks.$taskId.tsx
│   │   ├── submit.tsx            # 一张提交卡：部署 XOR 外接
│   │   └── runners.index.tsx   profiles.tsx   dashboard.tsx(P1)
│   ├── components/               # ui/（shadcn 生成）+ app/（AppShell、PipelineTimeline、LogViewer…）
│   ├── features/                 # 一能力一目录：组件 + hooks + 纯函数
│   │   └── task-list/ task-detail/ pipeline-timeline/ submit/ logs/ runners/ report/ retry/ board/
│   ├── lib/                      # api(generated) / query / sse / copy.ts / format.ts
│   └── styles/                   # globals.css（Tailwind + 主题变量）/ fonts.css
└── scripts/subset-fonts.mjs      # 字体子集重建，见 §5
```

切分规则：**`routes/` 里不出现 `fetch` 与状态机判断**；一个能力一个 `features/<name>/`，跨能力复用的只有 `components/` 与 `lib/`。`components/ui/` 只放 shadcn 生成/定制组件，不放业务逻辑。新 Runner 上线不改前端（§11.4），因此 `features/` 里不允许出现按 Runner 名分支的代码。

### 2 页面 ↔ 原型映射

> v2 路由全面 `/tasks` 化（D-73）：`/runs` 系列并入 `/tasks`；Task 详情按 **runner 管道时间线**呈现（无 DEPLOYING / CLEANUP 阶段图，D-70）。

| 路由 | 页面 | 右栏 | 数据来源 | 原型截图 |
|---|---|---|---|---|
| `/` | **概览 = 系统状态 + 全量 Task 表**（D-62）：KPI 四卡（进行中 / 排队中 / 成功率 / Runner 数）+ **完整 Task 表格**（Excel 式列筛选 + 游标分页 + 深链 URL）；右上「新建 Task」。**不再有独立的运行列表页** | 无（宽表，右栏取消；排队信息并入 KPI 区） | `GET /tasks`、`GET /tasks/facets`、`GET /queue-status`（30s 轮询） | `dashboard.png` |
| `/runs` | **仅作兼容重定向**：`/runs?<query>` → `/?<query>`（保留全部筛选与分页参数，旧链接与文档不失效）；`/runs/{id}` → `/tasks/{id}` | — | — | `runs.png` |
| `/tasks/$taskId` | 详情：概要 kv、**runner 管道时间线**（串行链路：deploy → pre_check → tests → clean，逐节点展开）、attempt 切换、节点详情（deploy / runner） | 事件时间线（首屏 `GET /tasks/{id}/events/history`，之后由 SSE 增量） | `GET /tasks/{id}` + `events/history` + SSE | `run-detail.png` |
| `/tasks/$taskId?tab=report` | 报告：指标表（按 labels 分组；仅 `result.json` 存在时渲染——D-74 三选一，仅 html/csv 的 runner 以产物预览/下载为主）、`report.md`、产物下载 | 报告分工说明 | `GET /tasks/{id}/attempts/{n}/runners/{seq}/result`、`GET /tasks/{id}/report.md`、`GET /tasks/{id}/artifacts`、`GET /tasks/{id}/files?path=` | `run-detail.png` 的「结果指标」卡 |
| `/tasks/$taskId?node=runner:N&tab=logs` | 日志：runner 执行记录级定位、跟随/分页 | 日志边界说明 | `GET /tasks/{id}/attempts/{n}/runners/{seq}/logs?stream=stdout&follow=1`（fetch 流） | `run-detail.png` |
| `/tasks/$taskId?node=deploy&pod=<pod>` | 部署 Pod 日志：按 Pod 分页签 + 重启/gap 标记 | Pod 拓扑与区间；**排队中显示 deploy-state.json 的 phase / 水位 / 已等待时长**（无位次，D-46） | `GET /tasks/{id}/deploy-state`、`GET /tasks/{id}/attempts/{n}/runners/{deploy_seq}/logs`；排队期无 Pod 时显示"尚未产生 Pod" | `deploy-logs.png` |
| `/submit` | 提交 Task：一张"服务来源"卡两形态（**部署 XOR 外接**，§10.1 互斥）+ env/secrets + runner 列表（每 runner 一个输入框，按 `input_format` 占位） | 参数说明（只读，来自 `runner.yaml.parameters`） | `GET /runners`、`POST /tasks?dry_run=true`、`POST /tasks` | `submit.png` / `submit-no-deploy.png` |
| `/runners` | Runner 目录：名称、版本、role、失败策略、`input_format`、参数数、exports/consumes；`?open=<name>` 打开详情抽屉（参数清单、env 声明、placeholder、镜像） | 注册规则 | `GET /runners`、`GET /runners/{name}` | `testers.png` / `tester-drawer.png` |
| `/profiles` | 执行环境：Profile / 集群 / 命名空间 / 挂载 | 边界说明 | `GET /profiles` | 无独立截图，见 `run-detail.png` 右栏 |

覆盖自检（§11.2 P0）：列表 ✅（概览全量 Task 表）、详情（runner 管道时间线）✅、报告 ✅、日志 ✅、单输入框提交 ✅、Runner 目录 ✅、取消/重试 ✅（详情页头：取消；重试 = 新 attempt，二次确认"将重新部署"）、环境信息 ✅、部署状态与 Pod 日志 ✅。看板为 P1（§11.4），路由保留、导航项按 `flags.board` 隐藏。

### 2.1 Task 详情：终态默认「结果视图」，执行细节二级展开

人打开一个**已结束**的 Task，绝大多数只想拿到结果，不想看编排细节。所以：

| Task 状态 | `/tasks/$taskId` 默认展示 | 其它内容的去处 |
|---|---|---|
| 终态（`succeeded` / `failed` / `service_unavailable` / `canceled`；v1 的 `completed_with_errors` 已并入 `failed`，D-73） | **结果视图**：结论（成功/失败/为什么失败）→ counts → 失败项列表 → **产物与下载**（`report.json` / `report.md` / `metadata.json` / `events.jsonl` / 各 attempt 的 runner 产物与日志 / 部署 Pod 日志，以及一键 `GET /tasks/{id}/bundle` 打包）→ 报告入口 | runner 管道时间线 / attempt 切换 / 实时日志 → 「执行细节」二级视图（折叠不是隐藏：排障一键展开） |
| 运行中（非终态） | 执行视图：runner 管道时间线 + 实时日志 + 等待原因 +（部署时）deploy-state 的 phase 与已等待时长 | 报告等终态后才有的区块显示占位说明 |

右栏保持"事件流 + 边界说明"两类，不塞主内容；主内容默认不超过 4 个块。

### 2.2 导航可见性（按 `07 §2.3` 的角色矩阵，不在前端另造规则）

| 导航项 | viewer | operator | admin |
|---|---|---|---|
| 概览 / 提交 Task / Runner 目录 / **集群与队列** | ✅ 可见（提交按钮对 viewer 置灰） | ✅ | ✅ |
| **管理台**（Profile / Cluster / Secret 引用 / 审计 / 清理产物） | —（不显示入口，直接访问显示无权限页） | — | ✅ |

> `集群与队列` 对 viewer **可见是设计要求的**：普通用户要能自己判断"卡在队列还是卡在平台槽位"（`ARCHITECTURE §10`）；它只读，不提供运维写操作。
### 2.3 错误与失败的呈现（三件套，不许只有 toast）

实测过的一次事故：提交失败的错误条渲染在表单最顶部、而用户视线停在底部按钮上，`[role=alert]` 的 `top=-279`、`inViewport=false`——错误"存在但看不见"。所以规则是：

1. **必滚到位**：校验/提交失败后 `scrollIntoView({block:'center'})` 把错误摘要滚进视口，并把焦点移到摘要容器（`role="alert"` + `aria-live="assertive"`，容器 `tabIndex=-1`）。
2. **字段级映射**：后端 `detail.issues[].path`（如 `task.name`、`task.runners[0].execution.profile`、jsonl 的 `line+field`）必须落到对应输入框——`aria-invalid` + 红边 + 字段下方一行红字；摘要里的每条可点击跳过去。**不要**把 issues 塞进一个 JSON 块里让人自己读。
3. **可复制、不自动消失**：摘要常驻，展示 `code` / `message` / `retryable` / `trace_id`（**一键复制**）——用户报障时唯一需要粘的就是 `trace_id`。

toast 只用于"成功且不需要后续动作"的反馈；需要用户动作的（复制 Task ID、跳详情、看失败项）一律用页面内区域 + 明确按钮。`dry_run`（校验）与真正提交用同一套呈现。

### 2.4 右栏文案：写给人看，不写接口名

右栏说明卡不许出现内部接口与实现措辞（"KPI 由 /tasks 与 /queue-status 现算，P0 没有独立看板接口""分页是游标：只回传 `next_cursor`""SSE 实时推送"这类）。要写**用户能拿去做判断的话**，例如：

- 概览右栏：「队列满 = 等资源；队列空还在排队 = 卡在平台槽位」
- 部署节点右栏：「queued / accepted 是平台推断的阶段，以 Genet 实际进度为准」
- 列表右栏：「列表不会自动刷新，切页或点刷新才更新」这类**行为**说明，而不是协议说明
- 事件流卡：「连接断开会自动重连；状态以列表页为准」

技术细节留在 `design/` 与 runbook，不进 UI。
**页面级操作只放页头**，"新建 Task"只在左导航与看板（原型约定）；详情页页头只放「重试（新 attempt）/ 取消 Task / 返回列表」这类操作当前对象的按钮。

> **原型的定位（K-2）**：原型只作**风格参考**——布局骨架、信息层级、状态谱、配色倾向可以照搬，但实现用 Tailwind + shadcn 重建，**不要求像素一致**，也不复制原型的 CSS 类名与自研组件。两处按 ARCHITECTURE 补齐：① 原型刻意不展示节点 / NPU / GPU 用量、概览页不放主按钮，§11.4 要求看板含节点资源块与右上「新建 Task」入口；② 原型的执行链路是顺序链，v2（D-69）与之对齐——**管道严格串行**，时间线渲染为单链逐节点展开，不存在并行节点。

### 3 数据层

**缓存键**（前缀即失效粒度，禁止在键里放数组对象，避免序列化不稳定）：

```ts
export const qk = {
  tasks:    ['tasks', 'list', filter] as const,          // filter 扁平化为 string
  task:     (id: string) => ['tasks', id] as const,
  attempts: (taskId: string) => ['tasks', taskId, 'attempts'] as const,
  runners:  (taskId: string, n: number) => ['tasks', taskId, 'attempts', n, 'runners'] as const,
  runner:   (taskId: string, n: number, seq: number) => [...qk.runners(taskId, n), seq] as const,
  result:   (taskId: string, n: number, seq: number) => [...qk.runner(taskId, n, seq), 'result'] as const,
  deployState: (taskId: string, n: number) => ['tasks', taskId, 'deploy-state', n] as const,
  logs:     (path: string, offset: number) => ['logs', path, offset] as const,
  // 低频：['board'] / ['runners'] / ['runners', name] / ['profiles'] / ['secrets'] / ['session']
};
```

**SSE 与 Query 的合并**（`lib/sse/useTaskEvents.ts`；**日志内容不走 SSE**，由日志面板自己读流，见 §4.3）：

| 事件 `type`（`control-plane.md` C-9.1） | 客户端动作 |
|---|---|
| `task.submitted` | 失效 `['tasks','list']`（新建跳转走详情，不依赖该事件） |
| `task.state_changed` | `setQueryData` 局部 patch 状态；**同时** `invalidateQueries(qk.attempts(id))` |
| `runner.state_changed` / `runner.started` / `runner.finished` | `invalidateQueries(qk.runners(id, attempt_no))` + `invalidateQueries(qk.result(id, attempt_no, seq))`；不 patch，留给服务端算 |
| `exports.harvested` | 失效 `qk.runner(id, attempt_no, seq)`（exports_ref 更新） |
| `deploy.state_changed` | 失效 `qk.deployState(id, attempt_no)`（phase / 水位 / Pod 数可能变）；日志面板另行 refresh 页签 |
| `clean.finished` / `task.finished` / `report.written` | `task.finished` 时 `invalidateQueries(qk.task(id))` + `['tasks','list']` 并关闭连接；`report.written` 失效 `report` 与 `artifacts` |

规则（按序执行，缺一不可）：
1. **seq 去重**：信封 `{seq, type, task_id, attempt_no?, runner_seq?, at, payload}`，`seq` 在 Task 内单调递增（服务端 `events.seq` 生成，见 `control-plane.md` C-9.2）。客户端 `lastSeq` 存在 `sessionStorage["sse:"+taskId]`，收到 `seq <= lastSeq` 直接丢弃。
2. **Last-Event-ID 重连**：请求头带 `Last-Event-ID: <lastSeq>`，服务端从 `seq+1` 回放（窗口 24h、单次上限 1000 条；C-9.2），超出窗口/条数时服务端发 `resync_required` → 客户端整页失效重建。
3. **不用原生 `EventSource`**：要带 `Last-Event-ID`、要在 401 时跳 OIDC 而不是静默重连、要在 `document.visibilityState==='hidden'` 时暂停。用 `fetch` + `ReadableStream` + `AbortController` 自写 ~120 行（`text/event-stream` 按 `\n\n` 切帧）。
4. **心跳与退避**：服务端 15s 一条 `: ping`；客户端 60s 收不到任何帧即重连，重连退避 1s → 2s → 4s → 8s → 上限 30s；连续 3 次失败降级为 5s 轮询 `GET /tasks/{id}`，恢复后回切并停轮询。
5. **单连接**：连接挂在 `/tasks/$taskId` 路由的 `beforeLoad` context 上，详情页内切节点/切 attempt **不重建**连接。

```ts
// lib/sse/useTaskEvents.ts —— 只管连接、去重、合并；事件集合与信封以 C-9 为准
const onFrame = (e: Envelope) => {
  if (e.seq <= lastSeq) return;                       // ① 回放与重连都可能重复
  lastSeq = e.seq; persist(taskId, lastSeq);
  switch (e.type) {
    case 'task.state_changed':
      patchState(e); qc.invalidateQueries({ queryKey: qk.attempts(taskId) }); break;
    case 'runner.state_changed': case 'runner.finished':
      qc.invalidateQueries({ queryKey: qk.runners(taskId, e.attempt_no!) }); break;
    case 'deploy.state_changed':
      qc.invalidateQueries({ queryKey: qk.deployState(taskId, e.attempt_no!) }); break;
    case 'task.finished': qc.invalidateQueries({ queryKey: ['tasks'] }); ctrl.abort(); break;
  }
};
```

**错误码 → 全局提示**（附录 D 全集的 UX 归类，`lib/copy.ts` 一张表驱动）：

| 呈现 | 错误码 | 行为 |
|---|---|---|
| 顶部 banner（不自动消失） | `PRECHECK_FAILED`、`ENDPOINT_UNREACHABLE`、`EXECUTOR_UNAVAILABLE`、`INTERNAL_ERROR` | 带排查方向（§7.5；Task 级 `service_unavailable` 用同一 banner 形态），提供"复制诊断信息" |
| 表单字段级 | `VALIDATION_ERROR`、`TASK_TOO_LARGE`、`RUNNER_NOT_FOUND`、`EXECUTION_PROFILE_NOT_FOUND`、`CONFLICT` | 按 `error.detail.path`（`runners[2].input`（jsonl 另带 `line`）/ `deploy` / `env.KEY`）定位到控件；YAML 侧再带 `line/col` |
| toast（一次）/ 静默 | `CANCELED`、`NOT_FOUND`（对象已变，刷新列表）；`CLEANUP_FAILED` 只在事件栏与看板计数出现，不影响 Task 结果（§6.6） | 文案指明对象已失效，不铺满屏幕 |
| 内联在对象卡上（不弹窗） | `RUNNER_TIMEOUT`、`RUNNER_NONZERO_EXIT`、`RESULT_MISSING`、`RESULT_INVALID`、`EXPORT_INVALID`、`ATTEMPT_EXHAUSTED`、`ARTIFACT_QUOTA_EXCEEDED`、`IMAGE_PULL_FAILED`、`MOUNT_UNAVAILABLE`、`PRECHECK_FAILED`、`ENDPOINT_UNREACHABLE` | 显示码 + 中文说明 + `retryable` 徽标；结果页的「重试」按钮 = 整 Task 新 attempt（二次确认"将重新部署"） |
| 跳转登录 | `UNAUTHENTICATED` | `location.assign('/auth/login?return=' + encodeURIComponent(current))` |
| 只读降级 | `FORBIDDEN` | 隐藏写按钮 + 页头提示"当前角色 viewer"；**以服务端返回为准**，不在前端推断角色 |

**分页**：一律游标，不用 offset。`GET /tasks?limit=50&cursor=<opaque>` → `{items, next_cursor}`；游标由服务端编码排序键 `(submitted_at DESC, id DESC)`，客户端只回传不解析。前端 `useInfiniteQuery`，`limit` 默认 50、上限 200。日志是唯一例外（按字节 `offset`，见 §4.3）。

### 4 关键交互实现

#### 4.1 提交页

| 控件 | 实现 | 明确不做 |
|---|---|---|
| runner 输入框 | 每个 runner 一个 `textarea`（等宽 12.5px / 1.7 行高 / `resize:none` / 随内容 104–360px 增高），`placeholder` 取 `runner.placeholder`（与 `input_format` 同格式，D-67）；下方静态提示"公共参数（`AG_SERVED_*`）由平台经环境变量注入，不需要写在输入里" | 不渲染表单、不做类型/必填/枚举校验、不预填；前端**不做** jsonl 结构校验（那是提交校验器的职责，dry-run 统一给行号级错误） |
| 照上次配置 | `?from=<task_id>` 时用 `GET /tasks/{id}/manifest` 回填最近一次 Task 的 runner 组合与 `input` **原文**（§11.2 唯一允许的预填：填的是上一次的输入） | 不推导、不合并、不"智能补全" |
| Task `env` | 行编辑器（key / value / 删除 / + 一行），明文可回显可编辑；**Task 级填一次，注入全部 runner**（§12.4） | 不脱敏、不折叠（§12.4 裁决） |
| `secrets` | key 输入 + 下拉，选项来自 `GET /secrets`，只显示 **名称与说明**；无可选项时给"去登记"文案（仅 admin 可见） | 不回显值、不提供"新建 Secret"内联表单（登记是 admin 独立页） |
| 服务来源（一张卡两形态，**部署 XOR 外接**，§10.1） | segmented：`部署` → YAML 编辑器（Genet 部署描述，原样作为 service-deploy runner 的输入；平台不解析）；`外接` → `url` / `model_name` / `model_path` 三字段 | 两形态**互斥**（都填/都不填提交被拒），切换时**保留**各自草稿不清空；不提供第三种"既部署又手填" |
| `precheck` | `auto` / `off` 分段 + 显式对象（折叠区） | 不替用户判定"该不该开" |
| dry-run | 提交按钮旁「校验」；`POST /tasks?dry_run=true` 与 `POST /tasks` 用**同一份 Manifest**，返回 `{valid, issues[]}` | 不在输入时实时校验（等于渲染校验），只在点击时调 |

提交前流程：点「提交」→ 先 dry-run → 有 `VALIDATION_ERROR` 就地标红并滚到第一个错误（不弹 toast 堆叠）→ 无错再 `POST /tasks`（**不做幂等**：每次提交都创建新 Task，重复提交就是多个 Task，D-37）→ 成功跳 `/tasks/$id`，toast 显示 Task ID。
错误定位规则：`detail.path` 与表单控件一一对应（`service.url`、`env.ENDPOINT_API_KEY`、`runners[0].input`、`deploy`），jsonl 校验错误带 `line + field`，YAML 解析失败额外给 `line/col`，编辑器 `dispatch(setDiagnostics)`。

#### 4.2 详情页

- **runner 管道时间线**（取代 v1 的并行链路图，D-69/D-70）：HTML/CSS（`.flow` / `.flow-node` / `.flow-arrow`），横向按 `seq` 排列，不用 SVG 也不做拖拽（§11.3）。节点 = `[service-deploy]` + `[pre_check]` + `runners[]` + `[service-clean]` 的**串行链**（v2 无 DEPLOYING/CLEANUP 阶段图——deploy 与 clean 就是管道首尾两个 runner 节点）。deploy 节点 meta 显示 deploy-state 的 `phase` + 已等待时长；非终态时恰好一个节点处于运行态（串行）。节点 tone 走 `nodeTone()` 同款映射；超出宽度横向滚动，名字 `ellipsis` + `title`。
- **attempt 切换**：`?attempt=n`，默认最后一个；`result` / `logs` / `artifacts` 三处共享该参数，历史 attempt 标"只读"。attempt 列表永远全量展示（重试是追加不是覆盖，§7.2）。
- **参数快照**：`input` 原文（输入落档 `attempts/<n>/input/<seq>/`）等宽只读 + 复制；并列展示 `runner` / `version` / `image_ref`（+ 实际 `image_digest`，G-16）冻结值（§5.7）。**不提供编辑**，改参数 = 新 Task。
- **结果指标表**：列 `指标 / 值 / 单位 / 统计口径 / labels / sample_size`；按 `labels` 排序后的 JSON 串分组，组间用 `tbody` 分组行分隔。**不聚合、不计算、不判定达标**（§8.4）。仅当 `result.json` 存在时渲染指标表；只产出 `result.html` / `result.csv` 的 runner（D-74）以产物预览/下载为主，指标区显示"该 Runner 产出非 JSON 结果，见产物"。空态按状态分文案：`SKIPPED`"未执行"、`CANCELED`"被终止"、`FAILED`"执行失败"，绝不显示"0"。
- **`waiting_reason`**：`QUEUED` 时卡片头显示中文枚举——`slot_waiting`（外接模式端点被其他 Task 独占）、`global_limit` 平台并发、`queue_full` 队列已满、`insufficient_resources` 资源不足、`gang_pending` gang 未满足；`PENDING` 显示"等待前序 runner"（串行管道，D-69）。带"看队列（P1）"链接，不提供插队（§6.9）；部署排队不在 waiting_reason 里，在 deploy 节点的 deploy-state 卡上呈现。

```tsx
// features/pipeline-timeline/PipelineTimeline.tsx —— 纯展示：节点是按钮，不是拖拽画布；严格串行单链
<ol className="flow" aria-label="runner 管道时间线">
  {deploy && <NodeBtn kind="deploy" tone={tone(deploy.status)} label="service-deploy"
                      meta={deployState?.phase === 'queued'
                        ? `排队中 · 已等待 ${fmt(deployState.waited_seconds)}`
                        : `${deployState?.pods?.length ?? 0} pod · ${deploy.duration}`} />}
  {runners.map((r) => (
    <NodeBtn key={r.seq} kind="runner" tone={nodeTone(r.status)} label={r.name}
             meta={r.waiting_reason ? WAIT_REASON_CN[r.waiting_reason] : r.duration}
             active={node === `runner:${r.seq}`} />
  ))}
  {clean && <NodeBtn kind="clean" tone={tone(clean.status)} label="service-clean"
                     meta={clean.duration} />}
</ol>
```

#### 4.3 日志查看

| 项 | 做法 |
|---|---|
| 定位 | 以 `task_id + attempt_no + runner_seq` 为唯一定位（Cache key `logs(taskId, n, seq, offset)`），切换节点/attempt 时 `AbortController.abort()` 上一路请求；不走"客户端拼文件 path"的形态 |
| 跟随 / 分页 | 首屏 `?tail=2000&follow=1`（`AG_LOG_TAIL_LINES`）；向上翻页与终态读取用 `?offset=&limit=`（单次 ≤ 64KiB 行数，C-12）；`follow` 走 `text/plain` + `ReadableStream` 逐块追加，`task.finished` 或 Tab 隐藏时停跟 |
| 虚拟滚动 | 10k 行以上启用 `useVirtualizer`（固定行高 20px）；20k 行以上只保留"尾部跟随"，向上滚动触发向前分段加载 |
| 高亮 | 行首模式匹配：`--- [container restarted #N` → 琥珀 + 左侧 3px 竖条；`--- [log gap:` → 红虚线框；`ERROR`/`Traceback`/`OOMKilled` → 红；其余按原型深色终端风（底 `#0b1220`） |
| 部署 Pod 日志 | 页签来自 deploy runner 产物目录的 `pods.json`（name / role / node / restarts / 日志区间，§7.2），一个 Pod 一个页签，**不合并**；页签角标显示重启次数与 `container restarted` 段数；Pod 清单快照由 deploy-state.json 同步 |
| 宣告 | 运行中页头 `.live-dot` + "追加中"；每份日志显示覆盖区间（`08:12:03 – 09:40:11`），与"测试期间仍在追加"文案（§8.1） |

#### 4.4 取消 / 重试

| 动作 | 前置 | 弹窗（shadcn `Dialog`，焦点陷阱 + Esc） |
|---|---|---|
| 重试（详情页头；**整 Task 新 attempt**，§7.8 / D-69） | Task 已终态 | **二次确认**：部署模式必须显示"**将重新部署**（新 attempt 全量重跑，不复用已部署服务）"，外接模式显示"将全量重跑"；底部"新 attempt `#n+1`，历史 attempt 保留只读"；`ATTEMPT_EXHAUSTED` 时按钮置灰并提示上限（默认 3） |
| 取消 Task | 非终态（含部署排队中） | 两栏：「将标记 SKIPPED（未开始）」与「将终止并标记 CANCELED（正在跑）」；部署模式额外说明"取消后会执行 service-clean（拆除已部署资源），clean 完成才落终态，可能不是即时生效" |
| 泄漏重放 `clean`（admin/operator，Task 终态后） | 存在部署且 clean 无成功记录 | 说明"将用落档导出重拉一次 service-clean Job（幂等）"；成功后刷新 clean 节点状态 |

确认后统一 `POST`（带 `X-CSRF-Token` 双提交，C-10），按钮进入 pending 态并禁用；成功不跳转，只让 SSE 把状态推回来（避免本地乐观更新与"先落库者为准"打架，§6.6 取消竞态）。

### 5 样式与设计令牌

**结论：Tailwind CSS v4 + shadcn/ui 为实现基座，原型 CSS 不再作为实现依赖（K-2）。**

- **主题映射**：把原型 `tokens.css` 的色板 / 字号 / 圆角 / 间距搬进 shadcn 的 CSS 变量（`--background`、`--foreground`、`--primary`、`--muted-foreground`、`--border`、`--ring`…），放在 `globals.css`；业务组件只用语义变量，**禁止裸 hex**（保留原型的配色纪律）。
- **组件映射**：原型 card → `Card`；badge → `Badge`；表格 → `Table`（排序/多选可用 TanStack Table）；抽屉 → `Sheet`；弹窗 → `Dialog`；输入 → `Input` / `Textarea` / `Select`；页签 → `Tabs`；提示 → `Sonner`；事件时间线用 `Card` + `ul` 自研。
- **圣杯布局**：`AppShell` 用 Tailwind grid（`grid-cols-[240px_minmax(0,1fr)_288px]`），DOM 顺序 `main` 在前、两侧 `aside` 用 `col-start` 摆位；断点与原型一致（`>1024` 三列、`768–1024` 隐右栏、`<768` 堆叠）。
- **业务专有视图**（执行链路图、日志终端、指标表分组）用 Tailwind 原子类或局部 `*.module.css`；不再有全局 `hg-*` 覆盖层，也不引 CSS-in-JS。
- **shadcn 用法**：`pnpm dlx shadcn@latest add <component>`，生成物入库（可审计、可改）；`components/ui/` 只放基元，不放业务逻辑。升级走 diff review。
- **禁止**：用 shadcn 默认主题覆盖原型色板；引入第二套图标集（图标统一 `lucide-react`）。

**字体**：继续自托管 Albert Sans（拉丁）+ Noto Sans SC 子集（中文），`--font-sans` 指向它们——shadcn 默认字体栈对中文混排不友好。构建期重建流程（`web/scripts/subset-fonts.mjs`，产物入库）：

```bash
# 1. 采集真实 CJK：前端固定文案 + 后端错误/状态文案 + 已注册 Runner 的 yaml
node scripts/collect-glyphs.mjs \
  --from 'src/**/*.{ts,tsx}' --from 'design/prototype/index.html' \
  --from 'runners/*/runner.yaml' --from 'server/internal/copy/*.go' \
  --out .cache/glyphs.txt
pyftsubset NotoSansSC[wght].ttf --text-file=.cache/glyphs.txt \
  --flavor=woff2 --layout-features='*' \
  --output-file=src/styles/fonts/noto-sans-sc-var.woff2   # fonttools，CI 镜像内已装
# 3. 覆盖率校验：构建期对字符串字面量做比对，缺字只 WARN 不改行为
```

规则：`--font-sans` 以自托管的 Albert Sans / Noto Sans SC 打头，尾部留系统兜底——**不以 `-apple-system` 打头**（Windows 上不存在，Latin 会掉到 Segoe UI 与中文对不齐）；拉丁用 Albert Sans 可变字重（31KB，`preload`），中文子集目标 `≥99.5%` 覆盖固定文案，**数据驱动的中文（Runner 名、错误信息、用户填的 Task 名）允许回落**到系统栈——接受它而不打包 8MB 全量字体。CI 在文案 PR 上跑覆盖率检查；新增 Runner 或新错误码文案时必须重建（否则该页出现两种中文字形）。

### 6 性能与可访问性

| 项 | 默认值 / 做法 |
|---|---|
| 首屏 budget / 拆包 | entry JS ≤ 250KB gz（Tailwind + shadcn 基元）、CSS ≤ 60KB gz、字体 178KB（latin preload，中文 `swap`）、LCP < 2.5s；每个 route `lazyRouteComponent`，CodeMirror / `react-virtual` / `report.md` 渲染器各自独立 chunk，不进首屏 |
| 表格虚拟化阈值 | Task 列表 > 100 行、日志 > 10k 行；低于阈值渲染完整 DOM（否则滚动条跳动） |
| SSE 降级 | 见 §3 第 4 条；降级时页头显示"实时更新已降级为 5s 轮询"，不静默 |
| 键盘可达 | 全局 skip link（跳到主内容 / 跳到导航）；focus 环统一 `--ring`（3px，不用 `outline:none`）；可点击表格行用 `tabIndex=0` + `Enter/Space` 且整行有 `<a>` 语义兜底；Dialog/Sheet 用 shadcn 自带的焦点陷阱 + `Esc` + 关闭后焦点归位 |
| 状态表达 | 状态不只靠颜色：badge 一律"文字 + 色"，链路节点带 `aria-label`；弱化文字用 `--muted-foreground`（对应原型 `#667085`，4.9:1），中文正文不低于 13.5px；只中文不引 i18n 库，文案集中在 `lib/copy.ts`（同时作为子集采集源） |
| 动效 | 用 Tailwind `motion-safe:` 变体；全局 `@media (prefers-reduced-motion: reduce)` 关掉位移/缩放/animation |

### 7 构建与集成

```text
CI: pnpm --dir web build → web/dist
  → cp -r web/dist server/internal/webui/dist     （go:embed 不能跨模块目录，必须复制）
  → cd server && go build ./cmd/argusd        （//go:embed all:dist；Go 模块在 server/）
```

| 项 | 结论 |
|---|---|
| 模块布局 | **Go 模块在 `server/`**（`server/go.mod`），`web/`、`runners/`、`deploy/` 与它平级；`go:embed` 只能嵌包内相对路径，因此前端产物复制进 `server/internal/webui/dist`，`runners/` 由镜像 `COPY` 到 `/app/runners`（E-10） |
| embed | `server/internal/webui/embed.go`：`//go:embed all:dist` + `fs.Sub`；`dist/` 入 `.gitignore`，本地 `make web` 生成 |
| 缓存头 | `/assets/*`、`/fonts/*` → `public, max-age=31536000, immutable`（Vite 内容 hash）；`/index.html` 与所有 SPA fallback → `no-cache, must-revalidate`；`/api/*` → `no-store` |
| 同源 | 单二进制同时服务静态资源与 `/api/v1`；fallback 规则：`/api/`、`/metrics`、`/healthz`、`/readyz` 不 fallback，其余 404 返回 `index.html` |
| CSP | `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`（`style-src` 留 `unsafe-inline`：Radix 定位与 CodeMirror 依赖内联样式；不接受 `unsafe-eval`）；另加 `X-Content-Type-Options: nosniff`、`Referrer-Policy: same-origin`、HSTS（Ingress 层） |
| 开发代理 | `vite server.proxy['/api'] → http://localhost:8080`；**SSE 要单独处理**：代理段关闭压缩并在 `configure` 里 `proxyRes.headers['cache-control']='no-cache'`，否则 dev 下事件被 buffer |
| 版本 | `__APP_VERSION__` 由 `git describe --tags --dirty` 注入；`GET /api/v1/meta` 返回同值，页脚不一致时提示"前后端版本不一致" |

---

## D 部分 · 交付

### 8 镜像与 CI

```dockerfile
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build                       # → /src/web/dist
FROM golang:1.24-alpine AS build
WORKDIR /src/server
RUN apk add --no-cache git ca-certificates
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
COPY --from=web /src/web/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/argusd ./cmd/argusd

FROM gcr.io/distroless/static-debian12:nonroot   # 无 shell、无包管理器
COPY --from=build /out/argusd /argusd
COPY runners/ /app/runners/                       # registry 扫描源（E-10），非 embed
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/argusd"]
```

| 项 | 结论 |
|---|---|
| 角色 | 同一镜像 `--role=all`（缺省）/ `api` / `controller` / `collector`；拆角色只是改 arg，小规模默认 `all` |
| 迁移 | **不进 entrypoint**（多副本会并发迁移），由 Helm pre-upgrade hook Job 执行，见 §11 |
| 版本号 | tag `vMAJOR.MINOR.PATCH`；镜像必打 `:git-<short-sha>`，仅在 git tag 构建时追加 `:vX.Y.Z`；**不打 `:latest`**（避免隐式漂移）；部署用 digest，记录到 `deploy/versions.lock`。内置 runner（service-deploy / service-clean / endpoint-probe）随平台镜像同版本发布 |
| 合规 | 镜像非 root、无 shell、无 build 工具；`docker scout`/`trivy` 扫 HIGH 以上阻断 |

CI 阶段（单流水线，按 `paths` 过滤触发）：

| 阶段 | 命令 | 门禁 |
|---|---|---|
| lint / openapi | `golangci-lint run`、`eslint --max-warnings=0`、`prettier --check`；`spectral lint api/openapi.yaml` + 重生成后 `git diff --exit-code` | 阻断（spec 是实现唯一源） |
| test / schema | `go test -race ./...`、`pnpm vitest run`；`runner.yaml` 校验（复用 registry 校验器）+ `result.json` 附录 A golden 校验 | 覆盖率 gate（views/05 §7.2）：`domain ≥85%`、`orchestrator ≥70%`、`executor/k8s ≥70%`、总体 ≥60% |
| build / image | `go build` + `pnpm build` + 体积 budget 检查；buildx 多架构（amd64/arm64）→ push → 打 digest | budget 超 10% 阻断；image 仅 `main` 与 tag 推送 |
| helm | `helm lint` + `helm template` + `ct lint` + `helm package` | 阻断 |

Runner 镜像流水线（独立 workflow）：`runners/*/` 变化触发矩阵，**上下文 = Runner 根目录**、`-f build/Dockerfile`（§5.5），tag 取该目录 `runner.yaml.version`；构建后冒烟：按 `runner.yaml.entry[0]` 检查 entry 文件存在且可执行（`docker run --rm --entrypoint test <image> -x <entry[0]>`，§5.6），**不在 CI 里跑真实测试**（那是 Runner 自己的 `tests/`）。合并到 `main` 才推送；PR 只构建不推送。

> 控制面镜像的构建上下文是**仓库根**（要用 `server/`、`web/`、`runners/` 三个目录）；`.dockerignore` 排除 `design/`、`web/node_modules`、`.git`，避免几 MB 原型与依赖进构建层。

### 9 Helm chart

`deploy/helm/argus/`，values 骨架：

```yaml
image: { repository: registry.internal/argus/argusd, tag: "", digest: "", pullPolicy: IfNotPresent }
replicaCount: 1                    # P0 单副本；P1 起可 2（选主后 controller/collector 单活）
role: all                          # all | api | controller | collector
resources: { requests: {cpu: 500m, memory: 512Mi}, limits: {cpu: "2", memory: 2Gi} }
storage: { existingClaim: argus-storage, mountPath: /storage-root }
database: { host: "", port: 5432, name: argus, sslMode: require, existingSecret: argus-db }
oidc: { issuer: "", clientId: "", existingSecret: argus-oidc, roleClaim: groups, viewerGroups: [], operatorGroups: [], adminGroups: [] }
# v2（D-70）：无 genet 平台配置——Genet OpenAPI 地址与 Open API Key 由 Task 级 secrets 注入
# service-deploy / service-clean runner（secret_env）；genet-local 地址在 dev values 里经 runner env 传入
bootstrap:                         # 结论：Profile / 集群由 DB 维护（§12.5 由 admin 管），values 只做首次 seed
  executionProfiles: []            # 例：[{name: k8s-npu, cluster: cluster-01, namespace: argus, mounts: [...]}]
  clusters: []                     # 集群名 → Secret 中的 kubeconfig key
sse: { heartbeatSeconds: 15, maxConnectionsPerTask: 5, replayWindowHours: 24 }   # maxConnectionsPerTask 为 P1 限流位，P0 不实现
retention: { days: 30, sweepCron: "0 3 * * *" }
networkPolicy: { enabled: true, egressAllow: ["10.0.0.0/8", "registry.internal/32", "{{ nfsServer }}/32"] }
```

| 对象 | 关键点 |
|---|---|
| Deployment | `strategy: RollingUpdate{maxUnavailable:0,maxSurge:1}`；`securityContext`: `runAsNonRoot`/`readOnlyRootFilesystem`/`allowPrivilegeEscalation:false`/`capabilities.drop:[ALL]`/`seccompProfile: RuntimeDefault`；`emptyDir` 只挂 `/tmp`；`startupProbe: /healthz`（30×2s，覆盖迁移等待）→ `readinessProbe: /readyz`（含 DB ping + 迁移版本匹配）→ `livenessProbe: /healthz`（**不含 DB**：DB 抖动不该重启进程） |
| Service | ClusterIP 8080；Ingress 注解：`proxy-buffering: "off"`、`proxy-read-timeout: "3600"`（SSE）、`proxy-body-size: 8m` |
| ConfigMap / Secret | ConfigMap 装非敏感配置（端口、并发上限、超时、保留期），`checksum/config` 注解触发滚动；Secret 用 `existingSecret` 引用（`argus-db` / `-oidc` / `-genet` / `-clusters`），chart **不生成任何明文** |
| ServiceAccount + RBAC | 见下表；`automountServiceAccountToken: true` 仅控制面需要 |
| PVC | `ReadWriteMany`（NFS CSI 或 `nfs:` volume），只挂控制面到 `/storage-root`；Runner Pod 由 Executor 以 **同 namespace 同 PVC 的 subPath** 挂 `/workspace/output` |
| NetworkPolicy | `default-deny` ingress+egress；放行 ingress：ingress-controller → 8080、Prometheus → 8080；放行 egress：kube-dns(53)、Postgres、Genet、镜像仓库、`egressAllow` 目标网段、NFS server（2049） |
| PSA / PDB | namespace 打 `pod-security.kubernetes.io/enforce: restricted`（控制面与 Runner Pod 都要在该档下可跑）；`PDB.minAvailable: 1` 保证滚动期间可用 |
| 选主 | controller 用 Lease `argus-controller`；collector 用 `argus-collector-leader` + **每 Job 一个** `arg-log-<hash>`（E-7.3），collector 因此可多副本分片 |

RBAC（最小集，`Role` 限定在 `execution.namespace`）：

| 资源 | 动词 | 用途 |
|---|---|---|
| `batch/jobs` | get, list, watch, create, delete | 创建/跟踪/取消 runner Job |
| `pods` / `pods/log` | get, list, watch, delete / get | 状态对账、按 deploy-state `label_selector` 发现部署 Pod、取消时兜底删 Pod；日志采集（`follow`） |
| `persistentvolumeclaims` | get | 渲染期校验 Profile 挂载的 PVC 存在（E-3.4） |
| `events` | list | 复核 Pod 的 `FailedMount` / `FailedScheduling` 事件（E-4.4） |
| `coordination.k8s.io/leases` | get, create, update | 选主 |
| `scheduling.volcano.sh/podgroups` | get, list, watch | 只读：gang 是否满足 |
| `scheduling.volcano.sh/queues` | get, list, watch | 只读：队列状态与 `deserved`/`allocated` |
| `scheduling.volcano.sh/jobs` | get, list, watch | 只读：Volcano Job 状态（E-11） |

**集群级资源**（`nodes`、`queues`、`podgroups`、`volcano jobs`）需要 `ClusterRole`（只读）；namespace 级资源用 `Role`。**不授予 `secrets`**：日志不脱敏（K-5），平台不需要读 Secret 明文，凭据只由 K8s 在 Pod 创建时注入。

### 10 配置与密钥

| 配置项（env / flag） | 默认值 | 说明 |
|---|---|---|
| `AG_LISTEN` / `--listen` / `--role` | `:8080` / `all` | 监听地址；角色 `all`/`api`/`controller`/`collector` |
| `AG_DB_DSN` | 必填 | Secret 注入；含 `sslmode=require` |
| `AG_DB_MAX_OPEN_CONNS` | `20` | 状态查询 P95 300ms 的关键参数 |
| `AG_STORAGE_ROOT` | `/storage-root` | NFS 挂载点，承载 `<task_id>/attempts/<n>/…` 产物树与报告 |
| `AG_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` | 必填 | 会话 cookie 流程；`_CLIENT_SECRET` 来自 Secret |
| `AG_OIDC_SCOPES` / `AG_OIDC_ROLE_CLAIM` | `openid profile email groups` / `groups` | 需要 `groups` 才能映射 `viewer/operator/admin` 三组 |
| `AG_SESSION_COOKIE` / `_TTL` / `_KEY` | `ag_session` / `8h` / 必填 32B | `HttpOnly; Secure; SameSite=Lax; Path=/`；签名密钥轮换时新老共存 1 个 TTL |
| `AG_AUTH_MODE` | `oidc` | `dev` 仅在 dev 环境允许，**其他环境启动即拒绝** |
| ~~`AG_GENET_BASE_URL` / `_TOKEN_FILE`~~ | - | **v2 移出平台配置（D-70）**：Genet OpenAPI 地址与 Open API Key 经 Task 级 `secret_env` 注入 service-deploy / service-clean runner；genet-local 地址在 dev values 经 runner env 传入 |
| `AG_DEPLOY_TIMEOUT` | `72h` | 部署总窗口（= service-deploy runner 的执行时限，K-6）；排队计入窗口，取消不受限 |
| `AG_CLUSTERS_DIR` / `_DEFAULT_CLUSTER` / `_EXECUTOR_NAMESPACE` | `/etc/argus/clusters` / 空 / `argus` | 一集群一 `.kubeconfig`；未指定时用默认集群（生产建议显式）；Job 与 PVC 所在 namespace |
| `AG_RUNNER_TIMEOUT` / `_MAX` | `4h` / `24h` | 默认 runner 超时；Manifest `runners[].timeout_seconds` 可覆盖，超上限拒绝 |
| `AG_ATTEMPT_MAX` | `3` | 单 Task attempt 上限（重试超限 `ATTEMPT_EXHAUSTED`，§12.7） |
| `AG_CONCURRENCY_MAX` / `TASK_MAX_RUNNERS` | `20` / `200` | 全局并发 runner 上限 / 单 Task runner 数上限（§12.7）；超出 `TASK_TOO_LARGE` |
| `AG_ARTIFACT_MAX_BYTES` / `LOG_SEGMENT_MAX_BYTES` / `RETENTION_DAYS` | `10GiB` / `200MiB` / `30` | 单 attempt 产物上限（`ARTIFACT_QUOTA_EXCEEDED`）；部署 Pod 日志分段；产物保留期（v2 `cleanup.policy` 取消，保留期只由本配置决定，D-70） |
| `AG_RESULT_MAX_BYTES` | `100MiB` | `result.json` 技术上限（仅防 OOM；K-10 不设业务限制） |
| `AG_SSE_HEARTBEAT` | `15s` | 心跳间隔；客户端 3× 判假活 |

**OIDC 参数**：只用标准 discovery（`<issuer>/.well-known/openid-configuration`），JWKS 缓存 10min；服务调用走 `Authorization: Bearer <JWT>`，`aud` 必须等于 `clientId`，`exp/nbf` 容许 60s 偏移；不实现密码模式、不实现 token 透传到 Runner。

**多集群 kubeconfig**：Secret `argus-clusters` 的 key = `<cluster-name>.kubeconfig`，挂到 `CLUSTERS_DIR`。轮换流程：① 更新 Secret（`kubectl create secret ... --dry-run=client -o yaml | kubectl apply`）→ ② 控制面 fsnotify 感知后重建 `rest.Config` 与 client（**不重启**），并在 `clusters` 表写一条 `credential_rotated` 审计事件 → ③ 新 Task 用新凭据，已运行的 Job 不受影响。**不把 kubeconfig 写进数据库**（避免凭据入库），库里只存集群名、namespace、`kubeconfig_key` 与最近连通性检查结果。

### 11 数据生命周期

| 项 | 结论 |
|---|---|
| 迁移执行 / 规范 | Helm `pre-install,pre-upgrade` hook **Job**（镜像同 `argusd`，`command: ["/argusd","migrate","up"]`，`hook-delete-policy: before-hook-creation`），失败阻断升级；**不用 initContainer**（多副本会并发迁移）。规范：只前向，expand（加列/加表）与 contract（删列）分两个版本发布，保证"回滚镜像不需要回滚库" |
| 备份 | 平台不引入备份组件：PostgreSQL 走 DBA 既有备份/WAL；NFS 由存储侧快照。平台承诺的底线只有"库挂掉不影响已有产物可下载"（§12.5） |
| 服务清理 vs 文件保留 | **v2（D-70）：`cleanup.policy` 取消**——提交了 `deploy` 就必有 service-clean（finally 语义，成功/失败/取消/超时全路径执行）；产物保留由 `AG_RETENTION_DAYS` 决定，与 clean 结果无关（clean 失败只打 `cleanup_failed` 并进 §9.3 重放清单） |
| 保留清理 / 日志轮转 | controller 内 `retention-sweeper`（cron `0 3 * * *`）：只删**终态**且超期的 `<storage-root>/<task_id>/` 整目录（先写 `retention_purged` 事件，单次上限 200 个 Task，避免打满 NFS IO）；`<pod>.N.log` 分段随目录一起删，不做单文件删除（会破坏段落语义）。**清理不豁免任何 Task**（含 clean 失败的）；需长期保留时由 admin 记录并人工处理（P1 可加 `retention_exempt` 标记） |

### 12 可观测性

| 指标 | 类型 | labels | 说明 |
|---|---|---|---|
| `runner_duration_seconds` | histogram | `runner`,`status` | §12.6 必列（v1 的 `task_duration_seconds` 更名，runner 才是执行单位）；桶 `[10,60,300,900,3600,14400]` |
| `queue_depth` | gauge | `reason` | §12.6 必列；`reason` 取 `waiting_reason` 枚举 |
| `runner_failures_total` / `precheck_failures_total` | counter | `code` | §12.6 必列；`code` 只用附录 D（v2）枚举；后者是连续失败告警的输入 |
| `executor_errors_total` / `cleanup_failures_total` | counter | `op`（create/get/delete/log）、`kind`（service/job/pod） | §12.6 必列；执行与 clean 两处失败面 |
| `deploy_state_blocked_total` | counter | `reason` | §12.6 必列（v2 新增）：deploy-state `blocked` 分类计数 |
| `http_request_duration_seconds` | histogram | `route`,`method`,`code` | 补充：P95 300ms 的度量口径（`route` 用模板而非原始路径） |
| `sse_connections` / `sse_events_sent_total` / `collector_log_bytes_total` / `collector_gaps_total` / `retention_purged_total` | gauge / counter | `cluster`、`result` | 补充：SSE 健康度与采集缺口（§8.6 允许 gap，但必须可数）、clean 失败的可观测面 |
| `deploy_wait_seconds` | histogram | `cluster` | 补充：部署排队（K-6，可能数天；数据源 deploy-state 的 `waited_seconds`），用于看板与"排队过久"告警 |

**结构化日志字段**：`ts`、`level`、`msg`、`request_id`、`user`、`role`、`task_id`、`attempt_no`、`runner_seq`、`job_id`、`cluster`、`namespace`、`duration_ms`、`err.code`、`err.retryable`（§12.6）。平台自身日志不打印 secret 值或 `Authorization` 头；**runner / 部署 Pod 日志不脱敏**（K-5，平台不读 Secret，无法扫描）。

**告警草案**（走已有通道，不新建 Alertmanager）：

| 告警 | 表达式（示意） | for | 处置 |
|---|---|---|---|
| 清理失败 | `increase(cleanup_failures_total[15m]) > 3` | 5m | 查 service-clean 日志与 Job 残留；不影响 Task 结论；可用 `argus clean --task` 重放（§9.3） |
| Executor 不可达 | `rate(executor_errors_total{op="create"}[10m]) > 0.2` | 10m | 查 `clusters` 连通性检查、kubeconfig 是否过期 |
| Pre-Check 连续失败 | `increase(precheck_failures_total[30m]) >= 3` | 15m | 先看 Genet/服务侧：属 `service_unavailable`，不是平台故障 |
| 队列积压 / 采集缺口 | `queue_depth{reason="slot_waiting"} > 20`；`increase(collector_gaps_total[1h]) > 0` | 30m / 1h | 前者看平台并发上限与挂死 Task，后者看 collector 重启与 NFS 写入延迟 |
| Deploy 排队过久 | `max(deploy_wait_seconds) > 21600` | 15m | 看 deploy-state 的 phase 与水位（Genet 侧）；不是平台故障，但要让提交者知道"还要等" |

**原则：不引入新组件。** `/metrics` 由已有监控抓取，日志走 stdout 由集群已有采集器收；本 chart 不含 Prometheus、Loki、OTel Collector。

### 13 测试策略

| 层 | 工具 | 测什么 | 不测什么 |
|---|---|---|---|
| Go 单测 | `go test -race` | 状态机转移合法性（无 `failed→queued` 回路）、jsonl 输入结构校验（行号+字段名）、exports/consumes 组合期校验、`subPath` 路径安全、exports 词法校验、退出码→错误码归类 | 真实 K8s |
| Go 集成 | testcontainers-go + PostgreSQL 16 | 迁移（v2 表结构）、事务内状态写入、外接模式端点槽位互斥、游标分页、`events.seq` 单调、保留清理 | 前端 |
| 控制器 | envtest（真实 apiserver+etcd） | Job Spec 2.0 渲染字段（`args` 恒空、input/output 挂载、`argus.io/*` label）、对账（`unknown` 不得判失败，附录 B）、取消删 Job、Lease 选主 | 调度行为 |
| e2e | kind + in-cluster NFS server Pod（提供 RWX） | 提交 Task → service-deploy（genet-local：`queued→ready`、`DEPLOY_BLOCKED`、`DEPLOY_FAILED`）→ pre_check → tests → service-clean → 报告 全链路（串行管道）；`abort_task` 的 SKIPPED/CANCELED 区分；排队中取消（clean 兜底）；整 Task 重试新 attempt；`argus clean --task` 泄漏重放 | 性能 |
| 外部替身 | genet-local（假 Genet：install/get/uninstall/queue，httptest 注入 401/5xx/超窗）、fake OpenAI（`/v1/models`、`/v1/chat/completions`，可注入 429/5xx/慢响应） | pre_check 的"未就绪 vs 不可重试"判定 | 真实模型 |
| Runner 独立运行 | CI：在无平台的 runner 上 `docker run <image> <entry> --params-file … --url=…`（§5.10） | 产物自足（三选一产物至少其一，D-74）、`AG_OUTPUT_DIR` 可覆盖、元信息缺省为 `local` | 平台链路 |
| 契约 / OpenAPI | golden 文件 + `-update` flag（`result.json` 附录 A v2、`report.json` §8.4、Job Spec 附录 B v2、错误对象附录 D v2）；`oapi-codegen` + `openapi-typescript` 重生成 diff 为空 | spec ↔ 实现 ↔ 前端类型一致 | 语义正确性 |
| 前端 / e2e | Vitest + MSW；Playwright | Query key 失效粒度、SSE reducer（去重/乱序/回放）、错误码→呈现映射、指标分组；提交（部署 XOR 外接两形态）→ 详情（管道时间线）→ 日志跟随 → 取消/重试弹窗（"将重新部署"确认）、键盘 Tab 顺序、`prefers-reduced-motion` | 真后端、视觉回归（P1） |
| 压测 | k6 | `GET /tasks`、`GET /tasks/{id}` P95 < 300ms（§12.7），1k 并发的 SSE 长连接 | — |
| 故障注入 | chaos 脚本 | ①删 Job ②`kill -9` 控制面后重启对账 ③NFS 抖动（暂停 nfs-server 30s）④删部署 Pod | — |

故障注入的**验收口径**：删 Job → runner 最终进 `failed(EXECUTOR_UNAVAILABLE)` 或经复核 `failed(INTERNAL_ERROR)`（`detail.review=exhausted`），绝不"凭空 SUCCEEDED"；杀控制面 → 重启后 `running` 必须经 Executor 对账恢复，不得直接判失败（§12.5）；clean Job 未被调度时进入 §9.3 重放清单；NFS 抖动 → 日志出现 `--- [log gap:` 标记、`collector_gaps_total` 增长、Task 结果不受影响。

### 14 环境与灰度

| 环境 | 依赖 | 准入条件 | 回滚 |
|---|---|---|---|
| dev | kind 或单节点 k3s、genet-local（假 Genet）、容器 Postgres、`AG_AUTH_MODE=dev` | `make dev` 一条命令起全栈；P0 主流程能在 10 分钟内跑通 | — |
| test | 真 Genet sandbox、1 个 K8s 集群、小规格 NPU | §14.1 的 17 项 P0 验收全绿；e2e 与契约 golden 通过 | `helm rollback` |
| staging | 生产同构 NPU 集群、真实 Runner 镜像、真实 OIDC、只读账号 + 1 个可写 namespace | 单副本灰度 1 周，`cleanup_failures_total`/`precheck_failures_total` 无非预期增长；压测 P95 达标 | `helm rollback` + 镜像 digest 回退 |
| prod | 多副本、RWX NFS、OIDC 真实身份、多集群 kubeconfig | staging 连续 5 个工作日无 P1 缺陷；迁移 expand 已合入；备份演练通过 | 先回镜像（迁移向后兼容两版本），再按需 contract |

发布方式：`RollingUpdate{maxUnavailable:0, maxSurge:1}` + pre-upgrade 迁移 hook；灰度靠 `replicaCount` 与 Ingress 权重（可选），**不做按用户的 A/B**（内部工具用不上）。**红线**：`AG_AUTH_MODE=dev` 在 `staging`/`prod` values 中出现即启动失败；`image.tag` 与 `image.digest` 必须二选一，release 环境强制 digest。

### 15 P1 扩展点

| P1 能力 | P0 就该留的东西 | 前端位置 | P0 不做什么 |
|---|---|---|---|
| MCP Server / CLI（`argus`） | 只要 OpenAPI 完备 + Bearer JWT 可用即可，无需新接口；`GET /api/v1/meta` 提供 `version`/`api_version` 供客户端协商 | — | 不加"仅 Agent 可用"的端点（MCP/CLI 实现见 agent-integration 分册） |
| Task 对比 | `tasks.retry_of` 列 P0 建表时就加（nullable） | `/compare?tasks=a,b`：左选两个 Task，按"指标名 + labels + workload"对齐展示，**不判断可比性** | 不建指标索引表、不做图表 |
| 指标索引（跨 Task 查询） | P0 只存 `result.json`；P1 可写 **MongoDB**（`metrics` 集合：task/attempt/runner_seq/name/statistic/unit/value/labels），也可先用 PG `metrics_index` 表过渡 | 列表页加"指标筛选"列 | 不在 P0 建指标表（避免两处真相） |
| Matrix 展开 | 不预留服务端字段：展开由客户端生成 `runners[]` 后走同一 `POST /tasks`（API 不变，`dry-run` 已能校验） | 提交页 runner 列表的「展开矩阵」按钮（P1） | 不做服务端 `expand` 参数 |
| SSE 回放窗口 | `events.seq`（Task 内单调，bigserial per task）与 `events.at` 索引 P0 必需，`Last-Event-ID` 靠它 | — | 不做跨 Task 的事件订阅 |
| 跨 Task 续跑（仅外接模式） | `tasks.retry_of` 列 + 复用校验五元组（§7.7）在 P0 的表结构里已具备 | 详情页「续跑失败的 Task」按钮（P1） | P0 的 retry = 新 attempt 全量重跑（§7.8），无部分复用 |
| 看板 | `GET /queue-status` 与 `queue_depth{reason}` 指标 P0 就产出（详情页 `waiting_reason` 要用；P1 的 `/board` 复用同一数据源） | `/dashboard` 路由保留、导航按 flag 隐藏 | P0 不实现看板 UI |

**落地顺序**：先定 `api/openapi.yaml`（v2 契约：`/tasks`、`/runners` 路径 + `events` 信封 + `waiting_reason` 枚举）→ 生成前后端类型 → 前端用 MSW 对着 spec 做静态页（对齐截图）→ 后端按 `control-plane.md` / `execution.md` 补实现 → 最后接 Helm 与 CI。**spec 先行**是唯一不可跳的一步：前端一旦手写类型，生成客户端与双向校验就都失效了。
