import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'
import { stringify as toYaml } from 'yaml'
import { API_BASE, api, writeBlockedReason } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Attempt, Deploy, DeploySnapshot, Run, TaskDetail, TaskSummary } from '@/api/types'
import { EventTimeline } from '@/components/app/EventTimeline'
import { LogViewer } from '@/components/app/LogViewer'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { DeployStateBadge, RunStatusBadge, TaskStatusBadge, WaitingReasonTag } from '@/components/app/StatusBadge'
import { Badge, Tag } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { KV } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Banner, EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useToast } from '@/components/ui/toast'
import { useAsync } from '@/hooks/useAsync'
import { useIdentity } from '@/hooks/useIdentity'
import { useRunEvents } from '@/hooks/useRunEvents'
import {
  attemptStatusMeta,
  isTerminalRun,
  runStatusMeta,
  taskStatusMeta,
  waitingReasonLabel,
} from '@/lib/status'
import { cn, formatClock, formatDateTime, formatDuration, runDuration } from '@/lib/utils'

/* ---------------------------------------------------------------- 小件 */

const DOT_TONE: Record<string, string> = {
  ok: 'bg-success',
  info: 'bg-primary',
  warn: 'bg-warn',
  err: 'bg-danger',
  muted: 'bg-line-strong',
  primary: 'bg-primary',
}

function FlowNode({
  name,
  badge,
  meta,
  tone,
  onClick,
}: {
  name: string
  badge: string
  meta: string
  tone: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'flex w-[172px] shrink-0 flex-col gap-1 rounded-[10px] border border-line-strong bg-surface px-3 py-2 text-left',
        'transition-all duration-150 hover:border-primary/40 hover:bg-surface-2',
      )}
    >
      <span className="flex items-center gap-1.5 text-sm font-medium text-fg">
        <span className={cn('inline-block size-1.5 rounded-full', DOT_TONE[tone] ?? DOT_TONE['muted'])} aria-hidden />
        <span className="truncate">{name}</span>
      </span>
      <span className="truncate text-2xs text-fg-3">
        {badge} · {meta}
      </span>
    </button>
  )
}

function taskMeta(task: TaskSummary): string {
  if (task.waiting_reason) return waitingReasonLabel(task.waiting_reason)
  if (task.status === 'running') return '进行中'
  if (task.attempts === 0) return '未开始'
  return `Attempt #${task.attempts}`
}

/* ---------------------------------------------------------------- Deploy 区块 */

function manifestText(snapshot: DeploySnapshot | null): string | null {
  const manifest = snapshot?.manifest
  if (!manifest) return null
  try {
    return toYaml(manifest).trimEnd()
  } catch {
    return JSON.stringify(manifest, null, 2)
  }
}

function deployStateText(deploy: Deploy): string {
  switch (deploy.state) {
    case 'queued':
      return '排队中：等部署方资源，平台不按分钟级判失败'
    case 'accepted':
      return '已受理：部署方开始调度'
    case 'deploying':
      return '部署中：Pod 正在拉起'
    case 'ready':
      return '就绪：是否真的可用由 Pre-Check 判定'
    case 'blocked':
      return '被阻塞：终止，附后端原因'
    case 'failed':
      return '失败：看 queue_reason / Run 的 error'
    case 'canceled':
      return '已取消'
    default:
      return deploy.state
  }
}

function DeploySection({ run, version }: { run: Run; version: number }) {
  const deploy = run.deploy ?? null
  const snapshot = useAsync(async () => {
    if (!deploy) return null
    return api.getDeploy(run.id)
  }, [run.id, version, Boolean(deploy)])

  if (!deploy) return null

  const pods = snapshot.data?.pods ?? []
  const waited =
    deploy.state === 'queued' && deploy.queued_at
      ? Math.round((Date.now() - new Date(deploy.queued_at).getTime()) / 1000)
      : null
  const manifest = manifestText(snapshot.data)

  return (
    <Card>
      <CardHeader
        tone={deploy.state === 'queued' ? 'muted' : deploy.state === 'ready' ? 'ok' : 'info'}
        title={
          <span className="flex items-center gap-2">
            Deploy
            <DeployStateBadge state={deploy.state} />
          </span>
        }
        sub={deploy.provider ? `provider: ${deploy.provider}` : 'Genet'}
        actions={!isTerminalRun(run.status) ? <Badge tone="primary">状态由后端推</Badge> : null}
      />

      <CardBody className="flex flex-col gap-3">
        {deploy.state === 'queued' ? (
          // D-46：部署方只给水位计数，排队发生在 Volcano 层，没有位次 —— 只展示「排队中 + 已等待」与原因原文。
          <Banner
            tone="warn"
            title={`排队中${waited == null ? '' : ` · 已等待 ${formatDuration(waited)}`}`}
          >
            {deploy.queue_reason ? (
              <>
                原因（原文）：<span className="break-all font-mono text-2xs">{deploy.queue_reason}</span>
              </>
            ) : (
              '排队计入部署总窗口（默认 72h），期间可以取消。'
            )}
          </Banner>
        ) : null}
        {deploy.queue_reason && deploy.state !== 'queued' ? <Banner tone="info" title={deploy.queue_reason} /> : null}

        <dl className="grid gap-x-8 gap-y-2 sm:grid-cols-2">
          <KV label="状态">{deployStateText(deploy)}</KV>
          <KV label="Deployment ID">
            <span className="font-mono text-2xs">{deploy.deployment_id ?? '—'}</span>
          </KV>
          <KV label="Pod">
            {pods.length === 0 ? '尚未产生 Pod' : `${pods.length} 个（${pods.map((p) => p.role ?? '—').join(' / ')}）`}
          </KV>
          <KV label="Endpoint">
            {run.endpoint?.url ? (
              <span className="break-all font-mono text-2xs">{run.endpoint.url}</span>
            ) : (
              '—'
            )}
          </KV>
          <KV label="排队时间">{formatDateTime(deploy.queued_at)}</KV>
          <KV label="部署窗口">
            {snapshot.data?.deploy_timeout_seconds ? formatDuration(snapshot.data.deploy_timeout_seconds) : '—'}
          </KV>
        </dl>

        {manifest ? (
          <details className="rounded-md border border-line-soft bg-surface-2 px-3 py-2" open>
            <summary className="cursor-pointer text-2xs text-fg-2">实际提交给部署方的参数（原文快照）</summary>
            <pre className="mt-2 overflow-x-auto font-mono text-[11.5px] leading-relaxed text-fg-2">{manifest}</pre>
          </details>
        ) : null}
      </CardBody>

      {snapshot.error ? (
        <CardBody className="pt-0">
          <ErrorState error={snapshot.error} onRetry={snapshot.reload} compact />
        </CardBody>
      ) : null}

      {pods.length > 0 ? (
        <div className="border-t border-line-soft">
          <CardHeader
            tone="info"
            title="Pods"
            sub="多节点 / PD 分离时一个 Deploy 对应多个 Pod"
            className="border-b-0 px-5 py-3"
          />
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>Pod</TH>
                  <TH>角色</TH>
                  <TH>phase</TH>
                  <TH>就绪</TH>
                  <TH>节点</TH>
                  <TH>重启</TH>
                </TR>
              </THead>
              <TBody>
                {pods.map((pod) => (
                  <TR key={pod.name}>
                    <TD className="font-mono text-2xs text-fg">{pod.name}</TD>
                    <TD>{pod.role ? <Tag mono>{pod.role}</Tag> : <span className="text-fg-3">—</span>}</TD>
                    <TD className="text-2xs">{pod.phase ?? '—'}</TD>
                    <TD>
                      <Badge tone={pod.ready ? 'ok' : 'warn'}>{pod.ready ? '已就绪' : '未就绪'}</Badge>
                    </TD>
                    <TD className="text-2xs">{pod.node ?? '—'}</TD>
                    <TD className="tabular-nums">{pod.restarts ?? 0}</TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
          <div className="border-t border-line-soft px-5 py-4">
            <div className="mb-1 flex flex-wrap items-center gap-2">
              <span className="text-sm font-semibold text-fg">Pod 日志</span>
              <span className="text-2xs text-fg-3">按 Pod 分别落盘，不合并</span>
            </div>
            <p className="text-2xs leading-relaxed text-fg-3">
              MVP 的 Deploy 是 mock 阶段，日志端点不在接口契约内，前端不请求；这里只展示 Pod 拓扑与日志分段元信息。
            </p>
          </div>
        </div>
      ) : deploy.state === 'queued' || deploy.state === 'accepted' ? (
        <CardBody className="pt-0">
          <p className="text-2xs text-fg-3">尚未产生 Pod —— 排队 / 部署初期没有 Pod 拓扑。</p>
        </CardBody>
      ) : null}
    </Card>
  )
}

/* ---------------------------------------------------------------- Task 面板 */

function TaskAttemptsPanel({ summary }: { summary: TaskSummary }) {
  const detail = useAsync(() => api.getTask(summary.id), [summary.id])
  const [attemptIdx, setAttemptIdx] = useState<number | null>(null)

  const task: TaskDetail | null = detail.data
  const attempts: Attempt[] = task?.attempts ?? []
  const active = attempts.find((item) => item.idx === attemptIdx) ?? attempts[attempts.length - 1] ?? null

  const logs = useAsync(async () => {
    if (!active) return null
    return api.getAttemptLogs(summary.id, active.idx)
  }, [summary.id, active?.idx])

  if (detail.loading && !task) return <LoadingBlock label="加载 Task 详情…" />
  if (detail.error) return <ErrorState error={detail.error} onRetry={detail.reload} compact />
  if (!task) return null

  return (
    <div className="flex flex-col gap-4 border-t border-line-soft px-5 py-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold text-fg">
          Task #{task.seq} · {task.name}
        </h3>
        <TaskStatusBadge status={task.status} />
        <Tag mono>{task.tester_name}</Tag>
        <Tag mono>v{task.tester_version}</Tag>
        {task.endpoint_exclusive ? <Tag>独占</Tag> : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">{task.id}</span>
      </div>

      <dl className="grid gap-x-8 gap-y-2 sm:grid-cols-2">
        <KV label="参数原文">
          <code className="break-all font-mono text-2xs text-fg-2">{task.args_raw || '（无）'}</code>
        </KV>
        <KV label="注入参数">
          <code className="break-all font-mono text-2xs text-fg-3">
            {(task.injected_argv ?? []).join(' ') || '—'}
          </code>
        </KV>
        <KV label="执行环境">{task.profile ?? '—'}</KV>
        <KV label="镜像">
          <span className="break-all font-mono text-2xs">{task.image_ref}</span>
        </KV>
        <KV label="失败策略">{task.on_failure === 'abort_run' ? 'abort_run（失败终止整个 Run）' : 'continue'}</KV>
        <KV label="超时">{task.timeout_seconds ? formatDuration(task.timeout_seconds) : '—'}</KV>
        {task.status_reason ? <KV label="跳过原因">{task.status_reason}</KV> : null}
      </dl>

      {task.error ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-danger/25 bg-danger-weak px-3 py-2 text-xs">
          <Badge tone="err" className="font-mono">
            {task.error.code}
          </Badge>
          <span className="text-fg">{task.error.message}</span>
          {task.error.retryable ? <Badge tone="warn">可重试</Badge> : null}
        </div>
      ) : null}

      {attempts.length === 0 ? (
        <p className="text-2xs text-fg-3">该 Task 还没有 Attempt（未开始或已跳过）。</p>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-1.5">
            {attempts.map((item) => (
              <button
                key={item.idx}
                type="button"
                onClick={() => setAttemptIdx(item.idx)}
                className={cn(
                  'rounded-full border px-2.5 py-1 text-2xs transition-colors',
                  active?.idx === item.idx
                    ? 'border-primary bg-primary-weak text-primary-text'
                    : 'border-line-strong text-fg-2 hover:bg-surface-2',
                )}
              >
                Attempt #{item.idx} · {attemptStatusMeta(item.status).label}
              </button>
            ))}
            <span className="ml-auto text-2xs text-fg-3">重试是追加 Attempt，历史不覆盖</span>
          </div>

          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH className="w-16">Attempt</TH>
                  <TH>状态</TH>
                  <TH>phase</TH>
                  <TH>退出码</TH>
                  <TH>开始</TH>
                  <TH>结束</TH>
                  <TH>镜像 digest</TH>
                </TR>
              </THead>
              <TBody>
                {attempts.map((item) => (
                  <TR key={item.idx}>
                    <TD className="tabular-nums">#{item.idx}</TD>
                    <TD>
                      <Badge tone={attemptStatusMeta(item.status).tone}>{attemptStatusMeta(item.status).label}</Badge>
                    </TD>
                    <TD className="text-2xs">{item.phase ?? '—'}</TD>
                    <TD className="tabular-nums">{item.exit_code ?? '—'}</TD>
                    <TD className="whitespace-nowrap text-2xs">{formatClock(item.started_at)}</TD>
                    <TD className="whitespace-nowrap text-2xs">{formatClock(item.finished_at)}</TD>
                    <TD className="max-w-[10rem] truncate font-mono text-2xs text-fg-3" title={item.image_digest ?? ''}>
                      {item.image_digest ?? '—'}
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>

          <LogViewer
            text={logs.data?.text ?? ''}
            loading={logs.loading}
            error={logs.error}
            coverage={logs.data?.coverage ?? null}
            live={active?.status === 'running'}
            onRetry={logs.reload}
            hint={active ? `tasks/${task.id}/attempts/${active.idx}/logs` : undefined}
          />
        </>
      )}
    </div>
  )
}

function TaskPanels({ run }: { run: Run }) {
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null)
  const tasks = run.tasks ?? []
  const selected = tasks.find((t) => t.id === selectedTaskId) ?? tasks[0] ?? null

  return (
    <Card>
      <CardHeader
        tone="primary"
        title="Task 列表"
        sub={`${tasks.length} 个 · 详情里只有摘要，完整字段点开 Task 拉取（GET /tasks/{id}）`}
        actions={<span className="text-2xs text-fg-3">点一行看 Attempt 与日志</span>}
      />
      {tasks.length === 0 ? (
        <EmptyState title="没有 Task" description="这个 Run 还没解析出 Task。" />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <TH>#</TH>
                <TH>名称</TH>
                <TH>状态</TH>
                <TH>Attempt</TH>
                <TH>排队原因</TH>
                <TH>错误</TH>
              </TR>
            </THead>
            <TBody>
              {tasks.map((task, index) => {
                const isActive = selected?.id === task.id
                return (
                  <TR
                    key={task.id}
                    onClick={() => setSelectedTaskId(task.id)}
                    className={cn('cursor-pointer', isActive && 'bg-primary-weak/60')}
                  >
                    <TD className="tabular-nums text-2xs text-fg-3">{index}</TD>
                    <TD className="max-w-[16rem] truncate text-fg" title={task.name}>
                      {task.name}
                    </TD>
                    <TD>
                      <TaskStatusBadge status={task.status} />
                    </TD>
                    <TD className="tabular-nums">{task.attempts === 0 ? '—' : `#${task.attempts}`}</TD>
                    <TD>
                      <WaitingReasonTag reason={task.waiting_reason} />
                      {!task.waiting_reason ? <span className="text-fg-3">—</span> : null}
                    </TD>
                    <TD>
                      {task.error ? (
                        <Badge tone="err" className="font-mono" title={task.error.message}>
                          {task.error.code}
                        </Badge>
                      ) : (
                        <span className="text-fg-3">—</span>
                      )}
                    </TD>
                  </TR>
                )
              })}
            </TBody>
          </Table>
        </TableWrap>
      )}

      {selected ? <TaskAttemptsPanel key={selected.id} summary={selected} /> : null}
    </Card>
  )
}

/* ---------------------------------------------------------------- 页面 */

export function RunDetailPage() {
  const { runId = '' } = useParams<{ runId: string }>()
  // 终态 Run 不再订阅事件（只补历史）：live 在拿到详情后翻转
  const [live, setLive] = useState(true)
  const { events, status, version, resyncToken } = useRunEvents(runId, { live })
  const toast = useToast()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [actionError, setActionError] = useState<ApiErrorBody | null>(null)

  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)

  const run = useAsync(() => api.getRun(runId), [runId, version, resyncToken])

  useEffect(() => {
    if (!run.data) return
    setLive(!isTerminalRun(run.data.status))
  }, [run.data])

  const cancel = async () => {
    setCancelling(true)
    setActionError(null)
    try {
      await api.cancelRun(runId)
      setConfirmOpen(false)
      toast({ title: '已提交取消请求', description: '状态由服务端推送，先等 cancelling → canceled', tone: 'warn' })
      run.reload()
    } catch (e) {
      setActionError(toApiError(e))
      setConfirmOpen(false)
    } finally {
      setCancelling(false)
    }
  }

  if (run.error) {
    return (
      <Card>
        <CardHeader tone="err" title="加载 Run 失败" />
        <CardBody>
          <ErrorState error={run.error} onRetry={run.reload} />
          <div className="mt-4">
            <Link to="/runs">
              <Button icon={<ArrowLeft className="size-3.5" aria-hidden />}>返回列表</Button>
            </Link>
          </div>
        </CardBody>
      </Card>
    )
  }

  const detail = run.data
  const terminal = detail ? isTerminalRun(detail.status) : false
  const tasks = detail?.tasks ?? []

  return (
    <>
      <RightRail>
        <RailCard title="事件" tone="info" sub={status === 'polling' ? '降级为 3s 轮询' : 'SSE 实时推送'}>
          <EventTimeline events={events} status={status} />
        </RailCard>
        <RailCard title="边界说明" tone="muted">
          <p>状态流转与错误都记在 events 流里，UI 只读，不做乐观更新。</p>
          <p className="mt-2">日志内容不走 SSE，由日志面板按 Attempt 单独读。</p>
        </RailCard>
      </RightRail>

      <div className="flex flex-wrap items-center gap-2">
        <Link to="/runs">
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回列表
          </Button>
        </Link>
        {detail && !terminal ? (
          <Button
            size="sm"
            variant="dangerGhost"
            data-testid="cancel-run"
            disabled={Boolean(blocked)}
            title={blocked ?? undefined}
            onClick={() => setConfirmOpen(true)}
          >
            取消 Run
          </Button>
        ) : null}
        {terminal ? (
          <a
            className={buttonClassName({ size: 'sm' })}
            href={`${API_BASE}/runs/${encodeURIComponent(runId)}/report.md`}
            target="_blank"
            rel="noreferrer"
            title="新窗口打开 report.md（Run 结束后才写出）"
          >
            打开报告
          </a>
        ) : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">{runId}</span>
      </div>

      {actionError ? <ErrorState error={actionError} compact /> : null}

      {run.loading && !detail ? (
        <Card>
          <CardHeader tone="muted" title="加载 Run 详情" />
          <CardBody className="flex flex-col gap-3">
            <Skeleton className="h-3 w-1/3" />
            <Skeleton className="h-3 w-2/3" />
            <Skeleton className="h-3 w-1/2" />
          </CardBody>
        </Card>
      ) : null}

      {detail ? (
        <>
          <Card>
            <CardHeader
              tone="muted"
              title={
                <span className="flex flex-wrap items-center gap-2">
                  {detail.name}
                  <RunStatusBadge status={detail.status} />
                  <Tag mono>{detail.id}</Tag>
                  <Tag>{detail.kind === 'run' ? 'Run' : '独立 Task'}</Tag>
                </span>
              }
              sub={
                status === 'open'
                  ? '实时更新中'
                  : status === 'polling'
                    ? '实时更新已降级为 3s 轮询'
                    : status === 'reconnecting'
                      ? '连接断开，重连中'
                      : status === 'closed'
                        ? '已结束 · 时间线为历史事件'
                        : undefined
              }
            />
            <CardBody className="flex flex-col gap-3">
              {detail.error ? (
                <div className="flex flex-wrap items-center gap-2 rounded-md border border-danger/25 bg-danger-weak px-3 py-2 text-xs">
                  <Badge tone="err" className="font-mono">
                    {detail.error.code}
                  </Badge>
                  <span className="text-fg">{detail.error.message}</span>
                  {detail.error.retryable ? <Badge tone="warn">可重试</Badge> : null}
                </div>
              ) : null}

              <dl className="grid gap-x-10 gap-y-2 sm:grid-cols-2 xl:grid-cols-3">
                <KV label="状态说明">{runStatusMeta(detail.status).label}</KV>
                <KV label="提交人">{detail.submitted_by}</KV>
                <KV label="创建时间">{formatDateTime(detail.submitted_at)}</KV>
                <KV label="开始时间">{formatDateTime(detail.started_at)}</KV>
                <KV label="结束时间">{formatDateTime(detail.finished_at)}</KV>
                <KV label="耗时">{formatDuration(runDuration(detail))}</KV>
                <KV label="清理策略">{detail.cleanup_policy}</KV>
                <KV label="Endpoint">
                  {detail.endpoint ? (
                    <span className="break-all font-mono text-2xs">{detail.endpoint.url}</span>
                  ) : (
                    '—（没有 Deploy）'
                  )}
                </KV>
                <KV label="模型">{detail.endpoint?.model_name ?? '—'}</KV>
                <KV label="互斥键">
                  <span className="break-all font-mono text-2xs">{detail.endpoint?.key ?? '—'}</span>
                </KV>
                <KV label="Task 进度">
                  {detail.task_counts
                    ? `成功 ${detail.task_counts.succeeded} / 共 ${detail.task_counts.total}`
                    : `${tasks.length} 个`}
                </KV>
                <KV label="revision">{detail.revision ?? '—'}</KV>
              </dl>
            </CardBody>
          </Card>

          <Card>
            <CardHeader
              tone="primary"
              title="执行链路"
              sub="点任一节点看它自己的详情；按 tests[] 顺序排列，独占型 Tester 天然串行"
              actions={<span className="text-2xs text-fg-3">共 {(detail.deploy ? 1 : 0) + tasks.length} 个节点</span>}
            />
            <CardBody>
              <ol className="flex items-center gap-2 overflow-x-auto pb-1" aria-label="执行链路">
                {detail.deploy ? (
                  <li className="flex shrink-0 items-center gap-2">
                    <FlowNode
                      name="Deploy"
                      badge={detail.deploy.state}
                      meta={detail.deploy.state === 'queued' ? '排队中' : detail.deploy.state}
                      tone={detail.deploy.state === 'ready' ? 'ok' : detail.deploy.state === 'failed' ? 'err' : 'info'}
                      onClick={() => {
                        document.getElementById('deploy-section')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                      }}
                    />
                    <span className="text-fg-3" aria-hidden>
                      →
                    </span>
                  </li>
                ) : null}
                {tasks.map((task) => {
                  const meta = taskStatusMeta(task.status)
                  return (
                    <li key={task.id} className="flex shrink-0 items-center gap-2">
                      <FlowNode
                        name={task.name}
                        badge={meta.label}
                        meta={taskMeta(task)}
                        tone={meta.tone}
                        onClick={() => {
                          document
                            .getElementById('task-panels')
                            ?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                        }}
                      />
                      <span className="text-fg-3" aria-hidden>
                        →
                      </span>
                    </li>
                  )
                })}
              </ol>
              <p className="mt-2 text-2xs text-fg-3">
                非独占 Task 可在 Pre-Check 通过后并行，链路不假设只有一条运行中的分支。
              </p>
            </CardBody>
          </Card>

          <div id="deploy-section">
            <DeploySection run={detail} version={version} />
          </div>

          <div id="task-panels">
            <TaskPanels run={detail} />
          </div>

          <Card>
            <CardFooter>
              <span>
                事件共 {events.length} 条，连接状态：{status}
              </span>
            </CardFooter>
          </Card>
        </>
      ) : null}

      <Dialog
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        title="取消这个 Run？"
        description="取消后不可逆；状态以服务端推送为准（先 cancelling，再 canceled）。"
        footer={
          <>
            <Button onClick={() => setConfirmOpen(false)}>不取消</Button>
            <Button variant="danger" loading={cancelling} onClick={() => void cancel()}>
              确认取消
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3 text-xs text-fg-2">
          <div className="rounded-md border border-line-soft bg-surface-2 p-3">
            <p className="font-medium text-fg">将被标记 SKIPPED</p>
            <p className="mt-1 text-2xs text-fg-3">还没开始的 Task（PENDING / QUEUED）。</p>
          </div>
          <div className="rounded-md border border-line-soft bg-surface-2 p-3">
            <p className="font-medium text-fg">将被终止并标记 CANCELED</p>
            <p className="mt-1 text-2xs text-fg-3">正在跑的 Task（PREPARING / RUNNING）。</p>
          </div>
          {detail?.deploy && ['queued', 'accepted', 'deploying'].includes(detail.deploy.state) ? (
            <Banner tone="warn" title="当前 Deploy 未就绪">
              会先向部署方取消排队 / 卸载，可能不是即时生效；清理范围由 cleanup_policy 决定。
            </Banner>
          ) : null}
        </div>
      </Dialog>
    </>
  )
}
