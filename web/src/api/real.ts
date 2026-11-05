/**
 * 真实接口实现 —— 端点与响应形状以 `server/api/openapi.yaml` 为准。
 * 基址由 api/client.ts 的 `API_BASE` 拼接（默认 `/api/v1`），这里只写资源路径。
 */
import type { Api, LogOptions } from './contract'
import { qs, request, requestText } from './client'
import { asArray, asPage, normalizeTester } from './normalize'
import { applyIdentity, clearAuth } from './auth'
import type {
  AdminCluster,
  AdminSecret,
  ArtifactList,
  Attempt,
  AuditEntry,
  AuditQuery,
  CancelResult,
  Cluster,
  ClusterWrite,
  DashboardStats,
  DeployLogQuery,
  DeployLogs,
  DeploySnapshot,
  DryRunResult,
  ExecutionProfile,
  Identity,
  Meta,
  Page,
  ProfileWrite,
  QueueStatus,
  Report,
  RetryPlan,
  Run,
  RunEvent,
  RunListQuery,
  RunManifest,
  RunSummary,
  SecretWrite,
  SubmitResult,
  Task,
  TaskCancelResult,
  TaskDetail,
  TaskListQuery,
  TaskRetryAccepted,
  Tester,
  TesterListQuery,
  WaitingReasonCounts,
} from './types'

/** `QueueStatus` 的防御性归一：契约保证 slots/waiting 必填，但别让页面因缺字段崩掉。 */
function normalizeQueueStatus(raw: unknown): QueueStatus {
  const rec = (raw ?? {}) as Record<string, unknown>
  const slots = (rec['slots'] ?? {}) as Record<string, unknown>
  const waiting = (rec['waiting'] ?? {}) as Record<string, unknown>
  return {
    generated_at: typeof rec['generated_at'] === 'string' ? rec['generated_at'] : undefined,
    slots: {
      concurrency_max: Number(slots['concurrency_max'] ?? 0),
      active: Number(slots['active'] ?? 0),
      available: Number(slots['available'] ?? 0),
      endpoints: Array.isArray(slots['endpoints']) ? (slots['endpoints'] as QueueStatus['slots']['endpoints']) : [],
    },
    waiting: {
      total: Number(waiting['total'] ?? 0),
      by_reason: (waiting['by_reason'] ?? {
        precheck_gate: 0,
        slot_waiting: 0,
        global_limit: 0,
        queue_full: 0,
        insufficient_resources: 0,
        gang_pending: 0,
      }) as WaitingReasonCounts,
      tasks: Array.isArray(waiting['tasks']) ? (waiting['tasks'] as QueueStatus['waiting']['tasks']) : [],
    },
    volcano: (rec['volcano'] ?? undefined) as QueueStatus['volcano'],
  }
}
export const realApi: Api = {
  async getIdentity(): Promise<Identity> {
    const identity = await request<Identity>('/me')
    // 有 csrf_token 即 OIDC 会话；没有即 dev 模式（判定逻辑集中在 api/auth.ts）
    applyIdentity(identity)
    return identity
  },

  async logout(): Promise<void> {
    await request<unknown>('/auth/logout', { method: 'POST' })
    clearAuth()
  },

  getMeta(): Promise<Meta> {
    return request<Meta>('/meta')
  },

  async listRuns(query: RunListQuery = {}): Promise<Page<RunSummary>> {
    const data = await request<unknown>(
      `/runs${qs({
        status: query.status,
        kind: query.kind,
        tester: query.tester,
        submitter: query.submitter,
        cursor: query.cursor ?? undefined,
        limit: query.limit,
      })}`,
    )
    return asPage<RunSummary>(data)
  },

  getRun(id: string): Promise<Run> {
    return request<Run>(`/runs/${encodeURIComponent(id)}`)
  },

  cancelRun(id: string): Promise<CancelResult> {
    return request<CancelResult>(`/runs/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
  },

  async listTasks(query: TaskListQuery = {}): Promise<Page<Task>> {
    const data = await request<unknown>(
      `/tasks${qs({
        status: query.status,
        tester: query.tester,
        cursor: query.cursor ?? undefined,
        limit: query.limit,
      })}`,
    )
    return asPage<Task>(data)
  },

  getTask(id: string): Promise<TaskDetail> {
    return request<TaskDetail>(`/tasks/${encodeURIComponent(id)}`)
  },

  cancelTask(id: string): Promise<TaskCancelResult> {
    return request<TaskCancelResult>(`/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
  },

  async listAttempts(taskId: string): Promise<Attempt[]> {
    const data = await request<unknown>(`/tasks/${encodeURIComponent(taskId)}/attempts`)
    return asArray<Attempt>(data).sort((a, b) => a.idx - b.idx)
  },

  async getAttemptLogs(taskId: string, idx: number, opts: LogOptions = {}) {
    const text = await requestText(
      `/tasks/${encodeURIComponent(taskId)}/attempts/${idx}/logs${qs({
        stream: opts.stream,
        offset: opts.offset,
        limit: opts.limit,
        tail: opts.tail,
        follow: opts.follow ? 1 : undefined,
      })}`,
      { headers: { Accept: 'text/plain' } },
    )
    return { text: text.text, offset: text.offset, coverage: text.coverage }
  },

  getDeploy(runId: string): Promise<DeploySnapshot> {
    return request<DeploySnapshot>(`/runs/${encodeURIComponent(runId)}/deploy`)
  },

  async getEventsHistory(runId: string, sinceSeq?: number): Promise<RunEvent[]> {
    // `EventList`：`next_cursor` 是本页末条 seq，续取时作为 since_seq 传回（契约明确不接受 cursor）
    const data = await request<unknown>(
      `/runs/${encodeURIComponent(runId)}/events/history${qs({ since_seq: sinceSeq, limit: 1000 })}`,
    )
    return asArray<RunEvent>(data).sort((a, b) => a.seq - b.seq)
  },

  async listTesters(query: TesterListQuery = {}): Promise<Tester[]> {
    const data = await request<unknown>(
      `/testers${qs({
        role: query.role,
        orphaned: query.orphaned === undefined ? undefined : String(query.orphaned),
      })}`,
    )
    return asArray<unknown>(data).map(normalizeTester)
  },

  async getTester(name: string): Promise<Tester> {
    return normalizeTester(await request<unknown>(`/testers/${encodeURIComponent(name)}`))
  },

  async listClusters(): Promise<Cluster[]> {
    const data = await request<unknown>('/clusters')
    return asArray<Cluster>(data)
  },

  async getQueueStatus(): Promise<QueueStatus> {
    return normalizeQueueStatus(await request<unknown>('/queue-status'))
  },

  submitRun(manifest: RunManifest): Promise<SubmitResult> {
    return request<SubmitResult>('/runs', { method: 'POST', body: JSON.stringify(manifest) })
  },

  dryRun(manifest: RunManifest): Promise<DryRunResult> {
    return request<DryRunResult>(`/runs${qs({ dry_run: 'true' })}`, {
      method: 'POST',
      body: JSON.stringify(manifest),
    })
  },

  /**
   * Deploy 日志：**`?pod=` 必选**（缺了服务端 400，指定不存在的 pod 404）。
   * `tail_lines` 与 `offset` 二选一 —— 同时传会 400，所以"要看最新"时干脆不带 offset。
   */
  getDeployLogs(runId: string, query: DeployLogQuery & { pod: string }): Promise<DeployLogs> {
    const useTail = Boolean(query.tail_lines && query.tail_lines > 0)
    return request<DeployLogs>(
      `/runs/${encodeURIComponent(runId)}/deploy/logs${qs({
        pod: query.pod,
        offset: useTail ? undefined : (query.offset ?? 0),
        limit: query.limit,
        tail_lines: useTail ? query.tail_lines : undefined,
      })}`,
    )
  },

  /** 产物清单：与 `/files?path=` 配对使用（下载链接直接拼 API_BASE） */
  listRunArtifacts(runId: string): Promise<ArtifactList> {
    return request<ArtifactList>(`/runs/${encodeURIComponent(runId)}/artifacts`)
  },

  /* 报告（契约内） */
  getReport(runId: string): Promise<Report> {
    return request<Report>(`/runs/${encodeURIComponent(runId)}/report`)
  },

  async getReportMd(runId: string): Promise<string> {
    const text = await requestText(`/runs/${encodeURIComponent(runId)}/report.md`, {
      headers: { Accept: 'text/markdown' },
    })
    return text.text
  },

  /* Profile（契约内只读） */
  async listProfiles(): Promise<ExecutionProfile[]> {
    const data = await request<unknown>('/profiles')
    return asArray<ExecutionProfile>(data)
  },

  /* 重试（P1：按 Lead 约定路径实现） */
  getRetryPlan(runId: string): Promise<RetryPlan> {
    return request<RetryPlan>(`/runs/${encodeURIComponent(runId)}/retry-plan`)
  },

  retryRun(runId: string): Promise<RetryPlan> {
    return request<RetryPlan>(`/runs/${encodeURIComponent(runId)}/retry`, { method: 'POST' })
  },

  retryTask(taskId: string): Promise<TaskRetryAccepted> {
    return request<TaskRetryAccepted>(`/tasks/${encodeURIComponent(taskId)}/retry`, { method: 'POST' })
  },

  /* 管理（P1：仅 admin） */
  async listAdminClusters(): Promise<AdminCluster[]> {
    const data = await request<unknown>('/admin/clusters')
    return asArray<AdminCluster>(data)
  },

  createAdminCluster(body: ClusterWrite): Promise<AdminCluster> {
    return request<AdminCluster>('/admin/clusters', { method: 'POST', body: JSON.stringify(body) })
  },

  /**
   * 更新集群登记：服务端注册的是 `PUT /admin/clusters`（**名字在 body 里，不是路径参数**），
   * 契约见 openapi `/api/v1/admin/clusters` 的 put。带 `?name=` 是兼容写法（服务端忽略）。
   */
  updateAdminCluster(name: string, body: ClusterWrite): Promise<AdminCluster> {
    return request<AdminCluster>(`/admin/clusters${qs({ name })}`, {
      method: 'PUT',
      body: JSON.stringify({ ...body, name: body.name || name }),
    })
  },

  /** 删除集群：`DELETE /admin/clusters?name=<name>`（openapi `NameQuery`）。 */
  async deleteAdminCluster(name: string): Promise<void> {
    await request<unknown>(`/admin/clusters${qs({ name })}`, { method: 'DELETE' })
  },

  createProfile(body: ProfileWrite): Promise<ExecutionProfile> {
    return request<ExecutionProfile>('/admin/profiles', { method: 'POST', body: JSON.stringify(body) })
  },

  /** 更新 Profile：同 cluster，`PUT /admin/profiles` + body 里的 name。 */
  updateProfile(name: string, body: ProfileWrite): Promise<ExecutionProfile> {
    return request<ExecutionProfile>(`/admin/profiles${qs({ name })}`, {
      method: 'PUT',
      body: JSON.stringify({ ...body, name: body.name || name }),
    })
  },

  /** 删除 Profile：`DELETE /admin/profiles?name=<name>`。 */
  async deleteProfile(name: string): Promise<void> {
    await request<unknown>(`/admin/profiles${qs({ name })}`, { method: 'DELETE' })
  },

  async listAdminSecrets(): Promise<AdminSecret[]> {
    const data = await request<unknown>('/admin/secrets')
    return asArray<AdminSecret>(data)
  },

  createAdminSecret(body: SecretWrite): Promise<AdminSecret> {
    return request<AdminSecret>('/admin/secrets', { method: 'POST', body: JSON.stringify(body) })
  },

  async listAudit(query: AuditQuery = {}): Promise<Page<AuditEntry>> {
    const data = await request<unknown>(
      `/admin/audit${qs({ action: query.action, cursor: query.cursor ?? undefined, limit: query.limit })}`,
    )
    return asPage<AuditEntry>(data)
  },

  async getDashboardStats(): Promise<DashboardStats> {
    // P0 没有 /board：用列表页的同一份数据 + 队列状态现算
    const [runs, testers, queue] = await Promise.all([
      realApi.listRuns({ limit: 200 }),
      realApi.listTesters(),
      realApi.getQueueStatus().catch(() => null),
    ])
    const items = runs.items
    const active = items.filter((r) =>
      ['submitted', 'queued', 'deploying', 'testing', 'cleanup', 'cancelling'].includes(r.status),
    )
    const terminal = items.filter((r) =>
      ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(r.status),
    )
    const succeeded = terminal.filter((r) => r.status === 'succeeded').length
    const dayAgo = Date.now() - 24 * 3600 * 1000
    const failed24h = items.filter(
      (r) =>
        ['failed', 'service_unavailable', 'completed_with_errors'].includes(r.status) &&
        new Date(r.submitted_at).getTime() >= dayAgo,
    ).length
    const queuedTasks =
      queue?.waiting?.total ??
      items
        .filter((r) => ['queued', 'deploying'].includes(r.status))
        .reduce((sum, r) => sum + (r.task_counts?.pending ?? 0), 0)

    return {
      active_runs: active.length,
      queued_tasks: queuedTasks,
      failed_24h: failed24h,
      success_rate: terminal.length === 0 ? null : succeeded / terminal.length,
      total_runs: items.length,
      tester_count: testers.length,
    }
  },
}
