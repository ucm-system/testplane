import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ArrowUpRight, Check, ChevronRight, Download, FileText } from 'lucide-react'
import { API_BASE, api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Artifact, Report, TaskDetail } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { TaskStatusBadge } from '@/components/app/StatusBadge'
import { useAsync } from '@/hooks/useAsync'
import { ARTIFACT_GROUPS, groupArtifacts, relativeArtifactPath } from '@/lib/artifact-groups'
import { artifactViewKindOf } from '@/components/app/ArtifactViewer'
import { formatBytes, saveBlob } from '@/lib/download'
import { cn, formatDateTime, formatDuration } from '@/lib/utils'

/**
 * 终态 Task 的「结果」视图（用户原话：跑完了点进去，只想拿测试结果）。
 *
 * 只放 4 个块：**结论 / 失败项 / 产物 / 报告**；runner 管道时间线、attempt、日志全部在
 * 「执行细节」页签里（二级）。产物下载按契约走 `GET /tasks/{id}/files?path=`，
 * 清单来自 `GET /tasks/{id}/artifacts`（前端**显式列全**，不只给 report.md）。
 * 「查看」不再弹窗：导航到整页查看器 `/tasks/{id}/file?path=…`（v2，URL 可收藏可分享）。
 */

const KIND_LABEL: Record<Artifact['kind'], string> = {
  report: '报告',
  result: '结果',
  log: '日志',
  raw: '原始数据',
  export: 'exports',
  other: '其它',
}

const KIND_TONE: Record<Artifact['kind'], 'info' | 'ok' | 'muted' | 'warn'> = {
  report: 'info',
  result: 'ok',
  log: 'muted',
  raw: 'warn',
  export: 'info',
  other: 'muted',
}

/** `/files?path=` 的下载地址（`path` 用 artifacts 返回的 task 目录相对路径） */
function fileUrl(taskId: string, path: string): string {
  return `${API_BASE}/tasks/${encodeURIComponent(taskId)}/files?path=${encodeURIComponent(path)}`
}

/** 一句结论：成功就是成功，失败要说清"为什么" */
function conclusion(
  task: TaskDetail,
  report: Report | null,
): { tone: 'ok' | 'warn' | 'err' | 'muted'; text: string } {
  const counts = task.runner_counts
  switch (task.status) {
    case 'succeeded':
      return {
        tone: 'ok',
        text: `全部 ${counts?.total ?? report?.counts.total ?? 0} 个 runner 通过（attempt #${task.current_attempt}），可以出报告。`,
      }
    case 'failed':
      return {
        tone: 'err',
        text: task.error
          ? `${task.error.code}：${task.error.message}`
          : report && report.failures.length > 0
            ? `${report.failures.length} 个 runner 失败：${report.failures.map((f) => f.name).join('、')}`
            : 'runner 管道有失败（原「部分失败」语义已并入 failed，D-73）。',
      }
    case 'service_unavailable':
      return { tone: 'err', text: '被测服务不可用（pre_check 未通过或部署被阻塞），测试未执行。' }
    case 'canceled':
      return { tone: 'muted', text: 'Task 被取消（service-clean 已收口），结果不完整。' }
    default:
      return { tone: 'muted', text: `当前状态 ${task.status}` }
  }
}

export function TaskResultView({ task }: { task: TaskDetail }) {
  const taskId = task.id
  const navigate = useNavigate()
  const artifacts = useAsync(() => api.listTaskArtifacts(taskId), [taskId])
  const report = useAsync(() => api.getTaskReport(taskId), [taskId])
  const [downloading, setDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState<ApiErrorBody | null>(null)
  const [downloadNotice, setDownloadNotice] = useState<string | null>(null)

  /** 「查看」= 整页查看器（v2 不再弹窗）：path 必带，attempt / seq 用于定位与展示。 */
  const openViewer = (item: Artifact) => {
    const params = new URLSearchParams()
    params.set('path', item.path)
    if (item.attempt_no != null) params.set('attempt', String(item.attempt_no))
    if (item.runner_seq != null) params.set('seq', String(item.runner_seq))
    void navigate(`/tasks/${encodeURIComponent(taskId)}/file?${params.toString()}`)
  }

  /**
   * 打包下载：与报告页同一套逻辑（带鉴权 fetch → Blob → a[download]），
   * 不是裸 `<a href>` 直链 —— dev/OIDC 下裸链会 401，用户只会看到一段 JSON。
   */
  const downloadBundle = async () => {
    setDownloading(true)
    setDownloadError(null)
    setDownloadNotice(null)
    try {
      const blob = await api.downloadBundle(taskId)
      const name = `${taskId}.zip`
      saveBlob(blob, name)
      setDownloadNotice(`${name} · ${formatBytes(blob.size)} 已开始下载`)
    } catch (e) {
      setDownloadError(toApiError(e))
    } finally {
      setDownloading(false)
    }
  }

  const counts = task.runner_counts
  const verdict = conclusion(task, report.data ?? null)
  const items = artifacts.data?.items ?? []
  // 分组只影响展示：部署日志 / runner 日志 / 报告与数据，三组都渲染，文件一个不少。
  const groups = groupArtifacts(items)

  return (
    <>
      {/* 1) 结论：成功 / 失败 / 为什么失败 + 计数 */}
      <Card data-testid="task-result-verdict">
        <CardHeader
          tone={verdict.tone === 'ok' ? 'ok' : verdict.tone === 'err' ? 'err' : 'warn'}
          title="结论"
          sub={`attempt #${report.data?.attempt_no ?? task.current_attempt} · ${formatDuration(
            task.started_at && task.finished_at
              ? Math.round((new Date(task.finished_at).getTime() - new Date(task.started_at).getTime()) / 1000)
              : null,
          )} · ${formatDateTime(task.finished_at)}`}
          actions={<TaskStatusBadge status={task.status} />}
        />
        <CardBody className="flex flex-col gap-4">
          <p className="text-sm leading-[16px] text-fg">{verdict.text}</p>
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {[
              ['runner 总数', counts?.total ?? 0, 'muted'],
              ['成功', counts?.succeeded ?? 0, 'ok'],
              ['失败', counts?.failed ?? 0, 'err'],
              ['跳过', counts?.skipped ?? 0, 'warn'],
              ['等待/执行', (counts?.pending ?? 0) + (counts?.running ?? 0), 'muted'],
            ].map(([label, value, tone]) => (
              <div key={String(label)} className="rounded-md border border-line-soft bg-surface-2 px-3 py-2">
                <dt className="text-2xs text-fg-3">{label}</dt>
                <dd className="mt-1 flex items-baseline gap-2">
                  <span className="text-xl font-semibold tabular-nums leading-none text-fg">{value}</span>
                  <Badge tone={tone as 'muted'}>{String(label)}</Badge>
                </dd>
              </div>
            ))}
          </dl>
        </CardBody>
      </Card>

      {/* 2) 失败项：只列失败的，给错误码与原因；0 失败时压成一行中性提示（C6） */}
      {report.data && report.data.failures.length === 0 ? (
        <p
          data-testid="task-result-no-failures"
          role="status"
          className="flex items-center gap-2 rounded-xl border border-line bg-transparent px-4 py-2.5 text-xs font-medium text-fg-2"
        >
          <span className="size-1.5 shrink-0 rounded-full bg-success" aria-hidden />
          <Check className="size-3.5 text-success-text" aria-hidden />
          无失败项
        </p>
      ) : (
        <Card data-testid="task-result-failures">
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
                    <TH>seq</TH>
                    <TH>runner</TH>
                    <TH>原因</TH>
                    <TH>错误码</TH>
                  </TR>
                </THead>
                <TBody>
                  {report.data!.failures.map((failure) => (
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
        </Card>
      )}

      {/* 3) 产物：显式列全（report.json / report.md / metadata.json / events.jsonl / 各 runner 的日志与 result.json / 部署 Pod 日志） */}
      <Card data-testid="task-result-artifacts">
        <CardHeader
          tone="info"
          title="产物下载"
          sub={artifacts.data ? `${items.length} 个文件 · 共 ${formatBytes(artifacts.data.total_size_bytes ?? 0)}` : undefined}
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
                href={`${API_BASE}/tasks/${encodeURIComponent(taskId)}/report.md`}
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
            <EmptyState title="还没有产物" description="产物在 Task 结束后写出；存储未接线时这里会是空的。" />
          </CardBody>
        ) : (
          <div className="flex flex-col gap-2 px-5 py-3" data-testid="artifact-groups">
            {ARTIFACT_GROUPS.map((group) => {
              const rows = groups[group.key]
              // 默认折叠（用户 2026-11-09）：三组加起来动辄十几个文件，展开会把下面内容全推下去；
              // 折叠后只留「组名 + 数量 + 说明」，点开才看文件表，数量仍然一眼可见。
              return (
                <details
                  key={group.key}
                  data-testid={`artifact-group-${group.key}`}
                  className="group rounded-md border border-line-soft"
                >
                  <summary className="flex list-none cursor-pointer flex-wrap items-center gap-2 px-3 py-2 text-xs">
                    <ChevronRight className="size-3 text-fg-3 transition-transform group-open:rotate-90" aria-hidden />
                    <span className="font-medium text-fg">{group.label}</span>
                    <Badge tone="muted">{rows.length}</Badge>
                    <span className="text-2xs text-fg-3">{group.description}</span>
                  </summary>
                  {rows.length === 0 ? (
                    <p className="border-t border-line-soft px-3 py-2 text-2xs text-fg-3">这一组没有文件</p>
                  ) : (
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
                          {rows.map((item) => (
                            <TR key={item.path}>
                              <TD className="break-all font-mono text-2xs text-fg">
                                {relativeArtifactPath(item.path)}
                              </TD>
                              <TD>
                                <Badge tone={KIND_TONE[item.kind]}>{KIND_LABEL[item.kind]}</Badge>
                              </TD>
                              <TD className="tabular-nums text-2xs">{formatBytes(item.size_bytes)}</TD>
                              <TD className="text-right">
                                {/* 整页查看（v2 不再弹窗）：json 树 / csv 表 / html sandbox / 文本（≤2MB）；二进制不提供预览 */}
                                {artifactViewKindOf(item.path) !== 'binary' ? (
                                  <button
                                    type="button"
                                    data-testid="artifact-view"
                                    className={cn(buttonClassName({ size: 'sm', variant: 'ghost' }), 'mr-1')}
                                    onClick={() => openViewer(item)}
                                  >
                                    查看
                                  </button>
                                ) : null}
                                <a
                                  data-testid="artifact-download"
                                  className={buttonClassName({ size: 'sm', variant: 'ghost' })}
                                  href={fileUrl(taskId, item.path)}
                                >
                                  下载
                                </a>
                              </TD>
                            </TR>
                          ))}
                        </TBody>
                      </Table>
                    </TableWrap>
                  )}
                </details>
              )
            })}
          </div>
        )}
        <CardFooter>
          <span>这些都是这次 Task 落下的文件；「查看」进整页查看器（URL 可收藏），下载直接开始。</span>
        </CardFooter>
      </Card>

      {/* 4) 报告入口 */}
      <Card data-testid="task-result-report">
        <CardHeader tone="muted" title="报告" sub="report.json 组织与索引（不做指标聚合，默认最后一个 attempt）" />
        <CardBody className="flex flex-wrap items-center gap-2">
          <Link to={`/tasks/${encodeURIComponent(taskId)}/report`}>
            <Button size="sm" variant="primary" icon={<FileText className="size-3" aria-hidden />}>
              打开报告页
            </Button>
          </Link>
          <a
            className={buttonClassName({ size: 'sm' })}
            href={fileUrl(taskId, 'report.json')}
            target="_blank"
            rel="noreferrer"
          >
            report.json
          </a>
          <a
            className={buttonClassName({ size: 'sm' })}
            href={fileUrl(taskId, 'report.md')}
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
