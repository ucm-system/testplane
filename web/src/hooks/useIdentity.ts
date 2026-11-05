import { useCallback, useEffect, useState } from 'react'
import { api } from '@/api'
import { getCurrentUser, onUserChanged, setCurrentUser } from '@/api/client'
import type { Identity } from '@/api/types'

export interface IdentityState {
  /** `GET /api/v1/auth/me` 的结果；接口不通时为 null（不阻塞页面） */
  identity: Identity | null
  /** dev 模式下的 Bearer subject（本地记录，改了就换身份） */
  subject: string
  setSubject: (next: string) => void
  reload: () => void
}

/**
 * 当前身份。MVP 是 dev 模式：Bearer subject 由本地决定，
 * 服务端 `/auth/me` 只负责回报"它认为我是谁、什么角色"。
 * 角色一律以服务端为准（07 §2.3），前端不推断。
 */
export function useIdentity(): IdentityState {
  const [identity, setIdentity] = useState<Identity | null>(null)
  const [subject, setSubjectState] = useState<string>(() => getCurrentUser())
  const [nonce, setNonce] = useState(0)

  useEffect(() => {
    let alive = true
    api.getIdentity().then(
      (value) => {
        if (alive) setIdentity(value)
      },
      () => {
        if (alive) setIdentity(null)
      },
    )
    return () => {
      alive = false
    }
  }, [nonce, subject])

  useEffect(() => {
    return onUserChanged((next) => setSubjectState(next))
  }, [])

  const setSubject = useCallback((next: string) => {
    // setCurrentUser 会派发 testplane:user-changed，再由 onUserChanged 更新 state
    setCurrentUser(next)
  }, [])

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  return { identity, subject: identity?.user ?? subject, setSubject, reload }
}
