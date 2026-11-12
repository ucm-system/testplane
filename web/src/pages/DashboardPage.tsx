import { Link } from 'react-router-dom'
import { ArrowUpRight, Plus, Server } from 'lucide-react'
import { api } from '@/api'
import type { QueueStatus } from '@/api/types'
import { KpiCard } from '@/components/app/KpiCard'
import { TasksTable } from '@/components/app/TasksTable'
import { Button } from '@/components/ui/button'
import { KpiSkeleton } from '@/components/ui/skeleton'
import { ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { formatNumber } from '@/lib/utils'

/**
 * 概览（D-62：与 Task 列表合并）。
 *
 * 一页两段，**都是主内容**：
 *  1. KPI 四卡 + 一行「排队中的 runner」摘要；
 *  2. 全量 Task 表（真分页 + Excel 式列筛选，条件全在 URL 上）。
 *
 * 数据分三路拉、互不阻塞：`getDashboardStats`（KPI）、`getQueueStatus`（排队摘要）、
 * `TasksTable` 自己的 `listTasks` + `listTaskFacets`（表与列筛选候选值）。
 * KPI 先出来、表后出来（或反过来）都是正常现象，两边各有 skeleton。
 */
export function DashboardPage() {
  const { data, loading, error, reload } = useAsync(async () => {
    const [stats, queue] = await Promise.all([api.getDashboardStats(), api.getQueueStatus()])
    return { stats, queue }
  }, [])

  const stats = data?.stats
  const queue: QueueStatus | null = data?.queue ?? null
  const waitingRunners = queue?.waiting?.runners ?? []

  return (
    <>
      {/* 页面级动作放页头行：新建 Task / Runner 目录 */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Link to="/runners">
            <Button size="sm" variant="ghost" icon={<Server className="size-3" aria-hidden />} data-testid="go-runners">
              Runner 目录
            </Button>
          </Link>
          <Link to="/submit">
            <Button variant="primary" data-testid="new-task" icon={<Plus className="size-3.5" aria-hidden />}>
              新建 Task
            </Button>
          </Link>
        </div>
      </div>

      {error ? <ErrorState error={error} onRetry={reload} /> : null}

      <section className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4" aria-label="概览指标">
        {loading && !stats ? (
          <>
            <KpiSkeleton />
            <KpiSkeleton />
            <KpiSkeleton />
            <KpiSkeleton />
          </>
        ) : (
          <>
            <KpiCard label="进行中 Task" value={formatNumber(stats?.active_tasks ?? 0)} tone="blue" />
            <KpiCard label="排队 runner" value={formatNumber(stats?.queued_runners ?? 0)} tone="ink" />
            <KpiCard
              label="成功率（终态 Task）"
              value={stats?.success_rate == null ? '—' : `${Math.round(stats.success_rate * 100)}%`}
              hint={`24h 失败 ${formatNumber(stats?.failed_24h ?? 0)}`}
              tone="green"
            />
            <KpiCard label="已注册 Runner" value={formatNumber(stats?.runner_count ?? 0)} tone="amber" />
          </>
        )}
      </section>

      {/* 有 runner 真的在排队才显示这一行（0 个的横条是噪音） */}
      {waitingRunners.length > 0 ? (
        <div
          data-testid="queue-summary"
          className="flex flex-wrap items-center gap-2 rounded-xl border border-line bg-surface px-4 py-2.5 text-2xs text-fg-3 shadow-xs"
        >
          <span className="text-fg-2">排队中的 runner：{waitingRunners.length} 个</span>
          <span aria-hidden>·</span>
          <span className="min-w-0 truncate" title={waitingRunners.map((runner) => runner.name).join('、')}>
            {waitingRunners
              .slice(0, 3)
              .map((runner) => runner.name)
              .join('、')}
            {waitingRunners.length > 3 ? ` 等 ${waitingRunners.length} 个` : ''}
          </span>
          <Link
            to="/clusters"
            className="ml-auto inline-flex h-6 items-center gap-1 text-primary-text hover:underline"
            data-testid="queue-link"
          >
            看队列
            <ArrowUpRight className="size-3" aria-hidden />
          </Link>
        </div>
      ) : null}

      {/* 全量 Task 表（真分页 + 列筛选，条件都在 URL 上） */}
      <TasksTable />
    </>
  )
}
