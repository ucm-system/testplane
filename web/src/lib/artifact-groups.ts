/**
 * 产物按**来源**分组（只影响展示，不过滤任何文件）。
 *
 * v2 落盘结构（§8.1 / D-70；`path` 相对 `<storage-root>/<task_id>/`）：
 *   attempts/<n>/runners/<seq>/logs/<pod>.log（部署 Pod 日志，文件名 ≠ stdout.log）
 *   attempts/<n>/runners/<seq>/logs/stdout.log              → runner 日志
 *   attempts/<n>/exports/<seq>.exports.env                  → exports 落档
 *   report.json / report.md / metadata.json / events.jsonl … → 报告与数据
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
  { key: 'deploy', label: '部署日志', description: 'deploy runner 落盘的 Pod 日志（attempts/<n>/runners/<deploy_seq>/logs/<pod>.log）' },
  { key: 'tests', label: 'runner 日志', description: 'attempts/<n>/runners/<seq>/logs/stdout.log（每个 runner 一份）' },
  { key: 'reports', label: '报告与数据', description: 'report.json / report.md / metadata.json / events.jsonl / exports 等' },
] as const

/** `path` 是 task 目录相对路径；显示时原样（已不含 task 前缀）。 */
export function relativeArtifactPath(path: string): string {
  return path.replace(/\\/g, '/').replace(/^\.?\//, '')
}

export function artifactGroupOf(path: string): ArtifactGroupKey {
  const rel = relativeArtifactPath(path)
  // runner 日志的文件名恒为 stdout.log（§8.6）；部署 Pod 日志按 Pod 一份（<pod_name>.log）
  if (/\/logs\/.+\.log$/i.test(rel) && !rel.endsWith('/logs/stdout.log')) return 'deploy'
  if (/\/logs\//i.test(rel) || /\.log$/i.test(rel)) return 'tests'
  return 'reports'
}

export function groupArtifacts(items: Artifact[]): Record<ArtifactGroupKey, Artifact[]> {
  const groups: Record<ArtifactGroupKey, Artifact[]> = { deploy: [], tests: [], reports: [] }
  for (const item of items) groups[artifactGroupOf(item.path)].push(item)
  return groups
}
