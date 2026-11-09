import type { HTMLAttributes, ReactNode } from 'react'
import { AlertTriangle, Inbox, RefreshCw } from 'lucide-react'
import type { ApiErrorBody } from '@/api/types'
import { detailToText } from '@/lib/utils'
import { cn } from '@/lib/utils'
import { Badge } from './badge'
import { Button } from './button'

/* ---------------------------------------------------------------- 空态 */

export interface EmptyStateProps extends Omit<HTMLAttributes<HTMLDivElement>, 'title'> {
  title: ReactNode
  description?: ReactNode
  action?: ReactNode
  icon?: ReactNode
  className?: string
}

export function EmptyState({ title, description, action, icon, className, ...rest }: EmptyStateProps) {
  return (
    <div className={cn('flex flex-col items-center justify-center gap-2 px-6 py-12 text-center', className)} {...rest}>
      <span className="grid size-10 place-items-center rounded-full bg-surface-3 text-fg-3" aria-hidden>
        {icon ?? <Inbox className="size-4" />}
      </span>
      <p className="text-sm font-medium text-fg-2">{title}</p>
      {description ? <p className="max-w-md text-xs leading-[16px] text-fg-3">{description}</p> : null}
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
 * 错误展示：**code + message 必须都在**，detail 折叠成可读文本，retryable=true 才给「重试」。
 *
 * 403 / `FORBIDDEN` 是权限问题，不是网络问题：显示成「无权限：<message>」，**不给重试**（重试也没用）。
 * 401 由 `api/client.ts` 直接跳登录，不会走到这里。
 */
export function ErrorState({ error, onRetry, className, compact = false }: ErrorStateProps) {
  const detail = detailToText(error.detail)
  const forbidden = error.code === 'FORBIDDEN'
  const message = forbidden ? `无权限：${error.message}` : error.message
  const retry = forbidden ? undefined : onRetry
  return (
    <div
      role="alert"
      className={cn(
        'flex flex-col gap-2 rounded-lg border px-4 py-3',
        forbidden ? 'border-warn/30 bg-warn-weak' : 'border-danger/25 bg-danger-weak',
        compact ? 'text-xs' : 'text-sm',
        className,
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <AlertTriangle className={cn('size-4 shrink-0', forbidden ? 'text-warn-text' : 'text-danger-text')} aria-hidden />
        <Badge tone={forbidden ? 'warn' : 'err'} className="font-mono">
          {error.code}
        </Badge>
        <span className="min-w-0 flex-1 text-fg">{message}</span>
        {forbidden ? (
          <Badge tone="muted">无权限</Badge>
        ) : error.retryable ? (
          <Badge tone="warn">可重试</Badge>
        ) : (
          <Badge tone="muted">不可重试</Badge>
        )}
        {retry ? (
          <Button size="sm" variant="secondary" icon={<RefreshCw className="size-4" aria-hidden />} onClick={retry}>
            重试
          </Button>
        ) : null}
      </div>
      {error.trace_id ? <p className="font-mono text-2xs text-fg-3">trace_id: {error.trace_id}</p> : null}
      {detail && !compact ? (
        <details className="group">
          <summary className="cursor-pointer text-xs text-fg-3 hover:text-fg-2">查看 detail</summary>
          <pre className="mt-1 max-h-48 overflow-auto rounded-sm bg-surface px-2 py-1.5 font-mono text-2xs leading-[16px] text-fg-2">
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
  /** 错误类横幅传 `alert`（断言式播报）；纯提示用默认 `status` */
  role?: 'status' | 'alert'
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

export function Banner({ tone = 'warn', role = 'status', title, children, actions, className }: BannerProps) {
  return (
    <div
      role={role}
      className={cn('flex flex-wrap items-start gap-x-3 gap-y-1 rounded-lg border px-4 py-3', BANNER_TONE[tone], className)}
    >
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-fg">{title}</p>
        {children ? <p className="mt-1 text-xs leading-[16px] text-fg-2">{children}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  )
}

/* ---------------------------------------------------------------- 加载 */

export function LoadingBlock({ label = '加载中…' }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 px-4 py-10 text-xs text-fg-3" role="status">
      <RefreshCw className="size-4 animate-spin" aria-hidden />
      {label}
    </div>
  )
}
