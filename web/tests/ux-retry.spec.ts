import { expect, test, type Page } from '@playwright/test'

/**
 * 重试 = 带参重新提交（D-76，用户 2026-11-11 裁决）：
 *  - 详情页「重试」按钮 → `/submit?retry=<taskId>`；
 *  - 提交原文（GET /tasks/{id}/manifest）回填进表单，**可修改**；
 *  - 提交后生成**新 Task（新 ID）**，原 Task 保留不动。
 *
 * attempt 级重放（§7.8 / D-69）仍保留在 API/CLI：UI 只暴露这一种「重试」语义。
 */

test.describe.configure({ mode: 'serial' })

async function openRetry(page: Page, taskId: string): Promise<void> {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto(`/submit?retry=${taskId}`)
  // 回填要等 runner 目录与 manifest 都到：以名字出现为准
  await expect(page.getByTestId('retry-banner')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByLabel('runner 1 Runner')).toContainText('aisbench', { timeout: 15_000 })
}

test('① 详情页终态的「重试」跳提交页并回填提交原文（名字加 -retry 后缀）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')
  const retry = page.getByTestId('retry-task')
  await expect(retry).toBeVisible({ timeout: 20_000 })
  await retry.click()
  await expect(page).toHaveURL(/\/submit\?retry=t-20261105-0003$/)

  await openRetry(page, 't-20261105-0003')

  // 名字回填 + 后缀（D-58：不超 200 rune）
  await expect(page.getByLabel('Task 名称')).toHaveValue(/-retry$/)
  // 原 Task 名字在横幅里可见，可跳回原 Task
  await expect(page.getByTestId('retry-banner')).toContainText('t-20261105-0003')
})

test('② 回填的表单可修改；提交生成新 Task（新 ID），原 Task 不动', async ({ page }) => {
  await openRetry(page, 't-20261105-0003')

  // 改名字 + 改 runner 输入（回填内容是可编辑的普通表单）
  await page.getByLabel('Task 名称').fill('retry-edited-name')
  await page.getByLabel('runner 1 输入').fill('{"dataset":"/data/gsm8k.jsonl","limit":100}')

  await page.getByTestId('submit-task').click()

  // toast：新 Task（重试自原 Task）
  const toast = page.locator('[data-sonner-toaster]')
  await expect(toast).toContainText('已生成新 Task', { timeout: 15_000 })
  await expect(toast).toContainText('重试自 t-20261105-0003')

  // 跳到新 Task 详情（URL 里的 id ≠ 原 id）
  await page.getByTestId('toast-open-task').click()
  await expect(page).toHaveURL(/\/tasks\/t-\d{8}-[A-Z0-9]{4}$/)
  const url = page.url()
  expect(url).not.toContain('t-20261105-0003')
  // 新 Task 的名字是改过的那个（页面内 h1；面包屑还有个「Task 详情」h1，用 name 精确匹配）
  await expect(page.getByRole('heading', { name: 'retry-edited-name', exact: true })).toBeVisible({ timeout: 20_000 })
})

test('③ retry=<不存在的 Task>：回填失败给错误横幅，不阻塞表单', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/submit?retry=t-99999999-XXXX')
  await expect(page.getByTestId('retry-banner-error')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('retry-banner-error')).toContainText('NOT_FOUND')
  // 表单仍然可用（默认空表单状态）
  await expect(page.getByTestId('submit-task')).toBeVisible()
})
