# TestPlane 低保真原型（示意）

纯前端、只读的示意原型，用来讨论「信息怎么组织、操作放哪里」，不是可运行系统。

- 打开方式：双击 `index.html`
- 不需要服务器 / 构建 / 联网
- 所有数据都是 `index.html` 里的 mock 常量，刷新即还原

## 风格：圣杯布局（holy-grail-layout）

按 `holy-grail-layout-style-pack` 技能实现，覆盖层是 `assets/holy-grail.css`（在 tokens / components 之后加载）。

| 规则 | 实现 |
|---|---|
| 固定页头 | `.hg-header` `position:sticky; top:0; z-index:50` |
| 固定页脚 | `.hg-footer` `position:sticky; bottom:0` |
| 三列 | `.hg-body` grid `240px minmax(0,1fr) 288px`，主内容决定整行高度 |
| 侧栏钉住 | 两侧栏 `position:sticky; top:60px`、高 `calc(100vh - 100px)`（页头 60 + 页脚 40）：不随主内容滚走；左栏定一屏（导航永不超一屏），右栏内容超出时栏内自滚（`overflow-y:auto`） |
| 主内容优先 | DOM 里 `<main>` 在两侧 `<aside>` **之前**，靠 `grid-column` 摆到中间 |
| 侧栏定宽 | 左 240 / 右 288，不随内容变化 |
| 响应式 | >1024 三列；768–1024 隐右栏（左栏仍钉住）；<768 全堆叠（左栏复原为顶部普通块） |
| 按钮 | `active: scale(0.98)` + focus 环；hover 上浮 2px + 蓝色投影 |
| 卡片 | hover `-2px` + `shadow-md`，150ms ease-out（不干扰阅读） |
| 侧边导航 | hover 只亮左边框 2px，**无垂直位移** |
| 动效 | 一律 150ms ease-out，无一处超过 200ms |
| 浮层 | 遮罩 `z-index:1200` → 抽屉 1210（压在遮罩之上）；遮罩只压暗，不做全屏高斯模糊——模糊会让抽屉里的表格看着发虚 |
| 降级 | `prefers-reduced-motion` 关掉位移与缩放 |

禁止项自检（脚本核对）：`rounded-none` / `shadow-2xl` / `font-black` / `font-extrabold` / `text-5xl~8xl` 均为 0 处。

## 截图

| 页面 | 文件 |
|---|---|
| 概览看板 | `screenshots/dashboard.png` |
| 运行列表 | `screenshots/runs.png` |
| 运行详情（流程图 + 右侧事件） | `screenshots/run-detail.png` |
| Tester 目录 | `screenshots/testers.png` |
| Tester 详情抽屉 | `screenshots/tester-drawer.png` |
| 提交 · 部署模式 | `screenshots/submit.png` |
| 提交 · 不部署模式 | `screenshots/submit-no-deploy.png` |
| Pod 日志里的重启标记 | `screenshots/deploy-logs.png` |
| 平板（隐右栏） | `screenshots/layout-tablet.png` |
| 手机（全堆叠） | `screenshots/layout-mobile.png` |

## 页头

| 元素 | 做法 |
|---|---|
| 品牌 | **SVG 字标**（图标 + 字母排在一起），高度 30px、字重 650——它是身份，不是 navigation |
| 品牌列 | 定宽 204px：24（页头左距）+ 204 + 12（间距）= 240 = 侧栏宽度 |
| 分隔线 | 1px 竖线，把"产品"和"当前页面"切开，正好落在侧栏右边界的延长线上 |
| 用户列 | 定宽 264px（右栏 288 − 24 页头右距）：左边框落在 `vw−288`，与右栏左边界同一条竖线——左右两侧的分隔线都踩在三列网格的列线上 |
| 页面标题 | 16px / 600，只说明"现在在哪" |
| 右侧 | 页面级操作 + 当前用户（不做全局搜索——列表页有自己的搜索）；概览与运行列表**不放主按钮**，提交入口统一在左侧导航 |

**页头只放"页面级操作"，不放"产品级操作"**：`新建 Run` / `提交运行` 属于后者，已经在左侧导航里，重复放在页头会跟页面主体抢注意力。现在只有运行详情页的页头有按钮（重试失败的 Task / 取消 Run / 返回列表），因为它们操作的是"当前这个对象"。

之前把页面标题做到 19px、品牌只有 15px，等于让"运行列表"压过产品名。现在的顺序是：**字标 > 页面标题 > 导航**。
## 页面

| 路由 | 页面 | 右栏放什么 |
|---|---|---|
| `#/dashboard` | 概览：队列 / 排队中的 Task / 概况 / 最近 Run | 快捷操作 + 页面说明 |
| `#/runs` | 运行列表 | 终态语义图例 |
| `#/runs/new` | 提交运行 | 参数说明（只读） |
| `#/runs/:id` | 运行详情：执行链路图 + 节点详情 | 事件时间线 |
| `#/testers` | Tester 目录 | 注册规则 |
| `#/profiles` | 执行环境 | 边界说明 |

右栏只放辅助信息，主内容永远最宽——这是圣杯布局的平衡点。

## 看板上讲什么

1. **谁在排队** —— 每个 Volcano 队列的「已分配 / 配额」、排队 Job 数、最长等待、主要原因。
2. **卡在哪一层** —— 队列已满 / 资源不足 / gang 未满足 / 平台槽位。排队中的 Task 单独列出，标出归属与原因。
3. **有没有东西挂着** —— 进行中 Run、排队中 Task、近 24h 失败数。

**刻意不展示节点与 NPU / GPU 用量**：使用者要判断的是"要不要提、要等多久"，不是集群硬件账本。要看机器去 K8s 面板，这里只讲到队列为止。

## 为什么之前看着"不够精致"

对照 stylekit 的 showcase 逐项量过，三个原因，按影响从大到小：

**1. 字号没有层级（主因）**

| | 之前 | 现在 |
|---|---|---|
| 页面标题 | 15px | 19px / 650 |
| 品牌名 | 14px | 17px / 650 |
| 卡片标题 | 13px | 15px / 600 |
| 表格正文 | 13px | 14px |
| 左侧导航 | 13px | 13.5px / 500 |
| 表头 | 11px | 12px / 500 |
| 说明文字 | 11px | 12.5px |
| KPI 数字 | 26px | 30px / 650，tabular-nums |

层级顺序要守住：**页面标题（19）≥ 品牌（17）> 卡片标题（15）≈ 正文（14）> 导航（13.5）**。

导航一旦和品牌一样大甚至更大，左上角就变成"又一个菜单项"，整个界面失去身份感——第一版就是这个毛病。

之前所有文字都挤在 11–13px，**没有主角**——眼睛找不到第一落点，读起来像一堆等重的清单。showcase 那边用了 	ext-4xl / 2xl / lg / sm / xs 五档，我们只用了三档且都在底部。

**2. 弱化文字太浅（次因）**

之前的 #9ca3af 在白底上对比度只有 **2.3:1**，WCAG AA 要求 4.5:1。stylekit 自己的无障碍表也把这一档标成不通过。现在换成 #667085（4.9:1），"看着更清楚"的同时也不再像没渲染完。

**3. 字体栈没配对（第三位，但 Windows 上明显）**

之前的栈是 -apple-system, Segoe UI, PingFang SC, Microsoft YaHei：Windows 上前两个都不存在，Latin 落到 Segoe UI，中文落到微软雅黑——两者字面高度和字宽差异大，混排时一行里忽宽忽窄。现在中文优先 Microsoft YaHei UI（比雅黑窄一档，小字号更清晰），并统一开了 -webkit-font-smoothing: antialiased。

> 说明：风格规则明确禁止 Inter / Roboto / Geist，所以没有引入 webfont，只在系统栈内部换更合适的顺序。

另外补了两处结构性的东西：

- **导航加图标**：纯文字竖向列表很难有"设计过"的感觉，加了 16px 线性图标（同色、激活态满对比）。
- **排队中的 Task 移到右栏**：主内容只剩 KPI / 队列 / 最近 Run 三块，右栏有真实内容，三列才平衡——这也是圣杯布局右栏该放的东西（辅助信息、上下文工具）。
## 字体：自托管，不依赖用户系统

**问题**：字体是在用户浏览器里渲染的，所以默认跟随用户的系统。团队里 Windows（微软雅黑）、macOS（苹方）、Linux（Noto）看到的是三种字，同一份设计在三台机器上不一样。

**做法**：把字体文件放进 ssets/fonts/，用 @font-face 自托管。部署时随静态资源一起发到服务器，浏览器从自己的服务下载——**不是运行时调 CDN，离线环境一样能用**。

| 用途 | 字体 | 体积 |
|---|---|---|
| 拉丁（数字、Run、Pre-Check） | Albert Sans 可变字重 | 31 KB |
| 中文 | Noto Sans SC 子集（可变字重 100–900） | 147 KB |

合计 178 KB，一次加载后浏览器缓存。

**中文为什么是子集**：完整 Noto Sans SC 可变字体约 8–9 MB，对一个内部工具太重。子集只收录页面里出现过的 529 个汉字，147 KB。

> 代价：**子集之外的汉字会回落到系统字体**。页面里的固定文案没问题，但由数据驱动的中文（Tester 名称、Tester 输出的错误信息、用户填的 Run 名称）可能包含子集里没有的字，那部分字形会不一致。要做干净就得二选一：打包完整中文字体（+8 MB），或在构建时用真实内容重新生成子集。

**字体栈**（ssets/holy-grail.css）：

`css
--fde-font-sans: "Albert Sans", "Noto Sans SC", "Segoe UI Variable Text",
                 "Segoe UI", system-ui, -apple-system, "PingFang SC",
                 "Microsoft YaHei UI", "Microsoft YaHei", sans-serif;
`

前面两个是自托管的，后面全是兜底——字体加载失败也不会出豆腐块。

**重新生成子集**：页面文案改动后，需要把新出现的汉字补进子集（否则回落到系统字体）。做法是对 index.html 提取 CJK 字符，再向 Google Fonts 请求 	ext= 子集，下载 woff2 覆盖 ssets/fonts/noto-sans-sc-var.woff2。
## 配色与文案约定

| 约定 | 做法 |
|---|---|
| 品牌图标 | `assets/testplane-icon.svg`：深藏青底 `#0B1220`（与主按钮 hover 同色）+ 青→靛渐变纸飞机；页头 28px 与 favicon 同源，原样使用不改背景 |
| 主按钮 | 墨黑 `#111827`：hover 上浮 + 黑色投影；蓝色留给链接、选中、焦点环 |
| 辅助色 | 蓝 `#3b82f6` / 绿 `#10b981` / 琥珀 `#f59e0b` / 红 `#ef4444`，按语义用而不是装饰 |
| 卡片头 | 按语义浅染底（4–8% 透明度）+ 标题前色点，全站同一套映射；卡片 `overflow:hidden`，染色不伸出圆角 |

**卡片头语义色映射**（`hg-blue / hg-green / hg-amber / hg-red / hg-ink / hg-muted`）：

| 色 | 语义 | 用在 |
|---|---|---|
| 蓝 | 执行 / 数据 | 执行链路、事件、结果指标、Task 列表、Tester 目录 |
| 绿 | Run / 成功 | 最近 Run、全部 Run、提交页的「运行」、成功 Task 的卡头 |
| 琥珀 | 等待 / 凭据 | Volcano 队列、排队中的 Task、环境变量与凭据、排队 Task 的卡头 |
| 红 | 失败 | 失败 Task 的卡头、错误横幅 |
| 墨黑 | 基础设施 | Deploy / Pods / Pod 日志、执行环境、日志卡 |
| 灰 | 说明 / 档案 | 参数说明、注册规则、边界、终态语义、Attempts / 产物 |

链路节点（`.flow-node--ok/err/warn/info`）按状态轻微染底，和徽标同一色相——链路是详情页的地图，颜色不该只属于徽标。
| KPI 数值 | 按状态上色，不留一律黑字 |
| 状态标签 | **一律中文**：成功 / 失败 / 跳过 / 已取消 / 排队中 / 待调度 / 运行中 / 开放 / 已关闭 |
| 标识符 | Tester 名、错误码、`role`、镜像引用保持英文；标识符用描边+等宽胶囊（`fde-tag--id`），中文标签用灰底胶囊（`fde-tag`），与 badge / chip 同为胶囊形状 |

## 服务来源：一张卡片，两种形态

`deploy` 与 `service` 二选一（互斥），UI 用「部署 / 不部署」切换：

| 模式 | 填什么 | 平台行为 |
|---|---|---|
| 部署 | Genet 的 YAML | 从 Genet 返回的 Endpoint 取三个公共参数 |
| 不部署 | `url` / `model_name` / `model_path` | 从填的这三个字段取 |

两种模式下游完全一致：都是先得到 Endpoint，再投影成 `--url` / `--model_name` / `--model_path` 追加到每个 Task 的 argv 末尾（§5.4.1）。

## mock 数据覆盖的场景

| Run | 场景 |
|---|---|
| `R-20261104-0031` | 全部成功 + 4 Pod PD 分离 |
| `R-20261104-0029` | 单 Task 失败但继续（`continue`） |
| `R-20261104-0027` | Pre-Check 失败 → `abort_run`：串行链路一个都没开始，全部 SKIPPED |
| `R-20261104-0032` | 执行中，日志实时追加 |
| `R-20261104-0034` | 用户取消：跑着的标 CANCELED，后面的标 SKIPPED |
| `R-20261104-0033` | 不部署、跑已有服务 |

**示例按真实形态排**：一个 Run 里绝大多数时间只有一个 Task 在跑——Pre-Check 和性能类都是独占 Endpoint 的，精度类也要排队等同一个 Endpoint。执行链路画成**横向顺序链**（不再有"并行分组"框），5 个节点在 1440 宽下能排下一行；名字过长时省略、悬停看全名，更多 Task 就横向滚动。

## 重试

| 入口 | 行为 |
|---|---|
| 顶部「重试失败的 Task」 | 批量，只重跑 `FAILED` / `CANCELED`，已成功的复用 |
| Task 详情「重试此 Task」 | 单个，新建 Attempt，旧 Attempt 保留 |

点下去能看到 Attempts 表新增一行、事件流多一条——重点演示 **Attempt 不是覆盖而是追加**。

## 日志

1. 一个 Deploy 可能多个 Pod，日志按 Pod 分页签，不合并。
2. 测试期间日志还在长：Deploy 详情标「日志区间」，运行中的 Run 标「追加中」。
3. Pod 重启插 `--- [container restarted #N ... OOMKilled] ---`，续写不覆盖。
4. 落在 `deploy/logs/<pod>.log`，与 Pod 生命周期解耦（§8.6）。
5. 日志查看器为深色终端风（底色 `#0b1220`，与品牌图标一致）：时间戳弱化、错误红、告警/重启标记琥珀、成功绿。

## 交互哪些是真的

搜索筛选、流程图节点切换、Pod 页签、部署/不部署切换、提交、重试——都是前端内存操作；取消 / 下载只弹提示。提交页 Deploy YAML 编辑器带实时语法着色与结构校验（透明 textarea 叠着色层实现，零依赖）。

**两处代码输入框（Deploy YAML 与 Task 参数）共用一套控件度量**：等宽 12.5px、1.7 行高、同边框圆角、`resize:none`，随内容自动长高（104–360px，超出转内部滚动）。原先参数框可拖拽变长、YAML 框固定 150px，是两套东西——同一个语义不该长两个样。

## 已知未实现

- K8s Executor、Genet、Volcano 查询、真实日志采集、SSE、鉴权、分页、真实 API。
- 表单不做校验（设计上就不校验参数），错误由 Tester 运行时暴露。例外：Deploy YAML 编辑器做**子集级结构检查**（Tab 缩进、2 格缩进、兄弟键查重、缺顶层 `provider`），不是完整 YAML 解析器。
- 日志是静态 mock，没有逐行追加动画（着色按行匹配 error/warn/OOM/重启标记等关键词）。

## 文件

```text
design/prototype/
├── index.html              # 所有页面与 mock 数据
├── assets/
│   ├── tokens.css          # 设计令牌
│   ├── components.css      # 基础组件
│   ├── holy-grail.css      # 圣杯布局覆盖层（颜色/圆角/动效/三列外壳）
│   ├── fonts.css           # @font-face 自托管字体声明
│   ├── fonts/              # albert-sans-latin.woff2 / noto-sans-sc-var.woff2
│   └── testplane-icon.svg  # 平台图标（favicon / 页头 logo）
├── screenshots/            # 10 张渲染截图
└── README.md
```
