import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * Run 详情的信息架构（用户 2026-11-11 裁决后的 v2）：
 *  - 终态 Task 默认进「结果」页签：结论 / runner 明细（谁成功谁失败）/ 测试结果（三选一内联渲染）/ 产物下载；
 *  - 「执行细节」页签：管道时间线、runner 详情（日志）、deploy、**执行事件**（事件并入执行细节，不再单独页签）；
 *  - 「报告」页签取消（report.json 只是平台产物之一，D-74）；运行中的 Task 只有「执行细节」页签。
 */

const OUT_DIR = join(process.cwd(), '.data', 'layout')

test.describe.configure({ mode: 'serial' })

test('终态 Task：默认结果页签（结论/runner 明细/测试结果/产物），执行细节含事件', async ({ page }) => {
  test.setTimeout(120_000)
  mkdirSync(OUT_DIR, { recursive: true })
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))

  await page.setViewportSize({ width: 1440, height: 1000 })
  // mock 里这条是 completed_with_errors（部分失败），有失败项也有各 Attempt 日志
  await page.goto('/tasks/t-20261105-0003')

  // v2：默认落在「结果」页签（结论 + 三选一渲染 + 产物，用户只要这三样）
  await expect(page.getByTestId('task-result-panel')).toBeVisible({ timeout: 20_000 })

  // 结果视图的三个块：结论 / 测试结果（三选一内联渲染）/ 产物下载（明细卡已删，用户裁决）
  await expect(page.getByTestId('task-result-verdict')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('task-result-render')).toBeVisible()
  await expect(page.getByTestId('task-result-artifacts')).toBeVisible()

  // 结论里要看得到"为什么失败"（失败 runner 名单进结论一句话）
  await expect(page.getByTestId('task-result-verdict')).toContainText('失败')

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

  // 结果视图里不应该有执行细节（runner 管道时间线）
  await expect(page.getByText('runner 管道时间线')).toHaveCount(0)
  await page.waitForTimeout(400)
  await page.screenshot({ path: join(OUT_DIR, '08-run-result-1440.png'), fullPage: true })

  // 折叠不是隐藏：切到「执行细节」能拿回链路 / Task / Attempt / 事件
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByText('runner 管道时间线')).toBeVisible()
  await expect(page.getByText('Deploy 日志', { exact: false }).first()).toBeVisible()
  await expect(page.getByTestId('task-events-panel')).toBeVisible()
  await expect(page.getByTestId('task-events-panel')).toContainText('执行事件')

  // 切回结果
  await page.getByRole('tab', { name: '结果' }).click()
  await expect(page.getByTestId('task-result-verdict')).toBeVisible()
})

test('运行中的 Task：只有「执行细节」页签（时间线与事件都在里面）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))
  await page.goto('/tasks/t-20261105-0005') // deploying，非终态
  // v2：运行中只有「执行细节」页签（默认选中）；结果页签只有终态才有
  await expect(page.getByText('runner 管道时间线')).toBeVisible({ timeout: 20_000 })
  await expect(page.getByTestId('task-result-verdict')).toHaveCount(0)
  await expect(page.getByRole('tab', { name: '结果' })).toHaveCount(0)

  // 事件就在执行细节页签里（不再单独页签）
  await expect(page.getByTestId('task-events-panel')).toBeVisible()
})

/**
 * 执行链路末尾多出来的箭头（用户反馈）：箭头只该出现在**节点之间**。
 * 断言的是结构关系（箭头数 = 节点数 - 1），Deploy + Task 混排时也一样；
 * 单节点时一个箭头都不该有。
 */
test('执行链路：箭头只出现在节点之间（末尾无箭头；单节点无箭头）', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))
  await page.setViewportSize({ width: 1440, height: 1000 })

  await page.goto('/')
  await page.locator('tbody a[href="/tasks/t-20261105-0002"]').first().click()
  // 终态 Task 默认「结果」视图，链路在「执行细节」里
  await page.getByRole('tab', { name: '执行细节' }).click()

  const nodes = page.locator('[data-chain-node]')
  const arrows = page.locator('[data-testid="task-chain-arrow"]')
  await expect(nodes.first()).toBeVisible({ timeout: 20_000 })

  const count = await nodes.count()
  expect(count, '这条 Task 应该是 Deploy + 多个 runner 的混排').toBeGreaterThan(1)
  await expect(arrows).toHaveCount(count - 1)
  // 最后一个节点（Task）后面没有箭头
  await expect(nodes.last().locator('[data-testid="task-chain-arrow"]')).toHaveCount(0)

  // 单节点：把这条 Task 的 runners 只留第一个（service-deploy），站内切走再切回触发重新拉取
  await page.evaluate(async () => {
    const mock = (await import('/src/api/mock.ts')) as {
      mockApi: { getTask: (id: string) => Promise<Record<string, unknown>> }
    }
    const original = mock.mockApi.getTask.bind(mock.mockApi)
    mock.mockApi.getTask = async (id: string) => {
      const task = await original(id)
      if (id !== 't-20261105-0002') return task
      const runners = (task.runners as unknown[]) ?? []
      return { ...task, runners: runners.slice(0, 1), runner_counts: { total: 1, succeeded: 1 } }
    }
  })
  await page.getByRole('link', { name: '概览' }).first().click()
  await expect(page.getByText('全部 Task')).toBeVisible()
  await page.locator('tbody a[href="/tasks/t-20261105-0002"]').first().click()
  await page.getByRole('tab', { name: '执行细节' }).click()

  await expect(page.locator('[data-chain-node]')).toHaveCount(1)
  await expect(page.locator('[data-testid="task-chain-arrow"]')).toHaveCount(0)
})
