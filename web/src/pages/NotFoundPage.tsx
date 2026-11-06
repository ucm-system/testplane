import { useEffect } from 'react'
import { Link } from 'react-router-dom'
import { ArrowLeft, List } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'

export interface NotFoundPageProps {
  /** 一句话说明（按上下文变化：路由不存在 / Run 已被清理 / …） */
  title?: string
  description?: string
}

/**
 * 全站统一的 404 页。
 *
 * 三处必须走**同一个组件**（`App.tsx` 里 `/404` 与 `*`，以及 `/runs/:id` 取到 404 时）：
 *  - 未匹配路由、`/404`：默认文案「这个地址没有对应的页面」；
 *  - Run 详情 404：换一句上下文文案（「Run 不存在或已被清理」），动作与插画不变。
 *
 * **landmark 纪律**：`AppShell` 已经有 `<main id="main">`，这里**不能再有** `role="main"`
 * —— 同页两个 main landmark 会让"跳到主内容"的目标变歧义，读屏也会重复播报。
 * 改用 `<section aria-labelledby>`，并把 `document.title` 设成「404 · 页面不存在」。
 *
 * 视觉只用本仓库的设计令牌（Tailwind + `Card` / `Button`），插画是**内联 SVG**，
 * 不引图片资源；动效是 CSS 动画，`prefers-reduced-motion: reduce` 时由 `index.css`
 * 里的同名工具类覆盖成 `animation: none`（不是只把时长调到 0.001ms）。
 */
export function NotFoundPage({
  title = '这个地址没有对应的页面',
  description = '可能是链接过期了，或者地址打错了。回到概览重新开始，或者去看看最近的 Run。',
}: NotFoundPageProps) {
  useEffect(() => {
    const previous = document.title
    document.title = '404 · 页面不存在'
    return () => {
      document.title = previous
    }
  }, [])

  return (
    <section aria-labelledby="not-found-title" data-testid="not-found" className="min-w-0">
      <Card className="px-6 py-10 sm:px-10 sm:py-12">
        <div className="grid items-center gap-8 md:grid-cols-[minmax(0,1fr)_minmax(0,20rem)]">
          <div className="min-w-0">
            <p className="font-mono text-2xs uppercase tracking-[0.32em] text-fg-3">page not found</p>
            <h1 id="not-found-title" className="mt-2 flex flex-wrap items-baseline gap-x-3 gap-y-1">
              {/*
                大字 404 是**标题的一部分**（读屏念出来是「404 这个地址没有对应的页面」），
                所以不 aria-hidden —— 它不是装饰，是这页的身份。
              */}
              <span className="text-[76px] font-semibold leading-[0.85] tracking-[-0.05em] text-fg">404</span>
              <span className="text-lg font-semibold leading-snug text-fg-2">{title}</span>
            </h1>
            <p className="mt-3 max-w-md text-sm leading-relaxed text-fg-2">{description}</p>

            <div className="mt-6 flex flex-wrap items-center gap-2">
              <Link to="/" data-testid="not-found-home">
                <Button variant="primary" icon={<ArrowLeft className="size-3.5" aria-hidden />}>
                  回到概览
                </Button>
              </Link>
              <Link to="/runs" data-testid="not-found-runs">
                <Button icon={<List className="size-3.5" aria-hidden />}>打开最近 Run 列表</Button>
              </Link>
            </div>
          </div>

          {/* 轻量插画：纸飞机沿虚线航线飞走。纯内联 SVG + CSS 动效，不依赖任何图片文件。 */}
          <svg
            viewBox="0 0 320 200"
            className="mx-auto h-40 w-full max-w-[320px] text-fg-3"
            role="img"
            aria-labelledby="not-found-art"
          >
            <title id="not-found-art">一架纸飞机沿着虚线航线飞走，表示这个页面已经不存在</title>
            <ellipse cx="160" cy="182" rx="88" ry="9" className="fill-surface-3" />
            <path
              d="M16 158 C 92 158 86 84 156 92 S 236 96 286 40"
              fill="none"
              strokeWidth="2.5"
              strokeLinecap="round"
              strokeDasharray="7 9"
              className="notfound-dash stroke-line-strong"
            />
            <circle cx="16" cy="158" r="5" className="fill-warn" />
            <circle cx="46" cy="150" r="2.5" className="fill-line-strong" />
            <circle cx="104" cy="120" r="2.5" className="fill-line-strong" />
            <g className="notfound-float">
              <path d="M286 40 L318 22 L300 58 L293 45 Z" className="fill-primary" />
              <path d="M300 58 L293 45 L286 40 L299 44 Z" className="fill-primary-text opacity-60" />
            </g>
          </svg>
        </div>
      </Card>
    </section>
  )
}
