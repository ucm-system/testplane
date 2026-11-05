import { expect, test } from '@playwright/test'

/**
 * 报告页的 UX 收口回归（B1 / B2 / B3 / C6 / C7 与两处顺带修掉的入口）：
 *  ① 页头标题是「报告」而不是「Run 详情」，副标题带 report.json 与产物；
 *  ② 报告 404（未终态）只出现**一个**空状态，带 Retry-After 倒计时重试，且不再渲染 Task/失败清单空壳；
 *  ③ 指标表懒加载、按 labels 分组；没有 result 的 Task 明确显示「该 Tester 未上报指标」（不编数据）；
 *  C6 结果视图 0 失败时压成一行绿字；
 *  C7 报告页右栏不再有「原始文件」卡；
 *  C2 集群页表头「入队时间」+「等待时长」两列；
 *  B12 概览有「新建 Run」入口。
 */

test.describe.configure({ mode: 'serial' })

test('① 报告页页头：标题「报告」+ 副标题「report.json 与产物」', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0003/report')

  const appHeader = page.locator('header').first()
  await expect(appHeader.locator('h1')).toHaveText('报告')
  await expect(appHeader).toContainText('report.json 与产物')

  // C7：右栏的「原始文件」卡已删除（直链在页头就有）
  await expect(page.getByText('原始文件')).toHaveCount(0)
  // 报告本身还在（标题不是 Run 详情，但内容照常）
  await expect(page.getByText('计数')).toBeVisible()
})

test('② 报告 404：只出现一个空状态 + 倒计时重试，不渲染 Task/失败清单空壳', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  // mock 里这条是 testing（非终态）：报告接口按契约回 404 + Retry-After
  await page.goto('/runs/r-20261105-0004/report')

  const empty = page.getByTestId('report-not-generated')
  await expect(empty).toHaveCount(1)
  await expect(empty).toContainText('报告尚未生成')
  await expect(empty).toContainText('404 + Retry-After')

  // 倒计时重试：按钮先禁用并显示秒数，或倒计时结束后可点
  const retry = page.getByTestId('retry-report')
  await expect(retry).toBeVisible()
  await expect(retry).toContainText(/秒后可重试|立即重试/)
  await expect(page.getByText('每 5 秒自动再试一次')).toBeVisible()

  // 唯一性：错误不再各渲染一遍（计数卡 + report.md 卡），也没有 Task/失败清单空壳
  await expect(page.getByTestId('report-not-generated')).toHaveCount(1)
  await expect(page.getByText('报告尚未生成')).toHaveCount(1)
  await expect(page.getByText('NOT_FOUND')).toHaveCount(0)
  await expect(page.getByText('Task 明细')).toHaveCount(0)
  await expect(page.getByText('失败清单')).toHaveCount(0)
  await expect(page.getByText('report.md 原文')).toHaveCount(0)
})

test('③ 指标表懒加载 + 按 labels 分组 + 无结果时的空态文案', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0006/report')

  await expect(page.getByText('计数')).toBeVisible()
  // 懒加载：还没点/滚之前不请求、不出表
  await expect(page.getByTestId('metrics-table')).toHaveCount(0)
  await expect(page.getByText('指标按需加载', { exact: false })).toBeVisible()

  await page.getByTestId('toggle-metrics').click()

  const table = page.getByTestId('metrics-table').first()
  await expect(table).toBeVisible()
  // 列 = labels 键 + 指标名 + 值 + 单位
  await expect(table).toContainText('dataset')
  await expect(table).toContainText('accuracy')
  await expect(table).toContainText('score')
  await expect(table.locator('tbody')).toContainText('gsm8k')

  // 探测类 Tester 的 result 里 metrics 是空数组 → 直说没上报，不补假值
  await expect(page.getByTestId('metrics-empty').first()).toContainText('该 Tester 未上报指标')

  // 没有任何 result_ref 的 Run：整页指标都是空态
  await page.goto('/runs/r-20261105-0002/report')
  await page.getByTestId('toggle-metrics').click()
  await expect(page.getByTestId('metrics-table')).toHaveCount(0)
  await expect(page.getByTestId('metrics-empty').first()).toContainText('该 Tester 未上报指标')
})

test('C6 结果视图 0 失败：压成一行绿字「无失败项」，产物下载照常', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0006')

  await expect(page.getByTestId('run-result-verdict')).toBeVisible({ timeout: 20_000 })
  const none = page.getByTestId('run-result-no-failures')
  await expect(none).toBeVisible()
  await expect(none).toHaveText('无失败项')
  // 0 失败时不再整块红色卡片
  await expect(page.getByTestId('run-result-failures')).toHaveCount(0)
  // 产物下载还在（且排在失败项后面紧接的位置）
  await expect(page.getByTestId('run-result-artifacts')).toBeVisible()
})

test('C2 集群页：表头是「入队时间」，并补了「等待时长」列', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/clusters')
  await expect(page.getByRole('heading', { name: '排队中的 Task' })).toBeVisible({ timeout: 20_000 })

  await expect(page.getByRole('columnheader', { name: '入队时间' })).toBeVisible({ timeout: 20_000 })
  await expect(page.getByRole('columnheader', { name: '等待时长' })).toBeVisible()
  await expect(page.getByRole('columnheader', { name: '自', exact: true })).toHaveCount(0)
})

test('B12 概览有「新建 Run」入口，点了去 /submit', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/')
  const button = page.getByTestId('new-run')
  await expect(button).toBeVisible()
  await expect(button).toHaveText('新建 Run')
  await button.click()
  await expect(page).toHaveURL(/\/submit$/)
})
