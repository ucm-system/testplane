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
} from './types'

/** 日志读取参数（07 §5.2：`?stream=&offset=&limit=&follow=`）。 */
export interface LogOptions {
  /** attempt 序号；缺省取最新 attempt */
  attempt?: number
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
 * 端点以 `server/api/openapi.yaml`（schema 2.0）为准（MVP 子集）：
 * `/meta`、`/me`、`POST /tasks`(+`?dry_run=true`)、`GET /tasks`、`GET /tasks/{id}`、
 * `POST /tasks/{id}/cancel`、`POST /tasks/{id}/retry`、`GET /tasks/{id}/events`(SSE)、
 * `GET /tasks/{id}/events/history`、`GET /tasks/{id}/deploy`、`GET /tasks/{id}/deploy/logs`、
 * `GET /tasks/{id}/runners`、`GET /tasks/{id}/runners/{seq}`(+`/result`、`/logs`、`/artifacts`)、
 * `GET /tasks/{id}/attempts`、`GET /tasks/{id}/attempts/{n}/runners/{seq}`(+`/result`、`/logs`)、
 * `GET /tasks/{id}/report`、`GET /tasks/{id}/report.md`、`GET /tasks/{id}/artifacts`、
 * `GET /runners`、`GET /runners/{name}`、`GET /clusters`、`GET /queue-status`。
 */
export interface Api {
  /* 身份与元信息（OIDC 会话时 getIdentity 会把 csrf_token 收进内存） */
  getIdentity(): Promise<Identity>
  getMeta(): Promise<Meta>
  /** 登出：`POST /auth/logout`（带 X-CSRF-Token），随后清空内存身份 */
  logout(): Promise<void>

  /* Task 列表 / facets（UI 走 offset 分页拿「第几页 / 共几页」） */
  listTasks(query?: TaskListQuery): Promise<Page<Task>>
  /** `GET /tasks/facets`：表头列筛选的候选值与计数（过滤参数与 listTasks 同名同义） */
  listTaskFacets(query?: TaskFacetsQuery): Promise<TaskFacets>

  /* Task 详情 / 取消 / 重试 */
  /** 详情带 `attempts: Attempt[]` 与最新 attempt 的 `runners: RunnerExecution[]` */
  getTask(id: string): Promise<TaskDetail>
  /** 取消：先终止当前 runner → service-clean（若含部署）→ 落 canceled */
  cancelTask(id: string): Promise<CancelResult>
  /** 整 Task 重试 = 新 attempt 全量重跑含 fresh deploy（§7.8）；409 ATTEMPT_EXHAUSTED = 达上限 */
  retryTask(id: string): Promise<TaskRetryAccepted>

  /* attempt 与 runner 执行记录（Task 级缺省最新 attempt；attempt 级权威定位） */
  listTaskAttempts(taskId: string): Promise<Attempt[]>
  /** `GET /tasks/{id}/runners`：最新 attempt 的 runner 列表（按 seq 升序） */
  listTaskRunners(taskId: string, query?: { attempt?: number }): Promise<RunnerExecution[]>
  /** `GET /tasks/{id}/runners/{seq}`：单个 runner 执行详情（缺省最新 attempt） */
  getTaskRunner(taskId: string, seq: number, query?: { attempt?: number }): Promise<RunnerExecution>
  /**
   * `GET /tasks/{id}/runners/{seq}/result`：该 runner 的 `result.json` 原文（附录 A schema 2.0）。
   * 产物三选一（D-74）：只产出 html/csv 的 runner 没有 result.json，端点 404（`detail.produced[]`）。
   */
  getRunnerResult(taskId: string, seq: number, query?: { attempt?: number }): Promise<Result>
  /** `GET /tasks/{id}/runners/{seq}/logs`：runner 日志明文（offset 字节游标） */
  getRunnerLogs(taskId: string, seq: number, opts?: LogOptions): Promise<LogChunk>
  /** `GET /tasks/{id}/runners/{seq}/artifacts`：runner 级产物清单 */
  listRunnerArtifacts(taskId: string, seq: number, query?: { attempt?: number }): Promise<ArtifactList>

  /* Deploy：快照（deploy-state 副本 / 排队水位 / Pod 拓扑）+ 按 Pod 分页签的日志（含 gap 标记） */
  getTaskDeploy(taskId: string): Promise<DeploySnapshot>
  /** `GET /tasks/{id}/deploy/logs`：排队期返回 `pods: []` + `message`，不是 404 */
  getTaskDeployLogs(taskId: string, query?: DeployLogQuery): Promise<DeployLogs>

  /* 事件（SSE 见 api/sse.ts；这里是回放接口，也是降级轮询的来源） */
  getEventsHistory(taskId: string, sinceSeq?: number): Promise<TaskEvent[]>

  /* Runner 目录：只读（注册靠目录扫描，没有运行时管理端点） */
  listRunners(query?: RunnerListQuery): Promise<Runner[]>
  getRunner(name: string): Promise<Runner>

  /* 集群与队列（只读） */
  listClusters(): Promise<Cluster[]>
  getQueueStatus(): Promise<QueueStatus>

  /* 提交（不做幂等 D-37：每次 POST 都创建新 Task） */
  submitTask(manifest: TaskManifest): Promise<SubmitResult>
  dryRun(manifest: TaskManifest): Promise<DryRunResult>

  /* 产物 */
  /** `GET /tasks/{id}/artifacts`：Task 级产物清单（`path` 可直接拼 `/files?path=` 下载） */
  listTaskArtifacts(taskId: string): Promise<ArtifactList>
  /** `GET /tasks/{id}/bundle`：整包下载（`application/zip`），必须走带鉴权的 fetch → Blob */
  downloadBundle(taskId: string): Promise<Blob>
  /** `GET /tasks/{id}/files?path=`：单文件下载（带鉴权） */
  downloadFile(taskId: string, path: string): Promise<Blob>

  /* 报告（契约内：`/tasks/{id}/report` 与 `/report.md`） */
  getTaskReport(taskId: string): Promise<Report>
  getTaskReportMd(taskId: string): Promise<string>

  /* Execution Profile（契约内只读；写走 /admin/profiles） */
  listProfiles(): Promise<ExecutionProfile[]>

  /* 共享挂载（D-65）：listSharedMounts 是 viewer 可读的只读视图（只回启用中），
     其余四个是 admin 写接口 */
  listSharedMounts(): Promise<SharedMount[]>
  listAdminSharedMounts(): Promise<SharedMount[]>
  createAdminSharedMount(body: SharedMountWrite): Promise<SharedMount>
  updateAdminSharedMount(name: string, body: SharedMountPatch): Promise<SharedMount>
  deleteAdminSharedMount(name: string): Promise<void>

  /* 管理（P1，仅 admin） */
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

  /* 看板 KPI（P0 无 /board：由 /tasks + /runners + /queue-status 现算） */
  getDashboardStats(): Promise<DashboardStats>
}
