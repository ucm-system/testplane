/**
 * 契约归一：把"线上形态"收敛成 UI 好用的形状。
 * 放在独立文件里，真实实现与 mock 实现共用（避免两份解析逻辑漂移）。
 *
 * 目前只做两件事：
 *  1. 列表信封两种形态（`{items,...}` 与裸数组）都能吃；
 *  2. `Tester`：wire 的 `parameters` / `env` 是 map，UI 要按数组渲染（`TesterWire → Tester`）。
 */
import type { Page, Tester } from './types'

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
    return { items, next_cursor: nextCursor, has_more: hasMore }
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

/** `Tester`（契约形状，`parameters`/`env` 是 map）→ UI 形状（数组 + 提平的 exclusive）。 */
export function normalizeTester(raw: unknown): Tester {
  const rec = (raw ?? {}) as Record<string, unknown>
  const concurrency = (rec['concurrency'] ?? {}) as Record<string, unknown>

  const parametersRaw = rec['parameters']
  const parameters: Tester['parameters'] = Array.isArray(parametersRaw)
    ? (parametersRaw as Tester['parameters'])
    : Object.entries((parametersRaw ?? {}) as Record<string, Record<string, unknown>>).map(([name, decl]) => ({
        name,
        type: String(decl?.['type'] ?? 'string'),
        required: decl?.['required'] === true,
        default: decl?.['default'] == null ? undefined : String(decl['default']),
        description: String(decl?.['description'] ?? '').trim(),
      }))

  const envRaw = rec['env']
  const env: Tester['env'] = Array.isArray(envRaw)
    ? (envRaw as Tester['env'])
    : Object.entries((envRaw ?? {}) as Record<string, Record<string, unknown>>).map(([name, decl]) => ({
        name,
        required: decl?.['required'] === true,
        secret: decl?.['secret'] === true,
        description: String(decl?.['description'] ?? '').trim(),
      }))

  const description = typeof rec['description'] === 'string' ? rec['description'].trim() : undefined
  return {
    name: String(rec['name'] ?? ''),
    version: String(rec['version'] ?? ''),
    display_name: typeof rec['display_name'] === 'string' ? rec['display_name'] : undefined,
    description: description || undefined,
    role: rec['role'] === 'pre_check' ? 'pre_check' : 'test',
    on_failure: rec['on_failure'] === 'abort_run' ? 'abort_run' : 'continue',
    entry: typeof rec['entry'] === 'string' ? rec['entry'] : undefined,
    image: String(rec['image'] ?? ''),
    exclusive: concurrency['endpoint_exclusive'] === true || rec['exclusive'] === true,
    placeholder: typeof rec['placeholder'] === 'string' ? rec['placeholder'].trim() : undefined,
    parameters,
    env,
    orphaned: rec['orphaned'] === true,
    source_file: typeof rec['source_file'] === 'string' ? rec['source_file'] : undefined,
    registered_at: typeof rec['registered_at'] === 'string' ? rec['registered_at'] : undefined,
  }
}
