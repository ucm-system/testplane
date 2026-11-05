import type { HTMLAttributes, ReactNode } from 'react'
import { cn } from '@/lib/utils'
import type { Tone } from '@/lib/status'

const TONES: Record<Tone, string> = {
  ok: 'border-success/25 bg-success-chip text-success-text',
  warn: 'border-warn/30 bg-warn-chip text-warn-text',
  err: 'border-danger/25 bg-danger-chip text-danger-text',
  info: 'border-primary/25 bg-info-chip text-info-text',
  primary: 'border-primary/25 bg-primary-chip text-primary-text',
  muted: 'border-line bg-surface-3 text-fg-2',
}

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  tone?: Tone
  children: ReactNode
}

/** 状态徽标：文字 + 色，不靠颜色单独表达状态（design §6 状态表达）。 */
export function Badge({ tone = 'muted', className, children, ...rest }: BadgeProps) {
  return (
    <span
      className={cn(
        'inline-flex h-[22px] shrink-0 items-center rounded-full border px-2 text-2xs font-medium leading-none',
        TONES[tone],
        className,
      )}
      {...rest}
    >
      {children}
    </span>
  )
}

export interface TagProps extends HTMLAttributes<HTMLSpanElement> {
  /** 机器标识符（genet / tester 名 / secret 引用）：描边 + 等宽 */
  mono?: boolean
  children: ReactNode
}

/** 中性胶囊标签（原型 .fde-tag）：区别于 Badge 的语义色。 */
export function Tag({ mono = false, className, children, ...rest }: TagProps) {
  return (
    <span
      className={cn(
        'inline-flex h-5 shrink-0 items-center rounded-full px-2 text-2xs leading-none',
        mono
          ? 'border border-line-strong bg-transparent font-mono text-2xs text-fg-2'
          : 'bg-line-soft font-medium text-fg-2',
        className,
      )}
      {...rest}
    >
      {children}
    </span>
  )
}
