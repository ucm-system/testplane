/**
 * 日志里的**标记行**（平台自己写的单行事件，不是 Tester / Pod 的正文）。
 *
 * 后端约定（design/views/03-data-view.md §4.4.1、design/impl/execution.md §7.2）：
 *   `--- [container restarted #3 at …, previous exit: OOMKilled] ---`
 *   `--- [pod terminated at …] ---`
 *   `--- [log gap: … – …, collector restarted] ---`
 *   `--- [collection stopped: run entered CLEANUP at …] ---`
 *
 * 只识别、不改内容：日志要能原样复制去搜，渲染层只加样式。
 */
export const LOG_MARKER_RE = /^--- \[.*\] ---$/

export type LogMarkerTone = 'warn' | 'danger' | 'neutral'

/** 标记行 → 语义色；普通行返回 null。 */
export function logMarkerTone(line: string): LogMarkerTone | null {
  // 文件可能带 Windows 换行残留，匹配时容忍首尾空白；展示仍用原始 line。
  if (!LOG_MARKER_RE.test(line.trim())) return null
  // `gap` 行的 reason 里常含 "collector restarted"：先判 gap，否则会被 restart 抢先标成 warn。
  if (/log gap/i.test(line)) return 'danger'
  if (/restarted/i.test(line)) return 'warn'
  return 'neutral'
}

export function isLogMarker(line: string): boolean {
  return logMarkerTone(line) !== null
}
