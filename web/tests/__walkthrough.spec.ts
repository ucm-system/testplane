import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/** 真后端 UI 走查：截图留档 + 关键元素断言（跑在嵌入式 dist 上） */
const OUT = join(process.cwd(), '.data', 'walkthrough')
const TASK = process.env.WALK_TASK ?? 't-20261111-HRTG'

test.describe.configure({ mode: 'serial' })

test('walkthrough: submit / detail / runners / admin', async ({ page }) => {
  mkdirSync(OUT, { recursive: true })
  await page.setViewportSize({ width: 1440, height: 1000 })

  // 1) 提交页：provider 卡片 + 示例块
  await page.goto('/submit')
  await expect(page.getByTestId('provider-card-genet-deploy')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('provider-card-mock-deploy')).toBeVisible()
  await expect(page.getByTestId('provider-card-existing-deploy')).toBeVisible()
  await expect(page.getByTestId('provider-card-service')).toBeVisible()
  // mock 卡片选中：input_format=jsonl + 无 clean
  await page.getByTestId('provider-card-mock-deploy').click()
  await expect(page.getByTestId('deploy-input-issues')).toHaveCount(0)
  await page.getByTestId('deploy-example').locator('summary').click()
  await expect(page.getByTestId('deploy-example')).toContainText('base_url')
  // runner 行示例
  await expect(page.getByTestId('input-1-example').locator('summary')).toBeVisible()
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT, '01-submit-provider-cards.png'), fullPage: true })

  // 2) 详情：结果视图默认 + runner 明细 + 测试结果渲染
  await page.goto(`/tasks/${TASK}`)
  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 30_000 })
  await expect(page.getByTestId('task-result-verdict')).toContainText('全部')
  await expect(page.getByTestId('task-result-runners')).toBeVisible()
  const render = page.getByTestId('task-result-render')
  const first = render.locator('[data-testid^="runner-result-"]').first()
  if ((await first.locator('button[aria-expanded="true"]').count()) === 0) {
    await first.locator('button[aria-expanded]').click()
  }
  await expect(first.locator('button[aria-expanded="true"]')).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT, '02-task-result.png'), fullPage: true })

  // 3) 执行细节：事件并入
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByText('runner 管道时间线')).toBeVisible()
  await expect(page.getByTestId('task-events-panel')).toContainText('执行事件')
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT, '03-task-detail-events.png'), fullPage: true })

  // 4) 重试按钮存在且指向提交页
  await expect(page.getByTestId('retry-task')).toBeVisible()
  await page.getByTestId('retry-task').click()
  await expect(page).toHaveURL(new RegExp('/submit\\?retry=' + TASK + '$'))
  await expect(page.getByTestId('retry-banner')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByLabel('Task 名称')).toHaveValue(/-retry$/)
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT, '04-submit-retry-prefill.png'), fullPage: true })

  // 5) Runner 目录：role 筛选 + mock-deploy 详情带示例
  await page.goto('/runners')
  await expect(page.getByTestId('runner-role-filter')).toBeVisible({ timeout: 20_000 })
  await page.getByTestId('runner-role-filter').getByRole('button', { name: 'deploy' }).click()
  const rows = page.locator('tbody tr')
  await expect(rows.first()).toContainText('deploy')
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT, '05-runners-filter.png'), fullPage: true })
  await page.locator('tbody tr', { hasText: 'mock-deploy' }).first().getByRole('button', { name: '详情' }).click()
  await expect(page.getByTestId('runner-drawer')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('runner-drawer')).toContainText('base_url')
  await page.waitForTimeout(300)
  await page.screenshot({ path: join(OUT, '06-runner-drawer-mock.png'), fullPage: true })

  // 6) 管理台挂载点：NFS 收进弹窗
  await page.goto('/admin')
  await page.getByRole('tab', { name: '挂载点' }).click()
  await expect(page.getByTestId('mounts-explainer')).toBeVisible({ timeout: 20_000 })
  await page.getByTestId('mounts-help-open').click()
  await expect(page.getByTestId('pv-pvc-yaml')).toBeVisible()
  await page.waitForTimeout(300)
  await page.screenshot({ path: join(OUT, '07-admin-mounts-dialog.png'), fullPage: true })
})
