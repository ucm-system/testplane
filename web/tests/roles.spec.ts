import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 角色可见性矩阵（design/views/07-api.md §2.3 + 用户裁决）：
 *
 * | 导航项 | viewer | operator | admin |
 * |---|---|---|---|
 * | 概览 / 运行列表 / 提交运行（按钮置灰） | ✅ | ✅ | ✅ |
 * | Tester 目录 | ✅ | ✅ | ✅ |
 * | 集群与队列（只读水位与原因） | ✅（设计：viewer 可读） | ✅ | ✅ |
 * | 管理台（Profile/Cluster/Secret/审计） | — | — | ✅ |
 */

const OUT_DIR = join(process.cwd(), '.data', 'layout')

test.describe.configure({ mode: 'serial' })

async function navLabels(page: import('@playwright/test').Page): Promise<string[]> {
  const nav = page.locator('aside[aria-label="主导航"]')
  await expect(nav).toBeVisible()
  return nav.locator('a').allInnerTexts()
}

test('viewer：能看运行列表与集群队列，看不到管理台', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'viewer'))
  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()

  const labels = (await navLabels(page)).map((text) => text.trim())
  expect(labels).toContain('运行列表')
  expect(labels).toContain('集群与队列')
  expect(labels).not.toContain('管理台')

  // 集群与队列对 viewer 是**正常视图**（不是无权限页），且只读
  await page.goto('/clusters')
  await expect(page.getByText('Volcano 队列')).toBeVisible()
  await expect(page.getByText('无权限：集群与队列仅 operator 及以上可见')).toHaveCount(0)

  // 直接访问 /admin：留在原地显示无权限
  await page.goto('/admin')
  await expect(page.getByText('无权限：本页仅 admin 可见')).toBeVisible()
  await expect(page).toHaveURL(/\/admin$/)
})

test('operator：能看集群队列，仍看不到管理台', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()

  const labels = (await navLabels(page)).map((text) => text.trim())
  expect(labels).toContain('集群与队列')
  expect(labels).not.toContain('管理台')

  await page.goto('/clusters')
  await expect(page.getByText('Volcano 队列')).toBeVisible()
  await page.goto('/admin')
  await expect(page.getByText('无权限：本页仅 admin 可见')).toBeVisible()
})

test('admin：能看到管理台', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
  mkdirSync(OUT_DIR, { recursive: true })
  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()

  const labels = (await navLabels(page)).map((text) => text.trim())
  expect(labels).toContain('集群与队列')
  expect(labels).toContain('管理台')

  await page.getByRole('link', { name: '管理台' }).click()
  await expect(page.getByText('Execution Profiles')).toBeVisible()
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '07-nav-admin.png'), fullPage: false })
})
