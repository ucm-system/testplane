import { Badge, Tag } from '@/components/ui/badge'
import { deployStateMeta, runStatusMeta, taskStatusMeta, waitingReasonLabel } from '@/lib/status'

/** 状态徽标三件套：Run / Task / Deploy。文字 + 色，不靠颜色单独表意。 */

export function RunStatusBadge({ status, className }: { status: string; className?: string }) {
  const meta = runStatusMeta(status)
  return (
    <Badge tone={meta.tone} className={className} title={status}>
      {meta.label}
    </Badge>
  )
}

export function TaskStatusBadge({ status, className }: { status: string; className?: string }) {
  const meta = taskStatusMeta(status)
  return (
    <Badge tone={meta.tone} className={className} title={status}>
      {meta.label}
    </Badge>
  )
}

export function DeployStateBadge({ state, className }: { state: string; className?: string }) {
  const meta = deployStateMeta(state)
  return (
    <Badge tone={meta.tone} className={className} title={state}>
      {meta.label}
    </Badge>
  )
}

/** 排队原因（waiting_reason）的中文胶囊。 */
export function WaitingReasonTag({ reason }: { reason: string | null | undefined }) {
  const label = waitingReasonLabel(reason)
  if (!label) return null
  return <Tag>{label}</Tag>
}
