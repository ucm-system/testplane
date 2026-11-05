// 演示截图：打真实后端（dev 鉴权），把关键界面逐页截下来。
// 用法（在仓库根）：node .data/demo/capture.mjs
//   前置：kubectl port-forward svc/testplane 8132:8080，且已用 demo 数据造过 Run。
import { chromium } from '@playwright/test'
import { mkdirSync, writeFileSync } from 'node:fs'

const BASE = process.env.DEMO_BASE ?? 'http://127.0.0.1:8132'
const OUT = process.env.DEMO_OUT ?? '.data/demo/shots'
const RUNS = JSON.parse(process.env.DEMO_RUNS ?? '{}')

mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch({ channel: 'chrome', headless: true })
const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 }, recordVideo: { dir: process.env.DEMO_VIDEO ?? '.data/demo/video', size: { width: 1440, height: 1000 } } })
// dev 鉴权：前端会自带 Bearer dev；这里只是保险
await ctx.addInitScript(() => {
  localStorage.setItem('testplane:demo', '1')
})
const page = await ctx.newPage()
const errors = []
page.on('pageerror', (e) => errors.push(String(e)))

async function shot(name, path, opts = {}) {
  await page.goto(BASE + path, { waitUntil: 'networkidle' })
  await page.waitForTimeout(1200)
  if (opts.before) await opts.before(page)
  await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: opts.fullPage !== false })
  console.log(`[shot] ${name} <- ${path}`)
}

const s = Date.now()

// 1) 首页概览
await shot('01-home', '/')

// 2) Run 列表
await shot('02-runs', '/runs')

// 3) 成功的 Run（service 模式）
if (RUNS.green) await shot('03-run-succeeded', `/runs/${RUNS.green}`)

// 4) 失败的 Run + 重试对话框
if (RUNS.red) {
  await page.goto(`${BASE}/runs/${RUNS.red}`, { waitUntil: 'networkidle' })
  await page.waitForTimeout(1000)
  const retry = page.locator('[data-testid="retry-run"]')
  if (await retry.count()) {
    await retry.first().click()
    await page.waitForTimeout(1500)
    await page.screenshot({ path: `${OUT}/04-retry-dialog.png`, fullPage: true })
    console.log('[shot] 04-retry-dialog')
  } else {
    await page.screenshot({ path: `${OUT}/04-run-failed.png`, fullPage: true })
    console.log('[shot] 04-run-failed（未找到重试按钮，截了详情页）')
  }
}

// 5) 报告
if (RUNS.red) await shot('05-report', `/runs/${RUNS.red}/report`)

// 6) 集群与 Volcano 队列
await shot('06-clusters', '/clusters')

// 7) 管理面（审计 / 集群 / Profile）
await shot('07-admin', '/admin')

// 8) Deploy 形态的 Run（两根轴 + cleanup policy）
if (RUNS.deploy) await shot('08-run-deploy', `/runs/${RUNS.deploy}`)

// 9) 提交页
await shot('09-submit', '/submit')

// 10) 队列状态接口（给“不可达 ≠ 0 排队”留证）
const qs = await page.evaluate(async () => {
  const r = await fetch('/api/v1/queue-status', { headers: { Authorization: 'Bearer dev' } })
  return r.json()
})
writeFileSync(`${OUT}/queue-status.json`, JSON.stringify(qs, null, 2))

const v = page.video()
await ctx.close()
await browser.close()
if (v) { const vp = await v.path(); console.log('[video] ' + vp) }
console.log(`done in ${((Date.now() - s) / 1000).toFixed(1)}s, errors=${errors.length}`)
if (errors.length) console.log(errors.join('\n'))
