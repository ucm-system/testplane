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
  await page.addInitScript(() => window.localStorage.setItem('argus:mock-role', 'operator'))
  await page.goto('/submit')
  await expect(page.getByLabel('runner 1 Runner')).toContainText('aisbench', { timeout: 15_000 })
}

/** 把 YAML 编辑器内容整体换掉（CodeMirror 是 contenteditable，不能直接 fill 语义） */
async function replaceYaml(page: Page, text: string): Promise<void> {
  const editor = page.getByLabel('部署描述 YAML')
  await editor.click()
  await page.keyboard.press('ControlOrMeta+A')
  await page.keyboard.insertText(text)
}

test('① 0 个测试 runner：允许提交（D-77，只验证服务能否启动），空态给中性说明', async ({ page }) => {
  await openSubmit(page)

  const submit = page.getByTestId('submit-task')
  const validate = page.getByTestId('validate-task')
  await expect(submit).toBeEnabled()

  // 干跑前先给 Task 名称（frontendIssues 的 name 必填与 0-runner 无关）
  await page.getByLabel('Task 名称').fill('deploy-only-check')
  await page.getByTestId('remove-runner-1').click()

  // 中性说明（不再是置灰警告）：只跑 服务来源 + precheck/clean
  const hint = page.getByTestId('runner-empty-hint')
  await expect(hint).toBeVisible()
  await expect(hint).toContainText('未添加测试 runner')
  await expect(hint).toContainText('适合验证服务能否启动')

  // 提交/校验保持可用：dry-run 通过（mock 校验同样允许 0 个 runner）
  await expect(submit).toBeEnabled()
  await expect(validate).toBeEnabled()
  await validate.click()
  await expect(page.getByTestId('form-error-summary')).toHaveCount(0)
  await expect(page.getByText('dry-run 通过')).toBeVisible({ timeout: 15_000 })
})

test('② 删掉最后一个 runner：提交始终可用；撤销恢复原行', async ({ page }) => {
  await openSubmit(page)

  // 默认值是空的（见 ux-submit-defaults）：先自己写一个名字，撤销才有「原值」可恢复
  await page.getByLabel('runner 1 名称').fill('accuracy-dataset-a')

  const submit = page.getByTestId('submit-task')
  await page.getByTestId('remove-runner-1').click()

  // D-77：删空不再置灰，只是出现中性说明
  await expect(submit).toBeEnabled()
  await expect(page.getByTestId('runner-empty-hint')).toBeVisible()

  // C5：删除给 undo toast，撤销把 runner 原位置放回去
  const toast = page.locator('[data-sonner-toaster]')
  await expect(toast).toContainText('已删除 runner accuracy-dataset-a')
  await toast.getByRole('button', { name: '撤销' }).click()

  await expect(page.getByLabel('runner 1 名称')).toHaveValue('accuracy-dataset-a')
  await expect(submit).toBeEnabled()
  await expect(page.getByTestId('runner-empty-hint')).toHaveCount(0)
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

  const name = page.getByLabel('Task 名称')
  const counter = page.getByTestId('task-name-counter')
  const submit = page.getByTestId('submit-task')

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



test('⑧ 跨行美化 JSON 粘贴：一条可操作错误（点格式化压成一行后恢复可提交）', async ({ page }) => {
  await openSubmit(page)

  // 把美化打印的 JSON 直接贴进 jsonl 输入框（最常见的 JSONL 误写）
  const args = page.getByLabel('runner 1 输入')
  const multiline = ['{"dataset":"/data/gsm8k.jsonl","sla":["ttft.avg:1200"],', '"input_len":2048,"limit":1}'].join('\n')
  await args.fill(multiline)

  // 不是逐行的 N 条语法错误，而是一条可操作错误（提示点「压缩」）
  const issues = page.getByTestId('input-issues-1')
  await expect(issues).toBeVisible()
  await expect(issues).toContainText('跨行的单个 JSON 对象')
  await expect(issues).toContainText('格式化')
  // 只有这一条：不再出现逐行 JSON 语法错误的噪音
  await expect(issues.locator('li')).toHaveCount(1)

  // 一键救回：点「压缩」→ 单行合法 JSONL，错误清空
  await page.getByTestId('input-format-1').click()
  await expect(args).toHaveValue('{"dataset": "/data/gsm8k.jsonl", "sla": ["ttft.avg:1200"], "input_len": 2048, "limit": 1}')
  await expect(page.getByTestId('input-issues-1')).toHaveCount(0)
})

test('⑦ 只有 pre_check 角色的 Runner：目录空仍可提交（服务来源 + precheck）', async ({ page }) => {
  await openSubmit(page)

  // 目录里只剩 pre_check 角色：B10 描述的就是这种「下拉只有一句占位」的状态
  await page.evaluate(async () => {
    const mock = (await import('/src/api/mock.ts')) as {
      mockApi: { listRunners: (query?: unknown) => Promise<Array<{ role: string }>> }
    }
    const original = mock.mockApi.listRunners.bind(mock.mockApi)
    mock.mockApi.listRunners = async (query) => (await original(query)).filter((runner) => runner.role === 'pre_check')
  })

  // 站内切走再切回：提交页真卸载再重挂 → 重新拉目录（补丁还在，整页刷新会丢）
  await page.getByRole('link', { name: '概览' }).click()
  await expect(page.getByText('全部 Task')).toBeVisible()
  await page.getByRole('link', { name: '提交 Task' }).click()
  await expect(page).toHaveURL(/\/submit$/)

  const hint = page.getByTestId('runner-empty-registered')
  await expect(hint).toBeVisible({ timeout: 15_000 })
  await expect(hint).toContainText('目录里暂无测试类 Runner')

  // D-77：服务来源（genet-deploy）+ precheck（auto 注入）仍可提交
  const submit = page.getByTestId('submit-task')
  await expect(submit).toBeEnabled()
  await expect(page.getByTestId('validate-task')).toBeEnabled()

  // 下拉里只剩占位，没有可选的测试 runner
  await expect(page.getByLabel('runner 1 Runner')).toHaveValue('')
})

test('⑨ 单行 JSON 带尾随换行（重试回填常见）：不算跨行，不报错', async ({ page }) => {
  await openSubmit(page)

  // 重试回填的原文常带行尾换行符（服务端逐行 trim 跳空行，本就合法）——
  // 校验器不得把它误判成「跨行的单个 JSON 对象」（用户 2026-11-13：点重试全报错）
  const args = page.getByLabel('runner 1 输入')
  await args.fill('{"dataset":"/data/gsm8k.jsonl","limit":200}\n')
  await expect(page.getByTestId('input-issues-1')).toHaveCount(0)

  // 真跨行（内部有换行）仍然报，且一键格式化可救回
  await args.fill('{"a":1,\n"b":2}')
  await expect(page.getByTestId('input-issues-1')).toContainText('跨行的单个 JSON 对象')
  await page.getByTestId('input-format-1').click()
  await expect(args).toHaveValue('{"a": 1, "b": 2}')
})
