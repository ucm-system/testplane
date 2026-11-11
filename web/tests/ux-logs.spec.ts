import { expect, test } from '@playwright/test'

/**
 * 运行中 Task 的日志面板回归（P0：日志不刷新）：
 *  ① 运行中 Attempt 日志自动轮询，6 秒内 DOM 行数增加，并显示「最后更新 / 已追加」；
 *  ② 用户手动上滚 → 自动暂停跟随（出现「已暂停跟随 · 回到底部」），新内容不再抢滚动位置；
 *  ③ 结果视图有「打包下载 .zip」，点击真的触发下载，同时保留按文件下载；
 *  ④ 运行中 Deploy 日志面板也按游标追加；
 *  ⑤ 终态 Task 停止轮询并标注「Run 已结束，日志不再更新」。
 *
 * mock 里 `t-20261105-0007` 是一个不结束的演示 Task（runner 标记 sticky）：
 * Attempt / Deploy 日志在带 offset 的轮询请求里会长出新行，接近真实服务端的行为。
 */

test.describe.configure({ mode: 'serial' })

const LIVE_RUN = 't-20261105-0007'

/** 打开运行中 Task 的执行视图，点开第二个 runner（stream-watch，正在跑） */
async function openStreamingAttempt(page: import('@playwright/test').Page): Promise<void> {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto(`/tasks/${LIVE_RUN}`)
  await expect(page.getByText('runner 管道时间线')).toBeVisible({ timeout: 20_000 })
  // v2：执行视图直接给当前 runner 的日志（默认选中 running/preparing 的节点）
  await page.locator('[data-chain-node] button').nth(1).click()
  await expect(page.getByRole('log').first()).toBeVisible({ timeout: 20_000 })
}

test('① 运行中 Task：日志面板自动轮询，6 秒内行数增加', async ({ page }) => {
  await openStreamingAttempt(page)

  const log = page.getByRole('log')
  const lines = log.getByTestId('runner-log-line')
  const before = await lines.count()
  expect(before, '首屏应该已经读到历史日志（mock 给了 40 行）').toBeGreaterThan(10)

  // 轮询间隔 3s：6 秒内必须至少追加一批（追加而不是整块替换）
  await expect.poll(() => lines.count(), { timeout: 6_000, intervals: [400] }).toBeGreaterThan(before)

  // 可观测状态：最后更新时间 + 已追加行数
  await expect(page.getByTestId('runner-log-last-updated')).toContainText('最后更新')
  await expect(page.getByTestId('runner-log-appended')).toContainText('已追加')
  await expect(page.getByTestId('runner-log-live')).toContainText('追加中')
})

test('② 手动上滚暂停跟随：出现「已暂停跟随 · 回到底部」，新内容不再自动滚到底', async ({ page }) => {
  await openStreamingAttempt(page)

  const scroller = page.getByTestId('runner-log-scroller')
  await expect(scroller).toBeVisible()

  // 上滚到顶：跟随应自动暂停
  await scroller.evaluate((element) => {
    element.scrollTop = 0
  })
  const paused = page.getByTestId('runner-log-follow-paused')
  await expect(paused).toBeVisible()
  await expect(paused).toContainText('已暂停跟随 · 回到底部')

  const topAfterPause = await scroller.evaluate((element) => element.scrollTop)
  // 等一轮轮询（3s）确认新行到达时没把视口抢回底部
  await page.waitForTimeout(3_600)
  const topLater = await scroller.evaluate((element) => element.scrollTop)
  expect(topLater, '暂停跟随后 scrollTop 不应被新内容改回底部').toBe(topAfterPause)

  // 点「回到底部」：恢复跟随并滚到最新
  await paused.click()
  await expect(paused).toBeHidden()
  await expect
    .poll(() => scroller.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight), {
      timeout: 3_000,
    })
    .toBeLessThan(30)
})

test('③ 结果视图：打包下载 .zip 存在且点击触发下载，按文件下载保留', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0006')
  await expect(page.getByTestId('task-result-verdict')).toBeVisible({ timeout: 20_000 })

  const bundle = page.getByTestId('download-bundle')
  await expect(bundle).toBeVisible()
  await expect(bundle).toContainText('打包下载 .zip')

  const [download] = await Promise.all([page.waitForEvent('download'), bundle.click()])
  expect(download.suggestedFilename()).toMatch(/\.zip$/)

  // 按文件下载仍在（不是把单文件入口换掉了）；分组默认折叠，先点开再断言
  for (const key of ['deploy', 'tests', 'reports']) {
    await page.getByTestId(`artifact-group-${key}`).locator('summary').click()
  }
  await expect(page.getByTestId('artifact-download').first()).toBeVisible()
})

test('④ 运行中 Task：Deploy 日志面板也按游标追加', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto(`/tasks/${LIVE_RUN}`)
  await expect(page.getByText('部署 Pod 日志', { exact: false })).toBeVisible({ timeout: 20_000 })

  const lines = page.getByTestId('deploy-log-line')
  // 先等首批行真的渲染出来再取基线：`goto` 之后立刻 count 偶尔是 0（面板还在拉首屏），
  // 那条 `expect(before).toBeGreaterThan(0)` 就会偶发变红——是取基线的时机问题，不是轮询没工作。
  await expect.poll(() => lines.count(), { timeout: 20_000, intervals: [300] }).toBeGreaterThan(0)
  const before = await lines.count()
  expect(before).toBeGreaterThan(0)
  await expect.poll(() => lines.count(), { timeout: 6_000, intervals: [400] }).toBeGreaterThan(before)
  await expect(page.getByTestId('deploy-log-last-updated')).toContainText('最后更新')
  await expect(page.getByTestId('deploy-log-appended')).toContainText('已追加')
})

test('⑤ 终态 Task：停止轮询并在面板标注「Run 已结束，日志不再更新」', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/tasks/t-20261105-0003')
  await page.getByRole('tab', { name: '执行细节' }).click()

  await expect(page.getByTestId('deploy-log-ended')).toHaveText('Task 已结束，日志不再更新', { timeout: 20_000 })

  await page.locator('[data-chain-node] button').first().click()
  await expect(page.getByTestId('runner-log-ended')).toHaveText('Task 已结束，日志不再更新', { timeout: 20_000 })
  // 终态不会再追加：等一轮轮询间隔后计数仍不出现
  await page.waitForTimeout(3_400)
  await expect(page.getByTestId('runner-log-appended')).toHaveCount(0)
})
