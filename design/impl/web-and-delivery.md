# Web UI 与交付实现方案

> 本文是 `design/ARCHITECTURE.md`（下称 ARCHITECTURE）在**前端与交付**两个面上的落地细化：只回答"怎么做"，不重述裁决。与 ARCHITECTURE 冲突时以它为准；本文给出的端口、超时、阈值都是**可调整的默认值**，改的时候同步 `control-plane.md` / `execution.md`。既定选型不再论证。

---

## W 部分 · 前端

### 1 技术栈与目录结构

| 关注点 | 选型 | 结论与理由 |
|---|---|---|
| 框架 | React 19 + TypeScript 5.x（`strict`，`noUncheckedIndexedAccess`） | 无 SSR 需求，SPA 足够（§11.4） |
| 构建 | Vite 7，`build.target=chrome110` | 内网 Chrome，无需 polyfill |
| 路由 | TanStack Router（文件式路由 + `beforeLoad` 取会话） | 类型安全 search params：`?node=task:t-0007&attempt=2` 可分享、可前进后退 |
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
│   │   ├── runs.index.tsx   runs.$runId.tsx   tasks.tsx
│   │   ├── submit.tsx            # ?mode=task 复用同一页
│   │   └── testers.index.tsx   profiles.tsx   dashboard.tsx(P1)
│   ├── components/               # ui/（shadcn 生成）+ app/（AppShell、FlowGraph、LogViewer…）
│   ├── features/                 # 一能力一目录：组件 + hooks + 纯函数
│   │   └── run-list/ run-detail/ flow-graph/ submit/ logs/ testers/ report/ retry/ board/
│   ├── lib/                      # api(generated) / query / sse / copy.ts / format.ts
│   └── styles/                   # globals.css（Tailwind + 主题变量）/ fonts.css
└── scripts/subset-fonts.mjs      # 字体子集重建，见 §5
```

切分规则：**`routes/` 里不出现 `fetch` 与状态机判断**；一个能力一个 `features/<name>/`，跨能力复用的只有 `components/` 与 `lib/`。`components/ui/` 只放 shadcn 生成/定制组件，不放业务逻辑。新 Tester 上线不改前端（§11.4），因此 `features/` 里不允许出现按 Tester 名分支的代码。

### 2 页面 ↔ 原型映射

| 路由 | 页面 | 右栏 | 数据来源 | 原型截图 |
|---|---|---|---|---|
| `/dashboard` | 概览：队列 / 排队 Task / 概况 / 最近 Run；右上「新建 Run」 | 快捷操作 + 排队中的 Task + 这页怎么看 | `GET /board`（**P1**；P0 用 `/queue-status`，30s 轮询，无 SSE） | `dashboard.png` |
| `/runs` | Run 列表：搜索、状态分段、游标分页 | 终态语义图例 | `GET /runs?cursor=` | `runs.png` |
| `/tasks` | 跨 Run Task 列表：状态、Tester、耗时、错误码 | `waiting_reason` 说明 | `GET /tasks?cursor=` | 复用 `runs.png` 表格形态 |
| `/runs/$runId` | 详情：概要 kv、执行链路图、节点详情（Deploy / Task） | 事件时间线（首屏 `GET /runs/{id}/events/history`，之后由 SSE 增量） | `GET /runs/{id}` + `events/history` + SSE | `run-detail.png` |
| `/runs/$runId?tab=report` | 报告：指标表（按 labels 分组）、`report.md`、产物下载 | 报告分工说明 | `GET /tasks/{id}/attempts/{n}/result`、`GET /runs/{id}/report.md`、`GET /runs/{id}/artifacts`、`GET /runs/{id}/files?path=` | `run-detail.png` 的「结果指标」卡 |
| `/runs/$runId?node=task:x&tab=logs` | 日志：Attempt 级定位、跟随/分页 | 日志边界说明 | `GET /tasks/{id}/attempts/{n}/logs?stream=stdout&follow=1`（fetch 流） | `run-detail.png` |
| `/runs/$runId?node=deploy&pod=<pod>` | Deploy 日志：按 Pod 分页签 + 重启/gap 标记 | Pod 拓扑与区间；**排队中显示水位/原因原文/已等待时长**（无位次，D-46） | `GET /runs/{id}/deploy/logs`、`...?pod=&offset=`；排队期无 Pod 时显示"尚未产生 Pod" | `deploy-logs.png` |
| `/submit` | 提交 Run：一张"服务来源"卡两形态 + Run/env/secrets + Task 列表 | 参数说明（只读，来自 `tester.yaml.parameters`） | `GET /testers`、`POST /runs?dry_run=true`、`POST /runs` | `submit.png` / `submit-no-deploy.png` |
| `/submit?mode=task` | 独立 Task 提交（无 `deploy`，强制 `service`） | 同上 | `POST /tasks?dry_run=true`、`POST /tasks` | `submit-no-deploy.png` |
| `/testers` | Tester 目录：名称、版本、role、失败策略、独占、参数数；`?open=<name>` 打开详情抽屉（参数清单、env 声明、placeholder、镜像） | 注册规则 | `GET /testers`、`GET /testers/{name}` | `testers.png` / `tester-drawer.png` |
| `/profiles` | 执行环境：Profile / 集群 / 命名空间 / 挂载 | 边界说明 | `GET /profiles` | 无独立截图，见 `run-detail.png` 右栏 |

覆盖自检（§11.2 P0）：列表 ✅（`/runs`、`/tasks`）、详情 ✅、报告 ✅、日志 ✅、单输入框提交 ✅、Tester 目录 ✅、取消/重试 ✅（详情页头 + Task 卡）、环境信息 ✅、Deploy 与 Pod 日志 ✅。看板为 P1（§11.5），路由保留、导航项按 `flags.board` 隐藏。
### 2.1 Run 详情：终态默认「结果视图」，执行细节二级展开

人打开一个**已结束**的 Run，绝大多数只想拿到结果，不想看编排细节。所以：

| Run 状态 | `/runs/$runId` 默认展示 | 其它内容的去处 |
|---|---|---|
| 终态（`succeeded` / `failed` / `completed_with_errors` / `service_unavailable`） | **结果视图**：结论（成功/失败/为什么失败）→ counts → 失败项列表 → **产物与下载**（`report.json` / `report.md` / `metadata.json` / `events.jsonl` / 各 Attempt 日志 / Deploy 日志，以及一键 `GET /runs/{id}/bundle` 打包）→ 报告入口 | 执行链路 / Task 表 / Attempt 表 / 实时日志 → 「执行细节」二级视图（折叠不是隐藏：排障一键展开） |
| 运行中（非终态） | 执行视图：执行链路 + 实时日志 + 队列原因 + 已等待时长 | 报告等终态后才有的区块显示占位说明 |

右栏保持"事件流 + 边界说明"两类，不塞主内容；主内容默认不超过 4 个块。

### 2.2 导航可见性（按 `07 §2.3` 的角色矩阵，不在前端另造规则）

| 导航项 | viewer | operator | admin |
|---|---|---|---|
| 概览 / 运行列表 / 提交运行 / Tester 目录 / **集群与队列** | ✅ 可见（提交按钮对 viewer 置灰） | ✅ | ✅ |
| **管理台**（Profile / Cluster / Secret 引用 / 审计 / 清理产物） | —（不显示入口，直接访问显示无权限页） | — | ✅ |

> `集群与队列` 对 viewer **可见是设计要求的**：普通用户要能自己判断"卡在队列还是卡在平台槽位"（`ARCHITECTURE §10`）；它只读，不提供运维写操作。
**页面级操作只放页头**，"新建 Run"只在左导航与看板（原型约定）；详情页页头只放「重试失败 Task / 取消 Run / 返回列表」这类操作当前对象的按钮。

> **原型的定位（K-2）**：原型只作**风格参考**——布局骨架、信息层级、状态谱、配色倾向可以照搬，但实现用 Tailwind + shadcn 重建，**不要求像素一致**，也不复制原型的 CSS 类名与自研组件。两处按 ARCHITECTURE 补齐：① 原型刻意不展示节点 / NPU / GPU 用量、概览页不放主按钮，§11.5 要求看板含节点资源块与右上「新建 Run」入口；② 原型的执行链路是顺序链，§6.7 允许非独占 Task 并行，实现按"Pre-Check 门控 + 端点槽位"渲染，多个 Task 可同时处于运行态。

### 3 数据层

**缓存键**（前缀即失效粒度，禁止在键里放数组对象，避免序列化不稳定）：

```ts
export const qk = {
  runs:     ['runs', 'list', filter] as const,          // filter 扁平化为 string
  run:      (id: string) => ['runs', id] as const,
  tasks:    (runId: string) => ['runs', runId, 'tasks'] as const,
  task:     (runId: string, taskId: string) => [...qk.tasks(runId), taskId] as const,
  attempts: (runId: string, taskId: string) => [...qk.task(runId, taskId), 'attempts'] as const,
  result:   (runId: string, taskId: string, n: number) => [...qk.task(runId, taskId), 'result', n] as const,
  deploy:   (runId: string) => ['runs', runId, 'deploy'] as const,
  logs:     (path: string, offset: number) => ['logs', path, offset] as const,
  // 低频：['board'] / ['testers'] / ['testers', name] / ['profiles'] / ['secrets'] / ['session']
};
```

**SSE 与 Query 的合并**（`lib/sse/useRunEvents.ts`；**日志内容不走 SSE**，由日志面板自己读流，见 §4.3）：

| 事件 `type`（`control-plane.md` C-9.1） | 客户端动作 |
|---|---|
| `run.submitted` | 失效 `['runs','list']`（新建跳转走详情，不依赖该事件） |
| `run.state_changed` / `task.state_changed` | `setQueryData` 局部 patch 状态与 `waiting_reason`；**同时** `invalidateQueries(['runs', id, 'tasks'])`（状态变化常带 Attempt 变化） |
| `attempt.started` / `attempt.finished` | `invalidateQueries(attempts)` + `invalidateQueries(result)`；不 patch，留给服务端算 |
| `deploy.state_changed` | 失效 `qk.deploy(id)`（Pod 数与拓扑可能变）；日志面板另行 refresh 页签 |
| `cleanup.started` / `run.finished` / `report.written` | `run.finished` 时 `invalidateQueries(['runs', id])` + `['runs','list']` 并关闭连接；`report.written` 失效 `report` 与 `artifacts` |

规则（按序执行，缺一不可）：
1. **seq 去重**：信封 `{seq, type, run_id, task_id?, attempt?, at, payload}`，`seq` 在 Run 内单调递增（服务端 `events.seq` 生成，见 `control-plane.md` C-9.2）。客户端 `lastSeq` 存在 `sessionStorage["sse:"+runId]`，收到 `seq <= lastSeq` 直接丢弃。
2. **Last-Event-ID 重连**：请求头带 `Last-Event-ID: <lastSeq>`，服务端从 `seq+1` 回放（窗口 24h、单次上限 1000 条；C-9.2），超出窗口/条数时服务端发 `resync_required` → 客户端整页失效重建。
3. **不用原生 `EventSource`**：要带 `Last-Event-ID`、要在 401 时跳 OIDC 而不是静默重连、要在 `document.visibilityState==='hidden'` 时暂停。用 `fetch` + `ReadableStream` + `AbortController` 自写 ~120 行（`text/event-stream` 按 `\n\n` 切帧）。
4. **心跳与退避**：服务端 15s 一条 `: ping`；客户端 60s 收不到任何帧即重连，重连退避 1s → 2s → 4s → 8s → 上限 30s；连续 3 次失败降级为 5s 轮询 `GET /runs/{id}`，恢复后回切并停轮询。
5. **单连接**：连接挂在 `/runs/$runId` 路由的 `beforeLoad` context 上，详情页内切节点/切 Attempt **不重建**连接。

```ts
// lib/sse/useRunEvents.ts —— 只管连接、去重、合并；事件集合与信封以 C-9 为准
const onFrame = (e: Envelope) => {
  if (e.seq <= lastSeq) return;                       // ① 回放与重连都可能重复
  lastSeq = e.seq; persist(runId, lastSeq);
  switch (e.type) {
    case 'task.state_changed': case 'run.state_changed':
      patchState(e); qc.invalidateQueries({ queryKey: qk.tasks(runId) }); break;
    case 'attempt.started': case 'attempt.finished':
      qc.invalidateQueries({ queryKey: qk.attempts(runId, e.task_id!) }); break;
    case 'deploy.state_changed': qc.invalidateQueries({ queryKey: qk.deploy(runId) }); break;
    case 'run.finished': qc.invalidateQueries({ queryKey: ['runs'] }); ctrl.abort(); break;
  }
};
```

**错误码 → 全局提示**（附录 D 全集的 UX 归类，`lib/copy.ts` 一张表驱动）：

| 呈现 | 错误码 | 行为 |
|---|---|---|
| 顶部 banner（不自动消失） | `PRECHECK_FAILED`、`ENDPOINT_UNREACHABLE`、`EXECUTOR_UNAVAILABLE`、`INTERNAL_ERROR` | 带排查方向（§7.5；Run 级 `SERVICE_UNAVAILABLE` 用同一 banner 形态），提供"复制诊断信息" |
| 表单字段级 | `VALIDATION_ERROR`、`RUN_TOO_LARGE`、`TESTER_NOT_FOUND`、`EXECUTION_PROFILE_NOT_FOUND`、`CONFLICT` | 按 `error.detail.path`（`tests[2].args` / `deploy.parameters` / `env.KEY`）定位到控件；YAML 侧再带 `line/col` |
| toast（一次）/ 静默 | `CANCELED`、`NOT_FOUND`（对象已变，刷新列表）；`CLEANUP_FAILED` 只在事件栏与看板计数出现，不影响 Task 结果（§6.6） | 文案指明对象已失效，不铺满屏幕 |
| 内联在对象卡上（不弹窗） | `TASK_TIMEOUT`、`TESTER_NONZERO_EXIT`、`RESULT_MISSING`、`RESULT_INVALID`、`ARTIFACT_QUOTA_EXCEEDED`、`IMAGE_PULL_FAILED`、`MOUNT_UNAVAILABLE`、`PRECHECK_FAILED`、`ENDPOINT_UNREACHABLE` | 显示码 + 中文说明 + `retryable` 徽标；`retryable=true` 才出「重试」 |
| 跳转登录 | `UNAUTHENTICATED` | `location.assign('/auth/login?return=' + encodeURIComponent(current))` |
| 只读降级 | `FORBIDDEN` | 隐藏写按钮 + 页头提示"当前角色 viewer"；**以服务端返回为准**，不在前端推断角色 |

**分页**：一律游标，不用 offset。`GET /runs?limit=50&cursor=<opaque>` → `{items, next_cursor}`；游标由服务端编码排序键 `(submitted_at DESC, id DESC)`，客户端只回传不解析。前端 `useInfiniteQuery`，`limit` 默认 50、上限 200。日志是唯一例外（按字节 `offset`，见 §4.3）。

### 4 关键交互实现

#### 4.1 提交页

| 控件 | 实现 | 明确不做 |
|---|---|---|
| Task 参数 | 一个 `textarea`（等宽 12.5px / 1.7 行高 / `resize:none` / 随内容 104–360px 增高），`placeholder` 取 `tester.placeholder`；下方静态提示"公共参数由平台注入，写在这里会被拒绝" | 不渲染表单、不做类型/必填/枚举校验、不预填；前端**不检测** `--url` 子串后拦截（那会造成"只有 UI 有"的校验，API First） |
| 照上次配置 | `?from=<run_id>` 时用 `GET /runs/{id}/manifest` 回填最近一次 Run 的 tester 组合与 `args` **原文**（§11.5 唯一允许的预填：填的是上一次的输入） | 不推导、不合并、不"智能补全" |
| Run `env` | 行编辑器（key / value / 删除 / + 一行），明文可回显可编辑 | 不脱敏、不折叠（§12.4 裁决） |
| `secrets` | key 输入 + 下拉，选项来自 `GET /secrets`，只显示 **名称与说明**；无可选项时给"去登记"文案（仅 admin 可见） | 不回显值、不提供"新建 Secret"内联表单（登记是 admin 独立页） |
| 服务来源（一张卡两形态） | segmented：`部署` → YAML 编辑器（`deploy.provider` + `parameters`，原样转交 Genet）；`不部署` → `url` / `model_name` / `model_path` 三字段 | 两形态互斥，切换时**保留**各自草稿不清空；不提供第三种"既部署又手填" |
| `precheck` | `auto` / `off` 分段 + 显式对象（折叠区） | 不替用户判定"该不该开" |
| dry-run | 提交按钮旁「校验」；`POST /runs?dry_run=true` 与 `POST /runs` 用**同一份 Manifest**，返回 `{valid, errors[]}` | 不在输入时实时校验（等于渲染校验），只在点击时调 |

提交前流程：点「提交」→ 先 dry-run → 有 `VALIDATION_ERROR` 就地标红并滚到第一个错误（不弹 toast 堆叠）→ 无错再 `POST /runs`（**不做幂等**：每次提交都创建新 Run，重复提交就是多个 Run，D-37）→ 成功跳 `/runs/$id`，toast 显示 Run ID。
错误定位规则：`detail.path` 与表单控件一一对应（`service.url`、`env.ENDPOINT_API_KEY`、`tests[0].args`、`deploy.parameters`），YAML 解析失败额外给 `line/col`，编辑器 `dispatch(setDiagnostics)`。

#### 4.2 详情页

- **链路图**：HTML/CSS（`.flow` / `.flow-node` / `.flow-arrow`），横向按 `seq` 排列，不用 SVG 也不做拖拽（§11.3）。节点 = `Deploy?` + `tests[]`；**允许并行**（§6.7）：Pre-Check 通过后多个 Task 可同时 `RUNNING`，节点并行显示各自状态，不假设只有一条运行中的链。节点 tone 走 `nodeTone()` 同款映射；超出宽度横向滚动，名字 `ellipsis` + `title`。
- **Attempt 切换**：`?attempt=n`，默认最后一个；`result` / `logs` / `artifacts` 三处共享该参数，历史 Attempt 标"只读"。Attempts 表永远全量展示（重试是追加不是覆盖，§7.2）。
- **参数快照**：`task.args` 原文等宽只读 + 复制；并列展示 `tester` / `version` / `image_ref` 冻结值（§5.7）。**不提供编辑**，改参数 = 新 Run。
- **结果指标表**：列 `指标 / 值 / 单位 / 统计口径 / labels / sample_size`；按 `labels` 排序后的 JSON 串分组，组间用 `tbody` 分组行分隔。**不聚合、不计算、不判定达标**（§8.4）。空态按状态分文案：`SKIPPED`"未执行"、`CANCELED`"被终止"、`FAILED`"执行失败"，绝不显示"0"。
- **`waiting_reason`**：`QUEUED` / `PENDING` 时卡片头显示中文枚举——`precheck_gate` 等待探活、`slot_waiting` 平台槽位、`global_limit` 平台并发、`queue_full` 队列已满、`insufficient_resources` 资源不足、`gang_pending` gang 未满足；带"看队列（P1）"链接，不提供插队（§6.9）。

```tsx
// features/flow-graph/FlowGraph.tsx —— 纯展示：节点是按钮，不是拖拽画布
<ol className="flow" aria-label="执行链路">
  {deploy && <NodeBtn kind="deploy" tone={tone(deploy.status)} label="Deploy"
                      meta={`${deploy.pods.length} pod · ${deploy.duration}`} />}
  {tasks.map((t) => (
    <NodeBtn key={t.id} kind="task" tone={nodeTone(t.status)} label={t.name}
             meta={t.waiting_reason ? WAIT_REASON_CN[t.waiting_reason] : t.duration}
             active={node === `task:${t.id}`} />
  ))}
</ol>
```

#### 4.3 日志查看

| 项 | 做法 |
|---|---|
| 定位 | 以 `task_id + attempt` 为唯一定位（Cache key `logs(taskId, n, offset)`），切换节点/Attempt 时 `AbortController.abort()` 上一路请求；不走"客户端拼文件 path"的形态 |
| 跟随 / 分页 | 首屏 `?tail=2000&follow=1`（`TESTPLANE_LOG_TAIL_LINES`）；向上翻页与终态读取用 `?offset=&limit=`（单次 ≤ 64KiB 行数，C-12）；`follow` 走 `text/plain` + `ReadableStream` 逐块追加，`run.finished` 或 Tab 隐藏时停跟 |
| 虚拟滚动 | 10k 行以上启用 `useVirtualizer`（固定行高 20px）；20k 行以上只保留"尾部跟随"，向上滚动触发向前分段加载 |
| 高亮 | 行首模式匹配：`--- [container restarted #N` → 琥珀 + 左侧 3px 竖条；`--- [log gap:` → 红虚线框；`ERROR`/`Traceback`/`OOMKilled` → 红；其余按原型深色终端风（底 `#0b1220`） |
| Deploy 日志 | 页签来自 `deploy/pods.json`（name / role / node / restarts / 日志区间），一个 Pod 一个页签，**不合并**；页签角标显示重启次数与 `container restarted` 段数；未落盘显示"尚未产生日志" |
| 宣告 | 运行中页头 `.live-dot` + "追加中"；每份日志显示覆盖区间（`08:12:03 – 09:40:11`），与"测试期间仍在追加"文案（§8.1） |

#### 4.4 取消 / 重试

| 动作 | 前置 | 弹窗（shadcn `Dialog`，焦点陷阱 + Esc） |
|---|---|---|
| 重试 Task（Run 级 / 单个） | 终态且存在 `FAILED`/`CANCELED`；单个重试入口在 Task 卡上 | `GET /runs/{id}/retry-plan` → 两栏：「将复用」（`SUCCEEDED`，灰）与「将重跑」（新 Attempt `#n+1`，高亮）；底部"新建 Attempt，历史 Attempt 保留" |
| 取消 Run | 非终态（含 Deploy 排队中） | 两栏：「将标记 SKIPPED（未开始）」与「将终止并标记 CANCELED（正在跑）」；**Deploy 排队/部署中**额外说明"会先向 Genet 取消排队/卸载，可能不是即时生效"；附"清理由 `cleanup.policy` 决定" |
| 取消单个 Task | 非终态 | 若该 Task `on_failure=abort_run`，弹窗**额外警告**"其余未完成 Task 会被一并终止"（§7.6） |

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
# 1. 采集真实 CJK：前端固定文案 + 后端错误/状态文案 + 已注册 Tester 的 yaml
node scripts/collect-glyphs.mjs \
  --from 'src/**/*.{ts,tsx}' --from 'design/prototype/index.html' \
  --from 'testers/*/tester.yaml' --from 'server/internal/copy/*.go' \
  --out .cache/glyphs.txt
pyftsubset NotoSansSC[wght].ttf --text-file=.cache/glyphs.txt \
  --flavor=woff2 --layout-features='*' \
  --output-file=src/styles/fonts/noto-sans-sc-var.woff2   # fonttools，CI 镜像内已装
# 3. 覆盖率校验：构建期对字符串字面量做比对，缺字只 WARN 不改行为
```

规则：`--font-sans` 以自托管的 Albert Sans / Noto Sans SC 打头，尾部留系统兜底——**不以 `-apple-system` 打头**（Windows 上不存在，Latin 会掉到 Segoe UI 与中文对不齐）；拉丁用 Albert Sans 可变字重（31KB，`preload`），中文子集目标 `≥99.5%` 覆盖固定文案，**数据驱动的中文（Tester 名、错误信息、用户填的 Run 名）允许回落**到系统栈——接受它而不打包 8MB 全量字体。CI 在文案 PR 上跑覆盖率检查；新增 Tester 或新错误码文案时必须重建（否则该页出现两种中文字形）。

### 6 性能与可访问性

| 项 | 默认值 / 做法 |
|---|---|
| 首屏 budget / 拆包 | entry JS ≤ 250KB gz（Tailwind + shadcn 基元）、CSS ≤ 60KB gz、字体 178KB（latin preload，中文 `swap`）、LCP < 2.5s；每个 route `lazyRouteComponent`，CodeMirror / `react-virtual` / `report.md` 渲染器各自独立 chunk，不进首屏 |
| 表格虚拟化阈值 | Run/Task 列表 > 100 行、日志 > 10k 行；低于阈值渲染完整 DOM（否则滚动条跳动） |
| SSE 降级 | 见 §3 第 4 条；降级时页头显示"实时更新已降级为 5s 轮询"，不静默 |
| 键盘可达 | 全局 skip link（跳到主内容 / 跳到导航）；focus 环统一 `--ring`（3px，不用 `outline:none`）；可点击表格行用 `tabIndex=0` + `Enter/Space` 且整行有 `<a>` 语义兜底；Dialog/Sheet 用 shadcn 自带的焦点陷阱 + `Esc` + 关闭后焦点归位 |
| 状态表达 | 状态不只靠颜色：badge 一律"文字 + 色"，链路节点带 `aria-label`；弱化文字用 `--muted-foreground`（对应原型 `#667085`，4.9:1），中文正文不低于 13.5px；只中文不引 i18n 库，文案集中在 `lib/copy.ts`（同时作为子集采集源） |
| 动效 | 用 Tailwind `motion-safe:` 变体；全局 `@media (prefers-reduced-motion: reduce)` 关掉位移/缩放/animation |

### 7 构建与集成

```text
CI: pnpm --dir web build → web/dist
  → cp -r web/dist server/internal/webui/dist     （go:embed 不能跨模块目录，必须复制）
  → cd server && go build ./cmd/testplaned        （//go:embed all:dist；Go 模块在 server/）
```

| 项 | 结论 |
|---|---|
| 模块布局 | **Go 模块在 `server/`**（`server/go.mod`），`web/`、`testers/`、`deploy/` 与它平级；`go:embed` 只能嵌包内相对路径，因此前端产物复制进 `server/internal/webui/dist`，`testers/` 由镜像 `COPY` 到 `/app/testers`（E-10） |
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
      -o /out/testplaned ./cmd/testplaned

FROM gcr.io/distroless/static-debian12:nonroot   # 无 shell、无包管理器
COPY --from=build /out/testplaned /testplaned
COPY testers/ /app/testers/                       # registry 扫描源（E-10），非 embed
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/testplaned"]
```

| 项 | 结论 |
|---|---|
| 角色 | 同一镜像 `--role=all`（缺省）/ `api` / `controller` / `collector`；拆角色只是改 arg，小规模默认 `all` |
| 迁移 | **不进 entrypoint**（多副本会并发迁移），由 Helm pre-upgrade hook Job 执行，见 §11 |
| 版本号 | tag `vMAJOR.MINOR.PATCH`；镜像必打 `:git-<short-sha>`，仅在 git tag 构建时追加 `:vX.Y.Z`；**不打 `:latest`**（避免隐式漂移）；部署用 digest，记录到 `deploy/versions.lock` |
| 合规 | 镜像非 root、无 shell、无 build 工具；`docker scout`/`trivy` 扫 HIGH 以上阻断 |

CI 阶段（单流水线，按 `paths` 过滤触发）：

| 阶段 | 命令 | 门禁 |
|---|---|---|
| lint / openapi | `golangci-lint run`、`eslint --max-warnings=0`、`prettier --check`；`spectral lint api/openapi.yaml` + 重生成后 `git diff --exit-code` | 阻断（spec 是实现唯一源） |
| test / schema | `go test -race ./...`、`pnpm vitest run`；`tester.yaml` 校验（复用 registry 校验器）+ `result.json` 附录 A golden 校验 | 覆盖率 gate（views/05 §7.2）：`domain ≥85%`、`orchestrator ≥70%`、`executor/k8s ≥70%`、总体 ≥60% |
| build / image | `go build` + `pnpm build` + 体积 budget 检查；buildx 多架构（amd64/arm64）→ push → 打 digest | budget 超 10% 阻断；image 仅 `main` 与 tag 推送 |
| helm | `helm lint` + `helm template` + `ct lint` + `helm package` | 阻断 |

Tester 镜像流水线（独立 workflow）：`testers/*/` 变化触发矩阵，**上下文 = Tester 根目录**、`-f build/Dockerfile`（§5.5），tag 取该目录 `tester.yaml.version`；构建后冒烟：镜像可启动且 `/tester/run.sh` 存在且可执行（`--entrypoint /bin/sh -c 'test -x /tester/run.sh'`），**不在 CI 里跑真实测试**（那是 Tester 自己的 `tests/`）。合并到 `main` 才推送；PR 只构建不推送。

> 控制面镜像的构建上下文是**仓库根**（要用 `server/`、`web/`、`testers/` 三个目录）；`.dockerignore` 排除 `design/`、`web/node_modules`、`.git`，避免几 MB 原型与依赖进构建层。

### 9 Helm chart

`deploy/helm/testplane/`，values 骨架：

```yaml
image: { repository: registry.internal/testplane/testplaned, tag: "", digest: "", pullPolicy: IfNotPresent }
replicaCount: 1                    # P0 单副本；P1 起可 2（选主后 controller/collector 单活）
role: all                          # all | api | controller | collector
resources: { requests: {cpu: 500m, memory: 512Mi}, limits: {cpu: "2", memory: 2Gi} }
storage: { existingClaim: testplane-storage-root, mountPath: /storage-root }
database: { host: "", port: 5432, name: testplane, sslMode: require, existingSecret: testplane-db }
oidc: { issuer: "", clientId: "", existingSecret: testplane-oidc, roleClaim: groups, viewerGroups: [], operatorGroups: [], adminGroups: [] }
genet: { baseURL: "", existingSecret: testplane-genet }
bootstrap:                         # 结论：Profile / 集群由 DB 维护（§12.5 由 admin 管），values 只做首次 seed
  executionProfiles: []            # 例：[{name: k8s-npu, cluster: cluster-01, namespace: testplane, mounts: [...]}]
  clusters: []                     # 集群名 → Secret 中的 kubeconfig key
sse: { heartbeatSeconds: 15, maxConnectionsPerRun: 5, replayWindowHours: 24 }   # maxConnectionsPerRun 为 P1 限流位，P0 不实现
retention: { days: 30, sweepCron: "0 3 * * *" }
networkPolicy: { enabled: true, egressAllow: ["10.0.0.0/8", "registry.internal/32", "{{ nfsServer }}/32"] }
```

| 对象 | 关键点 |
|---|---|
| Deployment | `strategy: RollingUpdate{maxUnavailable:0,maxSurge:1}`；`securityContext`: `runAsNonRoot`/`readOnlyRootFilesystem`/`allowPrivilegeEscalation:false`/`capabilities.drop:[ALL]`/`seccompProfile: RuntimeDefault`；`emptyDir` 只挂 `/tmp`；`startupProbe: /healthz`（30×2s，覆盖迁移等待）→ `readinessProbe: /readyz`（含 DB ping + 迁移版本匹配）→ `livenessProbe: /healthz`（**不含 DB**：DB 抖动不该重启进程） |
| Service | ClusterIP 8080；Ingress 注解：`proxy-buffering: "off"`、`proxy-read-timeout: "3600"`（SSE）、`proxy-body-size: 8m` |
| ConfigMap / Secret | ConfigMap 装非敏感配置（端口、并发上限、超时、保留期），`checksum/config` 注解触发滚动；Secret 用 `existingSecret` 引用（`testplane-db` / `-oidc` / `-genet` / `-clusters`），chart **不生成任何明文** |
| ServiceAccount + RBAC | 见下表；`automountServiceAccountToken: true` 仅控制面需要 |
| PVC | `ReadWriteMany`（NFS CSI 或 `nfs:` volume），只挂控制面到 `/storage-root`；Tester Pod 由 Executor 以 **同 namespace 同 PVC 的 subPath** 挂 `/workspace/output` |
| NetworkPolicy | `default-deny` ingress+egress；放行 ingress：ingress-controller → 8080、Prometheus → 8080；放行 egress：kube-dns(53)、Postgres、Genet、镜像仓库、`egressAllow` 目标网段、NFS server（2049） |
| PSA / PDB | namespace 打 `pod-security.kubernetes.io/enforce: restricted`（控制面与 Tester Pod 都要在该档下可跑）；`PDB.minAvailable: 1` 保证滚动期间可用 |
| 选主 | controller 用 Lease `testplane-controller`；collector 用 `testplane-collector-leader` + **每 Job 一个** `tp-log-<hash>`（E-7.3），collector 因此可多副本分片 |

RBAC（最小集，`Role` 限定在 `execution.namespace`）：

| 资源 | 动词 | 用途 |
|---|---|---|
| `batch/jobs` | get, list, watch, create, delete | 创建/跟踪/取消 Task Job |
| `pods` / `pods/log` | get, list, watch, delete / get | 状态对账、按 selector 发现 Deploy Pod、取消时兜底删 Pod；日志采集（`follow`） |
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
| `TESTPLANE_LISTEN` / `--listen` / `--role` | `:8080` / `all` | 监听地址；角色 `all`/`api`/`controller`/`collector` |
| `TESTPLANE_DB_DSN` | 必填 | Secret 注入；含 `sslmode=require` |
| `TESTPLANE_DB_MAX_OPEN_CONNS` | `20` | 状态查询 P95 300ms 的关键参数 |
| `TESTPLANE_STORAGE_ROOT` | `/storage-root` | NFS 挂载点，写 `deploy/logs` 与报告 |
| `TESTPLANE_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` | 必填 | 会话 cookie 流程；`_CLIENT_SECRET` 来自 Secret |
| `TESTPLANE_OIDC_SCOPES` / `TESTPLANE_OIDC_ROLE_CLAIM` | `openid profile email groups` / `groups` | 需要 `groups` 才能映射 `viewer/operator/admin` 三组 |
| `TESTPLANE_SESSION_COOKIE` / `_TTL` / `_KEY` | `tp_session` / `8h` / 必填 32B | `HttpOnly; Secure; SameSite=Lax; Path=/`；签名密钥轮换时新老共存 1 个 TTL |
| `TESTPLANE_AUTH_MODE` | `oidc` | `dev` 仅在 dev 环境允许，**其他环境启动即拒绝** |
| `TESTPLANE_GENET_BASE_URL` / `_TOKEN_FILE` / `_TIMEOUT` | 必填 / `30s` | 令牌用**文件**而非 env，支持轮换后热加载（默认每 5min 重读）；`_TIMEOUT` 是单次请求超时，不是部署总窗口 |
| `TESTPLANE_DEPLOY_TIMEOUT` | `72h` | Deploy 排队 + 部署总窗口（K-6）；Profile 可覆盖；排队计入窗口，取消不受限 |
| `TESTPLANE_CLUSTERS_DIR` / `_DEFAULT_CLUSTER` / `_EXECUTOR_NAMESPACE` | `/etc/testplane/clusters` / 空 / `testplane` | 一集群一 `.kubeconfig`；未指定时用默认集群（生产建议显式）；Job 与 PVC 所在 namespace |
| `TESTPLANE_TASK_TIMEOUT` / `_MAX` | `4h` / `24h` | Manifest 可覆盖，超上限拒绝 |
| `TESTPLANE_CONCURRENCY_MAX` / `RUN_MAX_TASKS` | `20` / `200` | 全局并发上限（§12.7）；超出 `RUN_TOO_LARGE` |
| `TESTPLANE_ARTIFACT_MAX_BYTES` / `LOG_SEGMENT_MAX_BYTES` / `RETENTION_DAYS` | `10GiB` / `200MiB` / `30` | 单 Attempt 产物上限（`ARTIFACT_QUOTA_EXCEEDED`）；Deploy 日志分段；产物保留期（**与 `cleanup.policy` 解耦**，`never` 不豁免） |
| `TESTPLANE_RESULT_MAX_BYTES` | `100MiB` | `result.json` 技术上限（仅防 OOM；K-10 不设业务限制） |
| `TESTPLANE_SSE_HEARTBEAT` | `15s` | 心跳间隔；客户端 3× 判假活 |

**OIDC 参数**：只用标准 discovery（`<issuer>/.well-known/openid-configuration`），JWKS 缓存 10min；服务调用走 `Authorization: Bearer <JWT>`，`aud` 必须等于 `clientId`，`exp/nbf` 容许 60s 偏移；不实现密码模式、不实现 token 透传到 Tester。

**多集群 kubeconfig**：Secret `testplane-clusters` 的 key = `<cluster-name>.kubeconfig`，挂到 `CLUSTERS_DIR`。轮换流程：① 更新 Secret（`kubectl create secret ... --dry-run=client -o yaml | kubectl apply`）→ ② 控制面 fsnotify 感知后重建 `rest.Config` 与 client（**不重启**），并在 `clusters` 表写一条 `credential_rotated` 审计事件 → ③ 新 Task 用新凭据，已运行的 Job 不受影响。**不把 kubeconfig 写进数据库**（避免凭据入库），库里只存集群名、namespace、`kubeconfig_key` 与最近连通性检查结果。

### 11 数据生命周期

| 项 | 结论 |
|---|---|
| 迁移执行 / 规范 | Helm `pre-install,pre-upgrade` hook **Job**（镜像同 `testplaned`，`command: ["/testplaned","migrate","up"]`，`hook-delete-policy: before-hook-creation`），失败阻断升级；**不用 initContainer**（多副本会并发迁移）。规范：只前向，expand（加列/加表）与 contract（删列）分两个版本发布，保证"回滚镜像不需要回滚库" |
| 备份 | 平台不引入备份组件：PostgreSQL 走 DBA 既有备份/WAL；NFS 由存储侧快照。平台承诺的底线只有"库挂掉不影响已有产物可下载"（§12.5） |
| 服务清理 vs 文件保留 | `cleanup.policy` 只管**被测服务是否卸载**，不管文件删除；产物保留由 `TESTPLANE_RETENTION_DAYS` 决定 |
| 保留清理 / 日志轮转 | controller 内 `retention-sweeper`（cron `0 3 * * *`）：只删**终态**且超期的 `<storage-root>/<run_id>/` 整目录（先写 `retention_purged` 事件，单次上限 200 个 Run，避免打满 NFS IO）；`<pod>.N.log` 分段随目录一起删，不做单文件删除（会破坏段落语义）。`cleanup.policy=never` **只豁免卸载，不豁免保留期**；需长期保留时由 admin 记录并人工处理（P1 可加 `retention_exempt` 标记） |

### 12 可观测性

| 指标 | 类型 | labels | 说明 |
|---|---|---|---|
| `task_duration_seconds` | histogram | `tester`,`status` | §12.6 必列；桶 `[10,60,300,900,3600,14400]` |
| `queue_depth` | gauge | `reason` | §12.6 必列；`reason` 取 `waiting_reason` 枚举 |
| `task_failures_total` / `precheck_failures_total` | counter | `code` | §12.6 必列；`code` 只用附录 D 枚举；后者是连续失败告警的输入 |
| `executor_errors_total` / `cleanup_failures_total` | counter | `op`（create/get/delete/log）、`kind`（service/job/pod） | §12.6 必列；执行与清理两处失败面 |
| `http_request_duration_seconds` | histogram | `route`,`method`,`code` | 补充：P95 300ms 的度量口径（`route` 用模板而非原始路径） |
| `sse_connections` / `sse_events_sent_total` / `collector_log_bytes_total` / `collector_gaps_total` / `retention_purged_total` | gauge / counter | `cluster`、`result` | 补充：SSE 健康度与采集缺口（§8.6 允许 gap，但必须可数）、清理失败的可观测面 |
| `deploy_queue_depth` / `deploy_wait_seconds` | gauge / histogram | `cluster` | 补充：Deploy 排队（K-6，可能数天），用于看板与"排队过久"告警 |

**结构化日志字段**：`ts`、`level`、`msg`、`request_id`、`user`、`role`、`run_id`、`task_id`、`attempt_id`、`job_id`、`cluster`、`namespace`、`duration_ms`、`err.code`、`err.retryable`。平台自身日志不打印 secret 值或 `Authorization` 头；**Tester/Deploy 日志不脱敏**（K-5，平台不读 Secret，无法扫描）。

**告警草案**（走已有通道，不新建 Alertmanager）：

| 告警 | 表达式（示意） | for | 处置 |
|---|---|---|---|
| 清理失败 | `increase(cleanup_failures_total[15m]) > 3` | 5m | 查 K8s 权限与 Job 是否残留；不影响 Task 结论 |
| Executor 不可达 | `rate(executor_errors_total{op="create"}[10m]) > 0.2` | 10m | 查 `clusters` 连通性检查、kubeconfig 是否过期 |
| Pre-Check 连续失败 | `increase(precheck_failures_total[30m]) >= 3` | 15m | 先看 Genet/服务侧：属 `SERVICE_UNAVAILABLE`，不是平台故障 |
| 队列积压 / 采集缺口 | `queue_depth{reason="slot_waiting"} > 20`；`increase(collector_gaps_total[1h]) > 0` | 30m / 1h | 前者看平台并发上限与挂死 Task，后者看 collector 重启与 NFS 写入延迟 |
| Deploy 排队过久 | `max(deploy_wait_seconds) > 21600` | 15m | 看 Genet 队列与配额；不是平台故障，但要让提交者知道"还要等" |

**原则：不引入新组件。** `/metrics` 由已有监控抓取，日志走 stdout 由集群已有采集器收；本 chart 不含 Prometheus、Loki、OTel Collector。

### 13 测试策略

| 层 | 工具 | 测什么 | 不测什么 |
|---|---|---|---|
| Go 单测 | `go test -race` | 状态机转移合法性、`args` 词法切分（引号/转义/元字符字面量）、公共参数冲突判定、`subPath` 路径安全、`retry` 合并取严、退出码→错误码归类 | 真实 K8s |
| Go 集成 | testcontainers-go + PostgreSQL 16 | 迁移、事务内状态写入、并发槽位互斥、游标分页、`events.seq` 单调、保留清理 | 前端 |
| 控制器 | envtest（真实 apiserver+etcd） | Job 渲染字段、对账（`unknown` 不得判失败，附录 B）、取消删 Job、Lease 选主 | 调度行为 |
| e2e | kind + in-cluster NFS server Pod（提供 RWX） | 提交 Run → Deploy(mock Genet：`queued→ready`、`blocked`、`500`) → PreCheck → Task → 报告 → 清理 全链路；`abort_run` 的 SKIPPED/CANCELED 区分；排队中取消 | 性能 |
| 外部替身 | mock Genet（httptest：accepted/ready/blocked/timeout/500）、fake OpenAI（`/v1/models`、`/v1/chat/completions`，可注入 429/5xx/慢响应） | Pre-Check 的"未就绪 vs 不可重试"判定 | 真实模型 |
| Tester 独立运行 | CI：在无平台的 runner 上 `docker run <image> --url=… --model_name=…`（§5.10） | 产物自足、`OUTPUT_DIR` 可覆盖、元信息缺省为 `local` | 平台链路 |
| 契约 / OpenAPI | golden 文件 + `-update` flag（`result.json` 附录 A、`report.json` §8.4、Job Spec 附录 B、错误对象附录 D）；`oapi-codegen` + `openapi-typescript` 重生成 diff 为空 | spec ↔ 实现 ↔ 前端类型一致 | 语义正确性 |
| 前端 / e2e | Vitest + MSW；Playwright | Query key 失效粒度、SSE reducer（去重/乱序/回放）、错误码→呈现映射、指标分组；提交（两形态）→ 详情 → 日志跟随 → 取消/重试弹窗、键盘 Tab 顺序、`prefers-reduced-motion` | 真后端、视觉回归（P1） |
| 压测 | k6 | `GET /runs`、`GET /runs/{id}` P95 < 300ms（§12.7），1k 并发的 SSE 长连接 | — |
| 故障注入 | chaos 脚本 | ①删 Job ②`kill -9` 控制面后重启对账 ③NFS 抖动（暂停 nfs-server 30s）④删 Deploy Pod | — |

故障注入的**验收口径**：删 Job → Task 最终进 `FAILED(EXECUTOR_UNAVAILABLE)` 或经复核 `failed(INTERNAL_ERROR)`（`detail.review=exhausted`），绝不"凭空 SUCCEEDED"；杀控制面 → 重启后 `RUNNING` 必须经 Executor 对账恢复，不得直接判失败（§12.5）；NFS 抖动 → 日志出现 `--- [log gap:` 标记、`collector_gaps_total` 增长、Task 结果不受影响。

### 14 环境与灰度

| 环境 | 依赖 | 准入条件 | 回滚 |
|---|---|---|---|
| dev | kind 或单节点 k3s、mock Genet、容器 Postgres、`TESTPLANE_AUTH_MODE=dev` | `make dev` 一条命令起全栈；P0 主流程能在 10 分钟内跑通 | — |
| test | 真 Genet sandbox、1 个 K8s 集群、小规格 NPU | §14.1 的 17 项 P0 验收全绿；e2e 与契约 golden 通过 | `helm rollback` |
| staging | 生产同构 NPU 集群、真实 Tester 镜像、真实 OIDC、只读账号 + 1 个可写 namespace | 单副本灰度 1 周，`cleanup_failures_total`/`precheck_failures_total` 无非预期增长；压测 P95 达标 | `helm rollback` + 镜像 digest 回退 |
| prod | 多副本、RWX NFS、OIDC 真实身份、多集群 kubeconfig | staging 连续 5 个工作日无 P1 缺陷；迁移 expand 已合入；备份演练通过 | 先回镜像（迁移向后兼容两版本），再按需 contract |

发布方式：`RollingUpdate{maxUnavailable:0, maxSurge:1}` + pre-upgrade 迁移 hook；灰度靠 `replicaCount` 与 Ingress 权重（可选），**不做按用户的 A/B**（内部工具用不上）。**红线**：`TESTPLANE_AUTH_MODE=dev` 在 `staging`/`prod` values 中出现即启动失败；`image.tag` 与 `image.digest` 必须二选一，release 环境强制 digest。

### 15 P1 扩展点

| P1 能力 | P0 就该留的东西 | 前端位置 | P0 不做什么 |
|---|---|---|---|
| MCP Server / CLI | 只要 OpenAPI 完备 + Bearer JWT 可用即可，无需新接口；`GET /api/v1/meta` 提供 `version`/`api_version` 供客户端协商 | — | 不实现 MCP/CLI 代码，不加"仅 Agent 可用"的端点 |
| Run 对比 | `runs.retry_of`、`tasks.reused_from_task_id` 两列 P0 建表时就加（nullable）；`(run_id, task_name)` 唯一约束 | `/compare?runs=a,b`：左选两个 Run，按"指标名 + labels + workload"对齐展示，**不判断可比性** | 不建指标索引表、不做图表 |
| 指标索引（跨 Run 查询） | P0 只存 `result.json`；P1 可写 **MongoDB**（`metrics` 集合：run/task/attempt/name/statistic/unit/value/labels），也可先用 PG `metrics_index` 表过渡 | 列表页加"指标筛选"列 | 不在 P0 建指标表（避免两处真相） |
| Matrix 展开 | 不预留服务端字段：展开由客户端生成 `tests[]` 后走同一 `POST /runs`（API 不变，`dry-run` 已能校验） | 提交页 Task 列表的「展开矩阵」按钮（P1） | 不做服务端 `expand` 参数 |
| SSE 回放窗口 | `events.seq`（Run 内单调，bigserial per run）与 `events.at` 索引 P0 必需，`Last-Event-ID` 靠它 | — | 不做跨 Run 的事件订阅 |
| 失败续跑（跨 Run） | `retry_of` 列 + 复用校验四元组（§7.7）在 P0 的 Task 表结构里已具备 | 详情页「续跑失败的 Task」按钮（P1） | P0 只做同 Run 追加 Attempt |
| 看板 | `GET /queue-status` 与 `queue_depth{reason}` 指标 P0 就产出（详情页 `waiting_reason` 要用；P1 的 `/board` 复用同一数据源） | `/dashboard` 路由保留、导航按 flag 隐藏 | P0 不实现看板 UI |

**落地顺序**：先定 `api/openapi.yaml`（含 `events` 信封与 `waiting_reason` 枚举）→ 生成前后端类型 → 前端用 MSW 对着 spec 做静态页（对齐截图）→ 后端按 `control-plane.md` / `execution.md` 补实现 → 最后接 Helm 与 CI。**spec 先行**是唯一不可跳的一步：前端一旦手写类型，生成客户端与双向校验就都失效了。
