/**
 * HTTP 底座：错误归一、dev 鉴权头、JSON/文本读取。
 *
 * - 错误模型吃两种线上形态（扁平 / `{error:{...}}`），统一成 ApiErrorBody。
 * - 鉴权（MVP dev 模式）：`Authorization: Bearer <subject>`，不带时用 `dev`；
 *   当前身份由 `GET /api/v1/auth/me` 读。右上角改名就换这个 Bearer 值。
 *   真 OIDC 上线后换成 Cookie + `X-CSRF-Token`（07 §2.1），页面不用动。
 */
import type { ApiErrorBody } from './types'

const rawBase = String(import.meta.env.VITE_API_BASE ?? '/api/v1')
export const API_BASE = rawBase.replace(/\/+$/, '')
export const USE_MOCK = String(import.meta.env.VITE_USE_MOCK ?? '0') === '1'

const SUBJECT_KEY = 'testplane:subject'
const DEFAULT_SUBJECT = String(import.meta.env.VITE_DEV_USER ?? 'dev') || 'dev'

/* ---------------------------------------------------------------- 身份 */

export function getCurrentUser(): string {
  try {
    return localStorage.getItem(SUBJECT_KEY) || DEFAULT_SUBJECT
  } catch {
    return DEFAULT_SUBJECT
  }
}

export function setCurrentUser(user: string): void {
  const next = user.trim() || DEFAULT_SUBJECT
  try {
    localStorage.setItem(SUBJECT_KEY, next)
  } catch {
    /* 隐私模式下写不进去就算了，退回默认身份 */
  }
  window.dispatchEvent(new CustomEvent('testplane:user-changed', { detail: next }))
}

/** 右上角改身份后，各页面重新拉数据（dev 模式没有真会话，只能这样通知）。 */
export function onUserChanged(listener: (user: string) => void): () => void {
  const handler = (e: Event) => listener(String((e as CustomEvent<string>).detail ?? getCurrentUser()))
  window.addEventListener('testplane:user-changed', handler)
  return () => window.removeEventListener('testplane:user-changed', handler)
}

/** dev 鉴权头：`Authorization: Bearer <subject>`（不带则为 `dev`）。 */
export function authHeaders(): Record<string, string> {
  return { Authorization: `Bearer ${getCurrentUser()}` }
}

/* ---------------------------------------------------------------- 错误 */

export class ApiClientError extends Error {
  readonly status: number
  readonly error: ApiErrorBody

  constructor(status: number, error: ApiErrorBody) {
    super(error.message)
    this.name = 'ApiClientError'
    this.status = status
    this.error = error
  }

  get code(): string {
    return this.error.code
  }

  get retryable(): boolean {
    return this.error.retryable
  }
}

export function isApiClientError(e: unknown): e is ApiClientError {
  return e instanceof ApiClientError
}

/** 把任意异常折成错误模型，页面永远只面对 ApiErrorBody。 */
export function toApiError(e: unknown): ApiErrorBody {
  if (e instanceof ApiClientError) return e.error
  if (e instanceof DOMException && e.name === 'AbortError') {
    return { code: 'CANCELED', message: '请求已取消', retryable: true }
  }
  if (e instanceof TypeError) {
    // fetch 在网络层失败时抛 TypeError：后端没起 / 代理不通
    return {
      code: 'ENDPOINT_UNREACHABLE',
      message: '连不上控制面（检查后端是否在 8080，或 vite 代理）',
      detail: { reason: e.message },
      retryable: true,
    }
  }
  if (e instanceof Error) {
    return { code: 'INTERNAL_ERROR', message: e.message, retryable: false }
  }
  return { code: 'INTERNAL_ERROR', message: '未知错误', retryable: false }
}

/** HTTP 状态 → 兜底错误码（07 §3 的映射表）。 */
function fallbackForStatus(status: number): ApiErrorBody {
  const map: Record<number, [string, string, boolean]> = {
    400: ['VALIDATION_ERROR', '请求参数不合法', false],
    401: ['UNAUTHENTICATED', '未登录或会话已过期', false],
    403: ['FORBIDDEN', '当前身份无权执行该操作', false],
    404: ['NOT_FOUND', '对象不存在（可能已被清理）', false],
    409: ['CONFLICT', '对象状态已变化，请刷新后重试', false],
    413: ['VALIDATION_ERROR', '请求体超过 1 MiB', false],
    422: ['VALIDATION_ERROR', '请求内容校验未通过', false],
    429: ['RESOURCE_EXHAUSTED', '请求过于频繁或槽位已满', true],
    500: ['INTERNAL_ERROR', '服务端内部错误', false],
    502: ['ENDPOINT_UNREACHABLE', '下游服务不可达', true],
    503: ['EXECUTOR_UNAVAILABLE', '执行侧不可用', true],
    504: ['TASK_TIMEOUT', '请求超时', true],
    507: ['ARTIFACT_QUOTA_EXCEEDED', '产物额度已超', false],
  }
  const hit = map[status]
  if (hit) return { code: hit[0], message: hit[1], retryable: hit[2] }
  return { code: status >= 500 ? 'INTERNAL_ERROR' : 'VALIDATION_ERROR', message: `请求失败（HTTP ${status}）`, retryable: status >= 500 }
}

function pickString(source: Record<string, unknown>, key: string): string | undefined {
  const v = source[key]
  return typeof v === 'string' && v.length > 0 ? v : undefined
}

/**
 * 错误体归一：兼容 `{code,message,...}` 与 `{error:{code,message,...}}`，
 * 缺字段时按 HTTP 状态补兜底值 —— 前端永远能展示 code + message。
 */
export function normalizeErrorBody(status: number, body: unknown): ApiErrorBody {
  const fallback = fallbackForStatus(status)
  if (!body || typeof body !== 'object') return fallback

  const record = body as Record<string, unknown>
  const nested = record['error']
  const inner: Record<string, unknown> =
    nested && typeof nested === 'object' ? (nested as Record<string, unknown>) : record

  const code = pickString(inner, 'code') ?? fallback.code
  const message = pickString(inner, 'message') ?? pickString(record, 'message') ?? fallback.message
  const retryableRaw = inner['retryable']
  const retryable = typeof retryableRaw === 'boolean' ? retryableRaw : fallback.retryable
  const traceId = pickString(inner, 'trace_id') ?? pickString(record, 'trace_id')
  const detailRaw = inner['detail'] ?? inner['issues'] ?? record['detail']
  const detail =
    detailRaw && typeof detailRaw === 'object' && !Array.isArray(detailRaw)
      ? (detailRaw as Record<string, unknown>)
      : Array.isArray(detailRaw)
        ? { issues: detailRaw }
        : undefined

  return {
    code,
    message,
    retryable,
    ...(detail === undefined ? {} : { detail }),
    ...(traceId ? { trace_id: traceId } : {}),
  }
}

/* ---------------------------------------------------------------- 请求 */

function baseHeaders(extra?: HeadersInit): Headers {
  const headers = new Headers(extra)
  if (!headers.has('Accept')) headers.set('Accept', 'application/json')
  for (const [key, value] of Object.entries(authHeaders())) {
    if (!headers.has(key)) headers.set(key, value)
  }
  return headers
}

/** 发请求并返回 Response；非 2xx 一律抛 ApiClientError。 */
export async function http(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = baseHeaders(init.headers)
  if (init.body != null && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  let res: Response
  try {
    res = await fetch(`${API_BASE}${path}`, {
      ...init,
      headers,
      credentials: 'same-origin',
    })
  } catch (e) {
    throw new ApiClientError(0, toApiError(e))
  }

  if (!res.ok) {
    const text = await res.text().catch(() => '')
    let body: unknown = null
    if (text) {
      try {
        body = JSON.parse(text)
      } catch {
        body = { message: text.slice(0, 300) }
      }
    }
    const error = normalizeErrorBody(res.status, body)
    const traceId = res.headers.get('X-Request-Id')
    throw new ApiClientError(res.status, traceId && !error.trace_id ? { ...error, trace_id: traceId } : error)
  }

  return res
}

/** JSON 响应；204 / 空体返回 undefined。 */
export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await http(path, init)
  if (res.status === 204) return undefined as T
  const text = await res.text()
  if (!text) return undefined as T
  try {
    return JSON.parse(text) as T
  } catch {
    throw new ApiClientError(res.status, {
      code: 'RESULT_INVALID',
      message: '响应不是合法 JSON',
      detail: { body: text.slice(0, 300) },
      retryable: false,
    })
  }
}

/** 文本响应（日志 `text/plain`）。 */
export async function requestText(path: string, init: RequestInit = {}): Promise<LogText> {
  const res = await http(path, init)
  const text = await res.text()
  const offset = Number(res.headers.get('X-Log-Offset') ?? '')
  return {
    text,
    ...(Number.isFinite(offset) ? { offset } : {}),
    coverage: res.headers.get('X-Log-Coverage'),
  }
}

export interface LogText {
  text: string
  offset?: number
  coverage: string | null
}

/** 查询字符串：undefined / '' / null 都跳过；数组用逗号（07 §6）。 */
export function qs(params: Record<string, string | number | boolean | null | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const s = search.toString()
  return s ? `?${s}` : ''
}
