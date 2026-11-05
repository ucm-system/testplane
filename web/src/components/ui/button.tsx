import type { ButtonHTMLAttributes, ReactNode } from 'react'
import { Loader2 } from 'lucide-react'
import { cn } from '@/lib/utils'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'dangerGhost'
type Size = 'sm' | 'md'

const VARIANTS: Record<Variant, string> = {
  // 主按钮用墨黑（holy-grail.css §主按钮），蓝色只留给链接与焦点
  primary:
    'border border-ink bg-ink text-white hover:shadow-pop active:scale-[0.98] disabled:hover:shadow-none',
  secondary: 'border border-line bg-surface text-fg-2 hover:bg-surface-2 hover:text-fg',
  ghost: 'border border-transparent text-fg-2 hover:bg-line-soft hover:text-fg',
  danger: 'border border-danger bg-danger text-white hover:brightness-95',
  dangerGhost: 'border border-transparent text-danger-text hover:bg-danger-weak',
}

const SIZES: Record<Size, string> = {
  sm: 'h-[30px] px-2.5 text-2xs gap-1',
  md: 'h-9 px-3.5 text-xs gap-1.5',
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
    'inline-flex select-none items-center justify-center whitespace-nowrap rounded-md font-medium',
    'transition-[background-color,box-shadow,border-color,color,transform] duration-150 ease-out',
    'focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/60',
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
      className={cn(buttonClassName({ variant, size, className }), 'disabled:cursor-not-allowed disabled:opacity-45')}
      aria-busy={loading || undefined}
      {...rest}
    >
      {loading ? <Loader2 className="size-3.5 animate-spin" aria-hidden /> : icon}
      {children}
    </button>
  )
}
