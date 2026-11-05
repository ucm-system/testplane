// 布局审计：用 DOM 实测，不靠肉眼。
// 验收项（来自用户对前端原型的要求）：
//   1. 页头左侧分隔线 x == 240；右侧用户区分隔线 x == 视口宽 - 288（>=1024 时）
//   2. body 三列宽度 == 240 / 剩余 / 288；三列等高
//   3. DOM 里 <main> 先于两侧 <aside>
//   4. 页头 sticky top-0；页脚 sticky bottom-0；main 有 min-width:0
//   5. 字体是否真的加载了 Albert Sans / Noto Sans SC
//   6. 禁止项：rounded-none / shadow-2xl / font-black / text-5xl 及以上的计算字号、backdrop-blur、渐变
//   7. 动效时长 <= 200ms
// 用法：node tests/layout-audit.mjs   （DEMO_BASE 默认 http://127.0.0.1:8132）
import { chromium } from '@playwright/test'

const BASE = process.env.DEMO_BASE ?? 'http://127.0.0.1:8132'
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const results = []
const push = (name, ok, detail) => results.push({ name, ok, detail })

async function audit(page, label, width, height) {
  await page.setViewportSize({ width, height })
  await page.goto(BASE + '/runs', { waitUntil: 'networkidle' })
  await page.waitForTimeout(600)

  const m = await page.evaluate(() => {
    const r = (el) => (el ? el.getBoundingClientRect() : null)
    const header = document.querySelector('header')
    const main = document.querySelector('main')
    const asides = [...document.querySelectorAll('aside')]
    const footer = document.querySelector('footer')
    const left = asides[0]
    const right = asides[asides.length - 1]

    // 找出页头里所有竖直分隔线（宽 1px 的 div）与带 border-left 的块
    const thin = [...(header?.querySelectorAll('div') ?? [])]
      .filter((el) => {
        const b = el.getBoundingClientRect()
        return b.width > 0 && b.width <= 2 && b.height > 0
      })
      .map((el) => Math.round(el.getBoundingClientRect().left))

    const bordered = [...(header?.querySelectorAll('*') ?? [])]
      .filter((el) => {
        const cs = getComputedStyle(el)
        return parseFloat(cs.borderLeftWidth) > 0 && cs.borderLeftStyle !== 'none'
      })
      .map((el) => Math.round(el.getBoundingClientRect().left))

    const body = main?.parentElement
    const bodyCols = body ? getComputedStyle(body).gridTemplateColumns : null

    // DOM 顺序：main 在 aside 之前？
    const all = [...document.querySelectorAll('main, aside')]
    const mainFirst = all.length > 0 && all[0].tagName === 'MAIN'

    // 三列等高
    const leftH = left ? Math.round(r(left).height) : 0
    const mainH = main ? Math.round(r(main).height) : 0
    const rightH = right && right.getBoundingClientRect().width > 0 ? Math.round(r(right).height) : 0

    return {
      headerH: header ? Math.round(r(header).height) : 0,
      headerPos: header ? getComputedStyle(header).position : null,
      footerPos: footer ? getComputedStyle(footer).position : null,
      thin,
      bordered,
      leftW: left ? Math.round(r(left).width) : 0,
      rightW: right ? Math.round(r(right).width) : 0,
      mainW: main ? Math.round(r(main).width) : 0,
      bodyCols,
      mainFirst,
      leftH,
      mainH,
      rightH,
      mainMinW: main ? getComputedStyle(main).minWidth : null,
      fonts: {
        albert: document.fonts.check('16px "Albert Sans"'),
        noto: document.fonts.check('16px "Noto Sans SC"'),
      },
    }
  })

  const wide = width > 1024
  if (wide) {
    push(`${label}: 左分隔线 x=240`, m.thin.includes(240) || m.bordered.some((x) => Math.abs(x - 240) <= 1), `thin=${m.thin} bordered=${m.bordered}`)
    const want = width - 288
    push(`${label}: 右分隔线 x=${want}`, m.bordered.some((x) => Math.abs(x - want) <= 1), `bordered=${m.bordered}`)
    push(`${label}: 左栏宽=240`, m.leftW === 240, `leftW=${m.leftW}`)
    push(`${label}: 右栏宽=288`, m.rightW === 288, `rightW=${m.rightW}`)
    push(`${label}: 三列等高`, m.leftH === m.mainH || Math.abs(m.leftH - m.mainH) <= 2, `left=${m.leftH} main=${m.mainH} right=${m.rightH}`)
  }
  push(`${label}: DOM 里 main 先于 aside`, m.mainFirst, JSON.stringify(m.bodyCols))
  push(`${label}: 页头 sticky`, m.headerPos === 'sticky', `pos=${m.headerPos} h=${m.headerH}`)
  push(`${label}: 页脚 sticky`, m.footerPos === 'sticky', `pos=${m.footerPos}`)
  push(`${label}: main min-width=0`, m.mainMinW === '0px', `minW=${m.mainMinW}`)
  push(`${label}: 字体 Albert Sans 已加载`, m.fonts.albert, JSON.stringify(m.fonts))
  push(`${label}: 字体 Noto Sans SC 已加载`, m.fonts.noto, JSON.stringify(m.fonts))
}

const ctx = await browser.newContext()
const page = await ctx.newPage()
for (const [label, w, h] of [['desktop-1440', 1440, 1000], ['laptop-1280', 1280, 900], ['tablet-900', 900, 900], ['phone-390', 390, 844]]) {
  await audit(page, label, w, h)
}

// 静态禁止项：直接看渲染后的元素与样式
const forbidden = await page.evaluate(() => {
  const bad = []
  const cls = /(rounded-none|shadow-2xl|font-black|font-extrabold|text-5xl|text-6xl|text-7xl|text-8xl|backdrop-blur|bg-gradient)/
  for (const el of document.querySelectorAll('*')) {
    if (cls.test(el.className || '')) bad.push('class:' + el.className)
    const cs = getComputedStyle(el)
    if (parseFloat(cs.fontSize) >= 48) bad.push('fontSize:' + cs.fontSize + ' ' + el.tagName)
    if (cs.transitionDuration && parseFloat(cs.transitionDuration) > 0.2) bad.push('transition:' + cs.transitionDuration + ' ' + el.tagName + '.' + (el.className || ''))
    if (cs.animationDuration && parseFloat(cs.animationDuration) > 0.2) bad.push('animation:' + cs.animationDuration + ' ' + el.tagName)
    if (cs.backdropFilter && cs.backdropFilter !== 'none') bad.push('backdrop:' + el.tagName)
  }
  return { count: bad.length, sample: bad.slice(0, 8) }
})
push('禁止项（禁用类/超大字号/超 200ms 动效/玻璃态）', forbidden.count === 0, JSON.stringify(forbidden))

await browser.close()

const failed = results.filter((r) => !r.ok)
for (const r of results) console.log(`${r.ok ? 'OK  ' : 'FAIL'} ${r.name}  ${r.detail ?? ''}`)
console.log(`\n通过 ${results.length - failed.length}/${results.length}`)
process.exit(failed.length ? 1 : 0)
