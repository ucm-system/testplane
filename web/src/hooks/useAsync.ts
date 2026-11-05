import { useCallback, useEffect, useRef, useState } from 'react'
import { toApiError } from '@/api/client'
import type { ApiErrorBody } from '@/api/types'

export interface AsyncState<T> {
  data: T | null
  error: ApiErrorBody | null
  loading: boolean
}

export interface AsyncResult<T> extends AsyncState<T> {
  reload: () => void
}

/**
 * 极简服务端状态 hook：加载中 / 错误 / 数据三态 + 手动 reload。
 * 不做全局缓存（design 选了 TanStack Query，但 MVP 只有一张数据面，
 * 用 20 行换掉一个依赖；接 Query 时替换本文件即可，页面不动）。
 *
 * 语义：reload 时**保留**上一份数据（避免 SSE 触发刷新时闪屏）；
 * 换对象（比如换 runId）请在外层给组件加 `key`，让它整体重挂。
 */
export function useAsync<T>(loader: () => Promise<T>, deps: readonly unknown[]): AsyncResult<T> {
  const [state, setState] = useState<AsyncState<T>>({ data: null, error: null, loading: true })
  const [nonce, setNonce] = useState(0)
  const loaderRef = useRef(loader)
  loaderRef.current = loader

  useEffect(() => {
    let alive = true
    setState((prev) => ({ data: prev.data, error: null, loading: true }))
    loaderRef.current().then(
      (data) => {
        if (alive) setState({ data, error: null, loading: false })
      },
      (error: unknown) => {
        if (alive) setState({ data: null, error: toApiError(error), loading: false })
      },
    )
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  return { ...state, reload }
}
