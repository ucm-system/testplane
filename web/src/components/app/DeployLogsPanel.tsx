import { useMemo, useState } from 'react'
import { ChevronDown, Loader2, ScrollText } from 'lucide-react'
import { api } from '@/api'
import type { DeployLogEntry, DeployLogs, DeployPod } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Banner, EmptyState, ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { cn } from '@/lib/utils'

/**
 * Deploy 日志面板（契约：`GET /runs/{id}/deploy/logs`）。
 *
 * **`?pod=` 必选**（缺了服务端 400，不存在的 pod 404），`offset` 只作用于该 Pod 的日志流，
 * `tail_lines` 与 `offset` 二选一；`pods[]` / `gaps[]` 每次返回全部 Pod（用来渲染页签）。
 *
 * 三条必须守住的语义：
 *  1. 一个 Deploy 可能多个 Pod：**按 Pod 分页签，不合并**；
 *  2. `gaps[]` 是事实缺口（Pod 重启 / 回收），要在时间轴上**显式插一条**「此处不连续」；
 *  3. `pods=[]` 时**不发日志请求**（没 pod 会 400）：排队中就说排队，已回收就说回收。
 */

const TERMINAL_BG = '#0b1220'

/** 深色终端里按行着色：错误红、告警/重启琥珀、成功绿，时间戳稍亮。 */
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

export interface DeployLogsPanelProps {
  runId: string
  /** `GET /runs/{id}/deploy` 的 `deploy.state`：pods 为空时用它判断"排队中"还是"已回收" */
  deployState: string | null
  /** `GET /runs/{id}/deploy` 的 pods：有 Pod 才请求日志端点（没 pod 会 400） */
  pods: DeployPod[]
}

export function DeployLogsPanel({ runId, deployState, pods }: DeployLogsPanelProps) {
  const [pod, setPod] = useState<string | null>(null)
  const [tailLatest, setTailLatest] = useState(false)
  const [extra, setExtra] = useState<DeployLogs | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [moreError, setMoreError] = useState<string | null>(null)

  // 页签默认选第一个 Pod；`pods` 变了就回落到第一个
  const selectedPod = useMemo(() => {
    if (pods.length === 0) return null
    return pods.some((item) => item.name === pod) ? (pod as string) : (pods[0]?.name ?? null)
  }, [pods, pod])

  const base = useAsync(
    async () => {
      // pods 为空：**不请求**（缺 pod 参数服务端会 400），直接由 deploy 状态说明
      if (!selectedPod) return null
      return api.getDeployLogs(runId, {
        pod: selectedPod,
        // tail_lines 与 offset 二选一：要看最新就不带 offset
        ...(tailLatest ? { tail_lines: 40 } : { offset: 0 }),
      })
    },
    [runId, selectedPod, tailLatest],
  )

  const data: DeployLogs | null = useMemo(() => {
    if (extra && base.data && extra.run_id === base.data.run_id) {
      return {
        ...base.data,
        entries: [...base.data.entries, ...extra.entries],
        gaps: base.data.gaps,
        truncated: extra.truncated,
        next_offset: extra.next_offset,
      }
    }
    return base.data
  }, [base.data, extra])

  const switchPod = (next: string) => {
    setPod(next)
    setExtra(null)
  }

  const loadMore = async () => {
    if (!data || !selectedPod) return
    setLoadingMore(true)
    setMoreError(null)
    try {
      const next = await api.getDeployLogs(runId, { pod: selectedPod, offset: data.next_offset })
      setExtra((prev) => ({
        ...next,
        entries: [...(prev?.entries ?? []), ...next.entries],
      }))
    } catch (e) {
      setMoreError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoadingMore(false)
    }
  }

  /* 没有 Pod：不发请求，按 deploy 状态说清楚为什么没有日志 */
  if (pods.length === 0) {
    const queued = deployState === 'queued' || deployState === 'accepted' || deployState === 'deploying'
    return (
      <div className="px-5 py-4">
        {queued ? (
          <Banner tone="warn" title="尚未产生 Pod（排队中）">
            部署还在 {deployState} 阶段，Pod 起来后这里会按 Pod 分页签显示日志。
          </Banner>
        ) : (
          <EmptyState
            title="Pod 已回收，没有可读日志"
            description="日志随 Pod 一起消失（MVP 未落盘）；缺口与回收原因由后端在 gaps 里说明。"
          />
        )}
      </div>
    )
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

  const gaps = data.gaps.filter((gap) => gap.pod === selectedPod)

  return (
    <div className="flex flex-col gap-3 px-5 py-4">
      <div className="flex flex-wrap items-center gap-2">
        {/* 多 Pod（多节点 / PD 分离）按 Pod 分页签，不合并；切页签 = 换 ?pod= */}
        <div role="tablist" aria-label="Pod 页签" className="flex flex-wrap items-center gap-1">
          {pods.map((item) => (
            <button
              key={item.name}
              type="button"
              role="tab"
              aria-selected={selectedPod === item.name}
              data-testid="deploy-log-pod-tab"
              onClick={() => switchPod(item.name)}
              className={cn(
                'flex h-7 items-center gap-1.5 rounded-md px-2.5 font-mono text-xs transition-colors duration-150',
                selectedPod === item.name
                  ? 'bg-surface font-medium text-fg shadow-card'
                  : 'text-fg-2 hover:bg-surface-2',
              )}
              title={`${item.name}${(item.restarts ?? 0) > 0 ? ` · 重启 ${item.restarts} 次` : ''}`}
            >
              {item.name}
              <span
                className={cn('size-1.5 rounded-full', item.ready ? 'bg-success' : 'bg-fg-3')}
                aria-label={item.ready ? '就绪' : '未就绪'}
              />
              {(item.restarts ?? 0) > 0 ? <Badge tone="warn">重启 {item.restarts}</Badge> : null}
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
            }}
            className="size-3.5 rounded-sm border-line"
          />
          先看最新（tail 40 行）
        </label>
      </div>

      <div className="overflow-hidden rounded-lg border border-line" style={{ background: TERMINAL_BG }}>
        <div className="flex items-center gap-2 border-b border-white/10 px-3 py-2 text-2xs text-[#94a3b8]">
          <ScrollText className="size-3.5" aria-hidden />
          <span className="font-mono">deploy/logs/{selectedPod}.log</span>
          <span className="ml-auto">
            {data.entries.reduce((sum, entry) => sum + entry.line_count, 0)} 行
            {data.truncated ? ' · 还有更多' : ''}
          </span>
        </div>

        <div className="max-h-[24rem] overflow-auto px-3 py-2 pb-3">
          {data.entries.length === 0 ? (
            <p className="py-6 text-center font-mono text-xs text-[#94a3b8]">（这段区间没有日志行）</p>
          ) : (
            data.entries.map((entry, index) => (
              <div key={`${entry.pod}-${entry.offset}`}>
                {index > 0 ? (
                  <p className="mb-1 font-mono text-[11px] text-[#64748b]">
                    ── {entry.pod} · {entry.container} · 从第 {entry.offset} 行 ──
                  </p>
                ) : null}
                <LogBlock entry={entry} />
              </div>
            ))
          )}
          {/* 事实缺口：显式插一条，不假装连续 */}
          {gaps.map((gap) => (
            <p
              key={`${gap.pod}-${gap.from}`}
              className="my-2 rounded-sm border border-dashed border-[#f59e0b]/60 bg-[#f59e0b]/10 px-2 py-1 font-mono text-[11.5px] text-[#fcd34d]"
            >
              ⚠ 此处不连续：{gap.reason}（{formatGapTime(gap.from)} → {formatGapTime(gap.to)}，Pod {gap.pod}）
            </p>
          ))}
        </div>

        {data.truncated ? (
          <div className="flex items-center justify-end gap-2 border-t border-white/10 px-3 py-2">
            {moreError ? <span className="mr-auto text-2xs text-[#fca5a5]">{moreError}</span> : null}
            <Button
              size="sm"
              variant="ghost"
              data-testid="deploy-log-load-more"
              className="text-[#cbd5e1] hover:bg-white/10 hover:text-white"
              loading={loadingMore}
              onClick={() => void loadMore()}
            >
              加载更多 <ChevronDown className="size-3" aria-hidden />
            </Button>
          </div>
        ) : null}
      </div>

      {gaps.length === 0 ? (
        <p className="text-2xs text-fg-3">这段区间没有缺口（Pod 未重启）。有缺口时这里会显式标出来。</p>
      ) : null}
    </div>
  )
}
