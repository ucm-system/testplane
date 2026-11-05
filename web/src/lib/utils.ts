import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** shadcn 约定的类名合并（clsx 组合 + tailwind-merge 去冲突）。 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs))
}

/* ------------------------------------------------------------------ 时间 */

function toDate(value: string | null | undefined): Date | null {
  if (!value) return null
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? null : d
}

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}

/** `2026-11-04 10:14`（本地时区，列表用）。 */
export function formatDateTime(value: string | null | undefined): string {
  const d = toDate(value)
  if (!d) return '—'
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** `10:14:05`（本地时区，事件/日志用）。 */
export function formatClock(value: string | null | undefined): string {
  const d = toDate(value)
  if (!d) return '—'
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/** `3 分钟前` / `2 天前`（相对时间，右栏与事件流用）。 */
export function formatRelative(value: string | null | undefined): string {
  const d = toDate(value)
  if (!d) return '—'
  const diffMs = Date.now() - d.getTime()
  const abs = Math.abs(diffMs)
  const suffix = diffMs >= 0 ? '前' : '后'
  const minutes = Math.round(abs / 60_000)
  if (minutes < 1) return '刚刚'
  if (minutes < 60) return `${minutes} 分钟${suffix}`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours} 小时${suffix}`
  return `${Math.round(hours / 24)} 天${suffix}`
}

/** `23m 18s`：秒 → 人话时长；null/undefined 显示占位。 */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || Number.isNaN(seconds)) return '—'
  const total = Math.max(0, Math.round(seconds))
  if (total < 60) return `${total}s`
  const m = Math.floor(total / 60)
  const s = total % 60
  if (m < 60) return `${m}m ${pad(s)}s`
  const h = Math.floor(m / 60)
  return `${h}h ${pad(m % 60)}m`
}

/** 起止时间 → 时长；运行中传 now 表示"进行中"。 */
export function durationBetween(
  start: string | null | undefined,
  end: string | null | undefined,
): number | null {
  const a = toDate(start)
  if (!a) return null
  const b = toDate(end) ?? new Date()
  return Math.max(0, (b.getTime() - a.getTime()) / 1000)
}

/**
 * Run 的耗时（秒）：契约里 Run 只有 `started_at` / `finished_at`，没有 `duration_seconds`。
 * 运行中按"到现在"算，没开始算 null。
 */
export function runDuration(run: {
  started_at?: string | null
  finished_at?: string | null
  submitted_at?: string | null
  status?: string
}): number | null {
  const start = run.started_at ?? run.submitted_at ?? null
  if (!start) return null
  const from = new Date(start).getTime()
  if (Number.isNaN(from)) return null
  const to = run.finished_at ? new Date(run.finished_at).getTime() : Date.now()
  if (Number.isNaN(to)) return null
  return Math.max(0, Math.round((to - from) / 1000))
}

/** 数字千分位；小数最多两位。 */
export function formatNumber(value: number | null | undefined): string {
  if (value == null || Number.isNaN(value)) return '—'
  return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 }).format(value)
}

/* ------------------------------------------------------------------ 文本 */

export function dash(value: unknown): string {
  if (value == null) return '—'
  const s = String(value)
  return s.length === 0 ? '—' : s
}

/** 超长 ID / 名称截断（列表单元格用，完整值放 title）。 */
export function truncate(value: string, max = 28): string {
  return value.length <= max ? value : `${value.slice(0, max - 1)}…`
}

/** 复制到剪贴板：优先 async clipboard，失败退回 execCommand。 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* 落到下面的兜底 */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    return ok
  } catch {
    return false
  }
}

/** 把 `{a:{b:1}}` 之类的 detail 转成可读字符串（错误详情展示用）。 */
export function detailToText(detail: unknown): string {
  if (detail == null) return ''
  if (typeof detail === 'string') return detail
  try {
    return JSON.stringify(detail, null, 2)
  } catch {
    return String(detail)
  }
}
