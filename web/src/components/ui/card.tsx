import type { HTMLAttributes, ReactNode } from 'react'
import { cn } from '@/lib/utils'
import type { Tone } from '@/lib/status'

/** 卡片标题前的色点：只给有语义的卡片加（原型 .fde-card-hd.hg-*::before） */
const DOT: Record<Tone, string> = {
  ok: 'bg-success',
  warn: 'bg-warn',
  err: 'bg-danger',
  info: 'bg-primary',
  primary: 'bg-primary',
  muted: 'bg-fg-3',
}

/** 卡头浅染底（原型 `.fde-card-hd.hg-*` 的 6–8% 语义色），让版面有冷暖而不是一片白 */
// 卡头不染底色（P0-2）：层次靠圆点 + 字重 + 一条发丝分隔线，而不是浅色底块。
// 之前薄荷绿/浅蓝/浅琥珀的卡头会把"层次信号"变成"颜色信号"，整页看着花。
const HEADER_TONE: Record<Tone, string> = {
  ok: 'bg-transparent',
  warn: 'bg-transparent',
  err: 'bg-transparent',
  info: 'bg-transparent',
  primary: 'bg-transparent',
  muted: 'bg-transparent',
}

/**
 * 卡片（对齐 shadcn Card，见 docs/research/shadcn-visual-specs.md §2.2）：
 * `rounded-xl`(14) + 1px `--border` + `shadow-xs`（只有 1px 的浮起量）。
 * overflow-hidden：染底卡头 / 表头底色 / 行 hover 都是直角矩形，必须裁在圆角里（T39）。
 */
export function Card({ className, ...rest }: HTMLAttributes<HTMLElement>) {
  return (
    <section
      className={cn(
        'min-w-0 overflow-hidden rounded-xl border border-line bg-surface text-fg shadow-xs',
        'transition-shadow duration-150 ease-out',
        className,
      )}
      {...rest}
    />
  )
}

export interface CardHeaderProps {
  title: ReactNode
  /** 标题右侧的补充说明（弱化文字） */
  sub?: ReactNode
  /** 标题前的语义色点 */
  tone?: Tone
  actions?: ReactNode
  className?: string
  toneClass?: string
  id?: string
}

export function CardHeader({ title, sub, tone, actions, className, id }: CardHeaderProps) {
  return (
    <header
      className={cn(
        'flex flex-wrap items-center gap-x-2 gap-y-2 border-b border-line-soft px-6 py-4',
        tone ? HEADER_TONE[tone] : undefined,
        className,
      )}
    >
      <h2 id={id} className="flex items-center text-base font-semibold leading-none text-fg">
        {tone ? <span className={cn('mr-2 inline-block size-1.5 rounded-full', DOT[tone])} aria-hidden /> : null}
        {title}
      </h2>
      {sub ? <span className="text-xs leading-snug text-fg-3">{sub}</span> : null}
      {actions ? <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div> : null}
    </header>
  )
}

export function CardBody({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('px-6 py-4', className)} {...rest} />
}

export function CardFooter({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn('flex flex-wrap items-center gap-2 border-t border-line-soft px-6 py-3 text-xs text-fg-3', className)}
      {...rest}
    />
  )
}
