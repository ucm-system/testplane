import { Link, useParams } from 'react-router-dom'
import { ArrowLeft, Download } from 'lucide-react'
import { API_BASE, api } from '@/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { LoadingBlock, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { TaskStatusBadge } from '@/components/app/StatusBadge'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { useAsync } from '@/hooks/useAsync'
import { formatDuration } from '@/lib/utils'

/**
 * Run 报告页（`GET /runs/{id}/report` + `/report.md`）。
 *
 * 契约要点：报告只做组织与索引，**不对指标做聚合**；文件尚未生成时后端返回 404 + Retry-After，
 * 这里按普通错误呈现并给重试。
 */
export function ReportPage() {
  const { runId = '' } = useParams<{ runId: string }>()
  const report = useAsync(() => api.getReport(runId), [runId])
  const markdown = useAsync(() => api.getReportMd(runId), [runId])

  const data = report.data

  return (
    <>
      <RightRail>
        <RailCard title="这份报告是什么" tone="info">
          <p>report.json 只做组织与索引：Task 列表、失败清单、计数；不求和、不求平均。</p>
          <p className="mt-2">报告在 run.finished 之后异步生成，没生成时后端返回 404 + Retry-After。</p>
        </RailCard>
        <RailCard title="原始文件" tone="muted">
          <p className="break-all font-mono text-2xs">{API_BASE}/runs/{runId}/report.md</p>
        </RailCard>
      </RightRail>

      <div className="flex flex-wrap items-center gap-2">
        <Link to={`/runs/${runId}`}>
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回 Run 详情
          </Button>
        </Link>
        <a href={`${API_BASE}/runs/${runId}/report.md`} target="_blank" rel="noreferrer">
          <Button size="sm" variant="ghost" icon={<Download className="size-3" aria-hidden />}>
            打开 report.md
          </Button>
        </a>
        {data ? <Badge tone="muted" className="font-mono">{data.run_id}</Badge> : null}
        {data?.truncated ? <Badge tone="warn">摘要版（{data.report_ref ?? 'report.full.json'}）</Badge> : null}
      </div>

      <Card>
        <CardHeader tone="primary" title="计数" sub="ReportCounts：第 3 个数是失败，第 4 个是跳过" />
        {report.error ? (
          <CardBody>
            <ErrorState error={report.error} onRetry={report.reload} />
          </CardBody>
        ) : report.loading && !data ? (
          <LoadingBlock label="加载报告…" />
        ) : data ? (
          <CardBody>
            <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
              {[
                ['total', data.counts.total, 'muted'],
                ['succeeded', data.counts.succeeded, 'ok'],
                ['failed', data.counts.failed, 'err'],
                ['skipped', data.counts.skipped, 'warn'],
                ['canceled', data.counts.canceled, 'muted'],
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
        ) : null}
      </Card>

      <Card>
        <CardHeader tone="muted" title="Task 明细" sub="status / attempts / duration_ms / result_ref" />
        {data ? (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>task_id</TH>
                  <TH>名称</TH>
                  <TH>状态</TH>
                  <TH>Attempt</TH>
                  <TH>耗时</TH>
                  <TH>result_ref</TH>
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
                    <TD className="tabular-nums">{task.duration_ms == null ? '—' : formatDuration(task.duration_ms / 1000)}</TD>
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
        ) : null}
      </Card>

      <Card>
        <CardHeader tone="err" title="失败清单" sub="failures[]：reason 通常与 error_code 同值" />
        {data ? (
          data.failures.length === 0 ? (
            <CardBody>
              <p className="text-xs text-fg-3">没有失败。这是演示里最想看到的一行。</p>
            </CardBody>
          ) : (
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>task_id</TH>
                    <TH>reason</TH>
                    <TH>error_code</TH>
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
          )
        ) : null}
        <CardFooter>
          <span>失败清单来自 report.json；单 Task 的完整 result.json 见各 Attempt 面板。</span>
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
  )
}
