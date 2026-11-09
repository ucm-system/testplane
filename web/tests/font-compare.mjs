// 字体并排对比（票 #72 的证据）：同一页面、同一视口、同一裁切框，只换 --font-sans 的第一顺位。
//   node tests/font-compare.mjs
// 产物：.data/ui-audit/font-compare/{inter,albert}-{kpi,table}.png 与合成图 font-compare.png
//
// 为什么不是"凭感觉说 Inter 更好"：同一段中文+英文+数字混排，在 13–14px 下比较
// 字腔、数字对齐、中英基线；结论写在 docs/research/shadcn-visual-specs.md §4。
import { chromium } from '@playwright/test'
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const BASE = (process.env.PLAYWRIGHT_BASE_URL ?? 'http://127.0.0.1:5173').replace(/\/$/, '')
const OUT = join(ROOT, '.data', 'ui-audit', 'font-compare')
mkdirSync(OUT, { recursive: true })

const FONTS = {
  inter: null, // 现状（index.css 里的 --font-sans）
  albert: '"Albert Sans", "Noto Sans SC", system-ui, sans-serif',
}

const browser = await chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL ?? 'chrome', headless: true })
const page = await browser.newPage({ viewport: { width: 1440, height: 1000 }, deviceScaleFactor: 2 })
await page.goto(BASE + '/', { waitUntil: 'networkidle' })
await page.waitForTimeout(800)

/** 两个裁切框：KPI 数字（大字/数字对齐）与表格行（12–14px 密集文本 + 中英混排） */
const CLIPS = {
  kpi: { x: 260, y: 118, width: 860, height: 120 },
  table: { x: 260, y: 400, width: 860, height: 280 },
}

for (const [name, stack] of Object.entries(FONTS)) {
  if (stack) {
    await page.evaluate((value) => {
      document.documentElement.style.setProperty('--font-sans', value)
    }, stack)
    await page.waitForTimeout(400)
  }
  for (const [clipName, clip] of Object.entries(CLIPS)) {
    await page.screenshot({ path: join(OUT, `${name}-${clipName}.png`), clip, animations: 'disabled' })
  }
}

// 合成一张竖排对比图：纯 block 布局，图片宽度吃满容器，不做 flex 计算
const W = 880
const html = `<!doctype html><meta charset="utf-8">
<style>
  * { box-sizing: border-box; }
  body { margin:0; background:#0b1220; color:#e2e8f0;
         font:13px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
  .wrap { width:${W}px; padding:18px 22px 22px; }
  h1 { font-size:15px; margin:0 0 4px; font-weight:600; }
  p  { margin:0 0 14px; color:#94a3b8; font-size:11.5px; }
  h2 { font-size:12px; margin:14px 0 6px; color:#e2e8f0; font-weight:600; }
  h2 span { color:#94a3b8; font-weight:400; }
  img { display:block; width:100%; height:auto; border-radius:6px; background:#fff; }
  .gap { height:6px; }
</style>
<div class="wrap">
  <h1>TestPlane 字体对比 · 同一页面 / 同一裁切框，只换 --font-sans 首顺位</h1>
  <p>上：Inter（shadcn 默认，为 UI 小字号设计）；下：Albert Sans（改前，几何无衬线）。
     每行第一张是 KPI 大字（数字 + 中文标签），第二张是 Run 表行（12–14px 密集文本、中英混排）。设备像素比 2×。</p>

  <h2>Inter <span>· 改后</span></h2>
  <img src="inter-kpi.png"><div class="gap"></div><img src="inter-table.png">

  <h2>Albert Sans <span>· 改前</span></h2>
  <img src="albert-kpi.png"><div class="gap"></div><img src="albert-table.png">
</div>`

const htmlPath = join(OUT, 'compare.html')
writeFileSync(htmlPath, html, 'utf8')
const composer = await browser.newPage({ viewport: { width: W, height: 700 }, deviceScaleFactor: 2 })
await composer.goto('file:///' + htmlPath.replace(/\\/g, '/'))
await composer.waitForTimeout(300)
await composer.screenshot({ path: join(OUT, 'font-compare.png'), fullPage: true })

await browser.close()
console.log('font-compare ->', join('.data', 'ui-audit', 'font-compare', 'font-compare.png'))
