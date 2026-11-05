/**
 * Mock 数据源 —— 后端未起时把页面填满（`VITE_USE_MOCK=1`，**默认关**）。
 *
 * 规则：
 *  - 实现的是 `Api` 接口，字段形状与 `server/api/openapi.yaml` 一致；
 *    切真实接口时页面不改一行。
 *  - 数据有状态：提交 Run 会真的出现在列表里，取消走 cancelling → canceled，
 *    事件流（SSE）由 mock 按状态机推进 —— 用来验收「事件流实时追加」这一条。
 *  - 数据贴近 design/prototype/screenshots：同一批名字、同一批状态、同一套日志口径。
 */
import { ApiClientError, getCurrentUser } from './client'
import type { Api, LogOptions } from './contract'
import type {
  Attempt,
  AttemptStatus,
  CancelResult,
  Cluster,
  DashboardStats,
  Deploy,
  DeployPod,
  DeploySnapshot,
  DryRunResult,
  Endpoint,
  Failure,
  Identity,
  LogChunk,
  Meta,
  Page,
  QueueStatus,
  RetryPolicy,
  Run,
  RunEvent,
  RunEventType,
  RunListQuery,
  RunManifest,
  RunStatus,
  SkippedReason,
  SubmitResult,
  Task,
  TaskCancelResult,
  TaskDetail,
  TaskListQuery,
  TaskStatus,
  TaskSummary,
  Tester,
  TesterListQuery,
  TesterWire,
  ValidationIssue,
  WaitingReason,
  WaitingTask,
} from './types'
import { normalizeTester } from './normalize'
import { applyIdentity, clearAuth } from './auth'

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

function endpointOf(url: string, modelName: string, source: Endpoint['source'], deployId?: string): Endpoint {
  return {
    url,
    key: endpointKeyOf(url),
    source,
    model_name: modelName,
    model_path: '',
    protocol: 'http',
    deploy_id: deployId ?? null,
  }
}

/* ================================================================ Tester 目录 */

function testerWire(input: {
  name: string
  version: string
  display_name: string
  description: string
  role: 'pre_check' | 'test'
  on_failure: 'continue' | 'abort_run'
  exclusive: boolean
  placeholder: string
  parameters: TesterWire['parameters']
  env?: TesterWire['env']
}): TesterWire {
  return {
    name: input.name,
    version: input.version,
    display_name: input.display_name,
    description: input.description,
    role: input.role,
    on_failure: input.on_failure,
    entry: '/tester/run.sh',
    image: `registry.dev.huawei.com/testplane/${input.name}:${input.version}`,
    resources: { cpu: '1', memory: '256Mi', gpu: { resource: 'nvidia.com/gpu', count: 0 } },
    concurrency: { endpoint_exclusive: input.exclusive },
    mounts: [],
    parameters: input.parameters,
    env: input.env ?? {
      ENDPOINT_API_KEY: { secret: true, description: 'Bearer token；不提供时按无鉴权探测' },
    },
    placeholder: input.placeholder,
    content_sha: 'sha256:mock',
    source_file: `testers/${input.name}/tester.yaml`,
    orphaned: false,
    registered_at: iso(60 * 24 * 7),
    registered_by: 'system',
  }
}

const TESTERS: TesterWire[] = [
  testerWire({
    name: 'endpoint-probe',
    version: '1.0.0',
    display_name: '服务就绪探测',
    description:
      '对已部署的 LLM 服务执行生成型探活：POST <url>/chat/completions，max_tokens=1、stream=true，首包 token 一到即判定可服务。',
    role: 'pre_check',
    on_failure: 'abort_run',
    exclusive: true,
    placeholder: '--wait_timeout_seconds=600 --retry_interval_seconds=15',
    parameters: {
      url: { type: 'string', required: true, description: 'OpenAI 兼容 base URL；平台有值时注入，不得重复传' },
      model_name: { type: 'string', description: '模型名；省略时从 /v1/models 取第一个' },
      wait_timeout_seconds: { type: 'integer', default: '600', description: '就绪等待窗口（秒）' },
      retry_interval_seconds: { type: 'integer', default: '15', description: '两次探测间隔（秒）' },
      max_tokens: { type: 'integer', default: '1', description: '探活请求的 max_tokens，不要调大' },
      stream: { type: 'boolean', default: 'true', description: '流式探活；true 时首 token 到达即通过' },
    },
  }),
  testerWire({
    name: 'aisbench-acc',
    version: '1.0.0',
    display_name: '数据集精度评测',
    description: '数据集精度评测，输出逐数据集 accuracy。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    placeholder: '--dataset=/data/gsm8k.jsonl --limit=200',
    parameters: {
      url: { type: 'string', required: true, description: 'OpenAI 兼容 base URL' },
      model_name: { type: 'string', required: true, description: '模型名' },
      dataset: { type: 'string', required: true, description: '数据集在容器内的绝对路径' },
      limit: { type: 'integer', description: '最多评测多少条样本' },
    },
  }),
  testerWire({
    name: 'aisbench-perf',
    version: '1.0.0',
    display_name: '性能压测',
    description: '性能压测，输出 TTFT / TPOT / 吞吐。独占 Endpoint，避免与其它 Task 互相干扰。',
    role: 'test',
    on_failure: 'continue',
    exclusive: true,
    placeholder: '--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]',
    parameters: {
      url: { type: 'string', required: true, description: 'OpenAI 兼容 base URL' },
      model_name: { type: 'string', required: true, description: '模型名' },
      input_lens: { type: 'list<int>', required: true, description: '输入长度列表' },
      output_lens: { type: 'list<int>', required: true, description: '输出长度列表' },
      concurrency: { type: 'list<int>', required: true, description: '并发档位' },
    },
  }),
  testerWire({
    name: 'evalscope-acc',
    version: '2.3.0',
    display_name: 'EvalScope 精度评测',
    description: 'EvalScope 精度评测，适合跑多 benchmark 组合。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    placeholder: '--dataset=mmlu --subset=abstract_algebra --limit=100',
    parameters: {
      url: { type: 'string', required: true, description: 'OpenAI 兼容 base URL' },
      model_name: { type: 'string', required: true, description: '模型名' },
      dataset: { type: 'string', required: true, description: 'benchmark 名称' },
      subset: { type: 'string', description: '子集' },
      limit: { type: 'integer', description: '样本上限' },
    },
  }),
]

/* ================================================================ 内部状态 */

interface MockState {
  runs: Run[]
  /** `${taskId}` → TaskDetail（含 attempts） */
  tasks: Record<string, TaskDetail>
  /** 日志：`${taskId}#${attemptIdx}` → 文本 */
  logs: Record<string, string>
  /** `${runId}` → DeploySnapshot */
  deploys: Record<string, DeploySnapshot>
  events: Record<string, RunEvent[]>
  seq: Record<string, number>
  testers: TesterWire[]
  clusters: Cluster[]
  timers: Map<string, ReturnType<typeof setInterval>>
}

const state: MockState = {
  runs: [],
  tasks: {},
  logs: {},
  deploys: {},
  events: {},
  seq: {},
  testers: clone(TESTERS),
  clusters: [],
  timers: new Map(),
}

/* ================================================================ 造数据 */

interface TaskSeed {
  seq: number
  name: string
  tester: string
  status: TaskStatus
  args?: string
  waiting?: WaitingReason
  statusReason?: SkippedReason
  error?: Failure
  startedMinAgo?: number
  durationSec?: number
  attempts?: AttemptStatus[]
}

interface RunSeed {
  id: string
  name: string
  status: RunStatus
  submittedBy: string
  submittedMinAgo: number
  durationSec?: number
  deploy?: Deploy
  pods?: DeployPod[]
  deployManifest?: Record<string, unknown> | null
  endpoint?: Endpoint | null
  error?: Failure
  env?: Record<string, string>
  secrets?: Record<string, string>
  cleanupPolicy?: Run['cleanup_policy']
  tasks: TaskSeed[]
}

function attemptLog(taskSeed: TaskSeed, attempt: Attempt): string {
  const t = attempt.started_at
    ? new Date(attempt.started_at).toLocaleTimeString('zh-CN', { hour12: false })
    : '--:--:--'
  if (taskSeed.tester === 'endpoint-probe') {
    return [
      `[${t}] 探活 <endpoint>/chat/completions model_name=Qwen3-32B max_tokens=1 stream=true`,
      `[${t}] 第 1 次探测：未就绪 —— HTTP 503 {"error":"model loading"}`,
      `[${t}] 第 2 次探测：HTTP 200，收到 SSE 数据帧，服务可出 token`,
      `结论：status=succeeded（等待 38s）`,
    ].join('\n')
  }
  if (attempt.status === 'failed') {
    return [
      `[${t}] loading dataset ${taskSeed.args ?? ''}`,
      `[${t}] ERROR connection reset by peer (attempt=${attempt.idx})`,
      `[${t}] Traceback (most recent call last):`,
      `[${t}] requests.exceptions.ConnectionError: RemoteDisconnected`,
      `结论：status=failed exit=1 code=TESTER_NONZERO_EXIT`,
    ].join('\n')
  }
  if (taskSeed.tester === 'aisbench-perf') {
    return [
      `[${t}] [sweep] input_lens=[512,1024,4096] output_lens=[128,512] concurrency=[1,4,8,16]`,
      `[${t}] [sweep] 24 combos, 24 ok`,
      `[${t}] ttft_ms p95=143.7  tpot_ms mean=21.4  throughput=1482.6 tokens/s`,
    ].join('\n')
  }
  return [`[${t}] loading dataset ${taskSeed.args ?? ''}`, `[${t}] 200 samples done`, `[${t}] accuracy=0.8421`].join('\n')
}

function buildAttempts(taskId: string, seed: TaskSeed): Attempt[] {
  const statuses = seed.attempts ?? (['pending', 'queued', 'skipped'].includes(seed.status) ? [] : [mapAttemptStatus(seed.status)])
  return statuses.map((status, index) => {
    const idx = index + 1
    const started = seed.startedMinAgo == null ? null : iso(seed.startedMinAgo)
    const startedAt = started ? new Date(started) : null
    const finishedAt =
      startedAt && seed.durationSec != null ? new Date(startedAt.getTime() + seed.durationSec * 1000).toISOString() : null
    return {
      id: `${taskId}-a${idx}`,
      task_id: taskId,
      idx,
      status,
      phase: status === 'succeeded' ? 'Succeeded' : status === 'running' ? 'Running' : null,
      exit_code: status === 'succeeded' ? 0 : status === 'failed' ? 1 : null,
      image_digest: `sha256:${taskId.replace(/[^a-z0-9]/gi, '').slice(0, 12)}d1e7`,
      started_at: startedAt ? startedAt.toISOString() : null,
      finished_at: finishedAt,
      log_ref: `tasks/${taskId}/attempts/${idx}/stdout.log`,
      artifact_ref: null,
      result_ref: status === 'succeeded' ? `tasks/${taskId}/attempts/${idx}/result.json` : null,
      error: status === 'failed' ? (seed.error ?? null) : null,
      executor_ref: { cluster: 'prod-npu-01', namespace: 'testplane', job: `tp-${taskId}-a${idx}` },
    }
  })
}

function mapAttemptStatus(status: TaskStatus): AttemptStatus {
  switch (status) {
    case 'preparing':
      return 'preparing'
    case 'running':
      return 'running'
    case 'succeeded':
      return 'succeeded'
    case 'failed':
      return 'failed'
    case 'canceled':
      return 'canceled'
    default:
      return 'pending'
  }
}

function emptyCounts(): NonNullable<Run['task_counts']> {
  return { total: 0, pending: 0, running: 0, succeeded: 0, failed: 0, skipped: 0 }
}

function seedRun(seed: RunSeed): Run {
  const runId = seed.id
  const summaries: TaskSummary[] = []
  const counts = emptyCounts()

  for (const taskSeed of seed.tasks) {
    const taskId = `t-${runId.slice(2)}-${String(taskSeed.seq).padStart(2, '0')}`
    const attempts = buildAttempts(taskId, taskSeed)
    const currentAttempt = attempts.length
    const tester = state.testers.find((t) => t.name === taskSeed.tester)
    const task: TaskDetail = {
      id: taskId,
      run_id: runId,
      seq: taskSeed.seq,
      name: taskSeed.name,
      tester_name: taskSeed.tester,
      tester_version: tester?.version ?? '1.0.0',
      image_ref: tester?.image ?? `registry.dev.huawei.com/testplane/${taskSeed.tester}:1.0.0`,
      profile: 'k8s-npu',
      args_raw: taskSeed.args ?? '',
      args_argv: taskSeed.args ? taskSeed.args.split(/\s+/) : [],
      injected_argv: [`--url=${seed.endpoint?.url ?? 'http://10.0.0.10:8000/v1'}`, `--model_name=Qwen3-32B`],
      env: seed.env ?? {},
      secret_env: Object.keys(seed.secrets ?? {}).map((name) => ({ name })),
      on_failure: tester?.on_failure ?? 'continue',
      timeout_seconds: 14400,
      retry_policy: {} as RetryPolicy,
      status: taskSeed.status,
      status_reason: taskSeed.statusReason ?? null,
      waiting_reason: taskSeed.waiting ?? null,
      endpoint_key: seed.endpoint?.key ?? null,
      endpoint_exclusive: tester?.concurrency?.endpoint_exclusive ?? false,
      current_attempt: currentAttempt,
      available_at: null,
      cancel_requested: false,
      result_ref: null,
      error: taskSeed.error ?? null,
      created_at: iso(seed.submittedMinAgo),
      updated_at: iso(Math.max(0, seed.submittedMinAgo - 1)),
      attempts,
    }
    state.tasks[taskId] = task
    for (const attempt of attempts) {
      state.logs[`${taskId}#${attempt.idx}`] = attemptLog(taskSeed, attempt)
    }
    summaries.push({
      id: taskId,
      name: task.name,
      status: task.status,
      waiting_reason: task.waiting_reason,
      attempts: task.current_attempt,
      error: task.error,
    })

    counts.total += 1
    if (['pending', 'queued'].includes(task.status)) counts.pending = (counts.pending ?? 0) + 1
    else if (['preparing', 'running'].includes(task.status)) counts.running = (counts.running ?? 0) + 1
    else if (task.status === 'succeeded') counts.succeeded += 1
    else if (['failed', 'canceled'].includes(task.status)) counts.failed += 1
    else if (task.status === 'skipped') counts.skipped = (counts.skipped ?? 0) + 1
  }

  const finished = ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(
    seed.status,
  )
  const submittedAt = iso(seed.submittedMinAgo)
  const run: Run = {
    id: runId,
    name: seed.name,
    kind: 'run',
    status: seed.status,
    cleanup_policy: seed.cleanupPolicy ?? 'always',
    submitted_by: seed.submittedBy,
    submitted_at: submittedAt,
    started_at: iso(Math.max(0, seed.submittedMinAgo - 0.2)),
    finished_at: finished ? iso(Math.max(0, seed.submittedMinAgo - (seed.durationSec ?? 0) / 60)) : null,
    endpoint: seed.endpoint ?? null,
    deploy: seed.deploy ?? null,
    error: seed.error ?? null,
    task_counts: counts,
    revision: 3,
    tasks: summaries,
  }
  state.runs.push(run)

  if (seed.deploy) {
    state.deploys[runId] = {
      run_id: runId,
      deploy: seed.deploy,
      manifest: seed.deployManifest ?? null,
      pods: clone(seed.pods ?? []),
      deploy_timeout_seconds: 259200,
    }
  } else {
    state.deploys[runId] = { run_id: runId, deploy: null, manifest: null, pods: [] }
  }

  // 事件历史：给首屏一条可读的时间线
  let seq = 0
  const push = (
    type: RunEventType,
    from: string | null,
    to: string | null,
    extra?: Partial<RunEvent>,
  ) => {
    seq += 1
    const events = state.events[runId] ?? []
    events.push({
      seq,
      type,
      run_id: runId,
      task_id: extra?.task_id ?? null,
      attempt_id: extra?.attempt_id ?? null,
      at: iso(Math.max(0, seed.submittedMinAgo - seq * 0.3)),
      from,
      to,
      reason: extra?.reason ?? null,
      payload: extra?.payload ?? null,
    })
    state.events[runId] = events
  }
  push('run.submitted', null, 'submitted', { payload: { task_count: summaries.length } })
  if (seed.deploy) push('deploy.state_changed', null, seed.deploy.state)
  for (const task of summaries) {
    if (task.status === 'pending') continue
    push('task.state_changed', 'pending', task.status, { task_id: task.id })
    const terminal = ['succeeded', 'failed', 'canceled'].includes(task.status)
    if (task.attempts > 0 && terminal) {
      push('attempt.finished', 'running', task.status, { task_id: task.id, attempt_id: `${task.id}-a${task.attempts}` })
    } else if (task.attempts > 0) {
      push('attempt.started', null, 'running', { task_id: task.id, attempt_id: `${task.id}-a${task.attempts}` })
    }
  }
  if (finished) push('run.finished', 'testing', seed.status)
  state.seq[runId] = seq

  return run
}

const PODS_SINGLE: DeployPod[] = [
  { name: 'genet-qwen3-32b-0', role: 'main', node: 'npu-01', namespace: 'testplane', phase: 'Running', ready: true, restarts: 0 },
]

const PODS_PD: DeployPod[] = [
  { name: 'genet-qwen3-32b-router-0', role: 'router', node: 'npu-01', namespace: 'testplane', phase: 'Running', ready: true, restarts: 0 },
  { name: 'genet-qwen3-32b-prefill-0', role: 'prefill', node: 'npu-01', namespace: 'testplane', phase: 'Running', ready: true, restarts: 0 },
  { name: 'genet-qwen3-32b-decode-0', role: 'decode', node: 'npu-02', namespace: 'testplane', phase: 'Running', ready: true, restarts: 1 },
  { name: 'genet-qwen3-32b-decode-1', role: 'decode', node: 'npu-03', namespace: 'testplane', phase: 'Running', ready: true, restarts: 0 },
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

  const model = 'Qwen3-32B'
  const depReady: Deploy = {
    provider: 'genet',
    state: 'ready',
    deployment_id: 'd-1001',
    queue_position: null,
    queue_reason: null,
    queued_at: iso(120),
    parameters_sha: 'sha256:9f2a',
  }

  // 1) 成功：4 个 Task 全绿 + PD 分离 4 Pod
  seedRun({
    id: 'r-20261105-0006',
    name: 'qwen3-32b-daily',
    status: 'succeeded',
    submittedBy: '林知微',
    submittedMinAgo: 96,
    durationSec: 1398,
    env: { LOG_LEVEL: 'info', HTTP_TIMEOUT: '300' },
    secrets: { ENDPOINT_API_KEY: 'endpoint/prod-qwen' },
    endpoint: endpointOf('http://10.0.0.10:8000/v1', model, 'genet', 'd-1001'),
    deploy: depReady,
    deployManifest: { model, replicas: 1, pd_disaggregation: true, npu_count: 4 },
    pods: PODS_PD,
    tasks: [
      { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', startedMinAgo: 92, durationSec: 41 },
      { seq: 1, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: 91, durationSec: 484 },
      { seq: 2, name: 'accuracy-dataset-b', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/mmlu.jsonl --limit=200', startedMinAgo: 91, durationSec: 411 },
      { seq: 3, name: 'performance-sweep', tester: 'aisbench-perf', status: 'succeeded', args: '--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]', startedMinAgo: 50, durationSec: 432 },
    ],
  })

  // 2) Deploy 排队中（queued；D-46：没有位次，只有 queue_reason 原文）
  seedRun({
    id: 'r-20261105-0005',
    name: 'qwen3-32b-canary',
    status: 'deploying',
    submittedBy: '陈一鸣',
    submittedMinAgo: 18,
    env: { LOG_LEVEL: 'info' },
    deploy: {
      provider: 'genet',
      state: 'queued',
      deployment_id: null,
      queue_position: null, // D-46：部署方没有位次，字段保留但恒 null
      queue_reason: 'PodGroup phase=Pending, reason=Unschedulable(0/8 nodes available)',
      queued_at: iso(18),
      parameters_sha: 'sha256:1a2b',
    },
    deployManifest: { model, replicas: 1, canary: true },
    pods: [],
    tasks: [
      { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'pending', waiting: 'precheck_gate' },
      { seq: 1, name: 'smoke-acc', tester: 'aisbench-acc', status: 'pending', waiting: 'precheck_gate', args: '--dataset=/data/gsm8k.jsonl --limit=20' },
    ],
  })

  // 3) 执行中（service 模式，live：mock 会推事件把 Task 一个个跑完）
  seedRun({
    id: 'r-20261105-0004',
    name: 'existing-service-smoke',
    status: 'testing',
    submittedBy: '林知微',
    submittedMinAgo: 4,
    endpoint: endpointOf('http://10.0.0.30:8000/v1', model, 'service'),
    tasks: [
      { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', startedMinAgo: 4, durationSec: 5 },
      { seq: 1, name: 'smoke-acc', tester: 'aisbench-acc', status: 'running', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: 3 },
      { seq: 2, name: 'perf-sweep', tester: 'aisbench-perf', status: 'queued', waiting: 'slot_waiting' },
    ],
  })

  // 4) 部分失败（1 个 FAILED）
  seedRun({
    id: 'r-20261105-0003',
    name: 'qwen3-32b-daily',
    status: 'completed_with_errors',
    submittedBy: '陈一鸣',
    submittedMinAgo: 240,
    durationSec: 1082,
    endpoint: endpointOf('http://10.0.0.10:8000/v1', model, 'genet', 'd-1002'),
    deploy: { ...depReady, deployment_id: 'd-1002', queued_at: iso(260) },
    deployManifest: { model, replicas: 1 },
    pods: PODS_SINGLE,
    tasks: [
      { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', startedMinAgo: 238, durationSec: 12 },
      {
        seq: 1,
        name: 'accuracy-dataset-a',
        tester: 'aisbench-acc',
        status: 'failed',
        args: '--dataset=/data/gsm8k.jsonl --limit=200',
        startedMinAgo: 236,
        durationSec: 460,
        error: { code: 'TESTER_NONZERO_EXIT', message: 'Tester 退出码 1（连接被对端重置）', retryable: true, detail: { exit_code: 1 } },
      },
      { seq: 2, name: 'accuracy-dataset-b', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/mmlu.jsonl --limit=200', startedMinAgo: 236, durationSec: 402 },
      { seq: 3, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', statusReason: 'upstream_abort' },
    ],
  })

  // 5) 服务不可用（Pre-Check 失败）
  seedRun({
    id: 'r-20261105-0002',
    name: 'qwen3-32b-canary',
    status: 'service_unavailable',
    submittedBy: '林知微',
    submittedMinAgo: 300,
    durationSec: 18060,
    endpoint: endpointOf('http://10.0.0.31:8000/v1', model, 'genet', 'd-1003'),
    deploy: { ...depReady, deployment_id: 'd-1003', queued_at: iso(330) },
    deployManifest: { model, replicas: 1, canary: true },
    pods: PODS_SINGLE,
    error: {
      code: 'PRECHECK_FAILED',
      message: 'Pre-Check 在 18000 秒窗口内未通过：服务持续返回 503',
      retryable: true,
      detail: { http_status: 503 },
    },
    tasks: [
      {
        seq: 0,
        name: 'precheck',
        tester: 'endpoint-probe',
        status: 'failed',
        startedMinAgo: 305,
        durationSec: 18000,
        error: { code: 'PRECHECK_FAILED', message: '等待窗口内服务始终未就绪', retryable: true, detail: { http_status: 503 } },
      },
      { seq: 1, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'skipped', statusReason: 'upstream_abort' },
      { seq: 2, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', statusReason: 'upstream_abort' },
    ],
  })

  // 6) 已取消
  seedRun({
    id: 'r-20261105-0001',
    name: 'qwen3-32b-acc-200',
    status: 'canceled',
    submittedBy: '陈一鸣',
    submittedMinAgo: 420,
    durationSec: 753,
    endpoint: endpointOf('http://10.0.0.11:8000/v1', model, 'service'),
    tasks: [
      { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', startedMinAgo: 419, durationSec: 4 },
      { seq: 1, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'canceled', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: 418, durationSec: 740 },
      { seq: 2, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', statusReason: 'canceled' },
    ],
  })

  // 7~10) 更早的成功记录（凑分页）
  const older: Array<[string, string, string, number]> = [
    ['r-20261104-0014', 'qwen3-8b-nightly', '林知微', 1400],
    ['r-20261104-0013', 'qwen3-32b-daily', '陈一鸣', 1500],
    ['r-20261104-0012', 'evalscope-mmlu', '林知微', 1620],
    ['r-20261104-0011', 'existing-service-smoke', 'dev', 1740],
  ]
  for (const [id, name, by, minAgo] of older) {
    seedRun({
      id,
      name,
      status: 'succeeded',
      submittedBy: by,
      submittedMinAgo: minAgo,
      durationSec: 900 + ((minAgo * 7) % 600),
      endpoint: endpointOf('http://10.0.0.20:8000/v1', model, 'service'),
      tasks: [
        { seq: 0, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', startedMinAgo: minAgo - 1, durationSec: 3 },
        { seq: 1, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: minAgo - 2, durationSec: 400 },
      ],
    })
  }
}

seedAll()

/* ================================================================ 事件推送 */

const listeners = new Map<string, Set<(e: RunEvent) => void>>()

function emit(runId: string, event: RunEvent): void {
  for (const listener of listeners.get(runId) ?? []) listener(event)
}

function pushEvent(
  runId: string,
  type: RunEventType,
  from: string | null,
  to: string | null,
  extra?: Partial<RunEvent>,
): RunEvent {
  const seq = (state.seq[runId] ?? 0) + 1
  state.seq[runId] = seq
  const event: RunEvent = {
    seq,
    type,
    run_id: runId,
    task_id: extra?.task_id ?? null,
    attempt_id: extra?.attempt_id ?? null,
    at: nowIso(),
    from,
    to,
    reason: extra?.reason ?? null,
    payload: extra?.payload ?? null,
  }
  const events = state.events[runId] ?? []
  events.push(event)
  state.events[runId] = events
  emit(runId, event)
  return event
}

function isTerminal(status: RunStatus): boolean {
  return ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(status)
}

const TASK_FLOW: TaskStatus[] = ['pending', 'queued', 'preparing', 'running', 'succeeded']
const DEPLOY_FLOW: Deploy['state'][] = ['queued', 'accepted', 'deploying', 'ready']

function recomputeCounts(run: Run): void {
  const counts = emptyCounts()
  for (const summary of run.tasks ?? []) {
    counts.total += 1
    if (['pending', 'queued'].includes(summary.status)) counts.pending = (counts.pending ?? 0) + 1
    else if (['preparing', 'running'].includes(summary.status)) counts.running = (counts.running ?? 0) + 1
    else if (summary.status === 'succeeded') counts.succeeded += 1
    else if (['failed', 'canceled'].includes(summary.status)) counts.failed += 1
    else if (summary.status === 'skipped') counts.skipped = (counts.skipped ?? 0) + 1
  }
  run.task_counts = counts
}

function syncSummary(task: TaskDetail): void {
  const run = state.runs.find((r) => r.id === task.run_id)
  const summary = run?.tasks?.find((t) => t.id === task.id)
  if (summary) {
    summary.status = task.status
    summary.attempts = task.current_attempt
    summary.waiting_reason = task.waiting_reason
    summary.error = task.error
  }
  if (run) recomputeCounts(run)
}

/** 推进一档 mock 状态（每 3.5s 一次，直到 Run 终态）。 */
function step(runId: string): void {
  const run = state.runs.find((r) => r.id === runId)
  if (!run || isTerminal(run.status)) {
    stopLive(runId)
    return
  }
  const snapshot = state.deploys[runId]

  // Deploy 阶段优先推进
  if (run.status === 'deploying' && run.deploy && snapshot) {
    const index = DEPLOY_FLOW.indexOf(run.deploy.state)
    if (index >= 0 && index < DEPLOY_FLOW.length - 1) {
      const from = run.deploy.state
      const to = DEPLOY_FLOW[index + 1] ?? 'ready'
      run.deploy.state = to
      if (to === 'ready') {
        run.status = 'testing'
        run.endpoint = endpointOf('http://10.0.1.20:8000/v1', 'Qwen3-32B', 'genet', run.deploy.deployment_id ?? undefined)
        snapshot.pods = PODS_SINGLE.map((pod) => ({ ...pod, name: `genet-${run.name}-0` }))
      }
      pushEvent(runId, 'deploy.state_changed', from, to)
      return
    }
    run.status = 'testing'
    pushEvent(runId, 'run.state_changed', 'deploying', 'testing')
    return
  }

  // Task 阶段：每次推进第一个未终态的 Task
  const task = Object.values(state.tasks)
    .filter((t) => t.run_id === runId)
    .sort((a, b) => a.seq - b.seq)
    .find((t) => !['succeeded', 'failed', 'skipped', 'canceled'].includes(t.status))

  if (task) {
    const from = task.status
    const index = TASK_FLOW.indexOf(task.status)
    const to = TASK_FLOW[Math.min(index + 1, TASK_FLOW.length - 1)] ?? 'running'
    task.status = to
    if (to === 'queued') task.waiting_reason = null
    if (to === 'running') {
      task.current_attempt += 1
      const attempt: Attempt = {
        id: `${task.id}-a${task.current_attempt}`,
        task_id: task.id,
        idx: task.current_attempt,
        status: 'running',
        phase: 'Running',
        exit_code: null,
        image_digest: 'sha256:mock',
        started_at: nowIso(),
        finished_at: null,
        log_ref: `tasks/${task.id}/attempts/${task.current_attempt}/stdout.log`,
        error: null,
        executor_ref: { cluster: 'prod-npu-01', namespace: 'testplane', job: `tp-${task.id}` },
      }
      task.attempts = [...task.attempts, attempt]
      pushEvent(runId, 'attempt.started', null, 'running', { task_id: task.id, attempt_id: attempt.id })
    }
    if (to === 'succeeded') {
      const attempt = task.attempts[task.attempts.length - 1]
      if (attempt) {
        attempt.status = 'succeeded'
        attempt.exit_code = 0
        attempt.finished_at = nowIso()
      }
      state.logs[`${task.id}#${task.current_attempt}`] = `[10:14:26] mock 运行结束\n结论：status=succeeded`
      pushEvent(runId, 'attempt.finished', 'running', 'succeeded', {
        task_id: task.id,
        attempt_id: attempt?.id ?? null,
      })
    }
    syncSummary(task)
    pushEvent(runId, 'task.state_changed', from, to, { task_id: task.id })
    return
  }

  // 所有 Task 收口 → 清理 → 终态
  if (run.status !== 'cleanup') {
    run.status = 'cleanup'
    pushEvent(runId, 'cleanup.started', 'testing', 'cleanup')
    return
  }
  const failed = (run.tasks ?? []).some((t) => t.status === 'failed')
  run.status = failed ? 'completed_with_errors' : 'succeeded'
  run.finished_at = nowIso()
  recomputeCounts(run)
  pushEvent(runId, 'run.finished', 'cleanup', run.status)
  stopLive(runId)
}

function startLive(runId: string): void {
  if (state.timers.has(runId)) return
  const run = state.runs.find((r) => r.id === runId)
  if (!run || isTerminal(run.status)) return
  state.timers.set(
    runId,
    setInterval(() => step(runId), 3500),
  )
}

function stopLive(runId: string): void {
  const timer = state.timers.get(runId)
  if (timer !== undefined) {
    clearInterval(timer)
    state.timers.delete(runId)
  }
}

/** mock 版事件流：先回放历史，再按状态机持续推送。返回取消函数。 */
export function subscribeMockEvents(
  runId: string,
  opts: {
    sinceSeq?: number
    onEvent: (event: RunEvent) => void
    onOpen?: () => void
    onError?: (error: unknown) => void
  },
): () => void {
  const run = state.runs.find((r) => r.id === runId)
  if (!run) {
    opts.onError?.(new ApiClientError(404, { code: 'NOT_FOUND', message: 'Run 不存在', retryable: false }))
    return () => {}
  }

  const bucket = listeners.get(runId) ?? new Set()
  listeners.set(runId, bucket)
  bucket.add(opts.onEvent)

  const since = opts.sinceSeq ?? 0
  setTimeout(() => {
    opts.onOpen?.()
    for (const event of state.events[runId] ?? []) {
      if (event.seq > since) opts.onEvent(event)
    }
    startLive(runId)
  }, 60)

  return () => {
    bucket.delete(opts.onEvent)
    if (bucket.size === 0) {
      listeners.delete(runId)
      stopLive(runId)
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

function findRun(id: string): Run {
  const run = state.runs.find((r) => r.id === id)
  if (!run) notFound(`Run ${id}`)
  return run
}

function findTask(id: string): TaskDetail {
  const task = state.tasks[id]
  if (!task) notFound(`Task ${id}`)
  return task
}

let runCounter = 20
function nextRunId(): string {
  runCounter += 1
  const d = new Date()
  const ymd = `${d.getFullYear()}${String(d.getMonth() + 1).padStart(2, '0')}${String(d.getDate()).padStart(2, '0')}`
  return `r-${ymd}-${String(runCounter).padStart(4, '0')}`
}

function summarizeRun(run: Run): Run {
  const copy = clone(run)
  delete copy.tasks
  return copy
}

const PRE_CHECK_NAMES = new Set(['precheck'])

/** Manifest 校验：只做平台明确会做的几条（形态互斥、Tester 存在、Tester 版本、公共参数冲突）。 */
function validateManifest(manifest: RunManifest): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const body = manifest.run
  if (!body) {
    issues.push({ path: 'run', code: 'VALIDATION_ERROR', message: '缺少 run 段' })
    return issues
  }
  if (!body.name?.trim()) issues.push({ path: 'run.name', code: 'VALIDATION_ERROR', message: 'Run 名称不能为空' })
  if (body.deploy && body.service) {
    issues.push({ path: 'run', code: 'VALIDATION_ERROR', message: 'deploy 与 service 二选一，不能都给' })
  }
  if (!body.deploy && !body.service) {
    issues.push({ path: 'run.service', code: 'VALIDATION_ERROR', message: 'deploy 与 service 必须给其一' })
  }
  if (body.deploy && !body.deploy.provider) {
    issues.push({ path: 'run.deploy.provider', code: 'VALIDATION_ERROR', message: 'deploy 缺少 provider' })
  }
  if (body.service && !body.service.url?.trim()) {
    issues.push({ path: 'run.service.url', code: 'VALIDATION_ERROR', message: 'service.url 必填' })
  }
  if (!body.tests || body.tests.length === 0) {
    issues.push({ path: 'run.tests', code: 'VALIDATION_ERROR', message: '至少需要一个 Task' })
  }
  if ((body.tests?.length ?? 0) > 200) {
    issues.push({ path: 'run.tests', code: 'RUN_TOO_LARGE', message: '单个 Run 最多 200 个 Task' })
  }
  body.tests?.forEach((test, index) => {
    // TestSpec.name 契约必填：空名字必须报校验错误，否则 dry-run 会「假通过」
    if (!test.name?.trim()) {
      issues.push({ path: `tests[${index}].name`, code: 'VALIDATION_ERROR', message: 'Task 名称不能为空' })
    }
    if (!test.tester) {
      issues.push({ path: `tests[${index}].tester`, code: 'VALIDATION_ERROR', message: '必须选择 Tester' })
      return
    }
    const tester = state.testers.find((t) => t.name === test.tester)
    if (!tester) {
      issues.push({ path: `tests[${index}].tester`, code: 'TESTER_NOT_FOUND', message: `目录里没有 Tester「${test.tester}」` })
    } else if (test.version && test.version !== tester.version) {
      issues.push({
        path: `tests[${index}].version`,
        code: 'TESTER_VERSION_PINNED',
        message: `Tester ${test.tester} 只有版本 ${tester.version}`,
      })
    }
    if (!test.execution?.profile) {
      issues.push({ path: `tests[${index}].execution.profile`, code: 'VALIDATION_ERROR', message: '必须指定执行环境 profile' })
    }
    const args = test.args ?? ''
    for (const flag of ['--url', '--model_name', '--model_path']) {
      if (new RegExp(`(^|\\s)${flag}[=\\s]`).test(args)) {
        issues.push({
          path: `tests[${index}].args`,
          code: 'PUBLIC_PARAM_CONFLICT',
          message: `${flag} 由平台注入，args 中不得出现`,
        })
      }
    }
  })
  return issues
}

/* ================================================================ 测试钩子 */

/**
 * mock 的角色注入（只用于本地验收 / Playwright）：
 * `localStorage['testplane:mock-role'] = 'viewer' | 'operator' | 'admin'`，或 URL 上 `?mock_role=viewer`。
 * 注入后会带一个假的 `csrf_token`，让前端按 OIDC 会话处理（viewer 的写按钮才会置灰）。
 */
function mockRoleOverride(): string | null {
  try {
    const stored = localStorage.getItem('testplane:mock-role')
    if (stored === 'viewer' || stored === 'operator' || stored === 'admin') return stored
  } catch {
    /* 隐私模式忽略 */
  }
  try {
    const fromQuery = new URLSearchParams(window.location.search).get('mock_role')
    if (fromQuery === 'viewer' || fromQuery === 'operator' || fromQuery === 'admin') return fromQuery
  } catch {
    /* ignore */
  }
  return null
}

export const mockApi: Api = {
  async getIdentity(): Promise<Identity> {
    await delay(80)
    // 默认 dev 模式（无 csrf_token → 不跳登录）。
    // QA / Playwright 需要验角色时，用 `localStorage['testplane:mock-role']='viewer'`
    // 或 URL 上的 `?mock_role=viewer` 注入 OIDC 角色（会带一个假 csrf_token）。
    const override = mockRoleOverride()
    if (override) {
      const identity: Identity = {
        user: `mock-${override}`,
        role: override,
        csrf_token: 'mock-csrf-token',
        groups: [`testplane-${override}`],
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
        deploy: true,
        dry_run: true,
        events_history: true,
        events_sse: true,
        queue_status: true,
        report_md: true,
        standalone_task: true,
      },
    }
  },

  async listRuns(query: RunListQuery = {}): Promise<Page<Run>> {
    await delay()
    const statuses = query.status?.split(',').filter(Boolean) ?? []
    const filtered = state.runs
      .filter((r) => (statuses.length === 0 ? true : statuses.includes(r.status)))
      .filter((r) => (query.kind ? r.kind === query.kind : true))
      .filter((r) => (query.submitter ? r.submitted_by === query.submitter : true))
      .slice()
      .sort((a, b) => b.submitted_at.localeCompare(a.submitted_at))
      .map(summarizeRun)
    return paginate(filtered, query.cursor, query.limit ?? 50)
  },

  async getRun(id: string): Promise<Run> {
    await delay()
    return clone(findRun(id))
  },

  async cancelRun(id: string): Promise<CancelResult> {
    await delay()
    const run = findRun(id)
    if (isTerminal(run.status)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: 'Run 已经是终态，无法取消', retryable: false })
    }
    run.status = 'cancelling'
    pushEvent(id, 'run.state_changed', 'testing', 'cancelling')
    setTimeout(() => {
      run.status = 'canceled'
      run.finished_at = nowIso()
      for (const summary of run.tasks ?? []) {
        const task = state.tasks[summary.id]
        if (!task) continue
        if (['pending', 'queued'].includes(task.status)) {
          task.status = 'skipped'
          task.status_reason = 'canceled'
        } else if (!['succeeded', 'failed', 'skipped', 'canceled'].includes(task.status)) {
          task.status = 'canceled'
        }
        syncSummary(task)
      }
      recomputeCounts(run)
      pushEvent(id, 'run.finished', 'cancelling', 'canceled')
      stopLive(id)
    }, 1200)
    return { run_id: id, status: 'cancelling' }
  },

  async listTasks(query: TaskListQuery = {}): Promise<Page<Task>> {
    await delay()
    const statuses = query.status?.split(',').filter(Boolean) ?? []
    const all = Object.values(state.tasks)
      .filter((t) => (statuses.length === 0 ? true : statuses.includes(t.status)))
      .filter((t) => (query.tester ? t.tester_name === query.tester : true))
      .sort((a, b) => a.run_id.localeCompare(b.run_id) || a.seq - b.seq)
      .map((t) => {
        const copy = clone(t)
        delete (copy as Partial<TaskDetail>).attempts
        return copy as Task
      })
    return paginate(all, query.cursor, query.limit ?? 50)
  },

  async getTask(id: string): Promise<TaskDetail> {
    await delay(90)
    return clone(findTask(id))
  },

  async cancelTask(id: string): Promise<TaskCancelResult> {
    await delay()
    const task = findTask(id)
    if (['succeeded', 'failed', 'skipped', 'canceled'].includes(task.status)) {
      throw new ApiClientError(409, { code: 'CONFLICT', message: 'Task 已经是终态，无法取消', retryable: false })
    }
    task.status = 'canceled'
    task.cancel_requested = true
    syncSummary(task)
    pushEvent(task.run_id, 'task.state_changed', 'running', 'canceled', { task_id: task.id })
    return { task_id: task.id, run_id: task.run_id, status: 'canceled' }
  },

  async listAttempts(taskId: string): Promise<Attempt[]> {
    await delay(80)
    return clone(findTask(taskId).attempts.sort((a, b) => a.idx - b.idx))
  },

  async getAttemptLogs(taskId: string, idx: number, _opts: LogOptions = {}): Promise<LogChunk> {
    await delay(90)
    findTask(taskId)
    const text = state.logs[`${taskId}#${idx}`] ?? '（该 Attempt 还没有日志）'
    return { text, offset: text.length, coverage: null }
  },

  async getDeploy(runId: string): Promise<DeploySnapshot> {
    await delay(90)
    findRun(runId)
    return clone(state.deploys[runId] ?? { run_id: runId, deploy: null, manifest: null, pods: [] })
  },

  async getEventsHistory(runId: string, sinceSeq = 0): Promise<RunEvent[]> {
    await delay(60)
    return clone((state.events[runId] ?? []).filter((e) => e.seq > sinceSeq))
  },

  async listTesters(query: TesterListQuery = {}): Promise<Tester[]> {
    await delay(90)
    return state.testers
      .filter((t) => (query.role ? t.role === query.role : true))
      .filter((t) => (query.orphaned === undefined ? true : Boolean(t.orphaned) === query.orphaned))
      .map((t) => normalizeTester(t))
  },

  async getTester(name: string): Promise<Tester> {
    await delay(80)
    const tester = state.testers.find((t) => t.name === name)
    if (!tester) notFound(`Tester ${name}`)
    return normalizeTester(tester)
  },

  async listClusters(): Promise<Cluster[]> {
    await delay(80)
    return clone(state.clusters)
  },

  async getQueueStatus(): Promise<QueueStatus> {
    await delay(90)
    const pendingTasks: WaitingTask[] = []
    for (const run of state.runs) {
      if (isTerminal(run.status)) continue
      for (const summary of run.tasks ?? []) {
        if (!['pending', 'queued'].includes(summary.status)) continue
        pendingTasks.push({
          task_id: summary.id,
          run_id: run.id,
          name: summary.name,
          status: summary.status,
          waiting_reason: summary.waiting_reason ?? 'slot_waiting',
          since: run.submitted_at,
        })
      }
    }
    const byReason = {
      precheck_gate: 0,
      slot_waiting: 0,
      global_limit: 0,
      queue_full: 0,
      insufficient_resources: 0,
      gang_pending: 0,
    }
    for (const task of pendingTasks) byReason[task.waiting_reason] += 1

    const active = state.runs.filter((r) => ['testing', 'deploying'].includes(r.status)).length
    return {
      generated_at: nowIso(),
      slots: {
        concurrency_max: 20,
        active,
        available: Math.max(0, 20 - active),
        endpoints: [],
      },
      waiting: { total: pendingTasks.length, by_reason: byReason, tasks: pendingTasks },
      volcano: {
        available: true,
        // stale 在快照层：本次采样成功 → false（不可达时才置位，且那时 queues 恒为 []）
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

  async submitRun(manifest: RunManifest): Promise<SubmitResult> {
    await delay(260)
    const issues = validateManifest(manifest)
    if (issues.length > 0) {
      throw new ApiClientError(400, {
        code: 'VALIDATION_ERROR',
        message: issues.map((i) => `${i.path}: ${i.message}`).join('；'),
        detail: { issues },
        retryable: false,
      })
    }

    const body = manifest.run
    const runId = nextRunId()
    const submittedMinAgo = 0
    const tasks: TaskSeed[] = []

    const precheck = body.precheck ?? 'auto'
    if (precheck !== 'off') {
      const name = precheck === 'auto' ? 'precheck' : precheck.tester
      tasks.push({
        seq: 0,
        name,
        tester: precheck === 'auto' ? (state.testers.find((t) => t.role === 'pre_check')?.name ?? 'endpoint-probe') : precheck.tester,
        status: 'pending',
        waiting: 'precheck_gate',
      })
    }
    body.tests?.forEach((test, index) => {
      tasks.push({
        seq: precheck === 'off' ? index : index + 1,
        name: test.name,
        tester: test.tester,
        status: 'pending',
        args: test.args,
        waiting: body.deploy ? 'precheck_gate' : 'slot_waiting',
      })
    })

    const deploy: Deploy | undefined = body.deploy
      ? {
          provider: body.deploy.provider,
          state: 'queued',
          deployment_id: null,
          queue_position: null, // D-46：没有位次
          queue_reason: 'PodGroup phase=Pending, reason=Unschedulable(0/8 nodes available)',
          queued_at: nowIso(),
          parameters_sha: 'sha256:mock',
        }
      : undefined

    const run = seedRun({
      id: runId,
      name: body.name,
      status: 'submitted',
      submittedBy: getCurrentUser(),
      submittedMinAgo,
      env: body.env,
      secrets: body.secrets,
      cleanupPolicy: body.cleanup?.policy ?? 'always',
      deploy,
      deployManifest: body.deploy?.parameters ?? null,
      endpoint: body.service
        ? endpointOf(body.service.url, body.service.model_name ?? 'Qwen3-32B', 'service')
        : null,
      tasks,
    })

    // 后台推进：让「提交 → 详情页 → 事件流追加」在 mock 下也能完整演一遍
    setTimeout(() => {
      const created = state.runs.find((r) => r.id === runId)
      if (!created) return
      created.status = 'queued'
      pushEvent(runId, 'run.state_changed', 'submitted', 'queued')
      setTimeout(() => {
        created.status = created.deploy ? 'deploying' : 'testing'
        pushEvent(runId, 'run.state_changed', 'queued', created.status)
        startLive(runId)
      }, 1800)
    }, 1200)

    return {
      run: { id: run.id, status: run.status, kind: run.kind, submitted_at: run.submitted_at },
      tasks: (run.tasks ?? []).map((t) => ({ id: t.id, name: t.name, status: t.status })),
    }
  },

  async dryRun(manifest: RunManifest): Promise<DryRunResult> {
    await delay(220)
    const issues = validateManifest(manifest)
    return {
      valid: issues.length === 0,
      issues,
      resolved_preview: {
        precheck: manifest.run?.precheck === 'off' ? 'off' : { tester: 'endpoint-probe', seq: 0 },
        task_count: manifest.run?.tests?.length ?? 0,
        skipped_precheck_names: [...PRE_CHECK_NAMES],
      },
    }
  },

  async getDashboardStats(): Promise<DashboardStats> {
    await delay(120)
    const runs = state.runs
    const active = runs.filter((r) => !isTerminal(r.status))
    const terminal = runs.filter((r) => isTerminal(r.status))
    const succeeded = terminal.filter((r) => r.status === 'succeeded').length
    const queuedTasks = runs
      .filter((r) => !isTerminal(r.status))
      .reduce((sum, r) => sum + (r.task_counts?.pending ?? 0), 0)
    const dayAgo = Date.now() - 24 * 3600 * 1000
    const failed24h = runs.filter(
      (r) =>
        ['failed', 'service_unavailable', 'completed_with_errors'].includes(r.status) &&
        new Date(r.submitted_at).getTime() >= dayAgo,
    ).length
    return {
      active_runs: active.length,
      queued_tasks: queuedTasks,
      failed_24h: failed24h,
      success_rate: terminal.length === 0 ? null : succeeded / terminal.length,
      total_runs: runs.length,
      tester_count: state.testers.length,
    }
  },
}
