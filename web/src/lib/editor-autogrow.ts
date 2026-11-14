import { useEffect, useRef, useState } from 'react'

/** 行高（px）：fontSize 12 × lineHeight 1.7 ≈ 21.25 —— 与两个编辑器组件的 rowsHeight 公式一致。 */
const PER_LINE = 1.7 * 12.5

/**
 * 编辑器随内容自动长高（用户 2026-11-13：默认框太低）：
 * 高度 = rows 基准 + 超出部分 × 行高，封顶半屏；封顶后内部滚动，不再自动变高。
 * 用户拖过一次（外层容器的 CSS `resize-y` 把手）就以用户高度为准，自动增长让位。
 *
 * 区分「我们自己设的高度」与「用户拖出来的高度」靠 ResizeObserver：
 * 自己设的会在 lastApplied 里留底（差值 ≤ 边框 2px + 容差），超出容差的才是用户行为。
 */
export function useEditorAutoGrow(boxRef: { current: HTMLElement | null }, autoHeight: number): number {
  const [userHeight, setUserHeight] = useState<number | null>(null)
  const lastApplied = useRef(0)
  const height = userHeight ?? autoHeight

  useEffect(() => {
    const el = boxRef.current
    if (!el) return
    el.style.height = `${Math.round(height)}px`
    lastApplied.current = Math.round(height)
  }, [boxRef, height])

  useEffect(() => {
    const el = boxRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver((entries) => {
      const observed = entries[0]?.contentRect.height ?? 0
      if (observed > 0 && Math.abs(observed - lastApplied.current) > 3) setUserHeight(observed)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [boxRef])

  return height
}

/** rows 基准高度 → 随内容行数增长的自动高度（封顶 50vh，且永不小于基准）。 */
export function editorAutoHeight(rows: number, contentLines: number): number {
  const base = Math.round(rows * PER_LINE + 16)
  const vh = typeof window === 'undefined' ? 800 : window.innerHeight
  const cap = Math.max(Math.round(vh * 0.5), base + 3 * PER_LINE)
  return Math.min(base + Math.max(0, contentLines - rows) * PER_LINE, cap)
}

/** 编辑器内容行数（至少 rows 行，保证基准高度）。 */
export function editorContentLines(value: string, rows: number): number {
  return Math.max(rows, value.split('\n').length)
}
