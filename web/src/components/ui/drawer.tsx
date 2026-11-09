import { useEffect, useId, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface DrawerProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  description?: ReactNode
  children: ReactNode
  footer?: ReactNode
  side?: 'right' | 'left'
  /**
   * 尺寸档位（票 #58 反馈：Tester 详情塞不下参数说明）：
   * md = 28rem（默认，窄抽屉）；lg = 42rem；xl = min(64rem, 96vw)（宽表格/长说明用）。
   * 需要更细控制时仍可传 `widthClass` 覆盖。
   */
  size?: 'md' | 'lg' | 'xl'
  widthClass?: string
}

const DRAWER_SIZE: Record<'md' | 'lg' | 'xl', string> = {
  md: 'max-w-md',
  lg: 'max-w-2xl',
  // 宽屏上限 1024px，窄屏留 4vw 让遮罩可见；用 w- 而不是 max-w- 才能顶满这个宽度
  xl: 'w-[min(64rem,96vw)] max-w-none',
}

/** 右侧抽屉（原型 .drawer）：Tester 详情/注册、移动端导航都用它。 */
export function Drawer({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  side = 'right',
  size = 'md',
  widthClass,
}: DrawerProps) {
  const titleId = useId()

  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = previousOverflow
    }
  }, [open, onClose])

  if (!open) return null

  return createPortal(
    <div className="fixed inset-0 z-50">
      <div className="absolute inset-0 animate-fade-in bg-foreground/40" onClick={onClose} aria-hidden />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className={cn(
          'absolute inset-y-0 flex w-full flex-col bg-surface shadow-overlay',
          side === 'right' ? 'right-0 animate-slide-in-right border-l border-line' : 'left-0 animate-slide-in border-r border-line',
          widthClass ?? DRAWER_SIZE[size],
        )}
        data-drawer-size={widthClass ? undefined : size}
      >
        <header className="flex items-start gap-3 border-b border-line-soft px-6 py-4">
          <div className="min-w-0 flex-1">
            <h2 id={titleId} className="text-lg font-semibold leading-none text-fg">
              {title}
            </h2>
            {description ? <p className="mt-2 text-sm leading-[16px] text-fg-2">{description}</p> : null}
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="关闭"
            className="-mr-1 rounded-sm p-1 text-fg-3 outline-none transition-colors hover:bg-line-soft hover:text-fg focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          >
            <X className="size-4" aria-hidden />
          </button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-4">{children}</div>
        {footer ? (
          <footer className="flex flex-wrap items-center gap-2 border-t border-line-soft px-6 py-4">
            {footer}
          </footer>
        ) : null}
      </div>
    </div>,
    document.body,
  )
}
