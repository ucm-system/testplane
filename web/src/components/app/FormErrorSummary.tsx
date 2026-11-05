import { useEffect, useRef, useState } from 'react'
import { Check, Copy, Crosshair } from 'lucide-react'
import type { ApiErrorBody, ValidationIssue } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

/**
 * 表单内错误摘要（用户反馈 P0：提交失败"根本看不到为什么"）。
 *
 * 三条硬约束：
 *  1. **不自动消失**：它是页面里的一个块，不是 toast；
 *  2. **自己滚进视口并拿焦点**：`scrollIntoView({block:'center'})` + 焦点移到摘要，
 *     否则用户停在底部「提交」按钮处，错误渲染在文档顶部 279px 之外（实测过）；
 *  3. 语义完整：`role="alert"` + `aria-live="assertive"`，读屏也会播报。
 *
 * 内容：`code` + `message` + `retryable`；`detail.issues[]` 逐条列 `path → message`（可点跳字段）；
 * `trace_id` 一键复制（排障要它）。
 */

export interface FormErrorSummaryProps {
  error?: ApiErrorBody | null
  /** dry_run 的 issues（与 error.detail.issues 同构） */
  issues?: ValidationIssue[]
  /** 点某条 issue 时跳到对应字段 */
  onJumpTo?: (path: string) => void
  className?: string
}

function issuesFromError(error: ApiErrorBody | null | undefined): ValidationIssue[] {
  const detail = error?.detail as { issues?: unknown } | null | undefined
  return Array.isArray(detail?.issues) ? (detail!.issues as ValidationIssue[]) : []
}

export function FormErrorSummary({ error, issues = [], onJumpTo, className }: FormErrorSummaryProps) {
  const ref = useRef<HTMLDivElement>(null)
  const [copied, setCopied] = useState(false)
  const list = issues.length > 0 ? issues : issuesFromError(error)

  // 出现即滚到视口中间并聚焦（用户视线在底部按钮上，必须主动拉回）
  useEffect(() => {
    if (!error && issues.length === 0) return
    const node = ref.current
    if (!node) return
    node.scrollIntoView({ block: 'center', behavior: 'auto' })
    // 等滚动开始后再聚焦，避免焦点把滚动打断
    const timer = window.setTimeout(() => node.focus({ preventScroll: true }), 120)
    return () => window.clearTimeout(timer)
  }, [error, issues])

  if (!error && issues.length === 0) return null

  const copyTrace = async () => {
    if (!error?.trace_id) return
    try {
      await navigator.clipboard.writeText(error.trace_id)
    } catch {
      // 剪贴板不可用（http / 权限）时退化成选中文本
      const range = document.createRange()
      const target = ref.current?.querySelector('[data-trace-id]')
      if (target) {
        range.selectNodeContents(target)
        const selection = window.getSelection()
        selection?.removeAllRanges()
        selection?.addRange(range)
      }
    }
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div
      ref={ref}
      role="alert"
      aria-live="assertive"
      aria-atomic="false"
      tabIndex={-1}
      data-testid="form-error-summary"
      className={cn(
        'flex flex-col gap-3 rounded-lg border border-danger/30 bg-danger-weak px-5 py-4 shadow-card outline-none',
        'focus-visible:ring-2 focus-visible:ring-danger/40',
        className,
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone="err" className="font-mono">
          {error?.code ?? (issues[0]?.code || 'VALIDATION_ERROR')}
        </Badge>
        <span className="text-sm font-medium text-fg">
          {error?.message ?? `提交内容未通过校验（${list.length} 个问题）`}
        </span>
        {error ? <Badge tone={error.retryable ? 'warn' : 'muted'}>{error.retryable ? '可重试' : '不可重试'}</Badge> : null}
        {error?.trace_id ? (
          <span className="ml-auto flex items-center gap-1.5">
            <span className="text-2xs text-fg-3">trace_id</span>
            <code data-trace-id className="font-mono text-2xs text-fg-2">
              {error.trace_id}
            </code>
            <Button
              size="sm"
              variant="ghost"
              data-testid="copy-trace-id"
              title="复制 trace_id"
              icon={copied ? <Check className="size-3" aria-hidden /> : <Copy className="size-3" aria-hidden />}
              onClick={() => void copyTrace()}
            >
              {copied ? '已复制' : '复制'}
            </Button>
          </span>
        ) : null}
      </div>

      {list.length > 0 ? (
        <ul className="flex flex-col divide-y divide-danger/15 rounded-md border border-danger/20 bg-surface/70">
          {list.map((issue, index) => (
            <li key={`${issue.path}-${index}`} className="flex flex-wrap items-start gap-2 px-3 py-2">
              <code className="shrink-0 rounded-sm bg-danger-chip px-1.5 py-0.5 font-mono text-2xs text-danger-text">
                {issue.path || '(表单)'}
              </code>
              <span className="min-w-0 flex-1 text-xs text-fg">{issue.message}</span>
              {issue.code ? <Badge tone="muted" className="font-mono">{issue.code}</Badge> : null}
              {onJumpTo && issue.path ? (
                <Button
                  size="sm"
                  variant="ghost"
                  data-testid="jump-to-field"
                  title="跳到这个字段"
                  icon={<Crosshair className="size-3" aria-hidden />}
                  onClick={() => onJumpTo(issue.path)}
                >
                  定位
                </Button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}

      <p className="text-2xs leading-relaxed text-fg-3">
        这个摘要不会自动消失。改完再点「校验」或「提交」；如果一连串问题只报了一条，先修这条再看下一轮。
      </p>
    </div>
  )
}
