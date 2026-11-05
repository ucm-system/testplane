import { Link } from 'react-router-dom'
import { api } from '@/api'
import type { PlatformSlots, QueueEntry } from '@/api/types'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { WaitingReasonTag } from '@/components/app/StatusBadge'
import { Badge, Tag } from '@/components/ui/badge'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useAsync } from '@/hooks/useAsync'
import { formatDuration, formatRelative } from '@/lib/utils'

function UsageBar({ used, total }: { used: number; total: number }) {
  const ratio = total > 0 ? Math.min(1, used / total) : 0
  const tone = ratio >= 1 ? 'bg-danger' : ratio >= 0.75 ? 'bg-warn' : 'bg-success'
  return (
    <span className="flex min-w-[7rem] items-center gap-2">
      <span className="relative h-1.5 flex-1 overflow-hidden rounded-full bg-surface-3">
        <span className={`absolute inset-y-0 left-0 rounded-full ${tone}`} style={{ width: `${ratio * 100}%` }} />
      </span>
      <span className="w-12 shrink-0 text-right font-mono text-2xs tabular-nums text-fg-2">
        {used}/{total}
      </span>
    </span>
  )
}

function SlotsCard({ slots }: { slots: PlatformSlots | null | undefined }) {
  if (!slots) return null
  return (
    <Card>
      <CardHeader tone="info" title="平台槽位" sub="全局并发上限，超出排队但不拒绝" />
      <CardBody className="flex flex-wrap items-center gap-3">
        <UsageBar used={slots.used} total={slots.total} />
        <span className="text-2xs text-fg-3">
          已用 {slots.used} / 上限 {slots.total}
          {slots.queue_depth == null ? null : ` · 队列深度 ${slots.queue_depth}`}
        </span>
      </CardBody>
    </Card>
  )
}

function QueueRow({ queue }: { queue: QueueEntry }) {
  const deserved = queue.deserved ?? 0
  const allocated = queue.allocated ?? 0
  return (
    <TR>
      <TD className="font-mono text-2xs text-fg">{queue.name}</TD>
      <TD>
        <Badge tone={queue.state === 'open' ? 'ok' : 'muted'}>{queue.state ?? '—'}</Badge>
      </TD>
      <TD>
        <UsageBar used={allocated} total={deserved} />
      </TD>
      <TD className="tabular-nums">{queue.pending ?? 0}</TD>
      <TD className="whitespace-nowrap tabular-nums">{formatDuration(queue.longest_wait_seconds)}</TD>
      <TD>
        <WaitingReasonTag reason={queue.reason} />
        {!queue.reason ? <span className="text-fg-3">—</span> : null}
      </TD>
    </TR>
  )
}

export function ClustersPage() {
  const data = useAsync(async () => {
    const [clusters, queue] = await Promise.all([api.listClusters(), api.getQueueStatus()])
    return { clusters, queue }
  }, [])

  const clusters = data.data?.clusters ?? []
  const queue = data.data?.queue

  return (
    <>
      <RightRail>
        <RailCard title="边界说明" tone="warn">
          <p>平台不参与调度：这一页只回答「卡在队列还是卡在平台槽位」。</p>
          <p className="mt-2">队列满 = 等资源；队列空却还在排队 = 平台并发（TESTPLANE_CONCURRENCY_MAX）用满了。</p>
        </RailCard>
        <RailCard title="凭据" tone="muted">
          <p>集群概要不返回任何凭据，只给 Secret 里的 key 名与最近一次连通性检查结果。</p>
        </RailCard>
      </RightRail>

      {data.error ? <ErrorState error={data.error} onRetry={data.reload} /> : null}

      <Card>
        <CardHeader tone="primary" title="集群" sub={`${clusters.length} 个 · 只读`} />
        {data.loading && clusters.length === 0 ? (
          <TableSkeleton rows={3} cols={6} />
        ) : clusters.length === 0 ? (
          <EmptyState title="还没有登记集群" description="集群由 admin 在后端登记（kubeconfig 放 Secret），控制台只读。" />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>名称</TH>
                  <TH>命名空间</TH>
                  <TH>默认</TH>
                  <TH>连通性</TH>
                  <TH>节点</TH>
                  <TH>最近检查</TH>
                  <TH>凭据 key</TH>
                </TR>
              </THead>
              <TBody>
                {clusters.map((cluster) => (
                  <TR key={cluster.name}>
                    <TD className="font-mono text-2xs text-fg">{cluster.name}</TD>
                    <TD className="font-mono text-2xs">{cluster.namespace ?? '—'}</TD>
                    <TD>{cluster.is_default ? <Badge tone="primary">默认</Badge> : <span className="text-fg-3">—</span>}</TD>
                    <TD>
                      <Badge tone={cluster.healthy === false ? 'err' : cluster.healthy === true ? 'ok' : 'muted'}>
                        {cluster.healthy === false ? '不可达' : cluster.healthy === true ? '正常' : '未知'}
                      </Badge>
                      {cluster.message ? (
                        <p className="mt-0.5 text-2xs leading-relaxed text-fg-3">{cluster.message}</p>
                      ) : null}
                    </TD>
                    <TD className="tabular-nums">{cluster.node_count ?? '—'}</TD>
                    <TD className="whitespace-nowrap text-2xs text-fg-3">
                      {cluster.last_checked_at ? formatRelative(cluster.last_checked_at) : '—'}
                    </TD>
                    <TD className="font-mono text-2xs text-fg-3">{cluster.kubeconfig_key ?? '—'}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
      </Card>

      <SlotsCard slots={queue?.slots} />

      <Card>
        <CardHeader
          tone="warn"
          title="Volcano 队列"
          sub="已分配 / 配额"
          actions={<span className="text-2xs text-fg-3">{queue?.updated_at ? formatRelative(queue.updated_at) + '更新' : '只读'}</span>}
        />
        {data.loading && !queue ? (
          <TableSkeleton rows={3} cols={6} />
        ) : !queue || queue.queues.length === 0 ? (
          <EmptyState title="没有队列数据" description="集群没接 Volcano，或 /queue-status 暂不可用。" />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>队列</TH>
                  <TH>状态</TH>
                  <TH>已分配 / 配额</TH>
                  <TH>排队 Job</TH>
                  <TH>最长等待</TH>
                  <TH>主要原因</TH>
                </TR>
              </THead>
              <TBody>
                {queue.queues.map((item) => (
                  <QueueRow key={item.name} queue={item} />
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
        <CardFooter>
          <span>平台只读调度器状态，不插队、不改配额。</span>
        </CardFooter>
      </Card>

      <Card>
        <CardHeader tone="muted" title="排队中的 Task" sub={queue?.pending_tasks?.length ? `${queue.pending_tasks.length} 个` : undefined} />
        {queue?.pending_tasks?.length ? (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>Task</TH>
                  <TH>Run</TH>
                  <TH>排队原因</TH>
                  <TH>已等待</TH>
                </TR>
              </THead>
              <TBody>
                {queue.pending_tasks.map((task) => (
                  <TR key={task.task_id}>
                    <TD className="text-fg">
                      {task.name ?? task.task_id}
                      <Tag mono className="ml-2">
                        {task.task_id}
                      </Tag>
                    </TD>
                    <TD className="font-mono text-2xs">
                      {task.run_id ? (
                        <Link to={`/runs/${task.run_id}`} className="text-primary-text hover:underline">
                          {task.run_id}
                        </Link>
                      ) : (
                        '—'
                      )}
                    </TD>
                    <TD>
                      <WaitingReasonTag reason={task.waiting_reason} />
                    </TD>
                    <TD className="tabular-nums">{formatDuration(task.waited_seconds)}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        ) : (
          <EmptyState title="没有排队中的 Task" description="当前没有在等槽位或在等资源的 Task。" />
        )}
      </Card>
    </>
  )
}
