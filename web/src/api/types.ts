/**
 * Argus Web 的领域类型 —— **以 `server/api/openapi.yaml`（schema 2.0）为准**（契约唯一来源）。
 * 每个类型都标注了对应的 schema 名；与 `design/views/07-api.md` 冲突时以 openapi 形状为准。
 *
 * v2 术语（ARCHITECTURE §0.2）：Run→Task、Tester→Runner、Attempt 是 **Task 级**计数；
 * `completed_with_errors` 已并入 `failed`（D-73）；`args` / `retry` / `cleanup.policy` 不存在（D-66/69/70）。
 *
 * 约定：
 *  - 字段命名 snake_case；时间 RFC3339 UTC；响应允许新增字段（`additionalProperties: true`），
 *    页面只读自己认识的字段，缺字段一律当 `undefined` 处理。
 *  - 状态 token 全部小写（中文标签见 lib/status.ts）。
 */

/* ================================================================ 基础 */

/** RFC3339 时间串（UTC）。 */
export type Timestamp = string

/** `ErrorCode` schema：附录 D v2 全集（D-73 更名 TESTER_*→RUNNER_*、TASK_TIMEOUT→RUNNER_TIMEOUT）。 */
export type ErrorCode =
  | 'VALIDATION_ERROR'
  | 'UNAUTHENTICATED'
  | 'FORBIDDEN'
  | 'NOT_FOUND'
  | 'CONFLICT'
  | 'RUNNER_NOT_FOUND'
  | 'RUNNER_VERSION_PINNED'
  | 'RUNNER_INVALID'
  | 'EXECUTION_PROFILE_NOT_FOUND'
  | 'TASK_TOO_LARGE'
  | 'ATTEMPT_EXHAUSTED'
  | 'RESOURCE_EXHAUSTED'
  | 'ARTIFACT_QUOTA_EXCEEDED'
  | 'EXECUTOR_UNAVAILABLE'
  | 'IMAGE_PULL_FAILED'
  | 'MOUNT_UNAVAILABLE'
  | 'EXECUTOR_POD_ERROR'
  | 'EXPORT_INVALID'
  | 'DEPLOY_FAILED'
  | 'DEPLOY_BLOCKED'
  | 'ENDPOINT_UNREACHABLE'
  | 'PRECHECK_FAILED'
  | 'RUNNER_TIMEOUT'
  | 'RUNNER_NONZERO_EXIT'
  | 'SLA_PRECHECK_FAILED'
  | 'SLA_NO_FEASIBLE_CONCURRENCY'
  | 'RESULT_MISSING'
  | 'RESULT_INVALID'
  | 'CLEANUP_FAILED'
  | 'CANCELED'
  | 'INTERNAL_ERROR'

/** `Error` schema：错误信封（07 §3）。`trace_id` 与响应头 `X-Request-Id` 同源。 */
export interface ErrorBody {
  code: ErrorCode | string
  message: string
  detail?: Record<string, unknown>
  retryable: boolean
  trace_id?: string
}

/** 历史别名：页面与 hook 里统一用它表示"错误信封"。 */
export type ApiErrorBody = ErrorBody

/** `Failure` schema：Task / attempt / runner 同构的失败结构（没有 trace_id，retryable 是实例级的）。 */
export interface Failure {
  code: ErrorCode | string
  message: string
  detail?: Record<string, unknown>
  retryable: boolean
}

/** `ValidationIssue` schema：`detail.issues[]` 的一项（jsonl 带 `line`）。 */
export interface ValidationIssue {
  path: string
  code: string
  message: string
  line?: number
}

/** 统一分页信封（`TaskList` / `AttemptList` / `RunnerExecutionList` / `RunnerList` / `EventList`）。 */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
  has_more: boolean
  /** 满足过滤条件的总条数：**只在 offset 分页（page/page_size）下计算**，游标分页为 null */
  total?: number | null
  page?: number | null
  page_size?: number | null
}

/* ================================================================ 枚举（与 03 §3.2 CHECK 约束逐字一致） */

/** `TaskStatus`：Task 状态机（§7.4，D-66/D-73）。**没有** deploying/testing/cleanup 平台阶段；**没有** completed_with_errors。 */
export type TaskStatus =
  | 'submitted'
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'service_unavailable'
  | 'cancelling'
  | 'canceled'

/** `RunnerStatus`：runner 状态机 8 值（§7.1，D-68/D-69）。FAILED 是终态，没有重试回路。 */
export type RunnerStatus =
  | 'pending'
  | 'queued'
  | 'preparing'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'skipped'
  | 'canceled'

/** `AttemptStatus`：Task 级 attempt 聚合状态；同一 Task 至多一个活跃 attempt。 */
export type AttemptStatus = 'running' | 'succeeded' | 'failed' | 'canceled'

/** `AttemptTrigger`：`initial` 首次执行；`retry` 由 POST /tasks/{id}/retry 产生（fresh deploy）。 */
export type AttemptTrigger = 'initial' | 'retry'

/** `WaitingReason`（v2 4 值，D-32/D-69：precheck_gate / global_limit 已随门控取消）。 */
export type WaitingReason = 'slot_waiting' | 'queue_full' | 'insufficient_resources' | 'gang_pending'

/** `SkippedReason`：SKIPPED 表示"没跑"，必须带 reason（D-06）。 */
export type SkippedReason = 'upstream_abort' | 'canceled'

/** `RunnerRole`：`deploy` / `clean` 仅内置（service-deploy / service-clean，D-70）。 */
export type RunnerRole = 'test' | 'pre_check' | 'deploy' | 'clean'

/** `OnFailure`（D-69 更名）：v1 `abort_run` → v2 `abort_task`。 */
export type OnFailure = 'continue' | 'abort_task'

/** `InputFormat`（D-67）：`yaml` / `text` 只做语法校验；此时 `parameters` 必须为 `[]`。 */
export type InputFormat = 'jsonl' | 'yaml' | 'text'

/** `EndpointSource`：`genet` 来自 deploy exports 收割，`service` 来自 Manifest 静态直填。 */
export type EndpointSource = 'genet' | 'service'

/** `DeployState`：deploy-state.json 的 phase（D-70）；`blocked` 收口为 `service_unavailable`。 */
export type DeployState = 'queued' | 'accepted' | 'deploying' | 'ready' | 'blocked'

/** `DeployUninstall.state`：拆除收口（D-54/D-70）；clean 失败不回溯 Task 终态。 */
export type DeployUninstallState = 'pending' | 'running' | 'verified' | 'skipped' | 'failed'

/* ================================================================ 端点 / 部署 */

/** `Endpoint` schema：被测服务接入点；三个公共参数经 `AG_SERVED_URL` 等注入 runner（§5.4.1）。 */
export interface Endpoint {
  url: string
  /** 互斥键 `scheme://host:port`（不含路径）；仅外接模式互斥 */
  key: string
  source: EndpointSource
  model_name?: string | null
  model_path?: string | null
  protocol?: string | null
  /** 来源部署的 release 名（`source=genet` 时有值，`AG_DEPLOY_REF`） */
  deploy_ref?: string | null
}

/** `DeployQueue`：队列水位 `{phase, pending, inqueue, running, position}`（D-46：**只有计数，没有位次**）。 */
export interface DeployQueue {
  phase?: string | null
  pending?: number | null
  inqueue?: number | null
  running?: number | null
  /** **恒为 null（D-46）**：排队发生在 Volcano 层，Genet 只给计数。前端不得展示「第 N 位」。 */
  position?: number | null
}

/** `Deploy` schema：`tasks.deploy` —— 提交冻结的 runner 引用 + deploy-state.json 副本摘要的合成（deploy/clean 解耦）。 */
export interface Deploy {
  /** 提交冻结的 deploy runner 名（如 `genet-deploy` / `existing-deploy`） */
  runner?: string
  /** 解析后的 runner 版本 */
  version?: string
  /** 该 runner 的输入原文（`deploy.input`，平台不解析语义） */
  input?: string | null
  spec_digest?: string | null
  namespace?: string | null
  /** Helm release 名：平台派生 `arg-` + sha256(task_id:attempt_no)[:12]（D-44） */
  release_name?: string | null
  deploy_state_ref?: string | null
  /** deploy-state.json 的 phase；runner 未启动时为 null */
  phase?: DeployState | null
  /** Helm `status` 原值（`pending-install`/`deployed`/`failed`/…），原样保留不翻译（D-45） */
  genet_status?: string | null
  /** `phase=blocked` 时的归类原因原文（D-47） */
  reason?: string | null
  /** 部署排队/执行已等待时长（UI 展示用；**取代 v1 的位次**，D-46） */
  waited_seconds?: number | null
  pod_count?: number | null
  /** 服务 Pod 的 label selector（collector 按 Pod 落盘日志的依据） */
  label_selector?: string | null
  queue?: DeployQueue | null
  endpoint?: Endpoint | null
  queued_at?: Timestamp | null
  ready_at?: Timestamp | null
  cancel_requested_at?: Timestamp | null
  /** 部署失败详情：code ∈ {DEPLOY_FAILED, DEPLOY_BLOCKED}（D-47） */
  failure?: Failure | null
  uninstall?: DeployUninstall | null
}

/** `DeployUninstall`：拆除收口（service-clean finally 语义）。 */
export interface DeployUninstall {
  state: DeployUninstallState
  verified_at?: Timestamp | null
  cleanup_errors?: string[]
}

/** `DeployPod` schema：deploy-state.json 的 Pod 清单快照一项（§9.2）。 */
export interface DeployPod {
  name: string
  /** 部署方给出的角色（`router` / `prefill` / `decode` / `main`），平台只透传 */
  role?: string | null
  node?: string | null
  restarts?: number
  ready?: boolean
  started_at?: Timestamp | null
  last_seen_at?: Timestamp | null
}

/** `LogSegment` schema：一份部署 Pod 日志文件的覆盖区间（滚档后为 `<pod>.N.log`）。 */
export interface LogSegment {
  file: string
  start?: Timestamp | null
  end?: Timestamp | null
  restarts?: number
  truncated?: boolean
}

/** `Artifact` schema：产物条目；`path` 相对 `<storage-root>/<task_id>/`。 */
export interface Artifact {
  /** task 目录相对路径（如 `attempts/1/runners/3/result.json`） */
  path: string
  kind: 'result' | 'raw' | 'log' | 'report' | 'export' | 'other'
  size_bytes: number
  sha256?: string | null
  attempt_no?: number | null
  runner_seq?: number | null
}

/** `ArtifactList` schema：Task / runner 两级产物清单的统一信封。 */
export interface ArtifactList {
  items: Artifact[]
  total_size_bytes?: number
}

/** `DeploySnapshot` schema：`GET /tasks/{task_id}/deploy` 的应答；外接模式 `deploy: null`、`pods: []`。 */
export interface DeploySnapshot {
  task_id: string
  deploy: Deploy | null
  pods: DeployPod[]
  /** 部署总窗口（默认 72h；超窗判 DEPLOY_BLOCKED(reason=timeout)） */
  deploy_timeout_seconds?: number
}

/* ================================================================ Deploy 日志（契约内） */

/** `DeployLogPod` schema：Deploy 日志的 Pod 页签。 */
export interface DeployLogPod {
  name: string
  /** 至少一个容器 Ready */
  ready: boolean
  /** 容器最大重启次数（>0 会产生 gap） */
  restarts: number
  started_at?: string | null
  last_seen_at?: string | null
}

/** `DeployLogEntry` schema：一个 Pod 的一段连续日志行。 */
export interface DeployLogEntry {
  pod: string
  container: string
  /** 该段在本 Pod 日志流里的起始行号 */
  offset: number
  line_count: number
  /** 多行正文，行间 `\n` */
  text: string
}

/** `DeployLogGap` schema：事实缺口（Pod 重启 / 晚于 deploy 受理才启动），平台不假装连续。 */
export interface DeployLogGap {
  pod: string
  from: string
  to: string
  /** `pod restarted` | `pod started after deploy accepted` */
  reason: string
}

/** `DeployLogs` schema：`GET /tasks/{id}/deploy/logs`；排队期 `pods/entries` 为空 + `message`（不是 404）。 */
export interface DeployLogs {
  task_id: string
  attempt_no?: number
  pods: DeployLogPod[]
  entries: DeployLogEntry[]
  gaps: DeployLogGap[]
  /** `full` = 全文窗口（可续页）；`tail` = 末尾窗口（一次性，不可续页） */
  window: 'full' | 'tail'
  truncated: boolean
  /** 下一次请求的 offset；0 表示没有更多 */
  next_offset: number
  /** 排队中 / 外接模式 / 无 Pod 时的说明 */
  message?: string
}

export interface DeployLogQuery {
  /** attempt 序号；缺省取最新 attempt */
  attempt?: number
  /** 只看这个 Pod（页签）；不传时单 Pod 直接服务，多 Pod 409 */
  pod?: string
  offset?: number
  limit?: number
  /** 只取该 Pod 末尾 N 行（先看最新）；与 offset 互斥 */
  tail_lines?: number
}

/* ================================================================ Task / attempt / runner 投影 */

/** `Task.runner_counts`：最新 attempt 的 runner 状态计数摘要（"2 / 4"）。 */
export interface RunnerCounts {
  total: number
  succeeded: number
  failed: number
  pending?: number
  running?: number
  skipped?: number
  canceled?: number
}

/** `SecretEnv` schema：只存引用名，永不返回值（C-10）。 */
export interface SecretEnv {
  name: string
  secret_ref: string
}

/**
 * `Task` schema：Task 投影（`tasks` 表）。列表项不带 `runners`；详情带 attempt 列表与最新 attempt 的 runner 摘要。
 * **没有** `kind` / `cleanup_policy` / `args`（v2 取消）。
 */
export interface Task {
  id: string
  name: string
  status: TaskStatus
  submitted_by: string
  submitted_at: Timestamp
  started_at?: Timestamp | null
  finished_at?: Timestamp | null
  endpoint?: Endpoint | null
  deploy?: Deploy | null
  env?: Record<string, string>
  secret_env?: SecretEnv[]
  error?: Failure | null
  /** P1 跨 Task 续跑来源（仅外接模式）；非续跑为 null */
  retry_of?: string | null
  /** 最新 attempt 序号；0 = 尚未开始；重试只追加不覆盖（§7.2） */
  current_attempt: number
  runner_counts?: RunnerCounts
  revision?: number
}

/** `TaskDetail` schema：Task 投影 + attempt 列表 + 最新 attempt 的 runner 摘要（`GET /tasks/{id}`）。 */
export interface TaskDetail extends Task {
  attempts: Attempt[]
  /** 最新 attempt 的 runner 摘要，按 seq 升序（完整字段走 attempts/{n}/runners/{seq}） */
  runners: RunnerExecution[]
}

/** `Attempt` schema：Task 的第 N 次全量执行（`task_attempts` 表，D-68）。 */
export interface Attempt {
  task_id: string
  /** 从 1 递增；(task_id, attempt_no) 唯一 */
  attempt_no: number
  status: AttemptStatus
  trigger: AttemptTrigger
  input_ref?: string | null
  exports_ref?: string | null
  deploy_state_ref?: string | null
  error?: Failure | null
  started_at?: Timestamp | null
  finished_at?: Timestamp | null
}

/** `ExecutorRef` schema：外部执行体定位（每 runner 一个 Job，幂等键 (task_id, attempt_no, seq)）。 */
export interface ExecutorRef {
  cluster: string
  namespace: string
  job: string
}

/**
 * `RunnerExecution` schema：runner 执行记录投影（`task_runners` 表，D-68）。
 * 定位键 `(task_id, attempt_no, seq)`；attempt 内按序执行恰好一次，没有自己的重试（D-69）。
 * `result_ref` 可空：产物三选一（D-74），仅产出 html/csv 或 deploy/clean 角色为 null。
 */
export interface RunnerExecution {
  task_id: string
  attempt_no: number
  /** 执行序；(task_id, attempt_no, seq) 唯一 */
  seq: number
  /** Task 内唯一（用户 runners[].name；注入的为 service-deploy / precheck / service-clean） */
  name: string
  /** 冻结的 Runner 名称（name@version 引用，§5.7） */
  runner_name: string
  runner_version: string
  role: RunnerRole
  image_ref: string
  image_digest?: string | null
  profile: string
  input_ref: string
  on_failure: OnFailure
  timeout_seconds: number
  status: RunnerStatus
  /** 仅 skipped 时必填 */
  status_reason?: SkippedReason | null
  waiting_reason?: WaitingReason | null
  /** 外接模式互斥键；部署模式在 deploy exports 收割后回填 */
  endpoint_key?: string | null
  endpoint_exclusive?: boolean
  exit_code?: number | null
  error_code?: ErrorCode | string | null
  error?: Failure | null
  /** 收割落档引用；失败/SKIPPED 不收割，为 null（§5.12） */
  exports_ref?: string | null
  /** `attempts/<n>/runners/<seq>/result.json`；产物三选一（D-74），未产出 json 时为 null */
  result_ref?: string | null
  log_ref?: string | null
  started_at?: Timestamp | null
  finished_at?: Timestamp | null
  executor_ref?: ExecutorRef | null
}

/* ================================================================ Runner 目录（附录 C v2） */

/** `RunnerParameter` schema：jsonl 输入的结构校验 schema 一项（D-67：平台校验类型 + required）。 */
export interface RunnerParameter {
  name: string
  /** JSON 类型名（string / integer / number / boolean / object / array） */
  type: string
  required?: boolean
  /** 文档语义默认值（平台不校验） */
  default?: unknown
  /** 文档语义（范围归 Runner） */
  minimum?: unknown
  description?: string
}

/** `EnvVarDecl` schema：Runner 对某个环境变量的**读取声明**；值由 Task 级 env/secrets 提供（§12.4）。 */
export interface EnvVarDecl {
  required?: boolean
  secret?: boolean
  description?: string
}

/** `ResourceRequest` schema（附录 C）。 */
export interface ResourceRequest {
  cpu: string
  memory: string
  gpu?: { resource?: string; count?: number }
}

/** `ConcurrencyPolicy` schema：仅外接模式有效（§6.7，D-69）。 */
export interface ConcurrencyPolicy {
  endpoint_exclusive: boolean
}

/** `RunnerDefinition` schema（`runner.yaml`，附录 C **v2**）。`entry` 是 **exec form 数组**。 */
export interface RunnerDefinition {
  name: string
  version: string
  display_name: string
  description: string
  role: RunnerRole
  on_failure: OnFailure
  /** exec form 数组（如 `["/runner/run.sh"]`）；平台 exec 该数组、argv 恒空（D-66） */
  entry: string[]
  input_format: InputFormat
  image: string
  resources?: ResourceRequest
  concurrency?: ConcurrencyPolicy
  mounts?: Array<{ name: string; mount_path: string; read_only?: boolean; claim_name?: string }>
  parameters: RunnerParameter[]
  /** 本 Runner 产出的下游 key 列表（§5.12） */
  exports?: string[]
  /** 需要的上游 key 列表（如 AG_SERVED_URL） */
  consumes?: string[]
  /** 声明本 Runner 读取哪些环境变量 */
  env?: Record<string, EnvVarDecl>
  /** 与 `input_format` 同格式的合法片段，UI 输入框占位；平台不解析 */
  placeholder?: string
  /** deploy 角色声明的配对 clean runner 名（runner.yaml `clean_runner`）；未声明 = 无 clean 阶段 */
  clean_runner?: string
}

/**
 * `Runner` schema（wire）：`runner.yaml` 结构 + 注册表元数据。
 * `parameters` 是 **数组**（v2：jsonl 校验 schema 的有序列表）、`env` 是 map。
 */
export interface RunnerWire extends RunnerDefinition {
  content_sha?: string
  source_file?: string
  orphaned?: boolean
  /** 该 (name, version) 是否为这个名字的最高版本 */
  latest?: boolean
  /** 已退役：目录里不再存在，历史 Task 仍按 name@version 解析（§5.6.1） */
  retired?: boolean
  removed_at?: Timestamp | null
  registered_at?: Timestamp
  registered_by?: string
}

/**
 * UI 侧归一后的 Runner：`parameters` 按数组渲染、`exclusive` 提平。
 * 归一发生在 `api/normalize.ts`（wire 形状始终是上面的 `RunnerWire`）。
 */
export interface Runner {
  name: string
  version: string
  display_name?: string
  description?: string
  role: RunnerRole
  on_failure: OnFailure
  /** exec form 数组原样（展示为 JSON 数组或首元素） */
  entry?: string[]
  input_format: InputFormat
  image: string
  /** 来自 `concurrency.endpoint_exclusive`；仅外接模式有效 */
  exclusive: boolean
  /** deploy 角色声明的配对 clean runner 名（runner.yaml `clean_runner`）；未声明 = 无 clean 阶段 */
  clean_runner?: string
  placeholder?: string
  parameters: RunnerParameter[]
  env: Array<EnvVarDecl & { name: string }>
  exports?: string[]
  consumes?: string[]
  latest?: boolean
  retired?: boolean
  orphaned?: boolean
  source_file?: string
  registered_at?: Timestamp
}

/* ================================================================ 集群 / 队列 */

/** `SharedMount` schema：平台级统一共享挂载的一条（D-63/D-65）。 */
export interface SharedMount {
  name: string
  mount_path: string
  read_only: boolean
  claim_name: string
  enabled: boolean
  created_at?: Timestamp | null
  updated_at?: Timestamp | null
}

/** `SharedMountUpsert` schema：POST/PUT 的请求体（PUT 省略字段 = 保留旧值；`name` 不可改）。 */
export interface SharedMountWrite {
  name: string
  mount_path: string
  claim_name: string
  read_only?: boolean
  enabled?: boolean
}

/** PUT 的局部更新：`name` 不在里面（不可改） */
export type SharedMountPatch = Partial<Omit<SharedMountWrite, 'name'>>

/** `Cluster` schema：集群概要（不返回凭据）。 */
export interface Cluster {
  name: string
  api_server: string
  description?: string | null
  last_check_at?: Timestamp | null
  last_check_result?: string | null
}

/** `EndpointSlot` 的持有者（`(task_id, attempt_no, seq)` 定位）。 */
export interface EndpointHolder {
  task_id: string
  attempt_no: number
  seq: number
}

/** `EndpointSlot` schema：端点互斥租约（仅外接模式产生）。 */
export interface EndpointSlot {
  endpoint_key: string
  exclusive?: boolean
  holders: EndpointHolder[]
}

/** `PlatformSlots` schema：平台自身槽位。 */
export interface PlatformSlots {
  concurrency_max: number
  active: number
  available: number
  endpoints?: EndpointSlot[]
}

/** `WaitingReasonCounts` schema：四个取值都必填，没有排队记 0（不省略键）。 */
export interface WaitingReasonCounts {
  slot_waiting: number
  queue_full: number
  insufficient_resources: number
  gang_pending: number
}

/** `WaitingTask` schema：排队中的一个 runner 执行记录。 */
export interface WaitingTask {
  task_id: string
  attempt_no: number
  seq: number
  name: string
  status: RunnerStatus
  waiting_reason: WaitingReason
  detail?: Record<string, unknown>
}

/** `WaitingSummary` schema：排队 runner 的计数与明细。 */
export interface WaitingSummary {
  total: number
  by_reason: WaitingReasonCounts
  runners?: WaitingTask[]
}

/** `VolcanoQueue` schema：一个 Volcano Queue 的概要。 */
export interface VolcanoQueue {
  name: string
  state?: string | null
  capability?: Record<string, unknown>
  allocated?: Record<string, unknown>
  pending_jobs?: number
  longest_wait_seconds?: number
  /** PodGroup conditions[-1].message 的**原文**（不是枚举；结构化分类在 waiting.by_reason） */
  reason?: string | null
}

/** `VolcanoSnapshot` schema：不可达时只降级本块（detail 是四值枚举，D-49）。 */
export interface VolcanoSnapshot {
  available: boolean
  /** 不可达或无法判定时的降级原因：crd_missing / rbac_forbidden / scheduler_unavailable / disabled */
  detail?: 'crd_missing' | 'rbac_forbidden' | 'scheduler_unavailable' | 'disabled' | string | null
  /** 「上一次成功、本次失败」置位；绝不回放历史数字（D-49） */
  stale?: boolean
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

/** `EventType` schema（07 §7、§12.6；v2 取消 deploy.state_changed / cleanup.started 平台阶段事件）。 */
export type EventType =
  | 'submitted'
  | 'state_changed'
  | 'runner_started'
  | 'runner_finished'
  | 'exports_harvested'
  | 'clean_finished'
  | 'cleanup_failed'
  | 'task_finished'
  | 'report.written'
  | 'resync_required'

/** `Event` schema：SSE 帧的 data 与 `events/history` 的 items 同构；`seq` 每 Task 单调递增。 */
export interface TaskEvent {
  seq: number
  type: EventType | string
  task_id: string
  attempt_no?: number | null
  runner_seq?: number | null
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
  role?: 'viewer' | 'operator' | 'admin' | string
  csrf_token?: string
  groups?: string[]
}

/** `GET /api/v1/meta`。 */
export interface Meta {
  version: string
  api_version: string
  features?: Record<string, boolean>
}

/* ================================================================ Manifest（提交体：07 §4.1 + §10.1） */

/**
 * `DeployManifest` schema：部署模式提交 = **runner 引用**（与 `runners[]` 同构；deploy/clean
 * 解耦裁决）。平台按 runner+version 从注册表解析 role=deploy 的 Runner 注入为 seq=1；
 * clean 是否注入由该 runner 的 runner.yaml（clean_runner 声明）决定，提交体不出现 clean。
 */
export interface DeployManifest {
  /** deploy 角色的 runner 名（如 `genet-deploy` / `mock-deploy` / `existing-deploy`） */
  runner: string
  /** 语义化版本（runner.yaml 声明） */
  version: string
  /** 该 runner 的输入原文（格式按其 input_format：genet-deploy 为 YAML、existing-deploy 为 JSONL） */
  input?: string
}

/** `ServiceManifest` schema：外接模式，直接给已有服务（`AG_SERVED_URL` 等的来源）。 */
export interface ServiceManifest {
  url: string
  model_name?: string
  model_path?: string
}

/** `ExecutionSpec` schema：只能引用已登记的 Profile（§10.5）。 */
export interface ExecutionSpec {
  profile: string
}

/**
 * `RunnerSpec` schema：`runners[]` 一项。字段只有 name/runner/version/execution/input/on_failure/timeout_seconds；
 * **没有 args（argv 恒空，D-66）、没有 retry（D-69）、没有 cleanup.policy（D-70）**。
 */
export interface RunnerSpec {
  name: string
  runner: string
  version: string
  execution: ExecutionSpec
  /** 输入原文（JSONL / YAML 按 Runner 的 input_format；单条 ≤64 KiB） */
  input?: string
  on_failure?: OnFailure
  timeout_seconds?: number
}

/** `PrecheckSpec` schema：显式指定 pre_check Runner 的形态（C-5.2）。 */
export interface PrecheckSpec {
  runner: string
  version: string
  execution: ExecutionSpec
  input?: string
  on_failure?: OnFailure
  timeout_seconds?: number
}

/**
 * `TaskBody` schema：Manifest 的 `task:` 内容。`deploy` 与 `service` 二选一（D-27）。
 * **没有** `args`、`retry`、`cleanup.policy`（D-66/D-69/D-70）。
 */
export interface TaskBody {
  name: string
  deploy?: DeployManifest
  service?: ServiceManifest
  precheck?: 'auto' | 'off' | PrecheckSpec
  env?: Record<string, string>
  secrets?: Record<string, string>
  /** runner 列表（仅 test / 可选显式 pre_check）；1..200 条（D-57/§12.7） */
  runners?: RunnerSpec[]
}

/** `TaskManifest` schema：提交体，顶层是嵌套 `task:`（D-40）。 */
export interface TaskManifest {
  task: TaskBody
}

/** `DryRunResult` schema（`?dry_run=true`，200）。 */
export interface DryRunResult {
  valid: boolean
  issues?: ValidationIssue[]
  resolved_preview?: Record<string, unknown>
}

/** `AcceptedTaskRef` schema：202 里的 Task 摘要。 */
export interface AcceptedTaskRef {
  id: string
  status: TaskStatus
  submitted_at: Timestamp
  /** 202 时刻为 0（attempt 在 Task 进入 running 时创建） */
  current_attempt: number
}

/** `AcceptedRunner` schema：202 里的 runner 摘要（含注入行的最终执行序）。 */
export interface AcceptedRunner {
  seq: number
  name: string
  runner: string
  role?: RunnerRole
  status: RunnerStatus
}

/** `TaskAccepted` schema：`POST /tasks` 的 202 应答体。 */
export interface SubmitResult {
  task: AcceptedTaskRef
  runners: AcceptedRunner[]
}

/** `CancelAccepted` schema：202 与「已是 cancelling/canceled」的 200 同构。 */
export interface CancelResult {
  task_id: string
  status: TaskStatus
}

/** `TaskRetryAccepted` schema：`POST /tasks/{id}/retry` 的 202（整 Task 新 attempt，全量重跑含 fresh deploy）。 */
export interface TaskRetryAccepted {
  task_id: string
  /** 新 attempt 序号（current_attempt + 1） */
  attempt_no: number
  /** 重试后的 Task 状态（202 时刻为 queued） */
  status: 'queued'
}

/* ================================================================ 查询参数 */

export interface TaskListQuery {
  /** 多值，逗号分隔（`status=running,queued`） */
  status?: string
  /** Runner 名称；返回包含该 Runner 执行记录的 Task */
  runner?: string
  /** 多值，逗号分隔（`submitter=dev,qa`） */
  submitter?: string
  /** 游标分页（agent / CLI 用）；与 page 互斥，给了 page 就忽略 cursor（D-61） */
  cursor?: string | null
  limit?: number
  /** offset 分页页码（从 1 开始）；UI 走这套拿「第几页 / 共几页」 */
  page?: number
  /** offset 分页每页条数（默认 20，上限 100） */
  page_size?: number
}

/** `FacetBucket` schema：表头列筛选的候选值与计数（Excel 式）。 */
export interface FacetBucket {
  value: string
  count: number
}

/**
 * `GET /tasks/facets` 的应答：v2 维度为 `status` / `submitter` / `runner`（`kind` 随占位 Run 概念取消）。
 * 计数按维度独立（标准 facet 语义）。
 */
export interface TaskFacets {
  status: FacetBucket[]
  submitter: FacetBucket[]
  runner: FacetBucket[]
}

/** `/tasks/facets` 的查询：与 `GET /tasks` 同名同义（多值同样用逗号） */
export interface TaskFacetsQuery {
  status?: string
  runner?: string
  submitter?: string
}

export interface RunnerListQuery {
  role?: RunnerRole | string
  orphaned?: boolean
  /** 历史版本：默认只回最新，带了才把已退役的版本一起回（D-60） */
  include_retired?: boolean
}

export interface RunnerLogsQuery {
  attempt?: number
  stream?: 'stdout' | 'stderr'
  /** 起始偏移（字节） */
  offset?: number
  /** 本次返回的字节上限（服务端按 ≤64 KiB 收敛） */
  limit?: number
}

/* ================================================================ 看板统计（前端现算） */

/** P0 没有 `/board`：由 `/tasks` + `/runners` + `/queue-status` 现算。 */
export interface DashboardStats {
  active_tasks: number
  queued_runners: number
  failed_24h: number
  success_rate: number | null
  total_tasks: number
  runner_count: number
}

/* ================================================================ 报告（ARCHITECTURE §8.4） */

/** `ReportCounts` schema。 */
export interface ReportCounts {
  total: number
  succeeded: number
  failed: number
  skipped: number
  canceled: number
}

/** `ReportRunner` schema：报告里的单 runner 条目（按 seq 升序，含注入的 deploy/clean）。 */
export interface ReportRunner {
  seq: number
  name: string
  role: RunnerRole
  status: RunnerStatus
  exports_ref?: string | null
  /** `result.json` 相对引用；产物三选一（D-74），未产出 json 时为 null */
  result_ref?: string | null
  duration_ms?: number | null
}

/** `ReportFailure` schema：`{seq, name, error_code}`。 */
export interface ReportFailure {
  seq: number
  name: string
  error_code: ErrorCode | string
  reason?: string
}

/** `Report` schema：`GET /tasks/{id}/report`（`report.json`，只做组织不做聚合；默认最后一个 attempt）。 */
export interface Report {
  task_id: string
  attempt_no: number
  status: TaskStatus
  counts: ReportCounts
  runners: ReportRunner[]
  failures: ReportFailure[]
  /** service-clean 是否失败（不影响 Task 终态，只打 cleanup_failed 事件） */
  cleanup_failed?: boolean
  truncated?: boolean
  report_ref?: string
}

/* ================================================================ result.json（附录 A schema 2.0） */

/**
 * `ResultMetric` schema：`metrics[]` 的一项（附录 A / §8.3）。
 * 必须带 `unit` 与 `statistic`；同一 runner 内多组参数靠 `labels` 区分。
 */
export interface ResultMetric {
  name: string
  /** `null` 表示无法计算，必须在 `notes` 说明原因 */
  value: number | null
  unit: string
  statistic: 'mean' | 'p50' | 'p90' | 'p95' | 'p99' | 'min' | 'max' | 'sum' | 'count' | string
  sample_size?: number
  /** 标量维度（input_len / output_len / concurrency 不得丢失）；分组只发生在 UI */
  labels?: Record<string, unknown>
}

/** `ResultArtifact` schema：`result.json.artifacts[]` 的一项。 */
export interface ResultArtifact {
  name: string
  /** 相对 `/workspace/output` 的路径 */
  path: string
  size_bytes?: number
  sha256?: string
}

/**
 * `Result` schema（附录 A **schema 2.0**，D-73）：`GET /tasks/{id}/runners/{seq}/result` 的 `result.json` 原文。
 * 产物三选一（D-74）：不强求 `result.json`，未产出时端点 404（`detail.produced[]` 列实际产物）。
 * 平台只校验不收窄字段，允许未知字段；`attempt_no` / `runner_seq` 运行时取 `AG_ATTEMPT` / `AG_RUNNER_SEQ`。
 */
export interface Result {
  schema_version: string
  task_id: string
  attempt_no: number
  runner_seq: number
  runner: { name: string; version: string; image_ref?: string }
  status?: 'succeeded' | 'failed'
  started_at?: Timestamp
  finished_at?: Timestamp
  duration_ms?: number
  workload?: Record<string, unknown>
  /** 允许空数组（纯探测类），但需在 `notes` 说明 */
  metrics: ResultMetric[]
  error?: Failure | null
  artifacts?: ResultArtifact[]
  notes?: string
  metadata?: Record<string, unknown>
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
  /** service-deploy 的部署总窗口（§7.4，默认 72h） */
  deploy_timeout_seconds: number
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
