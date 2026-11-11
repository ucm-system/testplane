import { expect, test, type Page } from '@playwright/test'

/**
 * 用户这一轮提的 7 条 UI 问题的回归用例（`web/` 侧）：
 *
 *  2. 提交页不预填默认值，只留 placeholder；空表单被**前端**拦下；
 *  3. 右栏只显示当前选中的那个 Tester，参数可一键追加进 args；
 *  4. Task 行的 Tester 详情抽屉（与 `/testers?open=` 同一个组件）；
 *  5. Deploy YAML 实时校验：结构（provider + parameters）与语法都标在编辑器里；
 *  6. 页脚 `API: /api/v1` 链到真实存在的 `/api/v1/meta`；
 *  7. 统一 404 页（路由 / Run 详情 404 / 两个按钮 / 减少动效时动画关闭）。
 *
 * 1（执行链路末尾箭头）在 `run-views.spec.ts` 里补；这里只覆盖 2–7。
 * 全部跑 mock（`VITE_USE_MOCK=1`，见 playwright.config.ts），不连真后端。
 */

test.describe.configure({ mode: 'serial' })

/** 打开提交页并等 Tester 目录到位（右栏要拿到参数清单） */
async function openSubmit(page: Page): Promise<void> {
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/submit')
  await expect(page.getByLabel('runner 1 Runner')).toContainText('aisbench', { timeout: 15_000 })
}

/** 把 YAML 编辑器内容整体换掉（CodeMirror 是 contenteditable，不能直接 fill） */
async function replaceYaml(page: Page, text: string): Promise<void> {
  const editor = page.getByLabel('Genet 部署描述 YAML')
  await editor.click()
  await page.keyboard.press('ControlOrMeta+A')
  await page.keyboard.insertText(text)
}

/* ============================================================ 2. 默认值为空 */

test('② 提交页默认值全空：Run 名 / Task 名 / 参数 / env / secret 都只留 placeholder', async ({ page }) => {
  await openSubmit(page)

  const runName = page.getByLabel('Task 名称')
  await expect(runName).toHaveValue('')
  await expect(runName).toHaveAttribute('placeholder', '如 qwen3-32b-daily')

  const taskName = page.getByLabel('runner 1 名称')
  await expect(taskName).toHaveValue('')
  await expect(taskName).toHaveAttribute('placeholder', '如 accuracy-dataset-a')

  // 参数不再预填 `--dataset=/data/gsm8k.jsonl --limit=200`：值是空的，示例只在灰字里
  const args = page.getByLabel('runner 1 输入')
  await expect(args).toHaveValue('')
  await expect(args).toHaveAttribute('placeholder', /\"dataset\"/)

  // 环境变量 / 凭据引用默认 0 行（不再预填 LOG_LEVEL=info 与 ENDPOINT_API_KEY）
  await expect(page.getByLabel('env 键 1')).toHaveCount(0)
  await expect(page.getByLabel('secret 键 1')).toHaveCount(0)
  await expect(page.getByTestId('env-empty-hint')).toBeVisible()

  // placeholder 是灰字（用 fg-3 令牌，与正文颜色不同）
  const colors = await runName.evaluate((el) => {
    const probe = document.createElement('span')
    probe.style.color = 'var(--color-fg-3)'
    document.body.appendChild(probe)
    const fg3 = getComputedStyle(probe).color
    probe.remove()
    return { placeholder: getComputedStyle(el, '::placeholder').color, fg3, value: getComputedStyle(el).color }
  })
  expect(colors.placeholder).toBe(colors.fg3)
  expect(colors.placeholder).not.toBe(colors.value)
})

test('② 空表单点提交：前端拦下并给字段级错误，不发请求也不跳走', async ({ page }) => {
  await openSubmit(page)

  await page.getByTestId('submit-task').click()

  const summary = page.getByTestId('form-error-summary')
  await expect(summary).toBeVisible({ timeout: 10_000 })
  await expect(summary).toContainText('task.name')
  await expect(summary).toContainText('name 必填')
  await expect(summary).toContainText('task.runners[0].name')
  await expect(summary).toContainText('runner 名称必填')

  // 字段级：输入框 aria-invalid + 一行红字（不是只有顶部摘要）
  await expect(page.getByLabel('Task 名称')).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByLabel('runner 1 名称')).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByTestId('runner-name-error-1')).toBeVisible()

  // 停在本页：没有创建任何 Run
  await expect(page).toHaveURL(/\/submit$/)
})

/* ============================================================ 3. 参数说明只显示选中的 Tester（原右栏，已搬进主内容折叠卡） */

test('③ 参数说明只显示当前 Task 选中的 Tester，换 Tester 跟着换', async ({ page }) => {
  await openSubmit(page)

  // 右栏已经取消：说明搬进主内容里的 <details>，默认折叠，点开才看
  await page.getByTestId('submit-runner-help').locator('summary').click()

  const rail = page.getByTestId('rail-runner')
  await expect(rail).toBeVisible()
  await expect(rail).toHaveAttribute('data-runner', 'aisbench-acc')
  // 只有选中的这一个：别的 Tester 的参数不该出现
  await expect(rail).toContainText('dataset')
  await expect(page.getByTestId('rail-runner')).toContainText('limit')
  await expect(page.getByText('input_lens')).toHaveCount(0)
  await expect(page.getByText('subset', { exact: true })).toHaveCount(0)

  // 换 Tester：右栏跟着换，旧 Tester 的参数消失
  await page.getByLabel('runner 1 Runner').selectOption('aisbench-perf')
  await expect(page.getByTestId('rail-runner')).toHaveAttribute('data-runner', 'aisbench-perf')
  await expect(page.getByTestId('rail-runner')).toContainText('input_len')
  await expect(page.getByTestId('rail-runner')).not.toContainText('dataset')
})

test('③ 参数旁「复制」把 jsonl 片段追加进输入框（不覆盖），输入示例整段同样追加', async ({ page }) => {
  await openSubmit(page)
  // 参数说明默认折叠：先展开再点复制
  await page.getByTestId('submit-runner-help').locator('summary').click()

  const args = page.getByLabel('runner 1 输入')
  await expect(args).toHaveValue('')

  // v2：点参数复制追加 jsonl 片段 `"name": <default|示例值>`（没有默认值按类型给示例）
  await page.locator('[data-testid="copy-param"][data-param="limit"]').click()
  await expect(args).toHaveValue('"limit": 1')

  // 再点 dataset：追加，不覆盖（换行接上）
  await page.locator('[data-testid="copy-param"][data-param="dataset"]').click()
  await expect(args).toHaveValue('"limit": 1\n"dataset": "value"')

  // 输入示例（placeholder）整段一键复制：也走追加
  await page.getByTestId('copy-placeholder').click()
  await expect(args).toHaveValue('"limit": 1\n"dataset": "value"\n{"dataset":"/data/gsm8k.jsonl","limit":200}')
})

/* ============================================================ 4. Task 行 Tester 详情 */

test('④ Task 行的「详情」抽屉：与 Runner 目录「详情」同一套组件，参数可复制进 args', async ({ page }) => {
  await openSubmit(page)

  await page.locator('[data-testid="runner-runner-detail"]').first().click()

  const drawer = page.getByRole('dialog')
  await expect(drawer).toBeVisible()
  // 同一个组件（TesterDrawer）：name/version/role/失败策略/独占/镜像/参数清单/placeholder
  await expect(page.getByTestId('runner-drawer')).toBeVisible()
  await expect(drawer).toContainText('aisbench-acc')
  await expect(drawer).toContainText('数据集精度评测')
  await expect(drawer).toContainText('参数 schema')
  await expect(drawer).toContainText('registry.dev.huawei.com/argus/aisbench-acc')
  await expect(drawer).toContainText('continue')

  // 抽屉里点参数复制 → 追加进当前 Task 的 args
  await page.locator('[data-testid="drawer-copy-param"][data-param="limit"]').click()
  await page.getByRole('button', { name: '关闭' }).click()
  await expect(drawer).toBeHidden()
  await expect(page.getByLabel('runner 1 输入')).toHaveValue('"limit": 1')

  // 目录页打开的还是同一个组件（同样的 data-testid 与说明）
  await page.goto('/testers?open=aisbench-acc')
  await expect(page.getByRole('dialog')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('runner-drawer')).toBeVisible()
  await expect(page.getByRole('dialog')).toContainText('数据集精度评测')
})

/* ============================================================ 5. YAML 实时校验 */

// TODO(e2e): 待集群联调 —— v2 deploy.spec 是 Genet 部署描述原文（平台不解析语义），
// v1「缺 parameters 结构校验」的前题不存在；按 v2 校验口径（YAML 语法 + 映射形状）重写后再跑。
test.fixme('⑤ YAML 结构无效（缺 parameters）：一行错误 + 编辑器标红 + 字段级提示', async ({ page }) => {
  await openSubmit(page)
  await replaceYaml(page, 'provider: genet')

  const error = page.getByTestId('deploy-yaml-error')
  await expect(error).toHaveAttribute('data-state', 'error', { timeout: 5_000 })
  await expect(error).toContainText(/第 \d+ 行第 \d+ 列：结构无效/)
  await expect(error).toContainText('parameters')

  // 字段级：指出缺的是 deploy.parameters（不是"能 parse 就算有效"）
  await expect(page.getByTestId('deploy-yaml-field-hint')).toContainText('deploy.parameters')

  // 编辑器里标出来：红波浪 + gutter 圆点
  await expect(page.locator('.cm-yaml-error-mark').first()).toBeVisible()
  await expect(page.locator('.cm-yaml-error-dot').first()).toBeVisible()

  // 补上 parameters → 立刻变有效
  await replaceYaml(page, 'provider: genet\nparameters:\n  model: Qwen3-32B')
  await expect(error).toHaveAttribute('data-state', 'ok')
  await expect(error).toContainText('✓ YAML 结构有效（provider + parameters）')
  await expect(page.locator('.cm-yaml-error-mark')).toHaveCount(0)
})

test('⑤ YAML 语法错误：报错带行列，修好后标记消失', async ({ page }) => {
  await openSubmit(page)
  await replaceYaml(page, 'foo: [')

  const error = page.getByTestId('deploy-yaml-error')
  await expect(error).toHaveAttribute('data-state', 'error', { timeout: 5_000 })
  await expect(error).toContainText(/第 \d+ 行第 \d+ 列：YAML 语法错误/)
  await expect(page.locator('.cm-yaml-error-mark').first()).toBeVisible()
  await expect(page.locator('.cm-yaml-error-dot').first()).toBeVisible()

  await replaceYaml(page, 'provider: genet\nparameters:\n  model: Qwen3-32B')
  await expect(error).toHaveAttribute('data-state', 'ok')
  await expect(page.locator('.cm-yaml-error-mark')).toHaveCount(0)
})

/* ============================================================ 6. 页脚 API 链接 */

// TODO(e2e): 待集群联调 —— v2 侧栏底部有 文档/API/GitHub 入口（AppSidebar footer），
// 本用例"没有 API 链接"的前提已不成立；按 v2 信息架构重写后再跑。
test.fixme('⑥ 全局页脚已删除：chrome 里不再出现 API 前缀，页面高度撑到视口底部', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/')
  await expect(page.getByText('全部 Task')).toBeVisible()

  // 页脚整段删掉：既没有 footer 元素，也没有那条 API 链接
  await expect(page.locator('footer')).toHaveCount(0)
  await expect(page.getByTestId('sidebar-footer-API')).toHaveCount(0)
  await expect(page.getByText('TestPlane · 控制台')).toHaveCount(0)

  // shadcn 骨架（inset 变体）：主体底边贴住视口底边，只留 inset 的 8px 页边距
  const bottom = await page.evaluate(() => {
    const main = document.querySelector('#main')!
    return { main: Math.round(main.getBoundingClientRect().bottom), vh: window.innerHeight }
  })
  expect(bottom.main).toBeGreaterThanOrEqual(bottom.vh - 10)
})

/* ============================================================ 7. 统一 404 */

test('⑦ 未匹配路由与 /404 是同一个 404 组件，带两个可用动作', async ({ page }) => {
  await page.goto('/does-not-exist')

  const notFound = page.getByTestId('not-found')
  await expect(notFound).toBeVisible()
  // 同页只能有一个 main landmark：404 用 section + aria-labelledby（不再是 role=main）
  await expect(notFound).not.toHaveAttribute('role', 'main')
  await expect(notFound).toHaveAttribute('aria-labelledby', 'not-found-title')
  await expect(page.locator('main')).toHaveCount(1)
  await expect(notFound.getByRole('heading', { level: 1 })).toContainText('404')
  await expect(notFound).toContainText('这个地址没有对应的页面')

  // 两个按钮：回到概览 / 打开最近 Run 列表
  await expect(page.getByTestId('not-found-home')).toHaveAttribute('href', '/')
  await expect(page.getByTestId('not-found-tasks')).toHaveAttribute('href', '/tasks')

  // /404 走同一个组件
  await page.goto('/404')
  await expect(page.getByTestId('not-found')).toBeVisible()

  // 按钮真的可用
  await page.getByTestId('not-found-tasks').click()
  // /runs 现在重定向到概览（D-62）
  await expect(page).toHaveURL(/\/(\?.*)?$/)
})

test('⑦ /tasks/:id 取到 404：复用同一个 404 组件，文案按上下文变化', async ({ page }) => {
  await page.goto('/tasks/r-99999999-XXXX')

  const notFound = page.getByTestId('not-found')
  await expect(notFound).toBeVisible({ timeout: 15_000 })
  await expect(notFound).toContainText('Task 不存在或已被清理')
  await expect(page.getByTestId('not-found-home')).toBeVisible()
  await expect(page.getByTestId('not-found-tasks')).toBeVisible()
  // 不再是「加载失败」的红色错误条
  await expect(page.getByText('加载失败')).toHaveCount(0)
})

test('⑦ 404 主视觉是图片（webp，装饰性 alt 为空）；没有残留的内联 SVG 动画', async ({ page }) => {
  await page.goto('/404')

  // 用户 2026-11-09：404 的主元素换成提供的插画（public/404.webp，760px / 69KB）
  const art = page.getByTestId('not-found-art')
  await expect(art).toBeVisible()
  await expect(art).toHaveAttribute('src', '/404.webp')

  // 装饰性插画：alt 为空 + aria-hidden；页面身份由 h1 里的「404」承担
  await expect(art).toHaveAttribute('alt', '')
  await expect(art).toHaveAttribute('aria-hidden', 'true')

  // 图片解码是异步的：轮询到 naturalWidth>0 再断言，别和浏览器解码时序赛跑
  await expect
    .poll(
      () => art.evaluate((el) => (el.complete ? el.naturalWidth : 0)),
      { timeout: 10_000, intervals: [200] },
    )
    .toBeGreaterThan(0)

  // 纸飞机那套内联 SVG 动效已删除（连 utility 一起），不要回潮
  // 插画不再用内联 SVG（按钮里的 lucide 图标不算）：断言没有 role=img 的 SVG、也没有旧动画类
  await expect(page.locator('[data-testid="not-found"] svg[role="img"]')).toHaveCount(0)
  await expect(page.locator('.notfound-float, .notfound-dash')).toHaveCount(0)
})
