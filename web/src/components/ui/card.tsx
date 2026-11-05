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

export function Card({ className, ...rest }: HTMLAttributes<HTMLElement>) {
  return (
    <section
      className={cn(
        'min-w-0 rounded-lg border border-line-soft bg-surface shadow-card',
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
        'flex flex-wrap items-center gap-x-2 gap-y-2 border-b border-line-soft px-5 py-3.5',
        className,
      )}
    >
      <h2 id={id} className="flex items-center text-sm font-semibold leading-tight text-fg">
        {tone ? <span className={cn('mr-2 inline-block size-1.5 rounded-full', DOT[tone])} aria-hidden /> : null}
        {title}
      </h2>
      {sub ? <span className="text-2xs leading-snug text-fg-3">{sub}</span> : null}
      {actions ? <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div> : null}
    </header>
  )
}

export function CardBody({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('px-5 py-4', className)} {...rest} />
}

export function CardFooter({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn('flex flex-wrap items-center gap-2 border-t border-line-soft px-5 py-3 text-2xs text-fg-3', className)}
      {...rest}
    />
  )
}
