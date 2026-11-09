import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

export interface SegmentedOption<T extends string> {
  value: T
  label: ReactNode
}

export interface SegmentedProps<T extends string> {
  value: T
  onChange: (value: T) => void
  options: ReadonlyArray<SegmentedOption<T>>
  className?: string
  ariaLabel?: string
  disabled?: boolean
}

/**
 * 分段控件（原型 .fde-seg）：轨道灰底 + 选中白底，用于「部署 / 不部署」这类二态切换。
 *
 * 视觉对齐 shadcn Tabs 的"药丸在灰轨道里"：轨道 `rounded-lg bg-surface-3 p-1`，
 * 选中项 `rounded-md bg-surface shadow-xs`，高度 32px（可点面积够大），
 * 焦点 `ring-[3px] ring-ring/50`。
 */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  className,
  ariaLabel,
  disabled,
}: SegmentedProps<T>) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={cn('inline-flex rounded-lg bg-surface-3 p-1', disabled && 'opacity-50', className)}
    >
      {options.map((option) => {
        const active = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            disabled={disabled}
            aria-pressed={active}
            onClick={() => onChange(option.value)}
            className={cn(
              'h-8 whitespace-nowrap rounded-md px-3 text-xs transition-colors duration-150',
              'outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
              active
                ? 'bg-surface font-medium text-fg shadow-xs'
                : 'text-fg-2 hover:text-fg disabled:cursor-not-allowed',
            )}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}
