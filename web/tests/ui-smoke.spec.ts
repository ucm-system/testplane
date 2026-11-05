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

  // 终态 Run 的「重试」同样只对 operator 开放
  await page.goto('/runs/r-20261105-0003')
  const retry = page.locator('[data-testid="retry-run"]').first()
  await expect(retry).toBeVisible()
  await expect(retry).toBeDisabled()
  await expect(retry).toHaveAttribute('title', '需要 operator 角色')

  // 非 admin 打开 /admin：留在原地显示无权限，不跳走
  await page.goto('/admin')
  await expect(page.getByText('无权限：本页仅 admin 可见')).toBeVisible()
  await expect(page).toHaveURL(/\/admin$/)
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

/**
 * 管理台页签的**键盘可达性**（评审 #13）：roving tabindex + 方向键。
 * `admin` 角色下打开 /admin，用 ArrowRight 从 Profiles 走到 Audit，面板要跟着换。
 */
test('admin：键盘 ←/→ 切换页签到 Audit', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
  await page.goto('/admin')
  await expect(page.getByText('Execution Profiles')).toBeVisible()

  const profilesTab = page.getByRole('tab', { name: 'Profiles' })
  await profilesTab.focus()
  await expect(profilesTab).toHaveAttribute('aria-selected', 'true')

  // 方向键切页签（不是 Tab 键）
  await page.keyboard.press('ArrowRight') // Clusters
  await page.keyboard.press('ArrowRight') // Secrets
  await page.keyboard.press('ArrowRight') // Audit
  const auditTab = page.getByRole('tab', { name: 'Audit' })
  await expect(auditTab).toHaveAttribute('aria-selected', 'true')
  await expect(auditTab).toBeFocused()
  await expect(page.getByRole('tabpanel')).toContainText('审计日志')

  // Home / End 回到两端
  await page.keyboard.press('Home')
  await expect(page.getByRole('tab', { name: 'Profiles' })).toHaveAttribute('aria-selected', 'true')
  await page.keyboard.press('End')
  await expect(page.getByRole('tab', { name: 'Audit' })).toHaveAttribute('aria-selected', 'true')
})
