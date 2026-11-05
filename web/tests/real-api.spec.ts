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
/**
 * 管理台 CRUD 的**真后端**回归（评审 #1）：走**前端自己的 client**（不是手写 HTTP），
 * 这样"real.ts 发什么路径"本身是被测对象：
 *   - `PUT /admin/profiles`（名字在 body 里）
 *   - `DELETE /admin/profiles?name=`
 * 之前前端发 `PUT/DELETE /admin/profiles/{name}`，服务端没有这条路由 → 404。
 * **把 real.ts 改回路径参数，这条立刻红（PUT 404）**。
 *
 * 跑法（真实后端；dev 鉴权自动 `Bearer dev`）：
 *   $env:TESTPLANE_BACKEND='http://127.0.0.1:8141'; $env:VITE_USE_MOCK='0'
 *   pnpm exec playwright test tests/real-api.spec.ts
 */
test('real backend: admin profile 创建 → 更新 → 删除（走前端 client）', async ({ page, request }) => {
  const auth = { Authorization: 'Bearer dev' }
  const name = `it-profile-${Date.now().toString(36)}`

  // 依赖一个已登记集群：没有就现建一个（结尾清理）
  const clustersResp = await request.get(`${backend}/api/v1/admin/clusters`, { headers: auth })
  expect(clustersResp.status()).toBe(200)
  let clusters = ((await clustersResp.json()) as { items?: Array<{ name: string }> }).items ?? []
  let createdCluster: string | null = null
  if (clusters.length === 0) {
    createdCluster = `it-cluster-${Date.now().toString(36)}`
    const created = await request.post(`${backend}/api/v1/admin/clusters`, {
      headers: auth,
      data: { name: createdCluster, api_server: 'https://it.invalid:6443', kubeconfig_key: 'it.kubeconfig' },
    })
    expect(created.status()).toBe(201)
    clusters = [{ name: createdCluster }]
  }
  const cluster = clusters[0]!.name

  // 打开 dev server 拿到前端模块（client 会自己带 Bearer dev / CSRF 头）
  await page.goto('/admin')

  const result = await page.evaluate(
    async ({ profileName, clusterName }) => {
      const mod = (await import('/src/api/index.ts')) as {
        api: {
          createProfile: (body: unknown) => Promise<unknown>
          updateProfile: (name: string, body: unknown) => Promise<{ namespace?: string; deploy_timeout_seconds?: number }>
          deleteProfile: (name: string) => Promise<void>
          listProfiles: () => Promise<Array<{ name: string; namespace?: string }>>
        }
      }
      const { api } = mod
      await api.createProfile({
        name: profileName,
        cluster: clusterName,
        namespace: 'testplane',
        scheduler: 'default',
        deploy_timeout_seconds: 3600,
      })
      // 更新：client 必须发 PUT /admin/profiles（+ body 里的 name）
      const updated = await api.updateProfile(profileName, {
        name: profileName,
        cluster: clusterName,
        namespace: 'testplane-it',
        scheduler: 'default',
        deploy_timeout_seconds: 7200,
      })
      const afterUpdate = (await api.listProfiles()).find((item) => item.name === profileName)
      // 删除：client 必须发 DELETE /admin/profiles?name=
      await api.deleteProfile(profileName)
      const afterDelete = (await api.listProfiles()).some((item) => item.name === profileName)
      return {
        updatedNamespace: updated.namespace,
        updatedTimeout: updated.deploy_timeout_seconds,
        listedNamespace: afterUpdate?.namespace ?? null,
        stillThere: afterDelete,
      }
    },
    { profileName: name, clusterName: cluster },
  )

  expect(result.updatedNamespace, 'PUT /admin/profiles 应 200（旧写法 /admin/profiles/{name} 会 404）').toBe(
    'testplane-it',
  )
  expect(result.updatedTimeout).toBe(7200)
  expect(result.listedNamespace).toBe('testplane-it')
  expect(result.stillThere, 'DELETE /admin/profiles?name= 后列表里应该没有它').toBe(false)

  if (createdCluster) {
    await request.delete(`${backend}/api/v1/admin/clusters?name=${encodeURIComponent(createdCluster)}`, {
      headers: auth,
    })
  }
})