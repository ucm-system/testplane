import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { Search, X } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, RunSummary } from '@/api/types'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { RunStatusBadge } from '@/components/app/StatusBadge'
import { Button } from '@/components/ui/button'
import { Card, CardHeader } from '@/components/ui/card'
import { Input, Select } from '@/components/ui/input'
import { Segmented } from '@/components/ui/segmented'
import { TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { TERMINAL_LEGEND, isTerminalRun } from '@/lib/status'
import { formatDateTime, formatDuration, formatRelative, runDuration } from '@/lib/utils'

type StatusFilter = 'all' | 'active' | 'terminal' | 'failed'
type TimeFilter = 'all' | '1h' | '24h' | '7d'

const STATUS_QUERY: Record<StatusFilter, string | undefined> = {
  all: undefined,
  active: 'submitted,queued,deploying,testing,cleanup,cancelling',
  terminal: 'succeeded,completed_with_errors,service_unavailable,failed,canceled',
  failed: 'completed_with_errors,service_unavailable,failed',
}

const TIME_MS: Record<TimeFilter, number | null> = {
  all: null,
  '1h': 3600_000,
  '24h': 24 * 3600_000,
  '7d': 7 * 24 * 3600_000,
}

const STATUS_VALUES: StatusFilter[] = ['all', 'active', 'terminal', 'failed']
const TIME_VALUES: TimeFilter[] = ['all', '1h', '24h', '7d']

const PAGE_SIZE = 10

function progress(run: RunSummary): string {
  const counts = run.task_counts
  if (!counts) return '—'
  return `${counts.succeeded} / ${counts.total}`
}

function pickOne<T extends string>(value: string | null, allowed: readonly T[], fallback: T): T {
  return allowed.includes((value ?? fallback) as T) ? ((value ?? fallback) as T) : fallback
}

/**
 * 运行列表：筛选条件**双向同步进 URL**（B9）。
 *
 * `q / status / kind / tester / submitter`（外加 `time`）都从 `useSearchParams` 读，
 * 改筛选就写回 URL —— 直接打开 `/runs?q=xxx`、刷新、把链接发给别人，看到的都是同一份结果。
 */
export function RunsPage() {
  const [params, setParams] = useSearchParams()

  const statusFilter = pickOne<StatusFilter>(params.get('status'), STATUS_VALUES, 'all')
  const timeFilter = pickOne<TimeFilter>(params.get('time'), TIME_VALUES, 'all')
  const search = params.get('q') ?? ''
  const kind = params.get('kind') ?? ''
  const tester = params.get('tester') ?? ''
  const submitter = params.get('submitter') ?? ''

  /** 改任意筛选参数都走这里：空值即从 URL 删掉该键（`replace` 免得每敲一个字都进历史） */
  const updateQuery = useCallback(
    (patch: Record<string, string | null | undefined>) => {
      const next = new URLSearchParams(params)
      for (const [key, value] of Object.entries(patch)) {
        if (value == null || value === '') next.delete(key)
        else next.set(key, value)
      }
      setParams(next, { replace: true })
    },
    [params, setParams],
  )

  const [items, setItems] = useState<RunSummary[]>([])
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const requestId = useRef(0)

  const loadFirstPage = useCallback(async () => {
    const id = ++requestId.current
    setLoading(true)
    setError(null)
    try {
      const page = await api.listRuns({
        status: STATUS_QUERY[statusFilter],
        kind: kind || undefined,
        tester: tester || undefined,
        submitter: submitter || undefined,
        limit: PAGE_SIZE,
      })
      if (id !== requestId.current) return
      setItems(page.items)
      setNextCursor(page.next_cursor)
    } catch (e) {
      if (id !== requestId.current) return
      setError(toApiError(e))
      setItems([])
      setNextCursor(null)
    } finally {
      if (id === requestId.current) setLoading(false)
    }
  }, [statusFilter, kind, tester, submitter])

  useEffect(() => {
    void loadFirstPage()
  }, [loadFirstPage])

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return
    setLoadingMore(true)
    try {
      const page = await api.listRuns({
        status: STATUS_QUERY[statusFilter],
        kind: kind || undefined,
        tester: tester || undefined,
        submitter: submitter || undefined,
        cursor: nextCursor,
        limit: PAGE_SIZE,
      })
      setItems((prev) => [...prev, ...page.items])
      setNextCursor(page.next_cursor)
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setLoadingMore(false)
    }
  }

  /** 时间与关键字过滤都在已加载的页上做：服务端 P0 只有 status/kind/submitter 过滤（07 §5.1）。 */
  const visible = useMemo(() => {
    const keyword = search.trim().toLowerCase()
    const windowMs = TIME_MS[timeFilter]
    return items.filter((run) => {
      if (keyword && !`${run.id} ${run.name} ${run.submitted_by}`.toLowerCase().includes(keyword)) return false
      if (windowMs != null) {
        const submitted = new Date(run.submitted_at).getTime()
        if (Number.isNaN(submitted) || Date.now() - submitted > windowMs) return false
      }
      return true
    })
  }, [items, search, timeFilter])

  const activeCount = items.filter((run) => !isTerminalRun(run.status)).length
  const advanced = [
    kind ? { key: 'kind', label: `类型：${kind}` } : null,
    tester ? { key: 'tester', label: `Tester：${tester}` } : null,
    submitter ? { key: 'submitter', label: `提交人：${submitter}` } : null,
  ].filter(Boolean) as Array<{ key: string; label: string }>

  return (
    <>
      <RightRail>
        <RailCard title="终态语义" tone="info">
          <ul className="flex flex-col gap-2">
            {TERMINAL_LEGEND.map((item) => (
              <li key={item.status} className="flex items-start gap-2">
                <RunStatusBadge status={item.status} />
                <span className="min-w-0 flex-1 text-2xs leading-relaxed text-fg-2">{item.desc}</span>
              </li>
            ))}
          </ul>
          <p className="mt-3 border-t border-line-soft pt-3 text-2xs leading-relaxed text-fg-3">
            跳过 = 没跑，失败 = 跑了但没通过，两者不混用。
          </p>
        </RailCard>
        <RailCard title="筛选与刷新" tone="muted">
          <p>状态由服务端筛；时间与关键字只作用在已经加载出来的这些行上。</p>
          <p className="mt-2">列表不会自己刷新：切页、改筛选或重新打开这一页，拿到的才是最新。</p>
          <p className="mt-2">一次加载一页，点「加载更多」才取下一页；筛选条件都在地址栏里，复制链接给别人看到的和你一样。</p>
        </RailCard>
      </RightRail>

      <Card>
        <CardHeader
          tone="ok"
          title="全部 Run"
          sub={loading ? '加载中…' : `已加载 ${items.length} 个 · 进行中 ${activeCount} 个`}
          actions={
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-fg-3" aria-hidden />
                <Input
                  value={search}
                  onChange={(e) => updateQuery({ q: e.target.value })}
                  placeholder="筛选本页 Run（按名称 / ID）"
                  aria-label="筛选本页 Run（按名称 / ID）"
                  title="只过滤已加载的这一页，不是全局搜索"
                  className="h-9 w-56 pl-8"
                />
              </div>
              <Segmented<StatusFilter>
                ariaLabel="状态过滤"
                value={statusFilter}
                onChange={(next) => updateQuery({ status: next === 'all' ? null : next })}
                options={[
                  { value: 'all', label: '全部' },
                  { value: 'active', label: '进行中' },
                  { value: 'failed', label: '有失败' },
                  { value: 'terminal', label: '已结束' },
                ]}
              />
              <Select
                aria-label="时间范围"
                value={timeFilter}
                onChange={(e) => updateQuery({ time: e.target.value === 'all' ? null : e.target.value })}
                className="h-9 w-28"
              >
                <option value="all">全部时间</option>
                <option value="1h">近 1 小时</option>
                <option value="24h">近 24 小时</option>
                <option value="7d">近 7 天</option>
              </Select>
            </div>
          }
        />

        {advanced.length > 0 ? (
          <div className="flex flex-wrap items-center gap-2 border-b border-line-soft bg-surface-2 px-5 py-2 text-2xs text-fg-3">
            <span>链接带来的筛选：</span>
            {advanced.map((item) => (
              <button
                key={item.key}
                type="button"
                onClick={() => updateQuery({ [item.key]: null })}
                aria-label={`清除 ${item.key} 过滤`}
                className="inline-flex items-center gap-1 rounded-full border border-line bg-surface px-2 py-0.5 text-fg-2 transition-colors hover:border-line-strong hover:text-fg"
              >
                <span className="font-mono">{item.label}</span>
                <X className="size-3" aria-hidden />
              </button>
            ))}
          </div>
        ) : null}

        {error ? <div className="p-4"><ErrorState error={error} onRetry={() => void loadFirstPage()} /></div> : null}

        {loading && items.length === 0 ? (
          <TableSkeleton rows={6} cols={7} />
        ) : visible.length === 0 ? (
          <EmptyState
            title={items.length === 0 ? '没有符合条件的 Run' : '当前过滤条件下没有 Run'}
            description={
              items.length === 0
                ? '换个状态过滤，或去提交一个新的 Run。'
                : '时间范围或关键字太窄，放宽一点再看。'
            }
            action={
              <Button
                size="sm"
                onClick={() => updateQuery({ q: null, time: null, status: null, kind: null, tester: null, submitter: null })}
              >
                清除过滤
              </Button>
            }
          />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>Run</TH>
                  <TH>名称</TH>
                  <TH>状态</TH>
                  <TH>Task</TH>
                  <TH>耗时</TH>
                  <TH>提交人</TH>
                  <TH>创建时间</TH>
                </TR>
              </THead>
              <TBody>
                {visible.map((run) => (
                  <TR key={run.id}>
                    <TD className="whitespace-nowrap font-mono text-2xs">
                      <Link to={`/runs/${run.id}`} className="text-primary-text hover:underline">
                        {run.id}
                      </Link>
                    </TD>
                    <TD className="max-w-[16rem] truncate text-fg" title={run.name}>
                      {run.name}
                    </TD>
                    <TD>
                      <RunStatusBadge status={run.status} />
                    </TD>
                    <TD className="whitespace-nowrap tabular-nums">{progress(run)}</TD>
                    <TD className="whitespace-nowrap tabular-nums">{formatDuration(runDuration(run))}</TD>
                    <TD className="whitespace-nowrap">{run.submitted_by}</TD>
                    <TD className="whitespace-nowrap text-2xs text-fg-3" title={formatRelative(run.submitted_at)}>
                      {formatDateTime(run.submitted_at)}
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}

        <div className="flex flex-wrap items-center gap-3 border-t border-line-soft px-5 py-3 text-2xs text-fg-3">
          <span>
            共 {visible.length} 条{visible.length !== items.length ? `（已加载 ${items.length} 条，其中 ${items.length - visible.length} 条被本地过滤）` : ''}
          </span>
          {nextCursor ? (
            <Button size="sm" className="ml-auto" loading={loadingMore} onClick={() => void loadMore()}>
              加载更多
            </Button>
          ) : (
            <span className="ml-auto">没有更多了</span>
          )}
        </div>
      </Card>
    </>
  )
}
