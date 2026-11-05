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
  widthClass?: string
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
  widthClass = 'max-w-md',
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
      <div className="absolute inset-0 animate-fade-in bg-[rgb(15_23_42_/_0.45)]" onClick={onClose} aria-hidden />
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className={cn(
          'absolute inset-y-0 flex w-full flex-col bg-surface shadow-modal',
          side === 'right' ? 'right-0 animate-slide-in-right border-l border-line' : 'left-0 animate-slide-in border-r border-line',
          widthClass,
        )}
      >
        <header className="flex items-start gap-3 border-b border-line-soft px-5 py-4">
          <div className="min-w-0 flex-1">
            <h2 id={titleId} className="text-sm font-semibold text-fg">
              {title}
            </h2>
            {description ? <p className="mt-1 text-2xs leading-relaxed text-fg-3">{description}</p> : null}
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="关闭"
            className="-mr-1 rounded-md p-1 text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
          >
            <X className="size-4" aria-hidden />
          </button>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer ? (
          <footer className="flex flex-wrap items-center gap-2 border-t border-line-soft bg-surface-2 px-5 py-3">
            {footer}
          </footer>
        ) : null}
      </div>
    </div>,
    document.body,
  )
}
