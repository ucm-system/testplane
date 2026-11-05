import { useEffect, useMemo, useRef, useState } from 'react'
import { ChevronDown, Loader2, Pause, ScrollText } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, DeployLogEntry, DeployLogs, DeployPod } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Banner, EmptyState, ErrorState } from '@/components/ui/states'
import { useFollowScroll } from '@/hooks/useFollowScroll'
import { LOG_POLL_INTERVAL_MS } from '@/hooks/useLogTail'
import { cn, formatClock } from '@/lib/utils'

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
 *
 * 运行中（`live`）按 `offset` 游标轮询、**追加**新 entry；Run 终态后停止轮询并标注
 * 「Run 已结束，日志不再更新」。轮询用递归 `setTimeout`，同一时刻只有一次在飞，
 * 切 Pod / 切页签 / 卸载都会清掉。
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
        <span
          key={`${entry.pod}-${entry.offset}-${index}`}
          data-testid="deploy-log-line"
          className={cn('block', lineClass(line))}
        >
          {line}
        </span>
      ))}
    </pre>
  )
}

/** 已消费到的行号（下一页 / 下一轮轮询的游标） */
function cursorOf(entries: DeployLogEntry[]): number {
  return entries.reduce((max, entry) => Math.max(max, entry.offset + entry.line_count), 0)
}

/** 追加去重：`pod + offset` 是这条 entry 的身份 */
function appendEntries(previous: DeployLogEntry[], next: DeployLogEntry[]): DeployLogEntry[] {
  const seen = new Set(previous.map((entry) => `${entry.pod}:${entry.offset}`))
  return [...previous, ...next.filter((entry) => !seen.has(`${entry.pod}:${entry.offset}`))]
}

interface DeployLogView {
  run_id: string
  entries: DeployLogEntry[]
  gaps: DeployLogs['gaps']
  truncated: boolean
  next_offset: number
}

function toView(data: DeployLogs): DeployLogView {
  return {
    run_id: data.run_id,
    entries: data.entries,
    gaps: data.gaps,
    truncated: data.truncated,
    next_offset: data.next_offset,
  }
}

export interface DeployLogsPanelProps {
  runId: string
  /** `GET /runs/{id}/deploy` 的 `deploy.state`：pods 为空时用它判断"排队中"还是"已回收" */
  deployState: string | null
  /** `GET /runs/{id}/deploy` 的 pods：有 Pod 才请求日志端点（没 pod 会 400） */
  pods: DeployPod[]
  /** Run 是否仍在运行：运行中才轮询（终态停止，避免长尾请求） */
  live?: boolean
}

export function DeployLogsPanel({ runId, deployState, pods, live = false }: DeployLogsPanelProps) {
  const [pod, setPod] = useState<string | null>(null)
  const [tailLatest, setTailLatest] = useState(false)
  const [data, setData] = useState<DeployLogView | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [moreError, setMoreError] = useState<string | null>(null)
  const [appended, setAppended] = useState(0)
  const [lastUpdatedAt, setLastUpdatedAt] = useState<number | null>(null)
  const [reloadToken, setReloadToken] = useState(0)

  const cursorRef = useRef(0)

  // 页签默认选第一个 Pod；`pods` 变了就回落到第一个
  const selectedPod = useMemo(() => {
    if (pods.length === 0) return null
    return pods.some((item) => item.name === pod) ? (pod as string) : (pods[0]?.name ?? null)
  }, [pods, pod])

  const follow = useFollowScroll(data?.entries.length ?? 0, Boolean(data?.entries.length))

  /* ------------------------------------------------------ 首屏：全量（或 tail） */
  useEffect(() => {
    if (!selectedPod) {
      setData(null)
      setLoading(false)
      setError(null)
      setAppended(0)
      setLastUpdatedAt(null)
      return
    }
    let alive = true
    cursorRef.current = 0
    setData(null)
    setError(null)
    setMoreError(null)
    setAppended(0)
    setLoading(true)
    api
      .getDeployLogs(runId, {
        pod: selectedPod,
        // tail_lines 与 offset 二选一：要看最新就不带 offset
        ...(tailLatest ? { tail_lines: 40 } : { offset: 0 }),
      })
      .then(
        (next) => {
          if (!alive) return
          cursorRef.current = cursorOf(next.entries)
          setData(toView(next))
          setLoading(false)
          setLastUpdatedAt(Date.now())
        },
        (e: unknown) => {
          if (!alive) return
          setError(toApiError(e))
          setLoading(false)
        },
      )
    return () => {
      alive = false
    }
  }, [runId, selectedPod, tailLatest, reloadToken])

  /* ------------------------------------------------------ 轮询：按游标追加 */
  useEffect(() => {
    if (!live || !selectedPod || tailLatest) return
    let alive = true
    let timer: ReturnType<typeof setTimeout> | null = null

    const tick = async () => {
      try {
        const next = await api.getDeployLogs(runId, { pod: selectedPod, offset: cursorRef.current })
        if (!alive) return
        cursorRef.current = Math.max(cursorRef.current, cursorOf(next.entries))
        setData((previous) =>
          previous
            ? {
                ...previous,
                entries: appendEntries(previous.entries, next.entries),
                gaps: next.gaps,
                truncated: next.truncated,
                next_offset: next.next_offset,
              }
            : toView(next),
        )
        setAppended((count) => count + next.entries.reduce((sum, entry) => sum + entry.line_count, 0))
        setLastUpdatedAt(Date.now())
        setError(null)
      } catch (e) {
        if (!alive) return
        setError(toApiError(e))
      } finally {
        if (alive) timer = setTimeout(() => void tick(), LOG_POLL_INTERVAL_MS)
      }
    }

    timer = setTimeout(() => void tick(), LOG_POLL_INTERVAL_MS)
    return () => {
      alive = false
      if (timer !== null) clearTimeout(timer)
    }
  }, [runId, selectedPod, tailLatest, live, reloadToken])

  const switchPod = (next: string) => {
    setPod(next)
  }

  const loadMore = async () => {
    if (!selectedPod) return
    setLoadingMore(true)
    setMoreError(null)
    try {
      const next = await api.getDeployLogs(runId, { pod: selectedPod, offset: cursorRef.current })
      cursorRef.current = Math.max(cursorRef.current, cursorOf(next.entries))
      setData((previous) =>
        previous
          ? {
              ...previous,
              entries: appendEntries(previous.entries, next.entries),
              gaps: next.gaps,
              truncated: next.truncated,
              next_offset: next.next_offset,
            }
          : toView(next),
      )
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

  if (error && !data) return <ErrorState error={error} onRetry={() => setReloadToken((token) => token + 1)} compact />
  if (loading && !data) {
    return (
      <p className="flex items-center gap-2 px-5 py-4 text-xs text-fg-3">
        <Loader2 className="size-3.5 animate-spin" aria-hidden />
        加载 Deploy 日志…
      </p>
    )
  }
  if (!data) return null

  const gaps = data.gaps.filter((gap) => gap.pod === selectedPod)
  const lineCount = data.entries.reduce((sum, entry) => sum + entry.line_count, 0)

  return (
    <div className="flex flex-col gap-3 px-5 py-4">
      {error ? (
        <p role="alert" className="rounded-md border border-danger/25 bg-danger-weak px-2.5 py-1.5 text-2xs text-fg">
          {error.code}：{error.message}（已收到的日志保留，下一轮会自动重试）
        </p>
      ) : null}
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
            }}
            className="size-3.5 rounded-sm border-line"
          />
          {/* tail 是一次性快照：勾上就暂停自动刷新，取消勾选恢复跟随 */}
          先看最新（tail 40 行）
        </label>
      </div>

      <div className="overflow-hidden rounded-lg border border-line" style={{ background: TERMINAL_BG }}>
        <div className="flex flex-wrap items-center gap-2 border-b border-white/10 px-3 py-2 text-2xs text-[#94a3b8]">
          <ScrollText className="size-3.5" aria-hidden />
          <span className="font-mono">deploy/logs/{selectedPod}.log</span>
          <span className="font-mono" data-testid="deploy-log-last-updated">
            最后更新 {lastUpdatedAt == null ? '—' : formatClock(new Date(lastUpdatedAt).toISOString())}
          </span>
          {appended > 0 ? <span data-testid="deploy-log-appended">已追加 {appended} 行</span> : null}
          {!live ? <span data-testid="deploy-log-ended">Run 已结束，日志不再更新</span> : null}
          {tailLatest ? <span>先看最新一屏，已暂停自动刷新</span> : null}
          <span className="ml-auto">
            {lineCount} 行
            {data.truncated ? ' · 还有更多' : ''}
          </span>
          {lineCount > 0 ? (
            <label className="inline-flex items-center gap-1.5">
              <input
                type="checkbox"
                data-testid="deploy-log-follow-toggle"
                checked={follow.following}
                onChange={(event) => follow.setFollowing(event.target.checked)}
                className="size-3.5 rounded-sm border-white/20"
              />
              跟随最新
            </label>
          ) : null}
        </div>

        {!follow.following && lineCount > 0 ? (
          <div className="flex items-center gap-2 border-b border-warn/30 bg-warn-weak px-3 py-1.5 text-2xs">
            <Pause className="size-3 text-warn-text" aria-hidden />
            <span className="text-warn-text">已暂停跟随（新日志仍会继续追加）</span>
            <Button
              size="sm"
              variant="ghost"
              data-testid="deploy-log-follow-paused"
              className="ml-auto"
              onClick={follow.resume}
            >
              已暂停跟随 · 回到底部
            </Button>
          </div>
        ) : null}

        <div ref={follow.ref} onScroll={follow.onScroll} data-testid="deploy-log-scroller" className="max-h-[24rem] overflow-auto px-3 py-2 pb-3">
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
