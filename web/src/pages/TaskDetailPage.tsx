import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'
import { api, writeBlockedReason } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Deploy, RunnerExecution, TaskDetail } from '@/api/types'
import { EventTimeline, CONNECTION_LABEL, connectionStateOf } from '@/components/app/EventTimeline'
import { LogViewer } from '@/components/app/LogViewer'
import { TaskResultView } from '@/components/app/TaskResultView'
import { DeployStateBadge, RunnerStatusBadge, TaskStatusBadge, WaitingReasonTag } from '@/components/app/StatusBadge'
import { Badge, Tag } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { KV } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Banner, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { Tabs, tabPanelProps } from '@/components/ui/tabs'
import { useToast } from '@/components/ui/toast'
import { useAsync } from '@/hooks/useAsync'
import { DeployLogsPanel } from '@/components/app/DeployLogsPanel'
import { NotFoundPage } from '@/pages/NotFoundPage'
import { TaskReportView } from '@/pages/ReportPage'
import { useIdentity } from '@/hooks/useIdentity'
import { useLogTail } from '@/hooks/useLogTail'
import { useTaskEvents } from '@/hooks/useTaskEvents'
import { attemptStatusMeta, deployStateMeta, isTerminalTask, runnerStatusMeta, waitingReasonLabel } from '@/lib/status'
import { cn, formatDateTime, formatDuration, runDuration } from '@/lib/utils'

/* ---------------------------------------------------------------- 小件 */

const DOT_TONE: Record<string, string> = {
  ok: 'bg-success',
  info: 'bg-primary',
  warn: 'bg-warn',
  err: 'bg-danger',
  muted: 'bg-line-strong',
  primary: 'bg-primary',
}

/** 管道节点的角色图标语义（deploy / pre_check / test / clean 用小标签区分，不用图标库专属图标）。 */
const ROLE_LABEL: Record<RunnerExecution['role'], string> = {
  deploy: '部署',
  pre_check: 'pre_check',
  test: 'test',
  clean: 'clean',
}

function runnerDuration(runner: RunnerExecution): string {
  if (!runner.started_at) return '—'
  const start = new Date(runner.started_at).getTime()
  const end = runner.finished_at ? new Date(runner.finished_at).getTime() : Date.now()
  return formatDuration(Math.max(0, Math.round((end - start) / 1000)))
}

function runnerMeta(runner: RunnerExecution): string {
  if (runner.waiting_reason) return waitingReasonLabel(runner.waiting_reason)
  if (runner.status === 'pending') return '等待前序 runner'
  if (runner.status === 'skipped' && runner.status_reason) {
    return runner.status_reason === 'upstream_abort' ? '前序失败，未执行' : '取消，未执行'
  }
  return runnerDuration(runner)
}

/**
 * 管道时间线的单个节点（纯展示：节点是按钮，不是拖拽画布；严格串行单链，web-and-delivery §4.2）。
 */
function PipelineNode({
  name,
  role,
  status,
  meta,
  tone,
  active,
  onClick,
}: {
  name: string
  role: RunnerExecution['role']
  status: string
  meta: string
  tone: string
  active?: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={`${name}（${runnerStatusMeta(status).label}）`}
      aria-current={active ? 'true' : undefined}
      className={cn(
        'flex w-[176px] shrink-0 flex-col gap-1 rounded-lg border px-3 py-2 text-left transition-all duration-150',
        active ? 'border-primary bg-primary-weak/60' : 'border-line-strong bg-surface hover:border-primary/40 hover:bg-surface-2',
      )}
    >
      <span className="flex items-center gap-1.5 text-sm font-medium text-fg">
        <span className={cn('inline-block size-1.5 rounded-full', DOT_TONE[tone] ?? DOT_TONE['muted'])} aria-hidden />
        <span className="truncate">{name}</span>
      </span>
      <span className="flex items-center gap-1.5 text-2xs text-fg-3">
        <Tag>{ROLE_LABEL[role]}</Tag>
        <span className="truncate">
          {runnerStatusMeta(status).label} · {meta}
        </span>
      </span>
    </button>
  )
}

/* ---------------------------------------------------------------- Deploy 节点区块 */

/** deploy-state.json 的 phase 说明（D-47 / D-70；queued / accepted 是平台推断的阶段，以 Genet 实际进度为准）。 */
function deployPhaseText(deploy: Deploy): string {
  switch (deploy.phase) {
    case 'queued':
      return '排队中：等部署方资源，排队计入部署总窗口（默认 72h），期间可以取消。'
    case 'accepted':
      return '已受理：部署方开始调度。'
    case 'deploying':
      return '部署中：Pod 正在拉起。'
    case 'ready':
      return '就绪：是否真的可用由 pre_check 判定。'
    case 'blocked':
      return '被阻塞：会收口为 service_unavailable（D-47）。'
    case null:
    case undefined:
      return '尚未启动：service-deploy runner 还没跑到。'
    default:
      return deploy.phase
  }
}

function DeploySection({
  task,
  version,
}: {
  task: TaskDetail
  version: number
}) {
  const deploy = task.deploy ?? null
  const snapshot = useAsync(async () => {
    if (!deploy) return null
    return api.getTaskDeploy(task.id)
  }, [task.id, version, Boolean(deploy)])
  /** Pod 拓扑表点某一行 → 日志面板切到对应 Pod 页签（两边共享同一个选中值） */
  const [selectedPod, setSelectedPod] = useState<string | null>(null)

  if (!deploy) return null

  const pods = snapshot.data?.pods ?? []
  const phase = deploy.phase
  // D-46：**没有位次**（queue.position 恒 null）—— 只展示 phase + 水位计数 + 已等待时长。
  const queue = deploy.queue
  const waited = deploy.waited_seconds ?? null

  return (
    <Card id="deploy-section">
      <CardHeader
        tone={phase === 'queued' ? 'muted' : phase === 'ready' ? 'ok' : phase === 'blocked' ? 'err' : 'info'}
        title={
          <span className="flex items-center gap-2">
            service-deploy
            <DeployStateBadge state={phase} />
            {deploy.genet_status ? <Tag mono title="Helm status 原值（两根轴之一，D-45）">{deploy.genet_status}</Tag> : null}
          </span>
        }
        sub={`provider: ${deploy.provider}${deploy.release_name ? ` · release: ${deploy.release_name}` : ''}`}
        actions={!isTerminalTask(task.status) ? <Badge tone="primary">状态由后端推</Badge> : null}
      />

      <CardBody className="flex flex-col gap-3">
        {phase === 'queued' ? (
          // D-46：部署方只给水位计数，排队发生在 Volcano 层，没有位次 —— 只展示「排队中 + 已等待」与水位计数。
          <Banner tone="warn" title={`排队中${waited == null ? '' : ` · 已等待 ${formatDuration(waited)}`}`}>
            {queue ? (
              <span className="break-all">
                水位（计数，无位次）：pending {queue.pending ?? '—'} · inqueue {queue.inqueue ?? '—'} · running{' '}
                {queue.running ?? '—'}
              </span>
            ) : (
              '排队计入部署总窗口（默认 72h），期间可以取消。'
            )}
          </Banner>
        ) : null}
        {phase === 'blocked' && deploy.reason ? (
          <Banner tone="err" title={`被阻塞（${deploy.reason}）`}>
            deploy-state 的 blocked 会把 service-deploy 判失败（DEPLOY_BLOCKED），Task 收口为 service_unavailable。
          </Banner>
        ) : null}

        <dl className="grid gap-x-8 gap-y-2 sm:grid-cols-2">
          <KV label="阶段">{deployPhaseText(deploy)}</KV>
          <KV label="已等待时长">{waited == null ? '—' : formatDuration(waited)}</KV>
          <KV label="release">
            <span className="font-mono text-2xs">{deploy.release_name ?? '—'}</span>
          </KV>
          <KV label="Pod">
            {pods.length === 0 ? '尚未产生 Pod' : `${pods.length} 个（${pods.map((p) => p.role ?? '—').join(' / ')}）`}
          </KV>
          <KV label="Endpoint">
            {task.endpoint?.url ? <span className="break-all font-mono text-2xs">{task.endpoint.url}</span> : '—'}
          </KV>
          <KV label="部署窗口">
            {snapshot.data?.deploy_timeout_seconds ? formatDuration(snapshot.data.deploy_timeout_seconds) : '—'}
          </KV>
          {deploy.spec ? (
            <KV label="部署描述摘要">
              <span className="font-mono text-2xs">{deploy.spec_digest ?? '—'}</span>
            </KV>
          ) : null}
        </dl>

        {deploy.spec ? (
          <details className="rounded-md border border-line-soft bg-surface-2 px-3 py-2" open>
            <summary className="flex h-6 cursor-pointer items-center text-2xs text-fg-2">
              Genet 部署描述原文（提交冻结，平台不解析语义）
            </summary>
            <pre className="mt-2 overflow-x-auto font-mono text-2xs leading-[16px] text-fg-2">{deploy.spec}</pre>
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
            sub="多节点 / PD 分离时一个部署对应多个 Pod；点 Pod 名切到它的日志页签"
            className="border-b-0 px-5 py-3"
          />
          <div data-testid="deploy-pods">
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>Pod</TH>
                    <TH>角色</TH>
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
                          className="inline-flex h-6 items-center break-all text-left font-mono text-2xs text-primary-text hover:underline"
                          title={`查看 ${pod.name} 的日志`}
                        >
                          {pod.name}
                        </button>
                      </TD>
                      <TD>{pod.role ? <Tag mono>{pod.role}</Tag> : <span className="text-fg-3">—</span>}</TD>
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
              <span className="text-sm font-semibold text-fg">部署 Pod 日志</span>
              <span className="text-2xs text-fg-3">按 Pod 分页签，不合并；断过的地方显式标 gap；落盘文件可直接下载 .log</span>
            </div>
            <DeployLogsPanel
              taskId={task.id}
              deployState={phase ?? null}
              pods={pods}
              live={!isTerminalTask(task.status)}
              pod={selectedPod}
              onPodChange={setSelectedPod}
            />
          </div>
        </div>
      ) : (
        /* Pod 拓扑为空（排队中 / 已回收 / 外接模式）：日志端点返回 200 + 空列表 + message，
           面板按 deploy.phase 给"排队中 / 已回收 / 外接模式"的文案。 */
        <div className="border-t border-line-soft">
          <CardHeader
            tone="info"
            title="Pods"
            sub="Pod 清单来自 deploy-state.json；暂无 Pod 时仍可看排队水位与已等待时长"
            className="border-b-0 px-5 py-3"
          />
          <div data-testid="deploy-pods">
            <p className="px-5 pb-3 text-xs text-fg-3">暂无 Pod（排队中或已回收）</p>
          </div>
          <div id="deploy-logs" className="border-t border-line-soft">
            <div className="flex flex-wrap items-center gap-2 px-5 pb-1 pt-4">
              <span className="text-sm font-semibold text-fg">部署 Pod 日志</span>
              <span className="text-2xs text-fg-3">没有 Pod 时不请求日志端点（排队期 200 + message，不是 404）</span>
            </div>
            <DeployLogsPanel taskId={task.id} deployState={phase ?? null} pods={pods} pod={selectedPod} onPodChange={setSelectedPod} />
          </div>
        </div>
      )}
    </Card>
  )
}

/* ---------------------------------------------------------------- runner 详情面板 */

function RunnerDetailPanel({
  task,
  runner,
  attemptNo,
  live,
}: {
  task: TaskDetail
  runner: RunnerExecution
  attemptNo: number
  live: boolean
}) {
  const logs = useLogTail({
    streamKey: `${task.id}:${attemptNo}:${runner.seq}`,
    live: live && runner.status === 'running',
    load: (offset) => api.getRunnerLogs(task.id, runner.seq, { attempt: attemptNo, offset }),
  })

  return (
    <div className="flex flex-col gap-3 border-t border-line-soft px-5 py-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold text-fg">
          seq #{runner.seq} · {runner.name}
        </h3>
        <RunnerStatusBadge status={runner.status} />
        <Tag mono>
          {runner.runner_name}@{runner.runner_version}
        </Tag>
        <Tag>{ROLE_LABEL[runner.role]}</Tag>
        {runner.endpoint_exclusive ? <Tag>独占</Tag> : null}
        {runner.waiting_reason ? <WaitingReasonTag reason={runner.waiting_reason} /> : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">attempt #{attemptNo}</span>
      </div>

      <dl className="grid gap-x-8 gap-y-2 sm:grid-cols-2">
        <KV label="输入原文">
          <code className="break-all font-mono text-2xs text-fg-2">{runner.input_ref || '（空文件）'}</code>
        </KV>
        <KV label="执行环境">{runner.profile}</KV>
        <KV label="镜像">
          <span className="break-all font-mono text-2xs">{runner.image_ref}</span>
        </KV>
        <KV label="镜像 digest">
          <span className="break-all font-mono text-2xs text-fg-3">{runner.image_digest ?? 'unknown'}</span>
        </KV>
        <KV label="失败策略">
          {runner.on_failure === 'abort_task' ? 'abort_task（失败终止本 attempt 内后续 runner）' : 'continue'}
        </KV>
        <KV label="超时">{formatDuration(runner.timeout_seconds)}</KV>
        <KV label="退出码">{runner.exit_code ?? '—'}</KV>
        {runner.exports_ref ? (
          <KV label="exports 落档">
            <code className="break-all font-mono text-2xs text-fg-3">{runner.exports_ref}</code>
          </KV>
        ) : null}
        {runner.result_ref ? (
          <KV label="result.json">
            <code className="break-all font-mono text-2xs text-fg-3">{runner.result_ref}</code>
          </KV>
        ) : (
          <KV label="result.json">—（产物三选一：可能只产出 html / csv，见产物区，D-74）</KV>
        )}
        {runner.status_reason ? <KV label="跳过原因">{runner.status_reason}</KV> : null}
      </dl>

      {runner.error ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-danger/25 bg-danger-weak px-3 py-2 text-xs">
          <Badge tone="err" className="font-mono">
            {runner.error.code}
          </Badge>
          <span className="text-fg">{runner.error.message}</span>
          {runner.error.retryable ? <Badge tone="warn">可重试</Badge> : null}
        </div>
      ) : null}

      <LogViewer
        text={logs.text}
        loading={logs.loading}
        error={logs.error}
        coverage={logs.coverage}
        live={live && runner.status === 'running'}
        ended={!live}
        lastUpdatedAt={logs.lastUpdatedAt}
        appended={logs.appended}
        onRetry={logs.reload}
        hint={`tasks/${task.id}/attempts/${attemptNo}/runners/${runner.seq}/logs/stdout.log`}
        testIdPrefix="runner-log"
      />
    </div>
  )
}

/* ---------------------------------------------------------------- 页面 */

/** v1 的『重试部分失败』语义已取消：v2 重试 = 整 Task 新 attempt 全量重跑（§7.8 / D-69），Task 终态即可。 */
const RETRY_CONFIRM_DEPLOY = '将重新部署并全量重跑（fresh deploy，不复用已部署服务）'
const RETRY_CONFIRM_SERVICE = '将全量重跑（外接模式，无重新部署）'
/** attempt 上限（AG_ATTEMPT_MAX 默认 3，§12.7）；超限后端回 409 ATTEMPT_EXHAUSTED，前端置灰兜底。 */
const ATTEMPT_MAX_DEFAULT = 3

export function TaskDetailPage() {
  const { taskId = '' } = useParams<{ taskId: string }>()
  // 终态 Task 不再订阅事件（只补历史）：live 在拿到详情后翻转
  const [live, setLive] = useState(true)
  const { events, status, version, resyncToken, reconnect } = useTaskEvents(taskId, { live })
  // 连接态唯一来源：事件区小字、页内横幅都从这一个值派生（B5）
  const connection = connectionStateOf(status)
  const toast = useToast()
  // v2：报告为主体 —— Tab 顺序 报告 → 执行细节 → 事件 → 结果（产物清单留在结果 Tab），
  // 报告内容与 /tasks/:id/report 整页共用同一个 TaskReportView；运行中先看"报告尚未生成"的空态。
  const [view, setView] = useState<'report' | 'detail' | 'events' | 'result'>('report')
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const [actionError, setActionError] = useState<ApiErrorBody | null>(null)
  // 重试：整 Task 新 attempt（二次确认"将重新部署并全量重跑"）
  const [retryOpen, setRetryOpen] = useState(false)
  const [retrying, setRetrying] = useState(false)
  const [retryExhausted, setRetryExhausted] = useState(false)
  // 选中的 attempt（?attempt 语义在组件内；重试只追加不覆盖，历史 attempt 只读）
  const [attemptNo, setAttemptNo] = useState<number | null>(null)

  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)

  const task = useAsync(() => api.getTask(taskId), [taskId, version, resyncToken])

  // 只在"首屏"与"刚跑完"这两个时刻决定默认视图；
  // 数据刷新（SSE 推 version）不能把用户手选的页签顶回去
  const lastStatusRef = useRef<string | null>(null)
  useEffect(() => {
    if (!task.data) return
    const status = task.data.status
    const done = isTerminalTask(status)
    setLive(!done)
    const prev = lastStatusRef.current
    const firstLoad = prev === null
    const justFinished = prev !== null && !isTerminalTask(prev) && done
    if (firstLoad || justFinished) setView('report')
    lastStatusRef.current = status
  }, [task.data])

  const detail = task.data
  // attempt 展示列表：默认最后一个（重试是追加不是覆盖，§7.2）
  const attempts = detail?.attempts ?? []
  const activeAttemptNo = attemptNo ?? attempts[attempts.length - 1]?.attempt_no ?? 0
  // 当前展示的 runner 列表：最新 attempt 用详情自带的 runners[]，历史 attempt 显式拉取
  const isLatestAttempt = detail != null && activeAttemptNo === detail.current_attempt
  const attemptRunners = useAsync(async () => {
    if (!detail) return []
    if (isLatestAttempt) return detail.runners
    const data = await api.listTaskRunners(taskId, { attempt: activeAttemptNo })
    return data
  }, [detail?.id, version, activeAttemptNo, isLatestAttempt])
  const runners = useMemo(() => {
    if (isLatestAttempt && detail) return detail.runners
    return attemptRunners.data ?? []
  }, [isLatestAttempt, detail, attemptRunners.data])

  // 选中节点（runner seq）；默认选第一个未终态的（串行管道里"正在跑"的那一个）
  const [selectedSeq, setSelectedSeq] = useState<number | null>(null)
  const selectedRunner = useMemo(() => {
    if (runners.length === 0) return null
    if (selectedSeq != null) return runners.find((r) => r.seq === selectedSeq) ?? null
    return runners.find((r) => r.status === 'running' || r.status === 'preparing') ?? null
  }, [runners, selectedSeq])

  useEffect(() => {
    setSelectedSeq(null)
    setRetryExhausted(false)
  }, [taskId, activeAttemptNo])

  const cancel = async () => {
    setCancelling(true)
    setActionError(null)
    try {
      await api.cancelTask(taskId)
      setConfirmOpen(false)
      toast({
        title: '已提交取消请求',
        description: '先终止当前 runner → 执行 service-clean（若含部署）→ 落 canceled；状态由服务端推送',
        tone: 'warn',
      })
      task.reload()
    } catch (e) {
      setActionError(toApiError(e))
      setConfirmOpen(false)
    } finally {
      setCancelling(false)
    }
  }

  const confirmRetry = async () => {
    setRetrying(true)
    setRetryExhausted(false)
    try {
      const accepted = await api.retryTask(taskId)
      setRetryOpen(false)
      toast({
        title: `已提交重试：attempt #${accepted.attempt_no}`,
        description: '整 Task 全量重跑含重新部署（fresh deploy）；历史 attempt 保留只读',
        tone: 'ok',
      })
      setAttemptNo(accepted.attempt_no)
      task.reload()
    } catch (e) {
      const error = toApiError(e)
      if (error.code === 'ATTEMPT_EXHAUSTED') {
        // 409 ATTEMPT_EXHAUSTED：按钮置灰 + 提示上限（默认 3，§12.7）
        setRetryExhausted(true)
        setRetryOpen(false)
        toast({
          title: '已达到 attempt 上限',
          description: `${error.code}：${error.message}（默认上限 3）`,
          tone: 'err',
        })
      } else {
        toast({
          title: error.code === 'CONFLICT' ? 'Task 还在执行，不能重试' : '重试失败',
          description: `${error.code}：${error.message}`,
          tone: 'err',
        })
        setRetryOpen(false)
      }
    } finally {
      setRetrying(false)
    }
  }

  if (task.error) {
    /**
     * 取到 404（对象不存在 / 已被清理）不是「加载失败」—— 它是**页面不存在**，
     * 走全站统一的 404 组件（同一个组件、按上下文换文案），不摆红色错误条让人重试。
     */
    if (task.error.code === 'NOT_FOUND') {
      return (
        <NotFoundPage
          title="Task 不存在或已被清理"
          description="这个 Task 可能已经被清理（保留期 30 天），或者 ID 打错了。可以回到概览，或从 Task 表里找最近的记录。"
        />
      )
    }
    return (
      <Card>
        <CardHeader tone="err" title="加载 Task 失败" />
        <CardBody>
          <ErrorState error={task.error} onRetry={task.reload} />
          <div className="mt-4">
            <Link to="/">
              <Button icon={<ArrowLeft className="size-3.5" aria-hidden />}>返回概览</Button>
            </Link>
          </div>
        </CardBody>
      </Card>
    )
  }

  const terminal = detail ? isTerminalTask(detail.status) : false
  const counts = detail?.runner_counts
  const retryBlocked = blocked ?? (retryExhausted ? '已达到 attempt 上限（默认 3），不能再次重试' : null)

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <Link to="/">
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回概览
          </Button>
        </Link>
        {detail && !terminal ? (
          <Button
            size="sm"
            variant="dangerGhost"
            data-testid="cancel-task"
            disabled={Boolean(blocked)}
            title={blocked ?? undefined}
            onClick={() => setConfirmOpen(true)}
          >
            取消 Task
          </Button>
        ) : null}
        {detail && terminal ? (
          <Button
            size="sm"
            data-testid="retry-task"
            disabled={Boolean(retryBlocked)}
            title={retryBlocked ?? '整 Task 新 attempt 全量重跑（含重新部署）'}
            onClick={() => setRetryOpen(true)}
          >
            重试（新 attempt）
          </Button>
        ) : null}
        {terminal ? (
          <Link to={`/tasks/${encodeURIComponent(taskId)}/report`} data-testid="open-report">
            <Button size="sm">查看报告</Button>
          </Link>
        ) : null}
        <span className="ml-auto font-mono text-2xs text-fg-3">{taskId}</span>
      </div>

      {/* 断线横幅（主内容顶部）：连接态与事件区同源，恢复后自动消失 */}
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

      {task.loading && !detail ? (
        <Card>
          <CardHeader tone="muted" title="加载 Task 详情" />
          <CardBody className="flex flex-col gap-3">
            <Skeleton className="h-3 w-1/3" />
            <Skeleton className="h-3 w-2/3" />
            <Skeleton className="h-3 w-1/2" />
          </CardBody>
        </Card>
      ) : null}

      {detail ? (
        <Tabs<string>
          ariaLabel="Task 详情视图"
          idBase="task-view"
          value={view}
          onChange={(next: string) => setView(next as 'report' | 'detail' | 'events' | 'result')}
          items={[
            { value: 'report', label: '报告' },
            { value: 'detail', label: '执行细节' },
            { value: 'events', label: '事件' },
            ...(terminal ? [{ value: 'result', label: '结果' }] : []),
          ]}
          className="rounded-xl border border-line bg-surface px-3 pt-1 shadow-xs"
        />
      ) : null}

      {/* 报告页签（默认）：与 /tasks/:id/report 整页共用同一个内容区；产物清单留在「结果」Tab */}
      {detail && view === 'report' ? (
        <section {...tabPanelProps('task-view', 'report')} data-testid="task-report-panel" className="flex flex-col gap-4">
          <TaskReportView taskId={taskId} />
        </section>
      ) : null}

      {/* 事件页签：右栏取消后搬进主内容的第三个页签（折叠 ≠ 隐藏） */}
      {detail && view === 'events' ? (
        <section {...tabPanelProps('task-view', 'events')} data-testid="task-events-panel" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="text-base font-semibold text-fg">事件</h2>
            <span className="text-2xs text-fg-3">
              {terminal
                ? 'Task 已结束 · 下面是历史事件'
                : connection === 'live'
                  ? '实时更新中 · 断开后会自动重连'
                  : CONNECTION_LABEL[connection]}
            </span>
            <span className="ml-auto text-2xs text-fg-3">共 {events.length} 条</span>
          </div>
          <Card>
            <CardBody>
              <EventTimeline events={events} connection={connection} ended={terminal} />
            </CardBody>
            <CardFooter>
              <span>
                页面上的状态跟着服务端推送走，不会先替你改成「已完成」；日志不在事件流里 —— 点开某个 runner 才读它那一次的日志。
              </span>
            </CardFooter>
          </Card>
        </section>
      ) : null}

      {/* 页签与面板配对：role=tabpanel + id / aria-controls / aria-labelledby（B7） */}
      {detail && terminal && view === 'result' ? (
        <section {...tabPanelProps('task-view', 'result')} className="flex flex-col gap-4">
          <TaskResultView task={detail} />
        </section>
      ) : null}

      {detail && view === 'detail' ? (
        <section {...tabPanelProps('task-view', 'detail')} className="flex flex-col gap-4">
          <Card>
            <CardHeader
              tone="muted"
              title={
                <span className="flex flex-wrap items-center gap-2">
                  {/* 页面内 h1（对象名）：19px/600，压过卡头 15px 的普通标题 */}
                  <h1 className="text-xl font-semibold leading-tight tracking-[-0.02em]">{detail.name}</h1>
                  <TaskStatusBadge status={detail.status} />
                  <Tag mono>{detail.id}</Tag>
                  {detail.retry_of ? <Tag title={`续跑自 ${detail.retry_of}`}>retry_of</Tag> : null}
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
                <KV label="提交人">{detail.submitted_by}</KV>
                <KV label="提交时间">{formatDateTime(detail.submitted_at)}</KV>
                <KV label="开始时间">{formatDateTime(detail.started_at)}</KV>
                <KV label="结束时间">{formatDateTime(detail.finished_at)}</KV>
                <KV label="耗时">{formatDuration(runDuration(detail))}</KV>
                <KV label="当前 attempt">{detail.current_attempt === 0 ? '—（尚未开始）' : `#${detail.current_attempt}`}</KV>
                <KV label="runner 进度">
                  {counts ? `${counts.succeeded} / ${counts.total} 成功${counts.failed ? ` · ${counts.failed} 失败` : ''}` : '—'}
                </KV>
                <KV label="Endpoint">
                  {detail.endpoint ? (
                    <span className="break-all font-mono text-2xs">{detail.endpoint.url}</span>
                  ) : (
                    '—（没有部署）'
                  )}
                </KV>
                <KV label="模型">{detail.endpoint?.model_name ?? '—'}</KV>
                {detail.secret_env && detail.secret_env.length > 0 ? (
                  <KV label="凭据引用">
                    {detail.secret_env.map((s) => (
                      <Tag key={s.name} mono className="mr-1">
                        {s.name} → {s.secret_ref}
                      </Tag>
                    ))}
                  </KV>
                ) : null}
                <KV label="revision">{detail.revision ?? '—'}</KV>
              </dl>

              {/* attempt 切换：列表永远全量展示（重试是追加不是覆盖，§7.2）；历史 attempt 只读 */}
              {attempts.length > 0 ? (
                <div className="flex flex-wrap items-center gap-1.5">
                  {attempts.map((attempt) => (
                    <button
                      key={attempt.attempt_no}
                      type="button"
                      onClick={() => setAttemptNo(attempt.attempt_no)}
                      className={cn(
                        'rounded-full border px-2.5 py-1 text-2xs transition-colors',
                        activeAttemptNo === attempt.attempt_no
                          ? 'border-primary bg-primary-weak text-primary-text'
                          : 'border-line-strong text-fg-2 hover:bg-surface-2',
                      )}
                    >
                      attempt #{attempt.attempt_no} · {attemptStatusMeta(attempt.status).label}
                      {attempt.trigger === 'retry' ? ' · 重试' : ''}
                    </button>
                  ))}
                  <span className="ml-auto text-2xs text-fg-3">
                    {isLatestAttempt ? '最新 attempt' : '历史 attempt（只读）'} · 重试追加新 attempt，不覆盖历史
                  </span>
                </div>
              ) : (
                <p className="text-2xs text-fg-3">还没有 attempt（Task 在进入 running 时创建 attempt）。</p>
              )}
            </CardBody>
          </Card>

          <Card>
            <CardHeader
              tone="primary"
              title="runner 管道时间线"
              sub="严格串行：service-deploy? → pre_check? → tests → service-clean?（v2 无并行分支，D-69/D-70）；点任一节点看详情"
              actions={
                <span className="text-2xs text-fg-3">
                  attempt #{activeAttemptNo || '—'} · {runners.length} 个节点
                </span>
              }
            />
            <CardBody>
              {runners.length === 0 ? (
                <p className="text-2xs text-fg-3">该 attempt 还没有 runner 执行记录。</p>
              ) : (
                <ol className="flex items-stretch gap-2 overflow-x-auto pb-1" aria-label="runner 管道时间线">
                  {runners.map((runner, index) => (
                    <li key={runner.seq} className="flex shrink-0 items-center gap-2" data-chain-node>
                      <PipelineNode
                        name={runner.name}
                        role={runner.role}
                        status={runner.status}
                        meta={runnerMeta(runner)}
                        tone={runnerStatusMeta(runner.status).tone}
                        active={selectedRunner?.seq === runner.seq}
                        onClick={() => setSelectedSeq(runner.seq)}
                      />
                      {index === runners.length - 1 ? null : (
                        <span data-testid="task-chain-arrow" className="text-fg-3" aria-hidden>
                          →
                        </span>
                      )}
                    </li>
                  ))}
                </ol>
              )}
              <p className="mt-2 text-2xs text-fg-3">
                管道在 attempt 内按 seq 严格串行执行恰好一次；runner 没有自己的重试（FAILED 是终态，D-69）。
                {deployStateMeta(detail.deploy?.phase).label !== '未启动'
                  ? ` 当前部署阶段：${deployStateMeta(detail.deploy?.phase).label}。`
                  : ''}
              </p>
            </CardBody>
          </Card>

          {/* deploy 节点：deploy-state.json 的 phase / 水位 / 已等待时长（无位次，D-46）+ Pod 分页签日志 */}
          {detail.deploy ? <DeploySection task={detail} version={version} /> : null}

          {/* 选中 runner 的详情（含日志）；deploy / clean 的日志也在各自节点下读 */}
          {selectedRunner ? (
            <Card>
              <CardHeader tone="muted" title={`runner 详情 · seq #${selectedRunner.seq}`} sub="输入落档、冻结快照与执行日志" />
              <RunnerDetailPanel
                task={detail}
                runner={selectedRunner}
                attemptNo={activeAttemptNo || 1}
                live={!terminal && isLatestAttempt}
              />
            </Card>
          ) : null}

          <Card>
            <CardFooter>
              <span>
                事件共 {events.length} 条 · {terminal ? 'Task 已结束，显示历史事件' : CONNECTION_LABEL[connection]}
              </span>
            </CardFooter>
          </Card>
        </section>
      ) : null}

      <Dialog
        open={confirmOpen}
        onClose={() => setConfirmOpen(false)}
        title="取消这个 Task？"
        description="取消后不可逆；状态以服务端推送为准（先 cancelling，clean 收口后才落 canceled）。"
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
            <p className="font-medium text-fg">将标记 SKIPPED（未执行）</p>
            <p className="mt-1 text-2xs text-fg-3">还没开始的 runner（pending / queued），reason=canceled。</p>
          </div>
          <div className="rounded-md border border-line-soft bg-surface-2 p-3">
            <p className="font-medium text-fg">将终止并标记 CANCELED</p>
            <p className="mt-1 text-2xs text-fg-3">正在跑的 runner（preparing / running）。</p>
          </div>
          {detail?.deploy && ['queued', 'accepted', 'deploying'].includes(detail.deploy.phase ?? '') ? (
            <Banner tone="warn" title="当前部署未就绪">
              取消后会执行 service-clean（拆除已部署资源，finally 语义）；clean 完成才落终态，可能不是即时生效。
            </Banner>
          ) : detail?.deploy ? (
            <Banner tone="warn" title="取消会执行 service-clean">
              拆除已部署资源（finally 语义）；clean 完成才落终态，可能不是即时生效。
            </Banner>
          ) : null}
        </div>
      </Dialog>

      {/* 重试确认：整 Task 新 attempt 全量重跑（§7.8 / D-69）——没有"只重跑失败的"这种选项 */}
      <Dialog
        open={retryOpen}
        onClose={() => setRetryOpen(false)}
        title="重试这个 Task？"
        description="重试 = 新 attempt 全量重跑：重新生成全部 runner 执行记录，历史 attempt 保留只读。"
        footer={
          <>
            <Button onClick={() => setRetryOpen(false)}>取消</Button>
            <Button
              variant="primary"
              data-testid="retry-confirm"
              loading={retrying}
              onClick={() => void confirmRetry()}
            >
              确认重试
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3 text-xs">
          <Banner role="alert" tone={detail?.deploy ? 'warn' : 'info'} title={`attempt #${(detail?.current_attempt ?? 0) + 1}`}>
            {detail?.deploy ? RETRY_CONFIRM_DEPLOY : RETRY_CONFIRM_SERVICE}
          </Banner>
          <ul className="flex flex-col gap-1.5 rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-[16px] text-fg-2">
            <li>
              新 attempt：<span className="font-mono">#{(detail?.current_attempt ?? 0) + 1}</span>
              ，历史 attempt（共 {detail?.attempts.length ?? 0} 个）保留只读。
            </li>
            {detail?.deploy ? (
              <li>
                fresh deploy：release 名按 <span className="font-mono">(task_id, attempt_no)</span> 重新派生，
                <strong>不复用</strong>上一次已部署的服务（避免 warm cache 污染测量，§7.8）。
              </li>
            ) : null}
            <li>输入快照原样重用；Runner / 镜像引用按提交冻结的 resolved 重放。</li>
            <li>
              attempt 上限 {ATTEMPT_MAX_DEFAULT} 个（§12.7）；达到上限时服务端返回{' '}
              <span className="font-mono">409 ATTEMPT_EXHAUSTED</span>。
            </li>
          </ul>
        </div>
      </Dialog>
    </>
  )
}
