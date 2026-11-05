import { expect, test } from '@playwright/test'

/**
 * 前端 client 的**请求形状**回归（评审 #1，不需要真后端）：
 *
 * 服务端注册的是 `PUT /admin/{profiles,clusters}`（名字在 body）与
 * `DELETE /admin/{profiles,clusters}?name=`；之前前端发的是 `…/{name}` 路径参数 → 全 404。
 *
 * 做法：在浏览器里 import `src/api/real.ts`，把 `window.fetch` 换成记录器，
 * 断言真实 client 发出的 URL/method/body。**把 real.ts 改回路径参数，这条立刻红。**
 *
 * 真后端版在 `tests/real-api.spec.ts`（需要 VITE_USE_MOCK=0 + TESTPLANE_BACKEND）。
 */
test('client 契约：admin 更新/删除走 query 与 body，不是路径参数', async ({ page }) => {
  await page.goto('/')

  const captured = await page.evaluate(async () => {
    const real = (await import('/src/api/real.ts')) as {
      realApi: {
        updateProfile: (name: string, body: unknown) => Promise<unknown>
        deleteProfile: (name: string) => Promise<void>
        updateAdminCluster: (name: string, body: unknown) => Promise<unknown>
        deleteAdminCluster: (name: string) => Promise<void>
      }
    }

    const calls: Array<{ url: string; method: string; body: string | null }> = []
    const original = window.fetch
    window.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      calls.push({
        url,
        method: (init?.method ?? 'GET').toUpperCase(),
        body: typeof init?.body === 'string' ? init.body : null,
      })
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      })
    }) as typeof window.fetch

    try {
      await real.realApi.updateProfile('it-profile-1', {
        name: 'it-profile-1',
        cluster: 'c1',
        namespace: 'testplane',
        scheduler: 'default',
        deploy_timeout_seconds: 3600,
      })
      await real.realApi.deleteProfile('it-profile-1')
      await real.realApi.updateAdminCluster('it-cluster-1', { name: 'it-cluster-1', api_server: 'https://x:6443' })
      await real.realApi.deleteAdminCluster('it-cluster-1')
    } finally {
      window.fetch = original
    }
    return calls
  })

  const [putProfile, delProfile, putCluster, delCluster] = captured
  expect(captured).toHaveLength(4)

  expect(putProfile!.method).toBe('PUT')
  expect(putProfile!.url).toContain('/api/v1/admin/profiles?name=it-profile-1')
  expect(putProfile!.url).not.toContain('/admin/profiles/')
  expect(JSON.parse(putProfile!.body ?? '{}')).toMatchObject({ name: 'it-profile-1', namespace: 'testplane' })

  expect(delProfile!.method).toBe('DELETE')
  expect(delProfile!.url).toContain('/api/v1/admin/profiles?name=it-profile-1')

  expect(putCluster!.method).toBe('PUT')
  expect(putCluster!.url).toContain('/api/v1/admin/clusters?name=it-cluster-1')
  expect(putCluster!.url).not.toContain('/admin/clusters/')

  expect(delCluster!.method).toBe('DELETE')
  expect(delCluster!.url).toContain('/api/v1/admin/clusters?name=it-cluster-1')
})
