import { useState } from 'react'
import { Check, Copy, Wand2 } from 'lucide-react'
import { cn } from '@/lib/utils'

/**
 * 输入示例块（用户 2026-11-11：示例要「可查看、可复制」，不能只藏在 placeholder 灰字里）。
 *
 * - 内容 = runner.yaml 的 `placeholder`（与 input_format 同格式的合法片段，§5.4）；
 * - 「复制」进剪贴板；「填入」交给调用方（提交页覆盖输入框，带撤销 toast）；
 * - 默认折叠（一行 summary），点开看全文 —— 不占首屏，但一眼能找到。
 */
export function InputExample({
  example,
  onFill,
  title = '输入示例（placeholder 来自 runner.yaml）',
  testIdPrefix,
  className,
}: {
  example: string
  /** 给了才显示「填入」按钮（提交页覆盖输入框；目录页只复制） */
  onFill?: () => void
  title?: string
  /** data-testid 前缀（`{prefix}-example` / `{prefix}-copy-example` / `{prefix}-fill-example`） */
  testIdPrefix?: string
  className?: string
}) {
  const [copied, setCopied] = useState(false)

  const copy = () => {
    void navigator.clipboard?.writeText(example).catch(() => undefined)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1500)
  }

  return (
    <details
      data-testid={testIdPrefix ? `${testIdPrefix}-example` : undefined}
      className={cn('group rounded-md border border-line-soft bg-surface-2', className)}
    >
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-2.5 py-1.5 text-2xs text-fg-2 [&::-webkit-details-marker]:hidden">
        <span className="text-fg-3 transition-transform group-open:rotate-90" aria-hidden>
          ▸
        </span>
        示例
        <span className="text-fg-3">（可复制；点开看全文）</span>
      </summary>
      <div className="border-t border-line-soft px-2.5 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <p className="text-2xs text-fg-3">{title}</p>
          <div className="ml-auto flex items-center gap-1.5">
            <button
              type="button"
              data-testid={testIdPrefix ? `${testIdPrefix}-copy-example` : undefined}
              aria-label="复制输入示例"
              onClick={copy}
              className="inline-flex h-6 items-center gap-1 rounded-sm px-2 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
            >
              {copied ? (
                <Check className="size-3 text-success" aria-hidden />
              ) : (
                <Copy className="size-3" aria-hidden />
              )}
              {copied ? '已复制' : '复制'}
            </button>
            {onFill ? (
              <button
                type="button"
                data-testid={testIdPrefix ? `${testIdPrefix}-fill-example` : undefined}
                aria-label="把示例填进输入框"
                onClick={onFill}
                className="inline-flex h-6 items-center gap-1 rounded-sm px-2 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
              >
                <Wand2 className="size-3" aria-hidden />
                填入输入框
              </button>
            ) : null}
          </div>
        </div>
        <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-all rounded-md border border-line-soft bg-surface px-2 py-1.5 font-mono text-2xs leading-[16px] text-fg-2">
          {example}
        </pre>
      </div>
    </details>
  )
}
