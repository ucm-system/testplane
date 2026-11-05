import { useCallback, useEffect, useState } from 'react'
import { api } from '@/api'
import { setCurrentUser } from '@/api/client'
import {
  applyMetaVersion,
  getAuthState,
  markIdentityFailed,
  subscribeAuth,
  type AuthState,
} from '@/api/auth'

export interface IdentityState {
  /** 登录态（mode/user/role/csrf/canOperate/ready） */
  auth: AuthState
  /** 展示用用户名 */
  subject: string
  setSubject: (next: string) => void
  reload: () => void
}

/**
 * 当前身份与登录态。
 *
 * 启动顺序（也是模式判定顺序）：
 *  1. `GET /api/v1/meta` → `version` 像 dev（`dev` / `*-dev`）就先按 dev 处理，
 *     避免 401 把人送去并不存在的 `/auth/login`；
 *  2. `GET /api/v1/me` → **有 `csrf_token` 即 OIDC 会话**（覆盖第 1 步），没有就是 dev。
 * 判定结果与 CSRF 都存在 `api/auth.ts` 的模块内存里，组件通过 `subscribeAuth` 订阅。
 */
export function useIdentity(): IdentityState {
  const [auth, setAuth] = useState<AuthState>(getAuthState)
  const [subject, setSubjectState] = useState<string>(() => getAuthState().user ?? '')
  const [nonce, setNonce] = useState(0)

  useEffect(() => subscribeAuth(setAuth), [])

  useEffect(() => {
    let alive = true
    const boot = async () => {
      try {
        const meta = await api.getMeta()
        if (alive) applyMetaVersion(meta.version)
      } catch {
        /* meta 取不到不影响：/me 仍是权威信号 */
      }
      try {
        const identity = await api.getIdentity()
        if (alive) setSubjectState(identity.user)
      } catch {
        if (alive) markIdentityFailed()
      }
    }
    void boot()
    return () => {
      alive = false
    }
  }, [nonce])

  const setSubject = useCallback((next: string) => {
    setCurrentUser(next)
    setSubjectState(next)
  }, [])

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  return { auth, subject: auth.user ?? subject, setSubject, reload }
}
