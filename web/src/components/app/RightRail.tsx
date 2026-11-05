import { useEffect, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { cn } from '@/lib/utils'

/**
 * 右栏插槽：页面把 aside 内容 portal 到 AppShell 的第三列（288px）。
 * 这样路由组件不用关心外壳布局，圣杯的三列也只在一处定义。
 */
export function RightRail({ children }: { children: ReactNode }) {
  const [host, setHost] = useState<HTMLElement | null>(null)

  useEffect(() => {
    setHost(document.getElementById('right-rail'))
  }, [])

  if (!host) return null
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
    <section className={cn('min-w-0 rounded-lg border border-line-soft bg-surface p-5 shadow-card', className)}>
      <h2 className="flex items-center text-sm font-semibold text-fg">
        {tone ? <span className={cn('mr-2 inline-block size-1.5 rounded-full', DOT[tone])} aria-hidden /> : null}
        {title}
      </h2>
      {sub ? <p className="mt-0.5 text-2xs text-fg-3">{sub}</p> : null}
      <div className="mt-3 text-xs leading-relaxed text-fg-2">{children}</div>
    </section>
  )
}
