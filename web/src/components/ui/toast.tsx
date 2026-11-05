import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import type { Tone } from '@/lib/status'

export interface ToastOptions {
  title: string
  description?: string
  tone?: Tone
}

interface ToastItem extends ToastOptions {
  id: number
}

const DURATION_MS = 2800

const TONE_BAR: Record<Tone, string> = {
  ok: 'bg-success',
  warn: 'bg-warn',
  err: 'bg-danger',
  info: 'bg-primary',
  primary: 'bg-primary',
  muted: 'bg-line-strong',
}

const ToastContext = createContext<((options: ToastOptions) => void) | null>(null)

/** 轻量 toast（Sonner 的等价物）：右下角堆叠，2.8s 自动消失。 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const counter = useRef(0)

  const toast = useCallback((options: ToastOptions) => {
    counter.current += 1
    const id = counter.current
    setItems((prev) => [...prev, { ...options, id }])
    window.setTimeout(() => {
      setItems((prev) => prev.filter((item) => item.id !== id))
    }, DURATION_MS)
  }, [])

  const value = useMemo(() => toast, [toast])

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        aria-atomic="false"
        className="pointer-events-none fixed bottom-4 right-4 z-[60] flex w-[min(22rem,calc(100vw-2rem))] flex-col gap-2"
      >
        {items.map((item) => (
          <div
            key={item.id}
            className="pointer-events-auto flex animate-fade-in items-start gap-2.5 rounded-md bg-ink px-3.5 py-2.5 text-xs text-white shadow-modal"
          >
            <span className={cn('mt-0.5 size-1.5 shrink-0 rounded-full', TONE_BAR[item.tone ?? 'muted'])} aria-hidden />
            <div className="min-w-0">
              <p className="font-medium">{item.title}</p>
              {item.description ? <p className="mt-0.5 break-words text-2xs text-white/70">{item.description}</p> : null}
            </div>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): (options: ToastOptions) => void {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast 必须在 <ToastProvider> 里使用')
  return ctx
}
