/**
 * 真实接口实现 —— 端点全部来自 design/views/07-api.md §5（MVP 子集）。
 * 基址由 api/client.ts 的 API_BASE 拼接（默认 `/api/v1`），这里只写资源路径。
 */
import type { Api, LogOptions } from './contract'
import { qs, request, requestText } from './client'
import { asArray, asPage, normalizeTester } from './normalize'
import type {
  Attempt,
  Cluster,
  DashboardStats,
  DeploySnapshot,
  DryRunResult,
  Identity,
  LogChunk,
  Meta,
  Page,
  QueueEntry,
  QueueStatus,
  RunDetail,
  RunEvent,
  RunListQuery,
  RunManifest,
  RunSummary,
  SubmitResult,
  TaskListQuery,
  TaskSummary,
  Tester,
  TesterListQuery,
} from './types'

/** 队列状态：期望 `{slots, queues, pending_tasks}`，也容忍裸数组。 */
function normalizeQueueStatus(raw: unknown): QueueStatus {
  if (Array.isArray(raw)) {
    return { queues: raw as QueueEntry[], slots: null, pending_tasks: [] }
  }
  const rec = (raw ?? {}) as Record<string, unknown>
  return {
    slots: (rec['slots'] ?? null) as QueueStatus['slots'],
    queues: Array.isArray(rec['queues']) ? (rec['queues'] as QueueEntry[]) : [],
    pending_tasks: Array.isArray(rec['pending_tasks']) ? (rec['pending_tasks'] as QueueStatus['pending_tasks']) : [],
    updated_at: typeof rec['updated_at'] === 'string' ? rec['updated_at'] : null,
  }
}

export const realApi: Api = {
  getIdentity(): Promise<Identity> {
    return request<Identity>('/me')
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

  getRun(id: string): Promise<RunDetail> {
    return request<RunDetail>(`/runs/${encodeURIComponent(id)}`)
  },

  async cancelRun(id: string): Promise<void> {
    await request<unknown>(`/runs/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
  },

  async listTasks(query: TaskListQuery = {}): Promise<Page<TaskSummary>> {
    const data = await request<unknown>(
      `/tasks${qs({
        status: query.status,
        tester: query.tester,
        cursor: query.cursor ?? undefined,
        limit: query.limit,
      })}`,
    )
    return asPage<TaskSummary>(data)
  },

  async cancelTask(id: string): Promise<void> {
    await request<unknown>(`/tasks/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
  },

  async listAttempts(taskId: string): Promise<Attempt[]> {
    const data = await request<unknown>(`/tasks/${encodeURIComponent(taskId)}/attempts`)
    return asArray<Attempt>(data, 'items')
  },

  async getAttemptLogs(taskId: string, idx: number, opts: LogOptions = {}): Promise<LogChunk> {
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

  async getDeployLogs(runId: string, pod: string, opts: LogOptions = {}): Promise<LogChunk> {
    const text = await requestText(
      `/runs/${encodeURIComponent(runId)}/deploy/logs${qs({
        pod,
        offset: opts.offset,
        limit: opts.limit,
        tail: opts.tail,
      })}`,
      { headers: { Accept: 'text/plain' } },
    )
    return { text: text.text, offset: text.offset, coverage: text.coverage }
  },

  async getEventsHistory(runId: string, sinceSeq?: number): Promise<RunEvent[]> {
    const data = await request<unknown>(
      `/runs/${encodeURIComponent(runId)}/events/history${qs({ since_seq: sinceSeq, limit: 1000 })}`,
    )
    return asArray<RunEvent>(data, 'items').sort((a, b) => a.seq - b.seq)
  },

  async listTesters(query: TesterListQuery = {}): Promise<Tester[]> {
    const data = await request<unknown>(
      `/testers${qs({
        role: query.role,
        orphaned: query.orphaned === undefined ? undefined : String(query.orphaned),
      })}`,
    )
    return asArray<unknown>(data, 'items').map(normalizeTester)
  },

  async getTester(name: string): Promise<Tester> {
    return normalizeTester(await request<unknown>(`/testers/${encodeURIComponent(name)}`))
  },

  async listClusters(): Promise<Cluster[]> {
    const data = await request<unknown>('/clusters')
    return asArray<Cluster>(data, 'items')
  },

  async getQueueStatus(): Promise<QueueStatus> {
    return normalizeQueueStatus(await request<unknown>('/queue-status'))
  },

  async submitRun(manifest: RunManifest): Promise<SubmitResult> {
    return request<SubmitResult>('/runs', { method: 'POST', body: JSON.stringify(manifest) })
  },

  async dryRun(manifest: RunManifest): Promise<DryRunResult> {
    return request<DryRunResult>(`/runs${qs({ dry_run: 'true' })}`, {
      method: 'POST',
      body: JSON.stringify(manifest),
    })
  },

  async getDashboardStats(): Promise<DashboardStats> {
    // P0 没有 /board：用列表页的同一份数据 + 队列状态现算（web-and-delivery §15）
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
      queue?.pending_tasks?.length ??
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
