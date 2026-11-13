import { expect, test } from '@playwright/test'

/**
 * 可达性 / 深链回归（B6 / B7 / B9 / C1 / C3 / C4）：
 *  ④ 弹窗焦点陷阱：Tab 不进背景、Esc 后焦点回到触发按钮；
 *  ⑤ 结果 / 执行细节页签有 role=tabpanel，且 id / aria-controls / aria-labelledby 配对；
 *  ⑥ `/runs?q=` 直接生效，改筛选写回 URL；
 *  ⑦ `/testers?open=` 直接打开抽屉，关闭后去掉参数；
 *  ⑧ 900px 宽时右栏收起，但事件流在主内容底部仍可展开查看；
 *  C3 Task 行有可见的「查看」按钮作为键盘入口。
 */

test.describe.configure({ mode: 'serial' })

test('④ 弹窗焦点陷阱：Tab 不进背景，Esc 关闭后焦点回到触发按钮', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0005')

  const trigger = page.getByTestId('cancel-task')
  await expect(trigger).toBeVisible({ timeout: 20_000 })
  await trigger.click()

  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('取消这个 Task？')
  // P2：模态语义必须出现在 DOM 上（不能被透传属性盖掉）
  await expect(dialog).toHaveAttribute('aria-modal', 'true')

  // 打开后焦点落在弹窗内
  expect(
    await page.evaluate(() => Boolean(document.activeElement?.closest('[role="dialog"]'))),
    '打开弹窗后焦点应在弹窗内',
  ).toBe(true)

  // 连按 Tab：焦点始终不能跑到背景里
  for (let step = 1; step <= 8; step += 1) {
    await page.keyboard.press('Tab')
    const inside = await page.evaluate(() => Boolean(document.activeElement?.closest('[role="dialog"]')))
    expect(inside, `第 ${step} 次 Tab 后焦点仍应在弹窗内`).toBe(true)
  }

  // Esc 关闭 → 焦点归还给触发按钮
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await expect(trigger).toBeFocused()
})

test('⑤ 结果 / 执行细节：role=tabpanel 且 id 与页签互相指向', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')

  const resultTab = page.getByRole('tab', { name: '结果' })
  await expect(resultTab).toBeVisible({ timeout: 20_000 })
  // v2 默认停在「报告」页签：也只有一个按需挂载的面板
  await expect(page.getByRole('tabpanel')).toHaveCount(1)

  // 配对断言针对活动页签：先切到「结果」
  await resultTab.click()
  const panel = page.getByRole('tabpanel')
  const tabId = await resultTab.getAttribute('id')
  const panelId = await panel.getAttribute('id')
  expect(tabId, '页签要有 id').toBeTruthy()
  expect(panelId, '面板要有 id').toBeTruthy()
  await expect(resultTab).toHaveAttribute('aria-controls', panelId!)
  await expect(panel).toHaveAttribute('aria-labelledby', tabId!)

  // P2：面板按需挂载，非活动页签不能留悬空的 aria-controls
  const detailTab = page.getByRole('tab', { name: '执行细节' })
  expect(await detailTab.getAttribute('aria-controls'), '非活动页签不应指向不存在的面板').toBeNull()

  // 切到「执行细节」后仍是配对的一组面板
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByRole('tabpanel')).toHaveCount(1)
  await expect(page.getByRole('tabpanel')).toContainText('runner 管道时间线')
  await expect(detailTab).toHaveAttribute(
    'aria-controls',
    (await page.getByRole('tabpanel').getAttribute('id'))!,
  )
  expect(await resultTab.getAttribute('aria-controls'), '切走后「结果」页签也不该悬空').toBeNull()
})

test('⑥ /runs?q= 只在本页内查找，改关键字会写回 URL', async ({ page }) => {
  // 列表已经是真分页（D-61）：q 不跨页、不改 total，只在当前页的条目里找
  await page.goto('/?page_size=50&q=qwen3-8b-nightly')
  await expect(page.getByText('全部 Task')).toBeVisible()

  const search = page.getByLabel('搜索名称 / ID')
  await expect(search).toHaveValue('qwen3-8b-nightly')

  const rows = page.locator('tbody tr')
  await expect(rows).toHaveCount(1)
  await expect(rows.first()).toContainText('qwen3-8b-nightly')
  // 明确标注"页内查找，不跨页"
  await expect(page.getByText(/页内查找，不跨页/)).toBeVisible()

  // 改关键字 → URL 跟着变，结果也跟着变
  await search.fill('daily')
  await expect(page).toHaveURL(/[?&]q=daily/)
  await expect(rows.first()).toContainText('qwen3-32b-daily')
  await expect(page.getByText('qwen3-8b-nightly')).toHaveCount(0)
})

test('⑦ /testers?open= 直接打开抽屉，关闭后 URL 去掉该参数', async ({ page }) => {
  await page.goto('/testers?open=aisbench-perf')

  const drawer = page.getByRole('dialog')
  await expect(drawer).toBeVisible({ timeout: 20_000 })
  await expect(drawer).toContainText('性能压测')

  await drawer.getByRole('button', { name: '关闭' }).click()
  await expect(drawer).toBeHidden()
  await expect(page).not.toHaveURL(/open=/)
})

test('⑧ 900px 宽：没有右栏了，事件流在「执行细节」页签里照样可达', async ({ page }) => {
  await page.setViewportSize({ width: 900, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')

  // 右栏只在概览页存在：Run 详情页压根没有 aside[aria-label="辅助信息"]
  await expect(page.locator('aside[aria-label="辅助信息"]')).toHaveCount(0)

  // 事件流并入「执行细节」页签（折叠 ≠ 隐藏，用户 2026-11-11）
  await page.getByRole('tab', { name: '执行细节' }).click()
  const panel = page.getByTestId('task-events-panel')
  await expect(panel).toBeVisible({ timeout: 20_000 })
  await expect(panel).toContainText(/共 \d+ 条/)
})

test('C3 runner 时间线节点可聚焦并用键盘打开 runner 日志面板', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')
  await page.getByRole('tab', { name: '执行细节' }).click()

  // v2：runner 管道时间线的节点就是键盘入口（button，aria-label=名称（状态））
  const node = page.locator('[data-chain-node] button').first()
  await expect(node).toBeVisible({ timeout: 20_000 })
  await node.focus()
  expect(await node.evaluate((el) => el === document.activeElement)).toBe(true)
  await page.keyboard.press('Enter')
  await expect(page.getByRole('log').first()).toBeVisible()
})
