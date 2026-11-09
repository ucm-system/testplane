import { useId, useRef, type KeyboardEvent, type ReactNode } from 'react'
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
  /** 面板 id 前缀：与页面里的 `role="tabpanel"` 用 `aria-controls` 关联（不传则用 useId） */
  idBase?: string
}

/**
 * 轻量页签（shadcn Tabs 的等价物）：受控、无位移动画。
 *
 * 视觉（见 docs/research/shadcn-visual-specs.md §2.5）：沿用本项目的**下划线式**
 * （整条 tablist 有 border-b），但把尺寸/字重/焦点环对齐 shadcn：
 * `h-9` 行高、`text-sm font-medium`、hover 只变字色、选中项下划线 2px、焦点 `ring-[3px] ring-ring/50`。
 *
 * 键盘可达性（评审 #13）：roving tabindex + `←/→/Home/End` 切页签，Tab 键只落在当前页签上；
 * 页签用 `role="tab"` + `aria-selected`，面板侧用 `role="tabpanel"` + `aria-labelledby`（见 `tabPanelProps`）。
 * `aria-controls` **只给活动页签**：面板按需挂载，非活动页签指向不存在的 id 会成为悬空引用。
 */
export function Tabs<T extends string>({ value, onChange, items, className, ariaLabel, idBase }: TabsProps<T>) {
  const autoId = useId().replace(/[:]/g, '')
  const base = idBase ?? `tabs-${autoId}`
  const listRef = useRef<HTMLDivElement>(null)

  const index = Math.max(
    0,
    items.findIndex((item) => item.value === value),
  )

  const activate = (nextIndex: number) => {
    const clamped = (nextIndex + items.length) % items.length
    const next = items[clamped]
    if (!next) return
    onChange(next.value)
    // 焦点跟着选中项走（roving tabindex）
    const buttons = listRef.current?.querySelectorAll<HTMLButtonElement>('[role="tab"]')
    buttons?.[clamped]?.focus()
  }

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    switch (event.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        event.preventDefault()
        activate(index + 1)
        break
      case 'ArrowLeft':
      case 'ArrowUp':
        event.preventDefault()
        activate(index - 1)
        break
      case 'Home':
        event.preventDefault()
        activate(0)
        break
      case 'End':
        event.preventDefault()
        activate(items.length - 1)
        break
      default:
        break
    }
  }

  return (
    <div
      ref={listRef}
      role="tablist"
      aria-label={ariaLabel}
      aria-orientation="horizontal"
      onKeyDown={onKeyDown}
      className={cn('flex items-center gap-1 overflow-x-auto border-b border-line-soft', className)}
    >
      {items.map((item) => {
        const active = item.value === value
        return (
          <button
            key={item.value}
            id={`${base}-tab-${item.value}`}
            type="button"
            role="tab"
            aria-selected={active}
            // 只有活动页签的面板真的在 DOM 里；非活动页签不写 aria-controls，避免指向不存在的元素
            aria-controls={active ? `${base}-panel-${item.value}` : undefined}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(item.value)}
            className={cn(
              '-mb-px flex h-9 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 text-sm font-medium transition-colors duration-150',
              'outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50',
              active
                ? 'border-primary text-fg'
                : 'border-transparent text-fg-2 hover:border-line hover:text-fg',
            )}
          >
            {item.label}
            {item.count === undefined ? null : (
              <span className="rounded-full bg-surface-3 px-1.5 text-2xs tabular-nums text-fg-3">{item.count}</span>
            )}
          </button>
        )
      })}
    </div>
  )
}

/** 面板属性：`<section {...tabPanelProps('admin', tab)}>`，与上面的 Tabs 成对使用。 */
export function tabPanelProps(base: string, value: string) {
  return {
    id: `${base}-panel-${value}`,
    role: 'tabpanel' as const,
    'aria-labelledby': `${base}-tab-${value}`,
    tabIndex: 0,
  }
}
