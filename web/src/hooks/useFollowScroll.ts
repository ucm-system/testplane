import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'

/** 距底部多少像素内还算“在底部”（行高取整 + 滚动条会差几个像素） */
const BOTTOM_TOLERANCE = 24

export interface FollowScroll {
  /** 挂到真正滚动的那个 div 上 */
  ref: RefObject<HTMLDivElement | null>
  /** 是否跟随最新（用户上滚会自动变 false） */
  following: boolean
  onScroll: () => void
  /** 「回到底部」/ 打开开关：恢复跟随并立即滚到底 */
  resume: () => void
  /** 开关：关掉只是暂停跟随，不动滚动位置 */
  setFollowing: (next: boolean) => void
}

/**
 * 日志面板的「跟随最新」。
 *
 * - 默认开：`contentKey`（日志文本/条数）变化后自动滚到底；
 * - 用户手动上滚 → `following=false`（面板显示「已暂停跟随 · 回到底部」），
 *   新内容到达不再抢滚动位置；
 * - 用户自己滚回底部 → 自动恢复跟随；
 * - 组件卸载 / 切页签时 ref 自然释放，没有额外定时器。
 */
export function useFollowScroll(contentKey: unknown, enabled = true): FollowScroll {
  const ref = useRef<HTMLDivElement | null>(null)
  const [following, setFollowingState] = useState(true)

  useEffect(() => {
    if (!enabled || !following) return
    const element = ref.current
    if (!element) return
    element.scrollTop = element.scrollHeight
  }, [contentKey, enabled, following])

  const onScroll = useCallback(() => {
    const element = ref.current
    if (!element) return
    const distance = element.scrollHeight - element.scrollTop - element.clientHeight
    setFollowingState(distance <= BOTTOM_TOLERANCE)
  }, [])

  const resume = useCallback(() => {
    setFollowingState(true)
    const element = ref.current
    if (element) element.scrollTop = element.scrollHeight
  }, [])

  const setFollowing = useCallback(
    (next: boolean) => {
      if (next) resume()
      else setFollowingState(false)
    },
    [resume],
  )

  return { ref, following, onScroll, resume, setFollowing }
}
