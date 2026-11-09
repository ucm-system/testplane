import { useEffect, useRef, useState } from 'react'
import { ChevronDown, LogOut, UserRound } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import { roleTone } from '@/api/auth'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { useToast } from '@/components/ui/toast'
import { useIdentity } from '@/hooks/useIdentity'
import { cn } from '@/lib/utils'

export function RoleBadge({ role, className }: { role: string | null | undefined; className?: string }) {
  if (!role) return null
  return (
    <Badge tone={roleTone(role)} className={cn('h-[18px] px-1.5', className)} title={`角色：${role}`}>
      {role}
    </Badge>
  )
}

function MenuItem({
  children,
  onClick,
  disabled,
  icon,
}: {
  children: React.ReactNode
  onClick: () => void
  disabled?: boolean
  icon?: React.ReactNode
}) {
  return (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      onClick={onClick}
      className={cn(
        'flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-xs transition-colors',
        'text-fg-2 hover:bg-surface-2 hover:text-fg disabled:cursor-not-allowed disabled:opacity-50',
      )}
    >
      {icon}
      {children}
    </button>
  )
}

/**
 * 顶栏用户区：用户名 + 角色徽章 + 下拉。
 * - OIDC 会话：下拉里只有「登出」（`POST /auth/logout` 之后回 `/`）；
 * - dev 部署（无 `/auth/*`）：下拉里是「切换 dev 身份」（Bearer subject）。
 */
export function UserMenu() {
  const { auth, subject, setSubject } = useIdentity()
  const toast = useToast()
  const [open, setOpen] = useState(false)
  const [switchOpen, setSwitchOpen] = useState(false)
  const [draft, setDraft] = useState(subject)
  const [loggingOut, setLoggingOut] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

  useEffect(() => setDraft(subject), [subject])

  useEffect(() => {
    if (!open) return
    const onPointerDown = (event: MouseEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  const logout = async () => {
    setLoggingOut(true)
    try {
      await api.logout()
      window.location.assign('/')
    } catch (e) {
      toast({ title: '登出失败', description: toApiError(e).message, tone: 'err' })
    } finally {
      setLoggingOut(false)
      setOpen(false)
    }
  }

  const submitSubject = () => {
    const next = draft.trim()
    if (!next) return
    setSubject(next)
    setSwitchOpen(false)
    toast({ title: `已切换身份：${next}`, description: `之后的请求都带 Authorization: Bearer ${next}`, tone: 'info' })
  }

  const oidc = auth.mode === 'oidc'
  const groups = auth.identity?.groups ?? []
  const initial = (subject[0] ?? '?').toUpperCase()

  return (
    <div className="relative" ref={rootRef}>
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="账号与角色"
        className="flex items-center gap-2 rounded-md py-1 pl-3 pr-1.5 transition-colors hover:bg-surface-2"
        title={oidc ? 'OIDC 会话' : 'dev 部署'}
      >
        <span className="grid size-[26px] place-items-center rounded-full bg-primary-weak text-xs text-primary-text">
          {initial}
        </span>
        <span className="hidden max-w-[10rem] truncate text-xs text-fg-2 sm:inline">{subject || '未登录'}</span>
        <RoleBadge role={auth.role} />
        <ChevronDown className="size-3.5 text-fg-3" aria-hidden />
      </button>

      {open ? (
        <div
          role="menu"
          className="absolute right-0 top-full z-50 mt-1.5 w-60 rounded-md border border-line bg-surface p-1 shadow-overlay"
        >
          <div className="px-2 py-1.5">
            <p className="truncate text-xs font-medium text-fg">{subject || '未登录'}</p>
            <p className="mt-0.5 flex items-center gap-1.5 text-2xs text-fg-3">
              {oidc ? 'OIDC 会话' : 'dev 部署'}
              <RoleBadge role={auth.role} />
              {auth.role === 'viewer' ? <span className="text-warn-text">只读</span> : null}
            </p>
            {/* groups 只用于排障：一眼看出"为什么我是 viewer" */}
            {groups.length > 0 ? (
              <p className="mt-1.5 flex flex-wrap items-center gap-1">
                {groups.slice(0, 5).map((group) => (
                  <span
                    key={group}
                    className="rounded-full bg-surface-3 px-1.5 py-0.5 font-mono text-2xs leading-none text-fg-3"
                    title={group}
                  >
                    {group}
                  </span>
                ))}
                {groups.length > 5 ? (
                  <span className="text-2xs text-fg-3">等 {groups.length} 个</span>
                ) : null}
              </p>
            ) : null}
          </div>
          <div className="my-1 h-px bg-line-soft" />
          {oidc ? (
            <MenuItem onClick={() => void logout()} disabled={loggingOut} icon={<LogOut className="size-3.5" aria-hidden />}>
              {loggingOut ? '登出中…' : '登出'}
            </MenuItem>
          ) : (
            <MenuItem
              onClick={() => {
                setSwitchOpen(true)
                setOpen(false)
              }}
              icon={<UserRound className="size-3.5" aria-hidden />}
            >
              切换 dev 身份
            </MenuItem>
          )}
        </div>
      ) : null}

      <Dialog
        open={switchOpen}
        onClose={() => setSwitchOpen(false)}
        title="切换 dev 身份"
        description="dev 部署没有登录：请求头 Authorization: Bearer <subject>，不带时服务端按 dev 处理。"
        footer={
          <>
            <Button onClick={() => setSwitchOpen(false)}>取消</Button>
            <Button variant="primary" onClick={submitSubject}>
              保存
            </Button>
          </>
        }
      >
        <Field label="Bearer subject" hint="例如 dev / zhangsan / operator-01">
          <Input
            value={draft}
            autoFocus
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') submitSubject()
            }}
            placeholder="dev"
          />
        </Field>
      </Dialog>
    </div>
  )
}
