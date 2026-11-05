/**
 * Run 事件流（SSE）。
 *
 * 为什么不用原生 `EventSource`（design/impl/web-and-delivery.md §3 规则 3）：
 *  - 要带 `Authorization` 与 `Last-Event-ID` 请求头，EventSource 不能设头；
 *  - 401 时要跳登录而不是静默重连；
 *  - 页面切到后台时要暂停。
 * 所以用 fetch + ReadableStream 自己按 `\n\n` 切帧（07 §7 的线上格式）。
 *
 * 断线策略：1s → 2s → 4s → 8s → 30s 指数退避（07 §7）；seq 去重由调用方（useRunEvents）做。
 */
import { ApiClientError, USE_MOCK, authHeaders, normalizeErrorBody } from './client'
import { subscribeMockEvents } from './mock'
import type { RunEvent } from './types'

export type SseStatus = 'connecting' | 'open' | 'reconnecting' | 'closed' | 'polling'

export interface RunEventsOptions {
  /** 已经消费到的最大 seq；重连时作为 Last-Event-ID */
  lastSeq?: number
  onEvent: (event: RunEvent) => void
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

/** 解析一帧 `id: 43\nevent: task.state_changed\ndata: {...}`。 */
export function parseSseFrame(frame: string): RunEvent | null {
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

  const type = typeof rec['type'] === 'string' ? rec['type'] : (name ?? 'run.state_changed')
  return {
    seq,
    type: type as RunEvent['type'],
    run_id: String(rec['run_id'] ?? ''),
    task_id: typeof rec['task_id'] === 'string' ? rec['task_id'] : null,
    attempt: typeof rec['attempt'] === 'number' ? rec['attempt'] : null,
    at: typeof rec['at'] === 'string' ? rec['at'] : new Date().toISOString(),
    payload: (rec['payload'] ?? null) as RunEvent['payload'],
  }
}

/** 订阅一个 Run 的事件流；返回取消函数。 */
export function subscribeRunEvents(runId: string, options: RunEventsOptions): () => void {
  if (USE_MOCK) {
    return subscribeMockEvents(runId, {
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
      const res = await fetch(resolveUrl(`/runs/${encodeURIComponent(runId)}/events`), {
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
