import { useMemo } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { ArrowLeft } from 'lucide-react'
import { api } from '@/api'
import type { Artifact } from '@/api/types'
import {
  ArtifactDownloadButton,
  ArtifactPreviewBody,
  artifactViewKindOf,
  formatPreviewLimit,
} from '@/components/app/ArtifactViewer'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { formatBytes } from '@/lib/download'
import { relativeArtifactPath } from '@/lib/artifact-groups'
import { cn } from '@/lib/utils'

/**
 * 整页文件查看器（`/tasks/:taskId/file?path=&attempt=&seq=`）。
 *
 * v2：产物清单的「查看」从弹窗改为导航到本页 —— 大文件/长日志在整页里读，URL 可收藏、
 * 可直接粘贴给同事。渲染复用 `ArtifactPreviewBody`（与弹窗同一份按扩展名分发的实现，
 * 守 §1.3：不做任何按 runner 专属的解析；二进制仅下载，文本预览上限 2MB）。
 *
 * query 参数：
 *  - `path`    必填，task 目录相对路径（产物清单 `Artifact.path` 原样）；
 *  - `attempt` / `seq` 可选，仅用于定位与展示（attempt #n / seq n）。
 */

/** 按扩展名分发的查看形态中文标签（页头徽标用；与弹窗描述同一套口径）。 */
const VIEW_LABEL: Record<string, string> = {
  json: 'JSON',
  csv: 'CSV 表格',
  html: 'HTML（sandbox）',
  text: '文本',
  binary: '二进制（仅下载）',
}

export function FileViewerPage() {
  const { taskId = '' } = useParams<{ taskId: string }>()
  const [searchParams] = useSearchParams()
  const path = searchParams.get('path') ?? ''
  const attempt = searchParams.get('attempt')
  const seq = searchParams.get('seq')

  // 元信息尽力补齐：产物清单里能按 path 命中就用真实的 kind / 大小；
  // 命中不了（清单接口失败 / 路径刚落盘）也不阻塞预览 —— 拉取与渲染只依赖 path。
  const artifacts = useAsync(() => api.listTaskArtifacts(taskId), [taskId])
  const artifact: Artifact = useMemo(() => {
    const found = (artifacts.data?.items ?? []).find((item) => relativeArtifactPath(item.path) === relativeArtifactPath(path))
    if (found) return found
    return {
      path,
      kind: artifactViewKindOf(path) === 'binary' ? 'other' : 'raw',
      size_bytes: 0,
      attempt_no: attempt != null && attempt !== '' ? Number(attempt) : null,
      runner_seq: seq != null && seq !== '' ? Number(seq) : null,
    }
  }, [artifacts.data, path, attempt, seq])

  const kind = artifactViewKindOf(path)

  if (!path.trim()) {
    return (
      <Card>
        <CardHeader tone="warn" title="缺少文件路径" />
        <CardBody>
          <EmptyState
            title="没有指定要查看的文件"
            description="本页需要 ?path= 参数（task 目录相对路径）；从 Task 详情的结果页点「查看」进入即可。"
            action={
              <Link to={`/tasks/${encodeURIComponent(taskId)}`}>
                <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
                  返回 Task 详情
                </Button>
              </Link>
            }
          />
        </CardBody>
      </Card>
    )
  }

  return (
    <>
      {/* 面包屑：Task 详情 → 文件（path 可能很长，允许换行） */}
      <div className="flex flex-wrap items-center gap-2">
        <Link to={`/tasks/${encodeURIComponent(taskId)}`} data-testid="file-viewer-back">
          <Button size="sm" icon={<ArrowLeft className="size-3" aria-hidden />}>
            返回 Task 详情
          </Button>
        </Link>
        <span className="text-fg-3" aria-hidden>
          /
        </span>
        <span data-testid="file-viewer-path" className="min-w-0 break-all font-mono text-xs text-fg">
          {relativeArtifactPath(path)}
        </span>
      </div>

      {/* 整页查看器吃满视口：卡片 + 预览体 flex 拉伸（弹窗形态的固定高度在这里不适用） */}
      <div className="flex min-h-[calc(100vh-9.5rem)] flex-col">
        <Card className="flex flex-1 flex-col">
          <CardHeader
            tone={kind === 'binary' ? 'muted' : 'info'}
            title={
              <span className="flex flex-wrap items-center gap-2">
                文件查看
                <Badge tone="muted">{VIEW_LABEL[kind]}</Badge>
                {artifact.size_bytes > 0 ? <Badge tone="muted">{formatBytes(artifact.size_bytes)}</Badge> : null}
                {artifact.attempt_no != null ? <Badge tone="muted">attempt #{artifact.attempt_no}</Badge> : null}
                {artifact.runner_seq != null ? <Badge tone="muted">seq {artifact.runner_seq}</Badge> : null}
              </span>
            }
            sub={
              kind !== 'binary'
                ? `通用预览（按扩展名分发）· 上限 ${formatPreviewLimit()}，超出请下载`
                : '二进制文件不提供预览，请下载后用本地工具打开'
            }
            actions={<ArtifactDownloadButton taskId={taskId} artifact={artifact} />}
          />
          <CardBody className={cn('flex min-h-0 flex-1 flex-col gap-3', kind === 'text' ? 'pt-3' : undefined)}>
            <ArtifactPreviewBody taskId={taskId} artifact={artifact} stretch />
          </CardBody>
        </Card>
      </div>

      {artifacts.error ? (
        <p className="text-2xs text-fg-3" role="status">
          文件大小与类型来自 URL 参数（清单加载失败：{artifacts.error.message}）；预览与下载不受影响。
        </p>
      ) : null}
    </>
  )
}
