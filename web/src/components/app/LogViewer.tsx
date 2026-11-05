import { useMemo } from 'react'
import { Copy } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ErrorState } from '@/components/ui/states'
import { Skeleton } from '@/components/ui/skeleton'
import { useToast } from '@/components/ui/toast'
import type { ApiErrorBody } from '@/api/types'
import { cn, copyText } from '@/lib/utils'

type LineKind = 'plain' | 'time' | 'error' | 'restart' | 'gap' | 'ok'

const TIME_RE = /^(\[?\d{1,2}:\d{2}(?::\d{2})?\]?)\s*/

function classify(line: string): LineKind {
  if (/^---\s*\[container restarted/.test(line)) return 'restart'
  if (/^---\s*\[log gap:/.test(line)) return 'gap'
  if (/ERROR|Traceback|OOMKilled|error=|FAILED/.test(line)) return 'error'
  if (/结论：status=succeeded|accuracy=/.test(line)) return 'ok'
  if (TIME_RE.test(line)) return 'time'
  return 'plain'
}

const LINE_CLASS: Record<LineKind, string> = {
  plain: 'text-slate-300',
  time: 'text-slate-300',
  error: 'text-[#fca5a5]',
  restart: 'text-[#fcd34d]',
  gap: 'text-[#fca5a5]',
  ok: 'text-[#6ee7b7]',
}

/**
 * 日志面板（原型深色终端风，底 #0b1220，design web-and-delivery §4.3）：
 * 行首时间变暗；`--- [container restarted` 琥珀；`--- [log gap:` 红虚线；ERROR/Traceback 红。
 * 不在这里做虚拟滚动（P0 单次 ≤64KiB）；10k 行以上再上 useVirtualizer。
 */
export function LogViewer({
  text,
  loading = false,
  error = null,
  coverage,
  live = false,
  onRetry,
  hint,
  className,
}: {
  text: string
  loading?: boolean
  error?: ApiErrorBody | null
  coverage?: string | null
  live?: boolean
  onRetry?: () => void
  hint?: string
  className?: string
}) {
  const toast = useToast()
  const lines = useMemo(() => (text ? text.split('\n') : []), [text])

  const copyAll = async () => {
    const ok = await copyText(text)
    toast({ title: ok ? '已复制日志' : '复制失败，请手动选择文本', tone: ok ? 'ok' : 'err' })
  }

  return (
    <div className={cn('flex min-w-0 flex-col gap-2', className)}>
      <div className="flex flex-wrap items-center gap-2 text-2xs text-fg-3">
        {live ? (
          <span className="inline-flex items-center gap-1.5 text-success-text">
            <span className="live-dot" aria-hidden />
            追加中
          </span>
        ) : null}
        {coverage ? <span className="font-mono">覆盖区间 {coverage}</span> : null}
        {hint ? <span>{hint}</span> : null}
        {text ? (
          <Button
            size="sm"
            variant="ghost"
            className="ml-auto"
            icon={<Copy className="size-3" aria-hidden />}
            onClick={() => void copyAll()}
          >
            复制
          </Button>
        ) : null}
      </div>

      {error ? (
        <ErrorState error={error} onRetry={onRetry} compact />
      ) : (
        <div className="relative min-w-0 overflow-hidden rounded-md bg-terminal">
          {loading && lines.length === 0 ? (
            <div className="flex flex-col gap-2 p-4">
              {Array.from({ length: 6 }).map((_, i) => (
                <Skeleton key={i} className="h-3 bg-white/10" />
              ))}
            </div>
          ) : lines.length === 0 ? (
            <p className="px-4 py-6 text-center text-xs text-slate-400">还没有日志</p>
          ) : (
            <div
              className="max-h-[22rem] overflow-auto px-4 py-3 font-mono text-[11.5px] leading-[1.7]"
              role="log"
              aria-live="off"
            >
              {lines.map((line, index) => {
                const kind = classify(line)
                const match = TIME_RE.exec(line)
                return (
                  <div
                    key={index}
                    className={cn(
                      'whitespace-pre-wrap break-all',
                      LINE_CLASS[kind],
                      kind === 'restart' && 'border-l-[3px] border-warn pl-2 text-warn',
                      kind === 'gap' && 'my-1 rounded-sm border border-dashed border-danger px-2 py-0.5',
                    )}
                  >
                    {match ? (
                      <>
                        <span className="text-slate-500">{match[0].trim()}</span>
                        {line.slice(match[0].length)}
                      </>
                    ) : (
                      line
                    )}
                  </div>
                )
              })}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
