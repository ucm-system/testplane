import { Link, useLocation } from 'react-router-dom'
import { ChevronRight, Plus } from 'lucide-react'
import { UserMenu } from '@/components/app/UserMenu'
import { Wordmark } from '@/components/app/Wordmark'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/shadcn/separator'
import { SidebarTrigger } from '@/components/ui/shadcn/sidebar'

/** 路由 → 页头标题（页面级信息在页头，操作也在页头）。 */
const TITLES: Array<{ test: (path: string) => boolean; title: string; sub: string }> = [
  // D-62：概览与运行列表合并 —— 副标题覆盖集群、排队与全部 Run
  { test: (p) => p === '/', title: '概览', sub: '集群忙不忙、谁在排队、以及所有 Run（可筛可翻页）' },
  // /runs 只做兼容重定向（D-62），落地页是概览
  { test: (p) => p === '/runs', title: '概览', sub: '正在打开全部 Run…' },
  // 报告页必须排在 `/runs/` 之前：否则 `/runs/:id/report` 会被「Run 详情」先命中（B1）
  { test: (p) => /^\/runs\/[^/]+\/report$/.test(p), title: '报告', sub: 'report.json 与产物' },
  { test: (p) => p.startsWith('/runs/'), title: 'Run 详情', sub: '状态由服务端推，UI 只读' },
  { test: (p) => p === '/submit', title: '提交运行', sub: '参数是一段字符串，平台只做词法切分' },
  { test: (p) => p === '/testers', title: 'Tester 目录', sub: '随镜像内置，平台启动时扫描目录自动发现' },
  { test: (p) => p === '/clusters', title: '集群与队列', sub: '只回答「卡在队列还是卡在平台槽位」' },
  { test: (p) => p === '/admin', title: '管理台', sub: 'Profile / 集群 / Secret 引用 / 审计（仅 admin）' },
  { test: (p) => p === '/404', title: '页面不存在', sub: '这个地址没有对应的页面' },
]

function titleFor(pathname: string): { title: string; sub: string } {
  const hit = TITLES.find((item) => item.test(pathname))
  return hit ?? { title: 'TestPlane', sub: '' }
}

/**
 * 站点页头（shadcn `dashboard-01` 的 SiteHeader）：sticky、h-14、`border-b border-border/60`。
 *
 * 左起：侧栏折叠按钮（移动端就是汉堡，点开 sheet）→ 面包屑（当前页名）；**不显示页面副标题**（用户 2026-11-09：
 * "集群忙不忙、谁在排队……"这类副标题是自说自话，删掉；面包屑本身就够说明在哪一页）；
 * 右端：「新建 Run」+ 用户菜单（角色徽章 / 登出 / dev 身份切换都还在 UserMenu 里）。
 * 折叠按钮的 `aria-label` 与移动端 sheet 的标题/描述由 sidebar 组件自己兜住。
 */
export function SiteHeader() {
  const { pathname } = useLocation()
  const { title } = titleFor(pathname)

  return (
    <header className="sticky top-0 z-30 flex h-14 shrink-0 items-center gap-2.5 border-b border-border/60 bg-background px-4">
      <SidebarTrigger />
      {/* 移动端侧栏在 sheet 里，页头留一个品牌入口（≥md 时品牌在侧栏顶部，这里隐藏） */}
      <Link to="/" aria-label="TestPlane 首页" className="flex shrink-0 items-center md:hidden">
        <Wordmark className="text-fg" height={20} />
      </Link>
      <Separator orientation="vertical" className="hidden h-4 md:block" />

      <nav aria-label="面包屑" className="min-w-0">
        <ol className="flex min-w-0 items-center gap-1.5 text-sm">
          <li className="hidden shrink-0 sm:block">
            <Link to="/" className="text-fg-3 transition-colors hover:text-fg">
              TestPlane
            </Link>
          </li>
          <li aria-hidden className="hidden shrink-0 items-center sm:flex">
            <ChevronRight className="size-3.5 text-fg-4" />
          </li>
          <li className="min-w-0">
            {/* 页头保留唯一的 h1（当前页名）：面包屑的最后一段就是页面标题 */}
            <h1 aria-current="page" className="truncate text-sm font-medium text-fg">
              {title}
            </h1>
          </li>
        </ol>
      </nav>

      <div className="ml-auto flex shrink-0 items-center gap-2">
        {/* 概览页的动作行里已经有「新建 Run」（本轮不改页面结构），这里不重复放一颗；
            其余页面由页头统一提供入口。 */}
        {pathname !== '/' ? (
          <Link to="/submit" aria-label="新建 Run">
            <Button variant="primary" size="sm" icon={<Plus className="size-3.5" aria-hidden />} data-testid="header-new-run">
              新建 Run
            </Button>
          </Link>
        ) : null}
        <UserMenu />
      </div>
    </header>
  )
}
