import { useEffect, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { ChevronDown } from 'lucide-react'
import { cn } from '@/lib/utils'

/** 三列 → 两列的断点（与 AppShell 的 `max-[1024px]` 一致） */
const NARROW_QUERY = '(max-width: 1024px)'

/**
 * 右栏插槽：页面把 aside 内容 portal 到 AppShell 的第三列（288px）。
 * 这样路由组件不用关心外壳布局，圣杯的三列也只在一处定义。
 *
 * ≤1024px 时第三列整体收掉（两列布局），但右栏内容**不能跟着消失**：
 * 这里改挂到 `<main>` 底部的 `#rail-inline`，渲染成一个可展开区块（折叠 ≠ 隐藏）。
 * 只渲染一份实例（不复制 DOM），所以事件流不会重复请求/重复播报。
 */
export function RightRail({ children }: { children: ReactNode }) {
  const [narrow, setNarrow] = useState(
    () => typeof window !== 'undefined' && window.matchMedia(NARROW_QUERY).matches,
  )
  const [host, setHost] = useState<HTMLElement | null>(null)

  useEffect(() => {
    const media = window.matchMedia(NARROW_QUERY)
    const sync = () => setNarrow(media.matches)
    sync()
    media.addEventListener('change', sync)
    return () => media.removeEventListener('change', sync)
  }, [])

  useEffect(() => {
    setHost(document.getElementById(narrow ? 'rail-inline' : 'right-rail'))
  }, [narrow])

  if (!host) return null

  if (narrow) {
    return createPortal(
      <details
        data-testid="rail-inline-details"
        className="group rounded-xl border border-line bg-surface shadow-xs"
      >
        <summary className="flex cursor-pointer list-none items-center gap-2 px-4 py-3 text-sm font-medium text-fg [&::-webkit-details-marker]:hidden">
          <ChevronDown className="size-4 text-fg-3 transition-transform group-open:rotate-180" aria-hidden />
          事件流与边界说明
          <span className="ml-auto text-2xs font-normal text-fg-3">点开查看</span>
        </summary>
        <div className="flex flex-col gap-4 border-t border-line-soft p-4">{children}</div>
      </details>,
      host,
    )
  }

  return createPortal(<div className="flex flex-col gap-4">{children}</div>, host)
}

/** 右栏小卡片：纯说明性内容（规则/图例/怎么看），不带操作。 */
export function RailCard({
  title,
  sub,
  children,
  tone,
  className,
}: {
  title: ReactNode
  sub?: ReactNode
  children: ReactNode
  tone?: 'ok' | 'warn' | 'err' | 'info' | 'primary' | 'muted'
  className?: string
}) {
  const DOT = {
    ok: 'bg-success',
    warn: 'bg-warn',
    err: 'bg-danger',
    info: 'bg-primary',
    primary: 'bg-primary',
    muted: 'bg-fg-3',
  } as const
  return (
    <section className={cn('min-w-0 rounded-xl border border-line bg-surface p-5 shadow-xs', className)}>
      <h2 className="flex items-center text-base font-semibold text-fg">
        {tone ? <span className={cn('mr-2 inline-block size-1.5 rounded-full', DOT[tone])} aria-hidden /> : null}
        {title}
      </h2>
      {sub ? <p className="mt-1 text-xs text-fg-3">{sub}</p> : null}
      <div className="mt-3 text-sm leading-relaxed text-fg-2">{children}</div>
    </section>
  )
}
