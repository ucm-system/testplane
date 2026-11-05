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
  // 这条 seeded Run 有 Deploy 阶段与多个 Pod；终态默认是「结果」视图，先切到「执行细节」
  await page.goto('/runs/r-20261105-0003')
  await page.getByRole('tab', { name: '执行细节' }).click()
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
  await page.getByRole('tab', { name: '执行细节' }).click()
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

/**
 * 页头分隔线**只能**有两条，且必须落在网格列线上（用户："左侧分隔符与侧边栏没对齐，特别扎眼"）。
 * 约束：
 *  - 页头里所有竖直分隔线（border-left/right 或 1px 宽的 div）x ∈ {240, vw−288}，不允许第三条；
 *  - 品牌块左内边距与左栏导航项文字左边缘对齐（都是 24px 的节奏）。
 */
test('页头分隔线只有 240 与 vw−288 两条，且品牌与导航左边缘对齐', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  for (const width of [1280, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    await page.goto('/runs')
    await expect(page.getByText('全部 Run')).toBeVisible()
    await page.waitForTimeout(300)

    const m = await page.evaluate(() => {
      const header = document.querySelector('header')!
      const lines: number[] = []
      // 只看"整条竖线"：高度接近页头（>=40px）的边框，或 1px 宽且够高的 div。
      // 徽标/头像那种小圆角边框不算分隔线。
      for (const el of Array.from(header.querySelectorAll('*'))) {
        const cs = getComputedStyle(el)
        const r = el.getBoundingClientRect()
        const tall = r.height >= 40
        if (!tall) continue
        if (parseFloat(cs.borderLeftWidth) > 0) lines.push(Math.round(r.left))
        if (parseFloat(cs.borderRightWidth) > 0) lines.push(Math.round(r.right))
        if (Math.round(r.width) === 1) lines.push(Math.round(r.left))
      }
      const wordmark = header.querySelector('svg[role="img"]')!.getBoundingClientRect()
      const navItemText = (() => {
        const item = document.querySelector('aside[aria-label="主导航"] a')
        if (!item) return null
        const cs = getComputedStyle(item)
        const r = item.getBoundingClientRect()
        return Math.round(r.left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft))
      })()
      return { vw: window.innerWidth, lines: [...new Set(lines)].sort((a, b) => a - b), wordmarkLeft: Math.round(wordmark.left), navItemText }
    })

    const want = [240, m.vw - 288].sort((a, b) => a - b)
    expect(m.lines, `页头竖直分隔线应只有 ${JSON.stringify(want)}`).toEqual(want)
    // 品牌左边缘与导航项文字左边缘对齐（同一 24px 节奏，允许 2px 渲染误差）
    expect(Math.abs(m.wordmarkLeft - (m.navItemText ?? 0))).toBeLessThanOrEqual(2)
    expect(m.wordmarkLeft).toBe(24)
  }

  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '06-header-divider-1280.png'), fullPage: false })
})

/**
 * 提交页两件事（用户截图级反馈）：
 *  - 「服务来源」是一张卡两种形态：部署 / 不部署，同一张卡里换字段（URL / 模型名 / 模型路径）；
 *  - Deploy YAML 有**语法高亮**（B11 起换成 CodeMirror 6 + `@codemirror/lang-yaml`），
 *    可编辑、有行号、Tab 缩进 2 个空格。
 */
test('提交页：服务来源一张卡两形态 + Deploy YAML 语法高亮与 Tab 缩进', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.setViewportSize({ width: 1440, height: 1100 })
  await page.goto('/submit')

  // 服务来源卡片：标题 + 一句"一张卡两形态"的说明
  await expect(page.getByText('服务来源')).toBeVisible()
  await expect(page.getByText('一张卡两形态', { exact: false })).toBeVisible()

  // 部署形态：CodeMirror 编辑器在，且真做了语法高亮（key / 字符串至少两种颜色）+ 行号
  const editor = page.getByLabel('deploy 段 YAML')
  await expect(editor).toBeVisible()
  await expect(page.getByTestId('yaml-editor')).toBeVisible()
  const highlight = await page.evaluate(() => {
    const content = document.querySelector('.cm-content')
    if (!content) return null
    const base = getComputedStyle(content).color
    // 与正文不同的颜色 = 真的上了语法高亮（YAML 的 key 会被 tag 成 propertyName）
    const colors = new Set(Array.from(content.querySelectorAll('span')).map((span) => getComputedStyle(span).color))
    colors.delete(base)
    return {
      tokenColors: colors.size,
      hasLineNumbers: Boolean(document.querySelector('.cm-lineNumbers')),
      whiteSpace: getComputedStyle(content).whiteSpace,
    }
  })
  expect(highlight, 'YAML 应该渲染成 CodeMirror 的 .cm-content').not.toBeNull()
  expect(highlight?.tokenColors ?? 0, '高亮：key 的颜色必须与正文不同').toBeGreaterThanOrEqual(1)
  expect(highlight?.hasLineNumbers, 'YAML 编辑器要有行号').toBe(true)
  expect(highlight?.whiteSpace, '不折行：长值靠横向滚动').not.toBe('pre-wrap')

  // 可编辑 + Tab 缩进 2 个空格（YAML 不允许用制表符缩进）
  await editor.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.press('Enter')
  await page.keyboard.press('Tab')
  await page.keyboard.type('note: demo')
  await page.keyboard.press('Enter')
  await page.keyboard.type('# 注释演示')
  const value = await page.locator('.cm-content').innerText()
  expect(value, 'Tab 应插入 2 个空格').toContain('\n  note: demo')
  expect(value).toContain('# 注释演示')

  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '09-submit-yaml-highlight.png'), fullPage: true })

  // 切到「不部署」：同一张卡里换成服务字段
  await page.getByRole('group', { name: '服务来源' }).getByRole('button', { name: '不部署' }).click()
  await expect(page.getByLabel('服务 base URL')).toBeVisible()
  await expect(page.getByText('模型路径（可选）')).toBeVisible()
  await expect(page.getByLabel('deploy 段 YAML')).toHaveCount(0)
})
