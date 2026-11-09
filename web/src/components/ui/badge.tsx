import type { HTMLAttributes, ReactNode } from 'react'
import { cn } from '@/lib/utils'
import type { Tone } from '@/lib/status'

/**
 * 徽标（对齐 shadcn Badge，见 docs/research/shadcn-visual-specs.md §2.4）：
 * 高度由「text-xs + py-0.5」自然撑出来（20px），不写死；描边占位保证有无描边的徽标宽度不跳；
 * 图标 12px（`[&>svg]:size-3`），`data-icon` 只负责间距方向。
 */
// 状态徽标改成**胶囊描边 + 彩色小圆点**（shadcn `Badge variant="outline"` + 状态点）。
// 原因：一屏二十个"成功"淡绿底块会连成一片色噪，颜色就成了背景而不是信号；
// 描边 + 点保留语义（点仍按状态上色），文字统一 neutral，扫读时先看到状态词而不是色块。
const TONES: Record<Tone, string> = {
  ok: 'border-line bg-transparent text-fg',
  warn: 'border-line bg-transparent text-fg',
  err: 'border-line bg-transparent text-fg',
  info: 'border-line bg-transparent text-fg',
  primary: 'border-line bg-transparent text-fg',
  muted: 'border-line bg-transparent text-fg-2',
}

const DOTS: Record<Tone, string> = {
  ok: 'bg-success',
  warn: 'bg-warn',
  err: 'bg-danger',
  info: 'bg-primary',
  primary: 'bg-primary',
  muted: 'bg-fg-4',
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
        'inline-flex h-5 shrink-0 items-center gap-1 whitespace-nowrap rounded-full border px-2 text-xs font-medium leading-none',
        '[&>svg]:pointer-events-none [&>svg]:size-3',
        TONES[tone],
        className,
      )}
      data-tone={tone}
      {...rest}
    >
      <span className={cn('size-1.5 shrink-0 rounded-full', DOTS[tone])} aria-hidden />
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
        'inline-flex h-5 shrink-0 items-center gap-1 whitespace-nowrap rounded-full px-2 text-xs leading-none',
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
