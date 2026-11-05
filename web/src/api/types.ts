/**
 * TestPlane Web 的领域类型 —— 与 design/views/07-api.md 的 REST 契约一一对应。
 *
 * 约定（07 §1）：
 *  - 字段命名 snake_case；时间 RFC3339（UTC，`Z`）；ID 是不透明字符串。
 *  - 状态 token 一律小写（文档与 UI 展示大写，见 lib/status.ts 的中文标签表）。
 *  - 响应允许新增字段，客户端不得因未知字段失败 —— 因此所有"可选"字段一律标 `?`。
 *
 * 这里手写而非 `openapi-typescript` 生成：`api/openapi.yaml` 尚未落地（票 #5 的前提），
 * 落定后应改成生成物；生成命令见 web/README.md。
 */

/* ---------------------------------------------------------------- 状态枚举 */

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
  | 'skipped'
  | 'canceled'

export type AttemptStatus = 'preparing' | 'running' | 'succeeded' | 'failed' | 'canceled'

/** Deploy 子状态（design/views/02 §2.5）。 */
export type DeployState =
  | 'queued'
  | 'accepted'
  | 'deploying'
  | 'ready'
  | 'blocked'
  | 'failed'
  | 'canceled'

/** 排队原因（design/impl/web-and-delivery.md §4.2）。 */
export type WaitingReason =
  | 'precheck_gate'
  | 'slot_waiting'
  | 'global_limit'
  | 'queue_full'
  | 'insufficient_resources'
  | 'gang_pending'

export type CleanupPolicy = 'always' | 'on_failure' | 'never'

export type FailurePolicy = 'continue' | 'abort_run'

/* ---------------------------------------------------------------- 错误模型 */

/**
 * `design/views/07-api.md` §3 的错误体。注意两种线上形态都要吃得下：
 *  - 扁平：`{code, message, detail, retryable, trace_id}`
 *  - 包裹：`{error: {code, message, detail, retryable}}`
 * 归一化在 api/client.ts 的 `normalizeErrorBody` 里做，页面只认这一个类型。
 */
export interface ApiErrorBody {
  code: string
  message: string
  detail?: unknown
  retryable: boolean
  trace_id?: string
}

/** 校验失败时 `detail.issues[]` 的一条（07 §3）。 */
export interface ValidationIssue {
  path: string
  code: string
  message: string
}

/* ---------------------------------------------------------------- 通用 */

/** 游标分页响应（07 §6）。 */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
  has_more: boolean
}

/**
 * 当前身份（`GET /api/v1/auth/me`）。
 * MVP 是 dev 模式：`Authorization: Bearer <subject>`，不带时服务端按 `dev` 处理。
 */
export interface Identity {
  user: string
  /** viewer / operator / admin；服务端判定，前端不推断（07 §2.2） */
  role?: string
  groups?: string[]
  csrf_token?: string
}

/** 部署得到的服务端点。 */
export interface EndpointRef {
  id?: string
  url: string
  model_name?: string | null
  model_path?: string | null
  run_id?: string | null
  status?: string | null
  created_at?: string | null
  /** 该 Endpoint 背后的 Pod 数（有 Deploy 时才有意义） */
  pod_count?: number | null
}

/* ---------------------------------------------------------------- Run */

export interface TaskCounts {
  total: number
  succeeded: number
  failed: number
  skipped?: number
  running?: number
  pending?: number
}

export interface RunSummary {
  id: string
  name: string
  kind: RunKind
  status: RunStatus
  submitted_by: string
  submitted_at: string
  finished_at?: string | null
  endpoint?: EndpointRef | null
  /** 列表页的 Task 进度（4 / 4）；后端未给时前端从 `tasks` 兜底算 */
  task_counts?: TaskCounts | null
  duration_seconds?: number | null
  error?: ApiErrorBody | null
}

export interface RunDetail extends RunSummary {
  cleanup_policy?: CleanupPolicy
  deploy?: DeploySnapshot | null
  tasks: TaskSummary[]
  env?: Record<string, string>
  secrets?: Record<string, string> | string[]
  submitter_role?: string | null
  retry_of?: string | null
}

/* ---------------------------------------------------------------- Task / Attempt */

export interface TaskSummary {
  id: string
  run_id?: string
  /** tests[] 里的顺序，Pre-Check 是 0 或 1（platform-injected） */
  seq: number
  name: string
  status: TaskStatus
  tester: string
  version?: string | null
  exclusive?: boolean
  on_failure?: FailurePolicy
  waiting_reason?: WaitingReason | null
  /** 已产生的 Attempt 数（0 表示还没开始） */
  attempts: number
  /** 当前 Attempt 序号（从 1 开始） */
  attempt?: number | null
  started_at?: string | null
  finished_at?: string | null
  duration_seconds?: number | null
  error?: ApiErrorBody | null
  /** 参数原文（只读快照，平台只做词法切分） */
  args?: string | null
  image_ref?: string | null
  /** 重试时复用的上游 Task（P1 失败续跑） */
  reused_from_task_id?: string | null
}

export interface Attempt {
  idx: number
  status: AttemptStatus
  exit_code?: number | null
  started_at?: string | null
  finished_at?: string | null
  duration_seconds?: number | null
  error?: ApiErrorBody | null
  image_digest?: string | null
  note?: string | null
}

/* ---------------------------------------------------------------- Deploy */

export interface DeployPod {
  name: string
  /** all-in-one / router / prefill / decode … 由 Genet 决定，前端不枚举 */
  role: string
  status: string
  node?: string | null
  restarts?: number | null
  log_from?: string | null
  log_to?: string | null
}

export interface DeploySnapshot {
  state: DeployState
  provider?: string | null
  queue_position?: number | null
  queued_at?: string | null
  started_at?: string | null
  ready_at?: string | null
  duration_seconds?: number | null
  endpoint?: EndpointRef | null
  pods?: DeployPod[]
  /** Manifest 里 deploy 段的原文（YAML 文本） */
  manifest_yaml?: string | null
  /** 后端给的人话原因（如「Genet 队列已满」「镜像拉取失败」） */
  reason?: string | null
  error?: ApiErrorBody | null
}

/* ---------------------------------------------------------------- Tester */

export type TesterRole = 'pre_check' | 'test'

export interface TesterParameter {
  name: string
  type: string
  required: boolean
  description: string
  default?: string | null
  /** 公共参数由平台注入，args 里不得出现（07 §4.1） */
  public?: boolean
}

export interface TesterEnvDecl {
  name: string
  required: boolean
  secret: boolean
  description: string
}

export interface Tester {
  name: string
  version: string
  display_name?: string
  description?: string
  role: TesterRole
  on_failure: FailurePolicy
  /** 独占 Endpoint（同一 Endpoint 上不与其它 Task 并行） */
  exclusive: boolean
  image: string
  entry?: string
  /** args 输入框的占位示例 */
  placeholder?: string
  parameters: TesterParameter[]
  env: TesterEnvDecl[]
  /** 目录里文件已消失（孤儿条目） */
  orphaned?: boolean
}

/* ---------------------------------------------------------------- 事件（SSE） */

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

/** SSE 信封（07 §7；design/impl/web-and-delivery.md §3） */
export interface RunEvent {
  seq: number
  type: RunEventType
  run_id: string
  task_id?: string | null
  attempt?: number | null
  at: string
  payload?: Record<string, unknown> | null
}

/* ---------------------------------------------------------------- 日志 */

export interface LogChunk {
  text: string
  /** 服务端已返回到的字节偏移（X-Log-Offset） */
  offset?: number
  /** 覆盖时间区间（X-Log-Coverage） */
  coverage?: string | null
}

/* ---------------------------------------------------------------- 提交 */

export interface DeploySpec {
  provider: string
  parameters: Record<string, unknown>
}

export interface ServiceSpec {
  url: string
  model_name?: string
  model_path?: string
}

export interface ManifestTest {
  name: string
  tester: string
  version?: string
  execution?: { profile?: string }
  args?: string
  on_failure?: FailurePolicy
  exclusive?: boolean
}

/** Manifest（07 §4.1；也接受 JSON 对象） */
export interface RunManifest {
  run: {
    name: string
    deploy?: DeploySpec | null
    service?: ServiceSpec | null
    precheck?: 'auto' | 'off' | { tester?: string }
    env?: Record<string, string>
    secrets?: Record<string, string>
    cleanup?: { policy: CleanupPolicy }
    tests: ManifestTest[]
  }
}

export interface SubmitResult {
  run: RunSummary
  tasks: Array<{ id: string; name: string; status: TaskStatus }>
}

export interface DryRunResult {
  valid: boolean
  issues: ValidationIssue[]
  resolved_preview?: unknown
}

/* ---------------------------------------------------------------- 集群 / 队列 / 元信息 */

/** 集群概要（`GET /clusters`，不返回凭据）。 */
export interface Cluster {
  name: string
  namespace?: string | null
  is_default?: boolean
  healthy?: boolean | null
  last_checked_at?: string | null
  node_count?: number | null
  /** 凭据在 Secret 里的 key 名，不是凭据本身 */
  kubeconfig_key?: string | null
  message?: string | null
}

/** 平台槽位（`GET /queue-status`）。 */
export interface PlatformSlots {
  used: number
  total: number
  queue_depth?: number | null
}

/** 一条队列的分配状态（Volcano queue / deserved vs allocated）。 */
export interface QueueEntry {
  name: string
  /** open / closed … */
  state?: string | null
  deserved?: number | null
  allocated?: number | null
  pending?: number | null
  longest_wait_seconds?: number | null
  reason?: string | null
}

export interface PendingTaskRef {
  task_id: string
  name?: string | null
  run_id?: string | null
  waiting_reason?: WaitingReason | null
  waited_seconds?: number | null
}

/** `GET /queue-status`：平台槽位 + Volcano 等待原因（07 §5.3）。 */
export interface QueueStatus {
  slots?: PlatformSlots | null
  queues: QueueEntry[]
  pending_tasks?: PendingTaskRef[]
  updated_at?: string | null
}

/** `GET /meta`：版本协商（07 §5.3、§1 版本）。 */
export interface Meta {
  version: string
  api_version: string
  features?: Record<string, boolean>
}

/* ---------------------------------------------------------------- 查询参数 */

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

/* ---------------------------------------------------------------- 统计 */

/** 看板 KPI：由 `GET /runs` + `GET /testers` 现算，不依赖 P1 的 /board */
export interface DashboardStats {
  active_runs: number
  queued_tasks: number
  failed_24h: number
  success_rate: number | null
  total_runs: number
  tester_count: number
}
