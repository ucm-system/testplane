import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { ChevronRight, Download } from 'lucide-react'
import { API_BASE, api } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, Artifact, TaskDetail } from '@/api/types'
import { RunnerResultPanel } from '@/components/app/RunnerResultPanel'
import { Badge } from '@/components/ui/badge'
import { Button, buttonClassName } from '@/components/ui/button'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { TaskStatusBadge } from '@/components/app/StatusBadge'
import { useAsync } from '@/hooks/useAsync'
import { ARTIFACT_GROUPS, groupArtifacts, relativeArtifactPath } from '@/lib/artifact-groups'
import { artifactViewKindOf } from '@/components/app/ArtifactViewer'
import { formatBytes } from '@/lib/download'
import { stripErrorCodePrefix } from '@/lib/error-format'
import { formatDateTime, formatDuration, runDuration } from '@/lib/utils'
import { cn } from '@/lib/utils'

/**
 * 终态 Task 的「结果」视图（用户 2026-11-11：跑完了点进去只要三样东西）：
 *
 *  1) **结论**：成功/失败一句话 + 计数 + 每个 runner 谁成功谁失败（runner 明细表，含错误码）；
 *  2) **结果渲染**：每个测试 runner 的 result.json / result.html / result.csv 三选一内联渲染（D-74）；
 *  3) **产物下载**：清单显式列全，「查看」进整页查看器（v2 不再弹窗、不再裸链 API URL）。
 *
 * v2 取消了独立的「报告」页签（report.json 只是平台产物之一，不再强制依赖，D-74）；
 * runner 管道时间线、attempt、日志、事件全部在「执行细节」页签（二级）。
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

/** 一句结论：成功就是成功，失败要说清"为什么"（直接从 runner 执行记录推导，不依赖 report.json） */
function conclusion(task: TaskDetail): { tone: 'ok' | 'warn' | 'err' | 'muted'; text: string } {
  const counts = task.runner_counts
  const failedNames = (task.runners ?? []).filter((r) => r.status === 'failed').map((r) => r.name)
  switch (task.status) {
    case 'succeeded':
      return {
        tone: 'ok',
        text: `全部 ${counts?.total ?? 0} 个 runner 通过（attempt #${task.current_attempt}），可以出报告。`,
      }
    case 'failed':
      return {
        tone: 'err',
        text: task.error
          ? `${task.error.code}：${stripErrorCodePrefix(task.error.code, task.error.message)}`
          : failedNames.length > 0
            ? `${failedNames.length} 个 runner 失败：${failedNames.join('、')}`
            : 'runner 管道有失败。',
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
  const [downloadError, setDownloadError] = useState<ApiErrorBody | null>(null)
  const [downloadNotice, setDownloadNotice] = useState<string | null>(null)

  /** 「查看」= 整页查看器（v2 不再弹窗、不再裸链 API URL）：path 必带，attempt / seq 用于定位。 */
  const openViewer = (item: Artifact) => {
    const params = new URLSearchParams()
    params.set('path', item.path)
    if (item.attempt_no != null) params.set('attempt', String(item.attempt_no))
    if (item.runner_seq != null) params.set('seq', String(item.runner_seq))
    void navigate(`/tasks/${encodeURIComponent(taskId)}/file?${params.toString()}`)
  }

  /**
   * 打包下载：带鉴权 fetch → Blob → a[download]，不是裸 `<a href>` 直链 ——
   * dev/OIDC 下裸链会 401，用户只会看到一段 JSON。
   */
  const downloadBundle = async () => {
    setDownloadError(null)
    setDownloadNotice(null)
    try {
      const blob = await api.downloadBundle(taskId)
      const name = `${taskId}.zip`
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = name
      anchor.click()
      URL.revokeObjectURL(url)
      setDownloadNotice(`${name} · ${formatBytes(blob.size)} 已开始下载`)
    } catch (e) {
      setDownloadError(toApiError(e))
    }
  }

  const counts = task.runner_counts
  const verdict = conclusion(task)
  const items = artifacts.data?.items ?? []
  // 当前 attempt 的产物（重试后历史 attempt 的文件按 attempt_no 过滤掉，只展示最新一次）
  const currentItems = items.filter((item) => item.attempt_no == null || item.attempt_no === task.current_attempt)
  // 分组只影响展示：部署日志 / runner 日志 / 报告与数据，三组都渲染，文件一个不少。
  const groups = groupArtifacts(currentItems)
  const runners = task.runners ?? []

  return (
    <>
      {/* 1) 结论：一句话 + 计数 */}
      <Card data-testid="task-result-verdict">
        <CardHeader
          tone={verdict.tone === 'ok' ? 'ok' : verdict.tone === 'err' ? 'err' : 'warn'}
          title="结论"
          sub={`attempt #${task.current_attempt} · ${formatDuration(runDuration(task))} · ${formatDateTime(task.finished_at)}`}
          actions={<TaskStatusBadge status={task.status} />}
        />
        <CardBody className="flex flex-col gap-4">
          <p className="text-sm leading-[16px] text-fg">{verdict.text}</p>
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {[
              ['runner 总数', counts?.total ?? 0],
              ['成功', counts?.succeeded ?? 0],
              ['失败', counts?.failed ?? 0],
              ['跳过', counts?.skipped ?? 0],
              ['等待/执行', (counts?.pending ?? 0) + (counts?.running ?? 0)],
            ].map(([label, value]) => (
              <div key={String(label)} className="rounded-md border border-line-soft bg-surface-2 px-3 py-2">
                <dt className="text-2xs text-fg-3">{label}</dt>
                <dd className="mt-1 text-xl font-semibold tabular-nums leading-none text-fg">{value}</dd>
              </div>
            ))}
          </dl>
        </CardBody>
      </Card>

      {/* 2) runner 结果（用户 2026-11-11：deploy/precheck 也要有详情）：全部 runner 一个面板 */}
      <Card data-testid="task-result-render">
        <CardHeader tone="info" title="runner 结果" />
        {runners.length === 0 ? (
          <CardBody>
            <p className="text-xs text-fg-3">还没有 runner 执行记录。</p>
          </CardBody>
        ) : artifacts.error ? (
          <CardBody>
            <ErrorState error={artifacts.error} onRetry={artifacts.reload} compact />
          </CardBody>
        ) : artifacts.loading && !artifacts.data ? (
          <LoadingBlock label="加载产物清单…" />
        ) : (
          <div className="flex flex-col px-5 py-1">
            {runners.map((runner) => (
              <RunnerResultPanel key={runner.seq} taskId={taskId} runner={runner} attemptNo={task.current_attempt} artifacts={currentItems} />
            ))}
          </div>
        )}
      </Card>

      {/* 3) 产物下载：显式列全（report.json / report.md / metadata.json / events.jsonl / 各 runner 日志与 exports / 部署 Pod 日志） */}
      <Card data-testid="task-result-artifacts">
        <CardHeader
          tone="info"
          title="产物下载"
          sub={artifacts.data ? `${currentItems.length} 个文件 · 共 ${formatBytes(artifacts.data.total_size_bytes ?? 0)}` : undefined}
          actions={
            <Button
              size="sm"
              variant="primary"
              data-testid="download-bundle"
              icon={<Download className="size-3" aria-hidden />}
              onClick={() => void downloadBundle()}
            >
              打包下载 .zip
            </Button>
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
        ) : currentItems.length === 0 ? (
          <CardBody>
            <EmptyState title="还没有产物" description="Task 结束后产物会出现在这里。" />
          </CardBody>
        ) : (
          <div className="flex flex-col gap-2 px-5 py-3" data-testid="artifact-groups">
            {ARTIFACT_GROUPS.map((group) => {
              const rows = groups[group.key]
              // 默认折叠：三组加起来动辄十几个文件，展开会把下面内容全推下去
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
                                {/* 整页查看（v2 不再弹窗/裸链）：json 树 / csv 表 / html sandbox / 文本（≤2MB） */}
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
      </Card>
    </>
  )
}
