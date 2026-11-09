import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * Run 详情的信息架构（用户两条反馈）：
 *  - 终态 Run 默认进「结果」视图：结论 / counts / 失败项 / 产物下载 / 报告入口；
 *  - 执行链路、Task、Attempt、日志折到「执行细节」（**折叠不是隐藏**，一键展开）。
 * 运行中的 Run 仍默认执行视图。
 */

const OUT_DIR = join(process.cwd(), '.data', 'layout')

test.describe.configure({ mode: 'serial' })

test('终态 Run：默认结果视图（结论/失败项/产物/报告），执行细节一键展开', async ({ page }) => {
  test.setTimeout(120_000)
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))

  await page.setViewportSize({ width: 1440, height: 1000 })
  // mock 里这条是 completed_with_errors（部分失败），有失败项也有各 Attempt 日志
  await page.goto('/runs/r-20261105-0003')

  // 结果视图的四个块
  await expect(page.getByTestId('run-result-verdict')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('run-result-failures')).toBeVisible()
  await expect(page.getByTestId('run-result-artifacts')).toBeVisible()
  await expect(page.getByTestId('run-result-report')).toBeVisible()

  // 结论里要看得到"为什么失败"
  await expect(page.getByTestId('run-result-verdict')).toContainText('部分失败')
  await expect(page.getByTestId('run-result-failures')).toContainText('TESTER_NONZERO_EXIT')

  // 产物分组默认折叠（用户 2026-11-09）：先断言收起，再点开断言文件显式列全
  for (const key of ['deploy', 'tests', 'reports']) {
    const group = page.getByTestId(`artifact-group-${key}`)
    await expect(group).toBeVisible()
    await expect(group, '产物分组默认应折叠').not.toHaveAttribute('open', '')
    await group.locator('summary').click()
  }

  // 产物下载**显式列全**（不是只有 report.md）
  const downloads = page.locator('[data-testid="artifact-download"]')
  expect(await downloads.count()).toBeGreaterThan(3)
  await expect(page.getByText('report.json', { exact: false }).first()).toBeVisible()
  await expect(page.getByText('metadata.json', { exact: false }).first()).toBeVisible()
  await expect(page.getByText('events.jsonl', { exact: false }).first()).toBeVisible()
  await expect(page.getByText('stdout.log', { exact: false }).first()).toBeVisible()

  // 结果视图里不应该有执行细节（Task 列表）
  await expect(page.getByText('Task 列表')).toHaveCount(0)
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '08-run-result-1440.png'), fullPage: true })

  // 折叠不是隐藏：切到「执行细节」能拿回链路/Task/Attempt
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByText('Task 列表')).toBeVisible()
  await expect(page.getByText('Deploy 日志', { exact: true })).toBeVisible()

  // 切回结果
  await page.getByRole('tab', { name: '结果' }).click()
  await expect(page.getByTestId('run-result-verdict')).toBeVisible()
})

test('运行中的 Run：默认就是执行视图（没有结果页签）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
  await page.goto('/runs/r-20261105-0005') // deploying，非终态
  await expect(page.getByText('Task 列表')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('run-result-verdict')).toHaveCount(0)
  await expect(page.getByRole('tab', { name: '结果' })).toHaveCount(0)
})

/**
 * 执行链路末尾多出来的箭头（用户反馈）：箭头只该出现在**节点之间**。
 * 断言的是结构关系（箭头数 = 节点数 - 1），Deploy + Task 混排时也一样；
 * 单节点（只有 Deploy、0 个 Task）时一个箭头都不该有。
 */
test('执行链路：箭头只出现在节点之间（末尾无箭头；单节点无箭头）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
  await page.setViewportSize({ width: 1440, height: 1000 })

  await page.goto('/')
  await page.locator('tbody a[href="/runs/r-20261105-0002"]').first().click()
  // 终态 Run 默认「结果」视图，链路在「执行细节」里
  await page.getByRole('tab', { name: '执行细节' }).click()

  const nodes = page.locator('[data-chain-node]')
  const arrows = page.locator('[data-testid="run-chain-arrow"]')
  await expect(nodes.first()).toBeVisible({ timeout: 20_000 })

  const count = await nodes.count()
  expect(count, '这条 Run 应该是 Deploy + 多个 Task 的混排').toBeGreaterThan(1)
  await expect(arrows).toHaveCount(count - 1)
  // 最后一个节点（Task）后面没有箭头
  await expect(nodes.last().locator('[data-testid="run-chain-arrow"]')).toHaveCount(0)

  // 单节点：把这条 Run 的 tasks 清空（只留 Deploy），站内切走再切回触发重新拉取
  await page.evaluate(async () => {
    const mock = (await import('/src/api/mock.ts')) as {
      mockApi: { getRun: (id: string) => Promise<Record<string, unknown>> }
    }
    const original = mock.mockApi.getRun.bind(mock.mockApi)
    mock.mockApi.getRun = async (id: string) => {
      const run = await original(id)
      return id === 'r-20261105-0002' ? { ...run, tasks: [] } : run
    }
  })
  await page.getByRole('link', { name: '概览' }).click()
  await expect(page.getByText('全部 Run')).toBeVisible()
  await page.locator('tbody a[href="/runs/r-20261105-0002"]').first().click()
  await page.getByRole('tab', { name: '执行细节' }).click()

  await expect(page.locator('[data-chain-node]')).toHaveCount(1)
  await expect(page.locator('[data-testid="run-chain-arrow"]')).toHaveCount(0)
})
