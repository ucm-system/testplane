# TestPlane Web

TestPlane 的前端（控制台）。规格来源：**接口以 `server/api/openapi.yaml` 为准**（唯一机器契约），
视觉基准 `design/prototype/`（tokens.css / holy-grail.css + screenshots），实现方案 `design/impl/web-and-delivery.md`。

## 技术栈

| 关注点 | 选型 |
|---|---|
| 构建 | Vite 7（`build.target=chrome110`，产物 `dist/`，由 server embed，别改路径） |
| 框架 | React 19 + TypeScript 5.9（`strict` + `noUncheckedIndexedAccess`） |
| 样式 | Tailwind CSS v4（`@tailwindcss/vite`），语义 token 在 `src/index.css` 的 `@theme` 里 |
| 组件 | 手写 shadcn/ui 风格基元（`src/components/ui/`，无 Radix 依赖） |
| 路由 | react-router 7（`BrowserRouter`，路由级 `lazy` 拆包） |
| 图标 | `lucide-react` |
| YAML | `yaml`（提交页解析 deploy 段、渲染部署参数快照） |

## 命令

```bash
pnpm install
pnpm dev        # http://127.0.0.1:5173（端口 5173，strictPort）
pnpm build      # tsc -b && vite build → dist/
pnpm preview
pnpm typecheck  # 只跑 tsc -b
```

真实后端用例（`tests/real-api.spec.ts`，至少打 `/healthz` + `/api/v1/runs`，需控制面可访问）：

```bash
# 先准备控制面（默认 8080；有 port-forward 就覆盖 TESTPLANE_BACKEND）
VITE_USE_MOCK=0 TESTPLANE_BACKEND=http://127.0.0.1:8080 pnpm exec playwright test tests/real-api.spec.ts
```

## 环境变量（`.env.local`，参考 `.env.example`）

| 变量 | 默认 | 说明 |
|---|---|---|
| `VITE_USE_MOCK` | `0` | `0` 打真实后端（**默认，别改**：dist 会被 go:embed 进二进制）；`1` 用 `src/api/mock.ts` 的假数据，只在 `.env.local` 里开 |
| `VITE_API_BASE` | `/api/v1` | 接口前缀（07 §1） |
| `VITE_DEV_USER` | `dev` | dev 鉴权的默认 Bearer subject |
| `TESTPLANE_BACKEND` | `http://127.0.0.1:8080` | **仅 vite dev 用**：`server.proxy` 的目标（可用 `.env.local` 覆盖） |

- 接后端（默认路线）：`pnpm dev`，代理目标默认 8080；端口转发到别处就写 `TESTPLANE_BACKEND=http://127.0.0.1:8081` 到 `.env.local`。
- 后端没起、只想看页面：在 `web/.env.local` 里写 `VITE_USE_MOCK=1`（`.env.local` 已被 gitignore）。

Vite 代理把 `/api` 转给控制面，并对 SSE 关缓冲 + `no-cache`。

## 目录

```text
src/
├── api/            # client（fetch/错误归一/鉴权头）、contract（Api 接口）、real（真接口）、
│                   # mock（假数据 + 事件模拟）、normalize（信封/Tester 归一）、sse（fetch 流）、types（契约类型）
├── components/
│   ├── ui/         # shadcn 风格基元：button card badge table input dialog drawer tabs toast segmented skeleton states
│   └── app/        # AppShell（圣杯外壳）、RightRail、StatusBadge、LogViewer、EventTimeline、KpiCard
├── hooks/          # useAsync（三态数据）、useRunEvents（SSE）、useIdentity
├── lib/            # status（状态→中文标签/色）、utils（时间/时长/复制/cn）
└── pages/          # dashboard runs run-detail submit testers clusters
```

## 路由

| 路由 | 页面 | 数据 |
|---|---|---|
| `/` | 概览：4 个 KPI（进行中 Run / 排队 Task / 成功率 / Tester 数）+ 最近 Run + 排队 Task | `GET /runs`、`GET /testers`、`GET /queue-status` 现算 |
| `/runs` | Run 列表：状态过滤（服务端 `?status=a,b`）、时间与关键字（本地）、游标分页 | `GET /runs?limit&cursor&status` |
| `/runs/:id` | Run 详情：概要、执行链路、Deploy 区块（水位与原因原文 / Pod 拓扑 / 参数快照；**不展示位次**，D-46）、Task 摘要列表 + 点开的 Task 详情（Attempt + 日志）、事件流；页头：取消 / 重试（终态且失败类）/ 查看报告；Task 行有「取消 Task」、Task 详情有「重跑这个 Task」 | `GET /runs/{id}`、`GET /runs/{id}/deploy`、`GET /tasks/{id}`、`GET /tasks/{id}/attempts`、`GET /tasks/{id}/attempts/{idx}/logs`、`GET /runs/{id}/events`(SSE)、`GET /runs/{id}/events/history`、`GET /runs/{id}/retry-plan`、`POST /runs/{id}/retry`、`POST /tasks/{id}/cancel`、`POST /tasks/{id}/retry` |
| `/runs/:id/report` | Run 报告：counts / Task 明细 / 失败清单 / `report.md` 原文 | `GET /runs/{id}/report`、`GET /runs/{id}/report.md` |
| `/submit` | 提交：部署 / 不部署两形态、env + secrets、执行环境 profile、Task 列表（Tester 下拉 + args） | `GET /testers`、`POST /runs?dry_run=true`、`POST /runs` |
| `/testers` | Tester 目录（只读）+ 详情抽屉（参数 / env 声明） | `GET /testers`、`GET /testers/{name}` |
| `/clusters` | 集群与队列：集群连通性、平台槽位、Volcano 队列、排队 Task + 等待原因计数 | `GET /clusters`、`GET /queue-status` |
| `/admin` | 管理台（**仅 admin**，其他人看到「无权限」页、导航里没有入口）：Profiles / Clusters / Secrets / Audit 四个 Tab | `GET /profiles`、`POST|PUT|DELETE /admin/profiles`、`/admin/clusters`、`GET|POST /admin/secrets`、`GET /admin/audit` |
| `/endpoints` | 旧链接兼容 → 重定向 `/clusters` | — |

页面级操作放在 main 顶部（详情页：返回列表 / 取消 Run / 重试 / 查看报告），右上角是身份与角色（下拉里有登出 / dev 身份切换 / groups）。

## 演示脚本（截图 / 录屏）

```bash
cd web
pnpm exec playwright test tests/demo.spec.ts   # 走 mock，不依赖后端；产物在 web/.data/demo/（入库副本见 docs/demo/）
```

八步截图：`01-home` / `02-submit` / `03-runs` / `04-run-detail` / `05-report` / `06-clusters` / `07-admin-audit` / `08-retry-dialog`。
`.data/` 已被 `web/.gitignore` 忽略（演示产物不入库）。

## 接口约定

- 前缀 `/api/v1`；dev 鉴权 `Authorization: Bearer <subject>`（默认 `dev`），当前身份 `GET /api/v1/me` → `{user, role}`。
- 列表分页：游标，响应 `{items, next_cursor, has_more}`；`status` 支持逗号多值。
- 错误信封：契约是**扁平** `{code, message, detail?, retryable, trace_id}`；`api/client.ts` 同时兼容 `{error:{…}}` 包裹形态，
  页面统一展示 **code + message**（+ retryable / trace_id / detail 折叠）。
- 取消：`POST /runs/{id}/cancel`、`POST /tasks/{id}/cancel`（子路径，202 与「已 canceling」的 200 同构）。
- Attempt 以 `{task_id}` + `{idx}`（idx 从 1 开始）寻址；日志 `text/plain`，带 `X-Log-Offset` / `X-Log-Coverage`。
- `Run` 没有 `duration_seconds`：前端用 `started_at`（缺省 `submitted_at`）→ `finished_at`（缺省 now）现算。
- `Run.tasks` 是 `TaskSummary`（只有 id/name/status/waiting_reason/attempts/error）——**没有 tester/seq/耗时**；
  Tester、args、profile、Attempt 全部靠点开时 `GET /tasks/{id}` 拉 `TaskDetail`。
- `task_counts`：`failed` 含 canceled、`pending` 含 queued；契约只必填 `total/succeeded/failed`，
  缺 `pending/running/skipped` 时按 0 处理，整个字段缺失时列表显示 `—`。
- **已接** `GET /runs/{id}/deploy/logs`（按 Pod 分页签，`?pod=` 必选、`offset` 单 Pod 游标、`tail_lines` 与 `offset` 二选一、`gaps[]` 显式标缺口）；Pod 拓扑/参数快照来自 `GET /runs/{id}/deploy`。
- **重试与管理端点均已在 openapi 内**：`GET /runs/{id}/retry-plan`、`POST /runs/{id}/retry`、`POST /tasks/{id}/retry`
  与 `/admin/{profiles,clusters,secrets,audit}`；字段按 schema 对齐（改成路径参数之类的偏差请先改 openapi）。

### 与后端实测的差异（2026-11-05 首轮联调）

| 观察项 | 契约（openapi.yaml） | 后端实测 | 前端处理 |
|---|---|---|---|
| 错误信封 | 扁平 `{code,message,detail?,retryable,trace_id}` | 未取到错误体（联调时再核对） | 两种形态都能吃 |
| `GET /runs/{id}` | 200 + `Run`（含 `tasks[]`） | 200 但**空 body**（backend-core 修中） | 详情页会落到错误/空态，等修完再看 |
| `Run.task_counts` | 有 | 列表响应里**还没返回** | 列表「Task x / y」暂显示 `—` |
| `Run.duration_seconds` | 无 | 无 | 前端现算 |
| `TaskSummary.tester` | 无 | 无 | Task 表不显示 Tester，点开拉 TaskDetail |
| `GET /clusters` | `{items: Cluster[]}`，`Cluster{name, api_server, description, last_check_*} ` | 同形状 ✓ | 已按契约 |
| `GET /queue-status` | `{slots{concurrency_max,active,available,endpoints}, waiting{total,by_reason,tasks}, volcano{available,detail,queues}}` | 同形状 ✓ | 已按契约 |
| `GET /testers` | `Tester.parameters/env` 是 **map** | map ✓（含 `required`） | api 层归一成数组再给 UI |
| `POST /runs` 提交体 | 顶层 `run:` 嵌套 | 目前只吃**平级**写法（backend-core 修中） | 前端按嵌套发，等后端修完即通 |
| `tests[].execution.profile` | 必填 | — | **MVP 没有 `/profiles` 端点**：提交页给可编辑默认值 `k8s-npu`（待裁决） |
| `Run.deploy.queue_position` | 字段保留、**恒为 null**（D-46：部署方只给水位计数，排队在 Volcano 层，没有位次） | 恒 null | **UI 不展示位次**：排队态只显示「排队中 · 已等待 <时长>」+ `queue_reason` 原文；`types.ts` 注释写明"不要读取它做展示" |
| `VolcanoQueue.reason` | **自由文本 `string \| null`**：PodGroup `conditions[-1].message` 原文（如 `0/8 nodes are available: 8 Insufficient nvidia.com/npu.`），平台不翻译 | 同契约 | 队列卡片按原文渲染（等宽 + `break-all`），不做枚举映射；结构化分类只看 `waiting.by_reason` 的六个键 |
| `VolcanoSnapshot.stale` | **快照层**字段：本次刷新失败、上次成功；无时间阈值。置位时 `available=false` 且 `queues=[]`（不回放历史数字） | 同契约 | `available=false` 渲染降级卡片（`detail` 原文 + 可选「曾采到过快照」徽标），**绝不渲染成「0 排队 / 无排队」** |

## 登录态与权限（OIDC）

判定**不猜**，只有两个信号（`src/api/auth.ts`）：

| 信号 | 用途 |
|---|---|
| `GET /meta` 的 `version` | 保守预判：`dev` / `*-dev` → 先按 dev 部署处理，避免 401 把人送去并不存在的 `/auth/login`；其它 → 先按 oidc |
| `GET /me` 的 `csrf_token` | **权威**：有 token 即 OIDC 会话（覆盖 meta 的判断）；没有即 dev 部署 |

```ts
// api/auth.ts
export function looksLikeDevDeployment(version: string | undefined): boolean {
  if (!version) return true
  return /^dev\b/i.test(version) || /-dev\b/i.test(version)
}

export function applyIdentity(identity: Identity): void {
  const csrfToken = identity.csrf_token?.trim() ? identity.csrf_token : null
  state = {
    mode: csrfToken ? 'oidc' : 'dev',
    identity,
    csrfToken,                                   // 只存内存，不落 localStorage
    role: identity.role ?? null,
    canOperate: identity.role !== 'viewer',      // viewer 只读
    ready: true,
  }
}
```

| 行为 | 规则 |
|---|---|
| CSRF | token 只在内存；`api/client.ts` 对 `POST/PUT/PATCH/DELETE` 自动带 `X-CSRF-Token`（`POST /auth/logout` 同一条路） |
| 认证头 | OIDC 会话走 Cookie，**不发** Bearer（免得后端优先读 Bearer 里的假 subject 盖掉会话身份）；dev 部署才发 `Authorization: Bearer <subject>` |
| 401 | `handleUnauthorized()`：OIDC 部署 → `location.assign('/auth/login?return=<当前路径+查询串>')`；dev 部署**什么都不做**；一次会话只跳一次（`sessionStorage` guard），`/me` 成功后清除 guard |
| 403 | 不当网络错误：`ErrorState` 渲染成「无权限：&lt;message&gt;」，**不给重试按钮** |
| 角色 UI | 顶栏显示 `user` + 角色徽章（viewer 灰 / operator 蓝 / admin 琥珀）；下拉里 OIDC 有「登出」（`POST /auth/logout` → 回 `/`），dev 有「切换 dev 身份」 |
| viewer 只读 | 提交 / 校验 / 取消 Run 按钮置灰 + `title="需要 operator 角色"`，页脚写明只读；不靠后端 403 兜底 |

dev 部署与 mock 模式都没有 `/auth/*`：`/me` 无 `csrf_token` → mode=dev，不会触发登录跳转。

## SSE

`hooks/useRunEvents.ts` + `api/sse.ts`：

1. 首屏 `GET /runs/{id}/events/history` 补时间线（`since_seq` 续取，`next_cursor` 就是末条 seq）；
2. 再 `fetch` + `ReadableStream` 订阅 `/runs/{id}/events`（带 `Last-Event-ID` 与 `Authorization`），按 `\n\n` 切帧；
3. seq 去重（Set）+ 断点存 `sessionStorage["testplane:sse:<runId>"]`；
4. 退避 1→2→4→8→30s；连续失败 → 状态变 `polling`，hook 自己 3s 轮询 `events/history` 兜底（页头写明「已降级」）；
5. 切后台断开、回来续传；`resync_required` → 丢断点重建；终态 Run 不订阅只补历史。

不用原生 `EventSource` 的原因：要带自定义请求头（Authorization / Last-Event-ID），且 401 时不能静默重连。

事件信封按契约取 `{seq,type,run_id,task_id?,attempt_id?,at,from?,to?,reason?,payload?}`；时间线文案优先后端的 `payload.text`。

## 视觉与设计 token

`src/index.css` 的 `@theme` 把 `design/prototype/assets/tokens.css` + `holy-grail.css`（最终生效的 slate 家族）
搬成语义类名：`bg-canvas` / `bg-surface` / `text-fg[-2|-3]` / `border-line[-soft|-strong]` /
`text-primary-text` / `bg-success-chip` 等；圆角 6/8/12/16，字号 11–30，间距 4 的倍数，动效 150ms ease-out，只用 sm/md 两档阴影。
业务组件里不写裸 hex（唯一例外：日志终端的 `slate-300` 一类终端配色与 `#0b1220` 底，已在 `@theme` 里定成 `bg-terminal`）。

### 圣杯布局（对齐原型，别只改外观）

- **结构**：`AppShell` 的 body 是 `grid grid-cols-[240px_minmax(0,1fr)_288px] items-stretch`，
  **`<main>` 在 DOM 里写在两侧栏之前**，用 `col-start-2 row-start-1` 摆到中间（读屏/Tab 顺序先内容后导航）；
  两侧栏 `sticky top-14 h-[calc(100vh-6rem)] self-start overflow-y-auto`（页头 56 + 页脚 40）。
- **断点**：`>1024` 三列；`768–1024` 收右栏（`grid-cols-[240px_minmax(0,1fr)]`）；`<768` 单列堆叠、左栏走抽屉、页头由 grid 切成 flex 单行。
- **页头对齐**（用户最较真的两条）：页头与 body **共用同一套列宽**，品牌列 240px 带 `border-r`、
  用户列 288px 带 `border-l` —— 于是左分隔线落在 **x=240**（左栏右边缘）、右分隔线落在 **x=vw−288**（右栏左边缘，
  1280 宽时 = 992）。`tests/layout.spec.ts` 与 `tests/layout-audit.mjs`（Lead 的 DOM 实测脚本，35 项）都断言这些坐标，不靠肉眼：
  `DEMO_BASE=http://127.0.0.1:8133 node tests/layout-audit.mjs`（先 `VITE_USE_MOCK=1 pnpm build`，再 `pnpm exec vite preview --port 8133`）。
- **根字号必须 16px**：Tailwind 的 spacing / 尺寸类都是 rem，`html{font-size:14px}` 会把 `h-14` 缩成 49px、
  侧栏 240 缩成 210 —— "左栏差 30px / 页头高 49" 就是这个坑。正文 14px 由 `body` 给，别动根字号；
  布局关键尺寸另用显式像素：`h-[56px]`、`top-[56px]`、`h-[calc(100vh-96px)]`、`grid-cols-[240px_minmax(0,1fr)_288px]`。
- **等高与钉住同时成立**：两侧栏外层 `self-stretch`（撑满行高 ⇒ 三列等高），内层 `sticky top-[56px] h-[calc(100vh-96px)]`
  （不随主内容滚走）。
- 页头品牌是 **SVG 字标**（`components/app/Wordmark.tsx`，从原型原样搬来，`useId()` 生成渐变 id 防串色），
  不是 `<img favicon> + 文字`；页头**不放产品级按钮**（新建 Run / 提交运行都在左导航）。
- 页头标题 15px/600（"当前在哪"），页面内对象名用 h1 19px/600。

### 字号层级（原型 README 那张表）

| 位置 | 字号 / 字重 |
|---|---|
| 页面内 h1（对象名） | 19px / 600 |
| 品牌字标 | 26px 高，字 17–20px / 650 |
| 卡片标题 | 15px / 600 |
| 表格正文 | 14px；表头 12px / 500（中文表头） |
| 说明文字 | 12.5px；徽标 / 标签 12.5px |
| KPI 数字 | 30px / 600 + `tabular-nums` + `-0.03em` |

### 字体（自托管，不赌系统字体）

`design/prototype/assets/fonts/*.woff2` → `web/public/fonts/`，`src/styles/fonts.css` 原样两个 `@font-face`
（`font-display: swap`），`index.css` 里 `@import "./styles/fonts.css"` 且 `--font-sans` 用原型同序栈
（Albert Sans → Noto Sans SC → 系统兜底）。运行时零外网请求。

> `web/public/fonts/*.woff2` 是 `design/prototype/assets/fonts/` 的**副本**：改字体要同步两处（原型是设计基准）。

### 圆角与染底

`Card` 统一 `overflow-hidden`：语义染底的卡头（`bg-primary-weak` / `bg-warn-weak` …）、表头底色、
行 hover 都必须裁在 12px 圆角里（T39：配色是矩形、圆角是圆角，会"伸出去"）。

## 已知偏差 / TODO

| 项 | 现状 | 后续 |
|---|---|---|
| 类型来源 | `api/types.ts` **按 `server/api/openapi.yaml` 手写**（每个类型标注 schema 名） | `openapi-typescript` 生成并入库，CI 校验 diff |
| 数据层 | 约 50 行 `useAsync`（无缓存层） | 换成 TanStack Query，query key 按 web-and-delivery §3 |
| 字体 | **自托管** Albert Sans + Noto Sans SC 子集（`public/fonts/` + `src/styles/fonts.css`），已在用 | — |
| 提交页 YAML 编辑 | `textarea` + 解析校验 | CodeMirror 6（按行标错、`?from=<run>` 回填） |
| 执行环境 | 契约已有 `GET /profiles`；提交页仍是可编辑文本框（默认 `k8s-npu`），管理台可维护 | 提交页改下拉（一行改动） |
| 报告页 / 指标表 | 报告页**已做**（`/runs/:id/report`：counts / Task 明细 / 失败清单 / `report.md` 原文）；指标表仍不做（契约明确不聚合） | 需要聚合视图时另开票 |
| Deploy 日志 | **已接**：`GET /runs/{id}/deploy/logs`，按 Pod 分页签、`offset/next_offset` 加载更多、`tail_lines` 先看最新、`gaps[]` 显式标「此处不连续」 | 滚动跟随时可加 `follow` |
| Run / Task 级重试 | **已做**（`GET /runs/{id}/retry-plan` + `POST /runs/{id}/retry` + `POST /tasks/{id}/retry`），字段按 openapi 的 `RetryPlan`/`TaskRetryAccepted` 对齐（`reuse[].reason`、`rerun[].failure`、`blocked_items[]`） | `types.ts` 的直写类型换 `openapi-typescript` 生成 |
| 管理台 `/admin` | **已做**（Profiles / Clusters / Secrets / Audit），路径按 openapi：`PUT /admin/{profiles,clusters}` 名字走 body、`DELETE …?name=` | delete 409 的 `detail.profiles[]` 在确认弹窗里展示 |
| 日志跟随 / 虚拟滚动 | 一次性读取，无 follow 流 | `?follow=1` 流式追加 + `useVirtualizer`（>10k 行） |
| 权限 | **已按 role 收口**：viewer 只读（提交/校验/取消 Run/取消 Task/重试置灰 + title），`/admin` 非 admin 显示无权限页 | 服务端仍是权威，前端只做前置提示 |
| 测试 | Playwright：UI 冒烟 4 条（含 viewer 门禁与页签键盘）+ 布局/Deploy 日志 3 条 + client 契约 1 条 + 演示脚本 1 条；`tests/layout-audit.mjs` 35 项 DOM 实测；真后端用例 2 条（需 `VITE_USE_MOCK=0`） | 补 Vitest + MSW（错误码映射、SSE reducer） |
| Lint | 无 | eslint / prettier 按 CI 要求补 |

## 验证过的事

- **布局/页头坐标**：`tests/layout.spec.ts`（`getBoundingClientRect` 断言 240 / vw−288 / 三列等高 / main 先于 aside）+ `tests/layout-audit.mjs`（35 项，需 `DEMO_BASE`）→ 全绿。
- **client 请求形状**：`tests/client-contract.spec.ts` 在浏览器里 import `real.ts`、替换 `fetch` 记录 URL/method/body，
  断言 admin 更新/删除走 `?name=` + body（改成路径参数这条会红）。
- **真后端 CRUD**：`tests/real-api.spec.ts` 的 admin profile 创建→更新→删除（走前端 client，需 `VITE_USE_MOCK=0` + `TESTPLANE_BACKEND`）。
- **Deploy 日志单 Pod 游标**：`tests/layout.spec.ts` 第三条断言页签切换带 `pod`、`tail_lines` 不与 `offset` 同传、`next_offset` 续读、缺 pod 400 / 未知 pod 404。

- `pnpm exec tsc -b` ✅、`pnpm build` ✅（entry ≈119.3 KB，gzip ≈8.1 KB；路由级拆包）。
- `pnpm dev` 在 5173 起得来（默认 `VITE_USE_MOCK=0`，请求打到代理目标）。
- 默认（真实后端）产物在没有后端时**不白屏**：`/` 显示 `ENDPOINT_UNREACHABLE` + 中文说明 + 可重试 + detail，其余区块走空态；
  页头无 `mock` 徽标、页脚显示「控制台」，确认 mock 没有被编进默认产物。
- mock 产物（`VITE_USE_MOCK=1`）跑 jsdom 冒烟：`/`、`/runs`、`/runs/:id`（deploy 排队 / 就绪 / 执行中三种）、`/submit`、`/testers`、`/clusters`
  全部 0 运行时错误；详情页 Deploy 区块有 Pod 拓扑与参数快照、Task 点开有 Attempt 与日志、事件时间线有内容；
  执行中 Run 的 SSE 在 9s 内追加了 attempt/task 事件；不存在的 Run 落到 `NOT_FOUND`（code + message 同时展示）。

## 联调记录

| 日期 | 后端 | 走通的功能 | 关键响应 / 差异 |
|---|---|---|---|
| 2026-11-05 | port-forward `127.0.0.1:8081`（kind 内 `testplaned`） | 只读探测：`/meta`、`/me`、`/clusters`、`/queue-status`、`/runs?limit=3`、`/testers`、`/tasks?limit=2` 全部 200 且形状与契约一致；据此把 `types.ts` 全量对齐 openapi.yaml | 差异见上面「与后端实测的差异」表：`GET /runs/{id}` 空 body、`task_counts` 未返回、提交体平级写法，均在 backend-core 修 |
| 2026-11-05 | **契约 fixture**（本地 18083，按 openapi 形状手写的假后端）+ `pnpm dev` 代理 | 真实模式（`VITE_USE_MOCK=0`）端到端跑通：`/`、`/runs`、`/runs/:id`、`/submit`、`/testers`、`/clusters` **0 运行时错误**；`/me` 身份（dev·admin）进来、列表 `task_counts` 进度（2 / 2）、Tester `parameters` map→数组（2 参数）、详情 Deployment ID / Pod 拓扑 / `report.md` 链接、Attempt + `X-Log-Offset`/`X-Log-Coverage` 日志、Volcano 不可达降级都验到了；代理对 SSE 不缓冲（`text/event-stream` 逐帧到） | 差异：无（fixture 就是按契约写的） |
| 待办 | 真后端（8081） | 提交（含 deploy）→ 详情 → 事件流 → Testers → Clusters 全链路 | 等 backend-core 报「改完」后填这一行 |
