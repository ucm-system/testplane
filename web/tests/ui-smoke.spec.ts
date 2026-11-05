import { expect, test } from '@playwright/test'

/**
 * 最小 UI 冒烟（PR #18 / 验收 #16）：
 *   /runs -> 点第一条 -> 详情页出现 Task 列表
 *   /submit -> 故意把 Task 名称清空 -> dry-run 报错可见
 */
test('runs list -> detail task table -> submit dry-run error', async ({ page }) => {
  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()

  const firstRun = page.locator('tbody a[href^="/runs/"]').first()
  await expect(firstRun).toBeVisible()
  const href = await firstRun.getAttribute('href')
  expect(href).toMatch(/^\/runs\/.+/)

  await firstRun.click()
  await expect(page).toHaveURL(new RegExp(href!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
  await expect(page.getByText('Task 列表')).toBeVisible()
  await expect(page.locator('table').first()).toBeVisible()

  await page.goto('/submit')
  await expect(page.getByText('提交前会先 dry-run', { exact: false })).toBeVisible()

  // 清空 Task 1 名称，让 dry-run 返回校验错误（页面应显示“校验未通过” Banner）。
  const taskName = page.getByLabel('Task 1 名称')
  await expect(taskName).toBeVisible()
  await taskName.fill('')
  await page.getByRole('button', { name: '校验', exact: true }).click()

  // dry-run 的校验错误以 alert 呈现（ErrorState：VALIDATION_ERROR / 提交内容未通过校验）
  const alert = page.getByRole('alert').first()
  await expect(alert).toBeVisible()
  await expect(alert).toContainText(/VALIDATION_ERROR|未通过校验/)
})
