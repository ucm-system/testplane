import { Suspense, useEffect, useState } from 'react'
import { Link, NavLink, Outlet, useLocation } from 'react-router-dom'
import { Layers, List, Menu, PlusCircle, Server, ShieldCheck } from 'lucide-react'
import { USE_MOCK } from '@/api'
import { UserMenu } from '@/components/app/UserMenu'
import { Wordmark } from '@/components/app/Wordmark'
import { Drawer } from '@/components/ui/drawer'
import { useIdentity } from '@/hooks/useIdentity'
import { hasRole, type Role } from '@/lib/roles'
import { cn } from '@/lib/utils'

interface NavItem {
  to: string
  label: string
  icon: typeof List
  end?: boolean
  /** 导航项的最低角色（缺省 viewer）；不够就**不渲染**这一项 */
  minRole?: Exclude<Role, 'viewer'>
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
      // 集群与队列：**viewer 也可读**（design/views/07 §2.3：GET /clusters 与 /queue-status 是 viewer ✅）
      { to: '/clusters', label: '集群与队列', icon: Server },
    ],
  },
  {
    title: '管理',
    items: [{ to: '/admin', label: '管理台', icon: ShieldCheck, minRole: 'admin' }],
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
  { test: (p) => p === '/admin', title: '管理台', sub: 'Profile / 集群 / Secret 引用 / 审计（仅 admin）' },
  { test: (p) => /^\/runs\/[^/]+\/report$/.test(p), title: 'Run 报告', sub: 'report.json：只做组织与索引' },
]

function titleFor(pathname: string): { title: string; sub: string } {
  const hit = TITLES.find((item) => item.test(pathname))
  return hit ?? { title: 'TestPlane', sub: '' }
}

function NavList({ onNavigate }: { onNavigate?: () => void }) {
  const { auth } = useIdentity()
  const groups = NAV_GROUPS.map((group) => ({
    ...group,
    items: group.items.filter((item) => (item.minRole ? hasRole(auth, item.minRole) : true)),
  })).filter((group) => group.items.length > 0)

  return (
    <nav aria-label="主导航" className="flex flex-col gap-0.5 px-2">
      {groups.map((group) => (
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
                  'flex h-[34px] items-center gap-2 rounded-r-md border-l-2 pl-[14px] pr-3 text-sm font-medium transition-colors duration-150',
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


/**
 * 圣杯外壳（`holy-grail.css`）：固定页头 + 固定页脚 + 三列等高网格。
 *
 * 两条**结构**纪律（不是外观细节，改之前先读原型）：
 *  1. 三列用 `grid-cols-[240px_minmax(0,1fr)_288px]`，**`<main>` 在 DOM 里先于两侧栏**，
 *     靠 `col-start-2` 摆到中间 —— 这样读屏与 Tab 顺序是"先内容后导航"；
 *  2. 页头与 `body` 共用同一套列宽，左分隔线落在 x=240（左栏右边缘）、
 *     右分隔线落在 x=vw−288（右栏左边缘），所以分隔线不用"随手一条"。
 */
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

      {/* 页头：列宽与 body 完全一致，两条 1px 竖线踩在列线上 */}
      <header className="sticky top-0 z-40 grid h-[56px] shrink-0 grid-cols-[240px_minmax(0,1fr)_288px] items-stretch border-b border-line bg-surface max-[1024px]:grid-cols-[240px_minmax(0,1fr)] max-[768px]:flex max-[768px]:items-center max-[768px]:gap-2">
        <div className="flex min-w-0 items-center gap-3 pl-6 pr-3 max-[768px]:w-auto max-[768px]:shrink-0 max-[768px]:pl-2">
          <button
            type="button"
            className="rounded-md p-1.5 text-fg-2 hover:bg-surface-2 min-[769px]:hidden"
            onClick={() => setMobileNavOpen(true)}
            aria-label="打开导航"
          >
            <Menu className="size-4" aria-hidden />
          </button>
          <Link to="/" aria-label="TestPlane 首页" className="flex shrink-0 items-center gap-2">
            <Wordmark className="text-fg" />
          </Link>
          {USE_MOCK ? (
            <span className="hidden rounded-full border border-dashed border-line-strong px-2 py-0.5 text-2xs text-fg-3 lg:inline">
              mock
            </span>
          ) : null}
        </div>

        <div className="flex min-w-0 flex-1 items-center gap-2.5 border-l border-line pl-5 max-[768px]:border-l-0 max-[768px]:pl-2">
          <h1 className="truncate whitespace-nowrap text-[15px] font-semibold tracking-[-0.01em] text-fg">{title}</h1>
          <span className="hidden truncate text-xs text-fg-3 md:inline">{sub}</span>
        </div>

        <div className="flex min-w-0 items-center justify-end gap-2 border-l border-line pl-4 pr-6 max-[1024px]:border-l-0 max-[1024px]:pr-4 max-[768px]:shrink-0 max-[768px]:pl-2 max-[768px]:pr-3">
          <UserMenu />
        </div>
      </header>

      {/* 三列主体：main 写在最前，用 grid-column 摆到第 2 列 */}
      <div className="grid flex-1 grid-cols-[240px_minmax(0,1fr)_288px] items-stretch max-[1024px]:grid-cols-[240px_minmax(0,1fr)] max-[768px]:grid-cols-[minmax(0,1fr)]">
        <main
          id="main"
          className="col-start-2 row-start-1 flex min-w-0 flex-col gap-4 p-6 max-[768px]:col-start-1 max-[768px]:row-start-2 max-[768px]:p-4"
        >
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

        {/* 左栏（col 1）：sticky，高度锁一屏（页头 56 + 页脚 40） */}
        <aside
          aria-label="主导航"
          className="col-start-1 row-start-1 self-stretch border-r border-line bg-surface max-[768px]:hidden"
        >
          {/* 外层撑满行高 → 三列等高；内层 sticky → 导航不随主内容滚走 */}
          <div className="sticky top-[56px] flex h-[calc(100vh-96px)] flex-col overflow-y-auto pb-4 pt-1.5">
            <NavList />
            <div className="mt-auto border-t border-line px-5 pb-2 pt-4 text-2xs text-fg-3">
              {USE_MOCK ? '示意原型 · 数据为 mock' : 'TestPlane · 实时数据'}
            </div>
          </div>
        </aside>

        {/* 右栏（col 3）：外层撑满行高（等高），内层 sticky 且作为 portal 目标 */}
        <aside
          aria-label="辅助信息"
          className="col-start-3 row-start-1 min-w-0 self-stretch border-l border-line bg-surface max-[1024px]:hidden"
        >
          <div
            id="right-rail"
            className="sticky top-[56px] flex h-[calc(100vh-96px)] flex-col gap-4 overflow-y-auto p-5 [&:empty]:hidden"
          />
        </aside>
      </div>

      <footer className="sticky bottom-0 z-30 flex h-10 shrink-0 items-center gap-3 border-t border-line bg-surface px-6 text-xs text-fg-2 max-[768px]:px-4">
        <span>TestPlane · {USE_MOCK ? '示意原型，数据为 mock，操作不产生真实执行' : '控制台'}</span>
        <span className="ml-auto hidden text-fg-3 sm:inline">API: /api/v1</span>
      </footer>

      <Drawer open={mobileNavOpen} onClose={() => setMobileNavOpen(false)} title="导航" widthClass="max-w-[16rem]" side="left">
        <NavList onNavigate={() => setMobileNavOpen(false)} />
      </Drawer>
    </div>
  )
}