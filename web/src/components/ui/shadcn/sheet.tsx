import type { ComponentProps } from 'react'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import { X } from 'lucide-react'
import { cn } from '@/lib/utils'

/**
 * Sheet（shadcn `@shadcn/sheet` 的 new-york-v4 源码）——模态抽屉，侧栏在移动端用它。
 *
 * 与 registry 的差别：
 *  - 用仓库已装的 `@radix-ui/react-dialog`，不新增 `radix-ui` 聚合包；
 *  - 进出场用仓库 tokens 里的 150ms 动效（`animate-fade-in` / `animate-slide-in`），
 *    不引 `tailwindcss-animate` 的 `animate-in/slide-in-from-*` 类（仓库没有这个插件）；
 *  - 关闭钮的 `aria-label` 用中文「关闭」。
 */
export function Sheet(props: ComponentProps<typeof DialogPrimitive.Root>) {
  return <DialogPrimitive.Root data-slot="sheet" {...props} />
}

export function SheetTrigger(props: ComponentProps<typeof DialogPrimitive.Trigger>) {
  return <DialogPrimitive.Trigger data-slot="sheet-trigger" {...props} />
}

export function SheetClose(props: ComponentProps<typeof DialogPrimitive.Close>) {
  return <DialogPrimitive.Close data-slot="sheet-close" {...props} />
}

export function SheetPortal(props: ComponentProps<typeof DialogPrimitive.Portal>) {
  return <DialogPrimitive.Portal data-slot="sheet-portal" {...props} />
}

export function SheetOverlay({ className, ...props }: ComponentProps<typeof DialogPrimitive.Overlay>) {
  return (
    <DialogPrimitive.Overlay
      data-slot="sheet-overlay"
      className={cn('fixed inset-0 z-50 animate-fade-in bg-foreground/40', className)}
      {...props}
    />
  )
}

export function SheetContent({
  className,
  children,
  side = 'right',
  showCloseButton = true,
  ...props
}: ComponentProps<typeof DialogPrimitive.Content> & {
  side?: 'top' | 'right' | 'bottom' | 'left'
  showCloseButton?: boolean
}) {
  return (
    <SheetPortal>
      <SheetOverlay />
      <DialogPrimitive.Content
        data-slot="sheet-content"
        className={cn(
          'fixed z-50 flex flex-col bg-surface shadow-overlay',
          side === 'right' && 'inset-y-0 right-0 h-full w-3/4 animate-slide-in-right border-l border-line sm:max-w-sm',
          side === 'left' && 'inset-y-0 left-0 h-full w-3/4 animate-slide-in border-r border-line sm:max-w-sm',
          side === 'top' && 'inset-x-0 top-0 h-auto border-b border-line',
          side === 'bottom' && 'inset-x-0 bottom-0 h-auto border-t border-line',
          className,
        )}
        {...props}
      >
        {children}
        {showCloseButton ? (
          <DialogPrimitive.Close
            aria-label="关闭"
            className="absolute right-4 top-4 rounded-xs p-1 text-fg-3 transition-colors hover:bg-surface-2 hover:text-fg focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            <X className="size-4" aria-hidden />
          </DialogPrimitive.Close>
        ) : null}
      </DialogPrimitive.Content>
    </SheetPortal>
  )
}

export function SheetHeader({ className, ...props }: ComponentProps<'div'>) {
  return <div data-slot="sheet-header" className={cn('flex flex-col gap-1.5 p-4', className)} {...props} />
}

export function SheetFooter({ className, ...props }: ComponentProps<'div'>) {
  return <div data-slot="sheet-footer" className={cn('mt-auto flex flex-col gap-2 p-4', className)} {...props} />
}

export function SheetTitle({ className, ...props }: ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      data-slot="sheet-title"
      className={cn('font-semibold text-fg', className)}
      {...props}
    />
  )
}

export function SheetDescription({ className, ...props }: ComponentProps<typeof DialogPrimitive.Description>) {
  return (
    <DialogPrimitive.Description
      data-slot="sheet-description"
      className={cn('text-sm text-fg-3', className)}
      {...props}
    />
  )
}
