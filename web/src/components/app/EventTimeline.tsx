import { useEffect, useRef } from 'react'
import type { SseStatus } from '@/api/sse'
import type { RunEvent } from '@/api/types'
import { EVENT_TYPE_CN } from '@/lib/status'
import { cn, formatClock } from '@/lib/utils'

const DOT: Record<string, string> = {
  'run.submitted': 'bg-line-strong',
  'run.state_changed': 'bg-primary',
  'task.state_changed': 'bg-primary',
  'attempt.started': 'bg-primary',
  'attempt.finished': 'bg-success',
  'deploy.state_changed': 'bg-warn',
  'cleanup.started': 'bg-line-strong',
  'run.finished': 'bg-success',
  'report.written': 'bg-success',
  resync_required: 'bg-danger',
}

function eventText(event: RunEvent): string {
  const payload = event.payload ?? {}
  const text = typeof payload['text'] === 'string' ? payload['text'] : null
  if (text) return text
  const from = payload['from']
  const to = payload['to']
  const base = EVENT_TYPE_CN[event.type] ?? event.type
  if (typeof from === 'string' || typeof to === 'string') {
    return `${base}：${String(from ?? '?')} → ${String(to ?? '?')}`
  }
  return base
}

export function EventTimeline({ events, status }: { events: RunEvent[]; status?: SseStatus }) {
  const bottomRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: 'nearest' })
  }, [events.length])

  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center gap-2 text-2xs text-fg-3">
        {status === 'open' ? (
          <span className="inline-flex items-center gap-1.5 text-success-text">
            <span className="live-dot" aria-hidden />
            实时
          </span>
        ) : status === 'polling' ? (
          <span className="text-warn-text">已降级为 3s 轮询</span>
        ) : status === 'reconnecting' ? (
          <span className="text-warn-text">重连中…</span>
        ) : status === 'closed' ? (
          <span>已结束 · 不再推送</span>
        ) : (
          <span>未连接</span>
        )}
        <span className="ml-auto font-mono">共 {events.length} 条</span>
      </div>

      {events.length === 0 ? (
        <p className="py-4 text-center text-xs text-fg-3">还没有事件</p>
      ) : (
        <ol className="max-h-[22rem] space-y-2.5 overflow-y-auto pr-1">
          {events.map((event) => (
            <li key={`${event.seq}-${event.type}`} className="flex gap-2.5">
              <span className="w-14 shrink-0 pt-0.5 font-mono text-2xs text-fg-3">{formatClock(event.at)}</span>
              <span
                className={cn('mt-1.5 size-1.5 shrink-0 rounded-full', DOT[event.type] ?? 'bg-line-strong')}
                aria-hidden
              />
              <p className="min-w-0 flex-1 text-2xs leading-relaxed text-fg-2">{eventText(event)}</p>
            </li>
          ))}
        </ol>
      )}
      <div ref={bottomRef} />
    </div>
  )
}
