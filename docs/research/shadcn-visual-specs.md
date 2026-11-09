# shadcn/ui 的视觉规格（一手抄录）与 TestPlane 的差距

> wayfinder ticket **#72**（视觉精修与可重复 UI 审计）的研究产物。
> 目的：把"shadcn 原版那种精致度"翻译成**可核对的数值**，让 `web/src/components/ui/*` 的
> 手写组件有据可依，让 `web/tests/ui-audit.mjs` 有断言可写。
>
> 引用约定：
> - **规格** = shadcn 官方 registry 里的组件源码（`npx shadcn@latest view @shadcn/<name>` 拉回来的
>   `new-york-v4` 源文件）；**文档** = `https://ui.shadcn.com/docs/components/radix/<name>`；
>   **示例** = 同站 `code/apps/v4/registry/bases/radix/examples/<name>-example.tsx`。
> - 本文件所有类名都是**逐字抄的**，未改写、未凭记忆补全。快照落在
>   `web/.data/ui-audit/registry/`（`*.tsx` = registry 源文件，`docs/`、`examples/` = 抓回来的文档与示例）。
> - 本机只装了 Tailwind **v4**，所以下文一律用 v4 的类名语义（`shadow-xs` / `outline-hidden` /
>   `size-*` / `ring-[3px]`）。

---

## 0. 怎么取到的一手规格（可复现）

```bash
# ① 组件真实源码（不是文档里的片段）：CLI 直接吐 registry item 的 content
npx shadcn@latest view @shadcn/button          # → 打印 registry-item JSON，files[].content 就是源码

# ② 文档与示例 URL：CLI 给的地址，逐个真抓（radix base）
npx shadcn@latest docs button card table badge tabs dialog input popover \
                    pagination dropdown-menu select tooltip skeleton
#   docs      https://ui.shadcn.com/docs/components/radix/<name>
#   examples  https://ui.shadcn.com/code/apps/v4/registry/bases/radix/examples/<name>-example.tsx

# ③ 主题与 token：https://ui.shadcn.com/docs/theming
```

> **坑（实测）**：CLI 默认给的 `docs` 链接是 `base` 体系的
> （`/docs/components/base/button`、`/registry/bases/base/ui/button.tsx`）。base 体系的源码里
> 只有语义壳子（`cn-button`、`cn-card` 这种**在 CSS 里定义**的类名），拿不到具体数值。
> 我们要的是 `radix` 体系的 `new-york-v4` 源码——那里才是 `h-9 px-4 py-2 text-sm font-medium
> rounded-md shadow-xs` 这种字面量。所以本文的一切数值都以 `view @shadcn/<name>` 出来的
> `registry/new-york-v4/ui/<name>.tsx` 为准。

---

## 1. shadcn 的 token 层（`neutral` 基色，Tailwind v4）

### 1.1 `:root` 的 oklch 色阶（**文档**：<https://ui.shadcn.com/docs/theming>）

| token | 值 | 换算 sRGB¹ | 白底对比度¹ | 我们用在哪 |
|---|---|---|---|---|
| `--background` | `oklch(1 0 0)` | `#ffffff` | 1.00 | 页面底（**接近纯白**，不是灰蓝） |
| `--foreground` | `oklch(0.145 0 0)` | `#0a0a0a` | 19.79 | 正文/标题 |
| `--card` | `oklch(1 0 0)` | `#ffffff` | 1.00 | 卡片面 |
| `--card-foreground` | `oklch(0.145 0 0)` | `#0a0a0a` | 19.79 | 卡片内文字 |
| `--popover` | `oklch(1 0 0)` | `#ffffff` | 1.00 | 弹层 |
| `--popover-foreground` | `oklch(0.145 0 0)` | `#0a0a0a` | 19.79 | 弹层文字 |
| `--primary` | `oklch(0.205 0 0)` | `#171717` | 17.91 | 主按钮底（**中性近黑**，不是蓝） |
| `--primary-foreground` | `oklch(0.985 0 0)` | `#fafafa` | 1.04 | 主按钮字 |
| `--secondary` | `oklch(0.97 0 0)` | `#f5f5f5` | 1.09 | 次级按钮底 |
| `--secondary-foreground` | `oklch(0.205 0 0)` | `#171717` | 17.91 | 次级按钮字 |
| `--muted` | `oklch(0.97 0 0)` | `#f5f5f5` | 1.09 | 表头/弱底/骨架 |
| `--muted-foreground` | `oklch(0.556 0 0)` | `#737373` | **4.73** | 次要文字（**刚好过 AA**） |
| `--accent` | `oklch(0.97 0 0)` | `#f5f5f5` | 1.09 | hover 底 / 骨架 |
| `--accent-foreground` | `oklch(0.205 0 0)` | `#171717` | 17.91 | hover 上的文字 |
| `--destructive` | `oklch(0.577 0.245 27.325)` | `#e7000b` | 4.76 | 危险操作 |
| `--border` | `oklch(0.922 0 0)` | `#e5e5e5` | 1.26 | 描边/分隔 |
| `--input` | `oklch(0.922 0 0)` | `#e5e5e5` | 1.26 | 输入框描边 |
| `--ring` | `oklch(0.708 0 0)` | `#a1a1a1` | 2.59 | 焦点环（中性灰） |

¹ 换算用 OKLab→sRGB 反变换（Björn Ottosson 的公式）+ WCAG 2.x 相对亮度，脚本见
`web/.data/ui-audit/oklch-check.mjs`，可复跑。**注意 `muted-foreground` 只有 4.73:1**：
shadcn 的"次要灰"已经贴着 AA 线，我们不能再压暗一级（`#98a2b3` 只有 2.9:1，只能当装饰）。

**另外三组 token 也照抄了**，理由是"将来 `npx shadcn add` 出来的组件要能直接用"：

| 组 | 值 | 谁会用 |
|---|---|---|
| `sidebar` / `sidebar-foreground` / `sidebar-primary` / `sidebar-accent` / `sidebar-border` / `sidebar-ring` | `0.985` / `0.145` / `0.205` / `0.97` / `0.922` / `0.708`（同中性阶） | shadcn `Sidebar` 组件（它默认引用这一组） |
| `chart-1…5` | `0.646 0.222 41.116` / `0.6 0.118 184.704` / `0.398 0.07 227.392` / `0.828 0.189 84.429` / `0.769 0.188 70.08` | shadcn `Chart` |
| `destructive` / `primary-foreground` | 分别别名到我们的 `danger` / `primary-fg` | shadcn Button/Badge 的 `destructive`、`primary-foreground` |

> Tailwind v4 只输出"被用到"的 `@theme` 变量，所以这些别名在没被使用前不会出现在产物 CSS 里，
> 一旦有组件写了 `bg-sidebar` / `text-chart-1` 就会自动补上——不需要额外配置。

**这解释了"为什么我们看起来糙"的第一层**：我们的 `--color-canvas: #f1f5f9` 是灰蓝底、
`--color-surface: #fff`，整页像"蒙了一层蓝灰"；shadcn 的 `--background` 是**纯白**，
灰只出现在 `--muted`（表头、内嵌块）。底色一白，卡片 `border + shadow-sm` 的层次立刻出来。

### 1.2 `--radius` 派生规则（**文档**：<https://ui.shadcn.com/docs/theming#radius-scale>）

```css
:root { --radius: 0.625rem; }        /* 10px */

@theme inline {
  --radius-sm:  calc(var(--radius) * 0.6);   /* 6px  */
  --radius-md:  calc(var(--radius) * 0.8);   /* 8px  */
  --radius-lg:  var(--radius);               /* 10px */
  --radius-xl:  calc(var(--radius) * 1.4);   /* 14px */
  --radius-2xl: calc(var(--radius) * 1.8);   /* 18px */
  --radius-3xl: calc(var(--radius) * 2.2);   /* 22px */
  --radius-4xl: calc(var(--radius) * 2.6);   /* 26px */
}
```

组件实际用到的档位只有四档：`rounded-sm`(6) = 菜单项/分页链接、`rounded-md`(8) =
按钮/输入/下拉面板、`rounded-lg`(10) = 弹窗/页签轨道、`rounded-xl`(14) = 卡片（**卡片比按钮更圆**）。
另有 `rounded-xs`(4) 只出现在 dialog 关闭按钮的 `rounded-xs`。

> 票 #72 的验收写"圆角只出现在 4/6/8/12"。按 shadcn 的派生规则，四档是 **4/6/8/10/14**。
> 两者冲突时以**派生规则**为准（否则 `--radius` 就不是唯一真源了），审计里断言的是
> "只出现 token 派生出来的值"，见 `web/tests/ui-audit.mjs` §圆角。

### 1.3 阴影：shadcn 其实有 4 档，但语义很清楚

| 用在哪 | 类 | 说明 |
|---|---|---|
| 输入框、`outline` 按钮、select trigger | `shadow-xs` | 约 `0 1px 2px rgb(0 0 0 / .05)`，只有"贴纸浮起 1px"的量 |
| 卡片 | `shadow-sm` | `0 1px 3px rgb(0 0 0 / .1), 0 1px 2px -1px …` |
| popover / dropdown / select 面板 | `shadow-md` | 浮层 |
| dialog | `shadow-lg` | 模态 |

我们收敛成**两级**（票要的）：`shadow-xs`（静置面：卡片/输入/分段选中块）+
`shadow-overlay`（所有浮层：popover / dropdown / dialog / drawer）。

### 1.4 统一的焦点环（**规格**：button / badge / input / tabs / select 全都一致）

```
focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50
```

- 环宽 **3px**（`ring-[3px]`），**半透明**（`ring-ring/50`），**无 offset**；
- **不是**旧版文档里的 `ring-2 ring-ring/50 ring-offset-2`（那是 v3 时代的写法，
  现在的 registry 源码里已经没有 `ring-offset` 了）；
- 边缘色同步换成 `border-ring`，所以"描边 + 光晕"是一体的，不会出现"蓝框外面套一圈灰边"；
- dialog 的关闭按钮是唯一例外：`focus:ring-2 focus:ring-ring focus:ring-offset-2`（小圆点用 2px 环 + offset）。

### 1.5 字体的"精致感"来自三个默认值

1. **字号只有 4 档在跑**：`text-xs`(12) 徽标/说明、`text-sm`(14) 正文/按钮/表格、
   `text-base`(16) 输入框（移动端）、`text-lg`(18) 弹窗标题。**没有 13/15/17 这种半档**。
2. **`font-medium` 是"强调"的默认手段**（按钮、表头、徽标、页签全是 `font-medium`，
   而不是 `font-semibold`）；只有卡片标题、弹窗标题用 `font-semibold`。
3. **标题收字距**：`CardTitle` 自身只有 `leading-none font-semibold`，
   `tracking-tight` 来自 `cn-font-heading`（nova 体系的排版类）；
   我们对应落到"h1/h2/h3 + CardTitle 一律 `tracking-tight`"。

---

## 2. 组件对照表（我们实际用到的 13 个）

### 2.1 Button（`button`）

规格出处：`npx shadcn@latest view @shadcn/button` →
<https://ui.shadcn.com/docs/components/radix/button>（含 `Size` / `With Icon` / `Spinner` 三节）。
示例：<https://ui.shadcn.com/code/apps/v4/registry/bases/radix/examples/button-example.tsx>

| 属性 | shadcn 原版 | 我们改前（`web/src/components/ui/button.tsx`） | 结论 |
|---|---|---|---|
| 高度 | `default h-9`(36) / `sm h-8`(32) / `xs h-6`(24) / `lg h-10`(40) / `icon size-9` | `md h-9` / `sm h-[30px]` | sm 提到 **h-8** |
| 横向内边距 | `px-4`；带图标时 `has-[>svg]:px-3` | `px-3.5` / `px-2.5` | 改 `px-4` + `has-[>svg]:px-3` |
| 圆角 | `rounded-md`(8) | `rounded-md` | 保留 |
| 字号/tracking | `text-sm font-medium`(14/500) | `text-xs`(12) / `text-2xs`(11) | 改 **text-sm font-medium** |
| 间距 | `gap-2`；`sm` 是 `gap-1.5` | `gap-1.5` / `gap-1` | 改 `gap-2` |
| 主色 | `bg-primary text-primary-foreground hover:bg-primary/90` | 墨黑 `bg-ink` + `hover:shadow-pop`（hover 靠阴影） | hover 用**颜色**（`/90`），不靠阴影 |
| 描边按钮 | `border bg-background shadow-xs hover:bg-accent hover:text-accent-foreground` | `border-line bg-surface hover:bg-surface-2` | 保留语义别名，补 `shadow-xs` |
| 禁用 | `disabled:pointer-events-none disabled:opacity-50` | `disabled:opacity-45` + `cursor-not-allowed` | 改 **opacity-50** |
| 焦点 | `focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50` | `ring-[3px] ring-ring/60` | 统一到 `/50` + `border-ring` |
| 图标 | `[&_svg:not([class*='size-'])]:size-4`，配 `data-icon="inline-start|inline-end"` | 调用点各自 `className="size-3"` | 组件内兜底 16px，调用点用 `data-icon` |
| 动效 | `transition-all`（150ms 隐式） | `transition-[…] duration-150` | 保留 150ms |

**它怎么变精致的**：hover **只动颜色**（`/90`）不动阴影、不动尺寸；禁用只降透明度不改颜色；
焦点环 3px 半透明且与描边同色；图标尺寸由组件统一兜底，调用点写 `data-icon` 而不是 `size-4`。
（`active:scale-[0.98]` 是我们自己加的，shadcn 没有——按下缩放会让密集工具栏"抖"，建议去掉。）

### 2.2 Card（`card`）

规格出处：`view @shadcn/card` → <https://ui.shadcn.com/docs/components/radix/card>

| 属性 | shadcn 原版 | 我们改前（`card.tsx`） |
|---|---|---|
| 外框 | `flex flex-col gap-6 rounded-xl border bg-card py-6 text-card-foreground shadow-sm` | `rounded-lg border-line-soft bg-surface shadow-card` |
| 头 | `grid auto-rows-min grid-rows-[auto_auto] items-start gap-2 px-6 [.border-b]:pb-6` | `flex flex-wrap gap-x-2 px-6 py-4 border-b`，且**带语义染色底** |
| 标题 | `leading-none font-semibold` | `text-[15px] font-semibold leading-tight` ← **15 是半档** |
| 描述 | `text-sm text-muted-foreground` | `text-[12.5px] text-fg-3` ← **12.5 是半档** |
| 内容 | `px-6` | `px-5 py-4` |
| 底 | `flex items-center px-6 [.border-t]:pt-6` | `px-6 py-3 text-[12.5px] text-fg-3` |

**它怎么变精致的**：`rounded-xl`(14) + `shadow-sm` + `py-6` 的**大留白**；头/身/脚之间不画满
`border-b`，而是靠 `gap-6` 和 `[.border-b]:pb-6` 只在需要时补间距；标题 `leading-none` 让
标题行高不继承正文，视觉上"钉"在顶部。

### 2.3 Table（`table`）

规格出处：`view @shadcn/table` → <https://ui.shadcn.com/docs/components/radix/table>

| 属性 | shadcn 原版 | 我们改前（`table.tsx`） |
|---|---|---|
| 表容器 | `relative w-full overflow-x-auto` | `w-full overflow-x-auto` |
| `<table>` | `w-full caption-bottom text-sm` | `w-full border-collapse text-left` |
| 表头行 | `[&_tr]:border-b`（**不带底色**） | `bg-surface-2` + 单元格 `border-b border-line` |
| 表头单元格 | `h-10 px-2 align-middle font-medium whitespace-nowrap text-foreground` | `px-3 py-2 text-xs font-medium text-fg-2` |
| 行 | `border-b transition-colors hover:bg-muted/50` | `border-b hover:bg-surface-2` |
| 单元格 | `p-2 align-middle whitespace-nowrap` | `px-3 py-2.5 text-[14px]` |
| 末行 | `[&_tr:last-child]:border-0` | 无（末行仍带 border） |

**它怎么变精致的**：表头**不加底色**、字色与正文同色（`text-foreground`）只降字重差
（`font-medium`），靠 `h-10` 的高度和 `border-b` 分隔；单元格内边距只有 `p-2`(8)——比我们
的 `px-3 py-2.5`(12/10) 紧，行高由 `text-sm` + `p-2` 撑出 ~40px；hover 用 `bg-muted/50`
（半透明）而不是实色，深浅不会跳。

### 2.4 Badge（`badge`）

| 属性 | shadcn 原版 | 我们改前（`badge.tsx`） |
|---|---|---|
| 形状 | `inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-full border border-transparent px-2 py-0.5` | `inline-flex h-[22px] rounded-full border px-2`（写死高度） |
| 字号 | `text-xs`(12) `font-medium` | `text-[12.5px]` ← 半档 |
| 语义色 | `default bg-primary` / `secondary bg-secondary` / `destructive bg-destructive text-white` / `outline border-border text-foreground` | ok/warn/err/info 四谱 chip |
| 图标 | `[&>svg]:size-3`，`data-icon="inline-start/inline-end"` 控制方向 | 无 |
| 焦点 | 同 button：`focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50` | 无 |

**它怎么变精致的**：高度**不写死**，由 `text-xs`(12) + `py-0.5`(2) 自然撑成 20px；
`border-transparent` 占位保证"有无描边"的徽标宽度不跳；`gap-1` + `data-icon` 让
"图标 + 文字"的徽标和纯文字徽标在同一行里对齐。

### 2.5 Tabs（`tabs`）

| 属性 | shadcn 原版 | 我们改前（`tabs.tsx`） |
|---|---|---|
| 轨道 | `inline-flex w-fit items-center justify-center rounded-lg p-[3px] bg-muted h-9` | `flex gap-1 overflow-x-auto border-b` （**下划线式**） |
| 触发项 | `h-[calc(100%-1px)] flex-1 gap-1.5 rounded-md border border-transparent px-2 py-1 text-sm font-medium text-foreground/60` | `-mb-px border-b-2 px-3 py-2 text-xs font-medium` |
| 选中 | `bg-background text-foreground shadow-sm`（药丸式） | `border-primary text-primary-text`（下划线式） |
| hover | `hover:text-foreground` | `hover:text-fg-2` |
| 焦点 | `focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring` | 无（继承全局） |
| line 变体 | `gap-1 bg-transparent`，选中项 `after:h-0.5 after:bg-foreground` 下划线 | — |

**它怎么变精致的**：默认是"药丸在灰轨道里"（`bg-muted` 轨道 + `bg-background` 白药丸 +
`shadow-sm`），不是下划线；`p-[3px]` 的 1.5px 内缩让药丸与轨道之间**刚好一圈**。

### 2.6 Dialog（`dialog`）

| 属性 | shadcn 原版 | 我们改前（`dialog.tsx`） |
|---|---|---|
| 遮罩 | `fixed inset-0 z-50 bg-black/50` | `bg-[rgb(15_23_42_/_0.45)]` ← **裸 rgb** |
| 内容 | `grid w-full max-w-[calc(100%-2rem)] gap-4 rounded-lg border bg-background p-6 shadow-lg` | `rounded-xl border-line bg-surface shadow-modal`，头/身/脚分三段 |
| 标题 | `text-lg leading-none font-semibold` | `text-sm font-semibold` ← 偏小 |
| 描述 | `text-sm text-muted-foreground` | `text-xs text-fg-2` |
| 关闭钮 | `absolute top-4 right-4 rounded-xs opacity-70 hover:opacity-100 focus:ring-2 focus:ring-ring focus:ring-offset-2`，图标 `size-4` | `-mr-1 p-1 text-fg-3 hover:bg-line-soft` |
| 动效 | `duration-200` + `fade-in-0` + `zoom-in-95` | `animate-fade-in` 150ms |

**它怎么变精致的**：整块 `p-6` 的**统一内边距**（而不是头/身/脚各一套 padding + 两条分隔线），
`gap-4` 管纵向节奏，只有需要时才在 footer 加 `border-t pt-4`；标题 18px 比正文大一档，
`leading-none` 让标题块变薄。

### 2.7 Input / Select（`input` / `select`）

| 属性 | shadcn 原版 | 我们改前（`input.tsx`） |
|---|---|---|
| 高度 | `h-9`（select 也是 `data-[size=default]:h-9`） | `h-9` ✅ |
| 内边距 | `px-3 py-1` | `px-3` |
| 圆角 | `rounded-md` | `rounded-md` ✅ |
| 底色 | **`bg-transparent`**（视觉上就是页面底） | `bg-surface-2`（灰底）+ focus 才变白 ← **重量差异最大的一处** |
| 描边 | `border border-input`（8% 灰） | `border-line` |
| 阴影 | `shadow-xs` | 无 |
| 字号 | `text-base md:text-sm`（移动 16 防 iOS 缩放，桌面 14） | `text-sm`(13) |
| placeholder | `placeholder:text-muted-foreground` | `placeholder:text-fg-3` |
| 焦点 | `focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50` | `focus:border-primary + focus:shadow-[0_0_0_3px_var(--color-ring)]` |
| 禁用 | `disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50` | `opacity-60` |
| 无效 | `aria-invalid:border-destructive aria-invalid:ring-destructive/20` | `aria-[invalid=true]:border-danger bg-danger-weak/40` |
| 选中文字 | `selection:bg-primary selection:text-primary-foreground` | 全局 `::selection` |

**它怎么变精致的**：输入框**是透明的**（`bg-transparent`），只有描边 + `shadow-xs`；
我们在灰色底上再铺一层 `bg-surface-2`，所以整页"每个控件都是一块灰砖"——这是"糙"的第二层原因。

### 2.8 Popover / DropdownMenu / Select 面板（浮层）

| 属性 | shadcn 原版 | 我们（`facet-filter.tsx` 的 Popover） |
|---|---|---|
| 面板 | `z-50 w-72 rounded-md border bg-popover p-4 text-popover-foreground shadow-md outline-hidden` | `rounded-lg border-line bg-surface shadow-pop` |
| sideOffset | `4` | `6` |
| 菜单项 | `relative flex items-center gap-2 rounded-sm px-2 py-1.5 text-sm focus:bg-accent focus:text-accent-foreground` | 自绘 label：`rounded-sm px-2 py-1.5 text-2xs hover:bg-surface-2` |
| 分隔线 | `-mx-1 my-1 h-px bg-border` | 无 |
| select item | `rounded-sm py-1.5 pr-8 pl-2 text-sm`；指示器 `absolute right-2 size-3.5`；勾 `size-4` | — |
| 动效 | `data-[state=open]:fade-in-0 zoom-in-95`，`origin-(--radix-*-transform-origin)` | 无（直接出现） |

**它怎么变精致的**：浮层用 `rounded-md`(8)（**比卡片的 14 更小**，小面板不该用大圆角），
`shadow-md` 比卡片重一档；菜单项 `py-1.5`(6) 的行高 + `rounded-sm`(4) 的 hover 块，
hover 是**整行浅灰块**而不是只变文字色。

### 2.9 Tooltip（`tooltip`）

`z-50 w-fit rounded-md bg-foreground px-3 py-1.5 text-xs text-balance text-background`，
箭头 `size-2.5 rotate-45 rounded-[2px] bg-foreground`。
→ **反色小气泡**（黑底白字 12px），不是浅灰底。

### 2.10 Pagination（`pagination`）

| 属性 | shadcn 原版 | 我们改前（`pagination.tsx`） |
|---|---|---|
| 结构 | `<nav aria-label="pagination">` → `<ul class="flex items-center gap-1">` → `<li>` | `div` + `nav gap-1` |
| 页码 | `PaginationLink` = `buttonVariants({ variant: isActive ? "outline" : "ghost", size })`，默认 `size="icon"` → **36×36** | `h-7 min-w-7 rounded-sm text-2xs`（28px） |
| 选中页 | `outline` 变体（描边 + `shadow-xs`），**不是实心蓝块** | `bg-primary text-white` |
| 省略号 | `flex size-9 items-center justify-center` + `MoreHorizontalIcon size-4` | 文本 `…` |
| 首末页 | 没有独立的"首页/末页"按钮 | 有四个箭头按钮 |

**它怎么变精致的**：页码用**按钮变体**复用同一套规格（36px 方形 + 描边表示当前页），
所以分页栏和别处的按钮"长得是一家人"；我们自绘 28px 小方块 + 实心蓝，是另一套视觉。

### 2.11 Skeleton（`skeleton`）

`animate-pulse rounded-md bg-accent`（`--accent` = `oklch(0.97 0 0)`，**纯中性灰**，无渐变）。
我们的 `@utility skeleton` 是**渐变 shimmer**（`linear-gradient` 扫光）——比 shadcn 重。

### 2.12 汇总：shadcn 用来"显得精致"的 8 条硬规则

1. **底色接近纯白**，灰只给 `--muted`（表头/弱底），不用"灰蓝页底 + 白卡"的对比套路。
2. **控件透明**：输入框 `bg-transparent`，只靠 1px `--input` 描边 + `shadow-xs`。
3. **描边比底色重要**：`--border` = `oklch(0.922)`（≈`#e5e5e5`），浅到几乎看不见，但一直在。
4. **字号只有 4 档**（12/14/16/18），字重只有 2 档（`font-medium` / `font-semibold`）。
5. **焦点环 3px 半透明 + 同色描边**，全局一致，不出现 `outline: none` 裸奔。
6. **圆角有层级**：容器 14 > 弹窗 10 > 按钮/输入/浮层 8 > 菜单项 6 > 微件 4。
7. **阴影轻到只有 1px**：`shadow-xs` / `shadow-sm` 负责静置面，浮层才用 `md/lg`。
8. **hover 只动颜色或只动一层底色**，不动尺寸、不动阴影（我们原来的 `active:scale-[0.98]`
   和 `hover:shadow-pop` 都违反这条）。

---

## 3. 我们 vs shadcn：差距清单（按"改起来值不值"排序）

| # | 差距 | 现状 | 目标 | 影响面 |
|---|---|---|---|---|
| 1 | **页底色是灰蓝** | `--color-canvas:#f1f5f9` | 近白 `oklch(1 0 0)`；灰只给 muted | 1 个 token，全站 |
| 2 | **输入框/控件是灰砖** | `bg-surface-2` | `bg-transparent` + `border-input` | input/select/textarea |
| 3 | **字号半档满地** | 12.5 / 15 / 11.5 / 10 / 13 混用 | 12/14/16/18/20/24/30 + 11（仅 2xs 元信息） | 23 处 |
| 4 | **焦点环不统一** | 全局 `box-shadow: 0 0 0 3px rgba(59,130,246,.2)` + 组件各自 `ring-2/ring-[3px]` | 统一 `ring-[3px] ring-ring/50` + `border-ring` | 全局 + 5 个组件 |
| 5 | **圆角不按 token 派生** | 4/6/8/12/16 手写 | `--radius:.625rem` 派生 6/8/10/14 | tokens |
| 6 | **阴影三档且自起名** | `shadow-card/pop/modal` | `shadow-xs` + `shadow-overlay` | tokens + 12 处 |
| 7 | **按钮 hover 靠阴影/缩放** | `hover:shadow-pop` + `active:scale-[0.98]` | `hover:bg-primary/90` | button |
| 8 | **表头染底 + 单元格偏松** | `bg-surface-2` + `px-3 py-2.5` | 无底色 + `h-10 px-2` + `text-sm` | table/data-table |
| 9 | **弹窗是"三段式"** | 头/身/脚 + 两条线 | `p-6 gap-4` 统一内边距，18px 标题 | dialog/drawer |
| 10 | **分页自绘小方块 + 实心蓝** | `h-7 rounded-sm bg-primary` | 36px，`outline` 变体表示当前页 | pagination |
| 11 | **骨架是扫光渐变** | `linear-gradient` shimmer | `animate-pulse bg-muted`（纯灰脉冲） | skeleton |
| 12 | **字体偏几何** | `Albert Sans`（几何无衬线）+ Noto SC | 见 §4 | fonts |

---

## 4. 字体：Albert Sans vs Inter（结论 + 证据）

### 4.1 事实

| 维度 | Albert Sans（现状） | Inter（shadcn 经典默认） |
|---|---|---|
| 设计意图 | 几何无衬线（display 友好、字面率大、x-height 相对高但**字腔开**） | **为 UI/屏幕小字号设计**（高的 x-height、紧凑字距、开放的字符区分） |
| 数字 | 比例数字为主，等宽数字需 `tnum` 且对齐一般 | 原生等宽数字（`tnum`）非常稳，表格数字列不跳 |
| 12–14px 表现 | 大写/小写区分度弱（`I`/`l`、`rn`/`m`），`a` 双层但字腔窄 | `I`/`l` 有区分，`a`/`g` 单层，小字号可读性更好 |
| 中文搭配 | 与 Noto Sans SC 的 x-height 差较大，中英混排基线漂 | 与 Noto Sans SC 的 x-height 更接近，混排更齐 |
| 体积 | 32 KB（latin，400–700 可变） | 48 KB（latin，可变，覆盖 400–600 的常规档） |

### 4.2 结论

**换成 Inter（latin，自托管）+ 保留 Noto Sans SC。** 理由按重要性：

1. 我们是**信息密集型后台**：表格、键值对、日志、状态徽标，大量 12–14px 文字与数字列。
   Inter 的 UI 优化正是为这个场景设计的；
2. 中英混排：Inter 与 Noto Sans SC 的 x-height 更接近，`vLLM 队列` 这种混排不会一大一小；
3. 体积代价可接受：+16 KB（48 KB Latin 可变字体，覆盖常规 400/500/600 三档），
   换掉原来的 32 KB Albert Sans，**净增 16 KB**，且中文 150 KB 不受影响；
4. `Noto Sans SC` 必须保留（中文没有替代品，且已是子集）。

证据：`web/.data/ui-audit/font-compare/font-compare.png`（同一页面、同一视口、同一裁切框，
只换 `--font-sans` 首顺位的并排图，设备像素比 2×；单张裁切也在同目录）。肉眼可见的差别：
**Albert Sans 的同字号数字与拉丁字母更大更圆、行高更松**，同一条表格在 1440 宽下更早被挤满；
Inter 的 x-height 与 Noto Sans SC 更接近，中英混排的基线更平，等宽数字（`1 / 2`、`17m 49s`）对齐更稳。
整站前后对比见 `web/.data/ui-audit/after/compare-*.png`。

### 4.3 不采纳的方案

- **Geist**：shadcn 新版默认，但自托管 Geist 的 woff2 需要从 Vercel 的包/仓库里取，
  且 latin 子集比 Inter 略大；在我们"中英混排 + 表格数字"的场景，收益不足以再换一次。
- **全站保留 Albert Sans**：它的几何风格在**展示型**页面（落地页、大字标题）更好看，
  但我们 90% 的界面是 14px 表格与表单。**折中做了**：Albert Sans 仍然自托管、仍然加载，
  但只给字标 `TestPlane`（`components/app/Wordmark.tsx` 的 SVG `<text>`）用——
  所以 `web/tests/layout-audit.mjs` 里"Albert Sans 已加载"那条断言不用改，字标也没换脸。
  代价是多留了 32 KB（字标只用一屏，命中率高，可接受）。

---

## 5. 落点（这份研究对应的改动）

| 研究结论 | 落点 |
|---|---|
| §1.1 oklch 色阶 + 语义别名 | `web/src/index.css`（`@theme` 里 `--color-surface: var(--color-card)` 等别名映射，见文件头注释） |
| §1.2 `--radius` 派生 | `web/src/index.css` |
| §1.3 两级阴影 | `web/src/index.css`（`--shadow-xs` / `--shadow-overlay`） |
| §1.4 统一焦点环 | `web/src/index.css` 的 `@layer base :focus-visible` + 各组件 `focus-visible:` |
| §4 字体 | `web/src/styles/fonts.css` + `web/public/fonts/inter-latin-var.woff2` |
| §2 各组件数值 | `web/src/components/ui/{button,card,badge,table,tabs,dialog,drawer,input,segmented,skeleton,states,pagination,data-table,facet-filter}.tsx` |
| 全部断言 | `web/tests/ui-audit.mjs` |

审计的口径（哪些是 P0、哪些是 P1/P2、阈值从哪来）写在 `web/tests/ui-audit.mjs` 的文件头，
每次运行产出的报告落在 `web/.data/ui-audit/ui-audit-report.md`（改前基线是
`ui-audit-report-before.md`）。

---

## 6. 取舍与偏差（**刻意没做 / 做不到**的部分）

这一节是交付的一部分，不是免责声明：每一条都给出"为什么"，便于后续有人不同意时直接改回去。

| # | 偏差 | 决定 | 理由 |
|---|---|---|---|
| 1 | 圆角集合 | 用 `--radius` 派生出来的 **4/6/8/10/14**，而不是票面写的 4/6/8/12 | `--radius:0.625rem` 派生出来的 `lg` 就是 10px；硬凑成 12 就等于把 `--radius` 从唯一真源变成摆设。审计断言的是"只出现派生值"。 |
| 2 | `fg-3`（muted-foreground） | 用 `oklch(0.53)` 而不是 shadcn 的 `oklch(0.556)` | 0.556 在白底 4.73:1、在 `--muted`(#f5f5f5) 底上只有 **4.34:1**，差 0.16 过不了 AA。压到 0.53 后白底 5.28、灰底 4.84，肉眼几乎无差别。 |
| 3 | 表头字号 | 12px（shadcn 是 14px `text-foreground`） | 6+ 列的密集数据表，14px 表头会和正文抢注意力，横向也更挤。表头用 12px + `font-medium` + 不染底，行高统一 `h-10`。 |
| 4 | Tabs | 保留下划线式，没换成 shadcn 的"药丸在灰轨道里" | 我们的 tablist 是**整条贴边**的（列表页/详情页顶部），药丸式需要一条灰轨道包着，结构上要改 DOM 与页面留白；票面明确"不改交互与信息结构"。尺寸/字重/焦点环已按 shadcn 对齐（h-9 / text-sm / font-medium / ring-[3px]）。 |
| 5 | 可点元素高度 | 23 个 24–28px 的目标保留（行内复制按钮、表头漏斗、字标链接、Task 名按钮），记 **P1 不阻断** | 这些是"表格行内的小工具"，强行拉到 32px 会把 40px 的行撑到 56px，密集表格失去意义。它们都 ≥24px，满足 WCAG 2.5.8（AA）的下限，P0 里没有它们。 |
| 6 | 阴影分档 | 只有 `shadow-xs` 与 `shadow-overlay` 两档 | 票要求的"收敛成两级"。shadcn 本身用 4 档（xs/sm/md/lg），我们把 md+lg 合并成 `--shadow-overlay`（两层叠加：10px 30px -6px 大扩散 + 2px 8px -2px 贴边），弹窗与气泡共用。 |
| 7 | `components.json` | 建了（`base: radix` / `style: new-york` / Tailwind v4 / `tailwindCss=src/index.css` / alias `@/components/ui`），但**没有 `add` 任何组件** | 票的边界：不能替换现有 API（100+ 调用点）。`style: new-york` 是**字面量 class** 体系，后续 `add` 出来的组件自带完整 Tailwind 类，能直接吃到我们的 `@theme` token；没有选 `radix-nova`，是因为 nova 把类名搬进了 `shadcn/tailwind.css` 的 `cn-*`，那些类我们仓库里没有，`add` 出来的组件会"没有样式"。 |
| 8 | 像素级回归 | **没做** | 像素比对只能回答"变了没变"，回答不了"对不对"；而且字体抗锯齿/系统 Chrome 版本一换就全红。替代方案就是 `ui-audit.mjs` 的规范断言 + 截图存档（人要复核时看得见）。 |
| 9 | 深色终端 | 保留深色底（`--color-terminal`），没有改成 shadcn 的浅色 | 日志是"原样原文"，深底是 `design/impl/web-and-delivery.md §4.3` 的既有决定，不在视觉精修范围。只把四个日志级别色收成语义 token（`--color-log-*`），并保证在深底上 ≥4.5:1。 |
| 10 | 卡片头上的语义色染底 | 保留（`bg-success-weak` 这类 50 级浅染） | 这是本产品的信息设计（"卡片颜色=卡片语义"），shadcn 没有对应物。保留但确保染底上的文字对比度达标（把 header 里的 `fg-3` 提到 `fg-2`/统一压深）。 |

---

## 7. 审计口径与实测结果

`web/tests/ui-audit.mjs` 跑在真实渲染结果上（mock 或真后端都行，用 `PLAYWRIGHT_BASE_URL` 指定），
输出三类东西：**规范断言**（P0/P1/P2）、**截图存档**（8 页 × 3 视口 = 24 张）、**Markdown 报告**。

| 阶段 | P0 | P1 | P2 | 截图 | 退出码 |
|---|---|---|---|---|---|
| 改前基线（`UI_AUDIT_PHASE=before`） | **162** | 35 | 140 | 24 | 1 |
| 精修后（默认 `after`） | **0** | 23 | 105 | 24 | 0 |

改前 162 条 P0 的构成（这也是"为什么看起来糙"的量化答案）：

| 规则 | 条数 | 典型 |
|---|---|---|
| `@theme` 里还有 hex 色值 | 35 | `--color-surface:#ffffff` |
| 渲染结果里有半档字号 | 34 | 12.5 / 11.5 / 15 / 22px |
| 可点元素 <24px | 24 | 行内复制按钮 15.9px |
| 源码里出现半档字号 | 22 | `text-[12.5px]` ×7、`text-[15px]` ×5 |
| 业务组件出现裸色值 | 22 | 日志面板 `text-[#fca5a5]` |
| 文本对比度不足 | 9 | 看板 30px 绿字 2.54:1、分页蓝底白字 3.68:1 |
| 圆角不在 token 派生值上 | 8 | 卡片 12px（应为 14） |
| 阴影不在两级之内 | 8 | 卡片 `shadow-card` |
| `--radius` 基准缺失 | 1 | 没有 `--radius` |

改后的 P1 只剩 23 条"可点元素 24–28px"（见 §6 第 5 条）；P2 是间距栅格的趋势统计（不阻断）。

