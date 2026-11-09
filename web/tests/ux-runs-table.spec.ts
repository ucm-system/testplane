import { expect, test } from '@playwright/test'

/**
 * 运行列表：真分页 + Excel 式表头列筛选（D-61）。
 *
 * 覆盖用户点名的几件事：
 *  ① 分页栏「共 N 条」、切每页条数、点页码 / 下一页后 URL 与行都变；
 *  ② 表头漏斗：勾选「提交人」后请求带多值参数、行数按 mock 变、`aria-expanded` 正确、Esc 可关；
 *  ③ 改筛选回到第 1 页；`page` 超界显示空态并给「回到第 1 页」；
 *  ④ 空结果态；
 *  ⑤ `q` 只在本页内查找（文案与提示都写清，不假装是全库搜索）。
 */

test.describe.configure({ mode: 'serial' })

/** 记录 listRuns / listRunFacets 的真实入参（mock 模式下网络面板看不到，这里包一层） */
async function spyApi(page: import('@playwright/test').Page) {
  await page.evaluate(async () => {
    const mod = (await import('/src/api/mock.ts')) as {
      mockApi: { listRuns: unknown; listRunFacets: unknown }
    }
    const w = window as unknown as { __runs: unknown[]; __facets: unknown[] }
    w.__runs = []
    w.__facets = []
    const originalRuns = mod.mockApi.listRuns as (q?: unknown) => Promise<unknown>
    const originalFacets = mod.mockApi.listRunFacets as (q?: unknown) => Promise<unknown>
    mod.mockApi.listRuns = (query?: unknown) => {
      w.__runs.push(query ?? {})
      return originalRuns.call(mod.mockApi, query)
    }
    mod.mockApi.listRunFacets = (query?: unknown) => {
      w.__facets.push(query ?? {})
      return originalFacets.call(mod.mockApi, query)
    }
  })
}

function runsCalls(page: import('@playwright/test').Page) {
  return page.evaluate(() => (window as unknown as { __runs: Array<Record<string, unknown>> }).__runs)
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
})

test('① 分页栏：共 N 条 / 每页条数 / 页码 / 下一页都进 URL', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/')
  await expect(page.getByText('全部 Run')).toBeVisible()
  await spyApi(page)

  // 总数来自服务端 total，不是"已加载多少"
  const total = page.getByTestId('pagination-total')
  await expect(total).toBeVisible({ timeout: 15_000 })
  await expect(total).toHaveText(/^共 \d+ 条$/)
  const totalNumber = Number((await total.innerText()).replace(/\D/g, ''))
  expect(totalNumber).toBeGreaterThan(10)

  // 默认 20/页 → 1 页；切到 10/页 → URL 带 page_size 且回第 1 页
  await expect(page.getByTestId('pagination-page')).toContainText('/')
  await page.getByTestId('page-size').selectOption('10')
  await expect(page).toHaveURL(/page_size=10/)
  await expect(page).not.toHaveURL(/\bpage=/)
  const callsAfterSize = await runsCalls(page)
  expect(callsAfterSize.at(-1)).toMatchObject({ page: 1, page_size: 10 })

  // 行数 = 每页 10 条
  await expect(page.locator('tbody tr')).toHaveCount(10)

  // 下一页：URL 带 page=2，行变化（第一行的 Run ID 不同）
  const firstRowBefore = await page.locator('tbody tr td a').first().innerText()
  await page.getByTestId('page-next').click()
  await expect(page).toHaveURL(/page=2/)
  await expect(page.locator('tbody td')).not.toHaveCount(0)
  await expect
    .poll(async () => page.locator('tbody tr td a').first().innerText(), { message: '第 2 页首行应该换人' })
    .not.toBe(firstRowBefore)
  expect((await runsCalls(page)).at(-1)).toMatchObject({ page: 2, page_size: 10 })

  // 点页码 1 回第 1 页（page=1 会被归一成不带 page）
  await page.getByTestId('page-1').click()
  await expect(page).not.toHaveURL(/page=2/)
  await expect
    .poll(async () => (await runsCalls(page)).at(-1)?.page, { message: '回第 1 页应重新请求 page=1' })
    .toBe(1)
})

test('② 表头漏斗：勾选提交人后请求带多值参数，行数变化，aria-expanded 正确，Esc 可关', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/')
  await expect(page.getByTestId('pagination-total')).toBeVisible({ timeout: 15_000 })
  await spyApi(page)

  const trigger = page.getByTestId('facet-trigger-提交人')
  await expect(trigger).toHaveAttribute('aria-expanded', 'false')
  await trigger.click()
  await expect(trigger).toHaveAttribute('aria-expanded', 'true')

  const panel = page.getByTestId('facet-panel-提交人')
  await expect(panel).toBeVisible()
  // 候选值带计数（facet 语义：自己那维不参与自己的计数）
  const option = panel.getByTestId('facet-option-提交人-陈一鸣')
  await expect(option).toContainText('陈一鸣')
  const count = Number((await option.innerText()).replace(/\D/g, ''))
  expect(count).toBeGreaterThan(0)

  // 勾选 + 应用 → URL 带 submitter，请求带多值参数，facets 也重拉了
  await option.locator('input[type="checkbox"]').check()
  await page.getByTestId('facet-apply-提交人').click()
  await expect(page).toHaveURL(/submitter=/)
  await expect(page.getByTestId('facet-panel-提交人')).toHaveCount(0)
  await expect(trigger).toHaveAttribute('aria-expanded', 'false')

  const calls = await runsCalls(page)
  expect(String(calls.at(-1)?.submitter)).toContain('陈一鸣')
  await expect(page.locator('tbody tr')).toHaveCount(count)
  await expect(page.getByTestId('pagination-total')).toHaveText(`共 ${count} 条`)

  // Esc 关面板
  await trigger.click()
  await expect(panel).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)

  // 再勾一个提交人：多值（逗号）进去
  await trigger.click()
  const devBox = page.getByTestId('facet-option-提交人-dev').locator('input[type="checkbox"]')
  await devBox.check()
  await expect(devBox).toBeChecked()
  await page.getByTestId('facet-apply-提交人').click()
  await expect
    .poll(async () => String((await runsCalls(page)).at(-1)?.submitter ?? '').split(',').length, {
      message: '两个提交人应当以逗号拼进同一个参数',
    })
    .toBe(2)
})

test('③ 改筛选回到第 1 页；page 超界显示空态并可回第 1 页', async ({ page }) => {
  await page.goto('/?page_size=10&page=2')
  await expect(page.getByTestId('pagination-total')).toBeVisible({ timeout: 15_000 })
  await expect(page).toHaveURL(/page=2/)

  // 应用一个筛选 → page 被清掉
  await page.getByTestId('facet-trigger-状态').click()
  await page.getByTestId('facet-option-状态-succeeded').locator('input[type="checkbox"]').check()
  await page.getByTestId('facet-apply-状态').click()
  await expect(page).toHaveURL(/status=/)
  await expect(page).not.toHaveURL(/page=2/)

  // 超界页：直接给一个很大的 page
  await page.goto('/?page=99')
  const outOfRange = page.getByTestId('page-out-of-range')
  await expect(outOfRange).toBeVisible({ timeout: 15_000 })
  await expect(outOfRange).toContainText('超出范围')
  await page.getByTestId('back-to-first').click()
  await expect(page).not.toHaveURL(/page=99/)
  await expect(page.getByTestId('pagination-total')).toBeVisible()
})

test('④ 空结果态：筛选到没有数据时给出空态与「清除过滤」', async ({ page }) => {
  // 提交人里没有的谁，直接给 URL
  await page.goto('/?submitter=不存在的提交人')
  await expect(page.getByTestId('runs-empty')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('runs-empty')).toContainText('没有 Run')
  await expect(page.getByTestId('pagination-total')).toHaveText('共 0 条')

  await page.getByTestId('empty-clear-filters').click()
  await expect(page).not.toHaveURL(/submitter=/)
  await expect(page.locator('tbody tr').first()).toBeVisible()
})

test('⑤ q 只在本页内查找：文案写清「在当前页内」，且不改变 total', async ({ page }) => {
  await page.goto('/?page_size=10')
  await expect(page.getByTestId('pagination-total')).toBeVisible({ timeout: 15_000 })
  const totalBefore = await page.getByTestId('pagination-total').innerText()

  const search = page.getByLabel('在当前页内查找（名称 / ID）')
  await expect(search).toBeVisible()
  await expect(page.getByText('筛选本页已加载的 Run')).toHaveCount(0)

  await search.fill('qwen3')
  await expect(page).toHaveURL(/q=qwen3/)
  // 页内命中提示出现，但 total 不变（服务端没有全文搜索）
  await expect(page.getByText(/本页 \d+ 条里命中 \d+ 条/)).toBeVisible()
  await expect(page.getByTestId('pagination-total')).toHaveText(totalBefore)
})

test('⑥ 列筛选候选值随其它列变化（facets 每次筛选都重拉）', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByTestId('pagination-total')).toBeVisible({ timeout: 15_000 })
  await spyApi(page)

  // 先记录「状态」维度的候选值数量
  await page.getByTestId('facet-trigger-状态').click()
  const before = await page.locator('[data-testid^="facet-option-状态-"]').count()
  await page.keyboard.press('Escape')

  // 筛一个只有部分状态的提交人 → 状态候选值应当减少
  await page.getByTestId('facet-trigger-提交人').click()
  await page.getByTestId('facet-option-提交人-dev').locator('input[type="checkbox"]').check()
  await page.getByTestId('facet-apply-提交人').click()
  await expect(page).toHaveURL(/submitter=/)

  await page.getByTestId('facet-trigger-状态').click()
  const after = await page.locator('[data-testid^="facet-option-状态-"]').count()
  expect(after).toBeLessThanOrEqual(before)

  // facets 请求确实带了当前过滤条件
  const facetCalls = await page.evaluate(() => (window as unknown as { __facets: Array<Record<string, unknown>> }).__facets)
  expect(String(facetCalls.at(-1)?.submitter)).toContain('dev')
})

test('⑦ D-62：/runs 兼容重定向到概览，query 原样保留且筛选生效', async ({ page }) => {
  await page.goto('/runs?status=completed_with_errors&page=1&page_size=10&submitter=陈一鸣')
  // URL 落到概览，参数一个不少
  await expect(page).toHaveURL(/\/\?.*status=completed_with_errors/)
  await expect(page).toHaveURL(/page_size=10/)
  await expect(page).toHaveURL(/submitter=/)
  // 列表页已经不存在：表格长在概览上，筛选确实生效
  await expect(page.getByText('全部 Run')).toBeVisible()
  await expect(page.getByTestId('pagination-total')).toBeVisible({ timeout: 15_000 })
  const total = await page.getByTestId('pagination-total').innerText()
  expect(total).toMatch(/^共 \d+ 条$/)
  // 已应用的筛选 chip 还在（说明参数被识别成筛选，而不是被忽略）
  await expect(page.getByText('状态：completed_with_errors')).toBeVisible()
})

test('⑦ D-62：导航里没有「运行列表」，概览页同时有 KPI 与全量 Run 表', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('进行中 Run')).toBeVisible({ timeout: 15_000 })

  const labels = (await page.locator('nav[aria-label="主导航"] a').allInnerTexts()).map((text) => text.trim())
  expect(labels).toContain('概览')
  expect(labels).not.toContain('运行列表')

  // 同屏：KPI 四卡 + 队列摘要 + 全量表 + 分页栏
  await expect(page.locator('[aria-label="概览指标"] > *')).toHaveCount(4)
  await expect(page.getByTestId('queue-summary')).toBeVisible()
  await expect(page.getByTestId('data-table')).toBeVisible()
  await expect(page.getByTestId('pagination')).toBeVisible()
})
