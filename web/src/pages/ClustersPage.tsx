import { Link } from 'react-router-dom'
import { api } from '@/api'
import type { Cluster, QueueStatus } from '@/api/types'
import { WaitingReasonTag } from '@/components/app/StatusBadge'
import { Badge } from '@/components/ui/badge'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useAsync } from '@/hooks/useAsync'
import { waitingReasonLabel } from '@/lib/status'
import { formatDuration, formatRelative } from '@/lib/utils'

function UsageBar({ used, total }: { used: number; total: number }) {
  const ratio = total > 0 ? Math.min(1, used / total) : 0
  const tone = ratio >= 1 ? 'bg-danger' : ratio >= 0.75 ? 'bg-warn' : 'bg-success'
  return (
    <span className="flex min-w-[7rem] items-center gap-2">
      <span className="relative h-1.5 flex-1 overflow-hidden rounded-full bg-surface-3">
        <span className={`absolute inset-y-0 left-0 rounded-full ${tone}`} style={{ width: `${ratio * 100}%` }} />
      </span>
      <span className="w-14 shrink-0 text-right font-mono text-2xs tabular-nums text-fg-2">
        {used}/{total}
      </span>
    </span>
  )
}

/** `last_check_result` 是后端原文（`ok` / 错误串），前端只做三态映射，不解析语义。 */
function clusterHealth(cluster: Cluster): 'ok' | 'err' | 'muted' {
  const result = (cluster.last_check_result ?? '').toLowerCase()
  if (!result) return 'muted'
  if (['ok', 'ready', 'healthy', 'true'].includes(result)) return 'ok'
  return 'err'
}

export function ClustersPage() {
  const data = useAsync(async () => {
    const [clusters, queue] = await Promise.all([api.listClusters(), api.getQueueStatus()])
    return { clusters, queue }
  }, [])

  const clusters: Cluster[] = data.data?.clusters ?? []
  const queue: QueueStatus | null = data.data?.queue ?? null
  const slots = queue?.slots
  const waiting = queue?.waiting
  const volcano = queue?.volcano

  return (
    <>

      {data.error ? <ErrorState error={data.error} onRetry={data.reload} /> : null}

      <Card>
        <CardHeader tone="primary" title="集群" sub={`${clusters.length} 个 · 只读`} />
        {data.loading && clusters.length === 0 ? (
          <TableSkeleton rows={2} cols={5} />
        ) : clusters.length === 0 ? (
          <EmptyState title="还没有登记集群" description="由 admin 登记后显示。" />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>名称</TH>
                  <TH>API Server</TH>
                  <TH>说明</TH>
                  <TH>连通性</TH>
                  <TH>最近检查</TH>
                </TR>
              </THead>
              <TBody>
                {clusters.map((cluster) => {
                  const health = clusterHealth(cluster)
                  return (
                    <TR key={cluster.name}>
                      <TD className="font-mono text-2xs text-fg">{cluster.name}</TD>
                      <TD className="break-all font-mono text-2xs">{cluster.api_server}</TD>
                      <TD className="text-2xs text-fg-3">{cluster.description ?? '—'}</TD>
                      <TD>
                        <Badge tone={health === 'ok' ? 'ok' : health === 'err' ? 'err' : 'muted'}>
                          {cluster.last_check_result ?? '未检查'}
                        </Badge>
                      </TD>
                      <TD className="whitespace-nowrap text-2xs text-fg-3">
                        {cluster.last_check_at ? formatRelative(cluster.last_check_at) : '—'}
                      </TD>
                    </TR>
                  )
                })}
              </TBody>
            </Table>
          </TableWrap>
        )}
      </Card>

      <Card>
        <CardHeader
          tone="info"
          title="平台槽位"
          actions={
            <span className="text-2xs text-fg-3">
              {queue?.generated_at ? `${formatRelative(queue.generated_at)}生成` : '只读'}
            </span>
          }
        />
        <CardBody className="flex flex-col gap-3">
          {data.loading && !slots ? (
            <TableSkeleton rows={1} cols={2} />
          ) : slots ? (
            <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
              <div className="flex items-center gap-2">
                <span className="text-2xs text-fg-3">活跃 / 上限</span>
                <UsageBar used={slots.active} total={slots.concurrency_max} />
              </div>
              <span className="text-2xs text-fg-3">剩余 {slots.available} 个名额</span>
              <span className="text-2xs text-fg-3">端点互斥租约 {(slots.endpoints ?? []).length} 个</span>
            </div>
          ) : (
            <p className="text-2xs text-fg-3">没有槽位数据。</p>
          )}

          {(slots?.endpoints ?? []).length > 0 ? (
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>Endpoint 互斥键</TH>
                    <TH>独占</TH>
                    <TH>持有 Task</TH>
                  </TR>
                </THead>
                <TBody>
                  {(slots?.endpoints ?? []).map((slot) => (
                    <TR key={slot.endpoint_key}>
                      <TD className="break-all font-mono text-2xs">{slot.endpoint_key}</TD>
                      <TD>{slot.exclusive ? <Badge tone="warn">独占</Badge> : <Badge tone="muted">共享</Badge>}</TD>
                      <TD className="font-mono text-2xs text-fg-3">
                        {(slot.holders ?? [])
                          .map((holder) => `${holder.task_id}#${holder.seq}`)
                          .join(', ') || '—'}
                      </TD>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </TableWrap>
          ) : null}
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          tone="warn"
          title="Volcano 队列"
          sub={volcano?.available ? '只读快照' : '调度器不可达'}
          actions={volcano?.stale ? <Badge tone="warn">曾采到过快照</Badge> : null}
        />
        {!volcano ? null : !volcano.available ? (
          // 契约：available=false 时 queues 恒为 []，平台不回放历史数字 ——
          // 所以这里**不能**渲染成「0 排队 / 无排队」，只给降级说明。
          <CardBody className="flex flex-col gap-2">
            <p className="text-xs text-fg-2">
              Volcano 快照不可用{volcano.detail ? <span className="ml-1 font-mono text-2xs text-fg-3">{volcano.detail}</span> : null}
            </p>
          </CardBody>
        ) : (volcano.queues ?? []).length === 0 ? (
          <EmptyState title="采样成功，但没有登记任何队列" description="集群里还没有登记 Volcano Queue。" />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>队列</TH>
                  <TH>状态</TH>
                  <TH>排队 Job</TH>
                  <TH>最长等待</TH>
                  <TH>主要原因</TH>
                </TR>
              </THead>
              <TBody>
                {(volcano.queues ?? []).map((item) => (
                  <TR key={item.name}>
                    <TD className="font-mono text-2xs text-fg">{item.name}</TD>
                    <TD>
                      <Badge tone={item.state === 'Open' ? 'ok' : 'muted'}>{item.state ?? '—'}</Badge>
                    </TD>
                    <TD className="tabular-nums">{item.pending_jobs ?? 0}</TD>
                    <TD className="whitespace-nowrap tabular-nums">{formatDuration(item.longest_wait_seconds)}</TD>
                    <TD className="max-w-[26rem]">
                      {/* 契约改了：reason 是 PodGroup conditions[-1].message 原文，不是枚举 —— 原样渲染，不翻译 */}
                      {item.reason ? (
                        <span className="block break-all font-mono text-2xs leading-[16px] text-fg-2" title={item.reason}>
                          {item.reason}
                        </span>
                      ) : (
                        <span className="text-fg-3">—</span>
                      )}
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
      </Card>

      <Card>
        <CardHeader
          tone="muted"
          title="排队中的 runner"
          sub={waiting ? `共 ${waiting.total} 个` : undefined}
        />
        <CardBody className="flex flex-col gap-3">
          {waiting ? (
            <div className="flex flex-wrap gap-2">
              {Object.entries(waiting.by_reason)
                .filter(([, count]) => count > 0)
                .map(([reason, count]) => (
                  <span
                    key={reason}
                    className="inline-flex items-center gap-1.5 rounded-full border border-line bg-surface-2 px-2.5 py-1 text-2xs text-fg-2"
                  >
                    {waitingReasonLabel(reason)}
                    <span className="font-mono tabular-nums text-fg-3">{count}</span>
                  </span>
                ))}
            </div>
          ) : null}

          {(waiting?.runners ?? []).length === 0 ? (
            <p className="text-2xs text-fg-3">暂无排队。</p>
          ) : (
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>runner</TH>
                    <TH>Task</TH>
                    <TH>状态</TH>
                    <TH>排队原因</TH>
                  </TR>
                </THead>
                <TBody>
                  {(waiting?.runners ?? []).map((runner) => (
                    <TR key={`${runner.task_id}:${runner.attempt_no}:${runner.seq}`}>
                      <TD className="text-fg">
                        {runner.name}
                        <span className="ml-2 font-mono text-2xs text-fg-3">seq {runner.seq}</span>
                      </TD>
                      <TD className="font-mono text-2xs">
                        <Link to={`/tasks/${runner.task_id}`} className="text-primary-text hover:underline">
                          {runner.task_id}
                        </Link>
                      </TD>
                      <TD className="text-2xs">{runner.status}</TD>
                      <TD>
                        <WaitingReasonTag reason={runner.waiting_reason} />
                      </TD>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </TableWrap>
          )}
        </CardBody>
      </Card>
    </>
  )
}