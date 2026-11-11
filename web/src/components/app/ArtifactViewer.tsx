import { useEffect, useMemo, useState } from 'react'
import { Download } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Artifact } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Banner, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { LogViewer } from '@/components/app/LogViewer'
import { cn } from '@/lib/utils'

/**
 * 产物**通用查看器**（守 §1.3：不做任何按 runner 专属的解析/渲染）。
 *
 * 按**扩展名**分发（与 content-type 同判）：
 *  - `.json`        → JSON 树视图（可折叠；解析失败降级为文本）；
 *  - `.csv`         → 通用表格（RFC4180 引号感知；只渲染前 200 行 + 提示）；
 *  - `.html`        → `<iframe sandbox>`（**不带 allow-scripts**：runner 自述内容按不可信处理，§11.4）；
 *  - `.log/.txt/.md/.yaml/.jsonl` 等文本 → 通用文本查看器（复用 LogViewer 终端风）；
 *  - 其余（二进制）  → **一律仅下载，不预览**。
 *
 * 文本类预览上限 2MB（`ARTIFACT_PREVIEW_LIMIT`）：超出直接给下载入口，不拉流。
 * 内容一律经带鉴权的 `api.downloadFile` fetch → Blob（dev/OIDC 下裸链会 401）。
 */

export const ARTIFACT_PREVIEW_LIMIT = 2 * 1024 * 1024

/** 预览上限的人话（UI 与说明共用）。 */
export function formatPreviewLimit(): string {
  return '2 MB'
}

export type ArtifactViewKind = 'json' | 'csv' | 'html' | 'text' | 'binary'

const TEXT_EXTENSIONS = new Set([
  'log',
  'txt',
  'md',
  'markdown',
  'yaml',
  'yml',
  'jsonl',
  'env',
  'out',
  'text',
  'ini',
  'toml',
])

export function artifactViewKindOf(path: string): ArtifactViewKind {
  const name = path.split('/').pop() ?? path
  const ext = name.includes('.') ? (name.split('.').pop() ?? '').toLowerCase() : ''
  if (ext === 'json') return 'json'
  if (ext === 'csv' || ext === 'tsv') return 'csv'
  if (ext === 'html' || ext === 'htm') return 'html'
  if (TEXT_EXTENSIONS.has(ext)) return 'text'
  // result.json 变体（如 result.full.json 已命中 json）；无扩展名按二进制处理（宁可不预览，不乱渲染）
  return 'binary'
}

/** 文件的 MIME 猜测（下载用 Blob 不需要，但二进制下载给个对的类型更友好）。 */
function mimeOf(path: string): string {
  const ext = (path.split('.').pop() ?? '').toLowerCase()
  if (ext === 'xlsx') return 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'
  if (ext === 'zip') return 'application/zip'
  if (ext === 'png') return 'image/png'
  return 'application/octet-stream'
}

/** 人类可读大小（与 lib/download 的 formatBytes 独立，避免组件间耦合）。 */
function humanSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

/* ---------------------------------------------------------------- JSON 树 */

const TREE_CHILDREN_LIMIT = 100

function scalarLabel(value: unknown): { text: string; tone: string } {
  if (value === null) return { text: 'null', tone: 'text-fg-3' }
  switch (typeof value) {
    case 'string':
      return { text: JSON.stringify(value), tone: 'text-success-text' }
    case 'number':
      return { text: String(value), tone: 'text-primary-text' }
    case 'boolean':
      return { text: String(value), tone: 'text-warn-text' }
    default:
      return { text: JSON.stringify(value), tone: 'text-fg-2' }
  }
}

function JsonNode({ name, value, depth }: { name?: string; value: unknown; depth: number }) {
  const [open, setOpen] = useState(depth < 2)

  if (value === null || typeof value !== 'object') {
    const label = scalarLabel(value)
    return (
      <div className="pl-3 leading-[18px]">
        {name != null ? <span className="text-primary-text">{name}</span> : null}
        {name != null ? <span className="text-fg-3">: </span> : null}
        <span className={cn('break-all font-mono text-2xs', label.tone)}>{label.text}</span>
      </div>
    )
  }

  const isArray = Array.isArray(value)
  const entries: Array<[string, unknown]> = isArray
    ? (value as unknown[]).map((item, index) => [String(index), item])
    : Object.entries(value as Record<string, unknown>)
  const truncated = entries.length > TREE_CHILDREN_LIMIT
  const visible = open ? entries.slice(0, TREE_CHILDREN_LIMIT) : []

  return (
    <div className="leading-[18px]">
      <button
        type="button"
        className="inline-flex items-center gap-1 text-2xs text-fg-2 hover:text-fg"
        aria-expanded={open}
        onClick={() => setOpen((prev) => !prev)}
      >
        <span className="text-fg-3" aria-hidden>
          {open ? '▾' : '▸'}
        </span>
        {name != null ? <span className="text-primary-text">{name}</span> : null}
        <span className="font-mono">
          {isArray ? `[${entries.length}]` : `{${entries.length}}`}
        </span>
      </button>
      {open ? (
        <div className="border-l border-line-soft pl-2">
          {visible.map(([key, item]) => (
            <JsonNode key={key} name={key} value={item} depth={depth + 1} />
          ))}
          {truncated ? (
            <p className="pl-3 text-2xs text-fg-3">
              还有 {entries.length - TREE_CHILDREN_LIMIT} 项未展开（展示前 {TREE_CHILDREN_LIMIT} 项，完整内容请下载）
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------- CSV（RFC4180 引号感知） */

export interface ParsedCsv {
  headers: string[]
  rows: string[][]
  truncated: boolean
}

export function parseCsv(text: string, maxRows = 200): ParsedCsv {
  const rows: string[][] = []
  let field = ''
  let record: string[] = []
  let inQuotes = false
  const pushField = () => {
    record.push(field)
    field = ''
  }
  const pushRecord = () => {
    pushField()
    if (record.length > 1 || (record[0] ?? '').length > 0) rows.push(record)
    record = []
  }

  for (let index = 0; index < text.length; index += 1) {
    const char = text[index]
    if (inQuotes) {
      if (char === '"') {
        if (text[index + 1] === '"') {
          field += '"'
          index += 1
        } else {
          inQuotes = false
        }
      } else {
        field += char
      }
      continue
    }
    if (char === '"') {
      inQuotes = true
    } else if (char === ',') {
      pushField()
    } else if (char === '\n') {
      pushRecord()
      if (rows.length >= maxRows) break
    } else if (char === '\r') {
      // \r\n 的 \r 直接吞掉
    } else {
      field += char
    }
  }
  if (!inQuotes && rows.length < maxRows && (field.length > 0 || record.length > 0)) pushRecord()

  const headers = rows.shift() ?? []
  return { headers, rows, truncated: rows.length >= maxRows && indexReachedEnd(text, maxRows + 1) }
}

/** 是否还有更多行（粗略：按换行计数）。 */
function indexReachedEnd(text: string, rows: number): boolean {
  let count = 0
  for (const char of text) {
    if (char === '\n') {
      count += 1
      if (count >= rows) return true
    }
  }
  return false
}

/* ---------------------------------------------------------------- 查看器（弹窗与整页共用） */

interface LoadedContent {
  kind: 'loaded'
  blob: Blob
  text: string | null
}

/**
 * 产物查看内容区（**弹窗与整页查看器共用**）：带鉴权拉取 + 按扩展名分发渲染。
 * 二进制一律仅下载；文本超 2MB 只给下载入口。
 */
export function ArtifactPreviewBody({
  taskId,
  artifact,
  /** 整页查看器用：预览区吃满可用高度（弹窗里保持固定高度的紧凑形态） */
  stretch = false,
}: {
  taskId: string
  artifact: Artifact
  stretch?: boolean
}) {
  const kind = artifactViewKindOf(artifact.path)
  const overLimit = artifact.size_bytes > ARTIFACT_PREVIEW_LIMIT
  const previewable = kind !== 'binary' && !overLimit

  const [content, setContent] = useState<LoadedContent | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)

  useEffect(() => {
    setContent(null)
    setError(null)
    if (!previewable) return
    let alive = true
    setLoading(true)
    api
      .downloadFile(taskId, artifact.path)
      .then(async (blob) => {
        if (!alive) return
        // HTML / JSON / CSV / 文本都需要文本；Blob.size 超限（服务端口径不一致时）也在此兜底
        if (blob.size > ARTIFACT_PREVIEW_LIMIT) {
          setError({
            code: 'RESOURCE_EXHAUSTED',
            message: `文件 ${humanSize(blob.size)} 超过预览上限 ${formatPreviewLimit()}`,
            retryable: false,
          })
          setLoading(false)
          return
        }
        const text = await blob.text()
        if (!alive) return
        setContent({ kind: 'loaded', blob, text })
        setLoading(false)
      })
      .catch((e: unknown) => {
        if (!alive) return
        setError(toApiError(e))
        setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [artifact.path, previewable, taskId])

  const body = useMemo(() => {
    if (kind === 'binary' || overLimit || error || !content) {
      return (
        <div className="flex flex-col items-center gap-3 py-6 text-center">
          <Download className="size-5 text-fg-3" aria-hidden />
          <p className="text-sm text-fg-2">
            {kind === 'binary'
              ? '二进制文件不提供预览，请下载后用本地工具打开。'
              : overLimit
                ? `文件 ${humanSize(artifact.size_bytes)} 超过预览上限 ${formatPreviewLimit()}，请下载查看。`
                : '预览不可用（下载后查看）。'}
          </p>
          <ArtifactDownloadButton taskId={taskId} artifact={artifact} />
        </div>
      )
    }

    if (kind === 'json') {
      let parsed: unknown
      let parseError: string | null = null
      try {
        parsed = JSON.parse(content.text ?? '')
      } catch (e) {
        parseError = e instanceof Error ? e.message : String(e)
      }
      if (parseError != null || parsed === undefined) {
        return (
          <div className="flex flex-col gap-2">
            <Banner tone="warn" title="不是合法 JSON，按文本展示">
              {parseError}
            </Banner>
            <pre className="max-h-[24rem] overflow-auto rounded-md bg-terminal px-3 py-2 font-mono text-2xs leading-[18px] text-log-muted">
              {content.text}
            </pre>
          </div>
        )
      }
      return (
        <div
          data-testid="artifact-json-tree"
          className={cn(
            'overflow-auto rounded-md border border-line-soft bg-surface-2 px-3 py-2',
            stretch ? 'h-full' : 'max-h-[26rem]',
          )}
        >
          <JsonNode value={parsed} depth={0} />
        </div>
      )
    }

    if (kind === 'csv') {
      const parsed = parseCsv(content.text ?? '')
      if (parsed.headers.length === 0 && parsed.rows.length === 0) {
        return (
          <pre className="max-h-[24rem] overflow-auto rounded-md bg-terminal px-3 py-2 font-mono text-2xs leading-[18px] text-log-muted">
            {content.text}
          </pre>
        )
      }
      return (
        <div data-testid="artifact-csv-table">
          <TableWrap className={cn('overflow-auto rounded-md border border-line-soft', stretch ? 'h-full' : 'max-h-[26rem]')}>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH className="w-10">#</TH>
                  {parsed.headers.map((header, index) => (
                    <TH key={`${header}-${index}`}>{header || '—'}</TH>
                  ))}
                </TR>
              </THead>
              <TBody>
                {parsed.rows.map((row, rowIndex) => (
                  <TR key={rowIndex}>
                    <TD className="tabular-nums text-2xs text-fg-3">{rowIndex + 1}</TD>
                    {parsed.headers.map((_, colIndex) => (
                      <TD key={colIndex} className="max-w-[16rem] break-all text-2xs">
                        {row[colIndex] ?? ''}
                      </TD>
                    ))}
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
          {parsed.truncated ? (
            <p className="mt-2 text-2xs text-fg-3">
              只渲染前 {parsed.rows.length} 行（通用预览不做分页），完整数据请下载。
            </p>
          ) : null}
        </div>
      )
    }

    if (kind === 'html') {
      return (
        <div className="flex flex-col gap-2">
          <Banner tone="info" title="sandbox 渲染（脚本已禁用）">
            Runner 自述内容按不可信文本处理：iframe 不带 allow-scripts / allow-same-origin。
          </Banner>
          <iframe
            data-testid="artifact-html-frame"
            sandbox=""
            srcDoc={content.text ?? ''}
            title={artifact.path}
            className={cn('w-full rounded-md border border-line bg-white', stretch ? 'h-[calc(100vh-14rem)]' : 'h-[26rem]')}
          />
        </div>
      )
    }

    // text（log/txt/md/yaml/jsonl/env…）：复用 LogViewer 的终端风（终态、不轮询）
    return (
      <LogViewer
        text={content.text ?? ''}
        stretch={stretch}
        ended
        hint={`${artifact.path} · ${humanSize(artifact.size_bytes)}`}
        testIdPrefix="artifact-preview"
      />
    )
  }, [artifact, kind, overLimit, error, content, taskId])

  if (loading) return <LoadingBlock label="加载产物…" />
  return body
}

/**
 * 产物查看弹窗：按扩展名分发渲染；**二进制一律仅下载**，文本超 2MB 只给下载入口。
 */
export function ArtifactViewerDialog({
  taskId,
  artifact,
  onClose,
}: {
  taskId: string
  artifact: Artifact | null
  onClose: () => void
}) {
  const kind = artifact ? artifactViewKindOf(artifact.path) : 'binary'
  const overLimit = artifact != null && artifact.size_bytes > ARTIFACT_PREVIEW_LIMIT

  return (
    <Dialog
      open={artifact !== null}
      onClose={onClose}
      size="lg"
      title={<span className="break-all font-mono text-sm">{artifact?.path ?? ''}</span>}
      description={
        artifact
          ? `${kindLabel(kind)} · ${humanSize(artifact.size_bytes)}${
              kind !== 'binary' && !overLimit ? ` · 预览上限 ${formatPreviewLimit()}` : ''
            }`
          : undefined
      }
      footer={
        artifact ? (
          <div className="flex w-full items-center gap-2">
            <Badge tone="muted">{artifact.kind}</Badge>
            {artifact.attempt_no != null ? <Badge tone="muted">attempt #{artifact.attempt_no}</Badge> : null}
            {artifact.runner_seq != null ? <Badge tone="muted">seq {artifact.runner_seq}</Badge> : null}
            <span className="ml-auto">
              <ArtifactDownloadButton taskId={taskId} artifact={artifact} asLink />
            </span>
          </div>
        ) : null
      }
    >
      {artifact ? <ArtifactPreviewBody taskId={taskId} artifact={artifact} /> : null}
    </Dialog>
  )
}

function kindLabel(kind: ArtifactViewKind): string {
  switch (kind) {
    case 'json':
      return 'JSON'
    case 'csv':
      return 'CSV 表格'
    case 'html':
      return 'HTML（sandbox）'
    case 'text':
      return '文本'
    case 'binary':
      return '二进制（仅下载）'
  }
}

/** 带鉴权的下载按钮（fetch → Blob → a[download]）；`asLink` 用链接样式。 */
export function ArtifactDownloadButton({
  taskId,
  artifact,
  asLink = false,
}: {
  taskId: string
  artifact: Artifact
  asLink?: boolean
}) {
  const [downloading, setDownloading] = useState(false)
  const download = async () => {
    setDownloading(true)
    try {
      const blob = await api.downloadFile(taskId, artifact.path)
      const name = artifact.path.split('/').pop() ?? 'artifact'
      const url = URL.createObjectURL(new Blob([blob], { type: mimeOf(artifact.path) }))
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = name
      anchor.click()
      URL.revokeObjectURL(url)
    } finally {
      setDownloading(false)
    }
  }
  if (asLink) {
    return (
      <button
        type="button"
        className={buttonClassName({ size: 'sm', variant: 'ghost' })}
        data-testid="artifact-download-dialog"
        disabled={downloading}
        onClick={() => void download()}
      >
        <Download className="size-3" aria-hidden /> 下载
      </button>
    )
  }
  return (
    <Button size="sm" variant="secondary" loading={downloading} onClick={() => void download()}>
      下载文件
    </Button>
  )
}
