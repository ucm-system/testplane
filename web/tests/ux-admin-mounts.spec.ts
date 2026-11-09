import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

/**
 * 管理台「挂载点」tab（票 #90 / D-65）。
 *
 * 覆盖用户点名的五件事：
 *  ① tab 能切、空态文案是"未配置不会阻断 Pod"；
 *  ② 新建 `/models` 后出现在表格；
 *  ③ 编辑把 `enabled=false` 后表格显示未启用；
 *  ④ 删除要二次确认（点两次）；
 *  ⑤ 相对路径 → 字段级红字，且**不发请求**。
 *
 * 另外断言页内说明（不能填 NFS 地址）与 PV/PVC YAML 的可复制性 —— 这是用户第三次提这件事的根因。
 */

const OUT_DIR = join(process.cwd(), '.data', 'mounts')

test.describe.configure({ mode: 'serial' })

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'admin'))
})

/** 打开管理台的「挂载点」页签 */
async function openMounts(page: import('@playwright/test').Page) {
  await page.goto('/admin')
  await page.getByRole('tab', { name: '挂载点' }).click()
  await expect(page.getByTestId('mounts-explainer')).toBeVisible({ timeout: 20_000 })
}

/** 记录写请求，用来验证"前端校验拦截时不发请求" */
async function spyWrites(page: import('@playwright/test').Page) {
  await page.evaluate(() => {
    const w = window as unknown as { __mountWrites: string[] }
    w.__mountWrites = []
  })
  await page.evaluate(async () => {
    const mod = (await import('/src/api/mock.ts')) as {
      mockApi: Record<string, (a?: unknown, b?: unknown) => Promise<unknown>>
    }
    const w = window as unknown as { __mountWrites: string[] }
    const record = (name: string) => {
      const original = mod.mockApi[name] as (a?: unknown, b?: unknown) => Promise<unknown>
      mod.mockApi[name] = (a?: unknown, b?: unknown) => {
        w.__mountWrites.push(name)
        return original.call(mod.mockApi, a, b)
      }
    }
    for (const name of ['createAdminSharedMount', 'updateAdminSharedMount', 'deleteAdminSharedMount']) {
      record(name)
    }
  })
}

function writes(page: import('@playwright/test').Page) {
  return page.evaluate(() => (window as unknown as { __mountWrites: string[] }).__mountWrites)
}

test('① 页签可切换 + 空态文案 + 页内说明（不能填 NFS 地址 / PV-PVC YAML）', async ({ page }) => {
  mkdirSync(OUT_DIR, { recursive: true })
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)

  // 空态：说明"未配置不会阻断 Pod"
  const empty = page.getByTestId('mounts-empty')
  await expect(empty).toBeVisible()
  await expect(empty).toContainText('还没有挂载点')
  await expect(empty).toContainText('未配置时不会阻断 Pod')

  // 页内说明：Pod 里不能直接填 NFS 地址（用户最容易误解的地方）
  const explainer = page.getByTestId('mounts-explainer')
  await expect(explainer).toContainText('Pod 里不能直接填 NFS 地址')
  await expect(explainer).toContainText('restricted')

  // 可复制的 PV + PVC YAML：NFS 示例三要素都在
  const yaml = page.getByTestId('pv-pvc-yaml')
  await expect(yaml).toContainText('kind: PersistentVolume')
  await expect(yaml).toContainText('ReadOnlyMany')
  await expect(yaml).toContainText('persistentVolumeReclaimPolicy: Retain')
  await expect(yaml).toContainText('server: 10.0.0.10')
  await expect(page.getByTestId('copy-pv-pvc')).toBeVisible()

  // 其它 tab 仍可用（同一套 Tabs）
  await page.getByRole('tab', { name: 'Profiles' }).click()
  await expect(page.getByRole('tab', { name: '挂载点' })).toHaveAttribute('aria-selected', 'false')
  await page.getByRole('tab', { name: '挂载点' }).click()
  await expect(page.getByTestId('mounts-explainer')).toBeVisible()

  await page.screenshot({ path: join(OUT_DIR, 'admin-mounts-1440.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(600)
  await page.screenshot({ path: join(OUT_DIR, 'admin-mounts-390.png'), fullPage: true })
})

test('② 新建 /models：出现在表格，且默认只读 + 启用', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)

  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('models')
  await page.getByTestId('mount-path').fill('/models')
  await page.getByTestId('mount-claim').fill('models-pvc')
  // 默认值：只读开、启用开
  await expect(page.getByTestId('mount-read-only')).toBeChecked()
  await expect(page.getByTestId('mount-enabled')).toBeChecked()

  await page.getByTestId('save-mount').click()

  const row = page.getByTestId('mount-row-models')
  await expect(row).toBeVisible({ timeout: 15_000 })
  await expect(row).toContainText('/models')
  await expect(row).toContainText('models-pvc')
  await expect(row).toContainText('只读')
  await expect(row).toContainText('启用')
  await expect(page.getByTestId('mounts-empty')).toHaveCount(0)
})

test('③ 编辑改成停用：表格显示未启用，且勾选即走 PUT', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)

  // 先造一条
  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('dataset')
  await page.getByTestId('mount-path').fill('/datasets')
  await page.getByTestId('mount-claim').fill('dataset-pvc')
  await page.getByTestId('save-mount').click()
  await expect(page.getByTestId('mount-row-dataset')).toBeVisible({ timeout: 15_000 })

  // 编辑 → 取消勾选「启用」
  await page.getByTestId('edit-mount-dataset').click()
  await expect(page.getByTestId('mount-name')).toBeDisabled()
  await page.getByTestId('mount-enabled').uncheck()
  await page.getByTestId('save-mount').click()

  await expect(page.getByTestId('mount-row-dataset')).toContainText('未启用', { timeout: 15_000 })

  // 表格里勾回来 = 直接 PUT（不需要进对话框）。
  // 注意：勾选是"受控 + 异步保存"，状态要等 PUT 回来才翻转，所以用 click + 轮询而不是 check()
  await spyWrites(page)
  await page.getByTestId('mount-enabled-dataset').click()
  await expect
    .poll(async () => (await writes(page)).length, { message: '勾选应触发一次 PUT' })
    .toBeGreaterThan(0)
  expect(await writes(page)).toContain('updateAdminSharedMount')
  await expect(page.getByTestId('mount-row-dataset')).toContainText('启用', { timeout: 15_000 })
})

test('④ 删除要二次确认：先弹确认框，确认后才消失', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)

  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('to-delete')
  await page.getByTestId('mount-path').fill('/to-delete')
  await page.getByTestId('mount-claim').fill('to-delete-pvc')
  await page.getByTestId('save-mount').click()
  await expect(page.getByTestId('mount-row-to-delete')).toBeVisible({ timeout: 15_000 })

  // 第一次点击：只出确认框，行还在
  await page.getByTestId('delete-mount-to-delete').click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('只影响之后提交的 Run')
  await expect(dialog).toContainText('已在跑的 Pod 不受影响')
  await page.getByRole('button', { name: '取消' }).click()
  await expect(page.getByTestId('mount-row-to-delete')).toBeVisible()

  // 第二次：确认后消失
  await page.getByTestId('delete-mount-to-delete').click()
  await page.getByTestId('confirm-delete-mount').click()
  await expect(page.getByTestId('mount-row-to-delete')).toHaveCount(0, { timeout: 15_000 })
})

test('⑤ 相对路径 → 字段级红字，且不发请求', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)
  await spyWrites(page)

  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('bad')
  await page.getByTestId('mount-path').fill('models') // 没有 / 开头
  await page.getByTestId('mount-claim').fill('bad-pvc')
  await page.getByTestId('save-mount').click()

  // 字段级错误（红边 + 下方红字）
  const pathInput = page.getByTestId('mount-path')
  await expect(pathInput).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByText('路径必须以 / 开头（容器内绝对路径）')).toBeVisible()
  // 对话框还开着（没保存成功）
  await expect(page.getByRole('dialog')).toBeVisible()
  // 前端就拦下了：一个写请求都没发
  expect(await writes(page)).toHaveLength(0)

  // 命中保留路径也要拦
  await pathInput.fill('/tmp/cache')
  await page.getByTestId('save-mount').click()
  await expect(page.getByText(/路径命中平台保留路径/)).toBeVisible()
  expect(await writes(page)).toHaveLength(0)

  // 改成合法路径就能存
  await pathInput.fill('/models')
  await page.getByTestId('save-mount').click()
  await expect(page.getByTestId('mount-row-bad')).toBeVisible({ timeout: 15_000 })
})

test('⑥ 后端 400 的 issues 也渲染到字段下方（服务端校验不被吞掉）', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openMounts(page)

  // 造一条占用 /models，再用另一条去撞同路径 → 后端 400 CONFLICT
  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('first')
  await page.getByTestId('mount-path').fill('/models')
  await page.getByTestId('mount-claim').fill('first-pvc')
  await page.getByTestId('save-mount').click()
  await expect(page.getByTestId('mount-row-first')).toBeVisible({ timeout: 15_000 })

  await page.getByTestId('new-mount').click()
  await page.getByTestId('mount-name').fill('second')
  await page.getByTestId('mount-path').fill('/models')
  await page.getByTestId('mount-claim').fill('second-pvc')
  await page.getByTestId('save-mount').click()

  await expect(page.getByText(/路径已被 first 占用/)).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('mount-path')).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByRole('dialog')).toBeVisible()
})
