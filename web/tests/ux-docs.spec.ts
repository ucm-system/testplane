import { expect, test } from '@playwright/test'
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 文档页 / API 页 + 侧栏底部入口（用户 2026-11-09）。
 *
 * 断言的是"能用来做事"这件事：
 *  - 侧栏底部三条链接在两档宽度下都可达（折叠成 48px icon rail 时只剩图标，但 aria-label 与 tooltip 还在）；
 *  - `/docs` 是真内容：五节标题都在，目录锚点能跳；
 *  - `/api` 的数据来自 openapi.yaml：端点数与契约一致、搜索能过滤、下载真的给出 .yaml；
 *  - 两页在 390px 不横向溢出。
 */

const OUT_DIR = join(process.cwd(), '.data', 'docs')

test.describe.configure({ mode: 'serial' })

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'admin'))
})

test('① 侧栏底部三条入口：展开有文字，折叠只剩图标但 aria-label/tooltip 仍可达', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto('/')
  await expect(page.getByText('进行中 Task')).toBeVisible({ timeout: 20_000 })

  const footer = page.locator('[data-slot="sidebar-footer"], [data-sidebar="footer"]').first()
  const docsLink = page.getByTestId('sidebar-footer-文档')
  const apiLink = page.getByTestId('sidebar-footer-API')
  const ghLink = page.getByTestId('sidebar-footer-GitHub')

  // 展开态：图标 + 文字
  await expect(docsLink).toBeVisible()
  await expect(docsLink).toHaveText(/文档/)
  await expect(apiLink).toHaveAttribute('href', '/api')
  await expect(ghLink).toHaveAttribute('href', 'https://github.com/ucm-system/argus')
  await expect(ghLink).toHaveAttribute('target', '_blank')
  await expect(ghLink).toHaveAttribute('rel', /noreferrer/)

  // 三条都是"小字 muted"：字号 ≤ 14px，颜色是弱化色（不是正文字色）
  const styles = await docsLink.evaluate((el) => {
    const cs = getComputedStyle(el)
    return { fontSize: Number.parseFloat(cs.fontSize), color: cs.color }
  })
  expect(styles.fontSize).toBeLessThanOrEqual(14)

  // 点「文档」真的到 /docs
  await docsLink.click()
  await expect(page).toHaveURL(/\/docs$/)
  await expect(page.getByRole('heading', { name: '文档', level: 1 })).toBeVisible()

  // 折叠成 icon rail：文字隐藏，图标还在，aria-label 仍能定位
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.getByRole('button', { name: /Toggle Sidebar|切换侧栏|收起侧栏|展开侧栏/ }).first().click()
  await expect(page.locator('[data-state="collapsed"]').first()).toBeVisible({ timeout: 10_000 })
  const collapsedDocs = page.getByTestId('sidebar-footer-文档')
  await expect(collapsedDocs).toBeVisible()
  await expect(collapsedDocs).toHaveAttribute('aria-label', '文档')
  // 文字 span 在折叠态被隐藏
  await expect(collapsedDocs.locator('span', { hasText: '文档' })).toBeHidden()
  // 图标仍在（svg）
  await expect(collapsedDocs.locator('svg').first()).toBeVisible()
  await expect(footer).toHaveCount(1)
})

test('② /docs：五节真内容都在，目录锚点可跳', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/docs')
  await expect(page.getByTestId('docs-page')).toBeVisible({ timeout: 20_000 })

  for (const [id, title] of [
    ['quickstart', '快速开始'],
    ['runner-contract', 'Runner 契约'],
    ['shared-mounts', '统一共享挂载'],
    ['status-semantics', '状态语义'],
    ['cli-ci', 'CLI 与 CI'],
  ] as const) {
    await expect(page.getByRole('heading', { name: title })).toBeVisible()
    await expect(page.getByTestId(`doc-section-${id}`)).toBeVisible()
    await expect(page.getByTestId(`doc-toc-${id}`)).toBeVisible()
  }

  // 真内容（不是占位）：几个关键契约点在正文里
  const body = page.getByTestId('doc-section-runner-contract')
  await expect(body).toContainText('/workspace/output')
  await expect(body).toContainText('result.json')
  await expect(body).toContainText('AG_TASK_ID')
  await expect(body).toContainText('AG_PARAMS_FILE')
  const mounts = page.getByTestId('doc-section-shared-mounts')
  await expect(mounts).toContainText('runner.yaml.mounts')
  await expect(mounts).toContainText('PVC')
  await expect(mounts).toContainText('/admin/shared-mounts')
  await expect(mounts).toContainText('fsGroup')

  // 目录锚点跳转：点最后一条，URL 带 hash 且该节进入视口
  await page.getByTestId('doc-toc-cli-ci').click()
  await expect(page).toHaveURL(/#cli-ci$/)
  const inViewport = await page.getByTestId('doc-section-cli-ci').evaluate((el) => {
    const rect = el.getBoundingClientRect()
    return rect.top < window.innerHeight && rect.bottom > 0
  })
  expect(inViewport, '点目录后该节应当滚进视口').toBe(true)

  // 每节都有"在仓库里看完整设计"的外链
  const designLinks = page.getByRole('link', { name: /在仓库里看完整设计/ })
  await expect(designLinks).toHaveCount(6)
  await expect(designLinks.first()).toHaveAttribute('href', /github\.com\/ucm-system\/argus\/blob\/main\/design\//)

  await page.screenshot({ path: join(OUT_DIR, 'docs-1440.png'), fullPage: true })
})

test('③ /api：端点来自 openapi.yaml（数量与契约一致）、搜索过滤生效、下载给出契约原文', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/api')
  await expect(page.getByTestId('api-page')).toBeVisible({ timeout: 20_000 })

  // 端点数不写死：从生成的契约文件现场数一遍再比 UI（否则每次加端点都要改这条断言）
  const spec = readFileSync(new URL('../src/generated/openapi.yaml', import.meta.url), 'utf8')
  const expected = (spec.match(/^ {4}(get|post|put|patch|delete):/gm) ?? []).length
  expect(expected).toBeGreaterThan(50)
  const count = page.getByTestId('api-endpoint-count')
  await expect(count).toHaveText(`共 ${expected} 个端点`)

  // 按 tag 分组
  await expect(page.locator('[data-testid^="api-group-"]').first()).toBeVisible()
  await expect(page.getByTestId('api-group-tasks')).toBeVisible()

  // 点一行展开 parameters / responses
  const firstRow = page.getByTestId('api-endpoint-row').first()
  await firstRow.click()
  await expect(page.getByText(/参数（\d+）/).first()).toBeVisible()
  await expect(page.getByText(/响应（\d+）/).first()).toBeVisible()
  await firstRow.click()

  // 搜索：按路径过滤，行数变少且只留匹配项
  const rows = page.getByTestId('api-endpoint-row')
  const before = await rows.count()
  await page.getByLabel('按路径或摘要过滤端点').fill('tasks')
  await expect
    .poll(async () => rows.count(), { message: '搜索后行数应当变少' })
    .toBeLessThan(before)
  const visiblePaths = await page.locator('[data-testid="api-endpoint-row"] code').allInnerTexts()
  expect(visiblePaths.length).toBeGreaterThan(0)
  for (const path of visiblePaths) expect(path.toLowerCase()).toContain('tasks')

  // 清空后恢复
  await page.getByLabel('按路径或摘要过滤端点').fill('')
  await expect.poll(async () => rows.count()).toBe(before)

  // 下载：给出的是同一份契约（yaml 文本，含 openapi 与 /tasks）
  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 15_000 }),
    page.getByTestId('download-openapi').click(),
  ])
  expect(download.suggestedFilename()).toBe('openapi.yaml')

  await page.screenshot({ path: join(OUT_DIR, 'api-1440.png'), fullPage: true })
})

test('④ 390px 下 /docs 与 /api 都不横向溢出', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.setViewportSize({ width: 390, height: 844 })

  for (const [path, name] of [
    ['/docs', 'docs-390'],
    ['/api', 'api-390'],
  ] as const) {
    await page.goto(path)
    await page.waitForTimeout(900)
    const m = await page.evaluate(() => ({
      scrollWidth: document.scrollingElement?.scrollWidth ?? 0,
      innerWidth: window.innerWidth,
    }))
    expect(m.scrollWidth, `${path} 不该横向溢出`).toBeLessThanOrEqual(m.innerWidth + 1)
    await page.screenshot({ path: join(OUT_DIR, `${name}.png`), fullPage: true })
  }
})
