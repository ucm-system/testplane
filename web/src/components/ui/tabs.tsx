import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

export interface TabItem<T extends string> {
  value: T
  label: ReactNode
  count?: number
}

export interface TabsProps<T extends string> {
  value: T
  onChange: (value: T) => void
  items: ReadonlyArray<TabItem<T>>
  className?: string
  ariaLabel?: string
}

/** 轻量页签（shadcn Tabs 的等价物）：受控，无动画，键盘左右切换留给 Tab 键。 */
export function Tabs<T extends string>({ value, onChange, items, className, ariaLabel }: TabsProps<T>) {
  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      className={cn('flex items-center gap-1 overflow-x-auto border-b border-line-soft', className)}
    >
      {items.map((item) => {
        const active = item.value === value
        return (
          <button
            key={item.value}
            type="button"
            role="tab"
            aria-selected={active}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(item.value)}
            className={cn(
              '-mb-px flex items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2 text-xs font-medium transition-colors duration-150',
              active
                ? 'border-primary text-primary-text'
                : 'border-transparent text-fg-3 hover:text-fg-2',
            )}
          >
            {item.label}
            {item.count === undefined ? null : (
              <span className="rounded-full bg-surface-3 px-1.5 text-2xs text-fg-3">{item.count}</span>
            )}
          </button>
        )
      })}
    </div>
  )
}
