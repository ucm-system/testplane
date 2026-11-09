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

const REPO = 'https://github.com/ucm-system/testplane/blob/main'

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
    id: 'quickstart',
    title: '快速开始',
    design: { label: 'design/views/07-api.md', href: `${REPO}/design/views/07-api.md` },
    body: `提交一次 Run 只有三步：**写测试 → 校验 → 正式提交**。

### 1. 写一份 \`tester.yaml\`

Tester 是平台唯一的"测试知识"载体，随镜像内置。最少需要 \`name\`、\`role\`、\`entry\`、\`image\`：

\`\`\`yaml
name: my-accuracy
version: 1.0.0
display_name: 我的精度评测
role: test              # test | pre_check
on_failure: continue    # continue | abort_run
entry: /tester/run.sh
image: registry.example.com/testplane/my-accuracy:1.0.0
parameters:
  dataset:
    type: string
    required: true
\`\`\`

### 2. 先 dry-run，不落库

\`\`\`bash
curl -sS -X POST "$TESTPLANE/api/v1/runs?dry_run=true" \\
  -H "Authorization: Bearer $TOKEN" \\
  -H "Content-Type: application/json" \\
  -d @manifest.json
\`\`\`

返回 \`200\` + \`{valid: true}\`，或者 \`200\` + \`issues[]\`（每条带 \`path\` / \`code\` / \`message\`）。
**校验不通过时不会创建任何 Run**，控制台的"校验"按钮走的就是这条。

### 3. 正式提交

把 \`?dry_run=true\` 去掉即可，返回 \`202\` + \`{run, tasks}\`。平台不做幂等：
同一个 Manifest 提交两次就是两个 Run（要避免重复先查 \`GET /api/v1/runs?submitter=…\`）。`,
  },
  {
    id: 'tester-contract',
    title: 'Tester 契约',
    design: { label: 'design/views/05-tester-contract.md', href: `${REPO}/design/views/05-tester-contract.md` },
    body: `一个 Tester 就是一个目录，平台只认这几个约定：

\`\`\`
testers/<name>/
├── tester.yaml          # 声明（参数、env、角色、失败策略）
├── run.sh               # 入口，必须可执行
└── build/Dockerfile     # 镜像构建
\`\`\`

### 参数怎么进来

平台把 \`args\` 做**词法切分**成 argv，原样追加在入口脚本后面，形式固定为 \`--key=value\`：

\`\`\`bash
# Manifest 里写：args: --dataset=/data/gsm8k.jsonl --limit=200
/ tester/run.sh --dataset=/data/gsm8k.jsonl --limit=200 \\
  --url=http://10.0.0.10:8000/v1 --model_name=Qwen3-32B
\`\`\`

\`--url\` / \`--model_name\` / \`--model_path\` 由平台注入到末尾，**args 里再写一遍会被拒绝**
（\`PUBLIC_PARAM_CONFLICT\`）。

### 平台注入的环境变量

| 变量 | 含义 |
|---|---|
| \`TESTPLANE_RUN_ID\` | 本次 Run 的 ID（\`r-YYYYMMDD-XXXX\`） |
| \`TESTPLANE_TASK_ID\` | 当前 Task 的 ID |
| \`TESTPLANE_ATTEMPT_ID\` | 当前 Attempt 的 ID（重试会变） |

### 产物写在哪

结果固定写到 **\`/workspace/output/result.json\`**（Attempt 级），平台读它拿指标与失败原因；
同目录下的其它文件会一起收进产物列表。没写这个文件不算成功。

### 改了行为就要 bump 版本

Tester 的 \`version\` 是 Run 引用的一部分（\`name@version\`）。改了参数或行为**必须**升版本，
否则历史 Run 的复现信息就是错的。`,
  },
  {
    id: 'shared-mounts',
    title: '统一共享挂载',
    design: { label: 'design/ARCHITECTURE.md §4', href: `${REPO}/design/ARCHITECTURE.md` },
    body: `模型、数据集这类大文件不进镜像，走**只读**共享挂载。

- 部署侧：chart 的 \`sharedMounts\` 声明要挂哪些 PVC / NFS 路径；
- 运行侧：容器里通过环境变量 \`TESTPLANE_SHARED_MOUNTS\` 拿到挂载清单（JSON）；
- 默认两个挂载点：**\`/models\`** 与 **\`/mnt/model\`**，都是**只读**。

### 导出侧的权限

平台会在挂载前把导出目录的权限收敛成 **目录 0755 / 文件 0644**，否则容器里以非 root 跑读不到。

### \`fsGroup\` 的坑

\`fsGroup\` 对**普通 NFS** 无效（NFS 不做 UID 映射）：给 Pod 设了 \`fsGroup\` 也改不了远端文件的属主。
所以不要指望"设了 fsGroup 就能写"——共享挂载一律按只读打算。`,
  },
  {
    id: 'status-semantics',
    title: '状态语义',
    design: { label: 'design/views/02-state-machine.md', href: `${REPO}/design/views/02-state-machine.md` },
    body: `界面上最重要的区分：**跳过 ≠ 失败**。

| 状态 | 含义 | 影响成功率 |
|---|---|---|
| \`succeeded\` | 跑完且通过 | ✅ 计入成功 |
| \`failed\` | 跑完但没通过 | ❌ 计入失败 |
| \`skipped\` | **没跑**（前面的 Task 失败导致 \`on_failure: abort_run\` 或 Run 被取消） | 不计入 |
| \`canceled\` | 人主动取消 | 不计入 |

- \`canceled\` 与其他终态一样是**终态**，不会自己再动；
- Run 的终态集合：\`succeeded\` / \`completed_with_errors\` / \`service_unavailable\` / \`failed\` / \`canceled\`；
- 页面上所有状态都由服务端推送（SSE），UI 只读，不做乐观更新。`,
  },
  {
    id: 'cli-ci',
    title: 'CLI 与 CI',
    design: { label: 'design/impl/control-plane.md §8', href: `${REPO}/design/impl/control-plane.md` },
    body: `当前仓库**没有**发布 \`tpctl\`（P0 只做了 REST + Web 控制台），CI 里请直接用 curl + token。

### 在 CI 里跑一次

\`\`\`bash
set -euo pipefail
TESTPLANE="$TESTPLANE_URL"   # 形如 https://testplane.example.com
TOKEN="$TESTPLANE_TOKEN"      # 身份源签发的 token

# 1) 先校验，避免创建垃圾 Run
curl -sSf -X POST "$TESTPLANE/api/v1/runs?dry_run=true" \\
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \\
  -d @manifest.json > dry.json
jq -e '.valid == true' dry.json > /dev/null || { jq '.issues' dry.json; exit 1; }

# 2) 提交，拿 Run ID
RUN_ID=$(curl -sSf -X POST "$TESTPLANE/api/v1/runs" \\
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \\
  -d @manifest.json | jq -r '.run.id')

# 3) 轮询到终态（也可以订阅 SSE: /api/v1/runs/$RUN_ID/events）
until jq -e '.status | test("succeeded|failed|completed_with_errors|service_unavailable|canceled")' \\
  <(curl -sSf "$TESTPLANE/api/v1/runs/$RUN_ID" -H "Authorization: Bearer $TOKEN"); do
  sleep 5
done
\`\`\`

退出码由 Run 终态决定：\`failed\` / \`service_unavailable\` 应当让流水线失败；
\`completed_with_errors\` 取决于你的策略（测试有失败但流程打完）。`,
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
