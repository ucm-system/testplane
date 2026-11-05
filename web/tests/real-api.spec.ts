import { expect, test } from '@playwright/test'

/**
 * 真实后端用例（票 #29-4）：
 *   VITE_USE_MOCK=0，打真实控制面的 /healthz + /api/v1/runs，
 *   并确认 /runs 页面渲染出非空列表。
 *
 * 只在显式 VITE_USE_MOCK=0 时执行，默认 mock 冒烟套件不会误跑。
 * 跑法见 web/README.md。
 */
const backend = (process.env.TESTPLANE_BACKEND ?? 'http://127.0.0.1:8080').replace(/\/+$/, '')

test.skip(process.env.VITE_USE_MOCK !== '0', '真实后端用例需要 VITE_USE_MOCK=0')

test('real backend: /healthz + runs list 非空', async ({ page, request }) => {
  const health = await request.get(`${backend}/healthz`)
  expect(health.status()).toBe(200)
  expect((await health.text()).trim().length).toBeGreaterThan(0)

  const runs = await request.get(`${backend}/api/v1/runs?limit=5`, {
    headers: { Authorization: 'Bearer dev' },
  })
  expect(runs.status()).toBe(200)
  const body = (await runs.json()) as { items?: unknown[] }
  expect(Array.isArray(body.items)).toBe(true)
  expect(body.items!.length).toBeGreaterThan(0)

  await page.goto('/runs')
  await expect(page.getByText('全部 Run')).toBeVisible()
  await expect(page.locator('tbody a[href^="/runs/"]').first()).toBeVisible()
})
