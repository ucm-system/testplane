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

/** 日志读取参数（07 §5.2：`?stream=&offset=&limit=&follow=`） */
export interface LogOptions {
  stream?: 'stdout' | 'stderr'
  /** 字节偏移，向上翻页用 */
  offset?: number
  /** 行数上限（服务端单次 ≤ 64 KiB） */
  limit?: number
  /** 首屏尾部行数 */
  tail?: number
  follow?: boolean
}

/**
 * 前端唯一的服务端入口。页面与 hook 只依赖这个接口，
 * 真实实现（api/real.ts）与 mock 实现（api/mock.ts）在 api/index.ts 里二选一。
 *
 * 端点全集以 `design/views/07-api.md §5` 为准；MVP 能用的是：
 * `GET /meta`、`GET /auth/me`、`POST /runs`、`GET /runs`、`GET /runs/{id}`、
 * `GET /runs/{id}/tasks`、`POST /runs/{id}/cancel`、`GET /runs/{id}/events`、
 * `GET /runs/{id}/events/history`、`GET /runs/{id}/report`、`GET /runs/{id}/report.md`、
 * `GET /runs/{id}/manifest`、`GET /runs/{id}/deploy`、`GET /tasks`、`GET /tasks/{id}`、
 * `POST /tasks/{id}/cancel`、`GET /tasks/{id}/attempts`、
 * `GET /tasks/{id}/attempts/{idx}/result`、`GET /tasks/{id}/attempts/{idx}/logs`、
 * `GET /testers`、`GET /testers/{name}`、`GET /clusters`、`GET /queue-status`。
 */
export interface Api {
  /* 身份与元信息 */
  getIdentity(): Promise<Identity>
  getMeta(): Promise<Meta>

  /* Run */
  listRuns(query?: RunListQuery): Promise<Page<RunSummary>>
  getRun(id: string): Promise<RunDetail>
  /** 取消：`POST /runs/{id}/cancel`（子路径，不是 `:cancel`） */
  cancelRun(id: string): Promise<void>

  /* Task / Attempt（Attempt 用 task_id + idx 寻址，idx 从 1 开始） */
  listTasks(query?: TaskListQuery): Promise<Page<TaskSummary>>
  cancelTask(id: string): Promise<void>
  listAttempts(taskId: string): Promise<Attempt[]>
  getAttemptLogs(taskId: string, idx: number, opts?: LogOptions): Promise<LogChunk>

  /* Deploy */
  getDeploy(runId: string): Promise<DeploySnapshot>
  getDeployLogs(runId: string, pod: string, opts?: LogOptions): Promise<LogChunk>

  /* 事件（SSE 见 api/sse.ts；这里是回放接口，也是降级轮询的来源） */
  getEventsHistory(runId: string, sinceSeq?: number): Promise<RunEvent[]>

  /* Tester：只读目录（随镜像内置，启动时自动发现，没有注册接口） */
  listTesters(query?: TesterListQuery): Promise<Tester[]>
  getTester(name: string): Promise<Tester>

  /* 集群与队列（只读） */
  listClusters(): Promise<Cluster[]>
  getQueueStatus(): Promise<QueueStatus>

  /* 提交 */
  submitRun(manifest: RunManifest): Promise<SubmitResult>
  dryRun(manifest: RunManifest): Promise<DryRunResult>

  /* 看板 KPI（P0 无 /board：由 /runs + /testers + /queue-status 现算） */
  getDashboardStats(): Promise<DashboardStats>
}
