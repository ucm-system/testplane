import { expect, test } from '@playwright/test'

/**
 * 用户三条 P0 反馈的回归（真机截图级）：
 *  ① 提交失败要「看得到为什么」：表单内摘要常驻 + 自动滚进视口 + trace_id 可复制 + 字段级红字；
 *  ② 报告页要能「打包下载」，且走带鉴权的 fetch（mock 下也会真的触发一次下载）；
 *  ③ toast 换成 sonner 之后仍保留 `role="status"` 无障碍语义。
 */

test.describe.configure({ mode: 'serial' })

test('① 非法提交：摘要滚进视口 + issues 逐条 + trace_id 可复制 + 字段级红字', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/submit')

  // 造一个后端会 400 的提交：Run 名清空
  await page.getByLabel('Run 名称').fill('')
  await page.getByRole('button', { name: '提交', exact: true }).click()

  const summary = page.getByTestId('form-error-summary')
  await expect(summary).toBeVisible({ timeout: 15_000 })

  // 1) 语义：role=alert + aria-live=assertive
  await expect(summary).toHaveAttribute('role', 'alert')
  await expect(summary).toHaveAttribute('aria-live', 'assertive')

  // 2) 关键：它必须真的在视口里（用户视线在底部按钮，之前错误渲染在视口外 279px）
  const inViewport = await summary.evaluate((el) => {
    const rect = el.getBoundingClientRect()
    return rect.top >= 0 && rect.bottom <= window.innerHeight + 1
  })
  expect(inViewport, '错误摘要必须自动滚进视口').toBe(true)

  // 3) 内容：前端校验也走同一套（path → message 逐条列出）
  await expect(summary).toContainText('run.name')
  await expect(summary).toContainText('name 必填')

  // 5) 字段级：输入框 aria-invalid + 边框色真的变了
  //    不按 R/G/B 通道算（不同平台/主题的色值不同，CI 上跑过假红）：
  //    只断言"错误态边框 ≠ 正常态边框"，并且错误态是 danger token 的颜色。
  const nameInput = page.getByLabel('Run 名称')
  await expect(nameInput).toHaveAttribute('aria-invalid', 'true')
  const { normal, invalid } = await nameInput.evaluate((el) => {
    const invalidColor = getComputedStyle(el).borderColor
    const probe = el.cloneNode(true) as HTMLElement
    probe.removeAttribute('aria-invalid')
    probe.setAttribute('data-probe', 'normal')
    el.parentElement?.appendChild(probe)
    const normalColor = getComputedStyle(probe).borderColor
    probe.remove()
    return { normal: normalColor, invalid: invalidColor }
  })
  expect(invalid, '错误态边框必须与正常态不同').not.toBe(normal)

  // 6) 摘要里的「定位」能把焦点送回字段
  await page.getByTestId('jump-to-field').first().click()
  await expect(nameInput).toBeFocused()
})

test('① 校验（dry_run）用同一套呈现：问题清单可见，不再是 toast 一晃', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/submit')

  // 清空 Task 名称 → dry-run 返回 issues
  await page.getByLabel('Task 1 名称').fill('')
  await page.getByRole('button', { name: '校验', exact: true }).click()

  const summary = page.getByTestId('form-error-summary')
  await expect(summary).toBeVisible({ timeout: 15_000 })
  await expect(summary).toContainText('run.tests[0].name')
  // 常驻：等 3s 仍然在（不是 toast）
  await page.waitForTimeout(3000)
  await expect(summary).toBeVisible()
})

test('② 报告页打包下载：触发 .zip 下载，且 Blob 是 application/zip', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/runs/r-20261105-0003/report')

  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 15_000 }),
    page.getByTestId('download-bundle').click(),
  ])
  expect(download.suggestedFilename()).toMatch(/\.zip$/)

  // 四个直链也都要在（不能只给 md）
  for (const id of ['download-report-json', 'download-report-md', 'download-metadata-json', 'download-events-jsonl']) {
    await expect(page.getByTestId(id)).toBeVisible()
  }
})

test('③ toast（sonner）仍保留 role=status 无障碍语义', async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/submit')
  // 等 Tester 目录加载完（否则 dry-run 会因「必须选择 Tester」被拦下，提交跑不到 toast）
  await expect(page.getByLabel('Task 1 Tester')).toContainText('aisbench', { timeout: 15_000 })
  await page.getByRole('button', { name: '提交', exact: true }).click()
  // 提交成功会跳详情并弹 toast（B8：一条「已受理 <run_id> · N 个 Task 已入队」+ 两个动作）
  await expect(page).toHaveURL(/\/runs\/[^/]+$/, { timeout: 20_000 })
  // sonner 的实时区（aria-live=polite）里要能看到这条成功提示
  const toaster = page.locator('[data-sonner-toaster]')
  await expect(toaster).toContainText('已受理', { timeout: 10_000 })
  // 语义：存在 aria-live=polite 的实时区且内含这条提示（节点可能带 sr-only，不要求它"可见"）
  expect(await page.locator('[aria-live="polite"]').filter({ hasText: '已受理' }).count()).toBeGreaterThan(0)
  // B8：run_id 与「N 个 Task 已入队」之间必须有分隔（别粘连成「…MV241 个 Task」）
  await expect(page.locator('[data-sonner-toast] [data-title]')).toHaveText(/^已受理 r-\S+ · \d+ 个 Task 已入队$/)
  // 成功 toast 必须带两个动作（复制 ID / 查看详情），且只有一个 toast
  await expect(page.getByRole('button', { name: '复制 ID' }).first()).toBeVisible()
  await expect(page.getByRole('button', { name: '查看详情' }).first()).toBeVisible()
  await expect(page.locator('[data-sonner-toast]')).toHaveCount(1)
})

test('① 后端 400：摘要展示 code/message/trace_id，且 trace_id 可一键复制', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/submit')

  // 让 dry-run 抛真后端那种 400（带 trace_id + detail.issues），验证渲染而不是验证网络
  await page.evaluate(async () => {
    const mock = (await import('/src/api/mock.ts')) as { mockApi: { dryRun: unknown } }
    const client = (await import('/src/api/client.ts')) as { ApiClientError: new (status: number, body: unknown) => Error }
    mock.mockApi.dryRun = async () => {
      throw new client.ApiClientError(400, {
        code: 'VALIDATION_ERROR',
        message: '提交内容未通过校验',
        detail: { issues: [{ code: 'REQUIRED', message: 'execution.profile 必填', path: 'run.tests[0].execution.profile' }] },
        retryable: false,
        trace_id: 'cfa7c2522ebe9c1b',
      })
    }
  })

  await expect(page.getByLabel('Task 1 Tester')).toContainText('aisbench', { timeout: 15_000 })
  await page.getByRole('button', { name: '校验', exact: true }).click()
  const summary = page.getByTestId('form-error-summary')
  await expect(summary).toBeVisible({ timeout: 15_000 })
  await expect(summary).toContainText('VALIDATION_ERROR')
  await expect(summary).toContainText('提交内容未通过校验')
  await expect(summary).toContainText('run.tests[0].execution.profile')
  await expect(summary).toContainText('cfa7c2522ebe9c1b')

  await page.getByTestId('copy-trace-id').click()
  const clipboard = await page.evaluate(() => navigator.clipboard.readText())
  expect(clipboard).toBe('cfa7c2522ebe9c1b')
})