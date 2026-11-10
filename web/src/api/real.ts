/**
 * 真实接口实现 —— 端点与响应形状以 `server/api/openapi.yaml`（schema 2.0）为准。
 * 基址由 api/client.ts 的 `API_BASE` 拼接（默认 `/api/v1`），这里只写资源路径。
 */
import type { Api, LogOptions } from './contract'
import { API_BASE, ApiClientError, authHeaders, normalizeErrorBody, qs, request, requestText } from './client'
import { asArray, asPage, normalizeRunner } from './normalize'
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
  LogChunk,
  Meta,
  Page,
  ProfileWrite,
  QueueStatus,
  Report,
  Result,
  Runner,
  RunnerExecution,
  RunnerListQuery,
  SecretWrite,
  SharedMount,
  SharedMountPatch,
  SharedMountWrite,
  SubmitResult,
  Task,
  TaskDetail,
  TaskEvent,
  TaskFacets,
  TaskFacetsQuery,
  TaskListQuery,
  TaskManifest,
  TaskRetryAccepted,
  WaitingReasonCounts,
} from './types'

/** 日志查询参数里的 attempt 透传（Task 级端点 `?attempt=` 缺省取最新 attempt）。 */
function attemptQ(attempt?: number): Record<string, string | number | undefined> {
  return { attempt }
}

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
        slot_waiting: 0,
        queue_full: 0,
        insufficient_resources: 0,
        gang_pending: 0,
      }) as WaitingReasonCounts,
      runners: Array.isArray(waiting['runners']) ? (waiting['runners'] as QueueStatus['waiting']['runners']) : [],
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

  async listTasks(query: TaskListQuery = {}): Promise<Page<Task>> {
    const data = await request<unknown>(
      `/tasks${qs({
        status: query.status,
        runner: query.runner,
        submitter: query.submitter,
        // offset 分页（UI）与游标分页（agent / CLI）互斥：给了 page 就不带 cursor
        page: query.page,
        page_size: query.page_size,
        cursor: query.page ? undefined : (query.cursor ?? undefined),
        limit: query.page ? undefined : query.limit,
      })}`,
    )
    return asPage<Task>(data)
  },

  listTaskFacets(query: TaskFacetsQuery = {}): Promise<TaskFacets> {
    return request<TaskFacets>(
      `/tasks/facets${qs({
        status: query.status,
        runner: query.runner,
        submitter: query.submitter,
      })}`,
    )
  },

  getTask(id: string): Promise<TaskDetail> {
    return request<TaskDetail>(`/tasks/${encodeURIComponent(id)}`)
  },

  cancelTask(id: string): Promise<CancelResult> {
    return request<CancelResult>(`/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
  },

  retryTask(id: string): Promise<TaskRetryAccepted> {
    return request<TaskRetryAccepted>(`/tasks/${encodeURIComponent(id)}/retry`, { method: 'POST' })
  },

  async listTaskAttempts(taskId: string): Promise<Attempt[]> {
    const data = await request<unknown>(`/tasks/${encodeURIComponent(taskId)}/attempts`)
    return asArray<Attempt>(data).sort((a, b) => a.attempt_no - b.attempt_no)
  },

  async listTaskRunners(taskId: string, query: { attempt?: number } = {}): Promise<RunnerExecution[]> {
    const data = await request<unknown>(
      `/tasks/${encodeURIComponent(taskId)}/runners${qs(attemptQ(query.attempt))}`,
    )
    return asArray<RunnerExecution>(data).sort((a, b) => a.seq - b.seq)
  },

  getTaskRunner(taskId: string, seq: number, query: { attempt?: number } = {}): Promise<RunnerExecution> {
    return request<RunnerExecution>(
      `/tasks/${encodeURIComponent(taskId)}/runners/${seq}${qs(attemptQ(query.attempt))}`,
    )
  },

  getRunnerResult(taskId: string, seq: number, query: { attempt?: number } = {}): Promise<Result> {
    return request<Result>(
      `/tasks/${encodeURIComponent(taskId)}/runners/${seq}/result${qs(attemptQ(query.attempt))}`,
    )
  },

  async getRunnerLogs(taskId: string, seq: number, opts: LogOptions = {}): Promise<LogChunk> {
    const text = await requestText(
      `/tasks/${encodeURIComponent(taskId)}/runners/${seq}/logs${qs({
        attempt: opts.attempt,
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

  async listRunnerArtifacts(
    taskId: string,
    seq: number,
    query: { attempt?: number } = {},
  ): Promise<ArtifactList> {
    const data = await request<unknown>(
      `/tasks/${encodeURIComponent(taskId)}/runners/${seq}/artifacts${qs(attemptQ(query.attempt))}`,
    )
    return { items: asArray<ArtifactList['items'][number]>(data) }
  },

  getTaskDeploy(taskId: string): Promise<DeploySnapshot> {
    return request<DeploySnapshot>(`/tasks/${encodeURIComponent(taskId)}/deploy`)
  },

  async getTaskDeployLogs(taskId: string, query: DeployLogQuery = {}): Promise<DeployLogs> {
    const useTail = Boolean(query.tail_lines && query.tail_lines > 0)
    return request<DeployLogs>(
      `/tasks/${encodeURIComponent(taskId)}/deploy/logs${qs({
        attempt: query.attempt,
        pod: query.pod,
        // `tail_lines` 与 `offset` 二选一 —— 同时传会 400，"要看最新"时干脆不带 offset
        offset: useTail ? undefined : (query.offset ?? 0),
        limit: query.limit,
        tail_lines: useTail ? query.tail_lines : undefined,
      })}`,
    )
  },

  async getEventsHistory(taskId: string, sinceSeq?: number): Promise<TaskEvent[]> {
    // `EventList`：`next_cursor` 是本页末条 seq，续取时作为 since_seq 传回（契约明确不接受 cursor）
    const data = await request<unknown>(
      `/tasks/${encodeURIComponent(taskId)}/events/history${qs({ since_seq: sinceSeq, limit: 1000 })}`,
    )
    return asArray<TaskEvent>(data).sort((a, b) => a.seq - b.seq)
  },

  async listRunners(query: RunnerListQuery = {}): Promise<Runner[]> {
    const data = await request<unknown>(
      `/runners${qs({
        role: query.role,
        orphaned: query.orphaned === undefined ? undefined : String(query.orphaned),
        // 默认只回最新；只有显式要历史时后端才会带上 retired 版本（D-60）
        include_retired: query.include_retired ? 'true' : undefined,
      })}`,
    )
    return asArray<unknown>(data).map(normalizeRunner)
  },

  async getRunner(name: string): Promise<Runner> {
    return normalizeRunner(await request<unknown>(`/runners/${encodeURIComponent(name)}`))
  },

  async listClusters(): Promise<Cluster[]> {
    const data = await request<unknown>('/clusters')
    return asArray<Cluster>(data)
  },

  async getQueueStatus(): Promise<QueueStatus> {
    return normalizeQueueStatus(await request<unknown>('/queue-status'))
  },

  submitTask(manifest: TaskManifest): Promise<SubmitResult> {
    return request<SubmitResult>('/tasks', { method: 'POST', body: JSON.stringify(manifest) })
  },

  dryRun(manifest: TaskManifest): Promise<DryRunResult> {
    return request<DryRunResult>(`/tasks${qs({ dry_run: 'true' })}`, {
      method: 'POST',
      body: JSON.stringify(manifest),
    })
  },

  async listTaskArtifacts(taskId: string): Promise<ArtifactList> {
    const data = await request<unknown>(`/tasks/${encodeURIComponent(taskId)}/artifacts`)
    return { items: asArray<ArtifactList['items'][number]>(data) }
  },

  /** 整包下载：必须走带鉴权的 fetch（`<a href>` 在 dev/OIDC 下会 401），拿到 Blob 再触发保存 */
  async downloadBundle(taskId: string): Promise<Blob> {
    const res = await fetch(`${API_BASE}/tasks/${encodeURIComponent(taskId)}/bundle`, {
      headers: authHeaders(),
      credentials: 'same-origin',
    })
    if (!res.ok) throw new ApiClientError(res.status, normalizeErrorBody(res.status, await res.json().catch(() => null)))
    return res.blob()
  },

  /** 单文件下载：同上，`path` 用 artifacts 返回的 task 目录相对路径 */
  async downloadFile(taskId: string, path: string): Promise<Blob> {
    const res = await fetch(`${API_BASE}/tasks/${encodeURIComponent(taskId)}/files?path=${encodeURIComponent(path)}`, {
      headers: authHeaders(),
      credentials: 'same-origin',
    })
    if (!res.ok) throw new ApiClientError(res.status, normalizeErrorBody(res.status, await res.json().catch(() => null)))
    return res.blob()
  },

  /* 报告（契约内） */
  getTaskReport(taskId: string): Promise<Report> {
    return request<Report>(`/tasks/${encodeURIComponent(taskId)}/report`)
  },

  async getTaskReportMd(taskId: string): Promise<string> {
    const text = await requestText(`/tasks/${encodeURIComponent(taskId)}/report.md`, {
      headers: { Accept: 'text/markdown' },
    })
    return text.text
  },

  /* Profile（契约内只读） */
  async listProfiles(): Promise<ExecutionProfile[]> {
    const data = await request<unknown>('/profiles')
    return asArray<ExecutionProfile>(data)
  },

  /* ---------------------------------------------------------------- 共享挂载（D-65） */

  /** `GET /shared-mounts`：viewer+ 只读视图，**只回启用中**的行 */
  async listSharedMounts(): Promise<SharedMount[]> {
    const data = await request<unknown>('/shared-mounts')
    return asArray<unknown>(data).map((item) => item as SharedMount)
  },

  /** `GET /admin/shared-mounts`：admin 视图，含停用行 */
  async listAdminSharedMounts(): Promise<SharedMount[]> {
    const data = await request<unknown>('/admin/shared-mounts')
    return asArray<unknown>(data).map((item) => item as SharedMount)
  },

  createAdminSharedMount(body: SharedMountWrite): Promise<SharedMount> {
    return request<SharedMount>('/admin/shared-mounts', { method: 'POST', body: JSON.stringify(body) })
  },

  /** `name` 走路径，**不可改**；body 省略的字段后端保留旧值 */
  updateAdminSharedMount(name: string, body: SharedMountPatch): Promise<SharedMount> {
    return request<SharedMount>(`/admin/shared-mounts/${encodeURIComponent(name)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    })
  },

  async deleteAdminSharedMount(name: string): Promise<void> {
    await request<void>(`/admin/shared-mounts/${encodeURIComponent(name)}`, { method: 'DELETE' })
  },

  /* 管理（仅 admin） */
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
    const [tasks, runners, queue] = await Promise.all([
      realApi.listTasks({ limit: 200 }),
      realApi.listRunners(),
      realApi.getQueueStatus().catch(() => null),
    ])
    const items = tasks.items
    const active = items.filter((t) => ['submitted', 'queued', 'running', 'cancelling'].includes(t.status))
    const terminal = items.filter((t) =>
      ['succeeded', 'failed', 'service_unavailable', 'canceled'].includes(t.status),
    )
    const succeeded = terminal.filter((t) => t.status === 'succeeded').length
    const dayAgo = Date.now() - 24 * 3600 * 1000
    const failed24h = items.filter(
      (t) =>
        ['failed', 'service_unavailable'].includes(t.status) &&
        new Date(t.submitted_at).getTime() >= dayAgo,
    ).length
    const queuedRunners =
      queue?.waiting?.total ??
      items.reduce((sum, t) => sum + (t.runner_counts?.pending ?? 0), 0)

    return {
      active_tasks: active.length,
      queued_runners: queuedRunners,
      failed_24h: failed24h,
      success_rate: terminal.length === 0 ? null : succeeded / terminal.length,
      total_tasks: items.length,
      runner_count: runners.length,
    }
  },
}
