import { expect, test } from '@playwright/test'

/**
 * Tester 详情抽屉的尺寸（用户反馈：原来的窄抽屉里参数说明看不清）。
 *
 * 口径：桌面宽度下抽屉是 `xl` 档（min(64rem, 96vw) ≈ 1024px），参数表「说明」列有足够宽度、
 * 「复制」按钮不换行。窄屏仍按 96vw 收窄，但不能退化成内容挤成一条。
 */
test.describe('Tester 详情抽屉尺寸', () => {
  test('桌面宽度：抽屉走 xl 档（≥900px），参数表可读', async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 950 })
    await page.goto('/testers')
    await page.getByRole('button', { name: '详情' }).first().click()

    const dialog = page.locator('[role=dialog]')
    await expect(dialog).toBeVisible()
    await expect(dialog).toHaveAttribute('data-drawer-size', 'xl')

    const box = await dialog.boundingBox()
    expect(box?.width ?? 0).toBeGreaterThanOrEqual(900)

    // 参数表：说明列有实际宽度（不是被压成零宽），复制按钮不换行
    const firstDescCell = page.locator('[data-testid=tester-drawer] table tbody tr td').nth(0)
    await expect(firstDescCell).toBeVisible()
    const descBox = await page
      .locator('[data-testid=tester-drawer] table tbody tr')
      .first()
      .locator('td')
      .nth(1)
      .boundingBox()
    expect(descBox?.width ?? 0).toBeGreaterThan(300)

    const copyBtn = page.locator('[data-testid=drawer-copy-param]').first()
    const copyBox = await copyBtn.boundingBox()
    expect(copyBox?.height ?? 0).toBeLessThan(44)
  })

  test('窄屏（900px）：抽屉仍可读，不出现横向溢出', async ({ page }) => {
    await page.setViewportSize({ width: 900, height: 900 })
    await page.goto('/testers')
    await page.getByRole('button', { name: '详情' }).first().click()

    const dialog = page.locator('[role=dialog]')
    await expect(dialog).toBeVisible()
    const box = await dialog.boundingBox()
    // 96vw 上限：900 * 0.96 = 864
    expect(box?.width ?? 0).toBeGreaterThanOrEqual(800)
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    )
    expect(overflow).toBeLessThanOrEqual(1)
  })
})
