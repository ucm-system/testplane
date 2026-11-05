/**
 * 数据层入口：`VITE_USE_MOCK=1` → mock，否则打真实接口。
 * 页面只 import `api`，不关心背后是哪一套。
 */
import { USE_MOCK } from './client'
import type { Api } from './contract'
import { mockApi } from './mock'
import { realApi } from './real'

export const api: Api = USE_MOCK ? mockApi : realApi

export { USE_MOCK, API_BASE, ApiClientError, getCurrentUser, setCurrentUser, isApiClientError, toApiError } from './client'
export { subscribeRunEvents } from './sse'
export type { SseStatus } from './sse'
export type { Api, LogOptions } from './contract'
export type * from './types'
