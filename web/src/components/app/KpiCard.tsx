import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

/**
 * KPI 数值色：**用 `-text` 档**（700 级别），保证 30px 大字在 3:1 以上、说明文字在 4.5:1 以上。
 * 原来看板上的 `text-success`(#10b981) 白底只有 2.54:1，大字号也不达标（审计 P0）。
 */
// KPI 大数字统一走**近黑**，颜色只留给左上角小圆点（审计 P0-3：一排彩色大字会把"状态色"变成"装饰色"，
// 而且蓝/绿/琥珀三个数字同屏时，读者会误以为它们在表达不同优先级）。
const VALUE_TONE = {
  blue: 'text-fg',
  green: 'text-fg',
  amber: 'text-fg',
  red: 'text-fg',
  ink: 'text-fg',
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

/**
 * 看板 KPI 卡（原型 .stat-value 那一套：小方点 + 标签 + 大字 + 说明）。
 * 对齐 shadcn Card：`rounded-xl` + `border-line` + `shadow-xs`；**hover 不位移、不加阴影**
 * （原来 `hover:-translate-y-0.5 hover:shadow-pop` 让整排卡片在鼠标掠过时抖）。
 */
export function KpiCard({ label, value, hint, tone = 'muted', className }: KpiCardProps) {
  return (
    <div
      className={cn('group min-w-0 rounded-xl border border-line bg-surface p-5 shadow-xs', className)}
    >
      <div className="flex items-center gap-2">
        <span className={cn('inline-block size-1.5 rounded-sm', DOT_TONE[tone])} aria-hidden />
        <span className="text-sm font-medium text-fg-2">{label}</span>
      </div>
      <p className={cn('mt-2 text-3xl font-semibold leading-none tabular-nums tracking-tight', VALUE_TONE[tone])}>
        {value}
      </p>
      {hint ? <p className="mt-2 text-xs leading-snug text-fg-3">{hint}</p> : null}
    </div>
  )
}
