// 布局审计：用 DOM 实测，不靠肉眼。
// 验收项（shadcn dashboard-01 骨架改造后）：
//   1. 外壳是 `<SidebarProvider>` + `<AppSidebar variant="inset">` + `<SidebarInset>`：
//      侧栏默认 256（容器 256 / 内层 240）、右侧内容区 `vw-8`（inset 有 8px 页边距）；
//   2. 折叠后：`data-state=collapsed`、容器 64、内层 icon rail 48、主内容左边界 72；
//      折叠状态写 cookie，刷新后保持（组件自己读回来）；
//   3. 全站**仍然没有右栏**（aside[aria-label="辅助信息"] 恒不存在），`data-rail` 恒 off；
//   4. 页头 sticky、h-14（56px），没有竖分隔线（侧栏自成一列，不再靠页头里的 1px 线对齐）；
//   5. 全页只有一个 `<main>`，min-width:0，DOM 里侧栏先于主内容（shadcn 骨架的顺序）；
//   6. 移动端（390）：桌面侧栏不渲染、主内容 0..vw，点汉堡出 288 的 sheet 抽屉（可 Esc 关闭）；
//   7. 字体是否真的加载了 Albert Sans / Noto Sans SC；
//   8. 禁止项：rounded-none / shadow-2xl / font-black / text-5xl 及以上的计算字号、backdrop-blur、渐变；
//   9. 动效时长 <= 200ms。
// 用法：node tests/layout-audit.mjs   （DEMO_BASE 默认 http://127.0.0.1:8132）
import { chromium } from '@playwright/test'

const BASE = process.env.DEMO_BASE ?? 'http://127.0.0.1:8132'
const browser = await chromium.launch({ channel: 'chrome', headless: true })
const results = []
const push = (name, ok, detail) => results.push({ name, ok: Boolean(ok), detail })

/** 页头里有几条"整条竖线"（透明描边的小按钮不算）。 */
const HEADER_LINE_DETECTOR = () => {
  const header = document.querySelector('header')
  if (!header) return { thin: [], bordered: [] }
  const thin = [...header.querySelectorAll('div')]
    .filter((el) => {
      const b = el.getBoundingClientRect()
      return b.width > 0 && b.width <= 2 && b.height >= 40
    })
    .map((el) => Math.round(el.getBoundingClientRect().left))
  const bordered = [...header.querySelectorAll('*')]
    .filter((el) => {
      const cs = getComputedStyle(el)
      const b = el.getBoundingClientRect()
      const transparent = /rgba\(0, 0, 0, 0\)|transparent/.test(cs.borderLeftColor)
      return b.height >= 40 && !transparent && parseFloat(cs.borderLeftWidth) > 0 && cs.borderLeftStyle !== 'none'
    })
    .map((el) => Math.round(el.getBoundingClientRect().left))
  return { thin, bordered }
}

/** 量侧栏 / 内容区 / 页头的几何。 */
const MEASURE = () => {
  const r = (el) => (el ? el.getBoundingClientRect() : null)
  const round = (b) =>
    b ? { left: Math.round(b.left), right: Math.round(b.right), top: Math.round(b.top), bottom: Math.round(b.bottom), width: Math.round(b.width), height: Math.round(b.height) } : null
  const header = document.querySelector('header')
  const main = document.querySelector('main')
  const sidebarRoot = document.querySelector('[data-slot="sidebar"]')
  const sidebar = document.querySelector('[data-slot="sidebar-container"]')
  const inner = document.querySelector('[data-slot="sidebar-inner"]')
  const inset = document.querySelector('[data-slot="sidebar-inset"]')
  const footer = document.querySelector('footer')
  const sideBeforeMain = sidebarRoot && main ? Boolean(sidebarRoot.compareDocumentPosition(main) & Node.DOCUMENT_POSITION_FOLLOWING) : false

  return {
    header: round(r(header)),
    headerPos: header ? getComputedStyle(header).position : null,
    footerPos: footer ? getComputedStyle(footer).position : null,
    sidebar: round(r(sidebar)),
    sidebarInner: round(r(inner)),
    inset: round(r(inset)),
    main: round(r(main)),
    mainOverflowY: main ? getComputedStyle(main).overflowY : null,
    mainScrollTop: main ? main.scrollTop : null,
    mainScrollable: main ? main.scrollHeight > main.clientHeight + 1 : false,
    mainCount: document.querySelectorAll('main').length,
    sideBeforeMain,
    sidebarState: sidebarRoot ? sidebarRoot.getAttribute('data-state') : null,
    sidebarCollapsible: sidebarRoot ? sidebarRoot.getAttribute('data-collapsible') : null,
    sidebarVariant: sidebarRoot ? sidebarRoot.getAttribute('data-variant') : null,
    railAsides: document.querySelectorAll('aside[aria-label="辅助信息"]').length,
    navItems: [...document.querySelectorAll('nav[aria-label="主导航"] a')].map((a) => a.textContent?.trim() ?? ''),
    rail: document.querySelector('[data-rail]')?.getAttribute('data-rail'),
    vw: window.innerWidth,
    vh: window.innerHeight,
    mainMinW: main ? getComputedStyle(main).minWidth : null,
    cookie: document.cookie,
    fonts: {
      albert: document.fonts.check('16px "Albert Sans"'),
      noto: document.fonts.check('16px "Noto Sans SC"'),
    },
  }
}

async function audit(page, label, width, height) {
  await page.setViewportSize({ width, height })
  await page.goto(BASE + '/', { waitUntil: 'networkidle' })
  await page.waitForTimeout(600)

  const m = await page.evaluate(MEASURE)
  const lines = await page.evaluate(HEADER_LINE_DETECTOR)
  const desktop = width >= 769

  // ---- 外壳与 columns（D-62 之后仍然没有右栏）----
  push(`${label}: data-rail=off`, m.rail === 'off', `rail=${m.rail}`)
  push(`${label}: 没有右栏 aside`, m.railAsides === 0, `asides(辅助信息)=${m.railAsides}`)
  push(`${label}: 全页唯一 main`, m.mainCount === 1, `mains=${m.mainCount}`)
  if (desktop) {
    push(`${label}: DOM 里侧栏先于主内容`, m.sideBeforeMain, `sideBeforeMain=${m.sideBeforeMain}`)
  }
  // 页头固定、内容在 main 里滚：先滚 200px 再量一次页头位置
  const scrolled = await page.evaluate(() => {
    const el = document.querySelector('main')
    if (el) el.scrollTop = 200
    return el ? el.scrollTop : 0
  })
  const mAfterScroll = await page.evaluate(MEASURE)
  push(`${label}: 滚动发生在 main 内部`, m.mainOverflowY === 'auto' && (m.mainScrollable ? scrolled > 0 : true), `overflowY=${m.mainOverflowY} scrollTop=${scrolled}`)
  push(
    `${label}: 内容滚动后页头位置不变（不再「顶一下」）`,
    mAfterScroll.header?.top === m.header?.top && mAfterScroll.header?.height === m.header?.height,
    `top=${m.header?.top}→${mAfterScroll.header?.top}`
  )
  await page.evaluate(() => {
    const el = document.querySelector('main')
    if (el) el.scrollTop = 0
  })
  push(`${label}: 页头高=56（h-14）`, m.header?.height === 56, `h=${m.header?.height}`)
  push(`${label}: main min-width=0`, m.mainMinW === '0px', `minW=${m.mainMinW}`)
  push(`${label}: 页头与内容区等宽`, m.header?.width === m.main?.width, `header=${m.header?.width} main=${m.main?.width}`)

  if (desktop) {
    const gutter = m.vw - 8 // inset variant 的 8px 右边距
    push(`${label}: 侧栏右边界=256`, m.sidebar?.right === 256, `sidebar=${JSON.stringify(m.sidebar)}`)
    push(`${label}: 侧栏宽=256（容器）`, m.sidebar?.width === 256, `width=${m.sidebar?.width}`)
    push(`${label}: 侧栏内层宽=240（inset p-2）`, m.sidebarInner?.width === 240, `inner=${m.sidebarInner?.width}`)
    push(`${label}: 侧栏高=视口高`, m.sidebar?.height === m.vh, `sidebarH=${m.sidebar?.height} vh=${m.vh}`)
    push(`${label}: 侧栏默认展开`, m.sidebarState === 'expanded' && m.sidebarVariant === 'inset', `state=${m.sidebarState} variant=${m.sidebarVariant}`)
    push(`${label}: 主内容左边界=256`, m.main?.left === 256, `mainLeft=${m.main?.left}`)
    push(`${label}: 主内容右边界=视口-8`, m.main?.right === gutter, `mainRight=${m.main?.right} want=${gutter}`)
    push(`${label}: 主内容顶边=页头底边`, m.main?.top === m.header?.bottom, `mainTop=${m.main?.top} headerBottom=${m.header?.bottom}`)
    push(`${label}: 页头没有竖分隔线`, [...new Set([...lines.thin, ...lines.bordered])].length === 0, `lines=${JSON.stringify(lines)}`)
  } else {
    push(`${label}: 移动端不渲染桌面侧栏`, m.sidebar === null, `sidebar=${JSON.stringify(m.sidebar)}`)
    push(`${label}: 主内容左边界=0`, m.main?.left === 0, `mainLeft=${m.main?.left}`)
    push(`${label}: 主内容右边界=视口`, m.main?.right === m.vw, `mainRight=${m.main?.right} vw=${m.vw}`)
  }

  // ---- 合并后的概览：KPI 四卡与全量 Run 表同屏 ----
  const merged = await page.evaluate(() => {
    const kpi = document.querySelector('[aria-label="概览指标"]')
    const table = document.querySelector('[data-testid="data-table"]')
    const pager = document.querySelector('[data-testid="pagination"]')
    const queue = document.querySelector('[data-testid="queue-summary"]')
    return {
      kpiCount: kpi ? kpi.children.length : 0,
      hasTable: Boolean(table),
      hasPager: Boolean(pager),
      hasQueue: Boolean(queue),
      tableWidth: table ? Math.round(table.getBoundingClientRect().width) : 0,
      mainWidth: Math.round(document.querySelector('#main').getBoundingClientRect().width),
    }
  })
  push(`${label}: KPI 四卡存在`, merged.kpiCount === 4, `count=${merged.kpiCount}`)
  push(`${label}: 全量 Run 表与分页栏同屏`, merged.hasTable && merged.hasPager, `table=${merged.hasTable} pager=${merged.hasPager}`)
  push(`${label}: 队列摘要并进主内容`, merged.hasQueue, `hasQueue=${merged.hasQueue}`)
  if (desktop) {
    push(`${label}: 表格占满主内容宽度`, merged.tableWidth >= merged.mainWidth - 120, `table=${merged.tableWidth} main=${merged.mainWidth}`)
  }

  // ---- 折叠 / icon rail / cookie 持久（只在 1440 做一次交互）----
  if (width === 1440) {
    const expandedMainLeft = m.main?.left
    await page.keyboard.press('Control+b')
    await page.waitForTimeout(400)
    const c = await page.evaluate(MEASURE)
    push(`${label}: Ctrl+B → data-state=collapsed`, c.sidebarState === 'collapsed', `state=${c.sidebarState}`)
    push(`${label}: 折叠态 data-collapsible=icon`, c.sidebarCollapsible === 'icon', `collapsible=${c.sidebarCollapsible}`)
    push(`${label}: 折叠后侧栏宽=icon rail（48–56）`, c.sidebarInner?.width >= 48 && c.sidebarInner?.width <= 56, `inner=${c.sidebarInner?.width} container=${c.sidebar?.width}`)
    push(`${label}: 折叠后主内容左边界=72`, c.main?.left === 72, `mainLeft=${c.main?.left}`)
    push(`${label}: 主内容左边界随折叠变化`, c.main?.left !== expandedMainLeft, `expanded=${expandedMainLeft} collapsed=${c.main?.left}`)
    push(`${label}: 折叠状态写入 cookie`, /sidebar_state=false/.test(c.cookie), `cookie=${c.cookie}`)

    // 刷新后仍然折叠（组件从 cookie 读回初始值）
    await page.reload({ waitUntil: 'networkidle' })
    await page.waitForTimeout(500)
    const afterReload = await page.evaluate(MEASURE)
    push(`${label}: 刷新后保持折叠`, afterReload.sidebarState === 'collapsed' && afterReload.sidebarInner?.width === 48, `state=${afterReload.sidebarState} inner=${afterReload.sidebarInner?.width}`)

    await page.keyboard.press('Control+b')
    await page.waitForTimeout(400)
    const restored = await page.evaluate(MEASURE)
    push(`${label}: Ctrl+B 再按 → 恢复 256`, restored.sidebarState === 'expanded' && restored.sidebar?.width === 256, `state=${restored.sidebarState} width=${restored.sidebar?.width}`)
  }

  // ---- 移动端：汉堡 → sheet 抽屉 ----
  if (width === 390) {
    const trigger = page.getByRole('button', { name: '切换侧栏' })
    push(`${label}: 折叠按钮有 aria-label 且可见`, await trigger.isVisible(), `count=${await trigger.count()}`)
    await trigger.click()
    await page.waitForTimeout(450)
    const drawer = page.getByRole('dialog')
    const drawerBox = await drawer.boundingBox()
    push(`${label}: 点汉堡出抽屉`, await drawer.isVisible(), `visible=${await drawer.isVisible()}`)
    push(`${label}: 抽屉宽=288（18rem）`, Math.round(drawerBox?.width ?? 0) === 288, `w=${Math.round(drawerBox?.width ?? 0)}`)
    push(`${label}: 抽屉里有主导航`, (await drawer.locator('nav[aria-label="主导航"] a').count()) >= 3, `links=${await drawer.locator('nav[aria-label="主导航"] a').count()}`)
    await page.keyboard.press('Escape')
    await page.waitForTimeout(350)
    push(`${label}: 抽屉可 Esc 关闭`, (await page.getByRole('dialog').count()) === 0, `dialogs=${await page.getByRole('dialog').count()}`)
  }

  // ---- 字体与页脚 ----
  push(`${label}: 字体 Albert Sans 已加载`, m.fonts.albert, JSON.stringify(m.fonts))
  push(`${label}: 字体 Noto Sans SC 已加载`, m.fonts.noto, JSON.stringify(m.fonts))
  push(`${label}: 全局页脚已删除`, m.footerPos === null, `on=${m.footerPos}`)
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
