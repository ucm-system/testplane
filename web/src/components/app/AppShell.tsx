import { Suspense, useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { Layers, List, Menu, PlusCircle, Server } from 'lucide-react'
import { USE_MOCK } from '@/api'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Drawer } from '@/components/ui/drawer'
import { Field, Input } from '@/components/ui/input'
import { useToast } from '@/components/ui/toast'
import { useIdentity } from '@/hooks/useIdentity'
import { cn } from '@/lib/utils'

interface NavItem {
  to: string
  label: string
  icon: typeof List
  end?: boolean
}

const NAV_GROUPS: Array<{ title: string; items: NavItem[] }> = [
  {
    title: '运行',
    items: [
      { to: '/', label: '概览', icon: Layers, end: true },
      { to: '/runs', label: '运行列表', icon: List },
      { to: '/submit', label: '提交运行', icon: PlusCircle },
    ],
  },
  {
    title: '能力',
    items: [
      { to: '/testers', label: 'Tester 目录', icon: Server },
      { to: '/clusters', label: '集群与队列', icon: Server },
    ],
  },
]

/** 路由 → 页头标题（页面级信息在页头，操作也在页头）。 */
const TITLES: Array<{ test: (path: string) => boolean; title: string; sub: string }> = [
  { test: (p) => p === '/', title: '概览', sub: '集群忙不忙、谁在排队、有没有东西挂着' },
  { test: (p) => p === '/runs', title: '运行列表', sub: '一次 Run = 一组 Task；Pre-Check 是平台注入的第一个 Task' },
  { test: (p) => p.startsWith('/runs/'), title: 'Run 详情', sub: '状态由服务端推，UI 只读' },
  { test: (p) => p === '/submit', title: '提交运行', sub: '参数是一段字符串，平台只做词法切分' },
  { test: (p) => p === '/testers', title: 'Tester 目录', sub: '随镜像内置，平台启动时扫描目录自动发现' },
  { test: (p) => p === '/clusters', title: '集群与队列', sub: '只回答「卡在队列还是卡在平台槽位」' },
]

function titleFor(pathname: string): { title: string; sub: string } {
  const hit = TITLES.find((item) => item.test(pathname))
  return hit ?? { title: 'TestPlane', sub: '' }
}

function NavList({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <nav aria-label="主导航" className="flex flex-col gap-0.5 px-2">
      {NAV_GROUPS.map((group) => (
        <div key={group.title} className="flex flex-col">
          <p className="px-3 pb-1.5 pt-3.5 text-2xs font-medium text-fg-3">{group.title}</p>
          {group.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              onClick={onNavigate}
              className={({ isActive }) =>
                cn(
                  'flex h-[34px] items-center gap-2 rounded-r-md border-l-2 px-3 text-sm font-medium transition-colors duration-150',
                  isActive
                    ? 'border-primary bg-primary-weak font-semibold text-primary-text'
                    : 'border-transparent text-fg-2 hover:bg-surface-2 hover:text-fg',
                )
              }
            >
              <item.icon className="size-4 shrink-0 opacity-70" aria-hidden />
              {item.label}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  )
}

/** 右上角身份块：dev 模式下点开可以换 Bearer subject。 */
function UserMenu() {
  const { subject, identity, setSubject } = useIdentity()
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState(subject)
  const toast = useToast()

  useEffect(() => setDraft(subject), [subject])

  const submit = () => {
    const next = draft.trim()
    if (!next) return
    setSubject(next)
    setOpen(false)
    toast({ title: `已切换身份：${next}`, description: '之后的请求都带 Authorization: Bearer ' + next, tone: 'info' })
  }

  const role = identity?.role ?? '…'

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="flex items-center gap-2 rounded-md py-1 pl-4 pr-2 transition-colors hover:bg-surface-2"
        title="切换 dev 身份（Authorization: Bearer）"
      >
        <span className="grid size-[26px] place-items-center rounded-full bg-primary-weak text-xs text-primary-text">
          {(subject[0] ?? 'd').toUpperCase()}
        </span>
        <span className="hidden text-xs text-fg-2 sm:inline">
          {subject} · {role}
        </span>
      </button>

      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        title="切换 dev 身份"
        description="MVP 是 dev 鉴权：请求头 Authorization: Bearer <subject>，不带时服务端按 dev 处理。"
        footer={
          <>
            <Button onClick={() => setOpen(false)}>取消</Button>
            <Button variant="primary" onClick={submit}>
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
              if (e.key === 'Enter') submit()
            }}
            placeholder="dev"
          />
        </Field>
      </Dialog>
    </>
  )
}

/** 圣杯外壳：固定页头 + 左导航 + 主内容 + 右栏 + 页脚（holy-grail.css）。 */
export function AppShell() {
  const location = useLocation()
  const [mobileNavOpen, setMobileNavOpen] = useState(false)
  const { title, sub } = titleFor(location.pathname)

  useEffect(() => setMobileNavOpen(false), [location.pathname])

  return (
    <div className="flex min-h-screen flex-col bg-canvas">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-2 focus:top-2 focus:z-[70] focus:rounded-md focus:bg-surface focus:px-3 focus:py-1.5 focus:text-xs focus:shadow-pop"
      >
        跳到主内容
      </a>

      <header className="sticky top-0 z-40 flex h-14 shrink-0 items-center gap-3 border-b border-line bg-surface px-4 md:gap-5 md:px-6">
        <button
          type="button"
          className="rounded-md p-1.5 text-fg-2 hover:bg-surface-2 md:hidden"
          onClick={() => setMobileNavOpen(true)}
          aria-label="打开导航"
        >
          <Menu className="size-4" aria-hidden />
        </button>

        <div className="flex shrink-0 items-center gap-2.5">
          <img src="/favicon.svg" alt="" className="size-7 rounded-[6px]" />
          <span className="text-sm font-semibold tracking-tight text-fg">TestPlane</span>
          {USE_MOCK ? (
            <span className="hidden rounded-full border border-dashed border-line-strong px-2 py-0.5 text-2xs text-fg-3 sm:inline">
              mock
            </span>
          ) : null}
        </div>

        <div className="hidden h-6 w-px bg-line md:block" aria-hidden />

        <div className="flex min-w-0 items-baseline gap-2.5">
          <h1 className="whitespace-nowrap text-[15px] font-semibold tracking-tight text-fg">{title}</h1>
          <span className="hidden truncate text-xs text-fg-3 lg:inline">{sub}</span>
        </div>

        <div className="ml-auto flex items-center gap-2">
          <UserMenu />
        </div>
      </header>

      <div className="flex min-h-0 flex-1 items-start">
        <aside className="sticky top-14 hidden h-[calc(100vh-6rem)] w-60 shrink-0 flex-col overflow-y-auto border-r border-line bg-surface pb-6 pt-2 md:flex">
          <NavList />
          <div className="mt-auto border-t border-line px-5 pb-3 pt-4 text-2xs text-fg-3">
            {USE_MOCK ? '示例原型 · 数据为 mock' : 'TestPlane · 实时数据'}
          </div>
        </aside>

        <main id="main" className="flex min-w-0 flex-1 flex-col gap-4 p-4 lg:p-6">
          <Suspense
            fallback={
              <div className="flex items-center justify-center rounded-lg border border-line-soft bg-surface py-16 text-xs text-fg-3 shadow-card">
                页面加载中…
              </div>
            }
          >
            <Outlet />
          </Suspense>
        </main>

        <aside
          id="right-rail"
          aria-label="辅助信息"
          className="sticky top-14 hidden h-[calc(100vh-6rem)] w-72 shrink-0 overflow-y-auto border-l border-line bg-surface p-5 xl:block [&:empty]:hidden"
        />
      </div>

      <footer className="sticky bottom-0 z-30 flex h-10 shrink-0 items-center gap-3 border-t border-line bg-surface px-4 text-2xs text-fg-2 md:px-6">
        <span>TestPlane · {USE_MOCK ? '示意原型，数据为 mock，操作不产生真实执行' : '控制台'}</span>
        <span className="ml-auto hidden text-fg-3 sm:inline">API: /api/v1</span>
      </footer>

      <Drawer open={mobileNavOpen} onClose={() => setMobileNavOpen(false)} title="导航" widthClass="max-w-[16rem]" side="left">
        <NavList onNavigate={() => setMobileNavOpen(false)} />
      </Drawer>
    </div>
  )
}
