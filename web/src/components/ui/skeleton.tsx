import { cn } from '@/lib/utils'

/** 骨架屏（对齐 shadcn Skeleton）：纯灰脉冲 + `rounded-md`，不做扫光渐变。 */
export function Skeleton({ className }: { className?: string }) {
  return <div className={cn('skeleton h-4 w-full rounded-md', className)} aria-hidden />
}

/** 表格加载态：n 行 × 若干列的灰条。 */
export function TableSkeleton({ rows = 5, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="flex flex-col gap-2 px-6 py-3" aria-busy="true" aria-label="加载中">
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

/** KPI 卡加载态（与 KpiCard 同款外框，避免加载时跳动）。 */
export function KpiSkeleton({ className }: { className?: string }) {
  return (
    <div className={cn('rounded-xl border border-line bg-surface p-5 shadow-xs', className)}>
      <Skeleton className="h-3 w-20" />
      <Skeleton className="mt-4 h-6 w-14" />
      <Skeleton className="mt-3 h-3 w-28" />
    </div>
  )
}
