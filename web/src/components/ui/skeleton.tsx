import { cn } from '@/lib/utils'

/** 骨架屏：列表/卡片加载态用它占位（design §6 加载骨架）。 */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('skeleton h-3 w-full rounded-sm', className)} aria-hidden />
}

/** 表格加载态：n 行 × 若干列的灰条。 */
export function TableSkeleton({ rows = 5, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="flex flex-col gap-2 px-4 py-3" aria-busy="true" aria-label="加载中">
      {Array.from({ length: rows }).map((_, rowIndex) => (
        <div key={rowIndex} className="flex items-center gap-4">
          {Array.from({ length: cols }).map((__, colIndex) => (
            <Skeleton key={colIndex} className={cn('h-3.5', colIndex === 0 ? 'w-28' : 'flex-1')} />
          ))}
        </div>
      ))}
    </div>
  )
}

/** KPI 卡加载态。 */
export function KpiSkeleton() {
  return (
    <div className="rounded-lg border border-line-soft bg-surface p-5 shadow-card">
      <Skeleton className="h-2.5 w-20" />
      <Skeleton className="mt-4 h-6 w-14" />
      <Skeleton className="mt-3 h-2.5 w-28" />
    </div>
  )
}
