/**
 * 契约归一：把"线上形态"收敛成 UI 好用的形状。
 * 放在独立文件里，真实实现与 mock 实现共用（避免两份解析逻辑漂移）。
 *
 * 目前只做两件事：
 *  1. 列表信封两种形态（`{items,...}` 与裸数组）都能吃；
 *  2. `Runner`：wire 的 `parameters` / `env` 兼容数组与 map（v2 契约是数组），UI 统一按数组渲染。
 */
import type { Page, Runner, RunnerParameter } from './types'

/** 统一分页信封；也容忍裸数组（后端早期实现）。 */
export function asPage<T>(data: unknown): Page<T> {
  if (Array.isArray(data)) {
    return { items: data as T[], next_cursor: null, has_more: false }
  }
  if (data && typeof data === 'object') {
    const rec = data as Record<string, unknown>
    const items = Array.isArray(rec['items']) ? (rec['items'] as T[]) : []
    const nextCursor = typeof rec['next_cursor'] === 'string' ? rec['next_cursor'] : null
    const hasMore = typeof rec['has_more'] === 'boolean' ? rec['has_more'] : nextCursor !== null
    // offset 分页（D-61）会多回 total/page/page_size：**必须透传**，否则 UI 只能拿 items.length 兜底，共 N 条 就成了假数字。
    const total = typeof rec['total'] === 'number' ? rec['total'] : null
    const page = typeof rec['page'] === 'number' ? rec['page'] : null
    const pageSize = typeof rec['page_size'] === 'number' ? rec['page_size'] : null
    return { items, next_cursor: nextCursor, has_more: hasMore, total, page, page_size: pageSize }
  }
  return { items: [], next_cursor: null, has_more: false }
}

/** `{items:[...]}` / `[...]` 两种形态下取数组。 */
export function asArray<T>(data: unknown, key = 'items'): T[] {
  if (Array.isArray(data)) return data as T[]
  if (data && typeof data === 'object') {
    const value = (data as Record<string, unknown>)[key]
    if (Array.isArray(value)) return value as T[]
  }
  return []
}

const ROLES: Runner['role'][] = ['test', 'pre_check', 'deploy', 'clean']

/** `Runner`（契约形状）→ UI 形状（数组 + 提平的 exclusive / input_format）。 */
export function normalizeRunner(raw: unknown): Runner {
  const rec = (raw ?? {}) as Record<string, unknown>
  const concurrency = (rec['concurrency'] ?? {}) as Record<string, unknown>

  // parameters：v2 是数组（jsonl 校验 schema 的有序列表）；容忍 map 形态（早期实现）
  const parametersRaw = rec['parameters']
  const parameters: RunnerParameter[] = Array.isArray(parametersRaw)
    ? (parametersRaw as RunnerParameter[]).filter(
        (p): p is RunnerParameter => Boolean(p) && typeof p === 'object' && typeof (p as RunnerParameter).name === 'string',
      )
    : Object.entries((parametersRaw ?? {}) as Record<string, Record<string, unknown>>).map(([name, decl]) => ({
        name,
        type: String(decl?.['type'] ?? 'string'),
        required: decl?.['required'] === true,
        default: decl?.['default'],
        description: String(decl?.['description'] ?? '').trim(),
      }))

  // env：v2 是 map（`env.<NAME> = {required, secret, description}`）；容忍数组形态
  const envRaw = rec['env']
  const env: Runner['env'] = Array.isArray(envRaw)
    ? (envRaw as Runner['env'])
    : Object.entries((envRaw ?? {}) as Record<string, Record<string, unknown>>).map(([name, decl]) => ({
        name,
        required: decl?.['required'] === true,
        secret: decl?.['secret'] === true,
        description: String(decl?.['description'] ?? '').trim(),
      }))

  const description = typeof rec['description'] === 'string' ? rec['description'].trim() : undefined
  const role = ROLES.includes(rec['role'] as Runner['role']) ? (rec['role'] as Runner['role']) : 'test'
  const entry = Array.isArray(rec['entry'])
    ? (rec['entry'] as unknown[]).filter((item): item is string => typeof item === 'string')
    : typeof rec['entry'] === 'string'
      ? [rec['entry'] as string]
      : undefined

  return {
    name: String(rec['name'] ?? ''),
    version: String(rec['version'] ?? ''),
    display_name: typeof rec['display_name'] === 'string' ? rec['display_name'] : undefined,
    description: description || undefined,
    role,
    on_failure: rec['on_failure'] === 'abort_task' ? 'abort_task' : 'continue',
    entry,
    input_format: rec['input_format'] === 'yaml' || rec['input_format'] === 'text' ? rec['input_format'] : 'jsonl',
    image: String(rec['image'] ?? ''),
    exclusive: concurrency['endpoint_exclusive'] === true || rec['exclusive'] === true,
    placeholder: typeof rec['placeholder'] === 'string' ? rec['placeholder'].trim() : undefined,
    parameters,
    env,
    exports: Array.isArray(rec['exports']) ? (rec['exports'] as string[]) : undefined,
    consumes: Array.isArray(rec['consumes']) ? (rec['consumes'] as string[]) : undefined,
    latest: rec['latest'] === true,
    retired: rec['retired'] === true,
    orphaned: rec['orphaned'] === true,
    source_file: typeof rec['source_file'] === 'string' ? rec['source_file'] : undefined,
    registered_at: typeof rec['registered_at'] === 'string' ? rec['registered_at'] : undefined,
  }
}
