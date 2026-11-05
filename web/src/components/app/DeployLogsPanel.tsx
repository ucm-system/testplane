import { useMemo, useState } from 'react'
import { ChevronDown, Loader2, ScrollText } from 'lucide-react'
import { api } from '@/api'
import type { DeployLogEntry, DeployLogs } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Banner, ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { cn } from '@/lib/utils'

/**
 * Deploy 日志面板（T29/T31，契约 `GET /runs/{id}/deploy/logs`）。
 *
 * 三条**必须守住**的语义：
 *  1. 一个 Deploy 可能多个 Pod：**按 Pod 分页签，不合并**；
 *  2. `gaps[]` 是事实缺口（Pod 重启 / 晚于 accepted_at 启动），要在时间轴上**显式插一条**
 *     「此处不连续」，**不许假装日志是连续的**；
 *  3. 排队期没有 Pod：后端返回 `pods: []` + `message`，这里原文显示，不当作错误。
 *
 * 视觉照原型：深色终端（`#0b1220`），时间戳弱化、错误红、重启/告警琥珀。
 */

const TERMINAL_BG = '#0b1220'

/** 深色终端里按行着色：错误红、告警/重启琥珀、成功绿，其余保持浅灰。 */
function lineClass(line: string): string {
  if (/\b(ERROR|FATAL|panic|OOMKilled)\b/i.test(line)) return 'text-[#fca5a5]'
  if (/\b(WARN|WARNING|restart|timeout|retry)\b/i.test(line)) return 'text-[#fcd34d]'
  if (/\b(ready|listening|succeeded|recovered)\b/i.test(line)) return 'text-[#6ee7b7]'
  if (/^\d{2}:\d{2}:\d{2}/.test(line)) return 'text-[#cbd5e1]'
  return 'text-[#94a3b8]'
}

/** gap 的时间戳只显示到秒，省得一行被塞满。 */
function formatGapTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleTimeString('zh-CN', { hour12: false })
}

function LogBlock({ entry }: { entry: DeployLogEntry }) {
  return (
    <pre className="whitespace-pre-wrap break-all font-mono text-[12.5px] leading-[1.7]">
      {entry.text.split('\n').map((line, index) => (
        <span key={`${entry.pod}-${entry.offset}-${index}`} className={cn('block', lineClass(line))}>
          {line}
        </span>
      ))}
    </pre>
  )
}

export function DeployLogsPanel({ runId }: { runId: string }) {
  const [pod, setPod] = useState<string | undefined>(undefined)
  const [tailLatest, setTailLatest] = useState(false)
  const [extra, setExtra] = useState<DeployLogs | null>(null)
  const [offset, setOffset] = useState(0)
  const [loadingMore, setLoadingMore] = useState(false)
  const [moreError, setMoreError] = useState<string | null>(null)

  const key = `${pod ?? 'all'}|${tailLatest ? 'tail' : 'head'}`
  const base = useAsync(
    () =>
      api.getDeployLogs(runId, {
        pod,
        ...(tailLatest ? { tail_lines: 40 } : {}),
      }),
    [runId, key],
  )

  // 换页签 / 换模式时清掉"加载更多"的累积
  const data: DeployLogs | null = useMemo(() => {
    if (extra && extra.run_id === base.data?.run_id) {
      return {
        ...base.data,
        entries: [...(base.data?.entries ?? []), ...extra.entries],
        gaps: [...(base.data?.gaps ?? []), ...extra.gaps],
        truncated: extra.truncated,
        next_offset: extra.next_offset,
      }
    }
    return base.data
  }, [base.data, extra])

  const loadMore = async () => {
    if (!data) return
    setLoadingMore(true)
    setMoreError(null)
    try {
      const next = await api.getDeployLogs(runId, {
        pod,
        offset: data.next_offset,
        ...(tailLatest ? { tail_lines: 40 } : {}),
      })
      setOffset(data.next_offset)
      setExtra((prev) => ({
        ...next,
        entries: [...(prev?.entries ?? []), ...next.entries],
        gaps: [...(prev?.gaps ?? []), ...next.gaps],
      }))
    } catch (e) {
      setMoreError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoadingMore(false)
    }
  }

  const switchPod = (next: string | undefined) => {
    setPod(next)
    setExtra(null)
    setOffset(0)
  }

  if (base.error) return <ErrorState error={base.error} onRetry={base.reload} compact />
  if (base.loading && !data) {
    return (
      <p className="flex items-center gap-2 px-5 py-4 text-xs text-fg-3">
        <Loader2 className="size-3.5 animate-spin" aria-hidden />
        加载 Deploy 日志…
      </p>
    )
  }
  if (!data) return null

  // 排队期 / 无 Pod：原文显示后端 message，不当错误
  if (data.pods.length === 0) {
    return (
      <div className="px-5 py-4">
        <Banner tone="warn" title="还没有 Pod 日志">
          {data.message ?? '当前没有可用的 Pod，因此没有日志正文。'}
        </Banner>
      </div>
    )
  }

  const selected = data.entries.filter((entry) => !pod || entry.pod === pod)
  const gaps = data.gaps.filter((gap) => !pod || gap.pod === pod)

  return (
    <div className="flex flex-col gap-3 px-5 py-4">
      <div className="flex flex-wrap items-center gap-2">
        {/* 多 Pod（多节点 / PD 分离）按 Pod 分页签，不合并 */}
        <div role="tablist" aria-label="Pod 页签" className="flex flex-wrap items-center gap-1">
          <button
            type="button"
            role="tab"
            aria-selected={pod === undefined}
            onClick={() => switchPod(undefined)}
            className={cn(
              'h-7 rounded-md px-2.5 text-xs transition-colors duration-150',
              pod === undefined ? 'bg-surface font-medium text-fg shadow-card' : 'text-fg-2 hover:bg-surface-2',
            )}
          >
            全部 Pod
          </button>
          {data.pods.map((item) => (
            <button
              key={item.name}
              type="button"
              role="tab"
              aria-selected={pod === item.name}
              onClick={() => switchPod(item.name)}
              className={cn(
                'flex h-7 items-center gap-1.5 rounded-md px-2.5 font-mono text-xs transition-colors duration-150',
                pod === item.name ? 'bg-surface font-medium text-fg shadow-card' : 'text-fg-2 hover:bg-surface-2',
              )}
              title={`${item.name}${item.restarts > 0 ? ` · 重启 ${item.restarts} 次` : ''}`}
            >
              {item.name}
              <span
                className={cn('size-1.5 rounded-full', item.ready ? 'bg-success' : 'bg-fg-3')}
                aria-label={item.ready ? '就绪' : '未就绪'}
              />
              {item.restarts > 0 ? <Badge tone="warn">重启 {item.restarts}</Badge> : null}
            </button>
          ))}
        </div>

        <label className="ml-auto flex items-center gap-1.5 text-2xs text-fg-2">
          <input
            type="checkbox"
            checked={tailLatest}
            onChange={(e) => {
              setTailLatest(e.target.checked)
              setExtra(null)
              setOffset(0)
            }}
            className="size-3.5 rounded-sm border-line"
          />
          先看最新（tail 40 行）
        </label>
      </div>

      <div className="overflow-hidden rounded-lg border border-line" style={{ background: TERMINAL_BG }}>
        <div className="flex items-center gap-2 border-b border-white/10 px-3 py-2 text-2xs text-[#94a3b8]">
          <ScrollText className="size-3.5" aria-hidden />
          <span className="font-mono">deploy/logs/{(pod ?? '*.log').replace('$', '')}</span>
          <span className="ml-auto">
            {selected.reduce((sum, entry) => sum + entry.line_count, 0)} 行
            {data.truncated ? ' · 还有更多' : ''}
          </span>
        </div>

        <div className="max-h-[24rem] overflow-auto px-3 py-2 pb-3">
          {selected.length === 0 ? (
            <p className="py-6 text-center font-mono text-xs text-[#94a3b8]">（这段区间没有日志行）</p>
          ) : (
            (() => {
              // gap 按 Pod 归属挂到对应 Pod 的那段日志后面（不按数组下标乱配）
              const pending = new Map<string, typeof gaps>()
              for (const gap of gaps) pending.set(gap.pod, [...(pending.get(gap.pod) ?? []), gap])
              return selected.map((entry, index) => {
                const entryGaps = pending.get(entry.pod) ?? []
                if (entryGaps.length > 0) pending.set(entry.pod, [])
                return (
                  <div key={`${entry.pod}-${entry.offset}`}>
                    {index > 0 || pod ? (
                      <p className="mb-1 font-mono text-[11px] text-[#64748b]">
                        ── {entry.pod} · {entry.container} · 从第 {entry.offset} 行 ──
                      </p>
                    ) : null}
                    <LogBlock entry={entry} />
                    {/* 事实缺口：显式插一条，不假装连续 */}
                    {entryGaps.map((gap) => (
                      <p
                        key={`${gap.pod}-${gap.from}`}
                        className="my-2 rounded-sm border border-dashed border-[#f59e0b]/60 bg-[#f59e0b]/10 px-2 py-1 font-mono text-[11.5px] text-[#fcd34d]"
                      >
                        ⚠ 此处不连续：{gap.reason}（{formatGapTime(gap.from)} → {formatGapTime(gap.to)}，Pod {gap.pod}）
                      </p>
                    ))}
                  </div>
                )
              })
            })()
          )}
        </div>

        {data.truncated ? (
          <div className="flex items-center justify-end gap-2 border-t border-white/10 px-3 py-2">
            {moreError ? <span className="mr-auto text-2xs text-[#fca5a5]">{moreError}</span> : null}
            <Button
              size="sm"
              variant="ghost"
              className="text-[#cbd5e1] hover:bg-white/10 hover:text-white"
              loading={loadingMore}
              onClick={() => void loadMore()}
            >
              加载更多 <ChevronDown className="size-3" aria-hidden />
            </Button>
          </div>
        ) : offset > 0 ? (
          <div className="border-t border-white/10 px-3 py-2 text-right text-2xs text-[#64748b]">已到末尾</div>
        ) : null}
      </div>

      {gaps.length === 0 && data.entries.length > 0 ? (
        <p className="text-2xs text-fg-3">这段区间没有缺口（Pod 未重启）。有缺口时这里会显式标出来。</p>
      ) : null}
    </div>
  )
}
