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
} from './types'

/** 日志读取参数（07 §5.2：`?stream=&offset=&limit=&follow=`）。 */
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
 * 端点以 `server/api/openapi.yaml` 为准（MVP 子集）：
 * `/meta`、`/me`、`POST /runs`(+`?dry_run=true`)、`GET /runs`、`GET /runs/{id}`、
 * `POST /runs/{id}/cancel`、`GET /runs/{id}/events`(SSE)、`GET /runs/{id}/events/history`、
 * `GET /runs/{id}/report.md`（前端只用新窗口链接）、`GET /runs/{id}/deploy`、
 * `GET /tasks`、`GET /tasks/{id}`、`POST /tasks/{id}/cancel`、`GET /tasks/{id}/attempts`、
 * `GET /tasks/{id}/attempts/{idx}/logs`、`GET /testers`、`GET /testers/{name}`、
 * `GET /clusters`、`GET /queue-status`。
 */
export interface Api {
  /* 身份与元信息（OIDC 会话时 getIdentity 会把 csrf_token 收进内存） */
  getIdentity(): Promise<Identity>
  getMeta(): Promise<Meta>
  /** 登出：`POST /auth/logout`（带 X-CSRF-Token），随后清空内存身份 */
  logout(): Promise<void>

  /* Run */
  listRuns(query?: RunListQuery): Promise<Page<RunSummary>>
  /** 详情带 `tasks: TaskSummary[]` */
  getRun(id: string): Promise<Run>
  /** 取消：`POST /runs/{id}/cancel`（子路径，不是 `:cancel`） */
  cancelRun(id: string): Promise<CancelResult>

  /* Task / Attempt（Attempt 用 task_id + idx 寻址，idx 从 1 开始） */
  listTasks(query?: TaskListQuery): Promise<Page<Task>>
  /** Task 详情 + Attempt 列表 */
  getTask(id: string): Promise<TaskDetail>
  cancelTask(id: string): Promise<TaskCancelResult>
  listAttempts(taskId: string): Promise<Attempt[]>
  getAttemptLogs(taskId: string, idx: number, opts?: LogOptions): Promise<LogChunk>
  /**
   * `GET /tasks/{task_id}/attempts/{idx}/result`：该 Attempt 的 `result.json` 原文。
   *
   * 报告页的指标表**只**从这里取（report.json 只做组织与索引，不做指标聚合，§8.4）；
   * 文件缺失/非法时后端按 `RESULT_MISSING` / `RESULT_INVALID` 收口，此端点 404。
   */
  getAttemptResult(taskId: string, idx: number): Promise<Result>

  /* Deploy：快照（Pod 拓扑与 manifest）+ 按 Pod 分页签的日志（含 gap 标记） */
  getDeploy(runId: string): Promise<DeploySnapshot>
  /** `GET /runs/{id}/deploy/logs`：排队期返回 `pods: []` + `message`，不是 404 */
  getDeployLogs(runId: string, query?: DeployLogQuery): Promise<DeployLogs>

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

  /** `GET /runs/{id}/artifacts`：Run 级产物清单（`path` 可直接拼 `/files?path=` 下载） */
  listRunArtifacts(runId: string): Promise<ArtifactList>
  /** `GET /runs/{id}/bundle`：整包下载（`application/zip`），必须走带鉴权的 fetch → Blob */
  downloadBundle(runId: string): Promise<Blob>
  /** `GET /runs/{id}/files?path=`：单文件下载（带鉴权） */
  downloadFile(runId: string, path: string): Promise<Blob>

  /* 报告（契约内：`/runs/{id}/report` 与 `/report.md`） */
  getReport(runId: string): Promise<Report>
  getReportMd(runId: string): Promise<string>

  /* Execution Profile（契约内只读；写走 /admin/profiles） */
  listProfiles(): Promise<ExecutionProfile[]>

  /* 重试（P1，openapi 尚未收录，按 Lead 约定路径） */
  getRetryPlan(runId: string): Promise<RetryPlan>
  retryRun(runId: string): Promise<RetryPlan>
  retryTask(taskId: string): Promise<TaskRetryAccepted>

  /* 管理（P1，仅 admin；openapi 尚未收录） */
  listAdminClusters(): Promise<AdminCluster[]>
  createAdminCluster(body: ClusterWrite): Promise<AdminCluster>
  updateAdminCluster(name: string, body: ClusterWrite): Promise<AdminCluster>
  deleteAdminCluster(name: string): Promise<void>
  createProfile(body: ProfileWrite): Promise<ExecutionProfile>
  updateProfile(name: string, body: ProfileWrite): Promise<ExecutionProfile>
  deleteProfile(name: string): Promise<void>
  listAdminSecrets(): Promise<AdminSecret[]>
  createAdminSecret(body: SecretWrite): Promise<AdminSecret>
  listAudit(query?: AuditQuery): Promise<Page<AuditEntry>>

  /* 看板 KPI（P0 无 /board：由 /runs + /testers + /queue-status 现算） */
  getDashboardStats(): Promise<DashboardStats>
}
