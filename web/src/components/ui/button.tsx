import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { Loader2 } from 'lucide-react'
import { cn } from '@/lib/utils'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'dangerGhost'
type Size = 'sm' | 'md'

/**
 * 按钮外观（对齐 shadcn Button，见 docs/research/shadcn-visual-specs.md §2.1）：
 *  - 高度：md = h-9（36）/ sm = h-8（32），**可点面积都 ≥32px**；
 *  - 字：text-sm font-medium（md）/ text-xs（sm）；
 *  - hover **只动颜色**（`/90` 或 `bg-accent`），不缩放、不加阴影（shadcn 没有 `active:scale`）；
 *  - 焦点：`ring-[3px] ring-ring/50` + `border-ring`（全站统一）；
 *  - 禁用：`pointer-events-none opacity-50`；
 *  - 图标：尺寸由组件兜底（16px），调用点只写 `data-icon="inline-start|inline-end"`。
 */
const VARIANTS: Record<Variant, string> = {
  // 主按钮用墨黑（= shadcn 的 --primary），蓝色只留给链接与焦点
  primary:
    'border border-ink bg-ink text-ink-fg shadow-xs hover:bg-ink/90 hover:border-ink/90',
  secondary:
    'border border-line bg-surface text-fg-2 shadow-xs hover:bg-line-soft hover:text-fg',
  ghost: 'border border-transparent text-fg-2 hover:bg-line-soft hover:text-fg',
  danger: 'border border-danger bg-danger text-white shadow-xs hover:bg-danger/90',
  dangerGhost: 'border border-transparent text-danger-text hover:bg-danger-weak',
}

const SIZES: Record<Size, string> = {
  sm: 'h-8 gap-1.5 px-3 text-xs has-[>svg]:px-2.5',
  md: 'h-9 gap-2 px-4 text-sm has-[>svg]:px-3',
}

/** 按钮外观（供 <a> 等非 button 元素复用，保证链接与按钮同款）。 */
export function buttonClassName({
  variant = 'secondary',
  size = 'md',
  className,
}: {
  variant?: Variant
  size?: Size
  className?: string
}): string {
  return cn(
    'inline-flex shrink-0 select-none items-center justify-center whitespace-nowrap rounded-md font-medium',
    'outline-none transition-all duration-150 ease-out',
    'focus-visible:outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50',
    'disabled:pointer-events-none disabled:opacity-50',
    '[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*="size-"])]:size-4',
    VARIANTS[variant],
    SIZES[size],
    className,
  )
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  loading?: boolean
  icon?: ReactNode
}

export function Button({
  variant = 'secondary',
  size = 'md',
  loading = false,
  icon,
  className,
  children,
  disabled,
  type = 'button',
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      disabled={disabled || loading}
      className={buttonClassName({ variant, size, className })}
      aria-busy={loading || undefined}
      {...rest}
    >
      {loading ? <Loader2 className="size-4 animate-spin" aria-hidden data-icon="inline-start" /> : icon}
      {children}
    </button>
  )
}
