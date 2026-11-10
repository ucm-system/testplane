import { useEffect } from 'react'
import { Link, useLocation } from 'react-router-dom'
import { BookOpen, Boxes, GitFork, Layers, List, PlusCircle, Server, ShieldCheck, Terminal } from 'lucide-react'
import { Wordmark } from '@/components/app/Wordmark'
import { USE_MOCK } from '@/api'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from '@/components/ui/shadcn/sidebar'
import { useIdentity } from '@/hooks/useIdentity'
import { hasRole, type Role } from '@/lib/roles'
import { cn } from '@/lib/utils'

interface NavItem {
  to: string
  label: string
  icon: typeof List
  /** 只有精确匹配才算 active（`/` 必须保留这个语义，否则所有页面都会点亮概览） */
  end?: boolean
  /** 导航项的最低角色（缺省 viewer）；不够就**不渲染**这一项 */
  minRole?: Exclude<Role, 'viewer'>
}

const NAV_GROUPS: Array<{ title: string; items: NavItem[] }> = [
  {
    title: 'Task',
    items: [
      { to: '/', label: '概览', icon: Layers, end: true },
      { to: '/submit', label: '提交 Task', icon: PlusCircle },
    ],
  },
  {
    title: '能力',
    items: [
      { to: '/runners', label: 'Runner 目录', icon: Boxes },
      // 集群与队列：**viewer 也可读**（design/views/07 §2.3：GET /clusters 与 /queue-status 是 viewer ✅）
      { to: '/clusters', label: '集群与队列', icon: Server },
    ],
  },
  {
    title: '管理',
    items: [{ to: '/admin', label: '管理台', icon: ShieldCheck, minRole: 'admin' }],
  },
]

/** 侧栏底部的三条入口：文档 / API 站内，GitHub 外链。 */
const FOOTER_LINKS: Array<{ label: string; to: string; icon: typeof List; external?: boolean }> = [
  { label: '文档', to: '/docs', icon: BookOpen },
  { label: 'API', to: '/api', icon: Terminal },
  // lucide 1.x 不含品牌图标：用 GitFork 表示仓库
  { label: 'GitHub', to: 'https://github.com/ucm-system/argus', icon: GitFork, external: true },
]

/** 路由是否命中导航项：`end` 项必须完全相等，其余按路径前缀匹配。 */
function isItemActive(pathname: string, item: NavItem): boolean {
  if (item.end) return pathname === item.to
  return pathname === item.to || pathname.startsWith(`${item.to}/`)
}

/**
 * 侧栏（shadcn `dashboard-01` 的 AppSidebar 骨架，`variant="inset"` + `collapsible="icon"`）。
 *
 * 纪律：
 *  - 组标题用 `SidebarGroupLabel`（小号 muted 标签），条目用 `SidebarMenuButton` + 16px 图标；
 *  - active 的状态走路由：`/` 仍然只有精确匹配才点亮（`end` 语义保留）；
 *  - 折叠/展开的过渡、icon rail 宽度、移动端 sheet 全部由 `ui/shadcn/sidebar` 默认样式负责，
 *    这里不写 transition；
 *  - 按角色过滤条目的逻辑与改造前一致（viewer 看不到「管理台」）。
 */
export function AppSidebar() {
  const { auth } = useIdentity()
  const { pathname } = useLocation()
  const { isMobile, setOpenMobile } = useSidebar()

  // 移动端：从 sheet 里点了导航就把它关掉（桌面端这里是 no-op）。
  useEffect(() => {
    if (isMobile) setOpenMobile(false)
  }, [pathname, isMobile, setOpenMobile])

  const groups = NAV_GROUPS.map((group) => ({
    ...group,
    items: group.items.filter((item) => (item.minRole ? hasRole(auth, item.minRole) : true)),
  })).filter((group) => group.items.length > 0)

  return (
    <Sidebar collapsible="icon" variant="inset">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild size="lg" tooltip="Argus" className="group-data-[collapsible=icon]:mx-auto">
              <Link to="/" aria-label="Argus 首页" className="flex items-center gap-2">
                {/*
                  品牌字标（SVG，图形 + 字母一体的身份标识）：
                  展开态显示完整 lockup，折叠成 icon rail 时只剩图形。
                */}
                <span data-testid="sidebar-brand" className="group-data-[collapsible=icon]:hidden">
                  <Wordmark height={26} />
                </span>
                <span
                  data-testid="sidebar-brand-icon"
                  aria-hidden
                  className="hidden h-6 shrink-0 items-center group-data-[collapsible=icon]:flex"
                >
                  <Wordmark height={20} />
                </span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <nav aria-label="主导航" className="flex min-h-0 flex-1 flex-col">
          {groups.map((group) => (
            <SidebarGroup key={group.title}>
              <SidebarGroupLabel>{group.title}</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  {group.items.map((item) => {
                    const active = isItemActive(pathname, item)
                    return (
                      <SidebarMenuItem key={item.to}>
                        <SidebarMenuButton asChild isActive={active} tooltip={item.label}>
                          <Link to={item.to} aria-current={active ? 'page' : undefined}>
                            <item.icon aria-hidden />
                            <span>{item.label}</span>
                          </Link>
                        </SidebarMenuButton>
                      </SidebarMenuItem>
                    )
                  })}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          ))}
        </nav>
      </SidebarContent>

      <SidebarFooter>
        {/*
          底部入口（shadcn dashboard-01 侧栏 footer 的那种小字链接）：
          文档 / API 走站内路由，GitHub 走外链。折叠成 icon rail（48px）时文字隐藏、
          只剩图标，靠 SidebarMenuButton 的 tooltip 兜住可读性。
        */}
        <SidebarMenu>
          {FOOTER_LINKS.map((link) => (
            <SidebarMenuItem key={link.label}>
              <SidebarMenuButton asChild size="sm" tooltip={link.label}>
                {link.external ? (
                  <a
                    href={link.to}
                    target="_blank"
                    rel="noreferrer"
                    aria-label={link.label}
                    data-testid={`sidebar-footer-${link.label}`}
                    className="text-fg-3 transition-colors hover:text-fg"
                  >
                    <link.icon aria-hidden />
                    <span className="group-data-[collapsible=icon]:hidden">{link.label}</span>
                  </a>
                ) : (
                  <Link
                    to={link.to}
                    aria-label={link.label}
                    data-testid={`sidebar-footer-${link.label}`}
                    className="text-fg-3 transition-colors hover:text-fg"
                  >
                    <link.icon aria-hidden />
                    <span className="group-data-[collapsible=icon]:hidden">{link.label}</span>
                  </Link>
                )}
              </SidebarMenuButton>
            </SidebarMenuItem>
          ))}
        </SidebarMenu>

        <p
          className={cn(
            'px-2 pb-1 text-2xs leading-[16px] text-fg-3',
            'group-data-[collapsible=icon]:hidden',
          )}
        >
          {USE_MOCK ? '示意原型 · 数据为 mock' : 'Argus · 实时数据'}
        </p>
      </SidebarFooter>
    </Sidebar>
  )
}
