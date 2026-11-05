import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowLeft, Download, FileWarning, RefreshCw } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Result, ResultMetric } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { TaskStatusBadge } from '@/components/app/StatusBadge'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { useAsync } from '@/hooks/useAsync'
import { formatBytes, saveBlob } from '@/lib/download'
import { formatDuration } from '@/lib/utils'

/**
 * Run 报告页（`GET /runs/{id}/report` + `/report.md`）。
 *
 * 契约要点（control-plane §8.4）：report.json **只做组织与索引、不做指标聚合**（不求和、不求平均），
 * 所以指标表是 UI 侧按 `tasks[].result_ref` 逐个拉 `GET /tasks/{id}/attempts/{idx}/result` 聚合的。
 * 文件尚未生成时后端返回 404 + Retry-After：这里只给**一个**空状态 + 按秒倒计时的重试，
 * 不把 404 的直译分别渲染到计数卡和 report.md 卡两份。
 */
/** 报告页固定提供这四个文件（report.md 也在里面，不再只给 md） */
const FILES = [
  { name: 'report.json', label: 'report.json' },
  { name: 'report.md', label: 'report.md' },
  { name: 'metadata.json', label: 'metadata.json' },
  { name: 'events.jsonl', label: 'events.jsonl' },
] as const

/** 后端没给 `Retry-After` 时的兜底秒数（与报告异步生成的节奏一致） */
const FALLBACK_RETRY_AFTER = 5

/** `Retry-After` 已由 api/client.ts 折进 `detail.retry_after_seconds` */
function retryAfterSeconds(error: ApiErrorBody | null): number {
  const raw = error?.detail?.['retry_after_seconds']
  const value = typeof raw === 'number' ? raw : Number(raw)
  if (!Number.isFinite(value) || value <= 0) return FALLBACK_RETRY_AFTER
  return Math.min(60, Math.max(1, Math.round(value)))
}

/** `report.json.tasks[].result_ref`（`…/attempts/<idx>/result.json`）→ Attempt 序号 */
function attemptIdxOfResultRef(resultRef: string | null | undefined, fallback: number): number {
  const hit = resultRef ? /attempts\/(\d+)\//.exec(resultRef) : null
  const idx = hit ? Number(hit[1]) : fallback
  return Number.isFinite(idx) && idx > 0 ? idx : 1
}

function formatMetricValue(value: number | null): string {
  if (value == null) return '—'
  if (Number.isInteger(value)) return value.toLocaleString('zh-CN')
  const abs = Math.abs(value)
  const digits = abs >= 100 ? 1 : abs >= 1 ? 2 : 4
  return value.toLocaleString('zh-CN', { maximumFractionDigits: digits })
}

interface MetricGroup {
  labels: Record<string, string>
  metrics: ResultMetric[]
}

/** 按 `labels` 分组（同组参数的指标排在一起，列里不重复贴标签） */
function groupByLabels(metrics: ResultMetric[]): MetricGroup[] {
  const groups = new Map<string, MetricGroup>()
  const order: string[] = []
  for (const metric of metrics) {
    const labels = metric.labels ?? {}
    const key = JSON.stringify(Object.keys(labels).sort().map((name) => [name, labels[name]]))
    let group = groups.get(key)
    if (!group) {
      group = { labels, metrics: [] }
      groups.set(key, group)
      order.push(key)
    }
    group.metrics.push(metric)
  }
  return order.map((key) => groups.get(key)!).filter(Boolean)
}

/** 一张指标表：列 = labels 键 + 指标名 + 值 + 单位 */
function MetricsTable({ result }: { result: Result }) {
  const groups = useMemo(() => groupByLabels(result.metrics), [result.metrics])
  const labelKeys = useMemo(() => {
    const keys: string[] = []
    for (const group of groups) {
      for (const key of Object.keys(group.labels)) if (!keys.includes(key)) keys.push(key)
    }
    return keys
  }, [groups])

  return (
    <TableWrap className="mt-2 rounded-md border border-line-soft">
      <Table data-testid="metrics-table">
        <THead>
          <TR className="hover:bg-transparent">
            {labelKeys.map((key) => (
              <TH key={key}>{key}</TH>
            ))}
            <TH>指标</TH>
            <TH>值</TH>
            <TH>单位</TH>
          </TR>
        </THead>
        <TBody>
          {groups.map((group) => {
            const groupKey = JSON.stringify(group.labels)
            return (
              <Fragment key={groupKey}>
                {group.metrics.map((metric, index) => (
                  <TR key={`${groupKey}-${metric.name}-${metric.statistic}-${index}`}>
                    {index === 0
                      ? labelKeys.map((labelKey) => (
                          <TD
                            key={labelKey}
                            rowSpan={group.metrics.length}
                            className="align-top font-mono text-2xs text-fg-2"
                          >
                            {group.labels[labelKey] ?? '—'}
                          </TD>
                        ))
                      : null}
                    <TD className="text-fg">
                      <span className="font-mono text-xs">{metric.name}</span>
                      <span className="ml-1.5 text-2xs text-fg-3">{metric.statistic}</span>
                    </TD>
                    <TD className="tabular-nums">{formatMetricValue(metric.value)}</TD>
                    <TD className="font-mono text-2xs text-fg-3">{metric.unit}</TD>
                  </TR>
                ))}
              </Fragment>
            )
          })}
        </TBody>
      </Table>
    </TableWrap>
  )
}

export function ReportPage() {
  const { runId = '' } = useParams<{ runId: string }>()
  const report = useAsync(() => api.getReport(runId), [runId])
  const markdown = useAsync(() => api.getReportMd(runId), [runId])
  const [downloading, setDownloading] = useState<string | null>(null)
  const [downloadNotice, setDownloadNotice] = useState<string | null>(null)
  const [downloadError, setDownloadError] = useState<ApiErrorBody | null>(null)

  const data = report.data

  /* ---------------------------------------------------------- 404：报告尚未生成 */
  const notGenerated = report.error?.code === 'NOT_FOUND'
  /** 重载时会短暂清空 error：用 sticky 标记保证空状态不闪、倒计时不断 */
  const [notReady, setNotReady] = useState(false)
  useEffect(() => {
    if (notGenerated) setNotReady(true)
    else if (data || (report.error && !report.loading)) setNotReady(false)
  }, [notGenerated, data, report.error, report.loading])
  const reportMissing = notReady && !data
  const retryAfter = retryAfterSeconds(report.error)

  const [countdown, setCountdown] = useState(retryAfter)
  useEffect(() => {
    if (!notGenerated) return
    setCountdown(retryAfter)
    let left = retryAfter
    const timer = setInterval(() => {
      left -= 1
      setCountdown(Math.max(0, left))
      if (left <= 0) {
        clearInterval(timer)
        report.reload()
      }
    }, 1000)
    return () => clearInterval(timer)
  }, [notGenerated, report.error, retryAfter, report.reload])

  /* ---------------------------------------------------------- 指标：懒加载 */
  const [metricsRequested, setMetricsRequested] = useState(false)
  /** 首屏就自动取会变成"一进来就打接口"；要求「点开或滚到这张卡才取」，所以先等一次滚动 */
  const [hasScrolled, setHasScrolled] = useState(false)
  const metricsCardRef = useRef<HTMLDivElement | null>(null)
  const fetchedForRef = useRef<string | null>(null)
  const [results, setResults] = useState<Record<string, Result | null>>({})
  const [resultErrors, setResultErrors] = useState<Record<string, string | null>>({})
  const [metricsLoading, setMetricsLoading] = useState(false)

  const tasksWithResult = useMemo(
    () => (data?.tasks ?? []).filter((task) => Boolean(task.result_ref)),
    [data],
  )

  // 换 Run 时重置（同一路由组件可能复用）
  useEffect(() => {
    setMetricsRequested(false)
    setHasScrolled(false)
    fetchedForRef.current = null
    setResults({})
    setResultErrors({})
    setMetricsLoading(false)
  }, [runId])

  useEffect(() => {
    const onScroll = () => setHasScrolled(true)
    window.addEventListener('scroll', onScroll, { passive: true, once: true })
    return () => window.removeEventListener('scroll', onScroll)
  }, [])

  // 懒加载：用户滚动后，卡片进入视野才请求（点「展开指标」也走同一个开关）
  useEffect(() => {
    if (!data || metricsRequested || !hasScrolled) return
    const element = metricsCardRef.current
    if (!element || typeof IntersectionObserver === 'undefined') return
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) setMetricsRequested(true)
      },
      { rootMargin: '120px' },
    )
    observer.observe(element)
    return () => observer.disconnect()
  }, [data, metricsRequested, hasScrolled])

  useEffect(() => {
    if (!metricsRequested || !data) return
    const cacheKey = `${runId}:${data.status}:${data.tasks.length}`
    if (fetchedForRef.current === cacheKey) return
    fetchedForRef.current = cacheKey

    const targets = data.tasks.filter((task) => Boolean(task.result_ref))
    if (targets.length === 0) {
      setResults({})
      setResultErrors({})
      setMetricsLoading(false)
      return
    }

    let alive = true
    setMetricsLoading(true)
    void Promise.all(
      targets.map(async (task) => {
        const idx = attemptIdxOfResultRef(task.result_ref, task.attempts || 1)
        try {
          return { taskId: task.task_id, result: await api.getAttemptResult(task.task_id, idx), error: null }
        } catch (e) {
          const error = toApiError(e)
          return { taskId: task.task_id, result: null, error: `${error.code}：${error.message}` }
        }
      }),
    ).then((rows) => {
      if (!alive) return
      const nextResults: Record<string, Result | null> = {}
      const nextErrors: Record<string, string | null> = {}
      for (const row of rows) {
        nextResults[row.taskId] = row.result
        nextErrors[row.taskId] = row.error
      }
      setResults(nextResults)
      setResultErrors(nextErrors)
      setMetricsLoading(false)
    })

    return () => {
      alive = false
    }
  }, [metricsRequested, data, runId])

  /**
   * 下载：`bundle` 走整包 zip，其余走单文件；两条都必须带鉴权（dev 是 Bearer、OIDC 是 Cookie），
   * 所以不能用 `<a href>` 裸链 —— 401 时用户只会看到一段 JSON。
   */
  const download = async (file: 'bundle' | string) => {
    setDownloading(file)
    setDownloadError(null)
    setDownloadNotice(null)
    try {
      const blob =
        file === 'bundle' ? await api.downloadBundle(runId) : await api.downloadFile(runId, `${runId}/${file}`)
      const name = file === 'bundle' ? `${runId}.zip` : file
      saveBlob(blob, name)
      setDownloadNotice(`${name} · ${formatBytes(blob.size)} 已开始下载`)
    } catch (e) {
      setDownloadError(toApiError(e))
    } finally {
      setDownloading(null)
    }
  }

  return (
    <>
      <RightRail>
        <RailCard title="这份报告是什么" tone="info">
          <p>报告按 Task 列清单：谁成功、谁失败、失败原因是什么，外加几个可以直接下载的原文件。</p>
          <p className="mt-2">
            指标不在这里汇总：展开下面的「指标」卡，才会去读每个 Task 自己上报的那份结果。
          </p>
        </RailCard>
        <RailCard title="打不开的时候" tone="muted">
          <p>Run 还没跑完时报告还没写出来，页面会等一会儿自动再试；跑完之后刷新就有。</p>
        </RailCard>
      </RightRail>

      <div className="flex flex-wrap items-center gap-2">
        <Link to={`/runs/${runId}`}>
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回 Run 详情
          </Button>
        </Link>

        {!reportMissing ? (
          <>
            {/* 打包下载：走 api.downloadBundle（带鉴权 fetch → Blob → a[download]），不是裸 href */}
            <Button
              size="sm"
              variant="primary"
              data-testid="download-bundle"
              icon={<Download className="size-3" aria-hidden />}
              loading={downloading === 'bundle'}
              onClick={() => void download('bundle')}
            >
              {downloading === 'bundle' ? '打包中…' : '打包下载 .zip'}
            </Button>

            {/* 四个直链：与结果视图的产物下载保持同一套动作（全部经 blob，不裸 href） */}
            {FILES.map((file) => (
              <Button
                key={file.name}
                size="sm"
                variant="ghost"
                data-testid={`download-${file.name.replace(/\W/g, '-')}`}
                loading={downloading === file.name}
                onClick={() => void download(file.name)}
              >
                {downloading === file.name ? '下载中…' : file.label}
              </Button>
            ))}
          </>
        ) : null}

        {data ? <Badge tone="muted" className="font-mono">{data.run_id}</Badge> : null}
        {data?.truncated ? <Badge tone="warn">摘要版（{data.report_ref ?? 'report.full.json'}）</Badge> : null}
        {downloadNotice ? <span className="text-2xs text-fg-3">{downloadNotice}</span> : null}
      </div>

      {downloadError ? (
        <ErrorState error={downloadError} compact onRetry={() => void download('bundle')} />
      ) : null}

      {reportMissing ? (
        /* 唯一空状态：404 只在这里出现一次，带 Retry-After 倒计时重试 */
        <Card data-testid="report-not-generated">
          <EmptyState
            icon={<FileWarning className="size-4" />}
            title="报告尚未生成"
            description="Run 未终态时后端返回 404 + Retry-After：等 Run 跑完，report.json 与 report.md 才会写出来。页面会按服务端给的间隔自动重试。"
            action={
              <div className="flex flex-col items-center gap-2">
                <Button
                  size="sm"
                  data-testid="retry-report"
                  disabled={countdown > 0}
                  onClick={report.reload}
                  icon={<RefreshCw className="size-3" aria-hidden />}
                >
                  {countdown > 0 ? `${countdown} 秒后可重试` : '立即重试'}
                </Button>
                <span className="text-2xs text-fg-3">每 {retryAfter} 秒自动再试一次</span>
              </div>
            }
          />
        </Card>
      ) : report.error ? (
        <Card>
          <CardBody>
            <ErrorState error={report.error} onRetry={report.reload} />
          </CardBody>
        </Card>
      ) : report.loading && !data ? (
        <Card>
          <LoadingBlock label="加载报告…" />
        </Card>
      ) : data ? (
        <>
          <Card>
            <CardHeader tone="primary" title="计数" sub="成功 / 失败 / 跳过 / 取消 各多少条" />
            <CardBody>
              <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
                {[
                  ['总数', data.counts.total, 'muted'],
                  ['成功', data.counts.succeeded, 'ok'],
                  ['失败', data.counts.failed, 'err'],
                  ['跳过', data.counts.skipped, 'warn'],
                  ['取消', data.counts.canceled, 'muted'],
                ].map(([label, value, tone]) => (
                  <div key={String(label)} className="rounded-md border border-line-soft bg-surface-2 px-3 py-2">
                    <dt className="text-2xs text-fg-3">{label}</dt>
                    <dd className="mt-1 flex items-center gap-2">
                      <span className="text-lg font-semibold tabular-nums text-fg">{value}</span>
                      <Badge tone={tone as 'muted'}>{String(label)}</Badge>
                    </dd>
                  </div>
                ))}
              </dl>
            </CardBody>
          </Card>

          {/* 指标：懒加载 + 按 labels 分组；没有 result/json 就直说，不编数据 */}
          <Card data-testid="metrics-card">
            <CardHeader
              tone="info"
              title="指标"
              sub="按 labels 分组 · 指标取自每个 Task 自己上报的结果"
              actions={
                metricsRequested ? (
                  <Badge tone="muted">{tasksWithResult.length} 个 Task 有结果</Badge>
                ) : (
                  <Button
                    size="sm"
                    variant="ghost"
                    data-testid="toggle-metrics"
                    onClick={() => setMetricsRequested(true)}
                  >
                    展开指标
                  </Button>
                )
              }
            />
            <div ref={metricsCardRef} />
            {!metricsRequested ? (
              <CardBody>
                <p className="text-xs leading-relaxed text-fg-3">
                  指标按需加载：点上面的「展开指标」，或把这一块滚进视野。报告文件本身不聚合指标。
                </p>
              </CardBody>
            ) : metricsLoading ? (
              <LoadingBlock label="读取各 Task 的指标…" />
            ) : (
              <div className="flex flex-col">
                {data.tasks.map((task) => {
                  const result = results[task.task_id] ?? null
                  const error = resultErrors[task.task_id] ?? null
                  const noMetrics = !task.result_ref || Boolean(error) || !result || result.metrics.length === 0
                  return (
                    <section
                      key={task.task_id}
                      data-testid="metrics-task"
                      className="border-t border-line-soft px-5 py-4 first:border-t-0"
                    >
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold text-fg">{task.name}</h3>
                        <TaskStatusBadge status={task.status} />
                        <span className="font-mono text-2xs text-fg-3">{task.task_id}</span>
                        {result ? (
                          <span className="ml-auto text-2xs text-fg-3">
                            {result.tester?.name} v{result.tester?.version}
                          </span>
                        ) : null}
                      </div>
                      {noMetrics ? (
                        <p data-testid="metrics-empty" className="mt-2 text-xs text-fg-3">
                          该 Tester 未上报指标
                          {error ? <span className="ml-2 font-mono text-2xs">（{error}）</span> : null}
                        </p>
                      ) : (
                        <MetricsTable result={result!} />
                      )}
                    </section>
                  )
                })}
                {data.tasks.length === 0 ? (
                  <CardBody>
                    <p className="text-xs text-fg-3">报告里没有 Task，自然也没有指标。</p>
                  </CardBody>
                ) : null}
              </div>
            )}
            <CardFooter>
              <span>同一组参数的指标靠 labels 区分；没有结果文件时显示「该 Tester 未上报指标」，不补假值。</span>
            </CardFooter>
          </Card>

          <Card>
            <CardHeader tone="muted" title="Task 明细" sub="每个 Task 的状态、尝试次数与耗时" />
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>任务 ID</TH>
                    <TH>名称</TH>
                    <TH>状态</TH>
                    <TH>尝试次数</TH>
                    <TH>耗时</TH>
                    <TH>结果文件</TH>
                  </TR>
                </THead>
                <TBody>
                  {data.tasks.map((task) => (
                    <TR key={task.task_id}>
                      <TD className="font-mono text-2xs text-fg-3">{task.task_id}</TD>
                      <TD className="max-w-[18rem] truncate text-fg" title={task.name}>
                        {task.name}
                      </TD>
                      <TD>
                        <TaskStatusBadge status={task.status} />
                      </TD>
                      <TD className="tabular-nums">{task.attempts}</TD>
                      <TD className="tabular-nums">
                        {task.duration_ms == null ? '—' : formatDuration(task.duration_ms / 1000)}
                      </TD>
                      <TD className="break-all font-mono text-2xs text-fg-3">{task.result_ref ?? '—'}</TD>
                    </TR>
                  ))}
                  {data.tasks.length === 0 ? (
                    <TR>
                      <TD colSpan={6} className="text-fg-3">
                        报告里没有 Task。
                      </TD>
                    </TR>
                  ) : null}
                </TBody>
              </Table>
            </TableWrap>
          </Card>

          <Card>
            <CardHeader tone="err" title="失败清单" sub="失败原因与错误码" />
            {data.failures.length === 0 ? (
              <CardBody>
                <p className="text-xs text-fg-3">没有失败。这是演示里最想看到的一行。</p>
              </CardBody>
            ) : (
              <TableWrap>
                <Table>
                  <THead>
                    <TR className="hover:bg-transparent">
                      <TH>任务 ID</TH>
                      <TH>原因</TH>
                      <TH>错误码</TH>
                    </TR>
                  </THead>
                  <TBody>
                    {data.failures.map((failure) => (
                      <TR key={`${failure.task_id}-${failure.error_code ?? 'x'}`}>
                        <TD className="font-mono text-2xs text-fg-3">{failure.task_id}</TD>
                        <TD className="text-fg">{failure.reason}</TD>
                        <TD>
                          {failure.error_code ? (
                            <Badge tone="err" className="font-mono">
                              {failure.error_code}
                            </Badge>
                          ) : (
                            <span className="text-fg-3">—</span>
                          )}
                        </TD>
                      </TR>
                    ))}
                  </TBody>
                </Table>
              </TableWrap>
            )}
            <CardFooter>
              <span>失败清单来自报告；单个 Task 的完整结果见「指标」卡里那一节。</span>
            </CardFooter>
          </Card>

          <Card>
            <CardHeader tone="info" title="report.md 原文" sub="markdown 人类可读摘要" />
            {markdown.error ? (
              <CardBody>
                <ErrorState error={markdown.error} onRetry={markdown.reload} />
              </CardBody>
            ) : markdown.loading && !markdown.data ? (
              <LoadingBlock label="加载 report.md…" />
            ) : (
              <CardBody>
                <pre className="max-h-[28rem] overflow-auto rounded-md border border-line-soft bg-surface-2 p-3 font-mono text-2xs leading-relaxed text-fg-2">
                  {markdown.data ?? '（空）'}
                </pre>
              </CardBody>
            )}
          </Card>
        </>
      ) : null}
    </>
  )
}
