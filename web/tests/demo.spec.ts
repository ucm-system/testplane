import { expect, test } from '@playwright/test'
import { existsSync, mkdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 演示脚本（录屏/截图用）：按顺序走一遍主路径，把每步截图写到 `.data/demo/<step>.png`。
 *
 * 跑法（不依赖真后端，走 mock）：
 *   pnpm exec playwright test tests/demo.spec.ts
 *
 * 两个注意点：
 *  1. mock 的数据在**内存**里，整页刷新就重置 —— 所以第 2→3→4→5 步走站内导航（点导航/按钮），
 *     不要用 `page.goto` 换页；
 *  2. mock 的 live 状态机每 3.5s 推进一步，第 4 步等终态最多约 60s。
 */

const OUT_DIR = join(process.cwd(), '.data', 'demo')

const STEPS = [
  '01-home',
  '02-submit',
  '03-runs',
  '04-run-detail',
  '04b-run-detail-exec',
  '05-report',
  '06-clusters',
  '07-admin-audit',
  '08-retry-dialog',
]

// 截图脚本不需要 trace：Windows 上 retain-on-failure 偶尔在 context 关闭时抢写 trace 文件报 ENOENT
test.use({ trace: 'off' })
test.describe.configure({ mode: 'serial' })

test('演示：提交服务形态 Run → 详情 → 报告 → 集群 → 管理台 → 重试预览', async ({ page }) => {
  test.setTimeout(180_000)
  mkdirSync(OUT_DIR, { recursive: true })

  // 每次导航前注入角色：mock 用这个键模拟 OIDC 角色（admin 才能进管理台）
  await page.addInitScript(() => {
    window.localStorage.setItem('testplane:mock-role', 'admin')
  })

  const shot = async (name: string) => {
    await page.waitForTimeout(700) // 等骨架屏/动画落定
    await page.screenshot({ path: join(OUT_DIR, `${name}.png`), fullPage: true })
  }

  /* 1) 首页概览 */
  await page.goto('/')
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible()
  await expect(page.getByText('进行中 Run')).toBeVisible()
  await shot('01-home')

  /* 2) 提交一个 service 形态的 manifest（不部署，直接打已有服务） */
  await page.goto('/submit')
  const runName = `demo-service-${Date.now().toString().slice(-5)}`
  await page.getByLabel('Run 名称').fill(runName)
  // 默认值已清空（只留 placeholder）：Task 名称要自己写，否则前端校验会拦住提交
  await page.getByLabel('Task 1 名称').fill('accuracy-dataset-a')
  await page.getByLabel('Task 1 参数').fill('--dataset=/data/gsm8k.jsonl --limit=200')
  // Segmented 是 role=group 里的 button（aria-pressed），不是原生 radio
  await page.getByRole('group', { name: '服务来源' }).getByRole('button', { name: '不部署' }).click()
  await expect(page.getByLabel('服务 base URL')).toBeVisible()
  await shot('02-submit')

  await page.getByRole('button', { name: '提交', exact: true }).click()
  await expect(page).toHaveURL(/\/runs\/[^/]+$/, { timeout: 20_000 })
  const runUrl = new URL(page.url()).pathname

  /* 3) 运行列表（新 Run 出现）—— 站内导航，别刷新，否则 mock 状态重置 */
  await page.getByRole('link', { name: '运行列表' }).click()
  await expect(page.getByText('全部 Run')).toBeVisible()
  await expect(page.getByText(runName).first()).toBeVisible({ timeout: 20_000 })
  await shot('03-runs')

  /* 4) Run 详情：等终态 → 默认就是「结果」视图（结论/counts/失败项/产物/报告） */
  await page.locator('tbody tr', { hasText: runName }).first().getByRole('link').first().click()
  await expect(page).toHaveURL(new RegExp(runUrl.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
  await expect(page.getByTestId('run-result-verdict')).toBeVisible({ timeout: 90_000 })
  await expect(page.locator('[data-testid="open-report"]')).toBeVisible()
  await shot('04-run-detail')

  /* 4b) 执行细节：链路 / Deploy / Task / Attempt / 日志（折叠不是隐藏） */
  await page.getByRole('tab', { name: '执行细节' }).click()
  await expect(page.getByText('Task 列表')).toBeVisible()
  await page.locator('tbody tr', { hasText: 'accuracy-dataset-a' }).first().click()
  await shot('04b-run-detail-exec')

  /* 5) 报告页（站内点「查看报告」） */
  await page.locator('[data-testid="open-report"]').click()
  await expect(page.getByText('Task 明细')).toBeVisible()
  await expect(page.getByText('report.md 原文')).toBeVisible()
  await shot('05-report')

  /* 6) 集群与队列（Volcano 队列卡片，reason 原文） */
  await page.getByRole('link', { name: '集群与队列' }).click()
  await expect(page.getByText('Volcano 队列')).toBeVisible()
  await expect(page.getByText('0/8 nodes are available', { exact: false })).toBeVisible()
  await shot('06-clusters')

  /* 7) 管理台：Audit Tab */
  await page.getByRole('link', { name: '管理台' }).click()
  await expect(page.getByText('Execution Profiles')).toBeVisible()
  await page.getByRole('tab', { name: 'Audit' }).click()
  await expect(page.getByText('审计日志')).toBeVisible()
  await shot('07-admin-audit')

  /* 8) 重试预览 Dialog（seeded 的 completed_with_errors Run；整页刷新没关系） */
  await page.goto('/runs/r-20261105-0003')
  const retry = page.locator('[data-testid="retry-run"]').first()
  await expect(retry).toBeVisible({ timeout: 20_000 })
  await retry.click()
  await expect(page.getByText('将复用（已成功）')).toBeVisible()
  await expect(page.getByText('将重跑（失败 / 跳过）')).toBeVisible()
  await expect(page.locator('[data-testid="retry-confirm"]')).toBeEnabled()
  await shot('08-retry-dialog')

  // 收尾自检（评审 #15）：断言**文件真的存在且非空**，不是断言路径字符串
  for (const step of STEPS) {
    const file = join(OUT_DIR, `${step}.png`)
    // eslint-disable-next-line no-console
    console.log(`screenshot: ${file}`)
    expect(existsSync(file), `${step}.png 应该已经写到磁盘`).toBe(true)
    expect(statSync(file).size, `${step}.png 不应是空文件`).toBeGreaterThan(1024)
  }
})
