import { useId, useMemo, useState } from 'react'
import * as Popover from '@radix-ui/react-popover'
import { Check, Filter, Search } from 'lucide-react'
import type { FacetBucket } from '@/api/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

/**
 * Excel 式表头列筛选（列头右侧的漏斗按钮 → 弹出面板）。
 *
 * 交互（与 Excel / Airtable / Notion 一致，用户点名要的那套）：
 *  - 搜索框：候选值多时过滤（只在面板内过滤，不影响已勾选的）；
 *  - 复选列表：`值 + 计数`，**多选**，勾选即进入草稿态（不立刻生效）；
 *  - 底部「清除本列」与「应用」：应用才写回 URL / 触发请求；
 *  - 无障碍：`aria-expanded` / `aria-controls`、Esc 关闭（Radix 自带）、Tab 在面板内走、
 *    关闭后焦点回到漏斗按钮。
 *
 * 计数来自 `GET /runs/facets`（facet 语义：自己那维不参与自己的计数），
 * 所以这里**不做任何本地计数**，只渲染后端给的数字。
 */
export interface FacetFilterProps {
  /** 列名（用于 aria-label 与面板标题，例如「提交人」） */
  label: string
  /** 当前生效的选中值（来自 URL） */
  selected: string[]
  /** 候选值与计数（来自 /runs/facets） */
  buckets: FacetBucket[]
  /** 值 → 展示文案（默认原样） */
  renderValue?: (value: string) => string
  /** 「应用」时回调；空数组表示清除本列 */
  onApply: (values: string[]) => void
  /** 面板宽度 class */
  widthClass?: string
}

export function FacetFilter({ label, selected, buckets, renderValue, onApply, widthClass }: FacetFilterProps) {
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<string[]>(selected)
  const [keyword, setKeyword] = useState('')
  const contentId = useId()

  // 每次打开都以「当前生效值」为草稿起点（取消的改动不残留）
  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (next) {
      setDraft(selected)
      setKeyword('')
    }
  }

  const visible = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    return kw ? buckets.filter((bucket) => bucket.value.toLowerCase().includes(kw)) : buckets
  }, [buckets, keyword])

  const toggle = (value: string) =>
    setDraft((prev) => (prev.includes(value) ? prev.filter((item) => item !== value) : [...prev, value]))

  const active = selected.length > 0

  return (
    <Popover.Root open={open} onOpenChange={handleOpenChange}>
      <Popover.Trigger asChild>
        <button
          type="button"
          data-testid={`facet-trigger-${label}`}
          aria-label={`筛选${label}${active ? `（已选 ${selected.length} 项）` : ''}`}
          aria-expanded={open}
          aria-controls={open ? contentId : undefined}
          title={active ? `${label}：已筛选 ${selected.length} 项` : `按${label}筛选`}
          className={cn(
            'ml-1 inline-flex size-5 shrink-0 items-center justify-center rounded-sm align-middle',
            'transition-colors duration-150 hover:bg-line-soft focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30',
            active ? 'text-primary' : 'text-fg-3',
          )}
        >
          <Filter className={cn('size-3', active && 'fill-current')} aria-hidden />
        </button>
      </Popover.Trigger>

      <Popover.Portal>
        <Popover.Content
          id={contentId}
          data-testid={`facet-panel-${label}`}
          sideOffset={6}
          align="start"
          className={cn(
            'z-50 flex w-64 flex-col overflow-hidden rounded-lg border border-line bg-surface shadow-pop',
            widthClass,
          )}
          onOpenAutoFocus={(event) => {
            // 打开就聚焦搜索框（Excel 也是这样：一开就能敲）
            event.preventDefault()
            const input = document.getElementById(`${contentId}-search`) as HTMLInputElement | null
            input?.focus()
          }}
        >
          <div className="border-b border-line-soft p-2">
            <div className="relative">
              <Search className="pointer-events-none absolute left-2 top-1/2 size-3 -translate-y-1/2 text-fg-3" aria-hidden />
              <Input
                id={`${contentId}-search`}
                value={keyword}
                onChange={(event) => setKeyword(event.target.value)}
                aria-label={`搜索${label}候选值`}
                placeholder="搜索值…"
                className="h-8 pl-7 text-2xs"
              />
            </div>
          </div>

          <ul role="group" aria-label={`${label}候选值`} className="max-h-56 overflow-y-auto p-1">
            {visible.length === 0 ? (
              <li className="px-2 py-3 text-2xs text-fg-3">没有匹配的值</li>
            ) : (
              visible.map((bucket) => {
                const checked = draft.includes(bucket.value)
                return (
                  <li key={bucket.value}>
                    <label
                      data-testid={`facet-option-${label}-${bucket.value}`}
                      className="flex cursor-pointer items-center gap-2 rounded-sm px-2 py-1.5 text-2xs hover:bg-surface-2"
                    >
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={() => toggle(bucket.value)}
                        className="size-3.5 shrink-0 accent-primary"
                        aria-label={`${renderValue?.(bucket.value) ?? bucket.value}，${bucket.count} 条`}
                      />
                      <span className={cn('min-w-0 flex-1 truncate', checked ? 'text-fg' : 'text-fg-2')}>
                        {renderValue?.(bucket.value) ?? bucket.value}
                      </span>
                      <span className="shrink-0 tabular-nums text-fg-3">{bucket.count}</span>
                      {checked ? <Check className="size-3 shrink-0 text-primary" aria-hidden /> : null}
                    </label>
                  </li>
                )
              })
            )}
          </ul>

          <div className="flex items-center gap-2 border-t border-line-soft bg-surface-2 px-2 py-2">
            <Button
              size="sm"
              variant="ghost"
              data-testid={`facet-clear-${label}`}
              onClick={() => setDraft([])}
              disabled={draft.length === 0}
            >
              清除本列
            </Button>
            <Button
              size="sm"
              variant="primary"
              className="ml-auto"
              data-testid={`facet-apply-${label}`}
              onClick={() => {
                onApply(draft)
                setOpen(false)
              }}
            >
              应用{draft.length > 0 ? `（${draft.length}）` : ''}
            </Button>
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
