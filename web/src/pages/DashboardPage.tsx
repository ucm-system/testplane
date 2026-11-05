import { Link } from 'react-router-dom'
import { api } from '@/api'
import type { QueueStatus, RunSummary } from '@/api/types'
import { KpiCard } from '@/components/app/KpiCard'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { RunStatusBadge, WaitingReasonTag } from '@/components/app/StatusBadge'
import { Button } from '@/components/ui/button'
import { Card, CardHeader } from '@/components/ui/card'
import { KpiSkeleton, TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useAsync } from '@/hooks/useAsync'
import { formatDateTime, formatDuration, formatNumber, runDuration } from '@/lib/utils'

function taskProgress(run: RunSummary): string {
  const counts = run.task_counts
  if (!counts) return '—'
  return `${counts.succeeded} / ${counts.total}`
}

export function DashboardPage() {
  const { data, loading, error, reload } = useAsync(async () => {
    const [stats, runs, queue] = await Promise.all([
      api.getDashboardStats(),
      api.listRuns({ limit: 8 }),
      api.getQueueStatus(),
    ])
    return { stats, runs: runs.items, queue }
  }, [])

  const stats = data?.stats
  const runs = data?.runs ?? []
  const queue: QueueStatus | null = data?.queue ?? null

  return (
    <>
      <RightRail>
        <RailCard title="快捷操作" tone="muted">
          <div className="flex flex-col gap-2">
            <Link to="/runs" className="block">
              <Button className="w-full">查看运行列表</Button>
            </Link>
            <Link to="/clusters" className="block">
              <Button className="w-full">看集群与队列</Button>
            </Link>
          </div>
        </RailCard>

        <RailCard title="排队中的 Task" tone="warn" sub={queue?.waiting?.tasks?.length ? `${queue.waiting.tasks.length} 个` : undefined}>
          {queue?.waiting?.tasks?.length ? (
            <ul className="flex flex-col divide-y divide-line-soft">
              {(queue.waiting.tasks ?? []).slice(0, 5).map((task) => (
                <li key={task.task_id} className="flex items-center justify-between gap-2 py-2 first:pt-0 last:pb-0">
                  <div className="min-w-0">
                    <p className="truncate text-xs text-fg" title={task.name}>
                      {task.name}
                    </p>
                    <p className="font-mono text-2xs text-fg-3">{task.run_id}</p>
                  </div>
                  <WaitingReasonTag reason={task.waiting_reason} />
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-2xs text-fg-3">当前没有排队中的 Task。</p>
          )}
        </RailCard>

        <RailCard title="这页怎么看" tone="muted">
          <p>队列满了 = 在等资源；队列空着却还在排队 = 卡在平台自己的并发名额上。</p>
          <p className="mt-2">这里的数字是打开页面时那一次的结果，不会自己跳：想确认最新情况，重新打开这一页。</p>
        </RailCard>
      </RightRail>

      {/* 看板是「新建 Run」的入口之一（B12）；页面级操作放页头行，不塞进卡片 */}
      <div className="flex flex-wrap items-center gap-3">
        <span className="text-2xs text-fg-3">打开页面时刷新的数字，不会自己跳</span>
        <Link to="/submit" className="ml-auto">
          <Button variant="primary" data-testid="new-run">
            新建 Run
          </Button>
        </Link>
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

      <Card>
        <CardHeader
          tone="ok"
          title="最近 Run"
          sub={stats ? `共 ${stats.total_runs} 个` : undefined}
          actions={
            <Link to="/runs" className="text-xs text-primary-text hover:underline">
              查看全部
            </Link>
          }
        />
        {loading && runs.length === 0 ? (
          <TableSkeleton rows={5} cols={6} />
        ) : runs.length === 0 ? (
          <EmptyState
            title="还没有 Run"
            description="提交第一份 Manifest 后，这里会显示最近 8 个 Run 的状态与耗时。"
            action={
              <Link to="/submit">
                <Button variant="primary" size="sm">
                  去提交
                </Button>
              </Link>
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
                  <TH>创建时间</TH>
                </TR>
              </THead>
              <TBody>
                {runs.map((run) => (
                  <TR key={run.id}>
                    <TD className="font-mono text-2xs">
                      <Link to={`/runs/${run.id}`} className="text-primary-text hover:underline">
                        {run.id}
                      </Link>
                    </TD>
                    <TD className="text-fg">{run.name}</TD>
                    <TD>
                      <RunStatusBadge status={run.status} />
                    </TD>
                    <TD className="tabular-nums">{taskProgress(run)}</TD>
                    <TD className="tabular-nums">{formatDuration(runDuration(run))}</TD>
                    <TD className="text-2xs text-fg-3">{formatDateTime(run.submitted_at)}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
        <div className="flex items-center justify-between border-t border-line-soft px-5 py-3 text-2xs text-fg-3">
          <span>状态流转由服务端推送，UI 只读</span>
          <Link to="/clusters" className="text-primary-text hover:underline">
            看集群与队列
          </Link>
        </div>
      </Card>

    </>
  )
}
