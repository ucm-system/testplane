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

/**
 * 角色门禁：viewer 只读。
 * mock 模式下用 `localStorage['testplane:mock-role']='viewer'` 注入 OIDC 角色
 * （会带假 csrf_token，前端按会话处理），再确认 Task 级取消按钮置灰且带提示。
 */
test('viewer：Task 取消按钮不可点', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('testplane:mock-role', 'viewer')
  })

  // mock 里这条 Run 处于 deploying、Task 全是 pending —— 都是可取消状态
  await page.goto('/runs/r-20261105-0005')
  await expect(page.getByText('Task 列表')).toBeVisible()

  const cancel = page.locator('[data-testid="cancel-task"]').first()
  await expect(cancel).toBeVisible()
  await expect(cancel).toBeDisabled()
  await expect(cancel).toHaveAttribute('title', '需要 operator 角色')

  // 顶栏能看到 viewer 身份（排障用）
  await expect(page.getByRole('button', { name: '账号与角色' })).toContainText('viewer')
})

test('operator：Task 取消按钮可点并走二次确认', async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem('testplane:mock-role', 'operator')
  })

  await page.goto('/runs/r-20261105-0005')
  await expect(page.getByText('Task 列表')).toBeVisible()

  const cancel = page.locator('[data-testid="cancel-task"]').first()
  await expect(cancel).toBeEnabled()
  await cancel.click()

  // 二次确认弹窗：说明"已完成的 Task 不受影响"
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('已完成的 Task 不受影响')
  await expect(page.getByRole('button', { name: '确认取消 Task' })).toBeVisible()
  // 这里不点确认：点了会改 mock 内存状态，后续断言没必要依赖它
})
