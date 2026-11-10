/**
 * Task 事件流（SSE，`GET /api/v1/tasks/{id}/events`）。
 *
 * 为什么不用原生 `EventSource`（design/impl/web-and-delivery.md §3 规则 3）：
 *  - 要带 `Authorization` 与 `Last-Event-ID` 请求头，EventSource 不能设头；
 *  - 401 时要跳登录而不是静默重连；
 *  - 页面切到后台时要暂停。
 * 所以用 fetch + ReadableStream 自己按 `\n\n` 切帧（07 §7 的线上格式）。
 *
 * 断线策略：1s → 2s → 4s → 8s → 30s 指数退避（07 §7）；seq 去重由调用方（useTaskEvents）做。
 */
import { ApiClientError, USE_MOCK, authHeaders, normalizeErrorBody } from './client'
import { subscribeMockEvents } from './mock'
import type { TaskEvent } from './types'

export type SseStatus = 'connecting' | 'open' | 'reconnecting' | 'closed' | 'polling'

export interface TaskEventsOptions {
  /** 已经消费到的最大 seq；重连时作为 Last-Event-ID */
  lastSeq?: number
  onEvent: (event: TaskEvent) => void
  onOpen?: () => void
  onError?: (error: unknown) => void
  /** 服务端判定回放窗口失效，前端应整页重建 */
  onResync?: () => void
  onStatus?: (status: SseStatus) => void
}

const BACKOFF_MS = [1000, 2000, 4000, 8000, 30000]

function resolveUrl(path: string): string {
  const base = String(import.meta.env.VITE_API_BASE ?? '/api/v1').replace(/\/+$/, '')
  return `${base}${path}`
}

/** 解析一帧 `id: 43\nevent: runner_finished\ndata: {...}`。 */
export function parseSseFrame(frame: string): TaskEvent | null {
  let id: string | undefined
  let name: string | undefined
  const data: string[] = []

  for (const rawLine of frame.split('\n')) {
    const line = rawLine.endsWith('\r') ? rawLine.slice(0, -1) : rawLine
    if (line.length === 0 || line.startsWith(':')) continue // 空行与心跳（`: ping`）
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'id') id = value
    else if (field === 'event') name = value
    else if (field === 'data') data.push(value)
  }

  if (data.length === 0) return null

  let payload: unknown
  try {
    payload = JSON.parse(data.join('\n'))
  } catch {
    return null
  }
  if (!payload || typeof payload !== 'object') return null

  const rec = payload as Record<string, unknown>
  const seqFromId = id === undefined ? Number.NaN : Number(id)
  const seq = typeof rec['seq'] === 'number' ? rec['seq'] : seqFromId
  if (!Number.isFinite(seq)) return null

  const type = typeof rec['type'] === 'string' ? rec['type'] : (name ?? 'state_changed')
  const str = (key: string) => (typeof rec[key] === 'string' ? (rec[key] as string) : null)
  const num = (key: string) => (typeof rec[key] === 'number' ? (rec[key] as number) : null)
  return {
    seq,
    type: type as TaskEvent['type'],
    task_id: String(rec['task_id'] ?? ''),
    attempt_no: num('attempt_no'),
    runner_seq: num('runner_seq'),
    at: typeof rec['at'] === 'string' ? rec['at'] : new Date().toISOString(),
    from: str('from'),
    to: str('to'),
    reason: str('reason'),
    payload: (rec['payload'] ?? null) as TaskEvent['payload'],
  }
}

/** 订阅一个 Task 的事件流；返回取消函数。 */
export function subscribeTaskEvents(taskId: string, options: TaskEventsOptions): () => void {
  if (USE_MOCK) {
    return subscribeMockEvents(taskId, {
      sinceSeq: options.lastSeq ?? 0,
      onEvent: options.onEvent,
      onOpen: () => {
        options.onStatus?.('open')
        options.onOpen?.()
      },
      onError: (error) => {
        options.onStatus?.('polling')
        options.onError?.(error)
      },
    })
  }

  let stopped = false
  let failureCount = 0
  let lastId = options.lastSeq ?? 0
  let controller: AbortController | null = null
  let retryTimer: ReturnType<typeof setTimeout> | null = null

  const scheduleReconnect = () => {
    if (stopped) return
    const wait = BACKOFF_MS[Math.min(failureCount, BACKOFF_MS.length - 1)] ?? 30000
    failureCount += 1
    options.onStatus?.(failureCount > 3 ? 'polling' : 'reconnecting')
    retryTimer = setTimeout(() => {
      void open()
    }, wait)
  }

  const open = async (): Promise<void> => {
    if (stopped) return

    controller = new AbortController()
    if (failureCount === 0) options.onStatus?.('connecting')
    const headers = new Headers(authHeaders())
    headers.set('Accept', 'text/event-stream')
    headers.set('Cache-Control', 'no-store')
    if (lastId > 0) headers.set('Last-Event-ID', String(lastId))

    try {
      const res = await fetch(resolveUrl(`/tasks/${encodeURIComponent(taskId)}/events`), {
        headers,
        signal: controller.signal,
        credentials: 'same-origin',
        cache: 'no-store',
      })

      if (!res.ok) {
        const text = await res.text().catch(() => '')
        let body: unknown = null
        try {
          body = text ? JSON.parse(text) : null
        } catch {
          body = { message: text.slice(0, 200) }
        }
        throw new ApiClientError(res.status, normalizeErrorBody(res.status, body))
      }
      if (!res.body) throw new Error('响应没有 body（浏览器不支持流式读取？）')

      failureCount = 0
      options.onStatus?.('open')
      options.onOpen?.()

      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buffer = ''

      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        buffer = buffer.replace(/\r\n/g, '\n')

        let boundary = buffer.indexOf('\n\n')
        while (boundary >= 0) {
          const frame = buffer.slice(0, boundary)
          buffer = buffer.slice(boundary + 2)
          const event = parseSseFrame(frame)
          if (event) {
            if (event.seq > lastId) lastId = event.seq
            if (event.type === 'resync_required') options.onResync?.()
            else options.onEvent(event)
          }
          boundary = buffer.indexOf('\n\n')
        }
      }

      // 流被正常关闭（服务端 60s 无事件即释放）：按断线重连
      options.onStatus?.('reconnecting')
      scheduleReconnect()
    } catch (error) {
      if (stopped) return
      if (error instanceof DOMException && error.name === 'AbortError') return
      options.onError?.(error)
      scheduleReconnect()
    }
  }

  void open()

  return () => {
    stopped = true
    controller?.abort()
    if (retryTimer !== null) clearTimeout(retryTimer)
    options.onStatus?.('closed')
  }
}
