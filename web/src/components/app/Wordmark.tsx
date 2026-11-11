import { cn } from '@/lib/utils'

/**
 * 品牌位图 Logo（设计师产出的 Argus 吉祥物 + 字标）。
 *
 *  - `variant="horizontal"`：完整横版（吉祥物 + Argus 字标），侧栏展开态使用；
 *  - `variant="icon"`：1:1 吉祥物头像，侧栏折叠态 / 紧凑场景使用；
 *  - 图片放 `web/public/`（Vite 静态透传，不打进 JS chunk）。
 */
export function Wordmark({
  className,
  height = 26,
  variant = 'horizontal',
}: {
  className?: string
  height?: number
  variant?: 'horizontal' | 'icon'
}) {
  const src = variant === 'icon' ? '/argus-icon.png' : '/argus-logo.png'
  // 横版原始比例 ≈ 1944:648（宽:高 ≈ 3:1）；高度 26 时宽度约 78。
  return (
    <img
      src={src}
      alt="Argus"
      className={cn('w-auto shrink-0 select-none', className)}
      style={{ height }}
      draggable={false}
    />
  )
}
