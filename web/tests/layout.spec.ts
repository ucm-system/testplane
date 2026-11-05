import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 布局与页头对齐的**量化**回归（用户连追三轮的点，别用"看着像"糊过去）：
 *  - 三列 `grid-cols-[240px_minmax(0,1fr)_288px]`，`<main>` 在 DOM 里先于两侧栏；
 *  - 页头左分隔线落在 x=240（=左栏右边缘），右分隔线落在 x=vw−288（=右栏左边缘）；
 *  - 1024 以下收右栏，768 以下堆叠。
 * 同时截三张响应式图 + 一张 Deploy 日志面板图。
 */

const OUT_DIR = join(process.cwd(), '.data', 'layout')

test.describe.configure({ mode: 'serial' })

test('三列布局与页头分隔线对齐（1280 / 900 / 480）', async ({ page }) => {
  test.setTimeout(120_000)
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  /* ---------- 1280：三列 ---------- */
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible()
  await page.waitForTimeout(500)

  const boxes = await page.evaluate(() => {
    const q = (sel: string) => document.querySelector(sel) as HTMLElement | null
    const rect = (el: Element | null) => (el ? el.getBoundingClientRect() : null)
    const header = q('header')
    const headerKids = header ? Array.from(header.children).map((el) => rect(el)) : []
    const body = q('main')?.parentElement ?? null
    const bodyKids = body ? Array.from(body.children).map((el) => el.tagName + ':' + (el.getAttribute('aria-label') ?? el.id)) : []
    return {
      vw: window.innerWidth,
      headerCols: headerKids.map((r) => (r ? { left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width) } : null)),
      bodyOrder: bodyKids,
      left: rect(q('aside[aria-label="主导航"]')),
      main: rect(q('main')),
      right: rect(q('aside[aria-label="辅助信息"]')),
    }
  })

  // DOM 顺序：main 必须在两侧栏之前
  expect(boxes.bodyOrder[0]).toBe('MAIN:main')

  // 左分隔线 = 左栏右边缘 = 240
  expect(boxes.headerCols[0]?.width).toBe(240)
  expect(Math.round(boxes.left!.left)).toBe(0)
  expect(Math.round(boxes.left!.right)).toBe(240)
  expect(boxes.headerCols[0]?.right).toBe(240)

  // 右分隔线 = 视口宽 − 288（1280 → 992）
  expect(boxes.vw - Math.round(boxes.right!.left)).toBe(288)
  expect(boxes.headerCols[2]?.left).toBe(boxes.vw - 288)

  // 三列同高（items-stretch）：main 与两侧栏顶边对齐
  expect(Math.round(boxes.main!.top)).toBe(Math.round(boxes.left!.top))

  console.log('measured@1280', JSON.stringify(boxes))
  await page.screenshot({ path: join(OUT_DIR, '01-three-columns-1280.png'), fullPage: true })

  /* ---------- 900：收右栏（两列） ---------- */
  await page.setViewportSize({ width: 900, height: 900 })
  await page.waitForTimeout(400)
  await expect(page.locator('aside[aria-label="辅助信息"]')).toBeHidden()
  const twoCol = await page.evaluate(() => ({
    mainLeft: Math.round((document.querySelector('main') as HTMLElement).getBoundingClientRect().left),
    navWidth: Math.round((document.querySelector('aside[aria-label="主导航"]') as HTMLElement).getBoundingClientRect().width),
  }))
  expect(twoCol.navWidth).toBe(240)
  expect(twoCol.mainLeft).toBe(240)
  await page.screenshot({ path: join(OUT_DIR, '02-two-columns-900.png'), fullPage: true })

  /* ---------- 480：堆叠（左栏收起、走抽屉） ---------- */
  await page.setViewportSize({ width: 480, height: 900 })
  await page.waitForTimeout(400)
  await expect(page.locator('aside[aria-label="主导航"]')).toBeHidden()
  const stacked = await page.evaluate(() => Math.round((document.querySelector('main') as HTMLElement).getBoundingClientRect().left))
  expect(stacked).toBe(0)
  await page.screenshot({ path: join(OUT_DIR, '03-stacked-480.png'), fullPage: true })
})

test('Deploy 日志面板：Pod 分页签 + gap 显式标记', async ({ page }) => {
  test.setTimeout(120_000)
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  await page.setViewportSize({ width: 1440, height: 1000 })
  // 这条 seeded Run 有 Deploy 阶段与多个 Pod
  await page.goto('/runs/r-20261105-0003')
  await expect(page.getByText('Deploy 日志')).toBeVisible({ timeout: 20_000 })

  const panel = page.locator('section', { hasText: 'Deploy 日志' }).first()
  await expect(page.getByRole('tab', { name: /全部 Pod/ })).toBeVisible()
  // 终端正文：等宽 + 有时间戳（注意别选到上面的 manifest YAML pre）
  await expect(page.getByText(/deploy accepted for/).first()).toBeVisible()
  await expect(page.locator('pre', { hasText: /Successfully pulled image/ }).first()).toBeVisible()
  // gap：Pod 重启过 → 必须显式标「此处不连续」
  await expect(page.getByText('此处不连续', { exact: false }).first()).toBeVisible()

  // 滚到底，让「此处不连续」完整进画面
  await page.evaluate(() => {
    const box = Array.from(document.querySelectorAll('div')).find((el) => el.className.includes('max-h-[22rem]'))
    if (box) box.scrollTop = box.scrollHeight
  })
  await page.waitForTimeout(300)
  await page.screenshot({ path: join(OUT_DIR, '04-deploy-logs.png'), fullPage: true })
  console.log(`screenshots: ${OUT_DIR}`)
})
