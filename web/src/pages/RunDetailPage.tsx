import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'
import { stringify as toYaml } from 'yaml'
import { API_BASE, api, writeBlockedReason } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Attempt, Deploy, DeploySnapshot, RetryPlan, Run, TaskDetail, TaskSummary } from '@/api/types'
import { EventTimeline, CONNECTION_LABEL, connectionStateOf } from '@/components/app/EventTimeline'
import { LogViewer } from '@/components/app/LogViewer'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { RunResultView } from '@/components/app/RunResultView'
import { DeployStateBadge, RunStatusBadge, TaskStatusBadge, WaitingReasonTag } from '@/components/app/StatusBadge'
import { Badge, Tag } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { KV } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Banner, EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { Tabs, tabPanelProps } from '@/components/ui/tabs'
import { useToast } from '@/components/ui/toast'
import { useAsync } from '@/hooks/useAsync'
import { DeployLogsPanel } from '@/components/app/DeployLogsPanel'
import { useIdentity } from '@/hooks/useIdentity'
import { useLogTail } from '@/hooks/useLogTail'
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

/** Pod / Attempt 的 phase 是 k8s 标识符：展示用中文，原文留在 title 里 */
const PHASE_LABEL: Record<string, string> = {
  Pending: '等待中',
  Preparing: '准备中',
  Running: '运行中',
  Succeeded: '成功',
  Failed: '失败',
  Unknown: '未知',
  Canceled: '已取消',
}

function phaseLabel(phase: string | null | undefined): string {
  if (!phase) return '—'
  return PHASE_LABEL[phase] ?? phase
}

/** Pod / Attempt 的 phase 是 k8s 标识符：展示用中文，原文留在 title 里 *//** `RetryPlan.reason` 是枚举：翻成人话，原始值留在括号里方便对日志 */
const RETRY_PLAN_REASON: Record<string, string> = {
  run_not_terminal: 'Run 还没结束（run_not_terminal）',
  retry_in_progress: '已有重试在进行中（retry_in_progress）',
  run_canceled: '已取消的 Run 不支持重试（run_canceled）',
  nothing_to_retry: '没有可重跑的 Task（nothing_to_retry）',
}

/** `RetryPlan.reason` 是枚举：翻成人话，原始值留在括号里方便对日志 */function DeploySection({ run, version }: { run: Run; version: number }) {
  const deploy = run.deploy ?? null
  const snapshot = useAsync(async () => {
    if (!deploy) return null
    return api.getDeploy(run.id)
  }, [run.id, version, Boolean(deploy)])
  /** Pod 拓扑表点某一行 → 日志面板切到对应 Pod 页签（两边共享同一个选中值） */
  const [selectedPod, setSelectedPod] = useState<string | null>(null)

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
            sub="多节点 / PD 分离时一个 Deploy 对应多个 Pod；点 Pod 名切到它的日志页签"
            className="border-b-0 px-5 py-3"
          />
          <div data-testid="deploy-pods">
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>Pod</TH>
                    <TH>角色</TH>
                    <TH>阶段</TH>
                    <TH>就绪</TH>
                    <TH>重启</TH>
                    <TH>节点</TH>
                  </TR>
                </THead>
                <TBody>
                  {pods.map((pod) => (
                    <TR key={pod.name}>
                      <TD>
                        <button
                          type="button"
                          data-testid="deploy-pod-name"
                          onClick={() => {
                            setSelectedPod(pod.name)
                            document.getElementById('deploy-logs')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
                          }}
                          className="break-all text-left font-mono text-2xs text-primary-text hover:underline"
                          title={`查看 ${pod.name} 的日志`}
                        >
                          {pod.name}
                        </button>
                      </TD>
                      <TD>{pod.role ? <Tag mono>{pod.role}</Tag> : <span className="text-fg-3">—</span>}</TD>
                      <TD className="text-2xs" title={pod.phase ?? undefined}>{phaseLabel(pod.phase)}</TD>
                      <TD>
                        <Badge tone={pod.ready ? 'ok' : 'warn'}>{pod.ready ? '已就绪' : '未就绪'}</Badge>
                      </TD>
                      <TD className="tabular-nums">
                        {(pod.restarts ?? 0) > 0 ? <Badge tone="warn">{pod.restarts} 次</Badge> : 0}
                      </TD>
                      <TD className="text-2xs">{pod.node ?? '—'}</TD>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </TableWrap>
          </div>
          <div id="deploy-logs" className="border-t border-line-soft">
            <div className="flex flex-wrap items-center gap-2 px-5 pb-1 pt-4">
              <span className="text-sm font-semibold text-fg">Deploy 日志</span>
              <span className="text-2xs text-fg-3">按 Pod 分页签，不合并；断过的地方显式标 gap；落盘文件可直接下载 .log</span>
            </div>
            <DeployLogsPanel
              runId={run.id}
              deployState={deploy.state}
              pods={pods}
              live={!isTerminalRun(run.status)}
              pod={selectedPod}
              onPodChange={setSelectedPod}
            />
          </div>
        </div>
      ) : (
        /* Pod 拓扑为空（排队中 / 已回收）：日志端点 `?pod=` 必选，没有 Pod 时**不请求**，
           面板按 deploy.state 给"排队中 / 已回收"的文案。 */
        <div className="border-t border-line-soft">
          <CardHeader
            tone="info"
            title="Pods"
            sub="Pod 列表来自 deploy/pods.json；暂无 Pod 时仍可看排队原因"
            className="border-b-0 px-5 py-3"
          />
          <div data-testid="deploy-pods">
            <p className="px-5 pb-3 text-xs text-fg-3">暂无 Pod（排队中或已回收）</p>
          </div>
          <div id="deploy-logs" className="border-t border-line-soft">
            <div className="flex flex-wrap items-center gap-2 px-5 pb-1 pt-4">
              <span className="text-sm font-semibold text-fg">Deploy 日志</span>
              <span className="text-2xs text-fg-3">没有 Pod 时不请求日志端点（`?pod=` 必选）</span>
            </div>
            <DeployLogsPanel
              runId={run.id}
              deployState={deploy.state}
              pods={pods}
              pod={selectedPod}
              onPodChange={setSelectedPod}
            />
          </div>
        </div>
      )}
    </Card>
  )
}

/* ---------------------------------------------------------------- Task 面板 */

function TaskAttemptsPanel({
  summary,
  version,
  runStatus,
}: {
  summary: TaskSummary
  version: number
  runStatus: string
}) {
  const [attemptRefresh, setAttemptRefresh] = useState(0)
  const detail = useAsync(() => api.getTask(summary.id), [summary.id, version, attemptRefresh])
  const [attemptIdx, setAttemptIdx] = useState<number | null>(null)
  const [retryOpen, setRetryOpen] = useState(false)
  const [retrying, setRetrying] = useState(false)
  const toast = useToast()
  const { auth } = useIdentity()
  const blocked = writeBlockedReason(auth)

  const task: TaskDetail | null = detail.data
  const attempts: Attempt[] = task?.attempts ?? []
  const active = attempts.find((item) => item.idx === attemptIdx) ?? attempts[attempts.length - 1] ?? null

  /**
   * 日志按游标轮询（P0）：运行中的 Attempt 每 3s 带 `offset` 取增量并**追加**；
   * Run / Attempt 终态后停止（`live=false`），面板改为「Run 已结束，日志不再更新」。
   * 切 Attempt / 切页签 / 卸载时 hook 内部会清掉定时器，不会重复请求。
   */
  const runEnded = isTerminalRun(runStatus)
  const logs = useLogTail({
    streamKey: active ? `${summary.id}#${active.idx}` : '',
    live: Boolean(active) && active!.status === 'running' && !runEnded,
    load: (offset) => api.getAttemptLogs(summary.id, active!.idx, { offset }),
  })

  const TASK_TERMINAL = ['succeeded', 'failed', 'canceled', 'skipped']
  const canRetryTask = task ? TASK_TERMINAL.includes(task.status) && runStatus !== 'canceled' : false

  const confirmRetryTask = async () => {
    if (!task) return
    setRetrying(true)
    try {
      const accepted = await api.retryTask(task.id)
      toast({
        title: `已提交重跑：${task.name}`,
        description: accepted.attempt_no ? `新 Attempt #${accepted.attempt_no} 已入队` : 'Task 已重新排队',
        tone: 'ok',
      })
      setRetryOpen(false)
      setAttemptIdx(null)
      setAttemptRefresh((token) => token + 1)
    } catch (e) {
      const error = toApiError(e)
      toast({ title: '重跑失败', description: `${error.code}：${error.message}`, tone: 'err' })
    } finally {
      setRetrying(false)
    }
  }

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
        {canRetryTask ? (
          <Button
            size="sm"
            data-testid="retry-task"
            disabled={Boolean(blocked)}
            title={blocked ?? '追加一个新 Attempt（历史 Attempt 不覆盖）'}
            onClick={() => setRetryOpen(true)}
          >
            重跑这个 Task
          </Button>
        ) : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">{task.id}</span>
      </div>

      <Dialog
        open={retryOpen}
        onClose={() => setRetryOpen(false)}
        title={`重跑 Task：${task.name}`}
        description="会追加一个新 Attempt（历史 Attempt 不覆盖）；已成功的 Run 上的 Task 也可单独重跑。"
        footer={
          <>
            <Button onClick={() => setRetryOpen(false)}>取消</Button>
            <Button
              variant="primary"
              data-testid="retry-task-confirm"
              loading={retrying}
              onClick={() => void confirmRetryTask()}
            >
              确认重跑
            </Button>
          </>
        }
      >
        <p className="text-xs text-fg-2">
          当前 Attempt 数：<span className="font-mono">{task.current_attempt}</span>，重跑后会变成{' '}
          <span className="font-mono">{task.current_attempt + 1}</span>。
        </p>
      </Dialog>

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
                  <TH>阶段</TH>
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
                    <TD className="text-2xs" title={item.phase ?? undefined}>{phaseLabel(item.phase)}</TD>
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
            text={logs.text}
            loading={logs.loading}
            error={logs.error}
            coverage={logs.coverage}
            live={Boolean(active) && active!.status === 'running' && !runEnded}
            ended={runEnded}
            lastUpdatedAt={logs.lastUpdatedAt}
            appended={logs.appended}
            onRetry={logs.reload}
            hint={active ? `tasks/${task.id}/attempts/${active.idx}/logs` : undefined}
            testIdPrefix="attempt-log"
          />
        </>
      )}
    </div>
  )
}

/** 只有非终态的 Task 才能取消（终态取消是无效操作，服务端会 409）。 */
const CANCELLABLE: ReadonlySet<string> = new Set(['pending', 'queued', 'preparing', 'running'])

function TaskPanels({ run, onReload }: { run: Run; onReload: () => void }) {
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null)
  const [pendingCancel, setPendingCancel] = useState<TaskSummary | null>(null)
  const [cancelling, setCancelling] = useState(false)
  const [refreshToken, setRefreshToken] = useState(0)
  const toast = useToast()
  const { auth } = useIdentity()
  const blocked = writeBlockedReason(auth)
  const tasks = run.tasks ?? []
  const selected = tasks.find((t) => t.id === selectedTaskId) ?? tasks[0] ?? null

  const confirmCancel = async () => {
    if (!pendingCancel) return
    setCancelling(true)
    try {
      await api.cancelTask(pendingCancel.id)
      toast({
        title: `已提交取消：${pendingCancel.name}`,
        description: 'Task 会先到 CANCELED；已完成的 Task 不受影响',
        tone: 'warn',
      })
      setPendingCancel(null)
      setRefreshToken((token) => token + 1)
      onReload()
    } catch (e) {
      const error = toApiError(e)
      toast({ title: '取消失败', description: `${error.code}：${error.message}`, tone: 'err' })
    } finally {
      setCancelling(false)
    }
  }

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
                <TH className="text-right">操作</TH>
              </TR>
            </THead>
            <TBody>
              {tasks.map((task, index) => {
                const isActive = selected?.id === task.id
                const cancellable = CANCELLABLE.has(task.status)
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
                    <TD className="text-right">
                      {/* 键盘可达：行内第一个焦点是可见的「查看」按钮，整行点击仍然保留（C3） */}
                      <span className="inline-flex items-center gap-1">
                        <Button
                          size="sm"
                          variant="ghost"
                          data-testid="view-task"
                          title={`查看 ${task.name} 的 Attempt 与日志`}
                          onClick={(event) => {
                            event.stopPropagation()
                            setSelectedTaskId(task.id)
                          }}
                        >
                          查看
                        </Button>
                        {cancellable ? (
                          <Button
                            size="sm"
                            variant="dangerGhost"
                            data-testid="cancel-task"
                            disabled={Boolean(blocked)}
                            title={blocked ?? '取消这个 Task（已完成的 Task 不受影响）'}
                            onClick={(event) => {
                              event.stopPropagation()
                              setSelectedTaskId(task.id)
                              setPendingCancel(task)
                            }}
                          >
                            取消 Task
                          </Button>
                        ) : null}
                      </span>
                    </TD>
                  </TR>
                )
              })}
            </TBody>
          </Table>
        </TableWrap>
      )}

      {selected ? <TaskAttemptsPanel key={selected.id} summary={selected} version={refreshToken} runStatus={run.status} /> : null}

      <Dialog
        open={pendingCancel !== null}
        onClose={() => setPendingCancel(null)}
        title={`取消 Task：${pendingCancel?.name ?? ''}`}
        description="取消后该 Task 置为 CANCELED（正在跑的会被终止）；已完成的 Task 不受影响，也不会重排新的 Attempt。"
        footer={
          <>
            <Button onClick={() => setPendingCancel(null)}>不取消</Button>
            <Button variant="danger" loading={cancelling} onClick={() => void confirmCancel()}>
              确认取消 Task
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-xs text-fg-2">
          <p>
            当前状态：<TaskStatusBadge status={pendingCancel?.status ?? 'pending'} />
          </p>
          <p className="text-2xs leading-relaxed text-fg-3">
            状态以服务端推送为准：确认后本页会重新拉一次 Run 与 Task 详情，不做乐观点更新。
          </p>
        </div>
      </Dialog>
    </Card>
  )
}

/* ---------------------------------------------------------------- 页面 */

export function RunDetailPage() {
  const { runId = '' } = useParams<{ runId: string }>()
  const navigate = useNavigate()
  // 终态 Run 不再订阅事件（只补历史）：live 在拿到详情后翻转
  const [live, setLive] = useState(true)
  const { events, status, version, resyncToken, reconnect } = useRunEvents(runId, { live })
  // 连接态唯一来源：右栏副标题、事件区小字、页内横幅都从这一个值派生（B5）
  const connection = connectionStateOf(status)
  const toast = useToast()
  // 终态默认看「结果」（用户：跑完了只想拿结果），运行中只有「执行细节」
  const [view, setView] = useState<'result' | 'detail'>('detail')
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [actionError, setActionError] = useState<ApiErrorBody | null>(null)
  // 重试：先拉 retry-plan 预览，再确认创建新 Run
  const [retryOpen, setRetryOpen] = useState(false)
  const [retryPlan, setRetryPlan] = useState<RetryPlan | null>(null)
  const [retrying, setRetrying] = useState(false)

  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)

  const run = useAsync(() => api.getRun(runId), [runId, version, resyncToken])

  const plan = useAsync(async () => (retryOpen ? api.getRetryPlan(runId) : null), [runId, retryOpen])

  useEffect(() => {
    if (plan.data) setRetryPlan(plan.data)
  }, [plan.data])

  // 只在"首屏"与"刚跑完"这两个时刻决定默认视图；
  // 数据刷新（SSE 推 version）不能把用户手选的页签顶回去
  const lastStatusRef = useRef<string | null>(null)
  useEffect(() => {
    if (!run.data) return
    const status = run.data.status
    const done = isTerminalRun(status)
    setLive(!done)
    const prev = lastStatusRef.current
    const firstLoad = prev === null
    const justFinished = prev !== null && !isTerminalRun(prev as typeof status) && done
    if (firstLoad || justFinished) setView(done ? 'result' : 'detail')
    lastStatusRef.current = status
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

  /** 可重试的终态：失败 / 部分失败 / 服务不可用（canceled 与成功的都不给重试） */
  const RETRYABLE_RUN_STATUS = new Set(['failed', 'completed_with_errors', 'service_unavailable'])
  const canRetry = (detailStatus: string) => RETRYABLE_RUN_STATUS.has(detailStatus)

  const openRetry = () => {
    setRetryPlan(null)
    setRetryOpen(true)
  }

  const confirmRetry = async () => {
    setRetrying(true)
    try {
      const accepted = await api.retryRun(runId)
      setRetryOpen(false)
      toast({
        title: '已创建重试 Run',
        description: `新 Run ${accepted.new_run_id}（retry_of ${accepted.retry_of}）`,
        tone: 'ok',
      })
      navigate(`/runs/${accepted.new_run_id}`)
    } catch (e) {
      const error = toApiError(e)
      toast({
        title: error.code === 'CONFLICT' ? '已有重试在进行中' : '重试失败',
        description: `${error.code}：${error.message}`,
        tone: 'err',
      })
    } finally {
      setRetrying(false)
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
        <RailCard
          title="事件"
          tone="info"
          sub={
            terminal
              ? 'Run 已结束 · 下面是历史事件'
              : connection === 'live'
                ? '实时更新中 · 断开后会自动重连'
                : CONNECTION_LABEL[connection]
          }
        >
          <EventTimeline events={events} connection={connection} ended={terminal} />
        </RailCard>
        <RailCard title="边界说明" tone="muted">
          <p>页面上的状态跟着服务端推送走，不会先替你改成「已完成」；刷新一下拿到的才是最新。</p>
          <p className="mt-2">日志不在事件流里：点开某个 Task，才单独读它那一次尝试的日志。</p>
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
        {detail && terminal && canRetry(detail.status) ? (
          <Button
            size="sm"
            data-testid="retry-run"
            disabled={Boolean(blocked)}
            title={blocked ?? '复用已成功的 Task，只重跑失败/跳过的'}
            onClick={openRetry}
          >
            重试
          </Button>
        ) : null}
        {terminal ? (
          <>
            <Link to={`/runs/${encodeURIComponent(runId)}/report`} data-testid="open-report">
              <Button size="sm">查看报告</Button>
            </Link>
            <a
              className={buttonClassName({ size: 'sm', variant: 'ghost' })}
              href={`${API_BASE}/runs/${encodeURIComponent(runId)}/report.md`}
              target="_blank"
              rel="noreferrer"
              title="新窗口打开 report.md（Run 结束后才写出）"
            >
              report.md
            </a>
          </>
        ) : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">{runId}</span>
      </div>

      {/* 断线横幅（主内容顶部）：连接态与右栏同源，恢复后自动消失 */}
      {!terminal && connection === 'reconnecting' ? (
        <Banner
          tone="warn"
          title="连接断开，正在重连…"
          actions={
            <Button size="sm" variant="secondary" data-testid="reconnect-events" onClick={reconnect}>
              立即重试
            </Button>
          }
        >
          事件时间线暂时停止更新，已经收到的内容仍然可读；恢复后这条提示会自动消失。
        </Banner>
      ) : null}

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

      {detail && terminal ? (
        <Tabs<string>
          ariaLabel="Run 详情视图"
          idBase="run-view"
          value={view}
          onChange={(next: string) => setView(next as 'result' | 'detail')}
          items={[
            { value: 'result', label: '结果' },
            { value: 'detail', label: '执行细节' },
          ]}
          className="rounded-lg border border-line-soft bg-surface px-3 pt-1 shadow-card"
        />
      ) : null}

      {/* 页签与面板配对：role=tabpanel + id / aria-controls / aria-labelledby（B7） */}
      {detail && terminal && view === 'result' ? (
        <section {...tabPanelProps('run-view', 'result')} className="flex flex-col gap-4">
          <RunResultView run={detail} />
        </section>
      ) : null}

      {detail && (!terminal || view === 'detail') ? (
        <section
          {...(terminal ? tabPanelProps('run-view', 'detail') : {})}
          className="flex flex-col gap-4"
        >
          <Card>
            <CardHeader
              tone="muted"
              title={
                <span className="flex flex-wrap items-center gap-2">
                  {/* 页面内 h1（对象名）：19px/600，压过卡头 15px 的普通标题 */}
                  <h1 className="text-[19px] font-semibold leading-tight tracking-[-0.02em]">{detail.name}</h1>
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
            <TaskPanels run={detail} onReload={run.reload} />
          </div>

          <Card>
            <CardFooter>
              <span>
                事件共 {events.length} 条 · {terminal ? 'Run 已结束，显示历史事件' : CONNECTION_LABEL[connection]}
              </span>
            </CardFooter>
          </Card>
        </section>
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

      {/* 重试预览：复用 vs 重跑，确认后创建新 Run（retry_of 指向当前） */}
      <Dialog
        open={retryOpen}
        onClose={() => setRetryOpen(false)}
        title="重试这个 Run？"
        description="会创建一个新 Run（retry_of 指向当前），已成功的 Task 不会重复执行。"
        size="lg"
        footer={
          <>
            <Button onClick={() => setRetryOpen(false)}>取消</Button>
            <Button
              variant="primary"
              data-testid="retry-confirm"
              loading={retrying}
              disabled={!retryPlan?.retryable}
              title={retryPlan && !retryPlan.retryable ? (retryPlan.reason ?? '当前 Run 不可重试') : undefined}
              onClick={() => void confirmRetry()}
            >
              确认重试
            </Button>
          </>
        }
      >
        {plan.loading && !retryPlan ? (
          <LoadingBlock label="正在计算 retry-plan…" />
        ) : plan.error ? (
          <ErrorState error={plan.error} onRetry={plan.reload} compact />
        ) : retryPlan ? (
          <div className="flex flex-col gap-3 text-xs">
            {!retryPlan.retryable ? (
              <Banner role="alert" tone="warn" title="当前 Run 不可重试">
                {RETRY_PLAN_REASON[retryPlan.reason ?? 'nothing_to_retry']}
              </Banner>
            ) : null}

            <section>
              <h3 className="mb-1.5 text-xs font-medium text-fg">
                将复用（已成功）<span className="ml-1 font-mono text-2xs text-fg-3">{retryPlan.reuse.length}</span>
              </h3>
              {retryPlan.reuse.length === 0 ? (
                <p className="text-2xs text-fg-3">没有可复用的 Task。</p>
              ) : (
                <ul className="flex flex-col divide-y divide-line-soft rounded-md border border-line-soft">
                  {retryPlan.reuse.map((task) => (
                    <li key={task.task_id} className="flex items-center gap-2 px-3 py-2">
                      <span className="min-w-0 flex-1 truncate text-fg" title={task.name}>
                        {task.name}
                      </span>
                      {/* 契约里 reuse[] 只有 reason（复用依据），没有 status */}
                      <Badge tone="ok">{task.reason === 'reused' ? '复用（reused）' : '已成功（succeeded）'}</Badge>
                      <span className="font-mono text-2xs text-fg-3">{task.task_id}</span>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            <section>
              <h3 className="mb-1.5 text-xs font-medium text-fg">
                将重跑（失败 / 跳过）<span className="ml-1 font-mono text-2xs text-fg-3">{retryPlan.rerun.length}</span>
              </h3>
              {retryPlan.rerun.length === 0 ? (
                <p className="text-2xs text-fg-3">没有需要重跑的 Task。</p>
              ) : (
                <ul className="flex flex-col divide-y divide-line-soft rounded-md border border-line-soft">
                  {retryPlan.rerun.map((task) => (
                    <li key={task.task_id} className="flex flex-wrap items-center gap-2 px-3 py-2">
                      <span className="min-w-0 flex-1 truncate text-fg" title={task.name}>
                        {task.name}
                      </span>
                      <TaskStatusBadge status={task.status as never} />
                      {/* 失败信息在 failure（Failure.code / retryable），不是 error_code */}
                      {task.failure ? (
                        <>
                          <Badge tone="err" className="font-mono" title={task.failure.message}>
                            {task.failure.code}
                          </Badge>
                          {task.failure.retryable ? <Badge tone="warn">可重试</Badge> : <Badge tone="muted">不可重试</Badge>}
                        </>
                      ) : task.reason ? (
                        <span className="text-2xs text-fg-3">{task.reason}</span>
                      ) : null}
                      <span className="font-mono text-2xs text-fg-3">{task.task_id}</span>
                    </li>
                  ))}
                </ul>
              )}
            </section>

            {/* blocked_items：想复用但不可复用（产物缺失 / 校验未通过），要单独讲清原因 */}
            {retryPlan.blocked_items.length > 0 ? (
              <section>
                <h3 className="mb-1.5 text-xs font-medium text-fg">
                  不能复用（产物不可用）
                  <span className="ml-1 font-mono text-2xs text-fg-3">{retryPlan.blocked_items.length}</span>
                </h3>
                <ul className="flex flex-col divide-y divide-line-soft rounded-md border border-warn/30 bg-warn-weak">
                  {retryPlan.blocked_items.map((item) => (
                    <li key={item.task_id} className="flex flex-wrap items-center gap-2 px-3 py-2">
                      <span className="min-w-0 flex-1 truncate text-fg" title={item.name}>
                        {item.name}
                      </span>
                      <span className="text-2xs text-warn-text">{item.reason}</span>
                      <span className="font-mono text-2xs text-fg-3">{item.task_id}</span>
                    </li>
                  ))}
                </ul>
              </section>
            ) : null}

            <p className="border-t border-line-soft pt-2 text-2xs leading-relaxed text-fg-3">
              底部的确认会创建新 Run：<code className="font-mono">retry_of</code> 指向当前 Run，已成功的 Task
              复用结果不重复执行（Manifest 摘要 <code className="font-mono">{retryPlan.manifest_digest}</code>）。
            </p>
          </div>
        ) : null}
      </Dialog>
    </>
  )
}