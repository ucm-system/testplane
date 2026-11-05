import { useEffect, useId, useRef, type ReactNode } from 'react'
import * as RadixDialog from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'

const SIZES = {
  sm: 'max-w-sm',
  md: 'max-w-lg',
  lg: 'max-w-2xl',
} as const

export interface DialogProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  description?: ReactNode
  children?: ReactNode
  footer?: ReactNode
  size?: keyof typeof SIZES
  /** 关闭按钮与 Esc 是否可用（提交中可关掉地板） */
  dismissible?: boolean
}

/**
 * 模态弹窗：基于 `@radix-ui/react-dialog`（focus trap + 关闭后焦点归还触发按钮）。
 *
 * 对外 props 与改造前完全一致（`open` / `onClose` / `title` / `description` / `footer` / `size` /
 * `dismissible`），所以「重跑 Run」「取消 Run」「取消 Task」「管理台删除」这些调用点不用改；
 * 无障碍细节交给 Radix：`role="dialog"` + `aria-modal` + Tab 循环 + Esc + 遮罩点击。
 */
export function Dialog({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  size = 'md',
  dismissible = true,
}: DialogProps) {
  const titleId = useId()
  const descId = useId()
  /**
   * 焦点归还：本组件的触发按钮在调用点，不是 `Dialog.Trigger`，
   * Radix 内部的 `triggerRef` 因此是空的（关闭时它 preventDefault 后无处可还，焦点会掉到 body）。
   * 这里自己记住「最近一个弹窗外的焦点」，关闭时还给它。
   */
  const lastOutsideFocusRef = useRef<HTMLElement | null>(null)

  useEffect(() => {
    const onFocusIn = (event: FocusEvent) => {
      const target = event.target
      if (!(target instanceof HTMLElement)) return
      if (target.closest('[role="dialog"]')) return
      lastOutsideFocusRef.current = target
    }
    document.addEventListener('focusin', onFocusIn)
    return () => document.removeEventListener('focusin', onFocusIn)
  }, [])

  return (
    <RadixDialog.Root
      open={open}
      onOpenChange={(next) => {
        // Radix 的关闭来源统一收敛到 onClose（Esc / 遮罩 / 关闭按钮 / 外部交互）
        if (!next) onClose()
      }}
    >
      <RadixDialog.Portal>
        <RadixDialog.Overlay className="fixed inset-0 z-50 animate-fade-in bg-[rgb(15_23_42_/_0.45)]" />
        <RadixDialog.Content
          /* 显式写出来：背景对读屏软件必须不可达（Radix 默认也会给，但别被透传的属性盖掉） */
          aria-modal="true"
          aria-labelledby={titleId}
          /* 没有 description 时必须显式置空，否则 Radix 会告警「缺 Description」 */
          aria-describedby={description ? descId : undefined}
          onCloseAutoFocus={(event) => {
            const target = lastOutsideFocusRef.current
            if (!target || !target.isConnected) return
            event.preventDefault()
            target.focus()
          }}
          onEscapeKeyDown={(event) => {
            if (!dismissible) event.preventDefault()
          }}
          onPointerDownOutside={(event) => {
            if (!dismissible) event.preventDefault()
          }}
          onInteractOutside={(event) => {
            if (!dismissible) event.preventDefault()
          }}
          className={cn(
            'fixed left-1/2 top-1/2 z-50 flex max-h-[calc(100vh-3rem)] w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2',
            'animate-fade-in flex-col overflow-hidden rounded-xl border border-line bg-surface shadow-modal',
            SIZES[size],
          )}
        >
          <header className="flex items-start gap-3 border-b border-line-soft px-5 py-4">
            <div className="min-w-0 flex-1">
              <RadixDialog.Title id={titleId} className="text-sm font-semibold text-fg">
                {title}
              </RadixDialog.Title>
              {description ? (
                <RadixDialog.Description id={descId} className="mt-1 text-xs leading-relaxed text-fg-2">
                  {description}
                </RadixDialog.Description>
              ) : null}
            </div>
            {dismissible ? (
              <RadixDialog.Close asChild>
                <button
                  type="button"
                  aria-label="关闭"
                  className="-mr-1 rounded-md p-1 text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
                >
                  <X className="size-4" aria-hidden />
                </button>
              </RadixDialog.Close>
            ) : null}
          </header>
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
          {footer ? (
            <footer className="flex flex-wrap items-center justify-end gap-2 border-t border-line-soft bg-surface-2 px-5 py-3">
              {footer}
            </footer>
          ) : null}
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  )
}
