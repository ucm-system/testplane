import { expect, test } from '@playwright/test'

/**
 * 最小 UI 冒烟（PR #18 / 验收 #16）：
 *   /runs -> 点第一条 -> 详情页的执行视图里出现 Task 列表
 *     （终态 Run 默认是「结果」视图，先切「执行细节」——折叠不是隐藏）
 *   /submit -> 故意把 Task 名称清空 -> dry-run 报错可见
 */

/** 终态 Run 默认落在「结果」视图：有页签就先切到「执行细节」 */
async function ensureExecutionView(page: import('@playwright/test').Page): Promise<void> {
  const detailTab = page.getByRole('tab', { name: '执行细节' })
  if ((await detailTab.count()) > 0) await detailTab.click()
}

test('runs list -> detail task table -> submit dry-run error', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('全部 Run')).toBeVisible()

  const firstRun = page.locator('tbody a[href^="/runs/"]').first()
  await expect(firstRun).toBeVisible()
  const href = await firstRun.getAttribute('href')
  expect(href).toMatch(/^\/runs\/.+/)

  await firstRun.click()
  await expect(page).toHaveURL(new RegExp(href!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
  await ensureExecutionView(page)
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

/** 相对亮度 + 对比度（WCAG AA：正文 ≥ 4.5:1） */
function contrastRatio(rgb1: [number, number, number], rgb2: [number, number, number]): number {
  const luminance = ([r, g, b]: [number, number, number]) => {
    const channel = (value: number) => {
      const v = value / 255
      return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
    }
    return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b)
  }
  const [light, dark] = [luminance(rgb1), luminance(rgb2)].sort((a, b) => b - a) as [number, number]
  return (light + 0.05) / (dark + 0.05)
}

function parseRgb(value: string): [number, number, number] {
  const match = /rgba?\(([^)]+)\)/.exec(value)
  const parts = (match?.[1] ?? '0,0,0').split(',').map((part) => Number(part.trim()))
  return [parts[0] ?? 0, parts[1] ?? 0, parts[2] ?? 0]
}

/**
 * 评审 #13：弱化文字色必须过 AA（4.5:1）。
 *
 * 只断言**对比度达标**，不断言具体色值字面量 —— 设计 tokens 由视觉精修维护（可以是 oklch / rgb / hex），
 * 测试只关心"读得清"。颜色先画到 canvas 再取像素，这样 oklch、color-mix、rgb 都能得到真实 sRGB。
 */
test('对比度：--color-fg-3 在白底上 ≥ 4.5:1（AA）', async ({ page }) => {
  await page.goto('/')
  const token = await page.evaluate(() => {
    const probe = document.createElement('span')
    probe.style.color = 'var(--color-fg-3)'
    document.body.appendChild(probe)
    const resolved = getComputedStyle(probe).color
    probe.remove()

    // 画到 canvas 上再读像素：把任何 CSS 颜色语法（oklch / color() / rgb / hex）换算成 sRGB
    const canvas = document.createElement('canvas')
    canvas.width = 1
    canvas.height = 1
    const ctx = canvas.getContext('2d')
    ctx.fillStyle = resolved
    ctx.fillRect(0, 0, 1, 1)
    const [r, g, b] = Array.from(ctx.getImageData(0, 0, 1, 1).data)
    return { resolved, rgb: [r, g, b] }
  })

  const [r, g, b] = token.rgb as [number, number, number]
  const ratio = contrastRatio([r, g, b], [255, 255, 255])
  // eslint-disable-next-line no-console
  console.log(`fg-3 ${token.resolved} → rgb(${r},${g},${b}) ratio=${ratio.toFixed(2)}:1`)
  expect(ratio, '弱化文字与白底的对比度必须 ≥ 4.5:1').toBeGreaterThanOrEqual(4.5)
})

/**
 * 评审 #15：删除必须二次确认 —— 取消不删、确认才删。
 */
test('admin：删除 Profile 有二次确认（取消不删、确认才删）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
  await page.goto('/admin')
  await expect(page.getByText('Execution Profiles')).toBeVisible()

  const rows = page.locator('tbody tr')
  // 先等数据行出现（卡头比数据先渲染，直接 count 会拿到 0）
  await expect(rows.first()).toBeVisible({ timeout: 15_000 })
  const before = await rows.count()
  expect(before).toBeGreaterThan(0)

  // 1) 点删除 → 只出对话框，数据还在
  await page.locator('[data-testid="delete-profile"]').first().click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByRole('dialog')).toContainText('删除这个 Profile？')
  await page.getByRole('button', { name: '取消', exact: true }).click()
  await expect(page.getByRole('dialog')).toBeHidden()
  await expect(rows).toHaveCount(before)

  // 2) 再点删除 → 确认 → 少一行
  await page.locator('[data-testid="delete-profile"]').first().click()
  await page.locator('[data-testid="confirm-delete"]').click()
  await expect(page.getByRole('dialog')).toBeHidden()
  await expect
    .poll(async () => rows.count(), { message: '确认后列表应该少一行' })
    .toBe(before - 1)
})
