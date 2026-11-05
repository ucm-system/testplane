import { useState } from 'react'
import { Link } from 'react-router-dom'
import { ArrowUpRight, Check, Download, FileText } from 'lucide-react'
import { API_BASE, api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Artifact, Run } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { RunStatusBadge } from '@/components/app/StatusBadge'
import { useAsync } from '@/hooks/useAsync'
import { formatBytes, saveBlob } from '@/lib/download'
import { formatDateTime, formatDuration } from '@/lib/utils'

/**
 * 终态 Run 的「结果」视图（用户原话：跑完了点进去，只想拿测试结果）。
 *
 * 只放 4 个块：**结论 / 失败项 / 产物 / 报告**；执行链路、Task、Attempt、日志全部在
 * 「执行细节」页签里（二级）。产物下载按契约走 `GET /runs/{id}/files?path=`，
 * 清单来自 `GET /runs/{id}/artifacts`（前端**显式列全**，不只给 report.md）。
 */

const KIND_LABEL: Record<Artifact['kind'], string> = {
  report: '报告',
  result: '结果',
  log: '日志',
  raw: '原始数据',
  other: '其它',
}

const KIND_TONE: Record<Artifact['kind'], 'info' | 'ok' | 'muted' | 'warn'> = {
  report: 'info',
  result: 'ok',
  log: 'muted',
  raw: 'warn',
  other: 'muted',
}

/** 结果页里最该被点到的几个文件：排前面并且高亮 */
const HEADLINE = ['report.json', 'report.md', 'metadata.json', 'events.jsonl']

function fileUrl(runId: string, path: string): string {
  return `${API_BASE}/runs/${encodeURIComponent(runId)}/files?path=${encodeURIComponent(path)}`
}

function fileName(path: string): string {
  return path.split('/').pop() ?? path
}

/** 一句结论：成功就是成功，失败要说清"为什么" */
function conclusion(run: Run): { tone: 'ok' | 'warn' | 'err' | 'muted'; text: string } {
  const failed = (run.tasks ?? []).filter((task) => task.status === 'failed')
  switch (run.status) {
    case 'succeeded':
      return {
        tone: 'ok',
        text: `全部 ${run.task_counts?.total ?? run.tasks?.length ?? 0} 个 Task 通过，可以出报告。`,
      }
    case 'completed_with_errors':
      return {
        tone: 'warn',
        text: `${failed.length} 个 Task 失败（其余通过）：${failed.map((task) => task.name).join('、') || '—'}`,
      }
    case 'failed':
      return {
        tone: 'err',
        text: run.error
          ? `${run.error.code}：${run.error.message}`
          : `${failed.length} 个 Task 失败：${failed.map((task) => task.name).join('、') || '—'}`,
      }
    case 'service_unavailable':
      return { tone: 'err', text: '被测服务不可用，Test 未执行（Pre-Check 未通过）。' }
    case 'canceled':
      return { tone: 'muted', text: 'Run 被取消，结果不完整。' }
    default:
      return { tone: 'muted', text: `当前状态 ${run.status}` }
  }
}

export function RunResultView({ run }: { run: Run }) {
  const runId = run.id
  const artifacts = useAsync(() => api.listRunArtifacts(runId), [runId])
  const report = useAsync(() => api.getReport(runId), [runId])
  const [downloading, setDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState<ApiErrorBody | null>(null)
  const [downloadNotice, setDownloadNotice] = useState<string | null>(null)

  /**
   * 打包下载：与报告页同一套逻辑（带鉴权 fetch → Blob → a[download]），
   * 不是裸 `<a href>` 直链 —— dev/OIDC 下裸链会 401，用户只会看到一段 JSON。
   */
  const downloadBundle = async () => {
    setDownloading(true)
    setDownloadError(null)
    setDownloadNotice(null)
    try {
      const blob = await api.downloadBundle(runId)
      const name = `${runId}.zip`
      saveBlob(blob, name)
      setDownloadNotice(`${name} · ${formatBytes(blob.size)} 已开始下载`)
    } catch (e) {
      setDownloadError(toApiError(e))
    } finally {
      setDownloading(false)
    }
  }

  const counts = run.task_counts
  const verdict = conclusion(run)
  const items = artifacts.data?.items ?? []
  const headline = items.filter((item) => HEADLINE.includes(fileName(item.path)))
  const rest = items.filter((item) => !HEADLINE.includes(fileName(item.path)))

  return (
    <>
      {/* 1) 结论：成功 / 失败 / 为什么失败 + 计数 */}
      <Card data-testid="run-result-verdict">
        <CardHeader
          tone={verdict.tone === 'ok' ? 'ok' : verdict.tone === 'err' ? 'err' : 'warn'}
          title="结论"
          sub={`${formatDuration(
            run.started_at && run.finished_at
              ? Math.round((new Date(run.finished_at).getTime() - new Date(run.started_at).getTime()) / 1000)
              : null,
          )} · ${formatDateTime(run.finished_at)}`}
          actions={<RunStatusBadge status={run.status} />}
        />
        <CardBody className="flex flex-col gap-4">
          <p className="text-sm leading-relaxed text-fg">{verdict.text}</p>
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {[
              ['Task 总数', counts?.total ?? 0, 'muted'],
              ['成功', counts?.succeeded ?? 0, 'ok'],
              ['失败', counts?.failed ?? 0, 'err'],
              ['跳过', counts?.skipped ?? 0, 'warn'],
              ['进行中/排队', (counts?.pending ?? 0) + (counts?.running ?? 0), 'muted'],
            ].map(([label, value, tone]) => (
              <div key={String(label)} className="rounded-md border border-line-soft bg-surface-2 px-3 py-2">
                <dt className="text-2xs text-fg-3">{label}</dt>
                <dd className="mt-1 flex items-baseline gap-2">
                  <span className="text-[22px] font-semibold tabular-nums leading-none text-fg">{value}</span>
                  <Badge tone={tone as 'muted'}>{String(label)}</Badge>
                </dd>
              </div>
            ))}
          </dl>
        </CardBody>
      </Card>

      {/* 2) 失败项：只列失败的，给错误码与原因；0 失败时压成一行绿字，不再用红色大块占屏（C6） */}
      {report.data && report.data.failures.length === 0 ? (
        <p
          data-testid="run-result-no-failures"
          role="status"
          className="flex items-center gap-2 rounded-lg border border-success/25 bg-success-weak px-4 py-2 text-xs font-medium text-success-text"
        >
          <Check className="size-3.5" aria-hidden />
          无失败项
        </p>
      ) : (
        <Card data-testid="run-result-failures">
          <CardHeader tone="err" title="失败项" sub={report.data ? `${report.data.failures.length} 条` : undefined} />
          {report.error ? (
            <CardBody>
              <ErrorState error={report.error} onRetry={report.reload} compact />
            </CardBody>
          ) : report.loading && !report.data ? (
            <LoadingBlock label="加载失败清单…" />
          ) : (
            <TableWrap>
              <Table>
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>Task</TH>
                    <TH>原因</TH>
                    <TH>错误码</TH>
                  </TR>
                </THead>
                <TBody>
                  {report.data!.failures.map((failure) => (
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
        </Card>
      )}

      {/* 3) 产物：显式列全（report.json / report.md / metadata.json / events.jsonl / 每个 Attempt 日志 / Deploy 日志） */}
      <Card data-testid="run-result-artifacts">
        <CardHeader
          tone="info"
          title="产物下载"
          sub={artifacts.data ? `${items.length} 个文件 · 共 ${formatBytes(artifacts.data.total_size_bytes)}` : undefined}
          actions={
            <div className="flex flex-wrap items-center justify-end gap-2">
              <Button
                size="sm"
                variant="primary"
                data-testid="download-bundle"
                icon={<Download className="size-3" aria-hidden />}
                loading={downloading}
                onClick={() => void downloadBundle()}
              >
                {downloading ? '打包中…' : '打包下载 .zip'}
              </Button>
              <a
                className={buttonClassName({ size: 'sm', variant: 'ghost' })}
                href={`${API_BASE}/runs/${encodeURIComponent(runId)}/report.md`}
                target="_blank"
                rel="noreferrer"
              >
                report.md <ArrowUpRight className="size-3" aria-hidden />
              </a>
            </div>
          }
        />
        {downloadError ? (
          <CardBody className="pb-0">
            <ErrorState error={downloadError} onRetry={() => void downloadBundle()} compact />
          </CardBody>
        ) : null}
        {downloadNotice ? (
          <p className="px-5 pt-3 text-2xs text-fg-3" role="status">
            {downloadNotice}
          </p>
        ) : null}
        {artifacts.error ? (
          <CardBody>
            <ErrorState error={artifacts.error} onRetry={artifacts.reload} compact />
          </CardBody>
        ) : artifacts.loading && !artifacts.data ? (
          <LoadingBlock label="加载产物清单…" />
        ) : items.length === 0 ? (
          <CardBody>
            <EmptyState title="还没有产物" description="产物在 Run 结束后写出；存储未接线时这里会是空的。" />
          </CardBody>
        ) : (
          <>
            {headline.length > 0 ? (
              <CardBody className="flex flex-wrap gap-2">
                {headline.map((item) => (
                  <a
                    key={item.path}
                    data-testid="artifact-download"
                    href={fileUrl(runId, item.path)}
                    className="flex items-center gap-2 rounded-md border border-line bg-surface-2 px-3 py-2 text-xs text-fg-2 transition-colors duration-150 hover:border-line-strong hover:text-fg"
                  >
                    <FileText className="size-3.5 text-primary" aria-hidden />
                    <span className="font-mono">{fileName(item.path)}</span>
                    <span className="text-2xs text-fg-3">{formatBytes(item.size_bytes)}</span>
                    <Download className="size-3.5 text-fg-3" aria-hidden />
                  </a>
                ))}
              </CardBody>
            ) : null}
            {rest.length > 0 ? (
              <TableWrap>
                <Table>
                  <THead>
                    <TR className="hover:bg-transparent">
                      <TH>文件</TH>
                      <TH>类型</TH>
                      <TH>大小</TH>
                      <TH className="text-right">下载</TH>
                    </TR>
                  </THead>
                  <TBody>
                    {rest.map((item) => (
                      <TR key={item.path}>
                        <TD className="break-all font-mono text-2xs text-fg">{item.path.replace(`${runId}/`, '')}</TD>
                        <TD>
                          <Badge tone={KIND_TONE[item.kind]}>{KIND_LABEL[item.kind]}</Badge>
                        </TD>
                        <TD className="tabular-nums text-2xs">{formatBytes(item.size_bytes)}</TD>
                        <TD className="text-right">
                          <a
                            data-testid="artifact-download"
                            className={buttonClassName({ size: 'sm', variant: 'ghost' })}
                            href={fileUrl(runId, item.path)}
                          >
                            下载
                          </a>
                        </TD>
                      </TR>
                    ))}
                  </TBody>
                </Table>
              </TableWrap>
            ) : null}
          </>
        )}
        <CardFooter>
          <span>这些都是这次 Run 落下的文件；点一下就开始下载，不需要再回报告页翻。</span>
        </CardFooter>
      </Card>

      {/* 4) 报告入口 */}
      <Card data-testid="run-result-report">
        <CardHeader tone="muted" title="报告" sub="report.json 组织与索引（不做指标聚合）" />
        <CardBody className="flex flex-wrap items-center gap-2">
          <Link to={`/runs/${encodeURIComponent(runId)}/report`}>
            <Button size="sm" variant="primary" icon={<FileText className="size-3" aria-hidden />}>
              打开报告页
            </Button>
          </Link>
          <a
            className={buttonClassName({ size: 'sm' })}
            href={fileUrl(runId, `${runId}/report.json`)}
            target="_blank"
            rel="noreferrer"
          >
            report.json
          </a>
          <a
            className={buttonClassName({ size: 'sm' })}
            href={fileUrl(runId, `${runId}/report.md`)}
            target="_blank"
            rel="noreferrer"
          >
            report.md
          </a>
        </CardBody>
      </Card>
    </>
  )
}
