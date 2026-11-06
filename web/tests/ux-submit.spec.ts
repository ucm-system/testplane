import { expect, test, type Page } from '@playwright/test'

/**
 * 提交页 UX 收口（A4 / B8 / B10 / B11 / C5 / C8）的回归用例。
 *
 * 全部跑 mock（`VITE_USE_MOCK` 默认 1，见 playwright.config.ts），不连真后端。
 * 每条都是「用户能看见的那一层」：置灰、说明文案、toast、gutter/波浪线、计数。
 */

test.describe.configure({ mode: 'serial' })

/** 打开提交页并等 Tester 目录到位（否则 B10 的置灰会把按钮一直拦住） */
async function openSubmit(page: Page): Promise<void> {
  await page.addInitScript(() => window.localStorage.setItem('testplane:mock-role', 'operator'))
  await page.goto('/submit')
  await expect(page.getByLabel('Task 1 Tester')).toContainText('aisbench', { timeout: 15_000 })
}

/** 把 YAML 编辑器内容整体换掉（CodeMirror 是 contenteditable，不能直接 fill 语义） */
async function replaceYaml(page: Page, text: string): Promise<void> {
  const editor = page.getByLabel('deploy 段 YAML')
  await editor.click()
  await page.keyboard.press('ControlOrMeta+A')
  await page.keyboard.insertText(text)
}

test('① 0 个测试 Task：提交/校验置灰（disabled + aria-disabled + title）且 Task 区说明可见', async ({ page }) => {
  await openSubmit(page)

  // 默认 1 个 Task → 先确认按钮是能点的（否则下面的置灰断言没有说服力）
  const submit = page.getByTestId('submit-run')
  const validate = page.getByTestId('validate-run')
  await expect(submit).toBeEnabled()

  await page.getByTestId('remove-task-1').click()

  // 说明就出现在 Task 卡片区
  const hint = page.getByTestId('task-empty-hint')
  await expect(hint).toBeVisible()
  await expect(hint).toHaveText('至少需要 1 个测试 Task（Pre-Check 由平台注入，不计入测试）')

  // 两个按钮：真 disabled + aria-disabled + title 说清原因
  for (const button of [submit, validate]) {
    await expect(button).toBeDisabled()
    await expect(button).toHaveAttribute('aria-disabled', 'true')
    await expect(button).toHaveAttribute('title', /至少需要 1 个测试 Task/)
  }

  // 点了也不会发请求（按钮 disabled，dry-run 的摘要区不该出现）
  await page.getByTestId('submit-run').click({ force: true })
  await expect(page.getByTestId('form-error-summary')).toHaveCount(0)
})

test('② 删掉最后一个 Task 后立即置灰；撤销后恢复并重新可用', async ({ page }) => {
  await openSubmit(page)

  // 默认值是空的（见 ux-submit-defaults）：先自己写一个名字，撤销才有「原值」可恢复
  await page.getByLabel('Task 1 名称').fill('accuracy-dataset-a')

  const submit = page.getByTestId('submit-run')
  await page.getByTestId('remove-task-1').click()

  // 「立即」：不等 dry-run、不刷新，点完就应该变灰
  await expect(submit).toBeDisabled()
  await expect(page.getByTestId('task-empty-hint')).toBeVisible()

  // C5：删除给 undo toast，撤销把 Task 原位置放回去，A4 的置灰随之解除
  const toast = page.locator('[data-sonner-toaster]')
  await expect(toast).toContainText('已删除 Task accuracy-dataset-a')
  await toast.getByRole('button', { name: '撤销' }).click()

  await expect(page.getByLabel('Task 1 名称')).toHaveValue('accuracy-dataset-a')
  await expect(submit).toBeEnabled()
  await expect(page.getByTestId('task-empty-hint')).toHaveCount(0)
})

test('③ 环境变量删除后给 undo toast，撤销恢复原位置原值', async ({ page }) => {
  await openSubmit(page)

  // 默认 0 行：先手动加一行并填好，再删（验证的是「删除 → 撤销」这条链路）
  await expect(page.getByLabel('env 键 1')).toHaveCount(0)
  await page.getByTestId('add-env').click()
  await page.getByLabel('env 键 1').fill('LOG_LEVEL')
  await page.getByLabel('env 值 1').fill('info')
  await expect(page.getByLabel('env 键 1')).toHaveValue('LOG_LEVEL')

  await page.getByLabel('删除该环境变量').click()

  // 行真的没了
  await expect(page.getByLabel('env 键 1')).toHaveCount(0)

  // 有 undo toast（不是静默删除，也不是二次确认弹窗）
  const toast = page.locator('[data-sonner-toaster]')
  await expect(toast).toContainText('已删除 LOG_LEVEL')
  await expect(toast.getByRole('button', { name: /撤销/ })).toBeVisible()

  await toast.getByRole('button', { name: /撤销/ }).click()

  // 原位置原值回来
  await expect(page.getByLabel('env 键 1')).toHaveValue('LOG_LEVEL')
  await expect(page.getByLabel('env 值 1')).toHaveValue('info')
})

test('④ YAML 语法错误：波浪线 + gutter 标记存在，且编辑器滚到出错行', async ({ page }) => {
  await openSubmit(page)

  // 造一段「错在很下面」的 YAML：40 行合法 + 最后一行有语法错
  const lines = Array.from({ length: 40 }, (_, i) => `parameter_${i}: value_${i}`)
  lines.push('broken: [unclosed')
  await replaceYaml(page, lines.join('\n'))

  // 页面下方的解析结论走同一套（错误信息来自同一个 parseDeploy），并带行列
  const yamlError = page.getByTestId('deploy-yaml-error')
  await expect(yamlError).toHaveAttribute('data-state', 'error')
  await expect(yamlError).toContainText(/第 41 行第 \d+ 列：YAML 语法错误/)

  // 红色波浪线（mark decoration）与左侧 gutter 圆点
  await expect(page.locator('.cm-yaml-error-mark').first()).toBeVisible()
  await expect(page.locator('.cm-yaml-error-dot').first()).toBeVisible()

  // 编辑器必须已经滚到出错行（错在第 41 行，7 行高的编辑框不滚根本看不到）
  await expect
    .poll(async () => page.locator('.cm-scroller').first().evaluate((el) => el.scrollTop), { timeout: 10_000 })
    .toBeGreaterThan(0)

  // 修好之后标注消失（不是一次性红着不走）
  await replaceYaml(page, 'provider: genet\nparameters:\n  model: Qwen3-32B')
  await expect(page.locator('.cm-yaml-error-mark')).toHaveCount(0)
})

test('⑤ Run 名称：200 rune 通过、201 rune 红字 + 置灰（不等后端 400）', async ({ page }) => {
  await openSubmit(page)

  const name = page.getByLabel('Run 名称')
  const counter = page.getByTestId('run-name-counter')
  const submit = page.getByTestId('submit-run')

  // 200 个中文 = 200 rune（按 UTF-16 算会是 200，按字节算才是 600 —— 这里断言的就是 rune）
  await name.fill('中'.repeat(200))
  await expect(counter).toHaveText('已输入 200 / 200')
  await expect(submit).toBeEnabled()

  // 粘贴/脚本可以绕过 maxlength：201 rune → 立刻红字 + 置灰
  await name.evaluate((el, value) => {
    const input = el as HTMLInputElement
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set
    setter?.call(input, value)
    input.dispatchEvent(new Event('input', { bubbles: true }))
  }, '中'.repeat(201))

  await expect(counter).toHaveText('已输入 201 / 200')
  await expect(page.getByText('name 超过 200 字符（当前 201）')).toBeVisible()
  await expect(name).toHaveAttribute('aria-invalid', 'true')
  await expect(submit).toBeDisabled()
  await expect(submit).toHaveAttribute('aria-disabled', 'true')
})

test('⑥ YAML 长值（超长 model 名）横向滚动看全', async ({ page }) => {
  await openSubmit(page)

  await replaceYaml(page, `provider: genet\nparameters:\n  model: ${'Qwen3-32B-'.repeat(24)}end`)

  // 编辑器没有开 lineWrapping：超长行靠横向滚动，而不是折行或截断
  await expect
    .poll(
      async () =>
        page
          .locator('.cm-scroller')
          .first()
          .evaluate((el) => el.scrollWidth > el.clientWidth + 1),
      { timeout: 10_000 },
    )
    .toBe(true)

  const wrapped = await page
    .locator('.cm-content')
    .first()
    .evaluate((el) => getComputedStyle(el).whiteSpace)
  expect(wrapped).not.toBe('pre-wrap')
})

test('⑦ 只有 pre_check 角色的 Tester：无可用 Tester 时置灰并在 Task 区说明原因', async ({ page }) => {
  await openSubmit(page)

  // 目录里只剩 pre_check 角色：B10 描述的就是这种「下拉只有一句占位」的状态
  await page.evaluate(async () => {
    const mock = (await import('/src/api/mock.ts')) as {
      mockApi: { listTesters: (query?: unknown) => Promise<Array<{ role: string }>> }
    }
    const original = mock.mockApi.listTesters.bind(mock.mockApi)
    mock.mockApi.listTesters = async (query) => (await original(query)).filter((tester) => tester.role === 'pre_check')
  })

  // 站内切走再切回：提交页真卸载再重挂 → 重新拉目录（补丁还在，整页刷新会丢）
  await page.getByRole('link', { name: '运行列表' }).click()
  await expect(page.getByText('全部 Run')).toBeVisible()
  await page.getByRole('link', { name: '提交运行' }).click()
  await expect(page).toHaveURL(/\/submit$/)

  const hint = page.getByTestId('tester-empty-hint')
  await expect(hint).toBeVisible({ timeout: 15_000 })
  await expect(hint).toHaveText('当前没有可用的 Tester；只有 pre_check 角色的 Tester 不能作为测试 Task')

  const submit = page.getByTestId('submit-run')
  await expect(submit).toBeDisabled()
  await expect(submit).toHaveAttribute('aria-disabled', 'true')
  await expect(submit).toHaveAttribute('title', /只有 pre_check 角色的 Tester 不能作为测试 Task/)
  await expect(page.getByTestId('validate-run')).toBeDisabled()

  // 下拉里只剩占位，没有可选的 Tester
  await expect(page.getByLabel('Task 1 Tester')).toHaveValue('')
})
