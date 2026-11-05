/**
 * 登录态（OIDC）与权限。
 *
 * 判定**不靠猜**，只有两个信号：
 *  1. `GET /api/v1/meta` 的 `version`：形如 `dev` / `v0.1.0-dev` → 这是 dev 部署（后端 `/auth/*` 是 404，绝不能跳登录）；
 *     形如 `v0.1.0` → OIDC 部署。
 *  2. `GET /api/v1/me` 的响应里**有没有 `csrf_token`**：有 → OIDC 会话；没有 → dev 模式（`{user:"dev",role:"admin"}`）。
 * `me` 是权威信号，取到后覆盖 meta 的判断。
 *
 * 其它约定：
 *  - `csrf_token` 只存**内存**（不写 localStorage），所有写请求由 api/client 自动带 `X-CSRF-Token`；
 *  - 401 只在 OIDC 部署里跳 `/auth/login`，dev 模式什么都不做；
 *  - 跳转带一次性 guard，避免 401 风暴把页面打成循环重定向。
 */
import type { Identity } from './types'

export type AuthMode = 'dev' | 'oidc' | 'unknown'

export interface AuthState {
  /** 部署形态：dev（无鉴权）/ oidc（会话 + CSRF） */
  mode: AuthMode
  /** `/me` 的原始响应；未取到时为 null */
  identity: Identity | null
  user: string | null
  role: string | null
  /** 只存在内存里，刷新后由 `/me` 重新拿 */
  csrfToken: string | null
  /** 写操作是否允许：viewer 只读，operator/admin 可写（服务端仍会再判一次） */
  canOperate: boolean
  /** 是否已经完成一次身份探测（用于区分"还在加载"与"确实是 dev"） */
  ready: boolean
}

const INITIAL: AuthState = {
  mode: 'unknown',
  identity: null,
  user: null,
  role: null,
  csrfToken: null,
  canOperate: false,
  ready: false,
}

let state: AuthState = INITIAL
const listeners = new Set<(state: AuthState) => void>()

function emit(): void {
  for (const listener of listeners) listener(state)
}

export function getAuthState(): AuthState {
  return state
}

export function subscribeAuth(listener: (state: AuthState) => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** `meta.version` 是否像 dev 部署。没有 version 时按 dev 处理（宁可不跳登录，也别把人送到 404）。 */
export function looksLikeDevDeployment(version: string | undefined): boolean {
  if (!version) return true
  return /^dev\b/i.test(version) || /-dev\b/i.test(version)
}

/** meta 只用来"提前"判断；取到 `/me` 后以它为准。 */
export function applyMetaVersion(version: string | undefined): void {
  if (state.mode !== 'unknown') return
  state = { ...state, mode: looksLikeDevDeployment(version) ? 'dev' : 'oidc' }
  emit()
}

/** `/me` 是权威信号：有 csrf_token 就是 OIDC 会话。 */
export function applyIdentity(identity: Identity): void {
  const csrfToken =
    typeof identity.csrf_token === 'string' && identity.csrf_token.length > 0 ? identity.csrf_token : null
  const role = typeof identity.role === 'string' && identity.role ? identity.role : null
  state = {
    mode: csrfToken ? 'oidc' : 'dev',
    identity,
    user: identity.user ?? null,
    role,
    csrfToken,
    canOperate: role !== 'viewer',
    ready: true,
  }
  clearRedirectGuard()
  emit()
}

/** 身份没取到（网络/401/接口 404）时，至少把 ready 置起来，避免 UI 永远转圈。 */
export function markIdentityFailed(): void {
  state = { ...state, ready: true }
  emit()
}

export function clearAuth(): void {
  state = { ...INITIAL, ready: true }
  emit()
}

/* ---------------------------------------------------------------- 401 跳登录 */

const REDIRECT_FLAG = 'testplane:auth-redirect'

function clearRedirectGuard(): void {
  try {
    sessionStorage.removeItem(REDIRECT_FLAG)
  } catch {
    /* 隐私模式忽略 */
  }
}

/**
 * 401：OIDC 部署 → 跳登录（带回跳地址）；dev 部署 → 什么都不做。
 * 一次会话只跳一次，避免"登录接口也 401"造成重定向风暴。
 */
export function handleUnauthorized(): void {
  if (state.mode === 'dev') return
  try {
    if (sessionStorage.getItem(REDIRECT_FLAG)) return
    sessionStorage.setItem(REDIRECT_FLAG, '1')
  } catch {
    /* 无 sessionStorage 时不做 guard，仍然跳 */
  }
  const back = `${window.location.pathname}${window.location.search}`
  window.location.assign(`/auth/login?return=${encodeURIComponent(back)}`)
}

/* ---------------------------------------------------------------- 角色 */

export type RoleTone = 'muted' | 'info' | 'warn'

/** 三个角色的颜色：viewer 灰、operator 蓝、admin 琥珀。 */
export function roleTone(role: string | null | undefined): RoleTone {
  switch (role) {
    case 'admin':
      return 'warn'
    case 'operator':
      return 'info'
    case 'viewer':
    default:
      return 'muted'
  }
}

export const ROLE_LABEL: Record<string, string> = {
  viewer: 'viewer',
  operator: 'operator',
  admin: 'admin',
}

/** 写操作的禁用原因；返回 null 表示可写。 */
export function writeBlockedReason(auth: AuthState): string | null {
  if (!auth.ready) return null
  if (auth.mode === 'oidc' && auth.role === 'viewer') return '需要 operator 角色'
  return null
}
