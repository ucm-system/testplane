import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Search } from 'lucide-react'
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

const PAGE_SIZE = 10

function progress(run: RunSummary): string {
  const counts = run.task_counts
  if (!counts) return '—'
  return `${counts.succeeded} / ${counts.total}`
}

export function RunsPage() {
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all')
  const [timeFilter, setTimeFilter] = useState<TimeFilter>('all')
  const [search, setSearch] = useState('')

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
      const page = await api.listRuns({ status: STATUS_QUERY[statusFilter], limit: PAGE_SIZE })
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
  }, [statusFilter])

  useEffect(() => {
    void loadFirstPage()
  }, [loadFirstPage])

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return
    setLoadingMore(true)
    try {
      const page = await api.listRuns({ status: STATUS_QUERY[statusFilter], cursor: nextCursor, limit: PAGE_SIZE })
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
            SKIPPED 表示没跑，FAILED 表示跑了但失败，两者不混用。
          </p>
        </RailCard>
        <RailCard title="过滤口径" tone="muted">
          <p>状态过滤走服务端（`?status=` 逗号分隔），时间与关键字在当前已加载的数据上过滤。</p>
          <p className="mt-2">分页是游标：只回传 next_cursor，不解析、不换算成 offset。</p>
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
                  onChange={(e) => setSearch(e.target.value)}
                  placeholder="筛选本页 Run（按名称 / ID）"
                  aria-label="筛选本页 Run（按名称 / ID）"
                  title="只过滤已加载的这一页，不是全局搜索"
                  className="h-9 w-56 pl-8"
                />
              </div>
              <Segmented<StatusFilter>
                ariaLabel="状态过滤"
                value={statusFilter}
                onChange={setStatusFilter}
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
                onChange={(e) => setTimeFilter(e.target.value as TimeFilter)}
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
              <Button size="sm" onClick={() => { setSearch(''); setTimeFilter('all'); setStatusFilter('all') }}>
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
