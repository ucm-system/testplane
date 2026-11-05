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

/** 分段控件（原型 .fde-seg）：轨道灰底 + 选中白底，用于「部署 / 不部署」这类二态切换。 */
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
      className={cn('inline-flex rounded-md bg-surface-3 p-0.5', disabled && 'opacity-60', className)}
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
              'h-7 whitespace-nowrap rounded-sm px-3 text-xs transition-colors duration-150',
              active
                ? 'bg-surface font-medium text-primary-text shadow-card'
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
