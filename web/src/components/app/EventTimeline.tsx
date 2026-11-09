import { useEffect, useRef } from 'react'
import type { SseStatus } from '@/api/sse'
import type { RunEvent } from '@/api/types'
import {
  EVENT_TYPE_CN,
  attemptStatusMeta,
  deployStateMeta,
  runStatusMeta,
  taskStatusMeta,
} from '@/lib/status'
import { cn, formatClock } from '@/lib/utils'

/**
 * 连接态**唯一口径**（B5）：右栏副标题、事件区小字、页内横幅都从这里取，
 * 不再各自读 `SseStatus` 后各写一套文案（之前右栏恒为「SSE 实时推送」、事件区却写「未连接」）。
 *
 * 只保留三态；「Run 已结束」不是连接态，是页面事实（`ended`），单独表达。
 */
export type ConnectionState = 'connecting' | 'live' | 'reconnecting'

export function connectionStateOf(status: SseStatus): ConnectionState {
  if (status === 'open') return 'live'
  if (status === 'connecting') return 'connecting'
  // reconnecting / polling / closed 都是「现在没连上」：降级轮询也算重连中，不另造第四态
  return 'reconnecting'
}

export const CONNECTION_LABEL: Record<ConnectionState, string> = {
  connecting: '正在连接事件流…',
  live: '实时更新中',
  reconnecting: '连接断开，正在自动重连',
}

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

/** 状态 token → 人话（按事件类别取对应词典；未知值原样保留） */
function tokenLabel(event: RunEvent, token: string | null | undefined): string {
  if (!token) return '开始'
  if (event.type === 'run.state_changed' || event.type === 'run.finished') return runStatusMeta(token).label
  if (event.type === 'task.state_changed') return taskStatusMeta(token).label
  if (event.type === 'attempt.started' || event.type === 'attempt.finished') return attemptStatusMeta(token).label
  if (event.type === 'deploy.state_changed') return deployStateMeta(token).label
  return token
}

/**
 * 事件文案：契约里状态转移在 `from` / `to`，说明性文字在 `payload.text`。
 *
 * B4：这里是右栏，写给用户做判断 —— 不出现 `∅ → submitted` 这种协议写法，
 * 状态 token 一律翻成中文（`排队中 → 运行中`）。
 */
function eventText(event: RunEvent): string {
  const payload = event.payload ?? {}
  const text = typeof payload['text'] === 'string' ? payload['text'] : null
  if (text) return text

  if (event.type === 'run.submitted') {
    const total = typeof payload['task_count'] === 'number' ? payload['task_count'] : null
    return total == null ? '已提交，等待执行' : `已提交 · 共 ${total} 个 Task`
  }
  if (event.type === 'run.finished') return `Run 已结束 · ${tokenLabel(event, event.to)}`
  if (event.type === 'report.written') return '报告已生成'
  if (event.type === 'cleanup.started') return '开始清理环境'

  const base = EVENT_TYPE_CN[event.type] ?? event.type
  if (event.from || event.to) {
    return `${base}：${tokenLabel(event, event.from)} → ${tokenLabel(event, event.to)}`
  }
  if (event.reason) return `${base}（${event.reason}）`
  return base
}

export function EventTimeline({
  events,
  connection,
  ended = false,
}: {
  events: RunEvent[]
  connection: ConnectionState
  /** Run 已终态：事件是历史回放，不再推送 */
  ended?: boolean
}) {
  const bottomRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: 'nearest' })
  }, [events.length])

  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center gap-2 text-2xs text-fg-3">
        {ended ? (
          <span>历史回放</span>
        ) : connection === 'live' ? (
          <span className="inline-flex items-center gap-1.5 text-success-text">
            <span className="live-dot" aria-hidden />
            {CONNECTION_LABEL.live}
          </span>
        ) : (
          <span className={connection === 'reconnecting' ? 'text-warn-text' : undefined}>
            {CONNECTION_LABEL[connection]}
          </span>
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
              <p className="min-w-0 flex-1 text-2xs leading-[16px] text-fg-2">{eventText(event)}</p>
            </li>
          ))}
        </ol>
      )}
      <div ref={bottomRef} />
    </div>
  )
}
