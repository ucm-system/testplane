/**
 * Mock 数据源 —— 后端未起时把页面填满（`VITE_USE_MOCK=1`）。
 *
 * 规则：
 *  - 这里实现的是 `Api` 接口，字段与 07-api.md 完全一致；切真实接口时页面不改一行。
 *  - 数据是有状态的：提交 Run 会真的出现在列表里，取消会走 cancelling → canceled，
 *    事件流（SSE）由 mock 自己按状态机推 —— 用来验收「事件流实时追加」这一条。
 *  - 数据贴近 design/prototype/screenshots：同一批名字、同一批状态、同一套日志口径。
 */
import { ApiClientError, getCurrentUser } from './client'
import type { Api, LogOptions } from './contract'
import type {
  Attempt,
  Cluster,
  DashboardStats,
  DeployPod,
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
  RunEventType,
  RunListQuery,
  RunManifest,
  RunStatus,
  RunSummary,
  SubmitResult,
  TaskListQuery,
  TaskStatus,
  TaskSummary,
  Tester,
  TesterListQuery,
  ValidationIssue,
} from './types'

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

/* ================================================================ Tester 目录 */

const TESTERS: Tester[] = [
  {
    name: 'endpoint-probe',
    version: '1.0.0',
    display_name: '服务就绪探测',
    description:
      '对已部署的 LLM 服务执行生成型探活：POST <url>/chat/completions，max_tokens=1、stream=true，首包 token 一到即判定可服务。只回答「服务此刻能不能出字」。',
    role: 'pre_check',
    on_failure: 'abort_run',
    exclusive: true,
    image: 'registry.dev.huawei.com/testplane/endpoint-probe:1.0.0',
    entry: '/tester/run.sh',
    placeholder: '--wait_timeout_seconds=600 --retry_interval_seconds=15',
    parameters: [
      { name: 'url', type: 'string', required: true, description: 'OpenAI 兼容 base URL；平台有值时注入，不得重复传', public: true },
      { name: 'model_name', type: 'string', required: false, description: '模型名；省略时从 /v1/models 取第一个', public: true },
      { name: 'wait_timeout_seconds', type: 'integer', required: false, description: '就绪等待窗口（秒）', default: '600' },
      { name: 'retry_interval_seconds', type: 'integer', required: false, description: '两次探测间隔（秒）', default: '15' },
      { name: 'max_tokens', type: 'integer', required: false, description: '探活请求的 max_tokens，不要调大', default: '1' },
      { name: 'stream', type: 'boolean', required: false, description: '流式探活；true 时首 token 到达即通过', default: 'true' },
    ],
    env: [
      { name: 'ENDPOINT_API_KEY', required: false, secret: true, description: 'Bearer token；不提供时按无鉴权探测' },
    ],
  },
  {
    name: 'aisbench-acc',
    version: '1.0.0',
    display_name: '数据集精度评测',
    description: '数据集精度评测，输出逐数据集 accuracy。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    image: 'registry.dev.huawei.com/testplane/aisbench-acc:1.0.0',
    entry: '/tester/run.sh',
    placeholder: '--dataset=/data/gsm8k.jsonl --limit=200',
    parameters: [
      { name: 'url', type: 'string', required: true, description: 'OpenAI 兼容 base URL', public: true },
      { name: 'model_name', type: 'string', required: true, description: '模型名', public: true },
      { name: 'dataset', type: 'string', required: true, description: '数据集在容器内的绝对路径，由挂载提供' },
      { name: 'limit', type: 'integer', required: false, description: '最多评测多少条样本' },
    ],
    env: [{ name: 'ENDPOINT_API_KEY', required: false, secret: true, description: 'Bearer token' }],
  },
  {
    name: 'aisbench-perf',
    version: '1.0.0',
    display_name: '性能压测',
    description: '性能压测，输出 TTFT / TPOT / 吞吐。独占 Endpoint，避免与其它 Task 互相干扰。',
    role: 'test',
    on_failure: 'continue',
    exclusive: true,
    image: 'registry.dev.huawei.com/testplane/aisbench-perf:1.0.0',
    entry: '/tester/run.sh',
    placeholder: '--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]',
    parameters: [
      { name: 'url', type: 'string', required: true, description: 'OpenAI 兼容 base URL', public: true },
      { name: 'model_name', type: 'string', required: true, description: '模型名', public: true },
      { name: 'input_lens', type: 'list<int>', required: true, description: '输入长度列表' },
      { name: 'output_lens', type: 'list<int>', required: true, description: '输出长度列表' },
      { name: 'concurrency', type: 'list<int>', required: true, description: '并发档位' },
    ],
    env: [{ name: 'ENDPOINT_API_KEY', required: false, secret: true, description: 'Bearer token' }],
  },
  {
    name: 'evalscope-acc',
    version: '2.3.0',
    display_name: 'EvalScope 精度评测',
    description: 'EvalScope 精度评测，适合跑多 benchmark 组合。',
    role: 'test',
    on_failure: 'continue',
    exclusive: false,
    image: 'registry.dev.huawei.com/testplane/evalscope-acc:2.3.0',
    entry: '/tester/run.sh',
    placeholder: '--dataset=mmlu --subset=abstract_algebra --limit=100',
    parameters: [
      { name: 'url', type: 'string', required: true, description: 'OpenAI 兼容 base URL', public: true },
      { name: 'model_name', type: 'string', required: true, description: '模型名', public: true },
      { name: 'dataset', type: 'string', required: true, description: 'benchmark 名称' },
      { name: 'subset', type: 'string', required: false, description: '子集' },
      { name: 'limit', type: 'integer', required: false, description: '样本上限' },
    ],
    env: [{ name: 'ENDPOINT_API_KEY', required: false, secret: true, description: 'Bearer token' }],
  },
]

/* ================================================================ 内部状态 */

interface LogStore {
  /** `${taskId}#${attemptIdx}` → 文本 */
  attempt: Record<string, string>
  /** `${runId}#${pod}` → 文本 */
  deploy: Record<string, string>
}

interface MockState {
  runs: RunDetail[]
  attempts: Record<string, Attempt[]>
  logs: LogStore
  pods: Record<string, DeployPod[]>
  events: Record<string, RunEvent[]>
  seq: Record<string, number>
  testers: Tester[]
  clusters: Cluster[]
  /** 正在跑的 live 定时器（runId → interval id） */
  timers: Map<string, ReturnType<typeof setInterval>>
}

const state: MockState = {
  runs: [],
  attempts: {},
  logs: { attempt: {}, deploy: {} },
  pods: {},
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
  waiting?: TaskSummary['waiting_reason']
  error?: TaskSummary['error']
  durationSec?: number
  startedMinAgo?: number
  note?: string
  extraAttempts?: Attempt[]
}

function buildAttempts(taskId: string, seed: TaskSeed): Attempt[] {
  const startedAt = seed.startedMinAgo == null ? null : iso(seed.startedMinAgo)
  const finished = seed.startedMinAgo != null && seed.durationSec != null
  const base: Attempt = {
    idx: 1,
    status:
      seed.status === 'succeeded'
        ? 'succeeded'
        : seed.status === 'failed'
          ? 'failed'
          : seed.status === 'canceled'
            ? 'canceled'
            : seed.status === 'running'
              ? 'running'
              : seed.status === 'preparing'
                ? 'preparing'
                : 'running',
    exit_code: seed.status === 'succeeded' ? 0 : seed.status === 'failed' ? 1 : null,
    started_at: startedAt,
    finished_at: finished ? new Date(T0 - (seed.startedMinAgo! - seed.durationSec! / 60) * 60_000).toISOString() : null,
    duration_seconds: seed.durationSec ?? null,
    error: seed.error ?? null,
    image_digest: `sha256:${taskId.replace(/[^a-z0-9]/gi, '').slice(0, 12)}d1e7`,
    note: seed.note ?? null,
  }
  if (seed.status === 'pending' || seed.status === 'queued' || seed.status === 'skipped') return []
  return [base, ...(seed.extraAttempts ?? [])]
}

function attemptLog(task: MockSeedTask, attempt: Attempt): string {
  const t = attempt.started_at ? new Date(attempt.started_at).toLocaleTimeString('zh-CN', { hour12: false }) : '--:--:--'
  const lines: string[] = []
  if (task.tester === 'endpoint-probe') {
    lines.push(
      `[${t}] 探活 <endpoint>/chat/completions model_name=Qwen3-32B max_tokens=1 stream=true`,
      `[${t}] 第 1 次探测：未就绪 —— HTTP 503 {"error":"model loading"}`,
      `[${t}] 第 2 次探测：HTTP 200，收到 SSE 数据帧，服务可出 token`,
      `结论：status=succeeded（等待 38s）`,
    )
  } else if (attempt.status === 'failed') {
    lines.push(
      `[${t}] loading dataset ${task.args ?? ''}`,
      `[${t}] ERROR connection reset by peer (attempt=1)`,
      `[${t}] Traceback (most recent call last):`,
      `[${t}]   File "/tester/run.sh", line 42, in <module>`,
      `[${t}] requests.exceptions.ConnectionError: ('Connection aborted.', RemoteDisconnected())`,
      `结论：status=failed exit=1 code=TESTER_NONZERO_EXIT`,
    )
  } else if (task.tester === 'aisbench-perf') {
    lines.push(
      `[${t}] [sweep] input_lens=[512,1024,4096] output_lens=[128,512] concurrency=[1,4,8,16]`,
      `[${t}] [sweep] 24 combos, 24 ok`,
      `[${t}] ttft_ms p95=143.7  tpot_ms mean=21.4  throughput=1482.6 tokens/s`,
    )
  } else {
    lines.push(
      `[${t}] loading dataset ${task.args ?? ''}`,
      `[${t}] 200 samples done`,
      `[${t}] accuracy=0.8421`,
    )
  }
  return lines.join('\n')
}

interface MockSeedTask extends TaskSeed {
  exclusive?: boolean
  on_failure?: TaskSummary['on_failure']
}

interface RunSeed {
  id: string
  name: string
  status: RunStatus
  submitted_by: string
  submitted_min_ago: number
  duration_sec?: number | null
  env?: Record<string, string>
  secrets?: string[]
  endpoint?: { url: string; model_name: string } | null
  deploy?: DeploySnapshot | null
  pods?: DeployPod[]
  tasks: MockSeedTask[]
  events?: Array<{ type: RunEventType; task_id?: string; text: string }>
  error?: RunSummary['error']
  taskCountOverride?: { total: number; succeeded: number; failed: number }
}

function mkTask(runId: string, seed: MockSeedTask): TaskSummary {
  const id = `t-${runId.slice(2)}-${String(seed.seq).padStart(2, '0')}`
  const attempts = buildAttempts(id, seed)
  return {
    id,
    run_id: runId,
    seq: seed.seq,
    name: seed.name,
    status: seed.status,
    tester: seed.tester,
    version: '1.0.0',
    exclusive: seed.exclusive ?? false,
    on_failure: seed.on_failure ?? 'continue',
    waiting_reason: seed.waiting ?? null,
    attempts: attempts.length,
    attempt: attempts.length > 0 ? attempts.length : null,
    started_at: attempts[0]?.started_at ?? null,
    finished_at: attempts[0]?.finished_at ?? null,
    duration_seconds: seed.durationSec ?? null,
    error: seed.error ?? null,
    args: seed.args ?? null,
    image_ref: `registry.dev.huawei.com/testplane/${seed.tester}:1.0.0`,
  }
}

function seedRun(seed: RunSeed): RunDetail {
  const tasks = seed.tasks.map((t) => mkTask(seed.id, t))
  const finished = ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(
    seed.status,
  )
  const counts = seed.taskCountOverride ?? {
    total: tasks.length,
    succeeded: tasks.filter((t) => t.status === 'succeeded').length,
    failed: tasks.filter((t) => t.status === 'failed').length,
  }
  const run: RunDetail = {
    id: seed.id,
    name: seed.name,
    kind: 'run',
    status: seed.status,
    submitted_by: seed.submitted_by,
    submitted_at: iso(seed.submitted_min_ago),
    finished_at: finished ? iso(Math.max(0, seed.submitted_min_ago - (seed.duration_sec ?? 0) / 60)) : null,
    endpoint: seed.endpoint
      ? { url: seed.endpoint.url, model_name: seed.endpoint.model_name, run_id: seed.id }
      : null,
    task_counts: { ...counts, total: counts.total },
    duration_seconds: seed.duration_sec ?? null,
    error: seed.error ?? null,
    cleanup_policy: 'always',
    deploy: seed.deploy ? clone(seed.deploy) : null,
    tasks,
    env: seed.env ?? { LOG_LEVEL: 'info' },
    secrets: seed.secrets ?? [],
  }

  state.runs.push(run)
  state.pods[seed.id] = seed.pods ? clone(seed.pods) : []

  // 事件历史：先灌"提交/状态变化"骨架，保证 Run 详情首屏就有时间线
  const events: RunEvent[] = []
  let seq = 0
  const push = (type: RunEventType, text: string, taskId?: string, payload?: Record<string, unknown>) => {
    seq += 1
    events.push({
      seq,
      type,
      run_id: seed.id,
      task_id: taskId ?? null,
      at: iso(Math.max(0, seed.submitted_min_ago - seq * 0.3)),
      payload: { ...(payload ?? {}), text },
    })
  }
  push('run.submitted', `Run 提交，${tasks.length} 个 Task`)
  if (seed.deploy) {
    push('deploy.state_changed', `Deploy 进入 ${seed.deploy.state}`, undefined, {
      from: null,
      to: seed.deploy.state,
    })
  }
  for (const t of tasks) {
    if (t.status === 'pending' || t.status === 'queued') continue
    push('task.state_changed', `Task「${t.name}」${t.status}`, t.id, {
      from: 'pending',
      to: t.status,
    })
    if (t.attempts > 0) {
      push('attempt.finished', `Attempt #${t.attempts} 结束（${t.status}）`, t.id)
    }
  }
  if (finished) push('run.finished', 'Run 终态', undefined, { to: seed.status })

  state.events[seed.id] = events
  state.seq[seed.id] = seq

  // Attempt 与日志
  seed.tasks.forEach((taskSeed, i) => {
    const task = tasks[i]
    if (!task) return
    state.attempts[task.id] = buildAttempts(task.id, taskSeed)
    for (const attempt of state.attempts[task.id] ?? []) {
      state.logs.attempt[`${task.id}#${attempt.idx}`] = attemptLog(taskSeed, attempt)
    }
  })

  // Deploy 日志
  for (const pod of seed.pods ?? []) {
    state.logs.deploy[`${seed.id}#${pod.name}`] = podLog(pod)
  }

  return run
}

function podLog(pod: DeployPod): string {
  const from = pod.log_from ?? '10:03:12'
  return [
    `[${from}] loading weights`,
    `[${from}] engine ready`,
    `[${from}] [req] serving, tokens/s=612`,
    `[${from}] [req] 132/200 samples, qps=4`,
  ].join('\n')
}

/* ---------------------------------------------------------------- 种子数据 */

function podsFor(model: string, variant: 'single' | 'pd'): DeployPod[] {
  if (variant === 'single') {
    return [
      {
        name: `${model}-0`,
        role: 'all-in-one',
        status: 'Running',
        node: 'gpu-02',
        restarts: 0,
        log_from: '10:03:12',
        log_to: '10:11:40',
      },
    ]
  }
  return [
    { name: `${model}-router-0`, role: 'router', status: 'Running', node: 'npu-01', restarts: 0, log_from: '09:37:58', log_to: '09:57:20' },
    { name: `${model}-prefill-0`, role: 'prefill', status: 'Running', node: 'npu-01', restarts: 0, log_from: '09:38:20', log_to: '09:57:22' },
    { name: `${model}-decode-0`, role: 'decode', status: 'Running', node: 'npu-02', restarts: 1, log_from: '09:38:31', log_to: '09:57:25' },
    { name: `${model}-decode-1`, role: 'decode', status: 'Running', node: 'npu-03', restarts: 0, log_from: '09:38:33', log_to: '09:57:25' },
  ]
}

function deployYaml(model: string, extra = ''): string {
  return `provider: genet
parameters:
  model: ${model}
  replicas: 1${extra}`
}

function seedAll(): void {
  // 1) 成功：4 个 Task 全绿（对齐 run-detail.png）
  seedRun({
    id: 'r-20261105-0031',
    name: 'qwen3-32b-daily',
    status: 'succeeded',
    submitted_by: '林知微',
    submitted_min_ago: 96,
    duration_sec: 1398,
    env: { LOG_LEVEL: 'info', HTTP_TIMEOUT: '300' },
    secrets: ['endpoint/prod-qwen'],
    endpoint: { url: 'http://10.0.0.10:8000/v1', model_name: 'Qwen3-32B' },
    deploy: {
      state: 'ready',
      provider: 'genet',
      queued_at: iso(110),
      started_at: iso(105),
      ready_at: iso(102),
      duration_seconds: 192,
      endpoint: { url: 'http://10.0.0.10:8000/v1', model_name: 'Qwen3-32B' },
      pods: podsFor('qwen3-32b', 'pd'),
      manifest_yaml: deployYaml('Qwen3-32B', '\n  pd_disaggregation: true\n  npu_count: 4'),
    },
    pods: podsFor('qwen3-32b', 'pd'),
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', exclusive: true, on_failure: 'abort_run', durationSec: 41, startedMinAgo: 92, note: '第 3 次探测通过' },
      { seq: 2, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/gsm8k.jsonl --limit=200', durationSec: 484, startedMinAgo: 91 },
      { seq: 3, name: 'accuracy-dataset-b', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/mmlu.jsonl --limit=200', durationSec: 411, startedMinAgo: 91 },
      { seq: 4, name: 'performance-sweep', tester: 'aisbench-perf', status: 'succeeded', exclusive: true, args: '--input_lens=[512,1024,4096] --output_lens=[128,512] --concurrency=[1,4,8,16]', durationSec: 432, startedMinAgo: 50 },
    ],
  })

  // 2) Deploy 排队中：把 mock 的 queued/deploying 两个形态显示出来
  seedRun({
    id: 'r-20261105-0047',
    name: 'qwen3-32b-canary',
    status: 'deploying',
    submitted_by: '陈一鸣',
    submitted_min_ago: 18,
    env: { LOG_LEVEL: 'info' },
    deploy: {
      state: 'queued',
      provider: 'genet',
      queue_position: 12,
      queued_at: iso(18),
      pods: [],
      manifest_yaml: deployYaml('Qwen3-32B', '\n  canary: true'),
    },
    pods: [],
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'pending', waiting: 'precheck_gate', exclusive: true, on_failure: 'abort_run' },
      { seq: 2, name: 'smoke-acc', tester: 'aisbench-acc', status: 'pending', waiting: 'precheck_gate', args: '--dataset=/data/gsm8k.jsonl --limit=20' },
    ],
  })

  // 3) Deploy 部署中（pods 已产生、尚未 ready）
  seedRun({
    id: 'r-20261105-0046',
    name: 'qwen3-8b-nightly',
    status: 'deploying',
    submitted_by: '陈一鸣',
    submitted_min_ago: 9,
    deploy: {
      state: 'deploying',
      provider: 'genet',
      queued_at: iso(9),
      started_at: iso(6),
      pods: [
        { name: 'qwen3-8b-0', role: 'all-in-one', status: 'Pending', node: 'gpu-02', restarts: 0, log_from: '10:03:12' },
      ],
      manifest_yaml: deployYaml('Qwen3-8B'),
    },
    pods: [{ name: 'qwen3-8b-0', role: 'all-in-one', status: 'Pending', node: 'gpu-02', restarts: 0, log_from: '10:03:12' }],
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'pending', waiting: 'precheck_gate', exclusive: true, on_failure: 'abort_run' },
      { seq: 2, name: 'smoke-acc', tester: 'aisbench-acc', status: 'pending', waiting: 'precheck_gate' },
    ],
  })

  // 4) 执行中（live：mock 会推事件把 Task 一个个跑完）
  seedRun({
    id: 'r-20261105-0045',
    name: 'existing-service-smoke',
    status: 'testing',
    submitted_by: '林知微',
    submitted_min_ago: 4,
    endpoint: { url: 'http://10.0.0.30:8000/v1', model_name: 'Qwen3-32B' },
    deploy: null,
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', exclusive: true, on_failure: 'abort_run', durationSec: 5, startedMinAgo: 4 },
      { seq: 2, name: 'smoke-acc', tester: 'aisbench-acc', status: 'running', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: 3 },
      { seq: 3, name: 'perf-sweep', tester: 'aisbench-perf', status: 'pending', exclusive: true },
    ],
  })

  // 5) 部分失败
  seedRun({
    id: 'r-20261105-0044',
    name: 'qwen3-32b-daily',
    status: 'completed_with_errors',
    submitted_by: '陈一鸣',
    submitted_min_ago: 240,
    duration_sec: 1082,
    endpoint: { url: 'http://10.0.0.10:8000/v1', model_name: 'Qwen3-32B' },
    deploy: {
      state: 'ready',
      provider: 'genet',
      queued_at: iso(260),
      started_at: iso(255),
      ready_at: iso(253),
      duration_seconds: 164,
      endpoint: { url: 'http://10.0.0.10:8000/v1', model_name: 'Qwen3-32B' },
      pods: podsFor('qwen3-32b', 'single'),
      manifest_yaml: deployYaml('Qwen3-32B'),
    },
    pods: podsFor('qwen3-32b', 'single'),
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', exclusive: true, on_failure: 'abort_run', durationSec: 12, startedMinAgo: 238 },
      {
        seq: 2,
        name: 'accuracy-dataset-a',
        tester: 'aisbench-acc',
        status: 'failed',
        args: '--dataset=/data/gsm8k.jsonl --limit=200',
        startedMinAgo: 236,
        durationSec: 460,
        error: { code: 'TESTER_NONZERO_EXIT', message: 'Tester 退出码 1（连接被对端重置）', retryable: true, detail: { exit_code: 1 } },
      },
      { seq: 3, name: 'accuracy-dataset-b', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/mmlu.jsonl --limit=200', startedMinAgo: 236, durationSec: 402 },
      { seq: 4, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', exclusive: true, args: '--input_lens=[512,1024] --output_lens=[128] --concurrency=[1,4]' },
    ],
  })

  // 6) 服务不可用（Pre-Check 失败）
  seedRun({
    id: 'r-20261105-0043',
    name: 'qwen3-32b-canary',
    status: 'service_unavailable',
    submitted_by: '林知微',
    submitted_min_ago: 300,
    duration_sec: 18060,
    endpoint: { url: 'http://10.0.0.31:8000/v1', model_name: 'Qwen3-32B' },
    deploy: {
      state: 'ready',
      provider: 'genet',
      queued_at: iso(330),
      started_at: iso(320),
      ready_at: iso(310),
      duration_seconds: 1200,
      endpoint: { url: 'http://10.0.0.31:8000/v1', model_name: 'Qwen3-32B' },
      pods: podsFor('qwen3-32b-canary', 'pd'),
      manifest_yaml: deployYaml('Qwen3-32B', '\n  canary: true'),
    },
    pods: podsFor('qwen3-32b-canary', 'pd'),
    error: {
      code: 'PRECHECK_FAILED',
      message: 'Pre-Check 在 18000 秒窗口内未通过：服务持续返回 503',
      retryable: true,
      detail: { http_status: 503, precheck_attempts: 41 },
    },
    tasks: [
      {
        seq: 1,
        name: 'precheck',
        tester: 'endpoint-probe',
        status: 'failed',
        exclusive: true,
        on_failure: 'abort_run',
        startedMinAgo: 305,
        durationSec: 18000,
        error: { code: 'PRECHECK_FAILED', message: '等待窗口内服务始终未就绪', retryable: true, detail: { http_status: 503 } },
      },
      { seq: 2, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'skipped' },
      { seq: 3, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', exclusive: true },
    ],
  })

  // 7) 已取消
  seedRun({
    id: 'r-20261105-0042',
    name: 'qwen3-32b-acc-200',
    status: 'canceled',
    submitted_by: '陈一鸣',
    submitted_min_ago: 420,
    duration_sec: 753,
    endpoint: { url: 'http://10.0.0.11:8000/v1', model_name: 'Qwen3-32B' },
    deploy: null,
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', exclusive: true, on_failure: 'abort_run', durationSec: 4, startedMinAgo: 419 },
      { seq: 2, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'canceled', args: '--dataset=/data/gsm8k.jsonl --limit=200', startedMinAgo: 418, durationSec: 740 },
      { seq: 3, name: 'performance-sweep', tester: 'aisbench-perf', status: 'skipped', exclusive: true },
    ],
  })

  // 8) 平台失败
  seedRun({
    id: 'r-20261105-0041',
    name: 'gpu-shared-eval',
    status: 'failed',
    submitted_by: 'dev',
    submitted_min_ago: 500,
    duration_sec: 120,
    error: { code: 'EXECUTOR_UNAVAILABLE', message: '集群 gpu-shared 的 API Server 连续 3 次不可达', retryable: true },
    tasks: [
      { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'failed', exclusive: true, on_failure: 'abort_run', error: { code: 'EXECUTOR_UNAVAILABLE', message: '创建 Job 失败：cluster unreachable', retryable: true } },
      { seq: 2, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'skipped' },
    ],
  })

  // 9~13) 更早的成功记录（凑出分页）
  const older: Array<[string, string, string, number]> = [
    ['r-20261104-0038', 'qwen3-8b-nightly', '林知微', 1400],
    ['r-20261104-0037', 'qwen3-32b-daily', '陈一鸣', 1500],
    ['r-20261104-0036', 'evalscope-mmlu', '林知微', 1620],
    ['r-20261104-0035', 'existing-service-smoke', 'dev', 1740],
    ['r-20261104-0034', 'qwen3-32b-daily', '陈一鸣', 1860],
  ]
  for (const [id, name, by, minAgo] of older) {
    seedRun({
      id,
      name,
      status: 'succeeded',
      submitted_by: by,
      submitted_min_ago: minAgo,
      duration_sec: 900 + Math.round(Math.random() * 600),
      endpoint: { url: 'http://10.0.0.20:8000/v1', model_name: 'Qwen3-32B' },
      deploy: null,
      tasks: [
        { seq: 1, name: 'precheck', tester: 'endpoint-probe', status: 'succeeded', exclusive: true, on_failure: 'abort_run', durationSec: 3, startedMinAgo: minAgo - 1 },
        { seq: 2, name: 'accuracy-dataset-a', tester: 'aisbench-acc', status: 'succeeded', args: '--dataset=/data/gsm8k.jsonl --limit=200', durationSec: 400, startedMinAgo: minAgo - 2 },
      ],
    })
  }

  // 集群（`GET /clusters` 的数据源；凭据不入 mock）
  state.clusters = [
    {
      name: 'prod-npu-01',
      namespace: 'testplane',
      is_default: true,
      healthy: true,
      last_checked_at: iso(2),
      node_count: 8,
      kubeconfig_key: 'prod-npu-01.kubeconfig',
      message: 'Volcano 调度可用',
    },
    {
      name: 'prod-gpu-02',
      namespace: 'testplane',
      is_default: false,
      healthy: true,
      last_checked_at: iso(4),
      node_count: 4,
      kubeconfig_key: 'prod-gpu-02.kubeconfig',
      message: 'Volcano 调度可用',
    },
    {
      name: 'sandbox-npu-03',
      namespace: 'testplane-sandbox',
      is_default: false,
      healthy: false,
      last_checked_at: iso(37),
      node_count: 2,
      kubeconfig_key: 'sandbox-npu-03.kubeconfig',
      message: 'API Server 连续 3 次不可达，新 Task 不会调度到该集群',
    },
  ]
}

seedAll()

/* ================================================================ 事件推送 */

function pushEvent(runId: string, type: RunEvent['type'], text: string, taskId?: string, payload?: Record<string, unknown>): RunEvent {
  const seq = (state.seq[runId] ?? 0) + 1
  state.seq[runId] = seq
  const event: RunEvent = {
    seq,
    type,
    run_id: runId,
    task_id: taskId ?? null,
    at: nowIso(),
    payload: { ...(payload ?? {}), text },
  }
  state.events[runId] = [...(state.events[runId] ?? []), event]
  return event
}

const listeners = new Map<string, Set<(e: RunEvent) => void>>()

function emit(runId: string, event: RunEvent): void {
  for (const listener of listeners.get(runId) ?? []) listener(event)
}

function isTerminal(status: RunStatus): boolean {
  return ['succeeded', 'completed_with_errors', 'service_unavailable', 'failed', 'canceled'].includes(status)
}

const TASK_FLOW: TaskStatus[] = ['pending', 'queued', 'preparing', 'running', 'succeeded']
const DEPLOY_FLOW: DeploySnapshot['state'][] = ['queued', 'accepted', 'deploying', 'ready']

/** 推进一档 mock 状态（每 3.5s 调一次，直到 Run 终态）。 */
function step(runId: string): void {
  const run = state.runs.find((r) => r.id === runId)
  if (!run || isTerminal(run.status)) {
    stopLive(runId)
    return
  }

  // Deploy 阶段优先推进
  if (run.status === 'deploying' && run.deploy) {
    const idx = DEPLOY_FLOW.indexOf(run.deploy.state)
    if (idx >= 0 && idx < DEPLOY_FLOW.length - 1) {
      const from = run.deploy.state
      const to = DEPLOY_FLOW[idx + 1] ?? 'ready'
      run.deploy.state = to
      if (to === 'accepted') run.deploy.started_at = nowIso()
      if (to === 'ready') {
        run.deploy.ready_at = nowIso()
        run.deploy.duration_seconds = 180
        run.status = 'testing'
        run.endpoint = { url: 'http://10.0.1.20:8000/v1', model_name: 'Qwen3-32B', run_id: run.id }
        run.deploy.endpoint = run.endpoint
        run.deploy.pods = [
          { name: `${run.name}-0`, role: 'all-in-one', status: 'Running', node: 'npu-01', restarts: 0, log_from: '10:03:12' },
        ]
        state.pods[run.id] = clone(run.deploy.pods)
        state.logs.deploy[`${run.id}#${run.name}-0`] = podLog(run.deploy.pods[0] as DeployPod)
      }
      emit(runId, pushEvent(runId, 'deploy.state_changed', `Deploy ${from} → ${to}`, undefined, { from, to }))
      return
    }
    run.status = 'testing'
    emit(runId, pushEvent(runId, 'run.state_changed', 'Run deploying → testing', undefined, { from: 'deploying', to: 'testing' }))
    return
  }

  // Task 阶段：每次推进第一个未终态的 Task
  const task = run.tasks.find((t) => !['succeeded', 'failed', 'skipped', 'canceled'].includes(t.status))
  if (task) {
    const idx = TASK_FLOW.indexOf(task.status)
    const from = task.status
    const to = TASK_FLOW[Math.min(idx + 1, TASK_FLOW.length - 1)] ?? 'running'
    task.status = to
    if (to === 'queued') task.waiting_reason = null
    if (to === 'running') {
      task.started_at = nowIso()
      task.attempts = Math.max(task.attempts, 1)
      task.attempt = 1
      state.attempts[task.id] = [
        {
          idx: 1,
          status: 'running',
          exit_code: null,
          started_at: nowIso(),
          finished_at: null,
          duration_seconds: null,
          image_digest: 'sha256:mock',
        },
      ]
      emit(runId, pushEvent(runId, 'attempt.started', `Task「${task.name}」开始 Attempt #1`, task.id, { attempt: 1 }))
    }
    if (to === 'succeeded') {
      task.finished_at = nowIso()
      task.duration_seconds = 42
      const attempt = state.attempts[task.id]?.[0]
      if (attempt) {
        attempt.status = 'succeeded'
        attempt.exit_code = 0
        attempt.finished_at = nowIso()
        attempt.duration_seconds = 42
      }
      state.logs.attempt[`${task.id}#1`] = `[10:14:26] mock 运行结束\n结论：status=succeeded`
      emit(runId, pushEvent(runId, 'attempt.finished', `Task「${task.name}」Attempt #1 succeeded`, task.id, { attempt: 1 }))
    }
    emit(runId, pushEvent(runId, 'task.state_changed', `Task「${task.name}」${from} → ${to}`, task.id, { from, to }))
    refreshCounts(run)
    return
  }

  // 所有 Task 收口 → 清理 → 终态
  if (run.status !== 'cleanup') {
    run.status = 'cleanup'
    emit(runId, pushEvent(runId, 'cleanup.started', '所有 Task 结束，开始清理'))
    return
  }
  const failed = run.tasks.some((t) => t.status === 'failed')
  run.status = failed ? 'completed_with_errors' : 'succeeded'
  run.finished_at = nowIso()
  run.duration_seconds = 300
  refreshCounts(run)
  emit(runId, pushEvent(runId, 'run.finished', `Run 终态 ${run.status}`, undefined, { to: run.status }))
  stopLive(runId)
}

function refreshCounts(run: RunDetail): void {
  run.task_counts = {
    total: run.tasks.length,
    succeeded: run.tasks.filter((t) => t.status === 'succeeded').length,
    failed: run.tasks.filter((t) => t.status === 'failed').length,
    running: run.tasks.filter((t) => t.status === 'running').length,
    pending: run.tasks.filter((t) => ['pending', 'queued'].includes(t.status)).length,
  }
}

function startLive(runId: string): void {
  if (state.timers.has(runId)) return
  const run = state.runs.find((r) => r.id === runId)
  if (!run || isTerminal(run.status)) return
  const timer = setInterval(() => step(runId), 3500)
  state.timers.set(runId, timer)
}

function stopLive(runId: string): void {
  const timer = state.timers.get(runId)
  if (timer !== undefined) {
    clearInterval(timer)
    state.timers.delete(runId)
  }
}

/**
 * mock 版事件流：先按 seq 回放历史，再按状态机持续推送。
 * 返回取消函数（对应真实 SSE 的 abort）。
 */
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
    opts.onError?.(
      new ApiClientError(404, { code: 'NOT_FOUND', message: 'Run 不存在', retryable: false }),
    )
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
    if (bucket.size === 0) listeners.delete(runId)
    if ((listeners.get(runId)?.size ?? 0) === 0) stopLive(runId)
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

function summarize(run: RunDetail): RunSummary {
  return {
    id: run.id,
    name: run.name,
    kind: run.kind,
    status: run.status,
    submitted_by: run.submitted_by,
    submitted_at: run.submitted_at,
    finished_at: run.finished_at ?? null,
    endpoint: run.endpoint ? clone(run.endpoint) : null,
    task_counts: run.task_counts ? clone(run.task_counts) : null,
    duration_seconds: run.duration_seconds ?? null,
    error: run.error ? clone(run.error) : null,
  }
}

function findRun(id: string): RunDetail {
  const run = state.runs.find((r) => r.id === id)
  if (!run) notFound(`Run ${id}`)
  return run
}

function findTask(taskId: string): { run: RunDetail; task: TaskSummary } {
  for (const run of state.runs) {
    const task = run.tasks.find((t) => t.id === taskId)
    if (task) return { run, task }
  }
  notFound(`Task ${taskId}`)
}

let runCounter = 100
function nextRunId(): string {
  runCounter += 1
  const d = new Date()
  const ymd = `${d.getFullYear()}${String(d.getMonth() + 1).padStart(2, '0')}${String(d.getDate()).padStart(2, '0')}`
  return `r-${ymd}-${String(runCounter).padStart(4, '0')}`
}

function makeTaskFromManifest(
  runId: string,
  seq: number,
  name: string,
  tester: string,
  args: string | undefined,
  status: TaskStatus,
  waiting: TaskSummary['waiting_reason'],
  onFailure: TaskSummary['on_failure'],
  exclusive: boolean,
): TaskSummary {
  return {
    id: `t-${runId.slice(2)}-${String(seq).padStart(2, '0')}`,
    run_id: runId,
    seq,
    name,
    status,
    tester,
    version: '1.0.0',
    exclusive,
    on_failure: onFailure,
    waiting_reason: waiting,
    attempts: 0,
    attempt: null,
    started_at: null,
    finished_at: null,
    duration_seconds: null,
    error: null,
    args: args ?? null,
    image_ref: `registry.dev.huawei.com/testplane/${tester}:1.0.0`,
  }
}

/** Manifest 校验：只做平台会做的那几条（公共参数冲突、Tester 存在、形态互斥）。 */
function validateManifest(manifest: RunManifest): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const run = manifest.run
  if (!run) {
    issues.push({ path: 'run', code: 'VALIDATION_ERROR', message: '缺少 run 段' })
    return issues
  }
  if (!run.name?.trim()) {
    issues.push({ path: 'run.name', code: 'VALIDATION_ERROR', message: 'Run 名称不能为空' })
  }
  if (!run.tests || run.tests.length === 0) {
    issues.push({ path: 'run.tests', code: 'VALIDATION_ERROR', message: '至少需要一个 Task' })
  }
  if (run.deploy && run.service) {
    issues.push({
      path: 'run',
      code: 'PUBLIC_PARAM_CONFLICT',
      message: '「部署」与「已有服务」两种形态互斥，只能选一个',
    })
  }
  if (!run.deploy && !run.service) {
    issues.push({
      path: 'run.service.url',
      code: 'VALIDATION_ERROR',
      message: '不部署时必须提供服务 base URL（service.url）',
    })
  }
  if (run.tests.length > 200) {
    issues.push({ path: 'run.tests', code: 'RUN_TOO_LARGE', message: '单个 Run 最多 200 个 Task' })
  }
  run.tests?.forEach((test, index) => {
    if (!test.tester) {
      issues.push({ path: `tests[${index}].tester`, code: 'VALIDATION_ERROR', message: '必须选择 Tester' })
    } else if (!state.testers.some((t) => t.name === test.tester)) {
      issues.push({
        path: `tests[${index}].tester`,
        code: 'TESTER_NOT_FOUND',
        message: `目录里没有 Tester「${test.tester}」`,
      })
    }
    const args = test.args ?? ''
    if (/(^|\s)--url[=\s]/.test(args)) {
      issues.push({
        path: `tests[${index}].args`,
        code: 'PUBLIC_PARAM_CONFLICT',
        message: '--url 由平台注入，args 中不得出现',
      })
    }
    if (/(^|\s)--model_name[=\s]/.test(args)) {
      issues.push({
        path: `tests[${index}].args`,
        code: 'PUBLIC_PARAM_CONFLICT',
        message: '--model_name 由平台注入，args 中不得出现',
      })
    }
  })
  return issues
}

export const mockApi: Api = {
  async getIdentity(): Promise<Identity> {
    await delay(80)
    return { user: getCurrentUser(), role: 'operator', groups: ['testplane-operator'] }
  },

  async getMeta(): Promise<Meta> {
    await delay(40)
    return { version: 'v0.0.0-mock', api_version: 'v1', features: { board: false, tester_register: false } }
  },

  async listRuns(query: RunListQuery = {}): Promise<Page<RunSummary>> {
    await delay()
    const statuses = query.status?.split(',').filter(Boolean) ?? []
    const filtered = [...state.runs]
      .filter((r) => (statuses.length === 0 ? true : statuses.includes(r.status)))
      .filter((r) => (query.kind ? r.kind === query.kind : true))
      .filter((r) => (query.submitter ? r.submitted_by === query.submitter : true))
      .sort((a, b) => b.submitted_at.localeCompare(a.submitted_at))
      .map(summarize)
    return paginate(filtered, query.cursor, query.limit ?? 50)
  },

  async getRun(id: string): Promise<RunDetail> {
    await delay()
    return clone(findRun(id))
  },

  async cancelRun(id: string): Promise<void> {
    await delay()
    const run = findRun(id)
    if (isTerminal(run.status)) {
      throw new ApiClientError(409, {
        code: 'CONFLICT',
        message: 'Run 已经是终态，无法取消',
        retryable: false,
      })
    }
    run.status = 'cancelling'
    emit(id, pushEvent(id, 'run.state_changed', '收到取消请求：cancelling', undefined, { from: run.status, to: 'cancelling' }))
    setTimeout(() => {
      run.status = 'canceled'
      run.finished_at = nowIso()
      for (const task of run.tasks) {
        if (['pending', 'queued'].includes(task.status)) task.status = 'skipped'
        else if (!['succeeded', 'failed', 'skipped', 'canceled'].includes(task.status)) task.status = 'canceled'
      }
      refreshCounts(run)
      emit(id, pushEvent(id, 'run.finished', 'Run 终态 canceled', undefined, { to: 'canceled' }))
      stopLive(id)
    }, 1200)
  },

  async cancelTask(id: string): Promise<void> {
    await delay()
    const { run, task } = findTask(id)
    if (['succeeded', 'failed', 'skipped', 'canceled'].includes(task.status)) {
      throw new ApiClientError(409, {
        code: 'CONFLICT',
        message: 'Task 已经是终态，无法取消',
        retryable: false,
      })
    }
    task.status = 'canceled'
    task.finished_at = nowIso()
    refreshCounts(run)
    emit(run.id, pushEvent(run.id, 'task.state_changed', `Task「${task.name}」被取消`, task.id, { to: 'canceled' }))
  },
  async listTasks(query: TaskListQuery = {}): Promise<Page<TaskSummary>> {
    await delay()
    const statuses = query.status?.split(',').filter(Boolean) ?? []
    const all = state.runs.flatMap((r) => r.tasks)
    const filtered = all
      .filter((t) => (statuses.length === 0 ? true : statuses.includes(t.status)))
      .filter((t) => (query.tester ? t.tester === query.tester : true))
    return paginate(clone(filtered), query.cursor, query.limit ?? 50)
  },

  async listAttempts(taskId: string): Promise<Attempt[]> {
    await delay(90)
    return clone(state.attempts[taskId] ?? [])
  },

  async getAttemptLogs(taskId: string, idx: number, _opts: LogOptions = {}): Promise<LogChunk> {
    await delay(90)
    findTask(taskId)
    const text = state.logs.attempt[`${taskId}#${idx}`] ?? '（该 Attempt 还没有日志）'
    return { text, offset: text.length, coverage: null }
  },

  async getDeploy(runId: string): Promise<DeploySnapshot> {
    await delay(90)
    const run = findRun(runId)
    if (!run.deploy) {
      throw new ApiClientError(404, {
        code: 'NOT_FOUND',
        message: '该 Run 没有 Deploy 阶段（提交时选了「不部署」）',
        retryable: false,
      })
    }
    return clone(run.deploy)
  },

  async getDeployLogs(runId: string, pod: string, _opts: LogOptions = {}): Promise<LogChunk> {
    await delay(90)
    const text = state.logs.deploy[`${runId}#${pod}`] ?? '（该 Pod 还没有落盘日志）'
    return { text, offset: text.length, coverage: null }
  },

  async getEventsHistory(runId: string, sinceSeq = 0): Promise<RunEvent[]> {
    await delay(90)
    return clone((state.events[runId] ?? []).filter((e) => e.seq > sinceSeq))
  },

  async listTesters(query: TesterListQuery = {}): Promise<Tester[]> {
    await delay(90)
    return clone(
      state.testers.filter((t) => (query.role ? t.role === query.role : true)),
    )
  },

  async getTester(name: string): Promise<Tester> {
    await delay(80)
    const tester = state.testers.find((t) => t.name === name)
    if (!tester) notFound(`Tester ${name}`)
    return clone(tester)
  },

  async listClusters(): Promise<Cluster[]> {
    await delay(80)
    return clone(state.clusters)
  },

  async getQueueStatus(): Promise<QueueStatus> {
    await delay(90)
    const running = state.runs.filter((r) => ['testing', 'deploying'].includes(r.status))
    const queues: QueueEntry[] = [
      { name: 'npu-batch', state: 'open', deserved: 32, allocated: 32, pending: 6, longest_wait_seconds: 1080, reason: 'queue_full' },
      { name: 'npu-interactive', state: 'open', deserved: 8, allocated: 3, pending: 1, longest_wait_seconds: 120, reason: 'insufficient_resources' },
      { name: 'gpu-shared', state: 'open', deserved: 16, allocated: 16, pending: 4, longest_wait_seconds: 2460, reason: 'gang_pending' },
    ]
    const pendingTasks: NonNullable<QueueStatus['pending_tasks']> = state.runs
      .filter((r) => !isTerminal(r.status))
      .flatMap((r) =>
        r.tasks
          .filter((t) => ['pending', 'queued'].includes(t.status))
          .map((t) => ({
            task_id: t.id,
            name: t.name,
            run_id: r.id,
            waiting_reason: t.waiting_reason ?? 'slot_waiting',
            waited_seconds: 60,
          })),
      )
    return {
      slots: { used: running.length, total: 20, queue_depth: pendingTasks.length },
      queues,
      pending_tasks: pendingTasks,
      updated_at: nowIso(),
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

    const runId = nextRunId()
    const run = manifest.run
    const tasks: TaskSummary[] = []
    let seq = 0
    const precheckTester = state.testers.find((t) => t.role === 'pre_check')
    if (run.precheck !== 'off' && precheckTester) {
      seq += 1
      tasks.push(
        makeTaskFromManifest(
          runId,
          seq,
          'precheck',
          precheckTester.name,
          undefined,
          'pending',
          run.deploy ? 'precheck_gate' : undefined,
          'abort_run',
          true,
        ),
      )
    }
    for (const test of run.tests) {
      const tester = state.testers.find((t) => t.name === test.tester)
      seq += 1
      tasks.push(
        makeTaskFromManifest(
          runId,
          seq,
          test.name,
          test.tester,
          test.args,
          'pending',
          run.deploy ? 'precheck_gate' : 'slot_waiting',
          test.on_failure ?? tester?.on_failure ?? 'continue',
          test.exclusive ?? tester?.exclusive ?? false,
        ),
      )
    }

    const detail: RunDetail = {
      id: runId,
      name: run.name,
      kind: 'run',
      status: 'submitted',
      submitted_by: getCurrentUser(),
      submitted_at: nowIso(),
      finished_at: null,
      endpoint: run.service
        ? { url: run.service.url, model_name: run.service.model_name ?? null, run_id: runId }
        : null,
      task_counts: { total: tasks.length, succeeded: 0, failed: 0, pending: tasks.length },
      duration_seconds: null,
      error: null,
      cleanup_policy: run.cleanup?.policy ?? 'always',
      deploy: run.deploy
        ? {
            state: 'queued',
            provider: run.deploy.provider,
            queue_position: 8,
            queued_at: nowIso(),
            pods: [],
            manifest_yaml: `provider: ${run.deploy.provider}\nparameters:\n${Object.entries(run.deploy.parameters ?? {})
              .map(([k, v]) => `  ${k}: ${String(v)}`)
              .join('\n')}`,
          }
        : null,
      tasks,
      env: run.env ?? {},
      secrets: Object.keys(run.secrets ?? {}),
    }

    state.runs.push(detail)
    state.pods[runId] = []
    for (const task of tasks) state.attempts[task.id] = []
    state.events[runId] = []
    state.seq[runId] = 0
    emit(runId, pushEvent(runId, 'run.submitted', `Run 提交，${tasks.length} 个 Task`))

    // 后台推进：让「提交 → 详情页 → 事件流追加」在 mock 下也能完整演一遍
    setTimeout(() => {
      const created = state.runs.find((r) => r.id === runId)
      if (!created) return
      created.status = 'queued'
      emit(runId, pushEvent(runId, 'run.state_changed', 'Run submitted → queued', undefined, { from: 'submitted', to: 'queued' }))
      setTimeout(() => {
        created.status = created.deploy ? 'deploying' : 'testing'
        emit(
          runId,
          pushEvent(runId, 'run.state_changed', `Run queued → ${created.status}`, undefined, { from: 'queued', to: created.status }),
        )
        startLive(runId)
      }, 1800)
    }, 1200)

    return {
      run: summarize(detail),
      tasks: tasks.map((t) => ({ id: t.id, name: t.name, status: t.status })),
    }
  },

  async dryRun(manifest: RunManifest): Promise<DryRunResult> {
    await delay(220)
    const issues = validateManifest(manifest)
    return {
      valid: issues.length === 0,
      issues,
      resolved_preview: {
        precheck: manifest.run?.precheck === 'off' ? 'off' : 'auto',
        task_count: manifest.run?.tests?.length ?? 0,
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
