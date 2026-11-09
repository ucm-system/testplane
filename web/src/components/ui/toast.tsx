import type { ReactNode } from 'react'
import { Toaster, toast as sonnerToast } from 'sonner'
import type { Tone } from '@/lib/status'

/**
 * Toast —— 用 **sonner**（成熟、可堆叠、可带 action、可关闭、自带无障碍播报），
 * 不再手搓定时器与堆叠布局。
 *
 * 保留一层薄适配：调用方仍是 `useToast()({ title, description, tone, action })`，页面零改动。
 * sonner 的容器自带 `aria-live="polite"` / `role="status"` 语义，`role="status"` 的断言仍成立。
 */

export interface ToastAction {
  label: string
  onClick: () => void
}

export interface ToastOptions {
  title: string
  description?: ReactNode
  tone?: Tone
  /** 需要用户立刻做的事（复制 ID、跳到详情…）；没有动作就别放 toast */
  action?: ToastAction
  dismissible?: boolean
  durationMs?: number
}

const TONE_TO_SONNER = {
  ok: 'success',
  warn: 'warning',
  err: 'error',
  info: 'info',
  primary: 'info',
  muted: 'info',
} as const

/** Provider：挂 sonner 的 Toaster，外观跟项目 token 对齐（右下、深底）。 */
export function ToastProvider({ children }: { children: ReactNode }) {
  return (
    <>
      {children}
      <Toaster
        position="bottom-right"
        closeButton
        visibleToasts={4}
        duration={4000}
        gap={8}
        offset={16}
        toastOptions={{
          classNames: {
            toast:
              'group flex w-[min(22rem,calc(100vw-2rem))] items-start gap-2.5 rounded-md border-0 bg-ink px-3.5 py-2.5 text-xs text-white shadow-overlay',
            title: 'font-medium',
            description: '!mt-0.5 break-words text-2xs !text-white/70',
            actionButton: '!rounded-sm !bg-white/15 !px-2 !py-1 text-2xs !text-white hover:!bg-white/25',
            closeButton: '!border-white/20 !bg-transparent !text-white/60 hover:!text-white',
          },
        }}
      />
    </>
  )
}

/** 与旧 API 同签名：`const toast = useToast(); toast({ title, description, tone })` */
export function useToast(): (options: ToastOptions) => void {
  return (options: ToastOptions) => {
    const { title, description, tone = 'muted', action, dismissible = true, durationMs = 4000 } = options
    const kind = TONE_TO_SONNER[tone]
    sonnerToast[kind](title, {
      description,
      duration: durationMs,
      dismissible,
      action: action ? { label: action.label, onClick: action.onClick } : undefined,
    })
  }
}
