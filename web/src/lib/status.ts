/**
 * 状态 → 中文标签 + 语义色（design/views/02 的状态机；原型里的展示口径）。
 *
 * 纪律：
 *  - 状态 token 一律小写（跟 API 一致），UI 展示中文；
 *  - 状态不只靠颜色（design/impl/web-and-delivery.md §6）：Badge 一律「文字 + 色」。
 */
import type { AttemptStatus, DeployState, RunStatus, TaskStatus, WaitingReason } from '@/api/types'

export type Tone = 'ok' | 'warn' | 'err' | 'info' | 'primary' | 'muted'

export interface StatusMeta {
  label: string
  tone: Tone
}

export const RUN_STATUS_META: Record<RunStatus, StatusMeta> = {
  submitted: { label: '已提交', tone: 'muted' },
  queued: { label: '排队中', tone: 'muted' },
  deploying: { label: '部署中', tone: 'info' },
  testing: { label: '执行中', tone: 'info' },
  cleanup: { label: '清理中', tone: 'info' },
  cancelling: { label: '取消中', tone: 'warn' },
  succeeded: { label: '成功', tone: 'ok' },
  completed_with_errors: { label: '部分失败', tone: 'warn' },
  service_unavailable: { label: '服务不可用', tone: 'err' },
  failed: { label: '平台失败', tone: 'err' },
  canceled: { label: '已取消', tone: 'muted' },
}

export const TASK_STATUS_META: Record<TaskStatus, StatusMeta> = {
  pending: { label: '待调度', tone: 'muted' },
  queued: { label: '排队中', tone: 'muted' },
  preparing: { label: '准备中', tone: 'info' },
  running: { label: '执行中', tone: 'info' },
  succeeded: { label: '成功', tone: 'ok' },
  failed: { label: '失败', tone: 'err' },
  skipped: { label: '跳过', tone: 'warn' },
  canceled: { label: '已取消', tone: 'muted' },
}

export const ATTEMPT_STATUS_META: Record<AttemptStatus, StatusMeta> = {
  pending: { label: '待开始', tone: 'muted' },
  preparing: { label: '准备中', tone: 'info' },
  running: { label: '执行中', tone: 'info' },
  succeeded: { label: '成功', tone: 'ok' },
  failed: { label: '失败', tone: 'err' },
  canceled: { label: '已取消', tone: 'muted' },
}

export const DEPLOY_STATE_META: Record<DeployState, StatusMeta> = {
  queued: { label: '排队中', tone: 'muted' },
  accepted: { label: '已受理', tone: 'info' },
  deploying: { label: '部署中', tone: 'info' },
  ready: { label: '就绪', tone: 'ok' },
  blocked: { label: '被阻塞', tone: 'err' },
  failed: { label: '失败', tone: 'err' },
  canceled: { label: '已取消', tone: 'muted' },
}

/** 排队原因的中文枚举（design/impl/web-and-delivery.md §4.2）。 */
export const WAITING_REASON_CN: Record<WaitingReason, string> = {
  precheck_gate: '等待探活',
  slot_waiting: '平台槽位',
  global_limit: '平台并发',
  queue_full: '队列已满',
  insufficient_resources: '资源不足',
  gang_pending: 'gang 未满足',
}

export function waitingReasonLabel(reason: string | null | undefined): string {
  if (!reason) return ''
  return WAITING_REASON_CN[reason as WaitingReason] ?? reason
}

export function runStatusMeta(status: string): StatusMeta {
  return RUN_STATUS_META[status as RunStatus] ?? { label: status, tone: 'muted' }
}

export function taskStatusMeta(status: string): StatusMeta {
  return TASK_STATUS_META[status as TaskStatus] ?? { label: status, tone: 'muted' }
}

export function attemptStatusMeta(status: string): StatusMeta {
  return ATTEMPT_STATUS_META[status as AttemptStatus] ?? { label: status, tone: 'muted' }
}

export function deployStateMeta(state: string): StatusMeta {
  return DEPLOY_STATE_META[state as DeployState] ?? { label: state, tone: 'muted' }
}

/** Run 是否终态（终态后不再订阅事件、不显示取消按钮）。 */
export function isTerminalRun(status: string): boolean {
  return ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(status)
}

/** 终态语义说明（列表页右栏图例，来自 Prototype runs.png）。 */
export const TERMINAL_LEGEND: Array<{ status: RunStatus; desc: string }> = [
  { status: 'succeeded', desc: '全部 Task 成功' },
  { status: 'completed_with_errors', desc: '流程走完但有失败' },
  { status: 'service_unavailable', desc: '服务没起来，找部署方' },
  { status: 'failed', desc: '平台侧问题，找平台' },
  { status: 'canceled', desc: '人主动停的，不算失败' },
]

/** 事件类型 → 中文（事件时间线用）。 */
export const EVENT_TYPE_CN: Record<string, string> = {
  'run.submitted': 'Run 提交',
  'run.state_changed': 'Run 状态变化',
  'task.state_changed': 'Task 状态变化',
  'attempt.started': 'Attempt 开始',
  'attempt.finished': 'Attempt 结束',
  'deploy.state_changed': 'Deploy 状态变化',
  'cleanup.started': '开始清理',
  'run.finished': 'Run 结束',
  'report.written': '报告已写',
  resync_required: '需要重建',
}
