import type { ComponentProps } from 'react'
import { cn } from '@/lib/utils'

/**
 * shadcn 的骨架条：`animate-pulse rounded-md bg-accent`。
 *
 * 落点用我们的 `--color-surface-3`（= surface-3 骨架灰），因为仓库的
 * `components/ui/skeleton.tsx` 已有自己的 `KpiSkeleton` 等业务骨架，这里只补 shadcn 原件。
 */
export function Skeleton({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      data-slot="skeleton"
      className={cn('animate-pulse rounded-md bg-surface-3', className)}
      {...props}
    />
  )
}
