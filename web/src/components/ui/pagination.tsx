import { ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { cn } from '@/lib/utils'

/**
 * 服务端分页栏（真分页，不是「加载更多」）。
 *
 * 左：`共 N 条` + 每页条数；右：首页 / 上一页 / 页码（最多 7 个，含省略号）/ 下一页 / 末页。
 * 页码窗口算法与 Excel / GitHub 一致：当前页尽量居中，两端固定露出首末页。
 */

export const PAGE_SIZES = [10, 20, 50] as const

/** 页码窗口：最多 7 格，超出用 '…' 占位 */
export function pageWindow(current: number, totalPages: number, max = 7): Array<number | 'gap'> {
  if (totalPages <= max) return Array.from({ length: totalPages }, (_, i) => i + 1)

  const edge = 1 // 首末页始终露出
  const innerSlots = max - edge * 2 - 2 // 两个省略号的额度
  let start = Math.max(2, current - Math.floor(innerSlots / 2))
  let end = Math.min(totalPages - 1, start + innerSlots - 1)
  start = Math.max(2, Math.min(start, totalPages - 1 - innerSlots + 1))

  const pages: Array<number | 'gap'> = [1]
  if (start > 2) pages.push('gap')
  for (let page = start; page <= end; page += 1) pages.push(page)
  if (end < totalPages - 1) pages.push('gap')
  pages.push(totalPages)
  return pages
}

export interface PaginationProps {
  /** 满足当前筛选的总条数（服务端 total） */
  total: number
  page: number
  pageSize: number
  /** 改页码（1 起） */
  onPageChange: (page: number) => void
  /** 改每页条数（改完回到第 1 页） */
  onPageSizeChange: (size: number) => void
  /** 空结果时是否禁用翻页 */
  disabled?: boolean
  className?: string
}

export function Pagination({
  total,
  page,
  pageSize,
  onPageChange,
  onPageSizeChange,
  disabled,
  className,
}: PaginationProps) {
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const pages = pageWindow(Math.min(page, totalPages), totalPages)
  const canPrev = page > 1 && !disabled
  const canNext = page < totalPages && !disabled

  return (
    <div
      data-testid="pagination"
      className={cn(
        'flex flex-wrap items-center gap-3 border-t border-line-soft px-5 py-3 text-2xs text-fg-3',
        className,
      )}
    >
      <span data-testid="pagination-total">共 {total} 条</span>
      <label className="flex items-center gap-1.5">
        <span>每页</span>
        <Select
          aria-label="每页条数"
          data-testid="page-size"
          value={String(pageSize)}
          onChange={(event) => onPageSizeChange(Number(event.target.value))}
          className="h-7 w-[4.5rem] text-2xs"
        >
          {PAGE_SIZES.map((size) => (
            <option key={size} value={size}>
              {size}
            </option>
          ))}
        </Select>
        <span>条</span>
      </label>
      <span data-testid="pagination-page" className="ml-auto tabular-nums">
        第 {Math.min(page, totalPages)} / {totalPages} 页
      </span>

      <nav aria-label="分页" className="flex items-center gap-1">
        <Button
          size="sm"
          variant="ghost"
          data-testid="page-first"
          aria-label="首页"
          disabled={!canPrev}
          onClick={() => onPageChange(1)}
          icon={<ChevronsLeft className="size-3" aria-hidden />}
        >
          <span className="sr-only">首页</span>
        </Button>
        <Button
          size="sm"
          variant="ghost"
          data-testid="page-prev"
          aria-label="上一页"
          disabled={!canPrev}
          onClick={() => onPageChange(page - 1)}
          icon={<ChevronLeft className="size-3" aria-hidden />}
        >
          <span className="sr-only">上一页</span>
        </Button>

        {pages.map((item, index) =>
          item === 'gap' ? (
            <span key={`gap-${index}`} className="px-1 text-fg-3" aria-hidden>
              …
            </span>
          ) : (
            <button
              key={item}
              type="button"
              data-testid={`page-${item}`}
              aria-label={`第 ${item} 页`}
              aria-current={item === page ? 'page' : undefined}
              disabled={disabled}
              onClick={() => onPageChange(item)}
              className={cn(
                'h-7 min-w-7 rounded-sm px-1.5 text-2xs tabular-nums transition-colors duration-150',
                item === page
                  ? 'bg-primary text-white'
                  : 'text-fg-2 hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30',
              )}
            >
              {item}
            </button>
          ),
        )}

        <Button
          size="sm"
          variant="ghost"
          data-testid="page-next"
          aria-label="下一页"
          disabled={!canNext}
          onClick={() => onPageChange(page + 1)}
          icon={<ChevronRight className="size-3" aria-hidden />}
        >
          <span className="sr-only">下一页</span>
        </Button>
        <Button
          size="sm"
          variant="ghost"
          data-testid="page-last"
          aria-label="末页"
          disabled={!canNext}
          onClick={() => onPageChange(totalPages)}
          icon={<ChevronsRight className="size-3" aria-hidden />}
        >
          <span className="sr-only">末页</span>
        </Button>
      </nav>
    </div>
  )
}
