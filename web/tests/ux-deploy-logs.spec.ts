import { expect, test } from '@playwright/test'

/**
 * Deploy / Task 日志落盘后的 UI 回归（D-59）：
 *  ① 每个 Pod 一张「下载 .log」：按当前 Pod（与容器）下载 `deploy/logs/<pod>[.<container>].log`，文件名 `<pod>.log`；
 *  ② 日志标记行（restart / gap / terminated / collection stopped）渲染成分隔样式，
 *     restarted = warn、gap = danger，且**行内容原样保留**；
 *  ③ `/runs/{id}/deploy` 的 `pods[]`：非空渲染拓扑表（Pod 名可点、重启次数 >0 标 warn），空数组给提示语；
 *  ④ 产物按来源分三组（部署日志 / 测试日志 / 报告与数据），组内逐条可下载，总数等于接口返回条数；
 *  ⑤ 盘上还没该日志（后端 404）时显示"还没落盘"，不弹红色错误。
 *
 * 全部走 mock：`r-20261105-0007` 是运行中的长日志 Run（Attempt / Deploy 日志会持续追加），
 * `r-20261105-0006` 是终态、4 Pod 的成功 Run，`r-20261105-0005` 是排队中、`pods=[]`。
 */

test.describe.configure({ mode: 'serial' })

const LIVE_RUN = 'r-20261105-0007'
const DONE_RUN = 'r-20261105-0006'
const QUEUED_RUN = 'r-20261105-0005'

async function asOperator(page: import('@playwright/test').Page, runId: string): Promise<void> {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto(`/runs/${runId}`)
}

test('① Deploy 日志面板：Pod 页签旁有「下载 .log」，点击真的下载 <pod>.log', async ({ page }) => {
  await asOperator(page, LIVE_RUN)

  const button = page.getByTestId('download-deploy-log')
  await expect(button).toBeVisible({ timeout: 20_000 })
  await expect(button).toContainText('下载 .log')

  const firstTab = page.getByTestId('deploy-log-pod-tab').first()
  const podName = (await firstTab.innerText()).split('\n')[0]!.trim()
  expect(podName.length).toBeGreaterThan(0)
  await expect(button).toHaveAttribute('title', new RegExp(`deploy/logs/${podName}\\.log`))

  const [download] = await Promise.all([page.waitForEvent('download'), button.click()])
  expect(download.suggestedFilename()).toBe(`${podName}.log`)
  await expect(page.getByTestId('deploy-log-download-notice')).toContainText('已开始下载')
  await expect(page.getByTestId('deploy-log-download-error')).toHaveCount(0)
})

test('② 日志标记行：渲染为 log-marker 分隔样式；restarted=warn、gap=danger，内容不改', async ({ page }) => {
  await asOperator(page, LIVE_RUN)
  await expect(page.getByText('Task 列表')).toBeVisible({ timeout: 20_000 })
  await page.getByTestId('view-task').nth(1).click()

  const scroller = page.getByTestId('attempt-log-scroller')
  await expect(scroller).toBeVisible({ timeout: 20_000 })

  const markers = scroller.getByTestId('log-marker')
  await expect(markers).toHaveCount(2)

  const restart = scroller.locator('[data-testid="log-marker"][data-marker-tone="warn"]')
  const gap = scroller.locator('[data-testid="log-marker"][data-marker-tone="danger"]')
  await expect(restart).toHaveCount(1)
  await expect(gap).toHaveCount(1)

  // 内容原样（用户可以复制去搜），不是被裁剪 / 改写的正文
  await expect(restart).toHaveText(
    '--- [container restarted #3 at 2026-11-05T06:44:11Z, previous exit: OOMKilled] ---',
  )
  await expect(gap).toHaveText(
    '--- [log gap: 2026-11-05T06:44:12Z – 2026-11-05T06:46:30Z, collector restarted] ---',
  )
})

test('③ Pod 拓扑：pods[] 非空渲染表格（Pod 名可点、重启标 warn），空数组给「暂无 Pod」', async ({ page }) => {
  await asOperator(page, DONE_RUN)
  await page.getByRole('tab', { name: '执行细节' }).click()

  const table = page.getByTestId('deploy-pods')
  await expect(table).toBeVisible({ timeout: 20_000 })
  const podNames = table.getByTestId('deploy-pod-name')
  await expect(podNames).toHaveCount(4)
  await expect(table).toContainText('运行中')
  // phase 原文留在 title 里（中文标签只做展示）
  await expect(table.locator('td[title="Running"]')).toHaveCount(4)
  // 重启次数 > 0 的 Pod 用 warn 徽标（不只是一个裸数字）
  const restartedRow = table.locator('tr', { hasText: 'decode-0' })
  await expect(restartedRow).toContainText('1 次')
  // 重启次数 > 0 的 Pod 用 warn 徽标（不只是一个裸数字）。
  // 断言走 `data-tone` 而不是颜色类：徽标改成了「描边 + 彩色点」，色不再写在文字 class 上。
  await expect(restartedRow.locator('[data-tone="warn"]').first()).toBeVisible()

  // 点 Pod 名 → 下方 Deploy 日志面板切到对应 Pod 页签
  const targetName = (await podNames.nth(1).innerText()).trim()
  await podNames.nth(1).click()
  const tabs = page.getByTestId('deploy-log-pod-tab')
  const selected = tabs.filter({ hasText: targetName }).first()
  await expect(selected).toHaveAttribute('aria-selected', 'true')

  // 排队中 / 已回收：pods=[] 时给一行明确提示，而不是空表格
  await asOperator(page, QUEUED_RUN)
  await expect(page.getByTestId('deploy-pods')).toContainText('暂无 Pod（排队中或已回收）', { timeout: 5_000 })
})

test('④ 产物分组：三个组都在，文件一个不少（数量 = /artifacts 返回条数）', async ({ page }) => {
  await asOperator(page, DONE_RUN)
  await expect(page.getByTestId('run-result-artifacts')).toBeVisible({ timeout: 20_000 })

  const expected = await page.evaluate(async (runId) => {
    const mod = (await import('/src/api/mock.ts')) as unknown as {
      mockApi: { listRunArtifacts: (id: string) => Promise<{ items: Array<{ path: string }> }> }
    }
    const list = await mod.mockApi.listRunArtifacts(runId)
    const rel = (path: string) => (path.startsWith(`${runId}/`) ? path.slice(runId.length + 1) : path)
    const deploy = list.items.filter(
      (item) => /^deploy\/logs\/.+\.log$/i.test(rel(item.path)) || rel(item.path) === 'deploy/pods.json',
    ).length
    const tests = list.items.filter(
      (item) =>
        rel(item.path).startsWith('tasks/') &&
        (/\/logs\//i.test(rel(item.path)) || /\.log$/i.test(rel(item.path))),
    ).length
    return { total: list.items.length, deploy, tests, reports: list.items.length - deploy - tests }
  }, DONE_RUN)
  expect(expected.total).toBeGreaterThan(expected.deploy + expected.tests)

  const groups = {
    deploy: page.getByTestId('artifact-group-deploy'),
    tests: page.getByTestId('artifact-group-tests'),
    reports: page.getByTestId('artifact-group-reports'),
  }
  for (const [key, group] of Object.entries(groups)) {
    await expect(group, `产物分组 ${key} 必须存在`).toBeVisible()
    const count = expected[key as keyof typeof groups]
    await expect(group.locator('tbody tr')).toHaveCount(count)
    await expect(group.getByTestId('artifact-download')).toHaveCount(count)
  }
  // 一个不少：结果视图里每份产物恰好一条下载入口
  await expect(page.getByTestId('artifact-download')).toHaveCount(expected.total)
  await expect(groups.deploy).toContainText('deploy/logs/')
  await expect(groups.deploy).toContainText('deploy/pods.json')
  await expect(groups.tests).toContainText('stdout.log')

  // 报告页不再放产物详情（用户 2026-11-09 / D-64）：产物只在结果页，报告页只有计数/指标/Task/失败清单/report.md
  await page.goto(`/runs/${DONE_RUN}/report`)
  await expect(page.getByTestId('report-artifacts')).toHaveCount(0)
  await expect(page.getByTestId('report-md')).toBeVisible({ timeout: 20_000 })
})

test('⑤ 落盘缺失：下载 404 显示「还没落盘」，不弹红色错误', async ({ page }) => {
  await asOperator(page, LIVE_RUN)
  await expect(page.getByTestId('download-deploy-log')).toBeVisible({ timeout: 20_000 })

  // 模拟后端 404（盘上还没有该 Pod 的日志文件）
  await page.evaluate(async () => {
    const mod = (await import('/src/api/mock.ts')) as unknown as {
      mockApi: { downloadFile: (runId: string, path: string) => Promise<Blob> }
    }
    const client = (await import('/src/api/client.ts')) as unknown as {
      ApiClientError: new (status: number, body: Record<string, unknown>) => Error
    }
    mod.mockApi.downloadFile = async () => {
      throw new client.ApiClientError(404, { code: 'NOT_FOUND', message: 'not on disk', retryable: false })
    }
  })

  await page.getByTestId('download-deploy-log').click()
  await expect(page.getByTestId('deploy-log-download-notice')).toHaveText('该 Pod 的日志还没落盘（可能刚创建）')
  await expect(page.getByTestId('deploy-log-download-error')).toHaveCount(0)
})
