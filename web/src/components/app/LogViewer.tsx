import { useMemo } from 'react'
import { Copy, Pause } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ErrorState } from '@/components/ui/states'
import { Skeleton } from '@/components/ui/skeleton'
import { useToast } from '@/components/ui/toast'
import type { ApiErrorBody } from '@/api/types'
import { useFollowScroll } from '@/hooks/useFollowScroll'
import { cn, copyText, formatClock } from '@/lib/utils'

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

/** 毫秒时间戳 → `HH:mm:ss`（“最后更新”用） */
function clockOf(timestamp: number | null | undefined): string {
  if (timestamp == null) return '—'
  return formatClock(new Date(timestamp).toISOString())
}

/**
 * 日志面板（原型深色终端风，底 #0b1220，design web-and-delivery §4.3）：
 * 行首时间变暗；`--- [container restarted` 琥珀；`--- [log gap:` 红虚线；ERROR/Traceback 红。
 *
 * 运行中的面板由调用方的 `useLogTail` 按游标**追加**文本（不是整块替换）；
 * 这里只负责：跟随最新 / 上滚暂停 / 状态行（最后更新、已追加行数、Run 已结束）。
 * 不在这里做虚拟滚动（P0 单次 ≤64KiB）；10k 行以上再上 Virtualizer。
 */
export function LogViewer({
  text,
  loading = false,
  error = null,
  coverage,
  live = false,
  ended = false,
  lastUpdatedAt = null,
  appended = 0,
  onRetry,
  hint,
  className,
  testIdPrefix = 'log',
}: {
  text: string
  loading?: boolean
  error?: ApiErrorBody | null
  coverage?: string | null
  /** 正在追加（Attempt / Run 都还没结束） */
  live?: boolean
  /** Run 已到终态：明确告诉用户日志不再更新 */
  ended?: boolean
  lastUpdatedAt?: number | null
  appended?: number
  onRetry?: () => void
  hint?: string
  className?: string
  /** 同页可能有多个日志面板（Deploy / Attempt），测试钩子用前缀区分 */
  testIdPrefix?: string
}) {
  const toast = useToast()
  const lines = useMemo(() => (text ? text.split('\n') : []), [text])
  const follow = useFollowScroll(text, lines.length > 0)

  const copyAll = async () => {
    const ok = await copyText(text)
    toast({ title: ok ? '已复制日志' : '复制失败，请手动选择文本', tone: ok ? 'ok' : 'err' })
  }

  return (
    <div className={cn('flex min-w-0 flex-col gap-2', className)}>
      <div className="flex flex-wrap items-center gap-2 text-2xs text-fg-3">
        {live ? (
          <span data-testid={`${testIdPrefix}-live`} className="inline-flex items-center gap-1.5 text-success-text">
            <span className="live-dot" aria-hidden />
            追加中
          </span>
        ) : null}
        {ended ? <span data-testid={`${testIdPrefix}-ended`}>Run 已结束，日志不再更新</span> : null}
        {coverage ? <span className="font-mono">覆盖区间 {coverage}</span> : null}
        {hint ? <span>{hint}</span> : null}
        <span data-testid={`${testIdPrefix}-last-updated`} className="font-mono">
          最后更新 {clockOf(lastUpdatedAt)}
        </span>
        {appended > 0 ? <span data-testid={`${testIdPrefix}-appended`}>已追加 {appended} 行</span> : null}
        {lines.length > 0 ? (
          <label className="ml-auto inline-flex items-center gap-1.5 text-2xs text-fg-2">
            <input
              type="checkbox"
              data-testid={`${testIdPrefix}-follow-toggle`}
              checked={follow.following}
              onChange={(event) => follow.setFollowing(event.target.checked)}
              className="size-3.5 rounded-sm border-line"
            />
            跟随最新
          </label>
        ) : null}
        {text ? (
          <Button
            size="sm"
            variant="ghost"
            className={cn(lines.length === 0 && 'ml-auto')}
            icon={<Copy className="size-3" aria-hidden />}
            onClick={() => void copyAll()}
          >
            复制
          </Button>
        ) : null}
      </div>

      {/* 用户上滚 → 自动暂停跟随；点这条回到最新并恢复跟随 */}
      {!follow.following && lines.length > 0 ? (
        <div className="flex items-center gap-2 rounded-md border border-warn/30 bg-warn-weak px-2.5 py-1.5 text-2xs">
          <Pause className="size-3 text-warn-text" aria-hidden />
          <span className="text-warn-text">新内容仍在追加，当前不会自动滚动</span>
          <Button
            size="sm"
            variant="ghost"
            data-testid={`${testIdPrefix}-follow-paused`}
            className="ml-auto"
            onClick={follow.resume}
          >
            已暂停跟随 · 回到底部
          </Button>
        </div>
      ) : null}

      {error && lines.length === 0 ? (
        <ErrorState error={error} onRetry={onRetry} compact />
      ) : (
        <>
          {error ? (
            <p role="alert" className="rounded-md border border-danger/25 bg-danger-weak px-2.5 py-1.5 text-2xs text-fg">
              {error.code}：{error.message}（已收到的日志保留，下一轮会自动重试）
            </p>
          ) : null}
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
              ref={follow.ref}
              onScroll={follow.onScroll}
              data-testid={`${testIdPrefix}-scroller`}
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
                    data-testid={`${testIdPrefix}-line`}
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
        </>
      )}
    </div>
  )
}
