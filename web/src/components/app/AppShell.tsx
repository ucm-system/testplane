import { Suspense } from 'react'
import { Outlet } from 'react-router-dom'
import { AppSidebar } from '@/components/app/AppSidebar'
import { SiteHeader } from '@/components/app/SiteHeader'
import { SidebarInset, SidebarProvider } from '@/components/ui/shadcn/sidebar'

/**
 * 控制台外壳（shadcn `dashboard-01` 骨架）：
 *
 * ```
 * <SidebarProvider>
 *   <AppSidebar variant="inset" />   ← 可折叠 256px ↔ icon rail，Ctrl/Cmd+B
 *   <SidebarInset>
 *     <SiteHeader />                 ← sticky h-14：面包屑 + 新建 Task + 用户菜单
 *     <main id="main">…</main>
 *   </SidebarInset>
 * </SidebarProvider>
 * ```
 *
 * 两条**结构**纪律：
 *  1. 侧栏在 DOM 里先于主内容（shadcn 的骨架如此；移动端侧栏收进 Radix Dialog 的 sheet，
 *     键盘顺序是：跳转链接 → 折叠按钮 → 面包屑 → 主内容）；
 *  2. `hasRail` 恒 false —— 右栏（288px）全站取消（D-62），`data-rail="off"` 保留给布局断言；
 *     不要在这里重新引入右栏，页面容器与内容层也不要因为外壳改造而改结构。
 *
 * `SidebarInset` 在 registry 里是 `<main>`，这里渲染成 `div`，真正的 landmark 是下面这个
 * `<main id="main">`（全页唯一 main；测试与读屏都按这个来）。
 */
export function AppShell() {
  // D-62：概览也要整幅宽度承载全量 Task 表 —— 现在没有页面需要右栏
  const hasRail = false

  return (
    <SidebarProvider data-rail={hasRail ? 'on' : 'off'}>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-2 focus:top-2 focus:z-[70] focus:rounded-md focus:bg-surface focus:px-3 focus:py-1.5 focus:text-xs focus:shadow-overlay"
      >
        跳到主内容
      </a>

      <AppSidebar />

      <SidebarInset>
        <SiteHeader />

        <main
          id="main"
          className="flex min-h-0 min-w-0 flex-1 flex-col gap-4 overflow-y-auto p-6 max-[768px]:p-4 [&>*]:shrink-0"
        >
          <Suspense
            fallback={
              <div className="flex items-center justify-center rounded-xl border border-line bg-surface py-16 text-xs text-fg-3 shadow-xs">
                页面加载中…
              </div>
            }
          >
            <Outlet />
          </Suspense>

          {/* ≤1024px 时右栏被收成两列：页面把右栏内容 portal 到这里（主内容底部的可展开区块），
              折叠 ≠ 隐藏 —— 事件流与边界说明仍然可达（C4）。当前 hasRail 恒 false，这段是保底。 */}
          {hasRail ? <div id="rail-inline" data-testid="rail-inline" className="min-[1025px]:hidden" /> : null}
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}
