import type { ComponentProps } from 'react'
import { cn } from '@/lib/utils'

/**
 * 分隔线（evaluated 自 shadcn `@shadcn/separator` 的 new-york-v4 源码）。
 *
 * 与 registry 的差别：不引 `radix-ui` 聚合包 / `@radix-ui/react-separator`
 *（仓库里没有这两个依赖，deps 也不许动），直接输出一个 `div` + `role`。
 * `decorative` 为 true 时读屏忽略它（shadcn 的默认行为）。
 */
export function Separator({
  className,
  orientation = 'horizontal',
  decorative = true,
  ...props
}: ComponentProps<'div'> & {
  orientation?: 'horizontal' | 'vertical'
  decorative?: boolean
}) {
  return (
    <div
      data-slot="separator"
      role={decorative ? undefined : 'separator'}
      aria-orientation={decorative ? undefined : orientation}
      aria-hidden={decorative || undefined}
      className={cn(
        'shrink-0 bg-border',
        orientation === 'horizontal' ? 'h-px w-full' : 'h-full w-px',
        className,
      )}
      {...props}
    />
  )
}
