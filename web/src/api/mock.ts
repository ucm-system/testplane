/**
 * Mock 数据源 —— 后端未起时把页面填满（`VITE_USE_MOCK=1`，**默认关**）。
 *
 * 规则：
 *  - 实现的是 `Api` 接口，字段形状与 `server/api/openapi.yaml`（schema 2.0）一致；
 *    切真实接口时页面不改一行。
 *  - 数据有状态：提交 Task 会真的出现在列表里，取消走 cancelling → canceled（clean 收口），
 *    事件流（SSE）由 mock 按 runner 管道推进 —— 用来验收「事件流实时追加」这一条。
 *  - v2 形态：Task / attempt / runner 投影（`(task_id, attempt_no, seq)` 定位）、
 *    管道严格串行（service-deploy? → precheck → tests → service-clean?，D-70）、
 *    `result_ref` 可空（产物三选一 D-74）、没有 args / retry / cleanup.policy。
 */
import { ApiClientError, getCurrentUser } from './client'
import type { Api, LogOptions } from './contract'
import type {
  AdminCluster,
  AdminSecret,
  Artifact,
  ArtifactList,
  Attempt,
  AuditEntry,
  AuditQuery,
  CancelResult,
  Cluster,
  ClusterWrite,
  DashboardStats,
  DeployLogEntry,
  DeployLogGap,
  DeployLogPod,
  DeployLogQuery,
  DeployLogs,
  DeployPod,
  DeploySnapshot,
  DryRunResult,
  Endpoint,
  ExecutionProfile,
  Failure,
  Identity,
  LogChunk,
  Meta,
  Page,
  ProfileWrite,
  QueueStatus,
  Report,
  ReportCounts,
  ReportFailure,
  ReportRunner,
  Result,
  ResultMetric,
  Runner,
  RunnerExecution,
  RunnerListQuery,
  RunnerRole,
  RunnerStatus,
  RunnerWire,
  SecretWrite,
  SharedMount,
  SharedMountPatch,
  SharedMountWrite,
  ManifestSnapshot,
  SubmitResult,
  Task,
  TaskDetail,
  TaskEvent,
  EventType,
  FacetBucket,
  TaskFacets,
  TaskFacetsQuery,
  TaskListQuery,
  TaskManifest,
  TaskRetryAccepted,
  TaskStatus,
  ValidationIssue,
  WaitingReason,
  WaitingTask,
} from './types'
import { normalizeRunner } from './normalize'
import { applyIdentity, clearAuth, getAuthState } from './auth'

/* ================================================================ 基础工具 */

const T0 = Date.now()

/** 相对"现在"的 ISO 时间（分钟）。 */
function iso(minutesAgo: number): string {
  return new Date(T0 - minutesAgo * 60_000).toISOString()
}

function nowIso(): string {
  return new Date().toISOString()
}

function delay(ms = 180): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/** 平台保留路径（D-63）：这些目录由平台写入，映射进来会互相踩 */
const RESERVED_MOUNT_PATHS = ['/workspace/output', '/workspace/input', '/tmp', '/runner']

function sharedMountValidation(issues: ValidationIssue[]): ApiClientError {
  return new ApiClientError(400, {
    code: 'VALIDATION_ERROR',
    message: '共享挂载未通过校验',
    detail: { issues },
    retryable: false,
    trace_id: `mock-${Math.random().toString(16).slice(2, 10)}`,
  })
}

/**
 * 与后端一致的校验（D-65）：名称/路径/PVC 必填、路径必须 `/` 开头且不得命中保留路径、
 * 路径不得与其它挂载重复（`self` 是编辑中的那条，排除自身）。
 */
function validateSharedMount(body: SharedMountWrite, self: string | null): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const name = (body.name ?? '').trim()
  const mountPath = (body.mount_path ?? '').trim()
  const claimName = (body.claim_name ?? '').trim()

  if (!name) issues.push({ path: 'name', code: 'REQUIRED', message: '名称必填' })
  if (name.length > 63) issues.push({ path: 'name', code: 'CONFLICT', message: '名称不得超过 63 字节' })
  if (!mountPath) {
    issues.push({ path: 'mount_path', code: 'REQUIRED', message: 'Pod 内路径必填' })
  } else if (!mountPath.startsWith('/')) {
    issues.push({ path: 'mount_path', code: 'CONFLICT', message: '路径必须以 / 开头（容器内绝对路径）' })
  } else if (RESERVED_MOUNT_PATHS.some((reserved) => mountPath === reserved || mountPath.startsWith(`${reserved}/`))) {
    issues.push({
      path: 'mount_path',
      code: 'CONFLICT',
      message: `路径命中平台保留路径（${RESERVED_MOUNT_PATHS.join(', ')}），请换一个`,
    })
  }
  if (!claimName) issues.push({ path: 'claim_name', code: 'REQUIRED', message: 'PVC 名必填' })

  if (self === null && name && state.sharedMounts.some((mount) => mount.name === name)) {
    issues.push({ path: 'name', code: 'CONFLICT', message: `名称 ${name} 已存在` })
  }
  if (mountPath && mountPath.startsWith('/')) {
    const clash = state.sharedMounts.find((mount) => mount.name !== self && mount.mount_path === mountPath)
    if (clash) {
      issues.push({ path: 'mount_path', code: 'CONFLICT', message: `路径已被 ${clash.name} 占用` })
    }
  }
  return issues
}

function notFound(what: string): never {
  throw new ApiClientError(404, {
    code: 'NOT_FOUND',
    message: `${what} 不存在（mock 数据里没有这一条）`,
    retryable: false,
  })
}

function clone<T>(value: T): T {
  return structuredClone(value)
}

/** 端点互斥键：`scheme://host:port`（不含路径）。 */
function endpointKeyOf(url: string): string {
  try {
    const u = new URL(url)
    return `${u.protocol}//${u.host}`
  } catch {
    return url
  }
}

function endpointOf(url: string, modelName: string, source: Endpoint['source'], deployRef?: string): Endpoint {
  return {
    url,
    key: endpointKeyOf(url),
    source,
    model_name: modelName,
    model_path: '',
    protocol: 'http',
    deploy_ref: deployRef ?? null,
  }
}

/** attempt 上限（`AG_ATTEMPT_MAX`，默认 3，§12.7）：超限 `ATTEMPT_EXHAUSTED`。 */
const ATTEMPT_MAX = 3

/* ================================================================ Runner 目录（附录 C v2） */

function runnerWire(input: {
  name: string
  version: string
  display_name: string
  description: string
  role: RunnerRole
  on_failure: 'continue' | 'abort_task'
  exclusive: boolean
  input_format: 'jsonl' | 'yaml' | 'text'
  placeholder: string
  parameters: RunnerWire['parameters']
  exports?: string[]
  consumes?: string[]
  /** deploy 角色的配对 clean runner 名（runner.yaml clean_runner）；未声明 = 无 clean 阶段 */
  clean_runner?: string
}): RunnerWire {
  return {
    name: input.name,
    version: input.version,
    display_name: input.display_name,
    description: input.description,
    role: input.role,
    on_failure: input.on_failure,
    // exec form 数组（D-66）：平台 exec 该数组、argv 恒空
    entry: ['/runner/run.sh'],
    input_format: input.input_format,
    image: `registry.dev.huawei.com/argus/${input.name}:${input.version}`,
    resources: { cpu: '4', memory: '16Gi', gpu: { resource: 'nvidia.com/gpu', count: 0 } },
    concurrency: { endpoint_exclusive: input.exclusive },
    mounts: [],
    parameters: input.parameters,
    exports: input.exports ?? [],
    consumes: input.consumes ?? ['AG_SERVED_URL', 'AG_SERVED_MODEL_NAME'],
    env: {
      ENDPOINT_API_KEY: { secret: true, description: 'Bearer token；不提供时按无鉴权探测' },
    },
    clean_runner: input.clean_runner,
    placeholder: input.placeholder,
    content_sha: 'sha256:mock',
    source_file: `runners/${input.name}/runner.yaml`,
    orphaned: false,
    latest: true,
    registered_at: iso(60 * 24 * 7),
    registered_by: 'system',
  }
}

const RUNNERS: RunnerWire[] = [
  runnerWire({
    name: 'endpoint-probe',
    version: '1.0.0',
    display_name: '服务就绪探测',
    description:
      '对已部署的 LLM 服务执行生成型探活：POST <url>/chat/completions，max_tokens=1、stream=true，首包 token 一到即判定可服务。',
    role: 'pre_check',
    on_failure: 'abort_task',
    exclusive: true,
    input_format: 'jsonl',
    placeholder: '{"wait_timeout_seconds":600,"retry_interval_seconds":15}',
    parameters: [
      { name: 'wait_timeout_seconds', type: 'integer', required: false, default: 600, description: '就绪等待窗口（秒）' },
      { name: 'retry_interval_seconds', type: 'integer', required: false, default: 15, description: '两次探测间隔（秒）' },
      { name: 'max_tokens', type: 'integer', required: false, default: 1, description: '探活请求的 max_tokens，不要调大' },
      { name: 'stream', type: 'boolean', required: false, default: true, description: '流式探活；true 时首 token 到达即通过' },
    ],
    exports: ['AG_PRECHECK_OK'],
  }),
  // deploy 角色的内置 runner（服务来源卡的选项；clean 配对由 clean_runner 声明，解耦裁决）：
  runnerWire({
    name: 'genet-deploy',
    version: '1.0.1',
    display_name: 'Genet 服务部署（内置）',
    description: '读取 Genet 部署描述（语义原样透传），经 Genet OpenAPI 幂等拉起并轮询到就绪，导出四件套。',
    role: 'deploy',
    on_failure: 'abort_task',
    exclusive: false,
    input_format: 'yaml',
    placeholder: 'model: Qwen3-32B\nreplicas: 1\nresources:\n  gpu: 8',
    parameters: [],
    exports: ['AG_SERVED_URL', 'AG_SERVED_MODEL_NAME', 'AG_MODEL_PATH', 'AG_DEPLOY_REF'],
    consumes: [],
    clean_runner: 'genet-clean',
  }),
  runnerWire({
    name: 'mock-deploy',
    version: '1.1.2',
    display_name: 'Mock 服务部署（内置）',
    description:
      '接入一个 OpenAI 兼容的 mock 服务，等它就绪即可；不做任何部署动作，与 Genet 无关。缺省接 chart 内置 mock-llm，不产生 clean 阶段。',
    role: 'deploy',
    on_failure: 'abort_task',
    exclusive: false,
    input_format: 'jsonl',
    placeholder: '{"base_url":"http://mock-llm.argus.svc.cluster.local:18080/v1","model_name":"mock-llm"}',
    parameters: [
      { name: 'base_url', type: 'string', required: false, description: 'OpenAI 兼容基地址；缺省用 AG_MOCK_BASE_URL 的 default（chart 内置 mock-llm）' },
      { name: 'model_name', type: 'string', required: false, description: '服务端模型名；缺省 mock-llm' },
      { name: 'model_path', type: 'string', required: false, description: '容器内模型权重路径；mock 服务无权重，一般不填' },
      { name: 'ready_timeout_seconds', type: 'integer', required: false, description: '就绪探测总窗口（秒），缺省 120；超窗 DEPLOY_BLOCKED(reason=timeout)' },
    ],
    exports: ['AG_SERVED_URL', 'AG_SERVED_MODEL_NAME', 'AG_MODEL_PATH', 'AG_DEPLOY_REF'],
    consumes: [],
  }),
  runnerWire({
    name: 'genet-clean',
    version: '1.0.1',
    display_name: '服务拆除（内置）',
    description: '消费 AG_DEPLOY_REF 调 Genet 卸载 release 并判干净；finally 语义执行。仅由 deploy runner 的 clean_runner 声明注入。',
    role: 'clean',
    on_failure: 'abort_task',
    exclusive: false,
    input_format: 'jsonl',
    placeholder: '{}',
    parameters: [],
    exports: [],
    consumes: ['AG_DEPLOY_REF'],
  }),
  runnerWire({
    name: 'aisbench-acc',
    version: '1.0.0',
    display_name: '数据集精度评测',
    description: '数据集精度评测，输出逐数据集 accuracy。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    input_format: 'jsonl',
    placeholder: '{"dataset":"/data/gsm8k.jsonl","limit":200}',
    parameters: [
      { name: 'dataset', type: 'string', required: true, description: '数据集在容器内的绝对路径' },
      { name: 'limit', type: 'integer', required: false, description: '最多评测多少条样本' },
    ],
  }),
  runnerWire({
    name: 'aisbench-perf',
    version: '1.0.0',
    display_name: '性能压测',
    description: '性能压测，输出 TTFT / TPOT / 吞吐。独占 Endpoint，避免与其它 Task 的 runner 互相干扰。',
    role: 'test',
    on_failure: 'continue',
    exclusive: true,
    input_format: 'jsonl',
    placeholder: '{"input_len":512,"output_len":128,"concurrency":8}',
    parameters: [
      { name: 'input_len', type: 'integer', required: true, description: '输入长度' },
      { name: 'output_len', type: 'integer', required: true, description: '输出长度' },
      { name: 'concurrency', type: 'integer', required: true, description: '并发档位' },
    ],
  }),
  // 票 #60 / D-60：同名多版本 —— 0.2.0 是当前版，0.1.0 已退役（目录里已移除，历史 Task 仍按 name@version 引用）
  runnerWire({
    name: 'aisbench-sla',
    version: '0.2.0',
    display_name: 'SLA 时延评测',
    description: '按目标 SLA 测首包与每 token 时延，输出达标率。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    input_format: 'jsonl',
    placeholder: '{"target_ttft_ms":800,"target_tpot_ms":30,"samples":200}',
    parameters: [
      { name: 'target_ttft_ms', type: 'integer', required: true, description: '首包时延目标（毫秒）' },
      { name: 'target_tpot_ms', type: 'integer', required: false, description: '每 token 时延目标（毫秒）' },
      { name: 'samples', type: 'integer', required: false, description: '采样条数' },
    ],
  }),
  {
    ...runnerWire({
      name: 'aisbench-sla',
      version: '0.1.0',
      display_name: 'SLA 时延评测',
      description: '旧版：只测首包时延，没有 TPOT 目标。',
      role: 'test',
      on_failure: 'continue',
      exclusive: false,
      input_format: 'jsonl',
      placeholder: '{"target_ttft_ms":900}',
      parameters: [
        { name: 'target_ttft_ms', type: 'integer', required: true, description: '首包时延目标（毫秒）' },
      ],
    }),
    // 已退役：默认不出现在目录里，只有 ?include_retired=true 才回
    retired: true,
    latest: false,
  },
  runnerWire({
    name: 'genet-yaml-check',
    version: '1.1.0',
    display_name: '部署描述静态检查',
    description: '对 Genet 部署描述（YAML）做静态体检：必填段、缩进、显存估值。演示 input_format=yaml 的 Runner。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    input_format: 'yaml',
    placeholder: 'model: Qwen3-32B\nreplicas: 1\nresources:\n  gpu: 8',
    parameters: [],
    consumes: [],
  }),
]

/* ================================================================ 内部状态 */

interface MockState {
  tasks: Record<string, TaskDetail>
  /** `${taskId}:${attemptNo}` → 该 attempt 的 runner 执行记录（按 seq 升序） */
  attemptRunners: Record<string, RunnerExecution[]>
  /** `${taskId}:${attemptNo}:${seq}` → runner 日志文本 */
  logs: Record<string, string>
  /** 运行中日志的"新行"计数：模拟服务端 stdout 在两次轮询之间增长 */
  logTicks: Record<string, number>
  /** `${taskId}:${attemptNo}#${pod}` → Deploy 日志行（持久化，轮询时按 offset 追加） */
  deployLogLines: Record<string, string[]>
  deployLogTicks: Record<string, number>
  /** `${taskId}:${attemptNo}` → DeploySnapshot */
  deploys: Record<string, DeploySnapshot>
  events: Record<string, TaskEvent[]>
  seq: Record<string, number>
  runners: RunnerWire[]
  /** `${taskId}` → 提交原文快照（C-8.2 / D-76 重试回填用） */
  manifests: Record<string, TaskManifest>
  clusters: Cluster[]
  /** `/admin/*` 的数据（P1） */
  profiles: ExecutionProfile[]
  adminClusters: AdminCluster[]
  /** 平台级统一共享挂载（D-65）：运行时真源，默认空 */
  sharedMounts: SharedMount[]
  secrets: AdminSecret[]
  audit: AuditEntry[]
  timers: Map<string, ReturnType<typeof setInterval>>
}

const state: MockState = {
  sharedMounts: [],
  tasks: {},
  attemptRunners: {},
  logs: {},
  logTicks: {},
  deployLogLines: {},
  deployLogTicks: {},
  deploys: {},
  events: {},
  seq: {},
  runners: clone(RUNNERS),
  manifests: {},
  clusters: [],
  profiles: [],
  adminClusters: [],
  secrets: [],
  audit: [],
  timers: new Map(),
}

/** mock 的"长时间运行"runner：状态机不推进它（用来演示 / 验证日志轮询） */
const stickyRunners = new Set<string>()

/* ================================================================ 造数据 */

interface RunnerSeed {
  name: string
  runner: string
  role?: RunnerRole
  status?: RunnerStatus
  input?: string
  waiting?: WaitingReason
  statusReason?: 'upstream_abort' | 'canceled'
  error?: Failure
  errorCode?: string
  startedMinAgo?: number
  durationSec?: number
  sticky?: boolean
}

interface TaskSeed {
  id: string
  name: string
  status: TaskStatus
  submittedBy: string
  submittedMinAgo: number
  durationSec?: number
  /** deploy 模式（含 Genet 部署描述原文）；不填则看 service */
  deploySpec?: string
  deployState?: 'queued' | 'accepted' | 'deploying' | 'ready' | 'blocked'
  waitedSeconds?: number
  pods?: DeployPod[]
  /** 外接模式（service）：直接给已有服务 */
  service?: { url: string; model_name?: string; model_path?: string }
  error?: Failure
  env?: Record<string, string>
  secrets?: Record<string, string>
  runners: RunnerSeed[]
  /** 已跑过的 attempt 数 - 1（历史 attempt 全部 succeeded 略化） */
  extraAttempts?: number
}

/** 注入的内置 runner 行名（D-70：用户不可手动添加）。deploy/clean 行名 = 引用的 runner 名（种子自带）。 */
const INJECTED = { precheck: 'precheck' } as const

function runnerNameOf(seed: RunnerSeed): string {
  if (seed.runner === 'endpoint-probe') return INJECTED.precheck
  return seed.name
}

function runnerLog(seed: RunnerSeed, startedAt: string | null | undefined): string {
  const t = startedAt
    ? new Date(startedAt).toLocaleTimeString('zh-CN', { hour12: false })
    : '--:--:--'
  if (seed.role === 'deploy') {
    return [
      `[${t}] [genet] release arg-mock0001 install --no-wait`,
      `[${t}] [genet] phase=deploying: pulling weights for Qwen3-32B`,
      `[${t}] [genet] phase=ready: endpoint written to exports (AG_SERVED_URL)`,
    ].join('\n')
  }
  if (seed.runner === 'endpoint-probe') {
    return [
      `[${t}] 探活 <AG_SERVED_URL>/chat/completions max_tokens=1 stream=true`,
      `[${t}] 第 1 次探测：未就绪 —— HTTP 503 {"error":"model loading"}`,
      `[${t}] 第 2 次探测：HTTP 200，收到 SSE 数据帧，服务可出 token`,
      `结论：status=succeeded（等待 38s）`,
    ].join('\n')
  }
  if (seed.role === 'clean') {
    return [`[${t}] [genet] uninstall release arg-mock0001`, `[${t}] [genet] GET /releases/arg-mock0001 → 404，判定干净`].join('\n')
  }
  if (seed.status === 'failed') {
    return [
      `[${t}] loading input ${seed.input ?? ''}`,
      `[${t}] ERROR connection reset by peer (attempt=1)`,
      `[${t}] Traceback (most recent call last):`,
      `[${t}] requests.exceptions.ConnectionError: RemoteDisconnected`,
      `结论：status=failed exit=1 code=RUNNER_NONZERO_EXIT`,
    ].join('\n')
  }
  if (seed.runner === 'aisbench-perf') {
    return [
      `[${t}] [sweep] input_len=512 output_len=128 concurrency=1..8`,
      `[${t}] [sweep] 24 combos, 24 ok`,
      `[${t}] ttft p95=143.7ms  tpot mean=21.4ms  throughput=1482.6 tokens/s`,
    ].join('\n')
  }
  return [`[${t}] input ${seed.input ?? ''}`, `[${t}] 200 samples done`, `[${t}] accuracy=0.8421`].join('\n')
}

/** 组装一个 attempt 的 runner 执行记录（严格串行：seq 从 1 递增）。 */
function buildRunners(taskId: string, attemptNo: number, seeds: RunnerSeed[]): RunnerExecution[] {
  return seeds.map((seed, index) => {
    const seq = index + 1
    const wire = state.runners.find((r) => r.name === seed.runner)
    const started = seed.startedMinAgo == null ? null : iso(seed.startedMinAgo)
    const startedAt = started ? new Date(started) : null
    const finishedAt =
      startedAt && seed.durationSec != null ? new Date(startedAt.getTime() + seed.durationSec * 1000).toISOString() : null
    const status = seed.status ?? 'pending'
    const hasResult = ['test', 'pre_check'].includes(seed.role ?? 'test') && status === 'succeeded'
    return {
      task_id: taskId,
      attempt_no: attemptNo,
      seq,
      name: runnerNameOf(seed),
      runner_name: seed.runner,
      runner_version: wire?.version ?? '1.0.0',
      role: seed.role ?? 'test',
      image_ref: wire?.image ?? `registry.dev.huawei.com/argus/${seed.runner}:1.0.0`,
      image_digest: `sha256:${taskId.replace(/[^a-z0-9]/gi, '').slice(0, 12)}d1e7`,
      profile: 'k8s-npu',
      input_ref: `attempts/${attemptNo}/input/`,
      on_failure: wire?.on_failure ?? 'continue',
      timeout_seconds: 14400,
      status,
      status_reason: seed.statusReason ?? null,
      waiting_reason: seed.waiting ?? null,
      endpoint_key: null,
      endpoint_exclusive: wire?.concurrency?.endpoint_exclusive ?? false,
      exit_code: status === 'succeeded' ? 0 : status === 'failed' ? 1 : null,
      error_code: seed.error?.code ?? (seed.errorCode as Failure['code'] | undefined) ?? null,
      error: seed.error ?? null,
      exports_ref: status === 'succeeded' ? `attempts/${attemptNo}/exports/${seq}.exports.env` : null,
      // 产物三选一（D-74）：仅产出 result.json 时指向之；html/csv 或 deploy/clean 为 null
      result_ref: hasResult ? `attempts/${attemptNo}/runners/${seq}/result.json` : null,
      log_ref: `attempts/${attemptNo}/runners/${seq}/logs/stdout.log`,
      started_at: startedAt ? startedAt.toISOString() : null,
      finished_at: finishedAt,
      executor_ref: {
        cluster: 'prod-npu-01',
        namespace: 'argus',
        job: `argus-${taskId}-1-${seq}`,
      },
    } satisfies RunnerExecution
  })
}

function countsOf(runners: RunnerExecution[]): NonNullable<Task['runner_counts']> {
  const counts = { total: runners.length, succeeded: 0, failed: 0, pending: 0, running: 0, skipped: 0, canceled: 0 }
  for (const r of runners) {
    if (r.status === 'succeeded') counts.succeeded += 1
    else if (r.status === 'failed') counts.failed += 1
    else if (r.status === 'skipped') counts.skipped += 1
    else if (r.status === 'canceled') counts.canceled += 1
    else if (['preparing', 'running'].includes(r.status)) counts.running += 1
    else counts.pending += 1
  }
  return counts
}

function isTerminalTask(status: TaskStatus): boolean {
  return ['succeeded', 'failed', 'service_unavailable', 'canceled'].includes(status)
}

function seedTask(seed: TaskSeed): TaskDetail {
  const taskId = seed.id
  const endpoint: Endpoint | null = seed.deploySpec
    ? endpointOf('http://10.0.1.20:8000/v1', 'Qwen3-32B', 'genet', 'arg-mock0001')
    : seed.service
      ? endpointOf(seed.service.url, seed.service.model_name ?? 'Qwen3-32B', 'service')
      : null

  const extraAttempts = seed.extraAttempts ?? 0
  const attemptNo = 1
  const runners = buildRunners(taskId, attemptNo, seed.runners)
  const attempts: Attempt[] = []
  for (let i = 1; i <= extraAttempts; i += 1) {
    attempts.push({
      task_id: taskId,
      attempt_no: i,
      status: 'succeeded',
      trigger: i === 1 ? 'initial' : 'retry',
      input_ref: `attempts/${i}/input/`,
      exports_ref: `attempts/${i}/exports/`,
      deploy_state_ref: seed.deploySpec ? `attempts/${i}/deploy-state.json` : null,
      error: null,
      started_at: iso(seed.submittedMinAgo + 40 * (extraAttempts - i + 1)),
      finished_at: iso(seed.submittedMinAgo + 30 * (extraAttempts - i + 1)),
    })
  }
  attempts.push({
    task_id: taskId,
    attempt_no: attemptNo,
    status: isTerminalTask(seed.status)
      ? seed.status === 'succeeded'
        ? 'succeeded'
        : seed.status === 'canceled'
          ? 'canceled'
          : 'failed'
      : 'running',
    trigger: extraAttempts > 0 ? 'retry' : 'initial',
    input_ref: `attempts/${attemptNo}/input/`,
    exports_ref: `attempts/${attemptNo}/exports/`,
    deploy_state_ref: seed.deploySpec ? `attempts/${attemptNo}/deploy-state.json` : null,
    error: seed.error ?? null,
    started_at: iso(Math.max(0, seed.submittedMinAgo - 0.2)),
    finished_at: isTerminalTask(seed.status) ? iso(Math.max(0, seed.submittedMinAgo - (seed.durationSec ?? 0) / 60)) : null,
  })

  const deploy: DeploySnapshot = {
    task_id: taskId,
    deploy: seed.deploySpec
      ? {
          runner: 'genet-deploy',
          version: '1.0.0',
          input: seed.deploySpec,
          spec_digest: 'sha256:9f2a',
          namespace: 'argus',
          release_name: 'arg-mock0001',
          deploy_state_ref: `attempts/1/deploy-state.json`,
          phase: seed.deployState ?? 'ready',
          genet_status: (seed.deployState ?? 'ready') === 'ready' ? 'deployed' : 'pending-install',
          reason: seed.deployState === 'blocked' ? 'timeout' : null,
          waited_seconds: seed.waitedSeconds ?? null,
          pod_count: seed.pods?.length ?? null,
          label_selector: 'app=genet-qwen3-32b',
          queue:
            seed.deployState === 'queued'
              ? { phase: 'Pending', pending: 6, inqueue: 6, running: 0, position: null }
              : null,
          endpoint,
          queued_at: iso(seed.submittedMinAgo),
          ready_at: seed.deployState === 'ready' ? iso(Math.max(0, seed.submittedMinAgo - 4)) : null,
          cancel_requested_at: null,
          failure: null,
          uninstall:
            isTerminalTask(seed.status) && seed.deploySpec
              ? { state: seed.status === 'canceled' ? 'verified' : 'verified', verified_at: iso(1), cleanup_errors: [] }
              : null,
        }
      : null,
    pods: clone(seed.pods ?? []),
    deploy_timeout_seconds: 259200,
  }
  state.deploys[`${taskId}:${attemptNo}`] = deploy

  const detail: TaskDetail = {
    id: taskId,
    name: seed.name,
    status: seed.status,
    submitted_by: seed.submittedBy,
    submitted_at: iso(seed.submittedMinAgo),
    started_at: iso(Math.max(0, seed.submittedMinAgo - 0.2)),
    finished_at: isTerminalTask(seed.status)
      ? iso(Math.max(0, seed.submittedMinAgo - (seed.durationSec ?? 0) / 60))
      : null,
    endpoint,
    deploy: deploy.deploy,
    env: seed.env ?? {},
    secret_env: Object.entries(seed.secrets ?? {}).map(([name, secret_ref]) => ({ name, secret_ref })),
    error: seed.error ?? null,
    retry_of: null,
    current_attempt: extraAttempts + 1,
    runner_counts: countsOf(runners),
    revision: 3,
    attempts,
    runners,
  }
  state.tasks[taskId] = detail
  state.attemptRunners[`${taskId}:${attemptNo}`] = runners
  // 种子任务的提交原文快照（getTaskManifest / 重试回填用）：从 seed 反推一份合理的 manifest
  state.manifests[taskId] = {
    task: {
      name: seed.name,
      ...(seed.deploySpec ? { deploy: { runner: 'genet-deploy', version: '1.0.0', input: seed.deploySpec } } : {}),
      ...(seed.service ? { service: seed.service } : {}),
      precheck: 'auto',
      env: seed.env ?? {},
      secrets: seed.secrets ?? {},
      runners: seed.runners
        // 注入行（precheck / deploy / clean）不进 runners[]：deploy 由提交体 deploy 块表达，D-70
        .filter((r) => r.runner !== 'endpoint-probe' && r.role !== 'deploy' && r.role !== 'clean')
        .map((r) => ({
          name: r.name,
          runner: r.runner,
          version: '1.0.0',
          execution: { profile: 'k8s-npu' },
          ...(r.input ? { input: r.input } : {}),
        })),
    },
  }
  for (const runner of runners) {
    const seedForLog = seed.runners[runner.seq - 1]
    if (seedForLog) state.logs[`${taskId}:${attemptNo}:${runner.seq}`] = runnerLog(seedForLog, runner.started_at)
    if (seedForLog?.sticky) stickyRunners.add(`${taskId}:${attemptNo}:${runner.seq}`)
  }

  // 事件历史：给首屏一条可读的时间线
  let seqNo = 0
  const push = (
    type: EventType,
    from: string | null,
    to: string | null,
    extra?: Partial<TaskEvent>,
  ) => {
    seqNo += 1
    const events = state.events[taskId] ?? []
    events.push({
      seq: seqNo,
      type,
      task_id: taskId,
      attempt_no: extra?.attempt_no ?? 1,
      runner_seq: extra?.runner_seq ?? null,
      at: iso(Math.max(0, seed.submittedMinAgo - seqNo * 0.3)),
      from,
      to,
      reason: extra?.reason ?? null,
      payload: extra?.payload ?? null,
    })
    state.events[taskId] = events
  }
  push('submitted', null, 'submitted', { payload: { runner_count: runners.length } })
  for (const runner of runners) {
    if (runner.status === 'pending' || runner.status === 'skipped') continue
    if (runner.status === 'canceled') {
      push('runner_finished', 'running', 'canceled', { runner_seq: runner.seq, payload: { status: 'canceled' } })
      continue
    }
    if (runner.started_at) push('runner_started', null, 'running', { runner_seq: runner.seq })
    if (runner.finished_at) {
      push('runner_finished', 'running', runner.status, {
        runner_seq: runner.seq,
        payload: { status: runner.status, exit_code: runner.exit_code, error_code: runner.error_code },
      })
      if (runner.status === 'succeeded') push('exports_harvested', null, null, { runner_seq: runner.seq })
    }
  }
  if (isTerminalTask(seed.status)) {
    push('clean_finished', 'running', 'succeeded')
    push('task_finished', 'running', seed.status, { payload: { status: seed.status } })
    push('report.written', null, null)
  }
  state.seq[taskId] = seqNo

  return detail
}

const PODS_SINGLE: DeployPod[] = [
  { name: 'genet-qwen3-32b-0', role: 'main', node: 'npu-01', restarts: 0, ready: true, started_at: iso(30), last_seen_at: iso(0) },
]

const PODS_PD: DeployPod[] = [
  { name: 'genet-qwen3-32b-router-0', role: 'router', node: 'npu-01', restarts: 0, ready: true, started_at: iso(30), last_seen_at: iso(0) },
  { name: 'genet-qwen3-32b-prefill-0', role: 'prefill', node: 'npu-01', restarts: 0, ready: true, started_at: iso(30), last_seen_at: iso(0) },
  { name: 'genet-qwen3-32b-decode-0', role: 'decode', node: 'npu-02', restarts: 1, ready: true, started_at: iso(30), last_seen_at: iso(0) },
  { name: 'genet-qwen3-32b-decode-1', role: 'decode', node: 'npu-03', restarts: 0, ready: true, started_at: iso(30), last_seen_at: iso(0) },
]

function seedAll(): void {
  state.clusters = [
    {
      name: 'prod-npu-01',
      api_server: 'https://10.0.0.2:6443',
      description: '生产 NPU 集群（Volcano 调度）',
      last_check_at: iso(2),
      last_check_result: 'ok',
    },
    {
      name: 'prod-gpu-02',
      api_server: 'https://10.0.0.12:6443',
      description: 'GPU 共享集群',
      last_check_at: iso(5),
      last_check_result: 'ok',
    },
  ]

  // /admin/* 的种子（P1）：Profile 必须有 k8s-npu，提交页与契约都依赖它存在
  state.profiles = [
    {
      name: 'k8s-npu',
      cluster: 'prod-npu-01',
      namespace: 'argus',
      mounts: [{ name: 'dataset', pvc: 'argus-dataset', sub_path: null, kind: 'dataset' }],
      scheduler: 'volcano',
      queue: 'npu-batch',
      image_pull_secrets: ['registry-cred'],
      resource_defaults: { cpu: '4', memory: '16Gi' },
      deploy_timeout_seconds: 259200,
    },
    {
      name: 'k8s-gpu',
      cluster: 'prod-gpu-02',
      namespace: 'argus',
      mounts: [],
      scheduler: 'default',
      queue: null,
      image_pull_secrets: [],
      resource_defaults: { cpu: '2', memory: '8Gi' },
      deploy_timeout_seconds: 86400,
    },
  ]

  state.adminClusters = state.clusters.map((cluster) => ({
    name: cluster.name,
    api_server: cluster.api_server,
    kubeconfig_key: `${cluster.name}.kubeconfig`,
    namespace: 'argus',
    description: cluster.description ?? null,
    last_check_at: cluster.last_check_at ?? null,
    last_check_result: cluster.last_check_result ?? null,
  }))

  state.secrets = [
    {
      name: 'endpoint/prod-qwen',
      namespace: 'argus',
      secret_name: 'endpoint-prod-qwen',
      key: 'api-key',
      description: '生产 Qwen 接入 token',
    },
    {
      name: 'registry/pull-secret',
      namespace: 'argus',
      secret_name: 'registry-cred',
      key: '.dockerconfigjson',
      description: '镜像拉取凭据',
    },
  ]

  state.audit = [
    {
      id: 'a-0001',
      at: iso(180),
      actor: '林知微',
      role: 'operator',
      action: 'task.submit',
      object_type: 'task',
      object_id: 't-20261105-0006',
      detail: { runners: 6 },
    },
    {
      id: 'a-0002',
      at: iso(120),
      actor: 'root',
      role: 'admin',
      action: 'profile.update',
      object_type: 'profile',
      object_id: 'k8s-npu',
      detail: { deploy_timeout_seconds: 259200 },
    },
    {
      id: 'a-0003',
      at: iso(60),
      actor: '陈一鸣',
      role: 'operator',
      action: 'task.cancel',
      object_type: 'task',
      object_id: 't-20261105-0001',
      detail: null,
    },
  ]

  const spec = 'model: Qwen3-32B\nreplicas: 1\nresources:\n  gpu: 8'

  // 1) 成功：runner 管道全绿 + PD 分离 4 Pod
  seedTask({
    id: 't-20261105-0006',
    name: 'qwen3-32b-daily',
    status: 'succeeded',
    submittedBy: '林知微',
    submittedMinAgo: 96,
    durationSec: 1398,
    deploySpec: spec,
    deployState: 'ready',
    pods: PODS_PD,
    env: { LOG_LEVEL: 'info', HTTP_TIMEOUT: '300' },
    secrets: { ENDPOINT_API_KEY: 'endpoint/prod-qwen' },
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'succeeded', startedMinAgo: 95, durationSec: 62 },
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'succeeded', startedMinAgo: 92, durationSec: 41 },
      { name: 'accuracy-dataset-a', runner: 'aisbench-acc', status: 'succeeded', input: '{"dataset":"/data/gsm8k.jsonl","limit":200}', startedMinAgo: 91, durationSec: 484 },
      { name: 'accuracy-dataset-b', runner: 'aisbench-acc', status: 'succeeded', input: '{"dataset":"/data/mmlu.jsonl","limit":200}', startedMinAgo: 83, durationSec: 411 },
      { name: 'performance-sweep', runner: 'aisbench-perf', status: 'succeeded', input: '{"input_len":512,"output_len":128,"concurrency":8}', startedMinAgo: 75, durationSec: 432 },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'succeeded', startedMinAgo: 74, durationSec: 18 },
    ],
  })

  // 2) Deploy 排队中（deploy-state.json phase=queued；D-46：只有计数没有位次，展示 waited_seconds）
  seedTask({
    id: 't-20261105-0005',
    name: 'qwen3-32b-canary',
    status: 'running',
    submittedBy: '陈一鸣',
    submittedMinAgo: 18,
    deploySpec: spec,
    deployState: 'queued',
    waitedSeconds: 1080,
    env: { LOG_LEVEL: 'info' },
    pods: [],
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'running', startedMinAgo: 17 },
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'pending' },
      { name: 'smoke-acc', runner: 'aisbench-acc', status: 'pending', input: '{"dataset":"/data/gsm8k.jsonl","limit":20}' },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'pending' },
    ],
  })

  // 3) 执行中（外接模式，live：mock 会推事件把 runner 一个个跑完）
  seedTask({
    id: 't-20261105-0004',
    name: 'existing-service-smoke',
    status: 'running',
    submittedBy: '林知微',
    submittedMinAgo: 4,
    service: { url: 'http://10.0.0.30:8000/v1', model_name: 'Qwen3-32B' },
    runners: [
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'succeeded', startedMinAgo: 4, durationSec: 5 },
      { name: 'smoke-acc', runner: 'aisbench-acc', status: 'running', input: '{"dataset":"/data/gsm8k.jsonl","limit":200}', startedMinAgo: 3 },
      { name: 'perf-sweep', runner: 'aisbench-perf', status: 'queued', waiting: 'slot_waiting' },
    ],
  })

  // 3b) 运行中 + 长日志：runner 与 Deploy 日志都会在轮询时"长出新行"（日志面板回归用）
  seedTask({
    id: 't-20261105-0007',
    name: 'live-log-stream',
    status: 'running',
    submittedBy: '林知微',
    submittedMinAgo: 1,
    deploySpec: spec,
    deployState: 'ready',
    pods: PODS_SINGLE,
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'succeeded', startedMinAgo: 2, durationSec: 40 },
      {
        name: 'stream-watch',
        runner: 'aisbench-acc',
        status: 'running',
        startedMinAgo: 1,
        // 状态机不推进这个 runner：Task 保持 running，日志轮询能一直演示下去
        sticky: true,
      },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'pending' },
    ],
  })
  // sticky runner 的日志种子：含平台标记行（UI 渲染成分隔条）
  state.logs['t-20261105-0007:1:2'] = [
    ...Array.from(
      { length: 40 },
      (_, i) =>
        `[00:00:${String(i % 60).padStart(2, '0')}] [stream] 第 ${i + 1} 行：input_len=${256 + i * 32} tokens/s=${1200 + i * 7}`,
    ),
    '--- [container restarted #3 at 2026-11-05T06:44:11Z, previous exit: OOMKilled] ---',
    '--- [log gap: 2026-11-05T06:44:12Z – 2026-11-05T06:46:30Z, collector restarted] ---',
  ].join('\n')

  // 4) failed（v2：completed_with_errors 已并入 failed，D-73）
  seedTask({
    id: 't-20261105-0003',
    name: 'qwen3-32b-daily',
    status: 'failed',
    submittedBy: '陈一鸣',
    submittedMinAgo: 240,
    durationSec: 1082,
    deploySpec: spec,
    deployState: 'ready',
    pods: PODS_SINGLE,
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'succeeded', startedMinAgo: 239, durationSec: 58 },
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'succeeded', startedMinAgo: 238, durationSec: 12 },
      {
        name: 'accuracy-dataset-a',
        runner: 'aisbench-acc',
        status: 'failed',
        input: '{"dataset":"/data/gsm8k.jsonl","limit":200}',
        startedMinAgo: 236,
        durationSec: 460,
        error: { code: 'RUNNER_NONZERO_EXIT', message: 'Runner 退出码 1（连接被对端重置）', retryable: true, detail: { exit_code: 1 } },
      },
      { name: 'accuracy-dataset-b', runner: 'aisbench-acc', status: 'succeeded', input: '{"dataset":"/data/mmlu.jsonl","limit":200}', startedMinAgo: 228, durationSec: 402 },
      { name: 'performance-sweep', runner: 'aisbench-perf', status: 'skipped', statusReason: 'upstream_abort' },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'succeeded', startedMinAgo: 221, durationSec: 15 },
    ],
  })

  // 5) 服务不可用（pre_check 失败）
  seedTask({
    id: 't-20261105-0002',
    name: 'qwen3-32b-canary',
    status: 'service_unavailable',
    submittedBy: '林知微',
    submittedMinAgo: 300,
    durationSec: 18060,
    deploySpec: spec,
    deployState: 'ready',
    pods: PODS_SINGLE,
    error: {
      code: 'PRECHECK_FAILED',
      message: 'pre_check 在 18000 秒窗口内未通过：服务持续返回 503',
      retryable: true,
      detail: { http_status: 503 },
    },
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'succeeded', startedMinAgo: 299, durationSec: 55 },
      {
        name: 'precheck',
        runner: 'endpoint-probe',
        role: 'pre_check',
        status: 'failed',
        startedMinAgo: 305,
        durationSec: 18000,
        error: { code: 'PRECHECK_FAILED', message: '等待窗口内服务始终未就绪', retryable: true, detail: { http_status: 503 } },
      },
      { name: 'accuracy-dataset-a', runner: 'aisbench-acc', status: 'skipped', statusReason: 'upstream_abort' },
      { name: 'performance-sweep', runner: 'aisbench-perf', status: 'skipped', statusReason: 'upstream_abort' },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'succeeded', startedMinAgo: 5, durationSec: 14 },
    ],
  })

  // 6) 已取消（取消后 clean 收口，clean 完才落 canceled）
  seedTask({
    id: 't-20261105-0001',
    name: 'qwen3-32b-acc-200',
    status: 'canceled',
    submittedBy: '陈一鸣',
    submittedMinAgo: 420,
    durationSec: 753,
    service: { url: 'http://10.0.0.11:8000/v1', model_name: 'Qwen3-32B' },
    runners: [
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'succeeded', startedMinAgo: 419, durationSec: 4 },
      { name: 'accuracy-dataset-a', runner: 'aisbench-acc', status: 'canceled', input: '{"dataset":"/data/gsm8k.jsonl","limit":200}', startedMinAgo: 418, durationSec: 740 },
      { name: 'performance-sweep', runner: 'aisbench-perf', status: 'skipped', statusReason: 'canceled' },
    ],
  })

  // 7) 重试产生的第二个 attempt（历史 attempt 略化为 succeeded）
  seedTask({
    id: 't-20261105-0008',
    name: 'qwen3-32b-nightly',
    status: 'running',
    submittedBy: '林知微',
    submittedMinAgo: 12,
    deploySpec: spec,
    deployState: 'deploying',
    waitedSeconds: 240,
    pods: PODS_SINGLE,
    extraAttempts: 1,
    runners: [
      { name: 'service-deploy', runner: 'genet-internal', role: 'deploy', status: 'running', startedMinAgo: 11 },
      { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'pending' },
      { name: 'accuracy-dataset-a', runner: 'aisbench-acc', status: 'pending', input: '{"dataset":"/data/gsm8k.jsonl","limit":100}' },
      { name: 'service-clean', runner: 'genet-internal', role: 'clean', status: 'pending' },
    ],
  })

  // 8~11) 更早的成功记录（凑分页）
  const older: Array<[string, string, string, number]> = [
    ['t-20261104-0014', 'qwen3-8b-nightly', '林知微', 1400],
    ['t-20261104-0013', 'qwen3-32b-daily', '陈一鸣', 1500],
    ['t-20261104-0012', 'evalscope-mmlu', '林知微', 1620],
    ['t-20261104-0011', 'existing-service-smoke', 'dev', 1740],
  ]
  for (const [id, name, by, minAgo] of older) {
    seedTask({
      id,
      name,
      status: 'succeeded',
      submittedBy: by,
      submittedMinAgo: minAgo,
      durationSec: 900 + ((minAgo * 7) % 600),
      service: { url: 'http://10.0.0.20:8000/v1', model_name: 'Qwen3-32B' },
      runners: [
        { name: 'precheck', runner: 'endpoint-probe', role: 'pre_check', status: 'succeeded', startedMinAgo: minAgo - 1, durationSec: 3 },
        { name: 'accuracy-dataset-a', runner: 'aisbench-acc', status: 'succeeded', input: '{"dataset":"/data/gsm8k.jsonl","limit":200}', startedMinAgo: minAgo - 2, durationSec: 400 },
      ],
    })
  }
}

seedAll()

/* ================================================================ 事件推送 */

const listeners = new Map<string, Set<(e: TaskEvent) => void>>()

function emit(taskId: string, event: TaskEvent): void {
  for (const listener of listeners.get(taskId) ?? []) listener(event)
}

function pushEvent(
  taskId: string,
  type: EventType,
  from: string | null,
  to: string | null,
  extra?: Partial<TaskEvent>,
): TaskEvent {
  const seq = (state.seq[taskId] ?? 0) + 1
  state.seq[taskId] = seq
  const event: TaskEvent = {
    seq,
    type,
    task_id: taskId,
    attempt_no: extra?.attempt_no ?? null,
    runner_seq: extra?.runner_seq ?? null,
    at: nowIso(),
    from,
    to,
    reason: extra?.reason ?? null,
    payload: extra?.payload ?? null,
  }
  const events = state.events[taskId] ?? []
  events.push(event)
  state.events[taskId] = events
  emit(taskId, event)
  return event
}

/** 写操作的审计记录（mock 的 /admin/audit 数据源）。 */
function auditLog(
  action: string,
  objectType: string,
  objectId: string,
  detail?: Record<string, unknown>,
): void {
  const auth = getAuthState()
  state.audit = [
    ...state.audit,
    {
      id: `a-${String(state.audit.length + 1).padStart(4, '0')}`,
      at: nowIso(),
      actor: auth.user ?? getCurrentUser(),
      role: auth.role ?? 'operator',
      action,
      object_type: objectType,
      object_id: objectId,
      detail: detail ?? null,
    },
  ]
}

/** 造一段 Deploy 日志（多 Pod、含重启前后的断点素材）。 */
function deployLogSeed(podName: string, taskName: string): string[] {
  const lines: string[] = []
  const t = (m: number) => new Date(Date.now() - (30 - m) * 60_000).toLocaleTimeString('zh-CN', { hour12: false })
  lines.push(`${t(0)} [genet] deploy accepted for ${taskName}, creating pod ${podName}`)
  lines.push(`${t(1)} [kubelet] Successfully pulled image "registry.dev.huawei.com/argus/serving:1.0.0"`)
  lines.push(`${t(2)} [main] container started, loading model weights from /models/Qwen3-32B`)
  lines.push(`${t(3)} [main] weights loaded in 42.3s, initializing KV cache (blocks=2048)`)
  lines.push(`${t(4)} [main] WARNING: NPU memory usage 82% — consider reducing max_model_len`)
  lines.push(`${t(5)} [main] server listening on 0.0.0.0:8000 (engine=vllm-ascend)`)
  lines.push(`${t(6)} [main] health endpoint /health ready, endpoint registered`)
  lines.push(`${t(7)} [main] serving request batch size=32, throughput=1280 tok/s`)
  lines.push(`${t(8)} [main] ERROR: transient HCCL timeout, retrying (attempt 1/3)`)
  lines.push(`${t(9)} [main] recovered, HCCL group re-established in 1.2s`)
  // 采集器写的标记行（与后端同一格式；UI 按分隔样式渲染，内容不改）
  lines.push('--- [container restarted #3 at 2026-11-05T06:44:11Z, previous exit: OOMKilled] ---')
  lines.push('--- [log gap: 2026-11-05T06:44:12Z – 2026-11-05T06:46:30Z, collector restarted] ---')
  return lines
}

const RUNNER_FLOW: RunnerStatus[] = ['pending', 'queued', 'preparing', 'running', 'succeeded']
const DEPLOY_PHASE_FLOW: Array<DeploySnapshot['deploy'] extends null ? never : NonNullable<DeploySnapshot['deploy']>['phase']> = [
  'queued',
  'accepted',
  'deploying',
  'ready',
]
type DeployPhase = (typeof DEPLOY_PHASE_FLOW)[number]

/** 当前 attempt 里第一个未终态的 runner（严格串行管道）。 */
function currentRunner(taskId: string, attemptNo: number): RunnerExecution | null {
  const runners = state.attemptRunners[`${taskId}:${attemptNo}`] ?? []
  return (
    runners.find((r) => !['succeeded', 'failed', 'skipped', 'canceled'].includes(r.status)) ?? null
  )
}

/** 推进一档 mock 状态（每 3.5s 一次，直到 Task 终态）。 */
function step(taskId: string): void {
  const task = state.tasks[taskId]
  if (!task || isTerminalTask(task.status)) {
    stopLive(taskId)
    return
  }
  const attemptNo = task.current_attempt
  const runners = state.attemptRunners[`${taskId}:${attemptNo}`] ?? []

  // "长时间运行"的演示 runner：状态机不推进它；Task 因此保持非终态，日志轮询才有增量可拉
  const hasSticky = runners.some(
    (r) => stickyRunners.has(`${taskId}:${attemptNo}:${r.seq}`) && r.status === 'running',
  )
  if (hasSticky) return

  // 取消收口（v2：先终止当前 runner → pending/queued 标 skipped → clean finally → canceled）
  if (task.status === 'cancelling') {
    for (const runner of runners) {
      if (['pending', 'queued'].includes(runner.status)) {
        runner.status = 'skipped'
        runner.status_reason = 'canceled'
      } else if (runner.status === 'running' || runner.status === 'preparing') {
        runner.status = 'canceled'
        runner.finished_at = nowIso()
        pushEvent(taskId, 'runner_finished', 'running', 'canceled', {
          attempt_no: attemptNo,
          runner_seq: runner.seq,
          payload: { status: 'canceled' },
        })
      }
    }
    if (task.deploy) {
      task.deploy.uninstall = { state: 'verified', verified_at: nowIso(), cleanup_errors: [] }
      pushEvent(taskId, 'clean_finished', null, 'verified', { attempt_no: attemptNo })
    }
    task.status = 'canceled'
    task.finished_at = nowIso()
    task.runner_counts = countsOf(runners)
    const attempt = task.attempts.find((a) => a.attempt_no === attemptNo)
    if (attempt) {
      attempt.status = 'canceled'
      attempt.finished_at = nowIso()
    }
    pushEvent(taskId, 'task_finished', 'cancelling', 'canceled', { attempt_no: attemptNo, payload: { status: 'canceled' } })
    stopLive(taskId)
    return
  }

  const runner = currentRunner(taskId, attemptNo)
  if (!runner) {
    // 所有 runner 收口 → 落终态（没有 failed 就 succeeded）
    const failed = runners.some((r) => r.status === 'failed')
    task.status = failed ? 'failed' : 'succeeded'
    task.finished_at = nowIso()
    task.runner_counts = countsOf(runners)
    const attempt = task.attempts.find((a) => a.attempt_no === attemptNo)
    if (attempt) {
      attempt.status = failed ? 'failed' : 'succeeded'
      attempt.finished_at = nowIso()
    }
    pushEvent(taskId, 'task_finished', 'running', task.status, { attempt_no: attemptNo, payload: { status: task.status } })
    pushEvent(taskId, 'report.written', null, null, { attempt_no: attemptNo })
    stopLive(taskId)
    return
  }

  // deploy runner 运行中：推进 deploy-state 的 phase（queued → accepted → deploying → ready）
  if (runner.role === 'deploy' && runner.status === 'running' && task.deploy) {
    const phase = task.deploy.phase ?? 'queued'
    const index = DEPLOY_PHASE_FLOW.indexOf(phase as DeployPhase)
    if (index >= 0 && index < DEPLOY_PHASE_FLOW.length - 1) {
      const next = DEPLOY_PHASE_FLOW[index + 1] ?? 'ready'
      task.deploy.phase = next
      task.deploy.genet_status = next === 'ready' ? 'deployed' : 'pending-install'
      task.deploy.waited_seconds = (task.deploy.waited_seconds ?? 0) + 90
      if (next === 'ready' && task.deploy.endpoint == null) {
        task.deploy.endpoint = endpointOf('http://10.0.1.20:8000/v1', 'Qwen3-32B', 'genet', task.deploy.release_name ?? undefined)
        task.endpoint = task.deploy.endpoint
        const snapshot = state.deploys[`${taskId}:${attemptNo}`]
        if (snapshot && snapshot.pods.length === 0) snapshot.pods = PODS_SINGLE.map((pod) => ({ ...pod, name: `genet-${task.name}-0` }))
        pushEvent(taskId, 'exports_harvested', null, null, { attempt_no: attemptNo, runner_seq: runner.seq })
      }
      return
    }
  }

  const index = RUNNER_FLOW.indexOf(runner.status as RunnerStatus)
  const to = RUNNER_FLOW[Math.min(index + 1, RUNNER_FLOW.length - 1)] ?? 'running'
  runner.status = to
  if (to === 'queued') runner.waiting_reason = null
  if (to === 'running') {
    runner.started_at = nowIso()
    if (task.status === 'queued' || task.status === 'submitted') {
      task.status = 'running'
      task.started_at = nowIso()
      pushEvent(taskId, 'state_changed', 'queued', 'running', { attempt_no: attemptNo })
    }
    pushEvent(taskId, 'runner_started', null, 'running', { attempt_no: attemptNo, runner_seq: runner.seq })
    return
  }
  if (to === 'succeeded') {
    runner.finished_at = nowIso()
    runner.exit_code = 0
    state.logs[`${taskId}:${attemptNo}:${runner.seq}`] =
      `${state.logs[`${taskId}:${attemptNo}:${runner.seq}`] ?? ''}\n结论：status=succeeded`
    pushEvent(taskId, 'runner_finished', 'running', 'succeeded', {
      attempt_no: attemptNo,
      runner_seq: runner.seq,
      payload: { status: 'succeeded', exit_code: 0 },
    })
    pushEvent(taskId, 'exports_harvested', null, null, { attempt_no: attemptNo, runner_seq: runner.seq })
    if (runner.role === 'clean') pushEvent(taskId, 'clean_finished', 'running', 'succeeded', { attempt_no: attemptNo })
    task.runner_counts = countsOf(runners)
    return
  }
  task.runner_counts = countsOf(runners)
}

function startLive(taskId: string): void {
  if (state.timers.has(taskId)) return
  const task = state.tasks[taskId]
  if (!task || isTerminalTask(task.status)) return
  state.timers.set(
    taskId,
    setInterval(() => step(taskId), 3500),
  )
}

function stopLive(taskId: string): void {
  const timer = state.timers.get(taskId)
  if (timer !== undefined) {
    clearInterval(timer)
    state.timers.delete(taskId)
  }
}

/** mock 版事件流：先回放历史，再按状态机持续推送。返回取消函数。 */
export function subscribeMockEvents(
  taskId: string,
  opts: {
    sinceSeq?: number
    onEvent: (event: TaskEvent) => void
    onOpen?: () => void
    onError?: (error: unknown) => void
  },
): () => void {
  const task = state.tasks[taskId]
  if (!task) {
    opts.onError?.(new ApiClientError(404, { code: 'NOT_FOUND', message: 'Task 不存在', retryable: false }))
    return () => {}
  }

  const bucket = listeners.get(taskId) ?? new Set()
  listeners.set(taskId, bucket)
  bucket.add(opts.onEvent)

  const since = opts.sinceSeq ?? 0
  setTimeout(() => {
    opts.onOpen?.()
    for (const event of state.events[taskId] ?? []) {
      if (event.seq > since) opts.onEvent(event)
    }
    startLive(taskId)
  }, 60)

  return () => {
    bucket.delete(opts.onEvent)
    if (bucket.size === 0) {
      listeners.delete(taskId)
      stopLive(taskId)
    }
  }
}

/* ================================================================ Api 实现 */

function paginate<T>(items: T[], cursor: string | null | undefined, limit: number): Page<T> {
  const start = cursor ? Number.parseInt(cursor, 10) || 0 : 0
  const slice = items.slice(start, start + limit)
  const nextIndex = start + slice.length
  const hasMore = nextIndex < items.length
  return { items: slice, next_cursor: hasMore ? String(nextIndex) : null, has_more: hasMore }
}

/**
 * 列表与 facets 共用的过滤：多值用逗号，空值即不过滤。
 * 排序固定为提交时间倒序（07 §6「排序固定」）。
 */
function filterTasks(query: TaskListQuery): TaskDetail[] {
  const statuses = query.status?.split(',').filter(Boolean) ?? []
  const submitters = query.submitter?.split(',').filter(Boolean) ?? []
  const runnerNames = query.runner?.split(',').filter(Boolean) ?? []

  return Object.values(state.tasks)
    .filter((t) => (statuses.length === 0 ? true : statuses.includes(t.status)))
    .filter((t) => (submitters.length === 0 ? true : submitters.includes(t.submitted_by)))
    // Runner 过滤：服务端按 runner 执行记录的 runner_name 筛 Task
    .filter((t) =>
      runnerNames.length === 0
        ? true
        : (state.attemptRunners[`${t.id}:${t.current_attempt}`] ?? []).some((r) =>
            runnerNames.includes(r.runner_name),
          ),
    )
    .slice()
    .sort((a, b) => b.submitted_at.localeCompare(a.submitted_at))
}

function summarizeTask(task: TaskDetail): Task {
  const { attempts: _attempts, runners: _runners, ...rest } = task
  void _attempts
  void _runners
  return rest
}

function findTask(id: string): TaskDetail {
  const task = state.tasks[id]
  if (!task) notFound(`Task ${id}`)
  return task
}

function taskAttemptOf(task: TaskDetail, attempt?: number): number {
  const n = attempt ?? task.current_attempt
  if (!task.attempts.some((a) => a.attempt_no === n)) notFound(`Attempt #${n}（Task ${task.id}）`)
  return n
}

function runnersOf(task: TaskDetail, attempt?: number): RunnerExecution[] {
  const n = taskAttemptOf(task, attempt)
  return state.attemptRunners[`${task.id}:${n}`] ?? []
}

function findRunner(task: TaskDetail, seq: number, attempt?: number): RunnerExecution {
  const runners = runnersOf(task, attempt)
  const runner = runners.find((r) => r.seq === seq)
  if (!runner) notFound(`runner seq=${seq}（Task ${task.id}）`)
  return runner
}

let taskCounter = 20
function nextTaskId(): string {
  taskCounter += 1
  const d = new Date()
  const ymd = `${d.getFullYear()}${String(d.getMonth() + 1).padStart(2, '0')}${String(d.getDate()).padStart(2, '0')}`
  return `t-${ymd}-${String(taskCounter).padStart(4, '0')}`
}

/**
 * Manifest 校验（C-5.1 的 mock 子集）：形态互斥（deploy XOR service，D-27）、name 必填（D-58）、
 * runner 数 1..200（D-57/§12.7）、Runner/Profile 命中（RUNNER_NOT_FOUND / EXECUTION_PROFILE_NOT_FOUND）。
 */
function validateManifest(manifest: TaskManifest): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const body = manifest.task
  if (!body) {
    issues.push({ path: 'task', code: 'VALIDATION_ERROR', message: '缺少 task 段' })
    return issues
  }
  if (!body.name?.trim()) issues.push({ path: 'task.name', code: 'VALIDATION_ERROR', message: 'Task 名称不能为空' })
  if (body.deploy && body.service) {
    issues.push({ path: 'task', code: 'VALIDATION_ERROR', message: 'deploy 与 service 二选一，不能都给' })
  }
  if (!body.deploy && !body.service) {
    issues.push({ path: 'task', code: 'VALIDATION_ERROR', message: 'deploy 与 service 必须给其一' })
  }
  if (body.deploy) {
    // deploy 块 = runner 引用（与 runners[] 同构；clean 配对由 runner.yaml clean_runner 声明）
    if (!body.deploy.runner?.trim()) {
      issues.push({ path: 'task.deploy.runner', code: 'VALIDATION_ERROR', message: 'deploy.runner 必填（runner 引用与 runners[] 同构）' })
    }
    if (!body.deploy.version?.trim()) {
      issues.push({ path: 'task.deploy.version', code: 'VALIDATION_ERROR', message: 'deploy.version 必填' })
    }
    const deployRunner = body.deploy.runner
      ? state.runners.find((r) => r.name === body.deploy!.runner && !r.retired)
      : undefined
    if (body.deploy.runner?.trim() && (!deployRunner || deployRunner.role !== 'deploy')) {
      issues.push({
        path: 'task.deploy.runner',
        code: 'RUNNER_NOT_FOUND',
        message: `目录里没有 role=deploy 的 Runner「${body.deploy.runner}」`,
      })
    } else if (deployRunner && body.deploy.version?.trim() && body.deploy.version !== deployRunner.version) {
      issues.push({
        path: 'task.deploy.version',
        code: 'RUNNER_VERSION_PINNED',
        message: `Runner ${deployRunner.name} 只有版本 ${deployRunner.version}`,
      })
    }
  }
  if (body.service && !body.service.url?.trim()) {
    issues.push({ path: 'task.service.url', code: 'VALIDATION_ERROR', message: 'service.url 必填' })
  }
  if ((body.runners?.length ?? 0) > 200) {
    issues.push({ path: 'task.runners', code: 'TASK_TOO_LARGE', message: '单个 Task 最多 200 个 runner（§12.7）' })
  }
  body.runners?.forEach((spec, index) => {
    if (!spec.name?.trim()) {
      issues.push({ path: `task.runners[${index}].name`, code: 'VALIDATION_ERROR', message: 'runner 名称必填' })
    }
    if (!spec.runner) {
      issues.push({ path: `task.runners[${index}].runner`, code: 'VALIDATION_ERROR', message: '必须选择 Runner' })
      return
    }
    const runner = state.runners.find((r) => r.name === spec.runner && !r.retired)
    if (!runner) {
      issues.push({ path: `task.runners[${index}].runner`, code: 'RUNNER_NOT_FOUND', message: `目录里没有 Runner「${spec.runner}」` })
    } else if (spec.version && spec.version !== runner.version) {
      issues.push({
        path: `task.runners[${index}].version`,
        code: 'RUNNER_VERSION_PINNED',
        message: `Runner ${spec.runner} 只有版本 ${runner.version}`,
      })
    }
    if (!spec.execution?.profile) {
      issues.push({
        path: `task.runners[${index}].execution.profile`,
        code: 'EXECUTION_PROFILE_NOT_FOUND',
        message: '必须指定 execution.profile',
      })
    }
  })
  return issues
}

/** 提交体 → 最终执行序（service-deploy? → precheck? → tests → service-clean?，D-70）。 */
function resolveRunnerSeeds(manifest: TaskManifest): RunnerSeed[] {
  const body = manifest.task
  const seeds: RunnerSeed[] = []
  const deployRunner = body.deploy?.runner
    ? state.runners.find((r) => r.name === body.deploy!.runner && !r.retired)
    : undefined
  if (body.deploy) {
    // deploy 行 = 提交体引用的 runner 本尊；clean 行按该 runner 的 clean_runner 声明注入
    seeds.push({
      name: body.deploy.runner,
      runner: body.deploy.runner,
      role: 'deploy',
      status: 'pending',
      input: body.deploy.input,
    })
  }
  const precheck = body.precheck ?? 'auto'
  if (precheck !== 'off') {
    const runner = precheck === 'auto' ? (state.runners.find((r) => r.role === 'pre_check')?.name ?? 'endpoint-probe') : precheck.runner
    seeds.push({
      name: INJECTED.precheck,
      runner,
      role: 'pre_check',
      status: 'pending',
      input: precheck === 'auto' ? undefined : precheck.input,
    })
  }
  for (const spec of body.runners ?? []) {
    seeds.push({ name: spec.name, runner: spec.runner, status: 'pending', input: spec.input })
  }
  if (body.deploy && deployRunner?.clean_runner) {
    seeds.push({ name: deployRunner.clean_runner, runner: deployRunner.clean_runner, role: 'clean', status: 'pending' })
  }
  return seeds
}

/* ---------------------------------------------------------------- 测试钩子 */

/**
 * mock 的角色注入（只用于本地验收 / Playwright）：
 * `localStorage['argus:mock-role'] = 'viewer' | 'operator' | 'admin'`，或 URL 上 `?mock_role=viewer`。
 * 注入后会带一个假的 `csrf_token`，让前端按 OIDC 会话处理（viewer 的写按钮才会置灰）。
 * 旧键 `testplane:mock-role` 保留兼容（历史脚本）。
 */
function mockRoleOverride(): string | null {
  const pick = (value: string | null): string | null =>
    value === 'viewer' || value === 'operator' || value === 'admin' ? value : null
  try {
    const stored = pick(localStorage.getItem('argus:mock-role') ?? localStorage.getItem('testplane:mock-role'))
    if (stored) return stored
  } catch {
    /* 隐私模式忽略 */
  }
  try {
    return pick(new URLSearchParams(window.location.search).get('mock_role'))
  } catch {
    /* ignore */
  }
  return null
}

export const mockApi: Api = {
  async getIdentity(): Promise<Identity> {
    await delay(80)
    // 默认 dev 模式（无 csrf_token → 不跳登录）。
    const override = mockRoleOverride()
    if (override) {
      const identity: Identity = {
        user: `mock-${override}`,
        role: override,
        csrf_token: 'mock-csrf-token',
        groups: [`argus-${override}`],
      }
      applyIdentity(identity)
      return identity
    }
    const identity: Identity = { user: getCurrentUser(), role: 'operator' }
    applyIdentity(identity)
    return identity
  },

  async logout(): Promise<void> {
    await delay(60)
    clearAuth()
  },

  async getMeta(): Promise<Meta> {
    await delay(40)
    return {
      version: 'v0.0.0-mock',
      api_version: 'v1',
      features: {
        dry_run: true,
        events_history: true,
        report_md: true,
        deploy_state: true,
        queue_status: true,
      },
    }
  },

  async listTasks(query: TaskListQuery = {}): Promise<Page<Task>> {
    await delay()
    const filtered = filterTasks(query).map(summarizeTask)

    // offset 分页（UI）：回 total / page / page_size，next_cursor 恒为 null（D-61）
    if (query.page != null) {
      const pageSize = query.page_size ?? 20
      const page = Math.max(1, query.page)
      const start = (page - 1) * pageSize
      const slice = filtered.slice(start, start + pageSize)
      return {
        items: slice,
        next_cursor: null,
        has_more: start + slice.length < filtered.length,
        total: filtered.length,
        page,
        page_size: pageSize,
      }
    }

    return paginate(filtered, query.cursor, query.limit ?? 50)
  },

  /**
   * 表头列筛选的候选值（D-61 / 07 §5.1）。v2 维度：status / submitter / runner（kind 已取消）。
   * facet 语义：算某一维度的计数时，只应用其它维度的过滤，不应用自己那维。
   */
  async listTaskFacets(query: TaskFacetsQuery = {}): Promise<TaskFacets> {
    await delay(60)
    const count = (
      dimension: keyof TaskFacetsQuery,
      keyOf: (task: TaskDetail) => string[],
    ): FacetBucket[] => {
      const others: TaskFacetsQuery = { ...query }
      delete others[dimension]
      const buckets = new Map<string, number>()
      for (const task of filterTasks(others)) {
        for (const value of keyOf(task)) buckets.set(value, (buckets.get(value) ?? 0) + 1)
      }
      return [...buckets.entries()]
        .map(([value, n]) => ({ value, count: n }))
        .sort((a, b) => b.count - a.count || a.value.localeCompare(b.value))
    }

    return {
      status: count('status', (task) => [task.status]),
      submitter: count('submitter', (task) => [task.submitted_by]),
      runner: count('runner', (task) =>
        [...new Set((state.attemptRunners[`${task.id}:${task.current_attempt}`] ?? []).map((r) => r.runner_name))].filter(
          Boolean,
        ),
      ),
    }
  },

  async getTask(id: string): Promise<TaskDetail> {
    await delay()
    return clone(findTask(id))
  },

  /** 提交原文快照（C-8.2）：mock 在 submitTask / 种子任务构造时留存一份 */
  async getTaskManifest(id: string): Promise<ManifestSnapshot> {
    await delay()
    const manifest = state.manifests[id]
    if (!manifest) {
      throw new ApiClientError(404, { code: 'NOT_FOUND', message: 'Task 不存在或快照缺失', retryable: false })
    }
    return { task_id: id, manifest: clone(manifest), resolved: null }
  },

  async cancelTask(id: string): Promise<CancelResult> {
    await delay()
    const task = findTask(id)
    if (isTerminalTask(task.status)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: 'Task 已经是终态，无法取消', retryable: false })
    }
    if (task.status !== 'cancelling') {
      task.status = 'cancelling'
      pushEvent(id, 'state_changed', 'running', 'cancelling', { attempt_no: task.current_attempt })
      // clean 收口后再落 canceled（§7.6）；mock 里 1.2s 后 step() 完成
      setTimeout(() => startLive(id), 300)
    }
    return { task_id: id, status: task.status }
  },

  /**
   * 整 Task 重试（§7.8 / D-69）：新 attempt 全量重跑含 fresh deploy；历史 attempt 不覆盖。
   * 409 CONFLICT = 仍在执行；409 ATTEMPT_EXHAUSTED = 达上限（AG_ATTEMPT_MAX，默认 3）。
   */
  async retryTask(id: string): Promise<TaskRetryAccepted> {
    await delay(200)
    const task = findTask(id)
    if (!isTerminalTask(task.status)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: 'Task 还在执行，不能重试（等待终态）', retryable: false })
    }
    if (task.current_attempt >= ATTEMPT_MAX) {
      throw new ApiClientError(409, {
        code: 'ATTEMPT_EXHAUSTED',
        message: `attempt 已达上限（${ATTEMPT_MAX}），不能再重试`,
        retryable: false,
        detail: { attempt_max: ATTEMPT_MAX },
      })
    }
    const attemptNo = task.current_attempt + 1
    // 输入快照原样重用（落档 attempts/<n>/input/）；release 名按 (task_id, attempt_no) 重新派生（fresh deploy）
    const seeds = (state.attemptRunners[`${task.id}:1`] ?? []).map((runner) => ({
      name: runner.name,
      runner: runner.runner_name,
      role: runner.role,
      status: 'pending' as RunnerStatus,
      input: undefined,
    }))
    const runners = buildRunners(task.id, attemptNo, seeds)
    state.attemptRunners[`${task.id}:${attemptNo}`] = runners

    task.current_attempt = attemptNo
    task.status = 'queued'
    task.started_at = null
    task.finished_at = null
    task.error = null
    task.runner_counts = countsOf(runners)
    task.attempts.push({
      task_id: task.id,
      attempt_no: attemptNo,
      status: 'running',
      trigger: 'retry',
      input_ref: `attempts/${attemptNo}/input/`,
      exports_ref: `attempts/${attemptNo}/exports/`,
      deploy_state_ref: task.deploy ? `attempts/${attemptNo}/deploy-state.json` : null,
      error: null,
      started_at: null,
      finished_at: null,
    })
    if (task.deploy) {
      // fresh deploy：新 release 名、phase 归零
      task.deploy.release_name = `arg-${(task.id + String(attemptNo)).replace(/[^a-z0-9]/gi, '').padEnd(12, '0').slice(0, 12)}`
      task.deploy.phase = 'queued'
      task.deploy.genet_status = 'pending-install'
      task.deploy.waited_seconds = 0
      task.deploy.endpoint = null
      task.deploy.uninstall = null
      task.deploy.queued_at = nowIso()
      task.deploy.ready_at = null
      state.deploys[`${task.id}:${attemptNo}`] = clone({
        task_id: task.id,
        deploy: task.deploy,
        pods: [],
        deploy_timeout_seconds: 259200,
      })
      task.endpoint = null
    }

    for (const runner of runners) {
      state.logs[`${task.id}:${attemptNo}:${runner.seq}`] = runnerLog(
        { name: runner.name, runner: runner.runner_name, role: runner.role, status: 'pending' },
        null,
      )
    }
    pushEvent(task.id, 'submitted', null, 'queued', {
      attempt_no: attemptNo,
      payload: { trigger: 'retry', attempt_no: attemptNo },
    })
    auditLog('task.retry', 'task', task.id, { attempt_no: attemptNo })
    setTimeout(() => startLive(task.id), 600)
    return { task_id: task.id, attempt_no: attemptNo, status: 'queued' }
  },

  async listTaskAttempts(taskId: string): Promise<Attempt[]> {
    await delay(80)
    return clone(findTask(taskId).attempts.slice().sort((a, b) => a.attempt_no - b.attempt_no))
  },

  async listTaskRunners(taskId: string, query: { attempt?: number } = {}): Promise<RunnerExecution[]> {
    await delay(90)
    return clone(runnersOf(findTask(taskId), query.attempt))
  },

  async getTaskRunner(taskId: string, seq: number, query: { attempt?: number } = {}): Promise<RunnerExecution> {
    await delay(90)
    return clone(findRunner(findTask(taskId), seq, query.attempt))
  },

  async listRunnerArtifacts(
    taskId: string,
    seq: number,
    query: { attempt?: number } = {},
  ): Promise<ArtifactList> {
    await delay(90)
    const task = findTask(taskId)
    const runner = findRunner(task, seq, query.attempt)
    const items: Artifact[] = [
      {
        path: `attempts/${runner.attempt_no}/runners/${runner.seq}/logs/stdout.log`,
        kind: 'log',
        size_bytes: 9_216,
        sha256: null,
        attempt_no: runner.attempt_no,
        runner_seq: runner.seq,
      },
    ]
    if (runner.result_ref) {
      items.push({
        path: runner.result_ref,
        kind: 'result',
        size_bytes: 2_048,
        sha256: null,
        attempt_no: runner.attempt_no,
        runner_seq: runner.seq,
      })
    }
    if (runner.exports_ref) {
      items.push({
        path: runner.exports_ref,
        kind: 'export',
        size_bytes: 512,
        sha256: null,
        attempt_no: runner.attempt_no,
        runner_seq: runner.seq,
      })
    }
    return { items, total_size_bytes: items.reduce((sum, item) => sum + item.size_bytes, 0) }
  },

  async listTaskArtifacts(taskId: string): Promise<ArtifactList> {
    await delay(110)
    const task = findTask(taskId)
    const items: Artifact[] = [
      { path: 'report.json', kind: 'report', size_bytes: 18_432, sha256: null, attempt_no: null, runner_seq: null },
      { path: 'report.md', kind: 'report', size_bytes: 6_208, sha256: null, attempt_no: null, runner_seq: null },
      { path: 'metadata.json', kind: 'other', size_bytes: 3_914, sha256: null, attempt_no: null, runner_seq: null },
      { path: 'events.jsonl', kind: 'raw', size_bytes: 42_106, sha256: null, attempt_no: null, runner_seq: null },
    ]
    for (const attempt of task.attempts) {
      for (const runner of state.attemptRunners[`${task.id}:${attempt.attempt_no}`] ?? []) {
        items.push({
          path: `attempts/${attempt.attempt_no}/runners/${runner.seq}/logs/stdout.log`,
          kind: 'log',
          size_bytes: 9_216,
          sha256: null,
          attempt_no: attempt.attempt_no,
          runner_seq: runner.seq,
        })
        if (runner.result_ref) {
          items.push({
            path: runner.result_ref,
            kind: 'result',
            size_bytes: 2_048,
            sha256: null,
            attempt_no: attempt.attempt_no,
            runner_seq: runner.seq,
          })
        }
        if (runner.exports_ref) {
          items.push({
            path: runner.exports_ref,
            kind: 'export',
            size_bytes: 512,
            sha256: null,
            attempt_no: attempt.attempt_no,
            runner_seq: runner.seq,
          })
        }
        // 产物查看器的代表性 fixture（挂在 Task 0006 的三个 runner 下，§8.1 目录约定）：
        // csv（通用表格）/ html（sandbox）/ 二进制（仅下载）/ 文本超 2MB（仅下载），另每 runner 一份 raw
        if (task.id === 't-20261105-0006' && attempt.attempt_no === 1) {
          const rawExtras: Array<{ suffix: string; kind: Artifact['kind']; size: number; seq: number }> = [
            { suffix: 'raw/results.csv', kind: 'raw', size: 486, seq: 3 },
            { suffix: 'raw/report.html', kind: 'result', size: 1_024, seq: 3 },
            { suffix: 'raw/perf_summary.xlsx', kind: 'raw', size: 27, seq: 5 },
            { suffix: 'raw/raw_dump.log', kind: 'log', size: 3_145_728, seq: 3 },
          ]
          for (const extra of rawExtras) {
            if (extra.seq !== runner.seq) continue
            items.push({
              path: `attempts/${attempt.attempt_no}/runners/${runner.seq}/${extra.suffix}`,
              kind: extra.kind,
              size_bytes: extra.size,
              sha256: null,
              attempt_no: attempt.attempt_no,
              runner_seq: runner.seq,
            })
          }
        }
      }
      if (task.deploy) {
        for (const pod of state.deploys[`${task.id}:${attempt.attempt_no}`]?.pods ?? []) {
          items.push({
            path: `attempts/${attempt.attempt_no}/runners/1/logs/${pod.name}.log`,
            kind: 'log',
            size_bytes: 12_880,
            sha256: null,
            attempt_no: attempt.attempt_no,
            runner_seq: 1,
          })
        }
      }
    }
    return { items, total_size_bytes: items.reduce((sum, item) => sum + item.size_bytes, 0) }
  },

  /** 整包下载（mock）：最小 zip 字节流，够让 UI/用例验"是 application/zip 且真的触发下载" */
  async downloadBundle(taskId: string): Promise<Blob> {
    await delay(150)
    findTask(taskId)
    return new Blob([`PK\u0003\u0004 argus mock bundle for ${taskId}`], { type: 'application/zip' })
  },

  /** 单文件下载（mock）：按后缀给合理 content-type 与代表性 fixture（csv / html / 二进制 / 超 2MB 文本）。 */
  async downloadFile(taskId: string, path: string): Promise<Blob> {
    await delay(80)
    findTask(taskId)
    if (/attempts\/\d+\/runners\/\d+\/logs\/.+\.(log|out)/.test(path)) {
      // 产物查看器的"文本超限"fixture：raw_dump.log ≈ 3MB（60000 行 × ~52 字节）
      if (path.includes('raw_dump')) {
        const lines: string[] = []
        for (let i = 0; i < 60000; i += 1) {
          lines.push(`[2026-11-05T0${Math.floor(i / 3600) % 10}:${String(Math.floor(i / 60) % 60).padStart(2, '0')}:${String(i % 60).padStart(2, '0')}Z] [stream] row ${i + 1} of 60000: input_len=${256 + (i % 64) * 32} tokens/s=${1200 + (i % 97) * 7} latency_ms=${(i % 89) * 3.5}`)
        }
        return new Blob([lines.join('\n')], { type: 'text/plain' })
      }
      return new Blob([`mock log of ${path}`], { type: 'text/plain' })
    }
    if (path.endsWith('.csv')) {
      // RFC4180 fixture：含引号转义与带逗号的字段
      const csv = [
        'case,dataset,score,latency_p95_ms,note',
        '1,gsm8k,0.8421,182.4,"baseline, seed=42"',
        '2,mmlu,0.7355,201.9,full sweep',
        '3,bbh,"0.61""quoted""",240.1,"多行\nnote inside quotes"',
        '4,humaneval,0.5830,311.7,partial',
      ].join('\n')
      return new Blob([csv], { type: 'text/csv' })
    }
    if (path.endsWith('.html') || path.endsWith('.htm')) {
      const html = [
        '<!doctype html>',
        '<html lang="zh-CN">',
        '  <head><meta charset="utf-8"><title>mock result.html</title></head>',
        '  <body>',
        '    <h1>Qwen3-32B · accuracy-dataset-a</h1>',
        '    <p>Runner 产出的 result.html：沙箱渲染、脚本禁用。</p>',
        '    <table border="1"><tr><th>dataset</th><th>accuracy</th></tr><tr><td>gsm8k</td><td>0.8421</td></tr></table>',
        "    <script>document.body.setAttribute('data-script-ran', 'yes')</script>",
        '  </body>',
        '</html>',
      ].join('\n')
      return new Blob([html], { type: 'text/html' })
    }
    // 二进制 fixture（xlsx）：PK zip 头 + 乱序字节，前端应只提供下载、不预览
    if (path.endsWith('.xlsx')) {
      const bytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x06, 0x00, 0x08, 0x00, 0x00, 0x00, 0x21, 0x00, 0xde, 0xad, 0xbe, 0xef, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05])
      return new Blob([bytes], { type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' })
    }
    const type = path.endsWith('.md')
      ? 'text/markdown'
      : path.endsWith('.jsonl') || path.endsWith('.env')
        ? 'text/plain'
        : path.endsWith('.json')
          ? 'application/json'
          : 'text/plain'
    return new Blob([`mock content of ${path}`], { type })
  },

  /**
   * mock 的 `result.json`：形状照 `Result` schema（附录 A **schema 2.0**）。
   * 只有 test / pre_check 且 succeeded 的 runner 有 result；deploy / clean 无产物（404）。
   */
  async getRunnerResult(taskId: string, seq: number, query: { attempt?: number } = {}): Promise<Result> {
    await delay(90)
    const task = findTask(taskId)
    const runner = findRunner(task, seq, query.attempt)
    if (!['test', 'pre_check'].includes(runner.role)) {
      throw new ApiClientError(404, {
        code: 'NOT_FOUND',
        message: `deploy / clean 角色没有 result.json（seq=${seq}）`,
        detail: { produced: [] },
        retryable: false,
      })
    }
    if (runner.status !== 'succeeded' && runner.status !== 'failed') {
      throw new ApiClientError(404, {
        code: 'RESULT_MISSING',
        message: '该 runner 还没有产物（未跑到或被跳过）',
        detail: { produced: [] },
        retryable: false,
      })
    }
    const started = runner.started_at ? new Date(runner.started_at).getTime() : null
    const finished = runner.finished_at ? new Date(runner.finished_at).getTime() : null
    const base = {
      schema_version: '2.0',
      task_id: task.id,
      attempt_no: runner.attempt_no,
      runner_seq: runner.seq,
      runner: { name: runner.runner_name, version: runner.runner_version, image_ref: runner.image_ref },
      status: runner.status === 'failed' ? ('failed' as const) : ('succeeded' as const),
      started_at: runner.started_at ?? undefined,
      finished_at: runner.finished_at ?? undefined,
      duration_ms: started != null && finished != null ? finished - started : undefined,
      workload: { model_name: 'Qwen3-32B', dataset: 'gsm8k', input_len: 512, output_len: 128, concurrency: 8 },
      artifacts: [{ name: 'raw_results', path: 'raw/results.json', size_bytes: 2_048 }],
      error: runner.error ?? null,
    }

    if (runner.runner_name === 'endpoint-probe') {
      return { ...base, metrics: [], notes: '探活只回答服务能否出 token，不产出指标。' }
    }

    if (runner.runner_name === 'aisbench-perf') {
      const metrics: ResultMetric[] = []
      for (const [inputLen, concurrency] of [
        [512, 1],
        [4096, 8],
      ] as const) {
        const labels = { input_len: inputLen, output_len: 128, concurrency }
        metrics.push(
          { name: 'ttft', value: inputLen === 512 ? 82.4 : 143.7, unit: 'ms', statistic: 'p95', sample_size: 64, labels },
          { name: 'tpot', value: inputLen === 512 ? 18.6 : 21.4, unit: 'ms', statistic: 'mean', sample_size: 64, labels },
          {
            name: 'throughput',
            value: inputLen === 512 ? 1482.6 : 1290.2,
            unit: 'tokens_per_s',
            statistic: 'mean',
            sample_size: 64,
            labels,
          },
        )
      }
      return { ...base, metrics, notes: '压测组合：input_len × concurrency；同一组参数靠 labels 区分。' }
    }

    const metrics: ResultMetric[] = [
      { name: 'accuracy', value: 0.8421, unit: 'score', statistic: 'mean', sample_size: 200, labels: { dataset: 'gsm8k', limit: 200 } },
      { name: 'accuracy', value: 0.7355, unit: 'score', statistic: 'mean', sample_size: 200, labels: { dataset: 'mmlu', limit: 200 } },
      { name: 'latency', value: 412.8, unit: 'ms', statistic: 'p95', sample_size: 200, labels: { dataset: 'gsm8k', limit: 200 } },
      { name: 'sample_count', value: 200, unit: 'count', statistic: 'count', sample_size: 200, labels: { dataset: 'gsm8k', limit: 200 } },
    ]
    return { ...base, metrics, notes: '逐数据集 accuracy；指标名不编码维度，维度在 labels 里。' }
  },

  async getRunnerLogs(taskId: string, seq: number, opts: LogOptions = {}): Promise<LogChunk> {
    await delay(90)
    const task = findTask(taskId)
    const attemptNo = taskAttemptOf(task, opts.attempt)
    const runner = findRunner(task, seq, attemptNo)
    const key = `${taskId}:${attemptNo}:${seq}`
    let text = state.logs[key] ?? '（该 runner 还没有日志）'
    const offset = Math.max(0, opts.offset ?? 0)

    if (runner.status === 'running' && offset > 0) {
      const tick = (state.logTicks[key] ?? 0) + 1
      state.logTicks[key] = tick
      const clock = new Date().toLocaleTimeString('zh-CN', { hour12: false })
      text = `${text}\n[${clock}] [mock] stdout 新行 #${tick}（运行中，轮询追加）`
      state.logs[key] = text
    }

    const chunk = text.slice(Math.min(offset, text.length))
    return { text: chunk, offset: text.length, coverage: null }
  },

  async getTaskDeploy(taskId: string): Promise<DeploySnapshot> {
    await delay(90)
    const task = findTask(taskId)
    const attemptNo = task.current_attempt
    return clone(
      state.deploys[`${taskId}:${attemptNo}`] ?? { task_id: taskId, deploy: task.deploy ?? null, pods: [] },
    )
  },

  /**
   * Deploy 日志（契约 v2）：`?pod=` 可选 —— 0 个 Pod 返回 200 + 空列表 + message（排队中 / 外接模式）；
   * 1 个 Pod 直接服务；≥2 个不传 pod → 409 `pod_ambiguous`（不默默取第一个）；
   * pod 不存在 → 404 `pod_not_found`（附 `available_pods[]`）。`pods[]` 始终返回全部 Pod（页签渲染用）。
   */
  async getTaskDeployLogs(taskId: string, query: DeployLogQuery = {}): Promise<DeployLogs> {
    await delay(110)
    const task = findTask(taskId)
    const attemptNo = taskAttemptOf(task, query.attempt)
    const snapshot = state.deploys[`${taskId}:${attemptNo}`]
    const pods: DeployLogPod[] = (snapshot?.pods ?? []).map((pod, index, all) => ({
      name: pod.name,
      ready: pod.ready ?? false,
      // 故意让最后一个 Pod 重启过：这样 gap（此处不连续）在 UI 上一定看得到
      restarts: index === all.length - 1 ? 2 : (pod.restarts ?? 0),
      started_at: pod.started_at ?? iso(30),
      last_seen_at: iso(0),
    }))
    const gaps: DeployLogGap[] = pods
      .filter((pod) => pod.restarts > 0)
      .map((pod) => ({ pod: pod.name, from: iso(24), to: iso(22), reason: 'pod restarted' }))

    const phase = task.deploy?.phase ?? null
    if (pods.length === 0) {
      const deploying = phase === 'queued' || phase === 'accepted' || phase === 'deploying'
      return {
        task_id: taskId,
        attempt_no: attemptNo,
        pods: [],
        entries: [],
        gaps: [],
        window: 'full',
        truncated: false,
        next_offset: 0,
        message: deploying
          ? '部署还在排队（Genet / Volcano 层），Pod 尚未创建 —— 等 Pod 起来后这里会按 Pod 分页签显示日志。'
          : task.deploy
            ? 'Pod 已被回收，日志随 Pod 一起消失（MVP 未落盘）。'
            : '外接模式（无部署），没有部署 Pod 日志。',
      }
    }

    if (pods.length >= 2 && !query.pod) {
      throw new ApiClientError(409, {
        code: 'CONFLICT',
        message: '该部署有多个 Pod，必须指定 pod 参数',
        detail: { reason: 'pod_ambiguous', available_pods: pods.map((pod) => pod.name) },
        retryable: false,
      })
    }
    const target = pods.find((pod) => pod.name === query.pod) ?? (pods.length === 1 ? pods[0] : undefined)
    if (!target) {
      throw new ApiClientError(404, {
        code: 'NOT_FOUND',
        message: `没有名为 ${query.pod} 的 Pod`,
        detail: { reason: 'pod_not_found', available_pods: pods.map((pod) => pod.name) },
        retryable: false,
      })
    }

    // 日志行按 (task, attempt, pod) 持久化：游标 offset 才有稳定的行号语义；运行中轮询时追加新行
    const streamKey = `${taskId}:${attemptNo}#${target.name}`
    const lines = state.deployLogLines[streamKey] ?? deployLogSeed(target.name, task.name)
    state.deployLogLines[streamKey] = lines
    const offset = query.tail_lines ? 0 : (query.offset ?? 0)
    if (!isTerminalTask(task.status) && !query.tail_lines && offset > 0) {
      const tick = (state.deployLogTicks[streamKey] ?? 0) + 1
      state.deployLogTicks[streamKey] = tick
      const clock = new Date().toLocaleTimeString('zh-CN', { hour12: false })
      lines.push(`${clock} [main] [mock] 新日志行 #${tick}（运行中，轮询追加）`)
    }
    const tail = query.tail_lines && query.tail_lines > 0 ? lines.slice(-query.tail_lines) : lines
    const limit = query.limit ?? 200
    const slice = tail.slice(offset, offset + limit)
    const next = offset + slice.length
    const entries: DeployLogEntry[] =
      slice.length > 0
        ? [
            {
              pod: target.name,
              container: target.name.includes('pd') ? 'prefill' : 'main',
              offset,
              line_count: slice.length,
              text: slice.join('\n'),
            },
          ]
        : []

    return {
      task_id: taskId,
      attempt_no: attemptNo,
      pods,
      entries,
      gaps,
      window: query.tail_lines && query.tail_lines > 0 ? 'tail' : 'full',
      truncated: next < tail.length,
      next_offset: next < tail.length ? next : 0,
    }
  },

  async getEventsHistory(taskId: string, sinceSeq = 0): Promise<TaskEvent[]> {
    await delay(60)
    return clone((state.events[taskId] ?? []).filter((e) => e.seq > sinceSeq))
  },

  async listRunners(query: RunnerListQuery = {}): Promise<Runner[]> {
    await delay(90)
    const visible = state.runners.filter((r) =>
      query.include_retired || query.orphaned ? true : !r.retired,
    )
    return visible
      .filter((r) => (query.role ? r.role === query.role : true))
      .filter((r) => (query.orphaned === undefined ? true : Boolean(r.orphaned) === query.orphaned))
      .map((r) => normalizeRunner(r))
  },

  async getRunner(name: string): Promise<Runner> {
    await delay(80)
    const runner = state.runners.find((r) => r.name === name)
    if (!runner) notFound(`Runner ${name}`)
    return normalizeRunner(runner)
  },

  async listClusters(): Promise<Cluster[]> {
    await delay(80)
    return clone(state.clusters)
  },

  async getQueueStatus(): Promise<QueueStatus> {
    await delay(90)
    const waitingRunners: WaitingTask[] = []
    for (const task of Object.values(state.tasks)) {
      if (isTerminalTask(task.status)) continue
      for (const runner of state.attemptRunners[`${task.id}:${task.current_attempt}`] ?? []) {
        if (runner.status !== 'queued') continue
        waitingRunners.push({
          task_id: task.id,
          attempt_no: runner.attempt_no,
          seq: runner.seq,
          name: runner.name,
          status: runner.status,
          waiting_reason: runner.waiting_reason ?? 'slot_waiting',
        })
      }
    }
    const byReason = { slot_waiting: 0, queue_full: 0, insufficient_resources: 0, gang_pending: 0 }
    for (const runner of waitingRunners) byReason[runner.waiting_reason] += 1

    const active = Object.values(state.tasks).filter((t) => t.status === 'running').length
    return {
      generated_at: nowIso(),
      slots: {
        concurrency_max: 20,
        active,
        available: Math.max(0, 20 - active),
        endpoints: [],
      },
      waiting: { total: waitingRunners.length, by_reason: byReason, runners: waitingRunners },
      volcano: {
        available: true,
        stale: false,
        detail: null,
        queues: [
          {
            name: 'npu-batch',
            state: 'Open',
            pending_jobs: 6,
            longest_wait_seconds: 1080,
            // 契约：reason 是 PodGroup conditions[-1].message 原文，平台不翻译
            reason: '0/8 nodes are available: 8 Insufficient nvidia.com/npu.',
          },
          {
            name: 'gpu-shared',
            state: 'Open',
            pending_jobs: 4,
            longest_wait_seconds: 2460,
            reason:
              'PodGroup default/gpu-batch-7g2kd is Pending: minMember 4 > running 0; 0/1 nodes are available: 1 insufficient nvidia.com/gpu.',
          },
        ],
      },
    }
  },

  async submitTask(manifest: TaskManifest): Promise<SubmitResult> {
    await delay(260)
    const issues = validateManifest(manifest)
    if (issues.length > 0) {
      throw new ApiClientError(400, {
        code: 'VALIDATION_ERROR',
        // 与真后端一致：message 是给人看的一句话，细节在 detail.issues[]，trace_id 供排障
        message: '提交内容未通过校验',
        detail: { issues },
        retryable: false,
        trace_id: `mock-${Math.random().toString(16).slice(2, 10)}`,
      })
    }

    const body = manifest.task
    const taskId = nextTaskId()
    state.manifests[taskId] = clone(manifest)
    const seeds = resolveRunnerSeeds(manifest)
    const attemptNo = 1
    const runners = buildRunners(taskId, attemptNo, seeds)
    state.attemptRunners[`${taskId}:${attemptNo}`] = runners

    const endpoint: Endpoint | null = body.service
      ? endpointOf(body.service.url, body.service.model_name ?? 'Qwen3-32B', 'service')
      : null

    const detail: TaskDetail = {
      id: taskId,
      name: body.name,
      status: 'submitted',
      submitted_by: getCurrentUser(),
      submitted_at: nowIso(),
      started_at: null,
      finished_at: null,
      endpoint,
      deploy: body.deploy
        ? {
            runner: body.deploy.runner,
            version: body.deploy.version,
            input: body.deploy.input ?? '',
            spec_digest: 'sha256:mock',
            namespace: null,
            release_name: null,
            deploy_state_ref: `attempts/1/deploy-state.json`,
            phase: null, // runner 未启动时为 null（契约）
            genet_status: null,
            reason: null,
            waited_seconds: 0,
            pod_count: null,
            label_selector: null,
            queue: null,
            endpoint: null,
            queued_at: nowIso(),
            ready_at: null,
            cancel_requested_at: null,
            failure: null,
            uninstall: null,
          }
        : null,
      env: body.env ?? {},
      secret_env: Object.entries(body.secrets ?? {}).map(([name, secret_ref]) => ({ name, secret_ref })),
      error: null,
      retry_of: null,
      current_attempt: 0, // 202 时刻为 0（attempt 在进入 running 时创建）
      runner_counts: countsOf(runners),
      revision: 1,
      attempts: [],
      runners,
    }
    state.tasks[taskId] = detail
    state.deploys[`${taskId}:${attemptNo}`] = {
      task_id: taskId,
      deploy: detail.deploy ?? null,
      pods: [],
      deploy_timeout_seconds: 259200,
    }
    for (const runner of runners) {
      state.logs[`${taskId}:${attemptNo}:${runner.seq}`] = runnerLog(
        { name: runner.name, runner: runner.runner_name, role: runner.role, status: 'pending' },
        null,
      )
    }
    pushEvent(taskId, 'submitted', null, 'submitted', { payload: { runner_count: runners.length } })

    // 后台推进：attempt 1 创建 → runner 管道一个个跑（「提交 → 详情页 → 事件流追加」在 mock 下完整演一遍）
    setTimeout(() => {
      const created = state.tasks[taskId]
      if (!created) return
      created.current_attempt = attemptNo
      created.status = 'queued'
      created.attempts = [
        {
          task_id: taskId,
          attempt_no: attemptNo,
          status: 'running',
          trigger: 'initial',
          input_ref: `attempts/${attemptNo}/input/`,
          exports_ref: `attempts/${attemptNo}/exports/`,
          deploy_state_ref: created.deploy ? `attempts/${attemptNo}/deploy-state.json` : null,
          error: null,
          started_at: null,
          finished_at: null,
        },
      ]
      pushEvent(taskId, 'state_changed', 'submitted', 'queued', { attempt_no: attemptNo })
      setTimeout(() => {
        created.status = 'running'
        created.started_at = nowIso()
        pushEvent(taskId, 'state_changed', 'queued', 'running', { attempt_no: attemptNo })
        startLive(taskId)
      }, 1800)
    }, 1200)

    auditLog('task.submit', 'task', taskId, { runners: runners.length })
    return {
      task: { id: taskId, status: detail.status, submitted_at: detail.submitted_at, current_attempt: 0 },
      runners: runners.map((runner) => ({
        seq: runner.seq,
        name: runner.name,
        runner: runner.runner_name,
        role: runner.role,
        status: runner.status,
      })),
    }
  },

  async dryRun(manifest: TaskManifest): Promise<DryRunResult> {
    await delay(220)
    const issues = validateManifest(manifest)
    const body = manifest.task
    const previewRunners = resolveRunnerSeeds(manifest).map((seed, index) => ({
      seq: index + 1,
      name: runnerNameOf(seed),
      runner: seed.runner,
      role: seed.role ?? 'test',
      injected: seed.role === 'deploy' || seed.role === 'clean' || seed.runner === 'endpoint-probe',
    }))
    return {
      valid: issues.length === 0,
      issues,
      resolved_preview: {
        precheck: (body.precheck ?? 'auto') === 'off' ? { injected: false } : { injected: true, runner: 'endpoint-probe', version: '1.0.0' },
        runners: previewRunners,
        schema_version: '2.0',
      },
    }
  },

  /* ---------------------------------------------------------------- 报告（契约内） */

  async getTaskReport(taskId: string): Promise<Report> {
    await delay(120)
    const task = findTask(taskId)
    const attemptNo = task.current_attempt
    // 契约：报告在 task_finished 之后异步生成；未终态时 404 + Retry-After（C-7.2）。
    if (!isTerminalTask(task.status)) {
      throw new ApiClientError(404, {
        code: 'NOT_FOUND',
        message: `报告尚未生成：Task 当前是 ${task.status}，终态后才会写出 report.json`,
        detail: { retry_after_seconds: 5 },
        retryable: true,
      })
    }
    const runners = state.attemptRunners[`${taskId}:${attemptNo}`] ?? []
    const rows: ReportRunner[] = runners.map((runner) => ({
      seq: runner.seq,
      name: runner.name,
      role: runner.role,
      status: runner.status,
      exports_ref: runner.exports_ref ?? null,
      result_ref: runner.result_ref ?? null,
      duration_ms:
        runner.started_at && runner.finished_at
          ? new Date(runner.finished_at).getTime() - new Date(runner.started_at).getTime()
          : null,
    }))
    const counts: ReportCounts = {
      total: rows.length,
      succeeded: rows.filter((r) => r.status === 'succeeded').length,
      failed: rows.filter((r) => r.status === 'failed').length,
      skipped: rows.filter((r) => r.status === 'skipped').length,
      canceled: rows.filter((r) => r.status === 'canceled').length,
    }
    const failures: ReportFailure[] = runners
      .filter((runner) => runner.status === 'failed' && runner.error)
      .map((runner) => ({
        seq: runner.seq,
        name: runner.name,
        error_code: runner.error?.code ?? 'RUNNER_NONZERO_EXIT',
        reason: runner.error?.message ?? 'runner 失败',
      }))
    return {
      task_id: task.id,
      attempt_no: attemptNo,
      status: task.status,
      counts,
      runners: rows,
      failures,
      cleanup_failed: false,
      truncated: false,
    }
  },

  async getTaskReportMd(taskId: string): Promise<string> {
    await delay(100)
    const task = findTask(taskId)
    const report = await mockApi.getTaskReport(taskId)
    const lines = [
      `# ${task.id} · ${task.name}`,
      '',
      `- 状态：${task.status}`,
      `- 提交人：${task.submitted_by}`,
      `- attempt：#${report.attempt_no}`,
      '',
      '## runner 统计',
      '',
      `| 总数 | 成功 | 失败 | 跳过 | 取消 |`,
      `| --- | --- | --- | --- | --- |`,
      `| ${report.counts.total} | ${report.counts.succeeded} | ${report.counts.failed} | ${report.counts.skipped} | ${report.counts.canceled} |`,
      '',
      '## 失败',
      '',
      ...(report.failures.length === 0
        ? ['无。']
        : report.failures.map((f) => `- seq ${f.seq} ${f.name}：${f.reason}（${f.error_code}）`)),
    ]
    return lines.join('\n')
  },

  /* ---------------------------------------------------------------- Execution Profile */

  async listProfiles(): Promise<ExecutionProfile[]> {
    await delay(80)
    return clone(state.profiles)
  },

  /* ---------------------------------------------------------------- 共享挂载（D-65） */

  /** viewer+ 只读视图：只回启用中的行 */
  async listSharedMounts(): Promise<SharedMount[]> {
    await delay(90)
    return clone(state.sharedMounts.filter((mount) => mount.enabled))
  },

  /** admin 视图：含停用行 */
  async listAdminSharedMounts(): Promise<SharedMount[]> {
    await delay(90)
    return clone(state.sharedMounts)
  },

  async createAdminSharedMount(body: SharedMountWrite): Promise<SharedMount> {
    await delay(160)
    const issues = validateSharedMount(body, null)
    if (issues.length > 0) throw sharedMountValidation(issues)
    const mount: SharedMount = {
      name: body.name.trim(),
      mount_path: body.mount_path.trim(),
      claim_name: body.claim_name.trim(),
      read_only: body.read_only ?? true,
      enabled: body.enabled ?? true,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    }
    state.sharedMounts.push(mount)
    return clone(mount)
  },

  async updateAdminSharedMount(name: string, body: SharedMountPatch): Promise<SharedMount> {
    await delay(160)
    const current = state.sharedMounts.find((mount) => mount.name === name)
    if (!current) notFound(`共享挂载 ${name}`)
    const merged: SharedMountWrite = {
      name,
      mount_path: body.mount_path ?? current.mount_path,
      claim_name: body.claim_name ?? current.claim_name,
      read_only: body.read_only ?? current.read_only,
      enabled: body.enabled ?? current.enabled,
    }
    // 校验时把"自己"排除掉，否则改自己会撞自己的路径
    const issues = validateSharedMount(merged, name)
    if (issues.length > 0) throw sharedMountValidation(issues)
    Object.assign(current, {
      mount_path: merged.mount_path.trim(),
      claim_name: merged.claim_name.trim(),
      read_only: merged.read_only ?? current.read_only,
      enabled: merged.enabled ?? current.enabled,
      updated_at: new Date().toISOString(),
    })
    return clone(current)
  },

  async deleteAdminSharedMount(name: string): Promise<void> {
    await delay(140)
    const index = state.sharedMounts.findIndex((mount) => mount.name === name)
    // 重复删 404（契约）
    if (index < 0) notFound(`共享挂载 ${name}`)
    state.sharedMounts.splice(index, 1)
  },

  async listAdminClusters(): Promise<AdminCluster[]> {
    await delay(90)
    return clone(state.adminClusters)
  },

  async createAdminCluster(body: ClusterWrite): Promise<AdminCluster> {
    await delay(160)
    if (state.adminClusters.some((cluster) => cluster.name === body.name)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: `集群「${body.name}」已存在`, retryable: false })
    }
    const created: AdminCluster = {
      name: body.name,
      api_server: body.api_server,
      kubeconfig_key: body.kubeconfig_key ?? `${body.name}.kubeconfig`,
      description: body.description ?? null,
      last_check_at: nowIso(),
      last_check_result: 'ok',
    }
    state.adminClusters = [...state.adminClusters, created]
    state.clusters = [...state.clusters, { name: created.name, api_server: created.api_server, description: created.description }]
    auditLog('cluster.create', 'cluster', created.name)
    return clone(created)
  },

  async updateAdminCluster(name: string, body: ClusterWrite): Promise<AdminCluster> {
    await delay(160)
    const index = state.adminClusters.findIndex((cluster) => cluster.name === name)
    if (index < 0) notFound(`集群 ${name}`)
    const updated: AdminCluster = {
      ...(state.adminClusters[index] as AdminCluster),
      ...body,
      name: body.name || name,
    }
    state.adminClusters = state.adminClusters.map((cluster, i) => (i === index ? updated : cluster))
    auditLog('cluster.update', 'cluster', updated.name)
    return clone(updated)
  },

  async deleteAdminCluster(name: string): Promise<void> {
    await delay(160)
    const referenced = state.profiles.filter((profile) => profile.cluster === name)
    if (referenced.length > 0) {
      throw new ApiClientError(409, {
        code: 'CONFLICT',
        message: `集群「${name}」仍被 ${referenced.length} 个 Profile 引用`,
        detail: { profiles: referenced.map((profile) => profile.name) },
        retryable: false,
      })
    }
    state.adminClusters = state.adminClusters.filter((cluster) => cluster.name !== name)
    state.clusters = state.clusters.filter((cluster) => cluster.name !== name)
    auditLog('cluster.delete', 'cluster', name)
  },

  async createProfile(body: ProfileWrite): Promise<ExecutionProfile> {
    await delay(160)
    if (state.profiles.some((profile) => profile.name === body.name)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: `Profile「${body.name}」已存在`, retryable: false })
    }
    const created: ExecutionProfile = { ...body, mounts: [], image_pull_secrets: [] }
    state.profiles = [...state.profiles, created]
    auditLog('profile.create', 'profile', created.name)
    return clone(created)
  },

  async updateProfile(name: string, body: ProfileWrite): Promise<ExecutionProfile> {
    await delay(160)
    const index = state.profiles.findIndex((profile) => profile.name === name)
    if (index < 0) notFound(`Profile ${name}`)
    const updated: ExecutionProfile = { ...(state.profiles[index] as ExecutionProfile), ...body }
    state.profiles = state.profiles.map((profile, i) => (i === index ? updated : profile))
    auditLog('profile.update', 'profile', updated.name)
    return clone(updated)
  },

  async deleteProfile(name: string): Promise<void> {
    await delay(160)
    state.profiles = state.profiles.filter((profile) => profile.name !== name)
    auditLog('profile.delete', 'profile', name)
  },

  async listAdminSecrets(): Promise<AdminSecret[]> {
    await delay(90)
    return clone(state.secrets)
  },

  async createAdminSecret(body: SecretWrite): Promise<AdminSecret> {
    await delay(160)
    const created: AdminSecret = { ...body, description: body.description ?? null }
    state.secrets = [...state.secrets, created]
    auditLog('secret.register', 'secret', created.name)
    return clone(created)
  },

  async listAudit(query: AuditQuery = {}): Promise<Page<AuditEntry>> {
    await delay(90)
    const filtered = state.audit
      .filter((entry) => (query.action ? entry.action === query.action : true))
      .slice()
      .sort((a, b) => b.at.localeCompare(a.at))
    return paginate(clone(filtered), query.cursor, query.limit ?? 50)
  },

  async getDashboardStats(): Promise<DashboardStats> {
    await delay(120)
    const tasks = Object.values(state.tasks)
    const active = tasks.filter((t) => !isTerminalTask(t.status))
    const terminal = tasks.filter((t) => isTerminalTask(t.status))
    const succeeded = terminal.filter((t) => t.status === 'succeeded').length
    const queuedRunners = active.reduce(
      (sum, t) => sum + (t.runner_counts?.pending ?? 0) + (t.runner_counts?.running ?? 0),
      0,
    )
    const dayAgo = Date.now() - 24 * 3600 * 1000
    const failed24h = tasks.filter(
      (t) =>
        ['failed', 'service_unavailable'].includes(t.status) &&
        new Date(t.submitted_at).getTime() >= dayAgo,
    ).length
    return {
      active_tasks: active.length,
      queued_runners: queuedRunners,
      failed_24h: failed24h,
      success_rate: terminal.length === 0 ? null : succeeded / terminal.length,
      total_tasks: tasks.length,
      runner_count: state.runners.filter((r) => !r.retired).length,
    }
  },
}
