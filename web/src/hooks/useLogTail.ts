import { useCallback, useEffect, useRef, useState } from 'react'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, LogChunk } from '@/api/types'

/** 运行中日志的轮询间隔（design 要求 3~5s；与报告页的 5s 节奏一致但不联动） */
export const LOG_POLL_INTERVAL_MS = 3000

export interface LogTailOptions {
  /** 流身份（Task + Attempt / Run + Pod）：变了就重置并重新拉首屏 */
  streamKey: string
  /** 只有运行中才轮询；终态只做首屏读取（避免长尾请求） */
  live: boolean
  /** `offset` = 要从哪个游标之后继续读；首屏传 0（拿全量） */
  load: (offset: number) => Promise<LogChunk>
  intervalMs?: number
}

export interface LogTailState {
  text: string
  loading: boolean
  error: ApiErrorBody | null
  /** 服务端返回的覆盖区间（`X-Log-Coverage`，没有则 null） */
  coverage: string | null
  /** 轮询追加的行数（首屏不计） */
  appended: number
  /** 最后一次成功拿到数据的时间（毫秒时间戳） */
  lastUpdatedAt: number | null
  reload: () => void
}

function joinChunk(previous: string, delta: string): string {
  if (!previous) return delta
  if (!delta) return previous
  // 服务端游标是行边界：增量可能以换行开头，两边都别重复插
  if (previous.endsWith('\n') || delta.startsWith('\n')) return `${previous}${delta}`
  return `${previous}\n${delta}`
}

function countLines(chunk: string): number {
  return chunk.split('\n').filter((line) => line.length > 0).length
}

/**
 * 日志游标轮询（P0：运行中 Run 的日志面板不刷新）。
 *
 * 语义（评审要求）：
 *  1. **追加**而不是整块替换：首屏 `offset=0` 拿全量，之后每次带上次的 `offset` 只取增量；
 *  2. 用 `useEffect` + 递归 `setTimeout`，**同一时刻只有一次请求在飞**；
 *     组件卸载 / 切页签 / `live` 翻 false 时 effect 清理掉定时器与结果写入；
 *  3. `live=false`（Run 或 Attempt 终态）后不再轮询，也不清空已有日志。
 */
export function useLogTail({ streamKey, live, load, intervalMs = LOG_POLL_INTERVAL_MS }: LogTailOptions): LogTailState {
  const [text, setText] = useState('')
  const [loading, setLoading] = useState(Boolean(streamKey))
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [coverage, setCoverage] = useState<string | null>(null)
  const [appended, setAppended] = useState(0)
  const [lastUpdatedAt, setLastUpdatedAt] = useState<number | null>(null)
  const [reloadToken, setReloadToken] = useState(0)

  const offsetRef = useRef(0)
  const loaderRef = useRef(load)
  loaderRef.current = load

  /* ---------------------------------------------------------- 首屏（全量） */
  useEffect(() => {
    if (!streamKey) {
      setText('')
      setLoading(false)
      setError(null)
      setCoverage(null)
      setAppended(0)
      setLastUpdatedAt(null)
      return
    }
    let alive = true
    offsetRef.current = 0
    setText('')
    setLoading(true)
    setError(null)
    setCoverage(null)
    setAppended(0)
    loaderRef.current(0).then(
      (chunk) => {
        if (!alive) return
        offsetRef.current = typeof chunk.offset === 'number' ? chunk.offset : chunk.text.length
        setText(chunk.text)
        setCoverage(chunk.coverage ?? null)
        setLoading(false)
        setLastUpdatedAt(Date.now())
      },
      (e: unknown) => {
        if (!alive) return
        setError(toApiError(e))
        setLoading(false)
      },
    )
    return () => {
      alive = false
    }
  }, [streamKey, reloadToken])

  /* ---------------------------------------------------------- 轮询（增量） */
  useEffect(() => {
    if (!streamKey || !live) return
    let alive = true
    let timer: ReturnType<typeof setTimeout> | null = null

    const tick = async () => {
      try {
        const chunk = await loaderRef.current(offsetRef.current)
        if (!alive) return
        const start = offsetRef.current
        const nextOffset = typeof chunk.offset === 'number' ? chunk.offset : start + chunk.text.length
        if (chunk.text) {
          setText((previous) => joinChunk(previous, chunk.text))
          setAppended((count) => count + countLines(chunk.text))
        }
        if (chunk.coverage) setCoverage(chunk.coverage)
        offsetRef.current = nextOffset
        setError(null)
        setLastUpdatedAt(Date.now())
      } catch (e) {
        if (!alive) return
        // 一次失败不打断后续轮询：保留已有日志，把错误挂出来让用户能手动重试
        setError(toApiError(e))
      } finally {
        if (alive) timer = setTimeout(() => void tick(), intervalMs)
      }
    }

    timer = setTimeout(() => void tick(), intervalMs)
    return () => {
      alive = false
      if (timer !== null) clearTimeout(timer)
    }
  }, [streamKey, live, intervalMs, reloadToken])

  const reload = useCallback(() => setReloadToken((token) => token + 1), [])

  return { text, loading, error, coverage, appended, lastUpdatedAt, reload }
}
