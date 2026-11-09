#!/usr/bin/env node
/**
 * UI 规范审计（可重复、可留档、可当门禁）
 * =============================================================================
 * 不是像素比对：像素比对只能告诉你"变了"，说不清"对不对"。这里做的是**规范断言**——
 * 把设计规格写成可执行的规则，逐条在真实渲染结果上量。
 *
 * 三条口径（票 #72 定的）：
 *   1. 排版：正文/次要/标题的字号只落在标尺上（11/12/14/16/18/20/24/30），
 *      禁止 13/15/17 这类"半档"字号；≥16px 的标题必须 tracking-tight（负字距）。
 *   2. 颜色：业务组件源码里**不得出现裸 hex / rgb( / oklch(**，状态色只走语义 token；
 *      抽样算文本对比度，正文 ≥ 4.5:1，大字（≥24px 或 ≥18.66px 粗体）≥ 3:1。
 *   3. 细节：可点元素 ≥32px 高（兜底 ≥24px）、焦点环存在且统一、圆角只出现在 token 派生值上、
 *      阴影只有两级（shadow-xs / shadow-overlay）、间距 4 的倍数。
 *   4. 截图：概览/列表/详情/报告/提交/Tester/集群/管理台 × 1440/1024/390，全页存盘。
 *
 * 严重级别：
 *   P0 = 规范破坏（裸色、半档字号、对比度不足、无焦点环、圆角/阴影越界、可点元素 <24px）
 *        → **有 P0 就退出码 1**，报告里逐条带 `文件:行号`。
 *   P1 = 应该修但不阻断（可点元素 <32px、标题没 tracking-tight、间距不在 4 的栅格上）
 *   P2 = 提示（信息性统计、抽样说明）
 *
 * 用法：
 *   PLAYWRIGHT_BASE_URL=http://127.0.0.1:5173 node tests/ui-audit.mjs
 *   UI_AUDIT_PHASE=before node tests/ui-audit.mjs      # 改前基线：截图进 before/，报告写 ui-audit-report-before.md
 *   UI_AUDIT_PHASE=after  node tests/ui-audit.mjs      # 改后复核：截图进 after/，报告写 ui-audit-report.md
 *
 * 默认打 mock（dev server 用 VITE_USE_MOCK=1 起）；打真实例就把 PLAYWRIGHT_BASE_URL 指过去。
 * 依赖只有 @playwright/test（浏览器用系统 Chrome，不额外下载）。
 */
import { chromium } from '@playwright/test'
import { mkdirSync, readFileSync, readdirSync, statSync, writeFileSync, existsSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

/* ------------------------------------------------------------------ 配置 */

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..') // web/
const BASE = (process.env.PLAYWRIGHT_BASE_URL ?? 'http://127.0.0.1:5173').replace(/\/$/, '')
const PHASE = process.env.UI_AUDIT_PHASE === 'before' ? 'before' : 'after'
const OUT_DIR = join(ROOT, '.data', 'ui-audit', PHASE)
const REPORT = join(
  ROOT,
  '.data',
  'ui-audit',
  PHASE === 'before' ? 'ui-audit-report-before.md' : 'ui-audit-report.md',
)

/** 设计标尺（token 派生，见 src/index.css 的 --text-*）。11 只给 2xs 元信息。 */
const FONT_SCALE = [11, 12, 14, 16, 18, 20, 24, 30]
/** ≥30px 视为展示级（404 插画那种大数字），不参与半档判定。 */
const DISPLAY_MIN = 30
/** 业务代码里禁止出现的颜色字面量（语义 token 之外的一切）。 */
const COLOR_LITERAL = /(#[0-9a-fA-F]{3,8}\b|\brgba?\(|\boklch\()/
/** 允许例外：品牌 SVG 插画需要真实颜色；CodeMirror 主题在 JS 里必须给字面色。 */
const COLOR_ALLOW_FILES = new Set([
  'src/components/app/Wordmark.tsx',
  'src/components/ui/yaml-editor.tsx',
])

const VIEWPORTS = [
  { tag: '1440', width: 1440, height: 900 },
  { tag: '1024', width: 1024, height: 768 },
  { tag: '390', width: 390, height: 844 },
]

/* ------------------------------------------------------------------ 结果收集 */

const findings = [] // { severity, rule, where, detail }
const add = (severity, rule, where, detail) => findings.push({ severity, rule, where, detail })
const shots = [] // { page, viewport, file }

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    const st = statSync(full)
    if (st.isDirectory()) walk(full, out)
    else out.push(full)
  }
  return out
}

function rel(abs) {
  return abs.slice(ROOT.length + 1).replace(/\\/g, '/')
}

/* ================================================================== ① 静态检查 */

function staticChecks() {
  const files = [...walk(join(ROOT, 'src', 'components')), ...walk(join(ROOT, 'src', 'pages'))].filter(
    (f) => /\.(tsx|ts)$/.test(f),
  )

  for (const file of files) {
    const relPath = rel(file)
    const allowColor = COLOR_ALLOW_FILES.has(relPath)
    const lines = readFileSync(file, 'utf8').split(/\r?\n/)

    lines.forEach((line, index) => {
      const at = `${relPath}:${index + 1}`
      const trimmed = line.trim()

      // --- 颜色字面量 ---
      if (!allowColor && COLOR_LITERAL.test(line) && !trimmed.startsWith('//') && !trimmed.startsWith('*')) {
        add('P0', '颜色：业务组件出现裸色值', at, trimmed.slice(0, 120))
      }

      // --- 半档字号：text-[Npx] 里 N 不在标尺上且 < 30 ---
      const sizeHits = line.matchAll(/text-\[(\d+(?:\.\d+)?)px\]/g)
      for (const hit of sizeHits) {
        const size = Number(hit[1])
        if (size < DISPLAY_MIN && !FONT_SCALE.includes(size)) {
          add('P0', '排版：出现半档字号', at, `${hit[0]}（标尺 ${FONT_SCALE.join('/')}）`)
        }
      }
    })
  }

  // --- index.css 的 @theme 里不允许 hex（tokens 必须是 oklch 或 var 别名）---
  const css = readFileSync(join(ROOT, 'src', 'index.css'), 'utf8')
  const cssLines = css.split(/\r?\n/)
  const themeStartLine = cssLines.findIndex((line) => line.startsWith('@theme'))
  const themeEndLine = cssLines.findIndex((line, i) => i > themeStartLine && /^}/.test(line))
  for (let i = Math.max(0, themeStartLine); i < (themeEndLine === -1 ? cssLines.length : themeEndLine); i += 1) {
    if (/--color-[\w-]+:\s*#[0-9a-fA-F]{3,8}/.test(cssLines[i])) {
      add('P0', '颜色：@theme 里还有 hex 色值', `src/index.css:${i + 1}`, cssLines[i].trim())
    }
  }
  if (!/--radius:\s*0?\.625rem/.test(css)) {
    add('P1', '细节：--radius 不是 shadcn 的 0.625rem 基准', 'src/index.css', '找不到 `--radius: 0.625rem`')
  }
}

/* ================================================================== ② 运行时检查 */

/**
 * 度量一页：排版 / 颜色 / 细节。
 * 注意：`page.evaluate(fn)` 是**序列化函数体**执行的，闭包里的变量拿不到，
 * 所以颜色归一化 / 亮度 / 对比度这些工具必须整段写在函数体内部。
 */
function measurePage() {
  const canvas = document.createElement('canvas')
  const ctx = canvas.getContext('2d', { willReadFrequently: true })
  const colorCache = new Map()

  function toRgb(input) {
    if (!input) return null
    if (colorCache.has(input)) return colorCache.get(input)
    let out = null
    const probe = input.trim()
    if (probe === 'transparent' || probe === 'rgba(0, 0, 0, 0)' || probe === '') {
      out = { r: 0, g: 0, b: 0, a: 0 }
    } else {
      ctx.clearRect(0, 0, 1, 1)
      ctx.fillStyle = '#000'
      ctx.fillStyle = probe
      ctx.fillRect(0, 0, 1, 1)
      const data = ctx.getImageData(0, 0, 1, 1).data
      out = { r: data[0], g: data[1], b: data[2], a: data[3] / 255 }
    }
    colorCache.set(input, out)
    return out
  }

  function luminance({ r, g, b }) {
    const f = (v) => {
      const x = v / 255
      return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4
    }
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
  }

  const blend = (top, bottom) => ({
    r: Math.round(top.r * top.a + bottom.r * (1 - top.a)),
    g: Math.round(top.g * top.a + bottom.g * (1 - top.a)),
    b: Math.round(top.b * top.a + bottom.b * (1 - top.a)),
    a: 1,
  })

  const ratio = (a, b) => {
    const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
    return (hi + 0.05) / (lo + 0.05)
  }

  const selectorOf = (el) => {
    const parts = []
    let node = el
    for (let i = 0; i < 3 && node && node.nodeType === 1; i += 1) {
      let part = node.tagName.toLowerCase()
      if (node.id) {
        part += `#${node.id}`
        parts.unshift(part)
        break
      }
      const cls = (node.getAttribute('class') || '').split(/\s+/).filter(Boolean).slice(0, 2)
      if (cls.length) part += '.' + cls.join('.')
      parts.unshift(part)
      node = node.parentElement
    }
    return parts.join(' > ')
  }

  const visible = (el) => {
    const rect = el.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return false
    const cs = getComputedStyle(el)
    if (cs.visibility === 'hidden' || cs.display === 'none') return false
    if (Number(cs.opacity) === 0) return false
    return true
  }

  const bgOf = (el) => {
    let node = el
    let acc = null
    while (node && node.nodeType === 1) {
      const cs = getComputedStyle(node)
      const c = toRgb(cs.backgroundColor)
      if (c && c.a > 0) {
        acc = acc ? blend(acc, c) : c
        if (c.a >= 1) return acc
      }
      node = node.parentElement
    }
    return acc ?? { r: 255, g: 255, b: 255, a: 1 }
  }

  const result = {
    fontSizes: [],
    headings: [],
    contrast: [],
    clickables: [],
    radii: [],
    shadows: [],
    spacings: [],
    fonts: {
      inter: document.fonts.check('16px Inter'),
      noto: document.fonts.check('16px "Noto Sans SC"'),
    },
    tokens: {},
  }

  const rootStyle = getComputedStyle(document.documentElement)
  const px = (name, fallback) => {
    const raw = rootStyle.getPropertyValue(name).trim()
    if (!raw) return fallback
    if (raw.endsWith('rem')) return parseFloat(raw) * 16
    if (raw.endsWith('px')) return parseFloat(raw)
    return fallback
  }
  result.tokens = {
    radius: {
      xs: px('--radius-xs', 2),
      sm: px('--radius-sm', 6),
      md: px('--radius-md', 8),
      lg: px('--radius-lg', 10),
      xl: px('--radius-xl', 14),
      '2xl': px('--radius-2xl', 18),
      '3xl': px('--radius-3xl', 22),
      '4xl': px('--radius-4xl', 26),
    },
    shadow: {
      xs: getComputedStyle(document.documentElement).getPropertyValue('--shadow-xs').trim(),
      overlay: getComputedStyle(document.documentElement).getPropertyValue('--shadow-overlay').trim(),
    },
  }

  const elements = [...document.querySelectorAll('body *')]

  for (const el of elements) {
    if (!visible(el)) continue
    const cs = getComputedStyle(el)
    const tag = el.tagName.toLowerCase()
    const sel = selectorOf(el)
    const fontSize = parseFloat(cs.fontSize)
    const rect0 = el.getBoundingClientRect()
    // sr-only（1×1 的读屏文案）不是"可见文本"，跳过：它靠 clip 藏起来，不代表视觉规格
    const srOnly = rect0.width <= 1 && rect0.height <= 1

    /* ---- 排版：字号标尺 ---- */
    const hasText = [...el.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim().length > 0)
    // SVG 里的 <text> 是品牌字标（Wordmark），不是 UI 文本，不参与字号标尺
    const inSvg = Boolean(el.closest('svg'))
    if (hasText && fontSize > 0 && !inSvg && !srOnly) {
      result.fontSizes.push({
        size: Math.round(fontSize * 100) / 100,
        weight: cs.fontWeight,
        tag,
        sel,
        text: el.textContent.trim().slice(0, 40),
      })
    }

    /* ---- 排版：标题 tracking ---- */
    if (/^h[1-6]$/.test(tag) && fontSize >= 16) {
      result.headings.push({
        tag,
        size: fontSize,
        letterSpacing: cs.letterSpacing === 'normal' ? 0 : parseFloat(cs.letterSpacing),
        text: el.textContent.trim().slice(0, 40),
        sel,
      })
    }

    /* ---- 颜色：文本对比度 ---- */
    if (hasText && !el.closest('[aria-hidden="true"]') && !el.closest('svg')) {
      const fgRaw = toRgb(cs.color)
      const bg = bgOf(el)
      if (fgRaw) {
        const fg = fgRaw.a < 1 ? blend(fgRaw, bg) : fgRaw
        const bold = Number(cs.fontWeight) >= 700
        const large = fontSize >= 24 || (bold && fontSize >= 18.66)
        const disabled =
          el.closest('[disabled]') || el.closest('[aria-disabled="true"]') || cs.opacity === '0.5'
        result.contrast.push({
          ratio: ratio(fg, bg),
          need: large ? 3 : 4.5,
          size: fontSize,
          weight: cs.fontWeight,
          disabled: Boolean(disabled),
          text: el.textContent.trim().slice(0, 36),
          sel,
          fg: cs.color,
          bg: `rgb(${bg.r} ${bg.g} ${bg.b})`,
        })
      }
    }

    /* ---- 细节：可点元素尺寸 ----
       只量"按钮型"目标：<a> 只有在看起来像按钮（有底色或描边、或块级）时才算，
       正文里的行内链接不算 32px 目标（WCAG 2.5.8 也是按 target 算，不是按文字链接算）。 */
    const role = el.getAttribute('role')
    const linkLooksLikeButton =
      tag === 'a' &&
      el.hasAttribute('href') &&
      (cs.display === 'flex' ||
        cs.display === 'grid' ||
        cs.display === 'block' ||
        cs.backgroundColor !== 'rgba(0, 0, 0, 0)' ||
        parseFloat(cs.borderTopWidth) > 0)
    // 复选框/单选框若被 <label> 包着，真正的命中目标是 label（点文字也能勾），不按 24px 要求控件本身
    const wrappedChoice =
      tag === 'input' &&
      ['checkbox', 'radio'].includes(el.getAttribute('type') || '') &&
      Boolean(el.closest('label'))
    const clickable =
      tag === 'button' ||
      tag === 'select' ||
      tag === 'summary' ||
      linkLooksLikeButton ||
      (tag === 'input' &&
        ['checkbox', 'radio', 'button', 'submit'].includes(el.getAttribute('type') || '') &&
        !wrappedChoice) ||
      ['button', 'tab', 'option', 'menuitem', 'switch'].includes(role || '')
    if (clickable && !srOnly && !inSvg) {
      const rect = el.getBoundingClientRect()
      result.clickables.push({
        h: Math.round(rect.height * 10) / 10,
        w: Math.round(rect.width * 10) / 10,
        tag,
        sel,
        text: (el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 32),
      })
    }

    /* ---- 细节：圆角 ---- */
    const radius = parseFloat(cs.borderTopLeftRadius)
    const hasSurface = cs.backgroundColor !== 'rgba(0, 0, 0, 0)' || parseFloat(cs.borderTopWidth) > 0
    if (hasSurface && radius > 0) {
      result.radii.push({ radius: Math.round(radius * 10) / 10, tag, sel })
    }

    /* ---- 细节：阴影 ---- */
    if (cs.boxShadow && cs.boxShadow !== 'none') {
      result.shadows.push({ value: cs.boxShadow, tag, sel })
    }

    /* ---- 细节：间距栅格 ---- */
    const boxes = ['paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'rowGap', 'columnGap']
    for (const key of boxes) {
      const value = parseFloat(cs[key])
      if (Number.isFinite(value) && value > 0 && value % 4 !== 0) {
        result.spacings.push({ value, prop: key, tag, sel })
      }
    }
  }

  return result
}

/* ================================================================== ③ 执行 */

function parseShadow(value) {
  // 把 box-shadow 拆成若干层，只保留几何签名（x y blur spread），无视颜色写法差异。
  // 两个坑：
  //  1. Tailwind 会把 --tw-shadow 拼成 5 层占位（前几层是全透明的 0 0 0 0），必须丢掉；
  //  2. 颜色函数里的数字（oklch/rgba）会污染数字提取，先整段剥掉颜色再说。
  const TRANSPARENT = /(rgba\(\s*0\s*,\s*0\s*,\s*0\s*,\s*0\s*\)|rgb\(\s*0\s+0\s+0\s*\/\s*0\s*\))/i
  const COLOR_FN = /(?:oklch|rgba?|hsla?|color-mix|lab|lch)\([^()]*(?:\([^()]*\)[^()]*)*\)/gi
  const layers = []
  let depth = 0
  let current = ''
  for (const ch of value) {
    if (ch === '(') depth += 1
    if (ch === ')') depth -= 1
    if (ch === ',' && depth === 0) {
      layers.push(current)
      current = ''
    } else current += ch
  }
  if (current.trim()) layers.push(current)
  return layers
    .filter((layer) => !TRANSPARENT.test(layer))
    .map((layer) => {
      const geometry = layer.replace(COLOR_FN, ' ')
      // 偏移量可以不带单位（`0 1px 2px 0`），所以 px 后缀可选
      const nums = geometry.match(/-?\d+(?:\.\d+)?(?:px)?/g) || []
      if (nums.length < 3) return null
      const [x, y, blur, spread = '0px'] = nums.slice(0, 4).map((n) => Math.round(parseFloat(n) * 10) / 10)
      return `${x} ${y} ${blur} ${spread}`
    })
    .filter(Boolean)
}

async function main() {
  mkdirSync(OUT_DIR, { recursive: true })

  /* ---------- ① 静态检查（不依赖浏览器） ---------- */
  staticChecks()

  const browser = await chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL ?? 'chrome', headless: true })
  const context = await browser.newContext()
  const page = await context.newPage()
  page.setDefaultTimeout(30_000)

  const consoleErrors = []
  page.on('pageerror', (e) => consoleErrors.push(String(e)))

  const goto = async (path) => {
    await page.goto(BASE + path, { waitUntil: 'networkidle', timeout: 30_000 }).catch(() => {})
    await page.waitForTimeout(400)
  }

  /* ---------- 找出可用的 Run（详情/报告页要真 ID） ---------- */
  await goto('/runs')
  const runId = await page.evaluate(() => {
    const link = [...document.querySelectorAll('a[href^="/runs/"]')].find(
      (a) => /^\/runs\/[^/]+$/.test(a.getAttribute('href') || ''),
    )
    return link ? (link.getAttribute('href') || '').split('/')[2] : ''
  })

  const PAGES = [
    { name: '概览', path: '/' },
    { name: '列表', path: '/runs' },
    { name: '详情', path: runId ? `/runs/${runId}` : '/' },
    { name: '报告', path: runId ? `/runs/${runId}/report` : '/' },
    { name: '提交', path: '/submit' },
    { name: 'Tester', path: '/testers' },
    { name: '集群', path: '/clusters' },
    { name: '管理台', path: '/admin' },
  ]

  /* ---------- ② 逐页 × 逐视口：截图 + 度量 ---------- */
  const measured = []
  for (const viewport of VIEWPORTS) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    for (const entry of PAGES) {
      await goto(entry.path)
      const shotName = `${entry.name}-${viewport.tag}.png`
      await page
        .screenshot({ path: join(OUT_DIR, shotName), fullPage: true, animations: 'disabled', caret: 'hide' })
        .catch(() => {})
      shots.push({ page: entry.name, viewport: viewport.tag, file: join(PHASE, shotName) })

      // 度量只在主视口做，避免同一问题被三档重复计数
      if (viewport.tag === '1440') {
        const data = await page.evaluate(measurePage)
        measured.push({ page: entry.name, viewport: viewport.tag, data })
      }
    }
  }

  /* ---------- ③ 键盘焦点环（1440，概览页 Tab 前 14 个落点） ---------- */
  await page.setViewportSize({ width: 1440, height: 900 })
  await goto('/')
  await page.evaluate(() => document.body.focus())
  const focusStops = []
  for (let i = 0; i < 14; i += 1) {
    await page.keyboard.press('Tab')
    const stop = await page.evaluate(() => {
      const el = document.activeElement
      if (!el || el === document.body) return null
      const cs = getComputedStyle(el)
      const outline =
        parseFloat(cs.outlineWidth) > 0 && cs.outlineStyle !== 'none'
          ? `${cs.outlineWidth} ${cs.outlineStyle} ${cs.outlineColor}`
          : ''
      return {
        tag: el.tagName.toLowerCase(),
        text: (el.getAttribute('aria-label') || el.textContent || '').trim().slice(0, 32),
        outline,
        shadow: cs.boxShadow,
        borderColor: cs.borderTopColor,
      }
    })
    if (!stop) continue
    focusStops.push(stop)
    const hasRing = stop.outline !== '' || (stop.shadow && stop.shadow !== 'none')
    if (!hasRing) {
      add('P0', '细节：焦点落点没有可视指示', `焦点第 ${i + 1} 站 <${stop.tag}>「${stop.text}」`, 'outline 与 box-shadow 都为空')
    }
  }

  /* ---------- ④ 把度量结果折成结论 ---------- */
  // 允许的圆角 = :root 上真实存在的 token 派生值（不是写死的一张表），外加 0（直角）
  const radiusAllowed = new Set([0])
  for (const { data } of measured) {
    for (const value of Object.values(data.tokens.radius)) {
      if (Number.isFinite(value)) radiusAllowed.add(Math.round(value))
    }
  }

  for (const { page: pageName, data } of measured) {
    // 字体
    if (!data.fonts.inter) {
      add('P0', '排版：Inter 没有加载', `${pageName}`, JSON.stringify(data.fonts))
    }
    if (!data.fonts.noto) {
      add('P0', '排版：Noto Sans SC 没有加载', `${pageName}`, JSON.stringify(data.fonts))
    }

    // 字号标尺
    const seenSizes = new Map()
    for (const item of data.fontSizes) {
      if (item.size >= DISPLAY_MIN) continue
      if (FONT_SCALE.includes(item.size)) continue
      const key = `${item.size}`
      if (!seenSizes.has(key)) seenSizes.set(key, item)
    }
    for (const [size, item] of seenSizes) {
      add('P0', '排版：渲染结果里有半档字号', `${pageName} · ${item.sel}`, `${size}px「${item.text}」`)
    }

    // 标题 tracking
    for (const heading of data.headings) {
      if (heading.letterSpacing >= 0) {
        add('P1', '排版：标题没有 tracking-tight', `${pageName} · ${heading.sel}`, `${heading.size}px「${heading.text}」letter-spacing=${heading.letterSpacing}`)
      }
    }

    // 对比度
    const worst = new Map()
    for (const item of data.contrast) {
      if (item.disabled) continue
      if (item.ratio + 0.001 >= item.need) continue
      const key = `${item.size}/${item.fg}`
      if (!worst.has(key) || worst.get(key).ratio > item.ratio) worst.set(key, item)
    }
    for (const [, item] of worst) {
      add(
        'P0',
        '颜色：文本对比度不足',
        `${pageName} · ${item.sel}`,
        `${item.ratio.toFixed(2)}:1 < ${item.need}:1 · ${item.size}px/${item.weight} · fg ${item.fg} on ${item.bg} ·「${item.text}」`,
      )
    }

    // 可点元素
    const smallTargets = new Map()
    for (const item of data.clickables) {
      if (item.h >= 32) continue
      const key = `${item.h}/${item.sel}`
      if (!smallTargets.has(key)) smallTargets.set(key, item)
    }
    for (const [, item] of smallTargets) {
      const severity = item.h < 24 ? 'P0' : 'P1'
      const rule =
        severity === 'P0' ? '细节：可点元素 <24px 高（WCAG 2.5.8 下限）' : '细节：可点元素不足 32px 高'
      add(severity, rule, `${pageName} · ${item.sel}`, `${item.h}px ·「${item.text}」`)
    }

    // 圆角
    const badRadii = new Map()
    for (const item of data.radii) {
      if (item.radius >= 1000) continue // 胶囊
      if (radiusAllowed.has(Math.round(item.radius))) continue
      const key = `${item.radius}`
      if (!badRadii.has(key)) badRadii.set(key, item)
    }
    for (const [radius, item] of badRadii) {
      add('P0', '细节：圆角不在 token 派生值上', `${pageName} · ${item.sel}`, `${radius}px`)
    }

    // 阴影
    const allowedShadowSignatures = new Set()
    for (const token of [data.tokens.shadow.xs, data.tokens.shadow.overlay]) {
      if (!token) continue
      for (const sig of parseShadow(token)) allowedShadowSignatures.add(sig)
    }
    const badShadows = new Map()
    for (const item of data.shadows) {
      const sigs = parseShadow(item.value)
      const ok = sigs.length > 0 && sigs.every((sig) => allowedShadowSignatures.has(sig))
      if (!ok) {
        const key = sigs.join('|') || item.value
        if (!badShadows.has(key)) badShadows.set(key, item)
      }
    }
    for (const [sig, item] of badShadows) {
      add('P0', '细节：阴影不在两级之内', `${pageName} · ${item.sel}`, `[${sig}] ${item.value.slice(0, 80)}`)
    }

    // 间距栅格（P1：不阻断，但报告里要看得到趋势）
    const spacing = new Map()
    for (const item of data.spacings) {
      const key = `${item.prop}:${item.value}`
      spacing.set(key, (spacing.get(key) ?? 0) + 1)
    }
    for (const [key, count] of spacing) {
      add('P2', '细节：间距不在 4 的栅格上', `${pageName} · ${key}`, `${count} 处`)
    }
  }

  if (consoleErrors.length) {
    add('P1', '运行时：存在页面异常', 'console', consoleErrors.slice(0, 3).join(' | '))
  }
  if (!runId) {
    add('P2', '抽样：没有可用的 Run ID', '列表页', '详情/报告两张图退回概览页')
  }

  await browser.close()

  /* ---------- ⑤ 写报告 ---------- */
  const bySeverity = { P0: [], P1: [], P2: [] }
  for (const finding of findings) bySeverity[finding.severity].push(finding)

  const lines = []
  lines.push('# UI 规范审计报告')
  lines.push('')
  lines.push(`> 生成方式：\`node web/tests/ui-audit.mjs\`（阶段 **${PHASE}**）`)
  lines.push(`> 目标：${BASE} · 视口 ${VIEWPORTS.map((v) => v.tag).join(' / ')} · 页面 ${PAGES.length} 个`)
  lines.push(`> 截图：\`web/.data/ui-audit/${PHASE}/\` 共 ${shots.length} 张`)
  lines.push(
    PHASE === 'before'
      ? '> 这是**改前基线**：P0 允许存在，用于告诉精修"改哪些"。'
      : '> 改前基线见 `web/.data/ui-audit/ui-audit-report-before.md`（`UI_AUDIT_PHASE=before` 生成）。',
  )
  lines.push('')
  lines.push('## 结论')
  lines.push('')
  lines.push(`| 级别 | 条数 | 是否阻断 |`)
  lines.push(`|---|---|---|`)
  lines.push(`| P0 | ${bySeverity.P0.length} | **是**（退出码 1） |`)
  lines.push(`| P1 | ${bySeverity.P1.length} | 否 |`)
  lines.push(`| P2 | ${bySeverity.P2.length} | 否 |`)
  lines.push('')
  lines.push(`状态：${bySeverity.P0.length === 0 ? '✅ **全绿**（无 P0，可当门禁）' : '❌ **有 P0**（见下）'}`)
  lines.push('')

  const SECTION = {
    P0: '## P0 · 规范破坏（必须清零）',
    P1: '## P1 · 应该修',
    P2: '## P2 · 提示',
  }
  for (const severity of ['P0', 'P1', 'P2']) {
    const items = bySeverity[severity]
    if (!items.length) continue
    lines.push(SECTION[severity])
    lines.push('')
    const groups = new Map()
    for (const item of items) {
      if (!groups.has(item.rule)) groups.set(item.rule, [])
      groups.get(item.rule).push(item)
    }
    for (const [rule, group] of [...groups.entries()].sort((a, b) => b[1].length - a[1].length)) {
      lines.push(`### ${rule}（${group.length}）`)
      lines.push('')
      lines.push('| 位置 | 明细 |')
      lines.push('|---|---|')
      for (const item of group.slice(0, 60)) {
        lines.push(`| \`${item.where}\` | ${String(item.detail).replace(/\|/g, '\\|')} |`)
      }
      if (group.length > 60) lines.push(`| … | 其余 ${group.length - 60} 条同类 |`)
      lines.push('')
    }
  }

  lines.push('## 截图清单')
  lines.push('')
  lines.push('| 页面 | 视口 | 文件 |')
  lines.push('|---|---|---|')
  for (const shot of shots) lines.push(`| ${shot.page} | ${shot.viewport} | \`${shot.file}\` |`)
  lines.push('')
  lines.push('## 检查口径')
  lines.push('')
  lines.push(`- **字号标尺**：${FONT_SCALE.join(' / ')} px；≥${DISPLAY_MIN}px 视为展示级不判半档；`)
  lines.push('- **对比度**：正文 ≥ 4.5:1，大字（≥24px 或 ≥18.66px 粗体）≥ 3:1，禁用态豁免；')
  lines.push('- **可点元素**：≥32px 记 P1，<24px 记 P0（120/1024 视口同样量）；')
  lines.push('- **圆角**：只认 token 派生值（2/4/6/8/10/14/20/26）与胶囊；')
  lines.push('- **阴影**：只认 `--shadow-xs` 与 `--shadow-overlay` 两级的几何签名；')
  lines.push('- **间距**：4 的倍数，越界只记 P2（信息性，不阻断）；')
  lines.push('- **焦点环**：键盘 Tab 前 14 个落点逐个检查 outline / box-shadow 是否非空。')
  lines.push('')

  writeFileSync(REPORT, lines.join('\n'), 'utf8')
  writeFileSync(
    join(ROOT, '.data', 'ui-audit', `ui-audit-${PHASE}.json`),
    JSON.stringify({ base: BASE, phase: PHASE, findings, shots }, null, 2),
    'utf8',
  )

  const p0 = bySeverity.P0.length
  console.log(`\n[ui-audit:${PHASE}] 目标 ${BASE}`)
  console.log(`  P0=${p0}  P1=${bySeverity.P1.length}  P2=${bySeverity.P2.length}  截图=${shots.length}`)
  console.log(`  报告 ${rel(REPORT)}`)
  if (p0) {
    console.log('\n  P0 摘要：')
    for (const item of bySeverity.P0.slice(0, 12)) console.log(`   - [${item.rule}] ${item.where} · ${item.detail}`)
    if (p0 > 12) console.log(`   … 其余 ${p0 - 12} 条见报告`)
  }
  process.exit(p0 ? 1 : 0)
}

main().catch((error) => {
  console.error(error)
  process.exit(1)
})
