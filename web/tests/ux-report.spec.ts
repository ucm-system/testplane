import { expect, test } from '@playwright/test'

/**
 * Task 详情「结果」视图的回归（用户 2026-11-11 裁决：跑完点进来只要三样东西）：
 *  ① 旧报告链接 `/tasks/:id/report` 重定向回详情（「报告」页签已取消，D-74）；
 *  ② 结果视图：结论 / runner 明细（谁成功谁失败）/ 测试结果三选一内联渲染 / 产物下载；
 *  ③ 测试结果内联渲染 result.json 指标（不编数据；deploy/clean 不进该卡）；
 *  C6 0 失败：runner 明细全绿，无错误码；
 *  B12 概览有「新建 Task」入口。
 */

test.describe.configure({ mode: 'serial' })

test('① 旧报告链接重定向回 Task 详情（终态默认「结果」页签）', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003/report')

  // 重定向：URL 不再是 /report，页面是详情的「结果」视图
  await expect(page).toHaveURL(/\/tasks\/t-20261105-0003$/, { timeout: 15_000 })
  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 20_000 })
  // 「报告」页签与整页报告标题都不存在了
  await expect(page.getByRole('tab', { name: '报告' })).toHaveCount(0)
  await expect(page.locator('header h1')).not.toHaveText('报告')
})

test('② 结果视图：结论 + runner 明细 + 测试结果（三选一渲染）+ 产物下载', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  // mock 里这条是 completed_with_errors（部分失败）
  await page.goto('/tasks/t-20261105-0003')

  // 默认就在「结果」页签
  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('task-result-verdict')).toContainText('失败')

  // 结论：谁成功谁失败在结论一句话里（明细卡已删——与结论重复，用户 2026-11-11）
  await expect(page.getByTestId('task-result-verdict')).toContainText('个 runner 失败')
  await expect(page.getByTestId('task-result-runners')).toHaveCount(0)

  // 测试结果：失败 runner 默认展开（内联渲染），成功 runner 折叠
  const render = page.getByTestId('task-result-render')
  await expect(render).toBeVisible()
  // 产物清单是异步加载的：等第一批 runner-result 面板渲染出来再数/点（CI 慢机竞态）
  const first = render.locator('[data-testid^="runner-result-"]').first()
  await expect(first).toBeVisible({ timeout: 20_000 })
  const expandedState = await first.locator('button[aria-expanded]').getAttribute('aria-expanded')
  if (expandedState === 'false') await first.locator('button[aria-expanded]').click()
  await expect(first.locator('button[aria-expanded="true"]')).toBeVisible()

  // 产物下载：显式列全（report.json / metadata.json / events.jsonl / stdout.log）
  for (const key of ['deploy', 'tests', 'reports']) {
    await page.getByTestId(`artifact-group-${key}`).locator('summary').click()
  }
  const downloads = page.locator('[data-testid="artifact-download"]')
  expect(await downloads.count()).toBeGreaterThan(3)
  await expect(page.getByText('report.json', { exact: false }).first()).toBeVisible()

  // 产物的「查看」进整页查看器（不再弹窗、不再裸链 API URL）
  await page.locator('[data-testid="artifact-view"]').first().click()
  await expect(page).toHaveURL(/\/file\?path=/, { timeout: 15_000 })
})

test('③ 测试结果内联渲染 result.json 指标（不编数据；deploy/clean 不进该卡）', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0006') // 全部成功

  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 20_000 })
  const render = page.getByTestId('task-result-render')
  await expect(render).toBeVisible()

  // 成功 runner 默认折叠：全部展开（第一个可能是 pre_check 探针，指标为空属正常），断言任一指标表有 accuracy
  const panels = render.locator('[data-testid^="runner-result-"]')
  // 先等产物清单就位（第一批面板渲染完），否则 count 会读到 0（CI 慢机竞态）
  await expect(panels.first()).toBeVisible({ timeout: 20_000 })
  await page.waitForTimeout(300)
  const count = await panels.count()
  for (let i = 0; i < count; i += 1) {
    const panel = panels.nth(i)
    if ((await panel.locator('button[aria-expanded="true"]').count()) === 0) {
      await panel.locator('button[aria-expanded]').click()
    }
  }
  const metrics = render.getByTestId('metrics-table').first()
  // CI 机器慢：5 个面板逐个展开 + 每个 result 一次 mock fetch（120ms/个），10s 偶发不够
  await expect(metrics).toBeVisible({ timeout: 30_000 })
  await expect(metrics).toContainText('accuracy')

  // deploy 也有详情面板（用户 2026-11-11：deploy/precheck 都要能看）
  await expect(render).toContainText('service-deploy')
})

test('C6 0 失败：结论全绿，测试结果/产物下载照常', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0006')

  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('task-result-verdict')).toContainText('全部')
  // 全部通过：结论里没有失败名单
  await expect(page.getByTestId('task-result-verdict')).not.toContainText('失败：')
  await expect(page.getByTestId('task-result-artifacts')).toBeVisible()
})

test('B12 概览有「新建 Task」入口，点了去 /submit', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/')
  const button = page.getByTestId('new-task')
  await expect(button).toBeVisible()
  await expect(button).toHaveText('新建 Task')
  await button.click()
  await expect(page).toHaveURL(/\/submit$/)
})
