# TestPlane Web

TestPlane 的前端（控制台）。规格来源是 `design/`：视觉基准 `design/prototype/`（tokens.css / holy-grail.css + screenshots），
接口契约 `design/views/07-api.md`，实现方案 `design/impl/web-and-delivery.md`。

## 技术栈

| 关注点 | 选型 |
|---|---|
| 构建 | Vite 7（`build.target=chrome110`，产物 `dist/`，由 server embed，别改路径） |
| 框架 | React 19 + TypeScript 5.9（`strict` + `noUncheckedIndexedAccess`） |
| 样式 | Tailwind CSS v4（`@tailwindcss/vite`），语义 token 在 `src/index.css` 的 `@theme` 里 |
| 组件 | 手写 shadcn/ui 风格基元（`src/components/ui/`，无 Radix 依赖） |
| 路由 | react-router 7（`BrowserRouter`，路由级 `lazy` 拆包） |
| 图标 | `lucide-react` |
| YAML | `yaml`（提交页解析 deploy 段） |

## 命令

```bash
pnpm install
pnpm dev        # http://127.0.0.1:5173（端口 5173，strictPort）
pnpm build      # tsc -b && vite build → dist/
pnpm preview
pnpm typecheck  # 只跑 tsc -b
```

## 环境变量（`.env.local`，参考 `.env.example`）

| 变量 | 默认 | 说明 |
|---|---|---|
| `VITE_USE_MOCK` | `1` | `1` 用 `src/api/mock.ts` 的假数据；`0` 打真实接口 |
| `VITE_API_BASE` | `/api/v1` | 接口前缀（07 §1） |
| `VITE_DEV_USER` | `dev` | dev 鉴权的默认 Bearer subject |
| `TESTPLANE_BACKEND` | `http://127.0.0.1:8080` | **仅 vite dev 用**：`server.proxy` 的目标 |

接后端时：`$env:TESTPLANE_BACKEND='http://127.0.0.1:8080'; $env:VITE_USE_MOCK='0'; pnpm dev`
（或写进 `.env.local`）。Vite 代理把 `/api` 转给控制面，并对 SSE 关缓冲 + `no-cache`。

## 目录

```text
src/
├── api/            # 数据层：client（fetch/错误归一/鉴权头）、contract（Api 接口）、
│                   # real（真接口）、mock（假数据 + 事件模拟）、sse（fetch 流）、types（wire 类型）
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
| `/runs/:id` | Run 详情：概要、执行链路、Deploy 区块（含排队位次 / Pod / Pod 日志）、Task 列表 + Attempt + 日志、事件流 | `GET /runs/{id}`、`GET /runs/{id}/deploy`、`GET /runs/{id}/deploy/logs?pod=`、`GET /tasks/{id}/attempts`、`GET /tasks/{id}/attempts/{idx}/logs`、`GET /runs/{id}/events`(SSE)、`GET /runs/{id}/events/history` |
| `/submit` | 提交：部署 / 不部署两形态、env + secrets、Task 列表（Tester 下拉 + args） | `GET /testers`、`POST /runs?dry_run=true`、`POST /runs` |
| `/testers` | Tester 目录（只读）+ 详情抽屉（参数 / env 声明） | `GET /testers`、`GET /testers/{name}` |
| `/clusters` | 集群与队列：集群连通性、平台槽位、Volcano 队列、排队 Task | `GET /clusters`、`GET /queue-status` |
| `/endpoints` | 旧链接兼容 → 重定向 `/clusters` | — |

页面级操作放在 main 顶部（详情页：返回列表 / 取消 Run），右上角是 dev 身份切换（Bearer subject）。

## 接口约定

- 前缀 `/api/v1`；错误体 `{error:{code,message,detail,retryable}}`（`api/client.ts` 同时兼容扁平形态）。
- 鉴权：dev 模式 `Authorization: Bearer <subject>`，默认 `dev`；当前身份 `GET /api/v1/me` → `{user, role}`。
- 列表分页：游标，响应 `{items,next_cursor,has_more}`；`status` 支持逗号多值。
- 取消：`POST /runs/{id}/cancel`、`POST /tasks/{id}/cancel`（子路径）。
- Attempt 以 `{task_id}` + `{idx}`（idx 从 1 开始）寻址；日志 `text/plain`，带 `X-Log-Offset` / `X-Log-Coverage`。

## SSE

`hooks/useRunEvents.ts` + `api/sse.ts`：

1. 首屏 `GET /runs/{id}/events/history` 补时间线；
2. 再 `fetch` + `ReadableStream` 订阅 `/runs/{id}/events`（带 `Last-Event-ID` 与 `Authorization`），按 `\n\n` 切帧；
3. seq 去重（Set）+ 断点存 `sessionStorage["testplane:sse:<runId>"]`；
4. 退避 1→2→4→8→30s；连续失败 → 状态变 `polling`，hook 自己 3s 轮询 `events/history` 兜底（页头会写明「已降级」）；
5. 切后台断开、回来续传；`resync_required` → 丢断点重建。

不用原生 `EventSource` 的原因：要带自定义请求头（Authorization / Last-Event-ID），且 401 时不能静默重连。

## 视觉与设计 token

`src/index.css` 的 `@theme` 把 `design/prototype/assets/tokens.css` + `holy-grail.css`（最终生效的 slate 家族）
搬成语义类名：`bg-canvas` / `bg-surface` / `text-fg[-2|-3]` / `border-line[-soft|-strong]` /
`text-primary-text` / `bg-success-chip` 等；圆角 6/8/12/16，字号 11–30，间距 4 的倍数，动效 150ms ease-out，只用 sm/md 两档阴影。
业务组件里不写裸 hex（唯一例外：日志终端的 `slate-300` 一类终端配色与 `#0b1220` 底，已在 `@theme` 里定成 `bg-terminal`）。

响应式：`≥1280` 三列（240 / 1fr / 288），`<1280` 隐右栏，`<768` 左栏收进抽屉（页头汉堡）。

## 已知偏差 / TODO

| 项 | 现状 | 后续 |
|---|---|---|
| 类型来源 | `api/types.ts` 按 07-api.md 手写 | `api/openapi.yaml` 落地后 `openapi-typescript` 生成并入库，CI 校验 diff |
| 数据层 | 约 50 行 `useAsync`（无缓存层） | 换成 TanStack Query（设计 W-1），query key 按 web-and-delivery §3 |
| 字体 | 系统字体栈（`Segoe UI` / `Microsoft YaHei`） | 自托管 Albert Sans + Noto Sans SC 子集（设计 §5，需 `subset-fonts.mjs`） |
| 提交页 YAML 编辑 | `textarea` + 解析校验 | CodeMirror 6（按行标错、`?from=<run>` 回填） |
| 报告页 / 指标表 | 未做（路由未建） | `GET /runs/{id}/report(.md)` + `result.json` 指标分组表 |
| 日志跟随 / 虚拟滚动 | 一次性读取，无 follow 流 | `?follow=1` 流式追加 + `useVirtualizer`（>10k 行） |
| 权限 | 全部操作可见（dev 模式） | 按 `GET /me` 的 role 隐藏写按钮；403 时降级提示 |
| 测试 | 无 | Vitest + MSW（错误码映射、SSE reducer）、Playwright e2e |
| Lint | 无 | eslint / prettier 按 CI 要求补 |

## 验证过的事

- `pnpm build` 通过；产物 `dist/index.html` + hash 资源，entry ≈112KB gz、CSS ≈7.4KB gz。
- `pnpm dev` 在 5173 起得来，`/`、`/runs`、`/runs/:id`、`/submit`、`/testers`、`/clusters` 均正常渲染（含空态、骨架、错误态）。
- 构建产物用 jsdom 跑过冒烟：六个路由无运行时错误、mock 数据填满、事件时间线有内容；
  执行中 Run 的 SSE 在 9s 内追加了 `attempt.finished` / `task.state_changed` 事件（mock 状态机推进）。
