// 改前 / 改后并排对比图（票 #72 的证据）：把 .data/ui-audit/{before,after} 里同名截图并排合成。
//   node tests/ui-compare.mjs
// 产物：.data/ui-audit/after/compare-<页面>-<视口>.png
//
// 依赖顺序：先跑过 before（UI_AUDIT_PHASE=before）与 after（默认）两轮 ui-audit。
import { chromium } from '@playwright/test'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const BASE_DIR = join(ROOT, '.data', 'ui-audit')
const OUT = join(BASE_DIR, 'after')
mkdirSync(OUT, { recursive: true })

/** 挑三张最有信息量的：概览（KPI + 全量表）、详情（表头/页签/日志）、提交（表单/输入框/分段） */
const PAGES = ['概览', '详情', '提交']
const VIEWPORT = '1440'

const browser = await chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL ?? 'chrome', headless: true })

for (const name of PAGES) {
  const file = `${name}-${VIEWPORT}.png`
  const before = join(BASE_DIR, 'before', file)
  const after = join(BASE_DIR, 'after', file)
  if (!existsSync(before) || !existsSync(after)) {
    console.log(`skip ${name}: 缺少 before/after 截图`)
    continue
  }
  const html = `<!doctype html><meta charset="utf-8">
<style>
  * { box-sizing:border-box; }
  body { margin:0; background:#0b1220; color:#e2e8f0;
         font:13px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
  .wrap { padding:16px 18px 20px; }
  h1 { font-size:14px; margin:0 0 2px; font-weight:600; }
  p { margin:0 0 12px; color:#94a3b8; font-size:11.5px; }
  .row { display:grid; grid-template-columns:1440px 1440px; gap:14px; align-items:start; }
  .cell h2 { font-size:11.5px; margin:0 0 6px; color:#cbd5e1; font-weight:600; }
  .cell h2 b { color:#7dd3fc; font-weight:600; }
  .shot { background:#fff; border-radius:6px; overflow:hidden; }
  .shot img { display:block; width:1440px; height:auto; }
</style>
<div class="wrap">
  <h1>TestPlane 视觉精修 · ${name}（${VIEWPORT}px）</h1>
  <p>左：改前（手写 token / hex 色 / Albert Sans）；右：改后（shadcn oklch token / Inter / 两级阴影）。同一页面、同一视口、同一份 mock 数据。</p>
  <div class="row">
    <div class="cell"><h2>改前</h2><div class="shot"><img src="../before/${file}"></div></div>
    <div class="cell"><h2><b>改后</b></h2><div class="shot"><img src="${file}"></div></div>
  </div>
</div>`
  const htmlPath = join(OUT, `_compare-${name}.html`)
  writeFileSync(htmlPath, html, 'utf8')
  const page = await browser.newPage({ viewport: { width: 2930, height: 400 }, deviceScaleFactor: 1 })
  await page.goto('file:///' + htmlPath.replace(/\\/g, '/'))
  await page.waitForTimeout(300)
  await page.screenshot({ path: join(OUT, `compare-${name}-${VIEWPORT}.png`), fullPage: true })
  await page.close()
  console.log(`compare-${name}-${VIEWPORT}.png`)
}

await browser.close()

