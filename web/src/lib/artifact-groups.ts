/**
 * 产物按**来源**分组（只影响展示，不过滤任何文件）。
 *
 * 后端落盘结构（D-59 / design/views/03-data-view.md §4.4.1）：
 *   deploy/logs/<pod>[.<container>].log + deploy/pods.json   → 部署日志
 *   tasks/<task>/attempts/<n>/logs/*（也兼容 mock 的 …/stdout.log） → 测试日志
 *   report.json / report.md / metadata.json / events.jsonl / result.json … → 报告与数据
 *
 * 每份产物必须且只能进一组：未知路径一律落「报告与数据」，不允许丢文件。
 */
import type { Artifact } from '@/api/types'

export type ArtifactGroupKey = 'deploy' | 'tests' | 'reports'

export interface ArtifactGroupDef {
  key: ArtifactGroupKey
  label: string
  description: string
}

export const ARTIFACT_GROUPS: readonly ArtifactGroupDef[] = [
  { key: 'deploy', label: '部署日志', description: 'deploy/logs/*.log 与 deploy/pods.json' },
  { key: 'tests', label: '测试日志', description: 'tasks/.../logs/*（每个 Attempt 一份）' },
  { key: 'reports', label: '报告与数据', description: 'report.json / report.md / metadata.json / events.jsonl 等' },
] as const

/** `path` 是 storage-root 相对路径；显示与分组都先剥掉 `<run_id>/` 前缀。 */
export function relativeArtifactPath(path: string, runId: string): string {
  const norm = path.replace(/\\/g, '/').replace(/^\.?\//, '')
  const prefix = `${runId}/`
  return norm.startsWith(prefix) ? norm.slice(prefix.length) : norm
}

export function artifactGroupOf(path: string, runId: string): ArtifactGroupKey {
  const rel = relativeArtifactPath(path, runId)
  if (/^deploy\/logs\/.+\.log$/i.test(rel) || rel === 'deploy/pods.json' || rel.endsWith('/deploy/pods.json')) {
    return 'deploy'
  }
  // tasks/.../logs/* 是 Tester 容器日志；mock 的历史形状是 tasks/.../stdout.log，也归测试日志。
  if (rel.startsWith('tasks/') && (/\/logs\//i.test(rel) || /\.log$/i.test(rel))) return 'tests'
  return 'reports'
}

export function groupArtifacts(items: Artifact[], runId: string): Record<ArtifactGroupKey, Artifact[]> {
  const groups: Record<ArtifactGroupKey, Artifact[]> = { deploy: [], tests: [], reports: [] }
  for (const item of items) groups[artifactGroupOf(item.path, runId)].push(item)
  return groups
}
