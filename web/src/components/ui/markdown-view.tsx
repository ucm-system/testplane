import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { cn } from '@/lib/utils'

/**
 * report.md 渲染（用户 2026-11-09：原文 `<pre>` 看起来像终端输出，Markdown 应该有组件渲染）。
 *
 * 选 `react-markdown` 的理由：默认**不渲染原始 HTML**（不引入 `dangerouslySetInnerHTML`），
 * 也就不需要再挂 DOMPurify；`remark-gfm` 只补表格/删除线/任务列表这几样报告里真会用到的。
 *
 * 尺寸按 shadcn 的排版习惯：正文 14px、标题 `tracking-tight`、段落 `leading-[16px]`，
 * 表格沿用仓里的 `border-line` 发丝线。颜色只管语义（`text-fg` / `muted-foreground`），
 * 不顺带把 markdown 里的 HTML class 当样式通道。
 */
export function MarkdownView({
  source,
  className,
  'data-testid': testId,
}: {
  source: string
  className?: string
  'data-testid'?: string
}) {
  return (
    <div
      data-testid={testId}
      className={cn(
        'max-h-[32rem] overflow-auto text-sm leading-[16px] text-fg-2',
        '[&_h1]:mb-2 [&_h1]:mt-4 [&_h1]:text-lg [&_h1]:font-semibold [&_h1]:tracking-tight [&_h1]:text-fg',
        '[&_h2]:mb-2 [&_h2]:mt-4 [&_h2]:text-base [&_h2]:font-semibold [&_h2]:tracking-tight [&_h2]:text-fg',
        '[&_h3]:mb-1.5 [&_h3]:mt-3 [&_h3]:text-sm [&_h3]:font-semibold [&_h3]:text-fg',
        '[&_p]:my-2 [&_p]:text-fg-2',
        '[&_ul]:my-2 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:my-2 [&_ol]:list-decimal [&_ol]:pl-5',
        '[&_li]:my-1',
        '[&_a]:text-primary-text [&_a]:underline [&_a]:underline-offset-2',
        '[&_code]:rounded-sm [&_code]:bg-surface-3 [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-xs',
        '[&_pre]:my-3 [&_pre]:overflow-auto [&_pre]:rounded-lg [&_pre]:border [&_pre]:border-line [&_pre]:bg-surface-2 [&_pre]:p-3 [&_pre]:font-mono [&_pre]:text-xs',
        '[&_pre_code]:bg-transparent [&_pre_code]:p-0',
        '[&_table]:my-3 [&_table]:w-full [&_table]:border-collapse [&_table]:text-xs',
        '[&_th]:border-b [&_th]:border-line [&_th]:px-2 [&_th]:py-1.5 [&_th]:text-left [&_th]:font-medium [&_th]:text-fg',
        '[&_td]:border-b [&_td]:border-line-soft [&_td]:px-2 [&_td]:py-1.5',
        '[&_blockquote]:my-2 [&_blockquote]:border-l-2 [&_blockquote]:border-line [&_blockquote]:pl-3 [&_blockquote]:text-fg-3',
        '[&_hr]:my-4 [&_hr]:border-line',
        className,
      )}
    >
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{source}</ReactMarkdown>
    </div>
  )
}
