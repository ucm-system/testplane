import { expect, test } from '@playwright/test'

/**
 * 外壳与右栏改造（用户 2026-11-06 拍板）：
 *  A. **删掉全局 footer**（含 `API: /api/v1` 链接），栅格从 header/body/footer 三段变两段；
 *  B. **右栏只在概览保留**，Run 详情的事件流搬进「事件」页签，其余页无栏；
 *  C. Tester 目录**按 name 聚合**，历史版本折叠在「另有 N 个历史版本」里；
 *  D. 404 页不再有第二个 `role="main"`，改用 `section[aria-labelledby]` + `document.title`。
 */

test.describe.configure({ mode: 'serial' })

test('A 全局 footer 已删除：没有 footer 元素、没有 API 链接、主内容吃到底', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible()

  await expect(page.locator('footer')).toHaveCount(0)
  await expect(page.getByTestId('footer-api-link')).toHaveCount(0)
  await expect(page.getByText('/api/v1')).toHaveCount(0)

  // 两段栅格：主体底边贴住视口底边（原来下面是 40px 页脚）
  const m = await page.evaluate(() => ({
    shellBottom: Math.round((document.querySelector('[data-rail]') as HTMLElement).getBoundingClientRect().bottom),
    mainBottom: Math.round((document.querySelector('#main') as HTMLElement).getBoundingClientRect().bottom),
    vh: window.innerHeight,
  }))
  expect(m.shellBottom).toBeGreaterThanOrEqual(m.vh - 1)
  expect(m.mainBottom).toBeGreaterThanOrEqual(m.vh - 2)
})

test('B 右栏只在概览：/ 有栏，其余页面无栏（data-rail 与 aside 都对齐）', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  // 概览：有栏 + 三块内容（快捷操作 / 排队中 Task / 这页怎么看）
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible()
  await expect(page.locator('[data-rail="on"]')).toHaveCount(1)
  const rail = page.locator('aside[aria-label="辅助信息"]')
  await expect(rail).toBeVisible()
  await expect(rail).toContainText('快捷操作')
  await expect(rail).toContainText('排队中的 Task')

  // 其余页面：无栏，且没有任何右栏 aside
  for (const path of ['/runs', '/submit', '/testers', '/clusters', '/admin', '/runs/r-20261105-0003/report', '/runs/r-20261105-0003']) {
    await page.goto(path)
    await page.waitForTimeout(250)
    await expect(page.locator('[data-rail="off"]'), `${path} 应当是无栏布局`).toHaveCount(1)
    await expect(page.locator('aside[aria-label="辅助信息"]'), `${path} 不该有右栏`).toHaveCount(0)
  }

  // 无栏时主内容右边界贴视口
  const m = await page.evaluate(() => ({
    mainRight: Math.round((document.querySelector('#main') as HTMLElement).getBoundingClientRect().right),
    vw: window.innerWidth,
  }))
  expect(m.mainRight).toBe(m.vw)
})

test('B Run 详情：事件流在「事件」页签里，切页签不重连（SSE 只挂一次）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0003')

  // 终态 Run：结果 / 执行细节 / 事件 三个页签
  const eventTab = page.getByRole('tab', { name: '事件' })
  await expect(eventTab).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('tab', { name: '结果' })).toBeVisible()
  await expect(page.getByRole('tab', { name: '执行细节' })).toBeVisible()

  // 默认在结果视图：事件面板不渲染（页签语义：只有选中页签挂 panel）
  await expect(page.getByTestId('run-events-panel')).toHaveCount(0)

  await eventTab.click()
  const panel = page.getByTestId('run-events-panel')
  await expect(panel).toBeVisible()
  await expect(panel).toContainText('共')
  await expect(panel).toContainText('历史回放')

  // tabpanel 语义：id / role / aria-labelledby 对得上
  const panelId = await panel.getAttribute('id')
  expect(panelId).toBe('run-view-panel-events')
  await expect(panel).toHaveAttribute('role', 'tabpanel')
  await expect(panel).toHaveAttribute('aria-labelledby', 'run-view-tab-events')
  await expect(eventTab).toHaveAttribute('aria-controls', 'run-view-panel-events')
  await expect(eventTab).toHaveAttribute('aria-selected', 'true')

  // 切回执行细节再切回事件：事件条数与第一次一致（没有因为重连而清空/重复）
  const firstCount = await panel.locator('li').count()
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(panel).toHaveCount(0)
  await page.getByRole('tab', { name: '事件' }).click()
  await expect(page.getByTestId('run-events-panel').locator('li')).toHaveCount(firstCount)
})

test('B 运行中的 Run 也有「事件」页签（默认停在执行细节）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0005') // deploying
  await expect(page.getByText('Task 列表')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('tab', { name: '事件' })).toBeVisible()
  await expect(page.getByRole('tab', { name: '结果' })).toHaveCount(0)
  await page.getByRole('tab', { name: '事件' }).click()
  await expect(page.getByTestId('run-events-panel')).toBeVisible()
})

test('C Tester 目录按 name 聚合：一行一个名字，历史版本折叠可展开', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/testers')
  await expect(page.getByText('已注册 Tester')).toBeVisible()

  const rows = page.locator('tbody tr')
  await expect(rows.first()).toBeVisible({ timeout: 15_000 })

  // aisbench-sla 只占一行（不是两个版本并列）
  const slaRows = page.locator('tbody tr', { hasText: 'aisbench-sla' })
  await expect(slaRows).toHaveCount(1)
  await expect(slaRows.first()).toContainText('0.2.0')

  // 展开历史版本：多一行，标「已退役」并说明历史 Run 仍按 name@version 引用
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
  await expect(older).toContainText('历史 Run 仍按 name@version 引用')

  // 行主位的版本号仍能打开抽屉
  await page.locator('[data-testid="tester-version"]').first().click()
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
