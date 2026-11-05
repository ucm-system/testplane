import type { ReactNode } from 'react'
import { AlertTriangle, Inbox, RefreshCw } from 'lucide-react'
import type { ApiErrorBody } from '@/api/types'
import { detailToText } from '@/lib/utils'
import { cn } from '@/lib/utils'
import { Badge } from './badge'
import { Button } from './button'

/* ---------------------------------------------------------------- 空态 */

export interface EmptyStateProps {
  title: ReactNode
  description?: ReactNode
  action?: ReactNode
  icon?: ReactNode
  className?: string
}

export function EmptyState({ title, description, action, icon, className }: EmptyStateProps) {
  return (
    <div className={cn('flex flex-col items-center justify-center gap-2 px-6 py-12 text-center', className)}>
      <span className="grid size-9 place-items-center rounded-full bg-surface-3 text-fg-3" aria-hidden>
        {icon ?? <Inbox className="size-4" />}
      </span>
      <p className="text-sm font-medium text-fg-2">{title}</p>
      {description ? <p className="max-w-md text-xs leading-relaxed text-fg-3">{description}</p> : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  )
}

/* ---------------------------------------------------------------- 错误提示 */

export interface ErrorStateProps {
  error: ApiErrorBody
  onRetry?: () => void
  className?: string
  compact?: boolean
}

/**
 * 错误展示：**code + message 必须都在**（票 #5 的验收项），
 * detail 折叠成可读文本，retryable=true 才给「重试」。
 */
export function ErrorState({ error, onRetry, className, compact = false }: ErrorStateProps) {
  const detail = detailToText(error.detail)
  return (
    <div
      role="alert"
      className={cn(
        'flex flex-col gap-2 rounded-lg border border-danger/25 bg-danger-weak px-4 py-3',
        compact ? 'text-xs' : 'text-sm',
        className,
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <AlertTriangle className="size-4 shrink-0 text-danger" aria-hidden />
        <Badge tone="err" className="font-mono">
          {error.code}
        </Badge>
        <span className="min-w-0 flex-1 text-fg">{error.message}</span>
        {error.retryable ? <Badge tone="warn">可重试</Badge> : <Badge tone="muted">不可重试</Badge>}
        {onRetry ? (
          <Button size="sm" variant="secondary" icon={<RefreshCw className="size-3" aria-hidden />} onClick={onRetry}>
            重试
          </Button>
        ) : null}
      </div>
      {error.trace_id ? <p className="font-mono text-2xs text-fg-3">trace_id: {error.trace_id}</p> : null}
      {detail && !compact ? (
        <details className="group">
          <summary className="cursor-pointer text-2xs text-fg-3 hover:text-fg-2">查看 detail</summary>
          <pre className="mt-1 max-h-48 overflow-auto rounded-sm bg-surface/70 px-2 py-1.5 font-mono text-2xs leading-relaxed text-fg-2">
            {detail}
          </pre>
        </details>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------- 横幅 */

export interface BannerProps {
  tone?: 'err' | 'warn' | 'info'
  title: ReactNode
  children?: ReactNode
  actions?: ReactNode
  className?: string
}

const BANNER_TONE = {
  err: 'border-danger/25 bg-danger-weak',
  warn: 'border-warn/30 bg-warn-weak',
  info: 'border-primary/20 bg-primary-weak',
} as const

export function Banner({ tone = 'warn', title, children, actions, className }: BannerProps) {
  return (
    <div
      role="status"
      className={cn('flex flex-wrap items-start gap-x-3 gap-y-1 rounded-lg border px-4 py-2.5', BANNER_TONE[tone], className)}
    >
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium text-fg">{title}</p>
        {children ? <p className="mt-0.5 text-2xs leading-relaxed text-fg-2">{children}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  )
}

/* ---------------------------------------------------------------- 加载 */

export function LoadingBlock({ label = '加载中…' }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 px-4 py-10 text-xs text-fg-3" role="status">
      <RefreshCw className="size-3.5 animate-spin" aria-hidden />
      {label}
    </div>
  )
}
