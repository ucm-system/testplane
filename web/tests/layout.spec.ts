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
  // 契约收窄后没有「全部 Pod」页签：`?pod=` 必选，页签就是具体 Pod 名
  await expect(page.locator('[data-testid="deploy-log-pod-tab"]').first()).toBeVisible()
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

/**
 * Deploy 日志的**单 Pod 游标**语义（评审 #4 的契约收窄）：
 *  - `?pod=` 必选，且切 Pod 页签时请求要换 pod；
 *  - `tail_lines` 与 `offset` 二选一（勾"先看最新"时不能带 offset）；
 *  - `next_offset` 续读第二页（直接打 mock 的 API，验证游标语义）。
 */
test('Deploy 日志：pod 页签带参 + tail/offset 二选一 + next_offset 续读', async ({ page }) => {
  test.setTimeout(120_000)
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  // mock 模式不发 HTTP 请求，所以给 `mockApi.getDeployLogs` 装个探针，记录每次调用的入参
  await page.goto('/runs/r-20261105-0006')
  await expect(page.getByText('Deploy 日志')).toBeVisible({ timeout: 20_000 })
  await page.evaluate(async () => {
    const mod = (await import('/src/api/mock.ts')) as unknown as {
      mockApi: { getDeployLogs: (...args: unknown[]) => Promise<unknown> }
    }
    const w = window as unknown as { __logCalls: unknown[] }
    w.__logCalls = []
    const original = mod.mockApi.getDeployLogs.bind(mod.mockApi)
    mod.mockApi.getDeployLogs = (...args: unknown[]) => {
      w.__logCalls.push(args[1])
      return original(...args)
    }
  })

  const tabs = page.locator('[data-testid="deploy-log-pod-tab"]')
  await expect(tabs).toHaveCount(4)
  await expect(tabs.first()).toHaveAttribute('aria-selected', 'true')

  // 切到第二个 Pod：入参必须带对应 pod
  const secondName = (await tabs.nth(1).innerText()).split('\n')[0]!.trim()
  await tabs.nth(1).click()
  await expect(tabs.nth(1)).toHaveAttribute('aria-selected', 'true')
  await page.waitForTimeout(400)
  const calls = await page.evaluate(
    () => (window as unknown as { __logCalls: Array<Record<string, unknown>> }).__logCalls,
  )
  expect(calls.some((call) => call.pod === secondName)).toBe(true)

  // 勾"先看最新"：tail_lines 与 offset 不能同时出现
  await page.getByLabel(/先看最新/).check()
  await page.waitForTimeout(400)
  const tailCalls = await page.evaluate(
    () => (window as unknown as { __logCalls: Array<Record<string, unknown>> }).__logCalls,
  )
  const tailCall = tailCalls.filter((call) => call.tail_lines).pop()
  expect(tailCall, '勾"先看最新"后应发出带 tail_lines 的调用').toBeTruthy()
  expect(tailCall!.offset, 'tail_lines 与 offset 二选一：要最新就不该带 offset').toBeUndefined()

  // 游标续读：直接打 mock API（dev server 下模块可 import），limit=4 制造第二页
  const paging = await page.evaluate(async () => {
    const mod = await import('/src/api/mock.ts')
    const first = await mod.mockApi.getDeployLogs('r-20261105-0006', { pod: 'genet-qwen3-32b-decode-0', offset: 0, limit: 4 })
    const second = await mod.mockApi.getDeployLogs('r-20261105-0006', { pod: 'genet-qwen3-32b-decode-0', offset: first.next_offset, limit: 4 })
    return {
      firstOffset: first.entries[0]?.offset,
      firstCount: first.entries[0]?.line_count,
      truncated: first.truncated,
      nextOffset: first.next_offset,
      secondOffset: second.entries[0]?.offset,
      secondText: second.entries[0]?.text?.split('\n')[0] ?? '',
      firstText: first.entries[0]?.text?.split('\n')[0] ?? '',
    }
  })
  expect(paging.firstOffset).toBe(0)
  expect(paging.firstCount).toBe(4)
  expect(paging.truncated).toBe(true)
  expect(paging.nextOffset).toBe(4)
  expect(paging.secondOffset).toBe(4)
  expect(paging.secondText).not.toBe(paging.firstText)

  // 缺 pod 必须 400、不存在的 pod 必须 404（契约）
  const errors = await page.evaluate(async () => {
    const mod = await import('/src/api/mock.ts')
    const grab = async (fn: () => Promise<unknown>) => {
      try {
        await fn()
        return 'no-error'
      } catch (e) {
        return (e as { code?: string }).code ?? 'unknown'
      }
    }
    const missingPod = await grab(() => mod.mockApi.getDeployLogs('r-20261105-0006', {} as { pod: string }))
    const unknownPod = await grab(() => mod.mockApi.getDeployLogs('r-20261105-0006', { pod: 'nope-0' }))
    return { missingPod, unknownPod }
  })
  expect(errors.missingPod).toBe('VALIDATION_ERROR')
  expect(errors.unknownPod).toBe('NOT_FOUND')

  await page.screenshot({ path: join(OUT_DIR, '05-deploy-logs-multi-pod.png'), fullPage: true })
})
