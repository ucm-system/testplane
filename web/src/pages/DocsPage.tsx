import { useEffect, useMemo, useState } from 'react'
import { BookOpen, ExternalLink } from 'lucide-react'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { MarkdownView } from '@/components/ui/markdown-view'
import { cn } from '@/lib/utils'

/**
 * 文档页（`/docs`）。
 *
 * 定位：**给要用平台的工程师**的一份手册，不是设计文档的副本 —— 每节末尾都指回
 * `design/` 里的权威文件，正文只保留"今天就要用到的那些约定"。
 *
 * 结构：左侧页内目录（锚点）+ 右侧正文卡。正文用 `MarkdownView`（react-markdown + remark-gfm）
 * 渲染成组件，所以代码块/表格/列表都是真排版，不是 `<pre>` 里的一坨文本。
 */

const REPO = 'https://github.com/ucm-system/argus/blob/main'

interface DocSection {
  id: string
  title: string
  /** 正文（Markdown 字符串常量） */
  body: string
  /** 指向设计文档的权威出处 */
  design: { label: string; href: string }
}

const SECTIONS: DocSection[] = [
  {
    id: 'agent',
    title: 'Agent 亲和（MCP / llms.txt）',
    design: { label: 'design/impl/agent-integration.md', href: `${REPO}/design/impl/agent-integration.md` },
    body: `Argus 是"Agent 亲和"的：REST / CLI / MCP 三条入口共用同一套契约（spec 先行，openapi 是唯一真源）。机器入口从这里开始：

### 机器可读入口

- **[llms.txt](/llms.txt)** — AI Agent 标准入口：一句话简介 + 集成方式 + 最小闭环 + 文档索引（本站静态提供）；
- **[openapi.yaml](/openapi.yaml)** — 完整机器契约（50 路径 / 58 operations，schema 2.0）；
- **[GLOSSARY](https://github.com/ucm-system/argus/blob/main/GLOSSARY.md)** — 术语表（Task / runner / attempt / Endpoint）。

### MCP 工具清单（13 个 argus_*，stdio 传输）

| 工具 | 对应 REST | 备注 |
|---|---|---|
| \`argus_submit_task\` | \`POST /tasks\`（\`?dry_run=true\`） | manifest 是 YAML/JSON 原文（顶层 \`task:\`）；不做幂等 |
| \`argus_get_task\` | \`GET /tasks/{task_id}\` | 不等待，原样返回 |
| \`argus_wait_task\` | 轮询或 SSE（\`follow\`） | 返回 \`outcome\` / \`wait_mode\` / \`fallback_reason\`；timeout 不是错误 |
| \`argus_cancel_task\` | \`POST /tasks/{task_id}/cancel\` | 202/200 后回读确认；取消先 service-clean 再落 canceled |
| \`argus_retry_task\` | \`POST /tasks/{task_id}/retry\` | 整 Task 新 attempt 全量重跑含 fresh deploy；超限 \`ATTEMPT_EXHAUSTED\` |
| \`argus_clean_task\` | \`POST /tasks/{task_id}/clean\`（\`?attempt=\`） | 泄漏重放（§9.3）；幂等 |
| \`argus_list_runners\` | \`GET /runners\` | \`role\` / \`orphaned\` 过滤 |
| \`argus_get_report\` | \`GET /tasks/{task_id}/report[.md]\` | \`format=json|markdown\` |
| \`argus_get_logs\` | \`GET /tasks/{id}/attempts/{n}/runners/{seq}/logs\` | 返回 \`text\` + \`next_offset\` |
| \`argus_list_artifacts\` | \`GET /tasks/{id}/artifacts\` 或 runner 级 | Task 级 / runner 级二选一 |
| \`argus_get_file\` | \`GET /tasks/{id}/files?path=\` | 返回摘要（path/size/sha256/前 2KB），不塞二进制 |
| \`argus_queue_status\` | \`GET /queue-status\` | 三视图不可相加 |
| \`argus_list_profiles\` | \`GET /profiles\` | 返回 Profile + mounts |

### 用 Agent 跑一次评测（最小闭环）

\`\`\`text
1. 发现   argus_list_runners
          → 读 input_format / parameters / placeholder / consumes，自查能不能组成一条管道
2. 组合   按 附录 C 拼 Manifest（顶层 task:；deploy XOR service；runners[] 1..200）
3. 提交   argus_submit_task { manifest, dry_run: true }   ← 只校验不落库
          → issues[].path/line 修复后重试同一请求体（不做幂等，勿盲目重提）
          argus_submit_task { manifest }                  ← 正式提交，拿 task_id
4. 等待   argus_wait_task { task_id, follow: true }       ← SSE 优先，不可用自动降级轮询
          → 终态 succeeded / failed / service_unavailable / canceled
5. 取报告 argus_get_report { task_id, format: "markdown" }
          argus_list_artifacts { task_id } → argus_get_file { task_id, path }
\`\`\`

红线：提交不幂等（D-37）；runner 没有自己的重试（FAILED 是终态，D-69）；重试 = 整 Task 新 attempt
全量重跑含重新部署（§7.8）。`,
  },
  {
    id: 'quickstart',
    title: '快速开始',
    design: { label: 'design/views/07-api.md', href: `${REPO}/design/views/07-api.md` },
    body: `提交一次 Task 只有三步：**写 Runner → 校验 → 正式提交**。

### 1. 写一份 \`runner.yaml\`

Runner 是平台唯一的"测试知识"载体，随平台发版走目录扫描注册（附录 C **v2**）。最少需要 \`name\`、\`role\`、\`entry\`、\`input_format\`、\`image\`：

\`\`\`yaml
name: my-accuracy
version: 1.0.0
display_name: 我的精度评测
role: test              # test | pre_check（deploy / clean 仅内置，D-70）
on_failure: continue    # continue | abort_task（D-69）
entry: ["/runner/run.sh"]   # exec form 数组；平台 exec 它、argv 恒空（D-66）
input_format: jsonl     # jsonl | yaml | text（D-67）
image: registry.example.com/argus/my-accuracy:1.0.0
parameters:             # jsonl 输入的结构校验 schema（平台校验类型 + required）
  - name: dataset
    type: string
    required: true
\`\`\`

### 2. 先 dry-run，不落库

\`\`\`bash
curl -sS -X POST "$ARGUS/api/v1/tasks?dry_run=true"   -H "Authorization: Bearer $TOKEN"   -H "Content-Type: application/json"   -d @manifest.json
\`\`\`

返回 \`200\` + \`{valid: true}\`，或者 \`200\` + \`issues[]\`（每条带 \`path\` / \`code\` / \`message\`，jsonl 另带 \`line\`）。
**校验不通过时不会创建任何 Task**，控制台的"校验"按钮走的就是这条。

### 3. 正式提交

把 \`?dry_run=true\` 去掉即可，返回 \`202\` + \`{task, runners[]}\`（含注入的 service-deploy / precheck / service-clean 的最终执行序）。
平台不做幂等（D-37）：同一个 Manifest 提交两次就是两个 Task（要避免重复先查 \`GET /api/v1/tasks?submitter=…\`）。`,
  },
  {
    id: 'runner-contract',
    title: 'Runner 契约',
    design: { label: 'design/ARCHITECTURE.md 附录 C', href: `${REPO}/design/ARCHITECTURE.md` },
    body: `一个 Runner 就是一个目录，平台只认这几个约定（附录 C v2）：

\`\`\`
runners/<name>/
├── runner.yaml          # 声明（input_format、参数 schema、env、角色、失败策略）
├── run.sh               # entry[0] 指向的入口，必须可执行
└── build/Dockerfile     # 镜像构建
\`\`\`

### 输入怎么进来（v2：没有 argv）

Manifest 里 \`runners[].input\` 是**输入原文**：平台按 Runner 的 \`input_format\` 做
词法/结构校验（jsonl 校验字段类型 + required，报错带行号），原样落盘为
\`attempts/<n>/input/params.jsonl|yaml\`，并经 **\`AG_PARAMS_FILE\`** 注入路径 ——
**argv 恒空**（D-66），入口脚本自己读文件。

\`yaml\` / \`text\` 格式只做语法校验，语义完全归 Runner（此时 \`parameters\` 必须为 \`[]\`）。

### 平台注入的环境变量（\`AG_*\`）

| 变量 | 含义 |
|---|---|
| \`AG_TASK_ID\` | 本次 Task 的 ID（\`t-YYYYMMDD-XXXX\`） |
| \`AG_ATTEMPT\` | 当前 attempt 序号（重试会变；独立运行填 1） |
| \`AG_RUNNER_SEQ\` | 当前 runner 执行序 |
| \`AG_PARAMS_FILE\` | 输入文件在容器内的路径 |
| \`AG_OUTPUT_DIR\` | 产物输出目录（默认 \`/workspace/output\`） |
| \`AG_SERVED_URL\` / \`AG_SERVED_MODEL_NAME\` / \`AG_MODEL_PATH\` | 公共参数（deploy exports 收割或外接模式直填） |
| \`AG_DEPLOY_REF\` | 来源部署的 release 名（部署模式） |

### 产物写在哪

产物写到 **\`/workspace/output\`**，三选一（D-74）：\`result.json\` / \`result.html\` / \`result.csv\` 至少其一，
**不强求 result.json**。result.json 是附录 A **schema 2.0**（必填 \`schema_version\`/\`task_id\`/\`attempt_no\`/\`runner_seq\`/\`runner\`/\`status\`/\`metrics\`），
指标必须带 \`unit\` 与 \`statistic\`，多组参数靠 \`labels\` 区分。

### 改了行为就要 bump 版本

Runner 的 \`version\` 是 Task 引用的一部分（\`name@version\`）。改了参数或行为**必须**升版本，
同 name+version 不同内容会被拒（\`RUNNER_VERSION_PINNED\`）——否则历史 Task 的复现信息就是错的。`,
  },
  {
    id: 'shared-mounts',
    title: '统一共享挂载',
    design: { label: 'design/ARCHITECTURE.md §10.4', href: `${REPO}/design/ARCHITECTURE.md` },
    body: `模型、数据集这类大文件不进镜像，走**只读**共享挂载。

- 部署侧：chart 的共享挂载声明（admin 维护，\`/admin/shared-mounts\`）要挂哪些 PVC；
- 运行侧：Runner 在 \`runner.yaml.mounts\` 声明额外挂载（§10.4）；
- 平台保留路径 \`/workspace/output\`、\`/workspace/input\`、\`/tmp\`、\`/runner\` 冲突 → 提交期 400。

### 导出侧的权限

平台会在挂载前把导出目录的权限收敛成 **目录 0755 / 文件 0644**，否则容器里以非 root 跑读不到。

### \`fsGroup\` 的坑

\`fsGroup\` 对**普通 NFS** 无效（NFS 不做 UID 映射）：给 Pod 设了 \`fsGroup\` 也改不了远端文件的属主。
所以不要指望"设了 fsGroup 就能写"——共享挂载一律按只读打算。`,
  },
  {
    id: 'status-semantics',
    title: '状态语义',
    design: { label: 'design/ARCHITECTURE.md §7', href: `${REPO}/design/ARCHITECTURE.md` },
    body: `界面上最重要的区分：**跳过 ≠ 失败**（D-06）。

| runner 状态 | 含义 | 影响成功率 |
|---|---|---|
| \`succeeded\` | 跑完且通过 | ✅ 计入成功 |
| \`failed\` | 跑完但没通过（终态，没有重试回路，D-69） | ❌ 计入失败 |
| \`skipped\` | **没跑**（前序失败 \`on_failure: abort_task\` 或取消；必带 reason） | 不计入 |
| \`canceled\` | 人主动取消 | 不计入 |

- Task 终态集合：\`succeeded\` / \`failed\` / \`service_unavailable\` / \`canceled\`；
  **没有** \`completed_with_errors\`（v1 语义并入 \`failed\`，D-73），也**没有** deploying / testing / cleanup 平台阶段（部署就是管道第一个 runner，拆除就是最后一个）；
- \`canceled\` 与其他终态一样是**终态**，不会自己再动；取消后 service-clean 仍会执行（finally 语义）；
- 页面上所有状态都由服务端推送（SSE \`/tasks/{id}/events\`），UI 只读，不做乐观更新。`,
  },
  {
    id: 'cli-ci',
    title: 'CLI 与 CI',
    design: { label: 'design/impl/control-plane.md §8', href: `${REPO}/design/impl/control-plane.md` },
    body: `当前仓库**没有**发布 \`argus\` CLI（P0 只做了 REST + Web 控制台），CI 里请直接用 curl + token。

### 在 CI 里跑一次

\`\`\`bash
set -euo pipefail
ARGUS="$ARGUS_URL"           # 形如 https://argus.example.com
TOKEN="$ARGUS_TOKEN"          # 身份源签发的 token

# 1) 先校验，避免创建垃圾 Task
curl -sSf -X POST "$ARGUS/api/v1/tasks?dry_run=true"   -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json"   -d @manifest.json > dry.json
jq -e '.valid == true' dry.json > /dev/null || { jq '.issues' dry.json; exit 1; }

# 2) 提交，拿 Task ID
TASK_ID=$(curl -sSf -X POST "$ARGUS/api/v1/tasks"   -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json"   -d @manifest.json | jq -r '.task.id')

# 3) 轮询到终态（也可以订阅 SSE: /api/v1/tasks/$TASK_ID/events）
until jq -e '.status | test("succeeded|failed|service_unavailable|canceled")'   <(curl -sSf "$ARGUS/api/v1/tasks/$TASK_ID" -H "Authorization: Bearer $TOKEN"); do
  sleep 5
done
\`\`\`

退出码由 Task 终态决定：\`failed\` / \`service_unavailable\` 应当让流水线失败；
想整 Task 重试就 \`POST /tasks/{id}/retry\`（新 attempt 全量重跑含 fresh deploy；达到 attempt 上限返回 \`409 ATTEMPT_EXHAUSTED\`）。`,
  },
]

export function DocsPage() {
  const [active, setActive] = useState(SECTIONS[0]?.id ?? '')

  // 滚动时点亮目录项（只是提示，不阻塞）
  useEffect(() => {
    const nodes = SECTIONS.map((section) => document.getElementById(section.id)).filter(
      (node): node is HTMLElement => Boolean(node),
    )
    if (nodes.length === 0) return
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries.filter((entry) => entry.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top)
        if (visible[0]?.target.id) setActive(visible[0].target.id)
      },
      { rootMargin: '-80px 0px -60% 0px', threshold: [0, 1] },
    )
    for (const node of nodes) observer.observe(node)
    return () => observer.disconnect()
  }, [])

  const links = useMemo(
    () => SECTIONS.map((section) => ({ id: section.id, title: section.title })),
    [],
  )

  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(0,14rem)_minmax(0,1fr)]" data-testid="docs-page">
      {/* AI Agent 的机器入口（llms.txt 规范）：与 openapi.yaml 一起随静态资源服务 */}
      <a
        href="/llms.txt"
        target="_blank"
        rel="noreferrer"
        data-testid="docs-llms-txt"
        className="lg:col-span-2 flex flex-wrap items-center gap-2 rounded-xl border border-primary/30 bg-primary-weak/40 px-4 py-2.5 text-2xs text-fg-2 shadow-xs transition-colors hover:border-primary/60"
      >
        <span className="font-medium text-fg">AI Agent 入口</span>
        <span>
          <code className="font-mono">GET /llms.txt</code>（集成方式 + 最小闭环）与{' '}
          <code className="font-mono">GET /openapi.yaml</code>（机器契约）随本站静态提供；人看这一页，Agent 看那两份。
        </span>
        <span className="ml-auto font-mono text-primary-text">/llms.txt →</span>
      </a>
      {/* 页内目录：窄屏收到顶部（横向滚），宽屏占左列 */}
      <Card className="h-fit lg:sticky lg:top-4">
        <CardHeader tone="muted" title="本页目录" />
        <CardBody>
          <nav aria-label="文档目录">
            <ul className="flex flex-wrap gap-1 lg:flex-col lg:flex-nowrap">
              {links.map((link) => (
                <li key={link.id}>
                  <a
                    href={`#${link.id}`}
                    data-testid={`doc-toc-${link.id}`}
                    className={cn(
                      'block rounded-md px-2 py-1.5 text-sm transition-colors',
                      active === link.id ? 'bg-primary-chip font-medium text-primary-text' : 'text-fg-2 hover:bg-surface-2',
                    )}
                  >
                    {link.title}
                  </a>
                </li>
              ))}
            </ul>
          </nav>
        </CardBody>
      </Card>

      <div className="flex min-w-0 flex-col gap-4">
        {SECTIONS.map((section) => (
          <Card key={section.id} id={section.id} className="scroll-mt-4">
            <CardHeader
              tone="primary"
              title={
                <span className="flex items-center gap-2">
                  <BookOpen className="size-4 text-fg-3" aria-hidden />
                  {section.title}
                </span>
              }
              actions={
                <a
                  href={section.design.href}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-2xs text-primary-text hover:underline"
                >
                  在仓库里看完整设计：{section.design.label}
                  <ExternalLink className="size-3" aria-hidden />
                </a>
              }
            />
            <CardBody>
              {/* MarkdownView 自带 max-h + 内滚，这里要整段铺开，所以覆盖掉 */}
              <MarkdownView
                source={section.body}
                data-testid={`doc-section-${section.id}`}
                className="max-h-none overflow-visible"
              />
            </CardBody>
          </Card>
        ))}
      </div>
    </div>
  )
}
