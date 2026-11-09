import { useEffect } from 'react'
import { Link, useLocation } from 'react-router-dom'
import { Boxes, Layers, List, PlusCircle, Server, ShieldCheck } from 'lucide-react'
import { USE_MOCK } from '@/api'
import { Wordmark } from '@/components/app/Wordmark'
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
    title: '运行',
    items: [
      { to: '/', label: '概览', icon: Layers, end: true },
      { to: '/submit', label: '提交运行', icon: PlusCircle },
    ],
  },
  {
    title: '能力',
    items: [
      { to: '/testers', label: 'Tester 目录', icon: Boxes },
      // 集群与队列：**viewer 也可读**（design/views/07 §2.3：GET /clusters 与 /queue-status 是 viewer ✅）
      { to: '/clusters', label: '集群与队列', icon: Server },
    ],
  },
  {
    title: '管理',
    items: [{ to: '/admin', label: '管理台', icon: ShieldCheck, minRole: 'admin' }],
  },
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
            <SidebarMenuButton asChild size="lg" tooltip="TestPlane" className="group-data-[collapsible=icon]:mx-auto">
              <Link to="/" aria-label="TestPlane 首页" className="flex items-center gap-2">
                <Wordmark className="w-auto! text-fg group-data-[collapsible=icon]:hidden" height={24} />
                <span
                  aria-hidden
                  className="hidden size-8 shrink-0 place-items-center rounded-md bg-primary-chip text-sm font-semibold text-primary-text group-data-[collapsible=icon]:grid"
                >
                  TP
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
        <p
          className={cn(
            'px-2 pb-1 text-2xs leading-relaxed text-fg-3',
            'group-data-[collapsible=icon]:hidden',
          )}
        >
          {USE_MOCK ? '示意原型 · 数据为 mock' : 'TestPlane · 实时数据'}
        </p>
      </SidebarFooter>
    </Sidebar>
  )
}
