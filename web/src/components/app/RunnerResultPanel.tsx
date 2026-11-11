import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowUpRight, Download } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Artifact, RunnerExecution } from '@/api/types'
import { MetricsTable } from '@/components/app/MetricsTable'
import { RunnerStatusBadge } from '@/components/app/StatusBadge'
import { Badge, Tag } from '@/components/ui/badge'
import { buttonClassName } from '@/components/ui/button'
import { parseCsv } from '@/components/app/ArtifactViewer'
import { saveBlob } from '@/lib/download'
import { formatDuration } from '@/lib/utils'
import { cn } from '@/lib/utils'

/**
 * 单个 runner 的结果内联渲染（结果视图的主体，用户 2026-11-11：
 * 「结果页应该渲染结论 + result.json/html/csv 的渲染 + 产物下载」）。
 *
 * 产物三选一（D-74）按 runner 各自的实际产出分发：
 *  - result.json  → 附录 A schema 2.0（status + metrics 表，复用 MetricsTable）；
 *  - result.csv   → 通用表格（RFC4180，复用 ArtifactViewer 的 parseCsv）；
 *  - result.html  → sandbox iframe（不带 allow-scripts，§11.4）；
 *  - 都没有        → 说明占位（deploy/clean 角色本来就没有 result.json）。
 *
 * 每行给「查看」（整页查看器，URL 可收藏）与「下载」；runner 失败的默认展开。
 */

/** 该 runner 在产物清单里的三个结果文件（三选一） */
function resultArtifactsOf(items: Artifact[], seq: number): { json?: Artifact; csv?: Artifact; html?: Artifact } {
  const mine = items.filter((item) => item.runner_seq === seq && /\/result\.(json|csv|html)$/i.test(item.path))
  const pick = (ext: string) =>
    mine.find((item) => item.path.toLowerCase().endsWith(`/result.${ext}`)) ??
    mine.find((item) => item.path.toLowerCase().endsWith(`result.${ext}`))
  return { json: pick('json'), csv: pick('csv'), html: pick('html') }
}

function runnerDuration(runner: RunnerExecution): string {
  if (!runner.started_at) return '—'
  const start = new Date(runner.started_at).getTime()
  const end = runner.finished_at ? new Date(runner.finished_at).getTime() : Date.now()
  return formatDuration(Math.max(0, Math.round((end - start) / 1000)))
}

/** result.json 分支（附录 A）：状态 + 指标表 + notes/error */
function JsonResultBody({ taskId, runner, attemptNo }: { taskId: string; runner: RunnerExecution; attemptNo: number }) {
  const result = useAsyncResult(() => api.getRunnerResult(taskId, runner.seq, { attempt: attemptNo }), [taskId, runner.seq, attemptNo])

  if (result.loading && !result.data) return <p className="py-2 text-2xs text-fg-3">读取 result.json…</p>
  if (result.error) {
    return (
      <p className="py-2 text-2xs text-danger-text">
        result.json 读取失败（{result.error.code}）：{result.error.message}
      </p>
    )
  }
  if (!result.data) return null
  const data = result.data
  return (
    <div className="flex flex-col gap-2">
      {data.error ? (
        <div className="flex flex-wrap items-center gap-2 rounded-md border border-danger/25 bg-danger-weak px-3 py-2 text-2xs">
          <Badge tone="err" className="font-mono">
            {data.error.code}
          </Badge>
          <span className="text-fg">{data.error.message}</span>
        </div>
      ) : null}
      {data.metrics.length > 0 ? (
        <MetricsTable result={data} />
      ) : (
        <p className="text-2xs text-fg-3">该 runner 没有上报指标（纯探测/登记类结果）。</p>
      )}
      {data.notes ? <p className="text-2xs leading-[16px] text-fg-3">notes：{data.notes}</p> : null}
    </div>
  )
}

/* 极小 hook：避免本文件里重复 useAsync 样板（结果面板按需取数） */
function useAsyncResult<T>(fn: () => Promise<T>, deps: React.DependencyList) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    let alive = true
    setLoading(true)
    setError(null)
    fn()
      .then((d) => {
        if (alive) setData(d)
      })
      .catch((e: unknown) => {
        if (alive) setError(toApiError(e))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
  return { data, error, loading }
}

/** result.csv 分支：通用表格（懒加载，展开才拉文件） */
function CsvResultBody({ taskId, artifact }: { taskId: string; artifact: Artifact }) {
  const [text, setText] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let alive = true
    api
      .downloadFile(taskId, artifact.path)
      .then((blob) => blob.text())
      .then((t) => {
        if (alive) setText(t)
      })
      .catch((e: unknown) => {
        if (alive) setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      alive = false
    }
  }, [taskId, artifact.path])

  const parsed = useMemo(() => (text == null ? null : parseCsv(text, 50)), [text])
  if (error) return <p className="py-2 text-2xs text-danger-text">result.csv 读取失败：{error}</p>
  if (parsed == null) return <p className="py-2 text-2xs text-fg-3">读取 result.csv…</p>
  if (parsed.headers.length === 0 && parsed.rows.length === 0) {
    return (
      <pre className="max-h-48 overflow-auto rounded-md border border-line-soft bg-surface-2 px-2 py-1.5 font-mono text-2xs">
        {text}
      </pre>
    )
  }
  return (
    <div className="flex flex-col gap-1.5">
      <div className="overflow-x-auto rounded-md border border-line-soft">
        <table className="w-full border-collapse text-2xs">
          <thead>
            <tr className="bg-surface-2 text-left">
              <th className="w-8 px-2 py-1 font-medium text-fg-3">#</th>
              {parsed.headers.map((h, i) => (
                <th key={`${h}-${i}`} className="whitespace-nowrap px-2 py-1 font-medium text-fg-2">
                  {h || '—'}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {parsed.rows.map((row, ri) => (
              <tr key={ri} className="border-t border-line-soft">
                <td className="px-2 py-1 tabular-nums text-fg-3">{ri + 1}</td>
                {parsed.headers.map((_, ci) => (
                  <td key={ci} className="max-w-[16rem] break-all px-2 py-1 text-fg-2">
                    {row[ci] ?? ''}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="text-2xs text-fg-3">通用预览只渲染前 {parsed.rows.length} 行（完整数据下载/整页查看）。</p>
    </div>
  )
}

/** result.html 分支：sandbox iframe（不带 allow-scripts，runner 自述内容按不可信处理 §11.4） */
function HtmlResultBody({ taskId, artifact }: { taskId: string; artifact: Artifact }) {
  const [text, setText] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let alive = true
    api
      .downloadFile(taskId, artifact.path)
      .then((blob) => blob.text())
      .then((t) => {
        if (alive) setText(t)
      })
      .catch((e: unknown) => {
        if (alive) setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      alive = false
    }
  }, [taskId, artifact.path])

  if (error) return <p className="py-2 text-2xs text-danger-text">result.html 读取失败：{error}</p>
  if (text == null) return <p className="py-2 text-2xs text-fg-3">读取 result.html…</p>
  return (
    <div className="flex flex-col gap-1.5">
      <Badge tone="info" className="w-fit">
        sandbox 渲染（脚本已禁用）
      </Badge>
      <iframe sandbox="" srcDoc={text} title={artifact.path} className="h-96 w-full rounded-md border border-line bg-white" />
    </div>
  )
}

export function RunnerResultPanel({
  taskId,
  runner,
  attemptNo,
  artifacts,
}: {
  taskId: string
  runner: RunnerExecution
  attemptNo: number
  /** 该 attempt 的产物清单（用于找 runner 的 result.csv / result.html） */
  artifacts: Artifact[]
}) {
  const [open, setOpen] = useState(runner.status === 'failed')
  const [downloading, setDownloading] = useState(false)

  const found = useMemo(() => resultArtifactsOf(artifacts, runner.seq), [artifacts, runner.seq])
  // 三选一优先级：json > html > csv（json 结构化信息最多；html 是完整报告；csv 是明细）
  const picked = found.json ?? found.html ?? found.csv

  const download = async () => {
    if (!picked) return
    setDownloading(true)
    try {
      const blob = await api.downloadFile(taskId, picked.path)
      saveBlob(blob, picked.path.split('/').pop() ?? 'result')
    } finally {
      setDownloading(false)
    }
  }

  const isTestRole = runner.role === 'test'

  return (
    <div data-testid={`runner-result-${runner.seq}`} className="border-t border-line-soft first:border-t-0">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((prev) => !prev)}
        className="flex w-full flex-wrap items-center gap-2 px-1 py-2.5 text-left"
      >
        <span className="text-fg-3 transition-transform" style={{ transform: open ? 'rotate(90deg)' : 'none' }} aria-hidden>
          ▸
        </span>
        <span className="text-sm font-medium text-fg">
          <span className="mr-1.5 font-mono text-2xs text-fg-3">#{runner.seq}</span>
          {runner.name}
        </span>
        <RunnerStatusBadge status={runner.status} />
        <Tag mono>
          {runner.runner_name}@{runner.runner_version}
        </Tag>
        <span className="text-2xs text-fg-3">{runnerDuration(runner)}</span>
        {runner.result_ref ? (
          <Badge tone="ok">result.json</Badge>
        ) : found.html ? (
          <Badge tone="info">result.html</Badge>
        ) : found.csv ? (
          <Badge tone="info">result.csv</Badge>
        ) : (
          <Badge tone="muted">无结果文件</Badge>
        )}
        {runner.error ? (
          <Badge tone="err" className="font-mono">
            {runner.error.code}
          </Badge>
        ) : null}
      </button>
      {open ? (
        <div className={cn('flex flex-col gap-2 px-1 pb-3')}>
          {runner.error ? (
            <div className="flex flex-wrap items-center gap-2 rounded-md border border-danger/25 bg-danger-weak px-3 py-2 text-2xs">
              <Badge tone="err" className="font-mono">
                {runner.error.code}
              </Badge>
              <span className="text-fg">{runner.error.message}</span>
              {runner.error.retryable ? <Badge tone="warn">可重试</Badge> : null}
            </div>
          ) : null}

          {runner.result_ref ? (
            <JsonResultBody taskId={taskId} runner={runner} attemptNo={attemptNo} />
          ) : found.html ? (
            <HtmlResultBody taskId={taskId} artifact={found.html} />
          ) : found.csv ? (
            <CsvResultBody taskId={taskId} artifact={found.csv} />
          ) : (
            <p className="text-2xs leading-[16px] text-fg-3">
              {isTestRole
                ? '该 runner 没有产出 result.json / html / csv 三选一中的任何一份（D-74）；以日志与产物清单为准。'
                : 'deploy / clean 角色没有 result.json（产物只看日志与 exports，§9.1）。'}
            </p>
          )}

          <div className="flex flex-wrap items-center gap-2">
            {picked ? (
              <>
                <Link
                  to={`/tasks/${encodeURIComponent(taskId)}/file?path=${encodeURIComponent(picked.path)}&attempt=${attemptNo}&seq=${runner.seq}`}
                  className={buttonClassName({ size: 'sm', variant: 'ghost' })}
                >
                  查看 <ArrowUpRight className="size-3" aria-hidden />
                </Link>
                <button
                  type="button"
                  className={buttonClassName({ size: 'sm', variant: 'ghost' })}
                  disabled={downloading}
                  onClick={() => void download()}
                >
                  <Download className="size-3" aria-hidden /> 下载
                </button>
              </>
            ) : null}
            <Link
              to={`/tasks/${encodeURIComponent(taskId)}/file?path=${encodeURIComponent(runner.log_ref ?? `attempts/${attemptNo}/runners/${runner.seq}/logs/stdout.log`)}&attempt=${attemptNo}&seq=${runner.seq}`}
              className={buttonClassName({ size: 'sm', variant: 'ghost' })}
            >
              看日志 <ArrowUpRight className="size-3" aria-hidden />
            </Link>
          </div>
        </div>
      ) : null}
    </div>
  )
}
