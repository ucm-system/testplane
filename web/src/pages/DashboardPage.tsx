import { Link } from 'react-router-dom'
import { ArrowUpRight, Plus, Server } from 'lucide-react'
import { api } from '@/api'
import type { QueueStatus } from '@/api/types'
import { KpiCard } from '@/components/app/KpiCard'
import { RunsTable } from '@/components/app/RunsTable'
import { Button } from '@/components/ui/button'
import { KpiSkeleton } from '@/components/ui/skeleton'
import { ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { formatNumber } from '@/lib/utils'

/**
 * 概览（D-62：与运行列表合并）。
 *
 * 一页两段，**都是主内容**：
 *  1. KPI 四卡 + 一行「排队中的 Task」摘要（原来在右栏；右栏为了让宽表占满整幅宽度而取消）；
 *  2. 全量 Run 表（真分页 + Excel 式列筛选，条件全在 URL 上）—— 原来 `/runs` 那一整套，
 *     现在直接长在概览上，不再有独立列表页（`/runs` 只做兼容重定向）。
 *
 * 数据分三路拉、互不阻塞：`getDashboardStats`（KPI）、`getQueueStatus`（排队摘要）、
 * `RunsTable` 自己的 `listRuns` + `listRunFacets`（表与列筛选候选值）。
 * KPI 先出来、表后出来（或反过来）都是正常现象，两边各有 skeleton。
 */
export function DashboardPage() {
  const { data, loading, error, reload } = useAsync(async () => {
    const [stats, queue] = await Promise.all([api.getDashboardStats(), api.getQueueStatus()])
    return { stats, queue }
  }, [])

  const stats = data?.stats
  const queue: QueueStatus | null = data?.queue ?? null
  const waitingTasks = queue?.waiting?.tasks ?? []

  return (
    <>
      {/* 页面级动作放页头行：新建 Run / Tester 目录（原右栏「快捷操作」并到这里） */}
      <div className="flex flex-wrap items-center gap-3">
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Link to="/testers">
            <Button size="sm" variant="ghost" icon={<Server className="size-3" aria-hidden />} data-testid="go-testers">
              Tester 目录
            </Button>
          </Link>
          <Link to="/submit">
            <Button variant="primary" data-testid="new-run" icon={<Plus className="size-3.5" aria-hidden />}>
              新建 Run
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
            <KpiCard
              label="进行中 Run"
              value={formatNumber(stats?.active_runs ?? 0)}
              hint="含部署与 Pre-Check"
              tone="blue"
            />
            <KpiCard
              label="排队中 Task"
              value={formatNumber(stats?.queued_tasks ?? 0)}
              hint="平台槽位 + 队列两个来源"
              tone="ink"
            />
            <KpiCard
              label="成功率（终态 Run）"
              value={stats?.success_rate == null ? '—' : `${Math.round(stats.success_rate * 100)}%`}
              hint={`近 24h 失败 ${formatNumber(stats?.failed_24h ?? 0)} 个`}
              tone="green"
            />
            <KpiCard
              label="已注册 Tester"
              value={formatNumber(stats?.tester_count ?? 0)}
              hint="随镜像内置，启动时自动发现"
              tone="amber"
            />
          </>
        )}
      </section>

      {/* 原右栏「排队中的 Task」压成一行：谁在排队 + 去看队列 */}
      <div
        data-testid="queue-summary"
        className="flex flex-wrap items-center gap-2 rounded-xl border border-line bg-surface px-4 py-2.5 text-2xs text-fg-3 shadow-xs"
      >
        <span className="text-fg-2">排队中的 Task：{waitingTasks.length} 个</span>
        {waitingTasks.length > 0 ? (
          <>
            <span aria-hidden>·</span>
            <span className="min-w-0 truncate" title={waitingTasks.map((task) => task.name).join('、')}>
              {waitingTasks
                .slice(0, 3)
                .map((task) => task.name)
                .join('、')}
              {waitingTasks.length > 3 ? ` 等 ${waitingTasks.length} 个` : ''}
            </span>
          </>
        ) : null}
        <Link
          to="/clusters"
          className="ml-auto inline-flex h-6 items-center gap-1 text-primary-text hover:underline"
          data-testid="queue-link"
        >
          看队列
          <ArrowUpRight className="size-3" aria-hidden />
        </Link>
      </div>

      {/* 全量 Run 表（原运行列表页整套；真分页 + 列筛选，条件都在 URL 上） */}
      <RunsTable />
    </>
  )
}
