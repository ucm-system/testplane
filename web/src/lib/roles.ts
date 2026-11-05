import type { AuthState } from '@/api/auth'

/**
 * 角色层级（用户裁决 2026-11-05）：
 *  - `viewer`：只读 —— 概览、运行列表/详情、报告、Tester 目录；
 *  - `operator`：再多「提交运行」与「集群与队列」；
 *  - `admin`：再多「管理台」。
 *
 * 前端只做**前置提示**（隐藏导航项 + 无权限页），权威判定永远是服务端 403。
 */
export type Role = 'viewer' | 'operator' | 'admin'

export const ROLE_RANK: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

/** 把任意字符串收敛成已知角色（未知一律按 viewer 处理，宁可少给权限） */
export function normalizeRole(role: string | null | undefined): Role {
  return role === 'admin' || role === 'operator' || role === 'viewer' ? role : 'viewer'
}

/** 当前身份是否达到某个最低角色要求 */
export function hasRole(auth: Pick<AuthState, 'role'>, min: Role): boolean {
  return ROLE_RANK[normalizeRole(auth.role)] >= ROLE_RANK[min]
}

export const ROLE_LABEL: Record<Role, string> = {
  viewer: '只读（viewer）',
  operator: '操作员（operator）',
  admin: '管理员（admin）',
}
