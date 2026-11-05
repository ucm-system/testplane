import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { subscribeRunEvents, type SseStatus } from '@/api/sse'
import type { RunEvent } from '@/api/types'

const MAX_EVENTS = 300
const TICK_DEBOUNCE_MS = 350
/** 连续失败后降级为 3s 轮询 events/history（web-and-delivery §3 规则 4） */
const POLL_INTERVAL_MS = 3000

const seqKey = (runId: string) => `testplane:sse:${runId}`

function readSeq(runId: string): number {
  try {
    const raw = sessionStorage.getItem(seqKey(runId))
    const parsed = raw ? Number(raw) : 0
    return Number.isFinite(parsed) ? parsed : 0
  } catch {
    return 0
  }
}

function writeSeq(runId: string, seq: number): void {
  try {
    sessionStorage.setItem(seqKey(runId), String(seq))
  } catch {
    /* 隐私模式忽略 */
  }
}

function clearSeq(runId: string): void {
  try {
    sessionStorage.removeItem(seqKey(runId))
  } catch {
    /* ignore */
  }
}

export interface RunEventsResult {
  events: RunEvent[]
  status: SseStatus
  /**
   * 每收到一批事件 +1（350ms 合并）。页面把它放进 useAsync 的 deps 就能
   * 「SSE 推 → 重新拉 Run 详情」，不用自己维护缓存合并逻辑。
   */
  version: number
  /** 服务端要求整页重建（回放窗口失效）时 +1 */
  resyncToken: number
}

export interface RunEventsOptions {
  /** 终态 Run 不订阅（设计 §3：run.finished 后关连接），只补历史 */
  live?: boolean
}

/**
 * `useRunEvents(runId)`：订阅 `/api/v1/runs/{id}/events`。
 *
 * 顺序（design/impl/web-and-delivery.md §3）：
 *  1. 首屏先 `GET /runs/{id}/events/history` 补时间线；
 *  2. 再带 `Last-Event-ID` 订阅增量，seq <= 已消费的最大值直接丢；
 *  3. 断线 1→2→4→8→30s 退避；连续失败 → status=`polling`，本 hook 自己 3s 轮询兜底；
 *  4. 页面切后台时断开，回来按 lastSeq 续上；`resync_required` → 丢断点重建。
 */
export function useRunEvents(runId: string | undefined, options: RunEventsOptions = {}): RunEventsResult {
  const live = options.live ?? true
  const [events, setEvents] = useState<RunEvent[]>([])
  const [status, setStatus] = useState<SseStatus>('connecting')
  const [version, setVersion] = useState(0)
  const [resyncToken, setResyncToken] = useState(0)
  const [epoch, setEpoch] = useState(0)

  const lastSeqRef = useRef(0)
  /** 已进过列表的 seq（历史和增量可能重叠，按 seq 去重） */
  const seenRef = useRef<Set<number>>(new Set())
  const tickTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const bump = useCallback(() => {
    if (tickTimer.current !== null) return
    tickTimer.current = setTimeout(() => {
      tickTimer.current = null
      setVersion((v) => v + 1)
    }, TICK_DEBOUNCE_MS)
  }, [])

  /**
   * 唯一入口：去重 + 落断点 + 追加。历史、SSE、轮询兜底共用它。
   * 去重按 seq 集合做（不是简单的 `> lastSeq`），因为「先拉历史再订阅」会让
   * 两边拿到同一批 seq；而 lastSeq 只用于 Last-Event-ID 续传。
   */
  const ingest = useCallback(
    (event: RunEvent) => {
      if (!runId) return
      if (seenRef.current.has(event.seq)) return
      seenRef.current.add(event.seq)
      if (event.seq > lastSeqRef.current) {
        lastSeqRef.current = event.seq
        writeSeq(runId, event.seq)
      }
      setEvents((prev) => [...prev, event].sort((a, b) => a.seq - b.seq).slice(-MAX_EVENTS))
      bump()
    },
    [runId, bump],
  )

  useEffect(() => {
    if (!runId) return
    let alive = true
    let stopStream: (() => void) | null = null

    lastSeqRef.current = readSeq(runId)
    seenRef.current = new Set()
    setEvents([])
    setStatus(live ? 'connecting' : 'closed')

    const boot = async () => {
      // ① 补历史：先拿到首屏时间线（用 0 取满窗口，重复的 seq 由 seen 集合丢掉），
      //    SSE 只负责之后的增量
      try {
        const history = await api.getEventsHistory(runId, 0)
        if (!alive) return
        for (const event of history) ingest(event)
      } catch {
        /* 历史取不到不阻塞：SSE 仍会从 Last-Event-ID 回放 */
      }
      if (!alive || !live) return

      // ② 订阅增量
      stopStream = subscribeRunEvents(runId, {
        lastSeq: lastSeqRef.current,
        onEvent: (event) => {
          if (alive) ingest(event)
        },
        onResync: () => {
          if (!alive) return
          lastSeqRef.current = 0
          seenRef.current = new Set()
          clearSeq(runId)
          setEvents([])
          setResyncToken((t) => t + 1)
          bump()
        },
        onStatus: (next) => {
          if (alive) setStatus(next)
        },
        onError: () => {
          if (alive) setStatus((prev) => (prev === 'open' ? 'reconnecting' : prev))
        },
      })
    }

    void boot()

    const onVisibility = () => {
      // 后台标签页不占着连接；回来时按 lastSeq 续上
      if (document.visibilityState === 'visible') setEpoch((e) => e + 1)
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      alive = false
      stopStream?.()
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [runId, live, epoch, bump, ingest])

  // 降级：SSE 连不上时轮询事件历史（3s）
  useEffect(() => {
    if (!runId || status !== 'polling') return
    let alive = true
    const poll = async () => {
      if (!alive) return
      try {
        const history = await api.getEventsHistory(runId, lastSeqRef.current)
        if (!alive) return
        for (const event of history) ingest(event)
      } catch {
        /* 下一轮再试；页面上的错误提示由详情查询负责 */
      }
    }
    void poll()
    const timer = setInterval(() => void poll(), POLL_INTERVAL_MS)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [runId, status, ingest])

  useEffect(() => {
    return () => {
      if (tickTimer.current !== null) clearTimeout(tickTimer.current)
    }
  }, [])

  return { events, status, version, resyncToken }
}
