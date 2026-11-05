/**
 * TestPlane Web 的领域类型 —— **以 `server/api/openapi.yaml` 为准**（契约唯一来源）。
 * 每个类型都标注了对应的 schema 名；与 `design/views/07-api.md` 冲突时以 openapi 形状为准。
 *
 * 约定：
 *  - 字段命名 snake_case；时间 RFC3339 UTC；响应允许新增字段（`additionalProperties: true`），
 *    页面只读自己认识的字段，缺字段一律当 `undefined` 处理。
 *  - 状态 token 全部小写（中文标签见 lib/status.ts）。
 */

/* ================================================================ 基础 */

/** RFC3339 时间串（UTC）。 */
export type Timestamp = string

/** `Error` schema：错误信封（07 §3）。`trace_id` 与响应头 `X-Request-Id` 同源。 */
export interface ErrorBody {
  code: string
  message: string
  detail?: Record<string, unknown>
  retryable: boolean
  trace_id?: string
}

/** 历史别名：页面与 hook 里统一用它表示"错误信封"。 */
export type ApiErrorBody = ErrorBody

/** `Failure` schema：Run / Task / Attempt 同构的失败结构（没有 trace_id，retryable 是实例级的）。 */
export interface Failure {
  code: string
  message: string
  detail?: Record<string, unknown>
  retryable: boolean
}

/** `ValidationIssue` schema：`detail.issues[]` 的一项。 */
export interface ValidationIssue {
  path: string
  code: string
  message: string
}

/** 统一分页信封（`RunList` / `TaskList` / `AttemptList` / `TesterList` / `EventList`）。 */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
  has_more: boolean
}

/* ================================================================ 枚举 */

export type RunKind = 'run' | 'task'

export type RunStatus =
  | 'submitted'
  | 'queued'
  | 'deploying'
  | 'testing'
  | 'cleanup'
  | 'cancelling'
  | 'succeeded'
  | 'completed_with_errors'
  | 'service_unavailable'
  | 'failed'
  | 'canceled'

export type TaskStatus =
  | 'pending'
  | 'queued'
  | 'preparing'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled'
  | 'skipped'

export type AttemptStatus = 'pending' | 'preparing' | 'running' | 'succeeded' | 'failed' | 'canceled'

export type DeployState = 'queued' | 'accepted' | 'deploying' | 'ready' | 'blocked' | 'failed' | 'canceled'

export type WaitingReason =
  | 'precheck_gate'
  | 'slot_waiting'
  | 'global_limit'
  | 'queue_full'
  | 'insufficient_resources'
  | 'gang_pending'

export type SkippedReason = 'upstream_abort' | 'deploy_failed' | 'canceled' | string

export type CleanupPolicy = 'always' | 'on_failure' | 'never'

export type TesterRole = 'pre_check' | 'test'

export type OnFailure = 'continue' | 'abort_run'

/** `Endpoint.source`：端点从哪来。 */
export type EndpointSource = 'genet' | 'service' | string

/* ================================================================ 端点 / 部署 */

/** `Endpoint` schema：`runs.endpoint`，也是三个公共参数（url / model_name / model_path）的来源。 */
export interface Endpoint {
  url: string
  /** 互斥键 `scheme://host:port`（不含路径） */
  key: string
  source: EndpointSource
  model_name?: string | null
  model_path?: string | null
  protocol?: string | null
  deploy_id?: string | null
}

/** `Deploy` schema：`runs.deploy`，Deploy 阶段快照（不含 Pod 拓扑）。 */
export interface Deploy {
  provider: string
  state: DeployState
  deployment_id?: string | null
  /**
   * **恒为 null（D-46）**：部署方只给 pending/inqueue/running 水位计数，排队发生在 Volcano 层，
   * 没有位次。字段保留只为与 `openapi.yaml` 兼容 —— **不要读取它做展示**。
   */
  queue_position?: number | null
  /** PodGroup phase / conditions 原文（后端不翻译，前端原样显示） */
  queue_reason?: string | null
  queued_at?: Timestamp | null
  parameters_sha?: string | null
}

/** `DeployPod` schema：`deploy/pods.json` 的一项。 */
export interface DeployPod {
  name: string
  role?: string | null
  node?: string | null
  namespace?: string | null
  /** K8s Pod phase 原值 */
  phase?: string | null
  ready?: boolean
  restarts?: number
  logs?: LogSegment[]
}

/** `LogSegment` schema：一份 Deploy 日志文件的覆盖区间（前端目前只展示元信息，不下载）。 */
export interface LogSegment {
  file: string
  start?: Timestamp | null
  end?: Timestamp | null
  restarts?: number
  truncated?: boolean
}

/** `DeploySnapshot` schema：`GET /runs/{run_id}/deploy` 的应答。 */
export interface DeploySnapshot {
  run_id: string
  /** 没有 Deploy（service 模式 / 独立 Task）时为 null */
  deploy: Deploy | null
  /** 实际提交给 Genet 的参数快照（原文，平台不解析拓扑） */
  manifest?: Record<string, unknown> | null
  pods: DeployPod[]
  deploy_timeout_seconds?: number
}

/* ================================================================ Run */

/** `Run.task_counts`：`failed` 含 canceled、`pending` 含 queued（契约只必填 total/succeeded/failed）。 */
export interface TaskCounts {
  total: number
  succeeded: number
  failed: number
  pending?: number
  running?: number
  skipped?: number
}

/** `Run` schema：列表项不带 `tasks`，详情（`GET /runs/{run_id}`）带 Task 摘要。 */
export interface Run {
  id: string
  name: string
  kind: RunKind
  status: RunStatus
  cleanup_policy: CleanupPolicy
  submitted_by: string
  submitted_at: Timestamp
  started_at?: Timestamp | null
  finished_at?: Timestamp | null
  endpoint?: Endpoint | null
  deploy?: Deploy | null
  error?: Failure | null
  task_counts?: TaskCounts
  revision?: number
  /** 仅详情返回（TaskSummary） */
  tasks?: TaskSummary[]
  /** P1：本 Run 由哪个 Run 重试而来（`POST /runs/{id}/retry` 创建的 Run 才有） */
  retry_of?: string | null
}

/** 列表项的别名（同一个 `Run`，只是不带 tasks）。 */
export type RunSummary = Run

/* ================================================================ Task / Attempt */

/** `TaskSummary` schema：Run 详情里的 Task 摘要 —— 只有这几个字段（没有 tester/seq/耗时）。 */
export interface TaskSummary {
  id: string
  name: string
  status: TaskStatus
  waiting_reason?: WaitingReason | null
  /** 最新 Attempt 序号（`current_attempt`）；0 = 还没跑过 */
  attempts: number
  error?: Failure | null
}

/** `SecretEnv` schema：只存引用名，永不返回值。 */
export interface SecretEnv {
  name: string
  source?: string | null
}

/** `RetryPolicy` schema。 */
export interface RetryPolicy {
  retry_on?: string[]
  max_attempts?: number
  backoff_seconds?: number
}

/** `Task` schema：Task 详情（`GET /tasks/{id}` / `GET /tasks`）。 */
export interface Task {
  id: string
  run_id: string
  /** `tests[]` 里的顺序；Pre-Check 为 0 */
  seq: number
  name: string
  tester_name: string
  tester_version: string
  image_ref: string
  profile?: string
  /** 提交时的参数原文 */
  args_raw: string
  args_argv?: string[]
  /** 平台注入的公共参数 argv */
  injected_argv?: string[]
  env?: Record<string, string>
  secret_env?: SecretEnv[]
  on_failure: OnFailure
  timeout_seconds?: number
  retry_policy?: RetryPolicy
  status: TaskStatus
  /** 仅 skipped 时有值 */
  status_reason?: SkippedReason | null
  waiting_reason?: WaitingReason | null
  /** 端点互斥键；queued 前必须非空 */
  endpoint_key?: string | null
  endpoint_exclusive?: boolean
  current_attempt: number
  available_at?: Timestamp | null
  cancel_requested?: boolean
  result_ref?: string | null
  error?: Failure | null
  created_at: Timestamp
  updated_at: Timestamp
}

/** `TaskDetail` schema：Task + Attempt 列表（`GET /tasks/{id}`）。 */
export interface TaskDetail extends Task {
  attempts: Attempt[]
}

/** `ExecutorRef` schema。 */
export interface ExecutorRef {
  cluster: string
  namespace: string
  job: string
}

/** `Attempt` schema：Attempt 投影。 */
export interface Attempt {
  id: string
  task_id: string
  /** 从 1 连续；(task_id, idx) 唯一 */
  idx: number
  status: AttemptStatus
  phase?: string | null
  exit_code?: number | null
  image_digest?: string | null
  started_at?: Timestamp | null
  finished_at?: Timestamp | null
  log_ref?: string | null
  artifact_ref?: string | null
  result_ref?: string | null
  error?: Failure | null
  executor_ref?: ExecutorRef | null
}

/* ================================================================ Tester */

/** `TesterParameter` schema：`tester.yaml.parameters.<name>`（文档语义，平台不渲染不校验）。 */
export interface TesterParameterDecl {
  type: string
  required?: boolean
  default?: string
  description?: string
}

/** `EnvVarDecl` schema：`tester.yaml.env.<name>`。 */
export interface EnvVarDecl {
  required?: boolean
  secret?: boolean
  description?: string
}

/** `ConcurrencyPolicy` schema。 */
export interface ConcurrencyPolicy {
  endpoint_exclusive: boolean
}

/** `Tester` schema（wire）：`tester.yaml` 结构 + 注册表元数据。`parameters` / `env` 都是 **map**。 */
export interface TesterWire {
  name: string
  version: string
  display_name: string
  description: string
  role: TesterRole
  on_failure: OnFailure
  entry: string
  image: string
  resources?: Record<string, unknown>
  concurrency?: ConcurrencyPolicy
  mounts?: Array<{ name: string; path: string; required?: boolean }>
  parameters: Record<string, TesterParameterDecl>
  env?: Record<string, EnvVarDecl>
  placeholder?: string
  content_sha?: string
  source_file?: string
  orphaned?: boolean
  removed_at?: Timestamp | null
  registered_at?: Timestamp
  registered_by?: string
}

/**
 * UI 侧归一后的 Tester：`parameters` / `env` 变成数组、`exclusive` 提平。
 * 归一发生在 `api/normalize.ts`（wire 形状始终是上面的 `TesterWire`）。
 */
export interface Tester {
  name: string
  version: string
  display_name?: string
  description?: string
  role: TesterRole
  on_failure: OnFailure
  entry?: string
  image: string
  /** 来自 `concurrency.endpoint_exclusive` */
  exclusive: boolean
  placeholder?: string
  parameters: Array<TesterParameterDecl & { name: string }>
  env: Array<EnvVarDecl & { name: string }>
  orphaned?: boolean
  source_file?: string
  registered_at?: Timestamp
}

/* ================================================================ 集群 / 队列 */

/** `Cluster` schema：集群概要（不返回凭据）。 */
export interface Cluster {
  name: string
  api_server: string
  description?: string | null
  last_check_at?: Timestamp | null
  /** 最近一次连通性检查的结论（原文） */
  last_check_result?: string | null
}

/** `EndpointSlot` schema：端点互斥租约。 */
export interface EndpointSlot {
  endpoint_key: string
  exclusive?: boolean
  holders: string[]
}

/** `PlatformSlots` schema：平台自身槽位。 */
export interface PlatformSlots {
  concurrency_max: number
  active: number
  available: number
  endpoints?: EndpointSlot[]
}

/** `WaitingReasonCounts` schema：六个取值都必填，没有排队记 0（不省略键）。 */
export interface WaitingReasonCounts {
  precheck_gate: number
  slot_waiting: number
  global_limit: number
  queue_full: number
  insufficient_resources: number
  gang_pending: number
}

/** `WaitingTask` schema：排队中的一个 Task。 */
export interface WaitingTask {
  task_id: string
  run_id: string
  name: string
  status: TaskStatus
  waiting_reason: WaitingReason
  seq?: number
  endpoint_key?: string | null
  since?: Timestamp | null
  detail?: Record<string, unknown>
}

/** `WaitingSummary` schema。 */
export interface WaitingSummary {
  total: number
  by_reason: WaitingReasonCounts
  tasks?: WaitingTask[]
}

/** `VolcanoQueue` schema：一个 Volcano Queue 的概要。 */
export interface VolcanoQueue {
  name: string
  state?: string | null
  capability?: Record<string, unknown>
  allocated?: Record<string, unknown>
  pending_jobs?: number
  longest_wait_seconds?: number
  /**
   * PodGroup `conditions[-1].message` 的**原文**（例如 `0/1 nodes are available: 1 insufficient nvidia.com/gpu`）。
   * 平台不翻译，所以是自由文本而不是枚举 —— 结构化分类只在 `waiting.by_reason` 的六个键上。
   */
  reason?: string | null
}

/** `VolcanoSnapshot` schema：不可达时只降级本块（不影响 slots / waiting）。 */
export interface VolcanoSnapshot {
  available: boolean
  /**
   * 「上一次刷新成功、本次刷新失败」——语义在**快照层**，没有时间阈值。
   * 置位时 `available` 仍是 `false`、`queues` 仍是 `[]`：平台**绝不回放历史数字**，
   * 所以前端不能把 `available=false` 渲染成「0 排队 / 无排队」。
   */
  stale?: boolean
  /** 不可达或无法判定时的说明（例如 `scheduler_unavailable`） */
  detail?: string | null
  /** 已登记的 Volcano Queue 概要；不可达时为 `[]` */
  queues?: VolcanoQueue[]
}

/** `QueueStatus` schema：`GET /queue-status`。 */
export interface QueueStatus {
  generated_at?: Timestamp
  slots: PlatformSlots
  waiting: WaitingSummary
  volcano?: VolcanoSnapshot
}

/* ================================================================ 事件 */

export type RunEventType =
  | 'run.submitted'
  | 'run.state_changed'
  | 'task.state_changed'
  | 'attempt.started'
  | 'attempt.finished'
  | 'deploy.state_changed'
  | 'cleanup.started'
  | 'run.finished'
  | 'report.written'
  | 'resync_required'

/** `Event` schema：SSE 帧的 data 与 `events/history` 的 items 同构。 */
export interface RunEvent {
  seq: number
  type: RunEventType
  run_id: string
  task_id?: string | null
  attempt_id?: string | null
  at: Timestamp
  from?: string | null
  to?: string | null
  reason?: string | null
  payload?: Record<string, unknown> | null
}

/* ================================================================ 日志 / 身份 / 元信息 */

/** 日志读取结果（`text/plain` + `X-Log-Offset` / `X-Log-Coverage` 响应头）。 */
export interface LogChunk {
  text: string
  offset?: number
  coverage?: string | null
}

/** `GET /api/v1/me`（dev 模式：`Authorization: Bearer <subject>`）。 */
export interface Identity {
  user: string
  role?: string
  csrf_token?: string
  groups?: string[]
}

/** `GET /api/v1/meta`。 */
export interface Meta {
  version: string
  api_version: string
  features?: Record<string, boolean>
}

/* ================================================================ Manifest（提交） */

/** `DeployManifest` schema。 */
export interface DeployManifest {
  provider: string
  parameters?: Record<string, unknown>
}

/** `ServiceManifest` schema：不部署时直接给已有服务。 */
export interface ServiceManifest {
  url: string
  model_name?: string
  model_path?: string
}

/** `ExecutionSpec` schema：只能引用已登记的 Profile。 */
export interface ExecutionSpec {
  profile: string
}

/** `TestSpec` schema：`tests[]` 的一个 Task 提交意图（name/tester/version/execution 必填）。 */
export interface TestSpec {
  name: string
  tester: string
  version: string
  execution: ExecutionSpec
  args?: string
  on_failure?: OnFailure
  timeout_seconds?: number
  retry?: RetryPolicy
}

/** `PrecheckSpec` schema：显式指定 Pre-Check 的形态。 */
export interface PrecheckSpec {
  tester: string
  version: string
  execution: ExecutionSpec
  args?: string
  on_failure?: OnFailure
  timeout_seconds?: number
  retry?: RetryPolicy
}

/** `CleanupSpec` schema。 */
export interface CleanupSpec {
  policy: CleanupPolicy
}

/** `RunBody` schema：Manifest 的 `run:` 内容（deploy / service 二选一）。 */
export interface RunBody {
  name: string
  deploy?: DeployManifest
  service?: ServiceManifest
  precheck?: 'auto' | 'off' | PrecheckSpec
  env?: Record<string, string>
  secrets?: Record<string, string>
  tests?: TestSpec[]
  cleanup?: CleanupSpec
}

/** `RunManifest` schema：提交体，顶层是 `run:`。 */
export interface RunManifest {
  run: RunBody
}

/** `DryRunResult` schema（`?dry_run=true`，200）。 */
export interface DryRunResult {
  valid: boolean
  issues?: ValidationIssue[]
  resolved_preview?: Record<string, unknown>
}

/** `AcceptedRun` schema（202 里只带这几个字段）。 */
export interface AcceptedRun {
  id: string
  status: RunStatus
  kind: RunKind
  submitted_at: Timestamp
}

/** `AcceptedTask` schema。 */
export interface AcceptedTask {
  id: string
  name: string
  status: TaskStatus
}

/** `RunAccepted` schema：`POST /runs` 的 202 应答。 */
export interface SubmitResult {
  run: AcceptedRun
  tasks: AcceptedTask[]
}

/** `CancelAccepted` schema：202 与「已是 cancelling/canceled」的 200 同构。 */
export interface CancelResult {
  run_id: string
  status: RunStatus
}

/** `TaskCancelAccepted` schema。 */
export interface TaskCancelResult {
  task_id: string
  run_id?: string
  status: TaskStatus
}

/* ================================================================ 查询参数 */

export interface RunListQuery {
  status?: string
  kind?: string
  tester?: string
  submitter?: string
  cursor?: string | null
  limit?: number
}

export interface TaskListQuery {
  status?: string
  tester?: string
  cursor?: string | null
  limit?: number
}

export interface TesterListQuery {
  role?: string
  orphaned?: boolean
}

/* ================================================================ 看板统计（前端现算） */

/** P0 没有 `/board`：由 `/runs` + `/testers` + `/queue-status` 现算。 */
export interface DashboardStats {
  active_runs: number
  queued_tasks: number
  failed_24h: number
  success_rate: number | null
  total_runs: number
  tester_count: number
}

/* ================================================================ 报告（契约内） */

/** `ReportCounts` schema。 */
export interface ReportCounts {
  total: number
  succeeded: number
  failed: number
  skipped: number
  canceled: number
}

/** `ReportTask` schema：报告里的单 Task 条目。 */
export interface ReportTask {
  task_id: string
  name: string
  status: TaskStatus
  attempts: number
  result_ref?: string | null
  duration_ms?: number | null
}

/** `ReportFailure` schema。 */
export interface ReportFailure {
  task_id: string
  reason: string
  error_code?: string | null
}

/** `Report` schema：`GET /runs/{id}/report`（`report.json`，只做组织不做聚合）。 */
export interface Report {
  run_id: string
  status: RunStatus
  counts: ReportCounts
  tasks: ReportTask[]
  failures: ReportFailure[]
  truncated?: boolean
  report_ref?: string
}

/* ================================================================ Execution Profile（契约内，只读） */

export interface ProfileMount {
  name: string
  pvc: string
  sub_path?: string | null
  kind?: string | null
}

/** `ExecutionProfile` schema：执行环境（提交时 `execution.profile` 必须命中）。 */
export interface ExecutionProfile {
  name: string
  cluster: string
  namespace: string
  mounts?: ProfileMount[]
  scheduler: string
  queue?: string | null
  image_pull_secrets?: string[]
  resource_defaults?: Record<string, unknown>
  deploy_timeout_seconds: number
}

/* ================================================================ P1：重试 / 管理（openapi 未收录） */

/*
 * 下面这几组端点**不在 `server/api/openapi.yaml` 的 MVP 契约内**（契约开头明确把
 * `POST /runs/{id}/retry`、`GET /runs/{id}/retry-plan`、`POST /tasks/{id}/retry`、`/admin/*`
 * 列为 P1）。这里按 Lead 指定的路径与字段先落地，后端实现后以 openapi 为准对齐。
 */

/** `GET /runs/{id}/retry-plan`：重试预览。 */
export interface RetryPlan {
  run_id: string
  /** 整个 Run 是否可重试（false 时 UI 禁用按钮并显示 `reason`） */
  retryable: boolean
  reason?: string | null
  /** 将复用（已成功）的 Task */
  reuse: RetryTaskRef[]
  /** 将重跑（失败 / 跳过）的 Task */
  rerun: RetryTaskRef[]
}

export interface RetryTaskRef {
  task_id: string
  name: string
  status: TaskStatus
  error_code?: string | null
}

/** `POST /runs/{id}/retry` 的应答。 */
export interface RetryAccepted {
  new_run_id: string
  retry_of: string
  /** 复用的 Task 在新 Run 里的 id（可选） */
  reused_task_ids?: string[]
}

/** `POST /tasks/{id}/retry` 的应答：只为刷新 Attempt 列表。 */
export interface TaskRetryAccepted {
  task_id: string
  run_id?: string
  new_attempt_idx?: number
}

/* ---------------------------------------------------------------- /admin/* */

/** `/admin/clusters` 的一行（含 kubeconfig_key：admin 可见凭据定位名）。 */
export interface AdminCluster {
  name: string
  api_server: string
  kubeconfig_key?: string | null
  namespace?: string | null
  description?: string | null
  last_check_at?: Timestamp | null
  last_check_result?: string | null
}

/** `/admin/secrets` 的一行：**只存引用，不返回值**。 */
export interface AdminSecret {
  name: string
  namespace: string
  secret_name: string
  key: string
  description?: string | null
}

/** `/admin/audit` 的一行。 */
export interface AuditEntry {
  id: string
  at: Timestamp
  actor: string
  role: string
  action: string
  object_type: string
  object_id: string
  detail?: Record<string, unknown> | null
}

export interface AuditQuery {
  action?: string
  cursor?: string | null
  limit?: number
}

/** 新建/编辑 Profile 的写体（`POST/PUT /admin/profiles`）。 */
export interface ProfileWrite {
  name: string
  cluster: string
  namespace: string
  scheduler: string
  queue?: string | null
  deploy_timeout_seconds: number
}

/** 新建/编辑 Cluster 的写体（`POST/PUT /admin/clusters`）。 */
export interface ClusterWrite {
  name: string
  api_server: string
  kubeconfig_key?: string | null
  description?: string | null
}

/** 登记 Secret 引用的写体（`POST /admin/secrets`）。 */
export interface SecretWrite {
  name: string
  namespace: string
  secret_name: string
  key: string
  description?: string | null
}
