import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 布局与页头对齐的**量化**回归（用户连追三轮的点，别用"看着像"糊过去）：
 *  - shadcn dashboard-01 骨架：`SidebarProvider` + `AppSidebar variant="inset"` + `SidebarInset`；
 *  - 侧栏容器 256（内层 240，inset p-2），右侧内容区贴到 `vw-8`（inset 的 8px 页边距）；
 *  - 页头 sticky h-14，里面没有竖分隔线；品牌字标与导航项文字左边缘都在 x=24；
 *  - 768 以下侧栏收进 sheet（不渲染桌面侧栏）。
 * 同时截三张响应式图 + 一张 Deploy 日志面板图。
 */

const OUT_DIR = join(process.cwd(), '.data', 'layout')

test.describe.configure({ mode: 'serial' })

test('两列布局：侧栏 256 + inset 内容区（1280 / 900 / 480）', async ({ page }) => {
  test.setTimeout(120_000)
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  /* ---------- 1280：两列（左侧导航 + 主内容；D-62 之后不再有右栏） ---------- */
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible()
  await page.waitForTimeout(600)

  const boxes = await page.evaluate(() => {
    const q = (sel: string) => document.querySelector(sel) as HTMLElement | null
    const rect = (el: Element | null) => {
      if (!el) return null
      const b = el.getBoundingClientRect()
      return {
        left: Math.round(b.left),
        right: Math.round(b.right),
        top: Math.round(b.top),
        bottom: Math.round(b.bottom),
        width: Math.round(b.width),
        height: Math.round(b.height),
      }
    }
    return {
      vw: window.innerWidth,
      vh: window.innerHeight,
      rail: document.querySelector('[data-rail]')?.getAttribute('data-rail'),
      header: rect(q('header')),
      sidebar: rect(q('[data-slot="sidebar-container"]')),
      inner: rect(q('[data-slot="sidebar-inner"]')),
      state: q('[data-slot="sidebar"]')?.getAttribute('data-state'),
      main: rect(q('main')),
      mainCount: document.querySelectorAll('main').length,
      railAside: Boolean(q('aside[aria-label="辅助信息"]')),
    }
  })

  // shadcn dashboard-01 骨架：侧栏 256 + inset 内容区（右边距 8），全页一个 main
  expect(boxes.mainCount).toBe(1)
  expect(boxes.rail).toBe('off')
  expect(boxes.railAside).toBe(false)
  expect(boxes.state).toBe('expanded')
  expect(boxes.sidebar!.left).toBe(0)
  expect(boxes.sidebar!.right).toBe(256)
  expect(boxes.sidebar!.width).toBe(256)
  expect(boxes.inner!.width).toBe(240)
  expect(boxes.sidebar!.height).toBe(boxes.vh)
  expect(boxes.header!.height).toBe(56)
  expect(boxes.main!.left).toBe(256)
  expect(boxes.main!.right).toBe(boxes.vw - 8)
  expect(boxes.main!.top).toBe(boxes.header!.bottom)
  expect(boxes.header!.width).toBe(boxes.main!.width)

  console.log('measured@1280', JSON.stringify(boxes))
  await page.screenshot({ path: join(OUT_DIR, '01-two-columns-1280.png'), fullPage: true })

  /* ---------- 900：同样是侧栏 256 + 主内容 ---------- */
  await page.setViewportSize({ width: 900, height: 900 })
  await page.waitForTimeout(400)
  const twoCol = await page.evaluate(() => ({
    mainLeft: Math.round((document.querySelector('main') as HTMLElement).getBoundingClientRect().left),
    mainRight: Math.round((document.querySelector('main') as HTMLElement).getBoundingClientRect().right),
    navWidth: Math.round((document.querySelector('[data-slot="sidebar-container"]') as HTMLElement).getBoundingClientRect().width),
    vw: window.innerWidth,
  }))
  expect(twoCol.navWidth).toBe(256)
  expect(twoCol.mainLeft).toBe(256)
  expect(twoCol.mainRight).toBe(twoCol.vw - 8)
  await page.screenshot({ path: join(OUT_DIR, '02-two-columns-900.png'), fullPage: true })

  /* ---------- 480：桌面侧栏不渲染（走 sheet 抽屉），主内容占满 ---------- */
  await page.setViewportSize({ width: 480, height: 900 })
  await page.waitForTimeout(400)
  await expect(page.locator('[data-slot="sidebar"]')).toBeHidden()
  const stacked = await page.evaluate(() =>
    Math.round((document.querySelector('main') as HTMLElement).getBoundingClientRect().left),
  )
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
  await expect(page.getByText('Deploy 日志', { exact: true })).toBeVisible({ timeout: 20_000 })

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
  await expect(page.getByText('Deploy 日志', { exact: true })).toBeVisible({ timeout: 20_000 })
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
 * 页头与侧栏的线（shadcn 骨架之后**页头里没有竖线**）：
 *  - 侧栏自成一列，右边界 x=256；页头里不再有 1px 分隔线（透明描边的小按钮不算线）；
 *  - 品牌字标（侧栏顶部）与左栏导航项文字左边缘对齐（都是 24px 的节奏）。
 */
test('页头没有竖分隔线，侧栏右边界=256，品牌与导航左边缘对齐', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  const measure = () =>
    page.evaluate(() => {
      const header = document.querySelector('header')
      const lines: number[] = []
      // 只看"整条竖线"：高度接近页头（>=40px）的边框，或 1px 宽且够高的 div。
      // 透明描边（ghost 按钮的 border-transparent）不算分隔线。
      for (const el of Array.from(header!.querySelectorAll('*'))) {
        const cs = getComputedStyle(el)
        const r = el.getBoundingClientRect()
        if (r.height < 40) continue
        const transparent = /rgba\(0, 0, 0, 0\)|transparent/.test(cs.borderLeftColor)
        if (!transparent && parseFloat(cs.borderLeftWidth) > 0) lines.push(Math.round(r.left))
        if (!transparent && parseFloat(cs.borderRightWidth) > 0) lines.push(Math.round(r.right))
        if (Math.round(r.width) === 1) lines.push(Math.round(r.left))
      }
      const wordmark = document
        .querySelector('[data-slot="sidebar-header"] svg[role="img"]')!
        .getBoundingClientRect()
      const navItemText = (() => {
        const item = document.querySelector('nav[aria-label="主导航"] a')
        if (!item) return null
        const cs = getComputedStyle(item)
        const r = item.getBoundingClientRect()
        return Math.round(r.left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft))
      })()
      const main = document.querySelector('main')!.getBoundingClientRect()
      return {
        vw: window.innerWidth,
        rail: document.querySelector('[data-rail]')!.getAttribute('data-rail'),
        lines: [...new Set(lines)].sort((a, b) => a - b),
        wordmarkLeft: Math.round(wordmark.left),
        navItemText,
        mainLeft: Math.round(main.left),
        mainRight: Math.round(main.right),
        hasRailAside: Boolean(document.querySelector('aside[aria-label="辅助信息"]')),
      }
    })

  for (const width of [1280, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    await page.goto('/')
    await expect(page.getByText('进行中 Run')).toBeVisible()
    await page.waitForTimeout(400)
    const m = await measure()

    expect(m.rail).toBe('off')
    expect(m.hasRailAside).toBe(false)
    expect(m.lines, '页头里不再有竖分隔线（侧栏自成一列）').toEqual([])
    expect(Math.abs(m.wordmarkLeft - (m.navItemText ?? 0))).toBeLessThanOrEqual(2)
    expect(m.wordmarkLeft).toBe(24)
    // 侧栏 256 + inset 8px 页边距
    expect(m.mainLeft).toBe(256)
    expect(m.mainRight).toBe(m.vw - 8)
  }

  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible()
  await page.waitForTimeout(500)
  await page.screenshot({ path: join(OUT_DIR, '06-overview-1280.png'), fullPage: false })
  await page.setViewportSize({ width: 1024, height: 900 })
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '07-overview-1024.png'), fullPage: true })
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
