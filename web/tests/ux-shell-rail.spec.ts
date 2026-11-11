import { expect, test } from '@playwright/test'

/**
 * 外壳与右栏改造（用户 2026-11-06 拍板）：
 *  A. **删掉全局 footer**（含 `API: /api/v1` 链接），栅格从 header/body/footer 三段变两段；
 *  B. **右栏只在概览保留**，Run 详情的事件流搬进「事件」页签，其余页无栏；
 *  C. Tester 目录**按 name 聚合**，历史版本折叠在「另有 N 个历史版本」里；
 *  D. 404 页不再有第二个 `role="main"`，改用 `section[aria-labelledby]` + `document.title`。
 */

test.describe.configure({ mode: 'serial' })

// TODO(e2e): 待集群联调 —— v2 侧栏底部新增了 文档/API/GitHub 三个入口（AppSidebar footer），
// 本用例"没有 API 链接"的前提已不成立；按 v2 信息架构重写后再跑。
test.fixme('A 全局 footer 已删除：没有 footer 元素、没有 API 链接、主内容吃到底', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Task')).toBeVisible()

  await expect(page.locator('footer')).toHaveCount(0)
  await expect(page.getByTestId('sidebar-footer-API')).toHaveCount(0)
  await expect(page.getByText('/api/v1')).toHaveCount(0)

  // 两段栅格：主体（inset 内容区）底边贴住视口底边，只留 inset 的 8px 页边距
  const m = await page.evaluate(() => ({
    shellBottom: Math.round((document.querySelector('[data-rail]') as HTMLElement).getBoundingClientRect().bottom),
    mainBottom: Math.round((document.querySelector('#main') as HTMLElement).getBoundingClientRect().bottom),
    vh: window.innerHeight,
  }))
  expect(m.shellBottom).toBeGreaterThanOrEqual(m.vh - 1)
  expect(m.mainBottom).toBeGreaterThanOrEqual(m.vh - 10)
})

test('B 全站无右栏（D-62）：所有页面都是两列，概览把队列摘要压成一行', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))

  // 概览：KPI + 队列摘要（原右栏内容压成一行）+ 全量 Task 表，且没有右栏
  await page.goto('/')
  await expect(page.getByText('进行中 Task')).toBeVisible()
  await expect(page.locator('aside[aria-label="辅助信息"]')).toHaveCount(0)
  await expect(page.getByTestId('queue-summary')).toContainText('排队中的 runner')
  await expect(page.getByTestId('queue-summary').getByTestId('queue-link')).toBeVisible()
  await expect(page.getByText('全部 Task')).toBeVisible()
  await expect(page.getByTestId('pagination-total')).toBeVisible()
  // 原来的右栏二卡（快捷操作 / 这页怎么看）已经并进页头动作区
  await expect(page.getByTestId('new-task')).toBeVisible()
  await expect(page.getByTestId('go-runners')).toBeVisible()

  // 其余页面：同样无栏
  for (const path of ['/submit', '/runners', '/clusters', '/admin', '/tasks/t-20261105-0003/report', '/tasks/t-20261105-0003']) {
    await page.goto(path)
    await page.waitForTimeout(250)
    await expect(page.locator('[data-rail="off"]'), `${path} 应当是无栏布局`).toHaveCount(1)
    await expect(page.locator('aside[aria-label="辅助信息"]'), `${path} 不该有右栏`).toHaveCount(0)
  }

  // 无栏时主内容右边界贴住视口（inset 变体有 8px 页边距）
  const m = await page.evaluate(() => ({
    mainRight: Math.round((document.querySelector('#main') as HTMLElement).getBoundingClientRect().right),
    vw: window.innerWidth,
  }))
  expect(m.mainRight).toBe(m.vw - 8)
})

test('B Run 详情：事件流并入「执行细节」页签，切页签不重连（SSE 只挂一次）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')

  // 终态 Run：结果 / 执行细节 两个页签（事件不再是独立页签）
  await expect(page.getByRole('tab', { name: '结果' })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('tab', { name: '执行细节' })).toBeVisible()
  await expect(page.getByRole('tab', { name: '事件' })).toHaveCount(0)

  // 默认在结果视图：事件卡不渲染（页签语义：只有选中页签挂 panel）
  await expect(page.getByTestId('task-events-panel')).toHaveCount(0)

  await page.getByRole('tab', { name: '执行细节' }).click()
  const panel = page.getByTestId('task-events-panel')
  await expect(panel).toBeVisible()
  await expect(panel).toContainText('执行事件')
  await expect(panel).toContainText('历史事件')

  // 切到结果再切回：事件条数与第一次一致（没有因为重连而清空/重复）
  const firstCount = await panel.locator('li').count()
  await page.getByRole('tab', { name: '结果' }).click()
  await expect(panel).toHaveCount(0)
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByTestId('task-events-panel').locator('li')).toHaveCount(firstCount)
})

test('B 运行中的 Run：事件就在执行细节里（无独立页签）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0005') // deploying
  // v2：运行中默认就是「执行细节」页签；事件并入其中
  await expect(page.getByText('runner 管道时间线')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('tab', { name: '事件' })).toHaveCount(0)
  await expect(page.getByRole('tab', { name: '结果' })).toHaveCount(0)
  await expect(page.getByTestId('task-events-panel')).toBeVisible()
})

test('C Runner 目录按 name 聚合：一行一个名字，历史版本折叠可展开', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/testers')
  await expect(page.getByText('已注册 Runner')).toBeVisible()

  const rows = page.locator('tbody tr')
  await expect(rows.first()).toBeVisible({ timeout: 15_000 })

  // aisbench-sla 只占一行（不是两个版本并列）
  const slaRows = page.locator('tbody tr', { hasText: 'aisbench-sla' })
  await expect(slaRows).toHaveCount(1)
  await expect(slaRows.first()).toContainText('0.2.0')

  // 展开历史版本：多一行，标「已退役」并说明历史 Task 仍按 name@version 引用
  const toggle = slaRows.first().getByTestId('toggle-older-versions')
  await expect(toggle).toHaveText(/另有 1 个历史版本/)
  await expect(toggle).toHaveAttribute('aria-expanded', 'false')
  await toggle.click()
  await expect(toggle).toHaveAttribute('aria-expanded', 'true')

  const withOlder = page.locator('tbody tr', { hasText: 'aisbench-sla' })
  await expect(withOlder).toHaveCount(2)
  const older = withOlder.nth(1)
  await expect(older).toContainText('0.1.0')
  await expect(older).toContainText('已退役')
  await expect(older).toContainText('历史 Task 仍按 name@version 引用')

  // 行主位的版本号仍能打开抽屉
  await page.locator('[data-testid="runner-version"]').first().click()
  await expect(page).toHaveURL(/open=/)
})

test('D 404：没有第二个 main landmark，标题与 aria-labelledby 正确', async ({ page }) => {
  await page.goto('/does-not-exist')

  const notFound = page.getByTestId('not-found')
  await expect(notFound).toBeVisible()

  // 全页只有一个 main（AppShell 的 #main）
  await expect(page.locator('main')).toHaveCount(1)
  await expect(notFound).not.toHaveAttribute('role', 'main')
  await expect(notFound).toHaveAttribute('aria-labelledby', 'not-found-title')
  await expect(page.locator('#not-found-title')).toContainText('404')
  await expect(page).toHaveTitle('404 · 页面不存在')
})

test('E 侧栏：SidebarMenuButton 的 active 语义 + Ctrl+B 折叠 + cookie 持久', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))
  await page.goto('/')
  await expect(page.getByText('进行中 Task')).toBeVisible()

  const sidebar = page.locator('[data-slot="sidebar"][data-state]')
  await expect(sidebar).toHaveAttribute('data-state', 'expanded')
  await expect(sidebar).toHaveAttribute('data-variant', 'inset')

  // 导航项是 SidebarMenuButton 语义：当前页 = data-active=true（且只有一条）
  const active = page.locator('nav[aria-label="主导航"] [data-slot="sidebar-menu-button"][data-active="true"]')
  await expect(active).toHaveCount(1)
  await expect(active).toHaveText('概览')
  await expect(active).toHaveAttribute('aria-current', 'page')

  // 折叠按钮有可读名字（读屏可达）
  const toggle = page.getByRole('button', { name: '切换侧栏' })
  await expect(toggle).toBeVisible()

  // Ctrl+B 折叠：icon rail（内层 48–56），主内容左边界跟着变
  await page.keyboard.press('Control+b')
  await expect(sidebar).toHaveAttribute('data-state', 'collapsed')
  await expect(sidebar).toHaveAttribute('data-collapsible', 'icon')
  await page.waitForTimeout(350) // 宽度过渡 200ms，等它走完再量
  const collapsed = await page.evaluate(() => ({
    inner: Math.round((document.querySelector('[data-slot="sidebar-inner"]') as HTMLElement).getBoundingClientRect().width),
    mainLeft: Math.round((document.querySelector('#main') as HTMLElement).getBoundingClientRect().left),
    cookie: document.cookie,
  }))
  expect(collapsed.inner).toBeGreaterThanOrEqual(48)
  expect(collapsed.inner).toBeLessThanOrEqual(56)
  expect(collapsed.mainLeft).toBe(72)
  // 状态由组件自己写 cookie（localStorage 在 mock 里只放角色）
  expect(collapsed.cookie).toContain('sidebar_state=false')

  // 刷新后仍保持折叠（组件从 cookie 读回）
  await page.reload()
  await expect(page.getByText('进行中 Task')).toBeVisible()
  await expect(page.locator('[data-slot="sidebar"][data-state]')).toHaveAttribute('data-state', 'collapsed')

  await page.keyboard.press('Control+b')
  await expect(page.locator('[data-slot="sidebar"][data-state]')).toHaveAttribute('data-state', 'expanded')
})

test('F 移动端：汉堡打开 288px 抽屉，点导航后自动关闭', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/')
  await expect(page.getByText('进行中 Task')).toBeVisible()

  // 桌面侧栏在移动端不渲染
  await expect(page.locator('[data-slot="sidebar-container"]')).toHaveCount(0)

  const trigger = page.getByRole('button', { name: '切换侧栏' })
  await expect(trigger).toBeVisible()
  await trigger.click()

  const drawer = page.getByRole('dialog')
  await expect(drawer).toBeVisible()
  const box = await drawer.boundingBox()
  expect(Math.round(box?.width ?? 0)).toBe(288)

  await drawer.getByRole('link', { name: '集群与队列' }).click()
  await expect(page).toHaveURL(/\/clusters$/)
  await expect(page.getByRole('dialog')).toHaveCount(0)
})
