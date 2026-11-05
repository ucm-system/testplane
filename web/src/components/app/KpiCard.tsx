import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

const VALUE_TONE = {
  blue: 'text-primary',
  green: 'text-success',
  amber: 'text-warn',
  red: 'text-danger',
  ink: 'text-ink',
  muted: 'text-fg',
} as const

const DOT_TONE = {
  blue: 'bg-primary',
  green: 'bg-success',
  amber: 'bg-warn',
  red: 'bg-danger',
  ink: 'bg-ink',
  muted: 'bg-fg-3',
} as const

export interface KpiCardProps {
  label: ReactNode
  value: ReactNode
  hint?: ReactNode
  tone?: keyof typeof VALUE_TONE
  className?: string
}

/** 看板 KPI 卡（原型 .stat-value 那一套：小方点 + 标签 + 大字 + 说明）。 */
export function KpiCard({ label, value, hint, tone = 'muted', className }: KpiCardProps) {
  return (
    <div
      className={cn(
        'group min-w-0 rounded-lg border border-line-soft bg-surface p-5 shadow-card transition-all duration-150',
        'hover:-translate-y-0.5 hover:shadow-pop',
        className,
      )}
    >
      <div className="flex items-center gap-1.5">
        <span className={cn('inline-block size-1.5 rounded-sm', DOT_TONE[tone])} aria-hidden />
        <span className="text-xs text-fg-2">{label}</span>
      </div>
      <p className={cn('mt-2 text-2xl font-semibold tabular-nums leading-none tracking-tight', VALUE_TONE[tone])}>
        {value}
      </p>
      {hint ? <p className="mt-1.5 text-2xs leading-snug text-fg-3">{hint}</p> : null}
    </div>
  )
}
