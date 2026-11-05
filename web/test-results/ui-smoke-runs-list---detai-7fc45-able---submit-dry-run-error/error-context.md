# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: ui-smoke.spec.ts >> runs list -> detail task table -> submit dry-run error
- Location: tests\ui-smoke.spec.ts:8:1

# Error details

```
Error: expect(locator).toContainText(expected) failed

Locator: getByRole('alert').first()
Expected pattern: /VALIDATION_ERROR|未通过校验/
Received string:  "必须选择 Tester"
Timeout: 10000ms

Call log:
  - Expect "toContainText" getByRole('alert').first() with timeout 10000ms
  - waiting for getByRole('alert').first()
    23 × locator resolved to <p role="alert" class="text-2xs text-danger-text">必须选择 Tester</p>
       - unexpected value "必须选择 Tester"

```

```yaml
- alert: 必须选择 Tester
```

# Test source

```ts
  1  | import { expect, test } from '@playwright/test'
  2  | 
  3  | /**
  4  |  * 最小 UI 冒烟（PR #18 / 验收 #16）：
  5  |  *   /runs -> 点第一条 -> 详情页出现 Task 列表
  6  |  *   /submit -> 故意把 Task 名称清空 -> dry-run 报错可见
  7  |  */
  8  | test('runs list -> detail task table -> submit dry-run error', async ({ page }) => {
  9  |   await page.goto('/runs')
  10 |   await expect(page.getByText('全部 Run')).toBeVisible()
  11 | 
  12 |   const firstRun = page.locator('tbody a[href^="/runs/"]').first()
  13 |   await expect(firstRun).toBeVisible()
  14 |   const href = await firstRun.getAttribute('href')
  15 |   expect(href).toMatch(/^\/runs\/.+/)
  16 | 
  17 |   await firstRun.click()
  18 |   await expect(page).toHaveURL(new RegExp(href!.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
  19 |   await expect(page.getByText('Task 列表')).toBeVisible()
  20 |   await expect(page.locator('table').first()).toBeVisible()
  21 | 
  22 |   await page.goto('/submit')
  23 |   await expect(page.getByText('提交前会先 dry-run', { exact: false })).toBeVisible()
  24 | 
  25 |   // 清空 Task 1 名称，让 dry-run 返回校验错误（页面应显示“校验未通过” Banner）。
  26 |   const taskName = page.getByLabel('Task 1 名称')
  27 |   await expect(taskName).toBeVisible()
  28 |   await taskName.fill('')
  29 |   await page.getByRole('button', { name: '校验', exact: true }).click()
  30 | 
  31 |   // dry-run 的校验错误以 alert 呈现（ErrorState：VALIDATION_ERROR / 提交内容未通过校验）
  32 |   const alert = page.getByRole('alert').first()
  33 |   await expect(alert).toBeVisible()
> 34 |   await expect(alert).toContainText(/VALIDATION_ERROR|未通过校验/)
     |                       ^ Error: expect(locator).toContainText(expected) failed
  35 | })
  36 | 
  37 | /**
  38 |  * 角色门禁：viewer 只读。
  39 |  * mock 模式下用 `localStorage['testplane:mock-role']='viewer'` 注入 OIDC 角色
  40 |  * （会带假 csrf_token，前端按会话处理），再确认 Task 级取消按钮置灰且带提示。
  41 |  */
  42 | test('viewer：Task 取消按钮不可点', async ({ page }) => {
  43 |   await page.addInitScript(() => {
  44 |     window.localStorage.setItem('testplane:mock-role', 'viewer')
  45 |   })
  46 | 
  47 |   // mock 里这条 Run 处于 deploying、Task 全是 pending —— 都是可取消状态
  48 |   await page.goto('/runs/r-20261105-0005')
  49 |   await expect(page.getByText('Task 列表')).toBeVisible()
  50 | 
  51 |   const cancel = page.locator('[data-testid="cancel-task"]').first()
  52 |   await expect(cancel).toBeVisible()
  53 |   await expect(cancel).toBeDisabled()
  54 |   await expect(cancel).toHaveAttribute('title', '需要 operator 角色')
  55 | 
  56 |   // 顶栏能看到 viewer 身份（排障用）
  57 |   await expect(page.getByRole('button', { name: '账号与角色' })).toContainText('viewer')
  58 | })
  59 | 
  60 | test('operator：Task 取消按钮可点并走二次确认', async ({ page }) => {
  61 |   await page.addInitScript(() => {
  62 |     window.localStorage.setItem('testplane:mock-role', 'operator')
  63 |   })
  64 | 
  65 |   await page.goto('/runs/r-20261105-0005')
  66 |   await expect(page.getByText('Task 列表')).toBeVisible()
  67 | 
  68 |   const cancel = page.locator('[data-testid="cancel-task"]').first()
  69 |   await expect(cancel).toBeEnabled()
  70 |   await cancel.click()
  71 | 
  72 |   // 二次确认弹窗：说明"已完成的 Task 不受影响"
  73 |   const dialog = page.getByRole('dialog')
  74 |   await expect(dialog).toBeVisible()
  75 |   await expect(dialog).toContainText('已完成的 Task 不受影响')
  76 |   await expect(page.getByRole('button', { name: '确认取消 Task' })).toBeVisible()
  77 |   // 这里不点确认：点了会改 mock 内存状态，后续断言没必要依赖它
  78 | })
  79 | 
```