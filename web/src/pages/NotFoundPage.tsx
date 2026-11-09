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
 * **版式（用户 2026-11-09 定稿）**：插画在**左**，右侧只放两个动作按钮。
 * 「404 / 这个地址没有对应的页面 / 可能是链接过期了……」那两行文字**不再显示**（图里已经写着 404，
 * 文字与图并排像没对齐的两栏）。文字没有丢：`h1` 与说明留成 `sr-only`，
 * 读屏与 SEO 仍拿得到「404 + 上下文」——是"视觉上删掉"，不是"语义上删掉"。
 *
 * 图是用户提供的插画：`public/404.webp`（760px / 69KB；源图 1254px PNG 1MB，转 webp 后才入库，
 * 别为一张 404 图把首屏撑大 1MB）。装饰性 `<img alt="">` + `aria-hidden`。
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
      <Card className="px-6 py-8 sm:px-10 sm:py-10">
        <div className="flex flex-col items-center gap-8 md:flex-row md:items-center md:gap-12">
          {/* 主视觉：左 */}
          <img
            src="/404.webp"
            alt=""
            aria-hidden
            width={760}
            height={760}
            decoding="async"
            data-testid="not-found-art"
            className="w-full max-w-[16rem] shrink-0 select-none sm:max-w-[20rem] md:max-w-[22rem]"
          />

          {/* 右侧只有动作；标题与说明对读屏可见（sr-only），视觉上由图承担 */}
          <div className="min-w-0 md:flex-1">
            <h1 id="not-found-title" className="sr-only">
              404 {title}
            </h1>
            <p className="sr-only">{description}</p>

            <div className="flex flex-col items-stretch gap-2 sm:flex-row sm:items-center">
              <Link to="/" data-testid="not-found-home">
                <Button
                  variant="primary"
                  icon={<ArrowLeft className="size-3.5" aria-hidden />}
                  className="w-full sm:w-auto"
                >
                  回到概览
                </Button>
              </Link>
              <Link to="/runs" data-testid="not-found-runs">
                <Button icon={<List className="size-3.5" aria-hidden />} className="w-full sm:w-auto">
                  打开最近 Run 列表
                </Button>
              </Link>
            </div>
          </div>
        </div>
      </Card>
    </section>
  )
}
