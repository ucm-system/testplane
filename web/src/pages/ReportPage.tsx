import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowLeft, Download, FileWarning, RefreshCw } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type { Result, ResultMetric } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { RunnerStatusBadge } from '@/components/app/StatusBadge'
import { useAsync } from '@/hooks/useAsync'
import { MarkdownView } from '@/components/ui/markdown-view'
import { formatBytes, saveBlob } from '@/lib/download'
import { formatDuration } from '@/lib/utils'

/**
 * Task 报告内容区（`GET /tasks/{id}/report` + `/report.md`，默认最后一个 attempt，§7.2）。
 *
 * 两个消费方：`/tasks/:taskId/report` 整页路由（ReportPage，带返回键），以及
 * Task 详情页的「报告」Tab（默认页签，v2：报告即主体）。内容与动作完全一致。
 *
 * 契约要点（§8.4）：report.json **只做组织与索引、不做指标聚合**（不求和、不求平均），
 * 所以指标表是 UI 侧按 `runners[].result_ref` 逐个拉
 * `GET /tasks/{id}/runners/{seq}/result`（附录 A schema 2.0）聚合的。
 * 产物三选一（D-74）：只产出 html / csv 的 runner 没有 result.json —— 指标区直说"非 JSON 结果"。
 * 文件尚未生成时后端返回 404 + Retry-After：这里只给**一个**空状态 + 按秒倒计时的重试。
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
function retryAfterSeconds(error: { detail?: Record<string, unknown> } | null): number {
  const raw = error?.detail?.['retry_after_seconds']
  const value = typeof raw === 'number' ? raw : Number(raw)
  if (!Number.isFinite(value) || value <= 0) return FALLBACK_RETRY_AFTER
  return Math.min(60, Math.max(1, Math.round(value)))
}

function formatMetricValue(value: number | null): string {
  if (value == null) return '—'
  if (Number.isInteger(value)) return value.toLocaleString('zh-CN')
  const abs = Math.abs(value)
  const digits = abs >= 100 ? 1 : abs >= 1 ? 2 : 4
  return value.toLocaleString('zh-CN', { maximumFractionDigits: digits })
}

interface MetricGroup {
  labels: Array<[string, string]>
  metrics: ResultMetric[]
}

/** 按 `labels` 分组（同组参数的指标排在一起，列里不重复贴标签；labels 值允许非字符串） */
function groupByLabels(metrics: ResultMetric[]): MetricGroup[] {
  const groups = new Map<string, MetricGroup>()
  const order: string[] = []
  for (const metric of metrics) {
    const raw = metric.labels ?? {}
    const pairs: Array<[string, string]> = Object.keys(raw)
      .sort()
      .map((name) => [name, String(raw[name])])
    const key = JSON.stringify(pairs)
    let group = groups.get(key)
    if (!group) {
      group = { labels: pairs, metrics: [] }
      groups.set(key, group)
      order.push(key)
    }
    group.metrics.push(metric)
  }
  return order.map((key) => groups.get(key)!).filter(Boolean)
}

/** 一张指标表：列 = labels 键 + 指标名 + 统计口径 + 值 + 单位 */
function MetricsTable({ result }: { result: Result }) {
  const groups = useMemo(() => groupByLabels(result.metrics), [result.metrics])
  const labelKeys = useMemo(() => {
    const keys: string[] = []
    for (const group of groups) {
      for (const [key] of group.labels) if (!keys.includes(key)) keys.push(key)
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
                      ? group.labels.map(([labelKey, labelValue]) => (
                          <TD
                            key={labelKey}
                            rowSpan={group.metrics.length}
                            className="align-top font-mono text-2xs text-fg-2"
                          >
                            {labelValue}
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

export function TaskReportView({ taskId }: { taskId: string }) {
  const report = useAsync(() => api.getTaskReport(taskId), [taskId])
  const markdown = useAsync(() => api.getTaskReportMd(taskId), [taskId])
  const [downloading, setDownloading] = useState<string | null>(null)
  const [downloadNotice, setDownloadNotice] = useState<string | null>(null)
  const [downloadError, setDownloadError] = useState<Error['message'] | null>(null)

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
  const [results, setResults] = useState<Record<number, Result | null>>({})
  const [resultErrors, setResultErrors] = useState<Record<number, string | null>>({})
  const [metricsLoading, setMetricsLoading] = useState(false)

  const runnersWithResult = useMemo(
    () => (data?.runners ?? []).filter((runner) => Boolean(runner.result_ref)),
    [data],
  )

  // 换 Task 时重置（同一路由组件可能复用）
  useEffect(() => {
    setMetricsRequested(false)
    setHasScrolled(false)
    fetchedForRef.current = null
    setResults({})
    setResultErrors({})
    setMetricsLoading(false)
  }, [taskId])

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
    const cacheKey = `${taskId}:${data.attempt_no}:${data.runners.length}`
    if (fetchedForRef.current === cacheKey) return
    fetchedForRef.current = cacheKey

    const targets = data.runners.filter((runner) => Boolean(runner.result_ref))
    if (targets.length === 0) {
      setResults({})
      setResultErrors({})
      setMetricsLoading(false)
      return
    }

    let alive = true
    setMetricsLoading(true)
    void Promise.all(
      targets.map(async (runner) => {
        try {
          return {
            seq: runner.seq,
            result: await api.getRunnerResult(taskId, runner.seq, { attempt: data.attempt_no }),
            error: null,
          }
        } catch (e) {
          const error = toApiError(e)
          return { seq: runner.seq, result: null, error: `${error.code}：${error.message}` }
        }
      }),
    ).then((rows) => {
      if (!alive) return
      const nextResults: Record<number, Result | null> = {}
      const nextErrors: Record<number, string | null> = {}
      for (const row of rows) {
        nextResults[row.seq] = row.result
        nextErrors[row.seq] = row.error
      }
      setResults(nextResults)
      setResultErrors(nextErrors)
      setMetricsLoading(false)
    })

    return () => {
      alive = false
    }
  }, [metricsRequested, data, taskId])

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
        file === 'bundle' ? await api.downloadBundle(taskId) : await api.downloadFile(taskId, file)
      const name = file === 'bundle' ? `${taskId}.zip` : file
      saveBlob(blob, name)
      setDownloadNotice(`${name} · ${formatBytes(blob.size)} 已开始下载`)
    } catch (e) {
      setDownloadError(e instanceof Error ? e.message : String(e))
    } finally {
      setDownloading(null)
    }
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
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

        {data ? <Badge tone="muted" className="font-mono">{data.task_id}</Badge> : null}
        {data ? <Badge tone="info">attempt #{data.attempt_no}</Badge> : null}
        {data?.truncated ? <Badge tone="warn">摘要版（{data.report_ref ?? 'report.full.json'}）</Badge> : null}
        {downloadNotice ? <span className="text-2xs text-fg-3">{downloadNotice}</span> : null}
      </div>

      {downloadError ? <ErrorState error={{ code: 'INTERNAL_ERROR', message: downloadError, retryable: false }} compact /> : null}

      {reportMissing ? (
        /* 唯一空状态：说人话（不出现 404 / Retry-After 这类实现细节），保留倒计时重试 */
        <Card data-testid="report-not-generated">
          <EmptyState
            icon={<FileWarning className="size-4" />}
            title="报告尚未生成"
            description="报告还没生成，可能是这个 Task 还没结束；我们会每隔几秒自动重试一次。"
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
            <CardHeader tone="primary" title="计数" sub={`attempt #${data.attempt_no} · 成功 / 失败 / 跳过 / 取消 各多少条`} />
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
              {data.cleanup_failed ? (
                <p className="mt-3 rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs text-warn-text">
                  service-clean 失败（只打 cleanup_failed 事件，不影响 Task 终态；进重放清单 §9.3）。
                </p>
              ) : null}
            </CardBody>
          </Card>

          {/* 指标：懒加载 + 按 labels 分组；没有 result.json 就直说，不编数据 */}
          <Card data-testid="metrics-card">
            <CardHeader
              tone="info"
              title="指标"
              sub="按 labels 分组 · 指标取自每个 runner 自己上报的 result.json（schema 2.0）"
              actions={
                metricsRequested ? (
                  <Badge tone="muted">{runnersWithResult.length} 个 runner 有结果</Badge>
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
                <p className="text-xs leading-[16px] text-fg-3">
                  指标按需加载：点上面的「展开指标」，或把这一块滚进视野。报告文件本身不聚合指标。
                </p>
              </CardBody>
            ) : metricsLoading ? (
              <LoadingBlock label="读取各 runner 的指标…" />
            ) : (
              <div className="flex flex-col">
                {data.runners.map((runner) => {
                  const result = results[runner.seq] ?? null
                  const error = resultErrors[runner.seq] ?? null
                  const noMetrics = !runner.result_ref || Boolean(error) || !result || result.metrics.length === 0
                  return (
                    <section
                      key={runner.seq}
                      data-testid="metrics-runner"
                      className="border-t border-line-soft px-5 py-4 first:border-t-0"
                    >
                      <div className="flex flex-wrap items-center gap-2">
                        <h3 className="text-sm font-semibold text-fg">
                          seq #{runner.seq} · {runner.name}
                        </h3>
                        <RunnerStatusBadge status={runner.status} />
                        <Badge tone="muted">{runner.role}</Badge>
                        {result ? (
                          <span className="ml-auto text-2xs text-fg-3">
                            {result.runner?.name} v{result.runner?.version}
                          </span>
                        ) : null}
                      </div>
                      {noMetrics ? (
                        <p data-testid="metrics-empty" className="mt-2 text-xs text-fg-3">
                          {runner.result_ref
                            ? '该 runner 未上报指标'
                            : runner.role === 'deploy' || runner.role === 'clean'
                              ? 'deploy / clean 角色没有 result.json（产物只看日志与 exports）'
                              : '该 runner 产出非 JSON 结果（html / csv），或三选一产物缺失 —— 以产物预览/下载为主（D-74）'}
                          {error ? <span className="ml-2 font-mono text-2xs">（{error}）</span> : null}
                        </p>
                      ) : (
                        <MetricsTable result={result!} />
                      )}
                    </section>
                  )
                })}
                {data.runners.length === 0 ? (
                  <CardBody>
                    <p className="text-xs text-fg-3">报告里没有 runner，自然也没有指标。</p>
                  </CardBody>
                ) : null}
              </div>
            )}
            <CardFooter>
              <span>同一组参数的指标靠 labels 区分；没有 result.json 时显示占位说明，不补假值。</span>
            </CardFooter>
          </Card>

          <Card>
            <CardHeader tone="muted" title="runner 明细" sub={`attempt #${data.attempt_no} · 每个 runner 的状态与耗时（含注入的 deploy / clean）`} />
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>seq</TH>
                    <TH>名称</TH>
                    <TH>角色</TH>
                    <TH>状态</TH>
                    <TH>耗时</TH>
                    <TH>result.json</TH>
                  </TR>
                </THead>
                <TBody>
                  {data.runners.map((runner) => (
                    <TR key={runner.seq}>
                      <TD className="tabular-nums text-2xs text-fg-3">#{runner.seq}</TD>
                      <TD className="max-w-[18rem] truncate text-fg" title={runner.name}>
                        {runner.name}
                      </TD>
                      <TD>
                        <Badge tone="muted">{runner.role}</Badge>
                      </TD>
                      <TD>
                        <RunnerStatusBadge status={runner.status} />
                      </TD>
                      <TD className="tabular-nums">
                        {runner.duration_ms == null ? '—' : formatDuration(runner.duration_ms / 1000)}
                      </TD>
                      <TD className="break-all font-mono text-2xs text-fg-3">{runner.result_ref ?? '—'}</TD>
                    </TR>
                  ))}
                  {data.runners.length === 0 ? (
                    <TR>
                      <TD colSpan={6} className="text-fg-3">
                        报告里没有 runner。
                      </TD>
                    </TR>
                  ) : null}
                </TBody>
              </Table>
            </TableWrap>
          </Card>

          <Card>
            <CardHeader tone="err" title="失败清单" sub="失败原因与错误码（附录 D v2）" />
            {data.failures.length === 0 ? (
              <CardBody>
                <p className="text-xs text-fg-3">没有失败。这是演示里最想看到的一行。</p>
              </CardBody>
            ) : (
              <TableWrap>
                <Table>
                  <THead>
                    <TR className="hover:bg-transparent">
                      <TH>seq</TH>
                      <TH>runner</TH>
                      <TH>原因</TH>
                      <TH>错误码</TH>
                    </TR>
                  </THead>
                  <TBody>
                    {data.failures.map((failure) => (
                      <TR key={`${failure.seq}-${failure.error_code}`}>
                        <TD className="tabular-nums text-2xs text-fg-3">#{failure.seq}</TD>
                        <TD className="text-fg">{failure.name}</TD>
                        <TD className="text-fg-2">{failure.reason ?? '—'}</TD>
                        <TD>
                          <Badge tone="err" className="font-mono">
                            {failure.error_code}
                          </Badge>
                        </TD>
                      </TR>
                    ))}
                  </TBody>
                </Table>
              </TableWrap>
            )}
            <CardFooter>
              <span>失败清单来自报告；单个 runner 的完整结果见「指标」卡里那一节。</span>
            </CardFooter>
          </Card>

          <Card>
            <CardHeader
              tone="info"
              title="report.md"
              sub="Markdown 渲染（表格/代码块保留；不渲染原始 HTML）"
            />
            {markdown.error ? (
              <CardBody>
                <ErrorState error={markdown.error} onRetry={markdown.reload} />
              </CardBody>
            ) : markdown.loading && !markdown.data ? (
              <LoadingBlock label="加载 report.md…" />
            ) : (
              <CardBody>
                <MarkdownView data-testid="report-md" source={markdown.data ?? '（空）'} />
              </CardBody>
            )}
          </Card>
        </>
      ) : null}
    </>
  )
}

/**
 * 报告整页路由（`/tasks/:taskId/report`）：面包屑返回键 + 报告内容区。
 * 内容区与 Task 详情页「报告」Tab 共用同一个 `TaskReportView`（渲染只有一份实现）。
 */
export function ReportPage() {
  const { taskId = '' } = useParams<{ taskId: string }>()
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <Link to={`/tasks/${taskId}`}>
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回 Task 详情
          </Button>
        </Link>
      </div>
      <TaskReportView taskId={taskId} />
    </>
  )
}
