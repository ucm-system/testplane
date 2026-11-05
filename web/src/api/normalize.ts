/**
 * 契约归一：把"后端可能给的形态"收敛成前端类型。
 * 放在独立文件里，真实实现与 mock 实现共用（避免两份解析逻辑漂移）。
 */
import type { Page, Tester } from './types'

/** 列表响应按 07 §6 是 `{items, next_cursor, has_more}`；也容忍裸数组。 */
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

/**
 * Tester 归一：`tester.yaml` 里 `parameters` / `env` 是 map（name → 声明），
 * 目录接口若直接回 YAML 结构就转成数组；已经是数组时原样返回。
 */
export function normalizeTester(raw: unknown): Tester {
  const rec = (raw ?? {}) as Record<string, unknown>
  const concurrency = (rec['concurrency'] ?? {}) as Record<string, unknown>

  const parametersRaw = rec['parameters']
  const parameters: Tester['parameters'] = Array.isArray(parametersRaw)
    ? (parametersRaw as Tester['parameters'])
    : Object.entries((parametersRaw ?? {}) as Record<string, Record<string, unknown>>).map(
        ([name, decl]) => ({
          name,
          type: String(decl?.['type'] ?? 'string'),
          required: decl?.['required'] === true,
          description: String(decl?.['description'] ?? '').trim(),
          default: decl?.['default'] == null ? null : String(decl['default']),
        }),
      )

  const envRaw = rec['env']
  const env: Tester['env'] = Array.isArray(envRaw)
    ? (envRaw as Tester['env'])
    : Object.entries((envRaw ?? {}) as Record<string, Record<string, unknown>>).map(
        ([name, decl]) => ({
          name,
          required: decl?.['required'] === true,
          secret: decl?.['secret'] === true,
          description: String(decl?.['description'] ?? '').trim(),
        }),
      )

  return {
    name: String(rec['name'] ?? ''),
    version: String(rec['version'] ?? ''),
    display_name: typeof rec['display_name'] === 'string' ? rec['display_name'] : undefined,
    description: typeof rec['description'] === 'string' ? rec['description'].trim() : undefined,
    role: rec['role'] === 'pre_check' ? 'pre_check' : 'test',
    on_failure: rec['on_failure'] === 'abort_run' ? 'abort_run' : 'continue',
    exclusive: rec['exclusive'] === true || concurrency['endpoint_exclusive'] === true,
    image: String(rec['image'] ?? ''),
    entry: typeof rec['entry'] === 'string' ? rec['entry'] : undefined,
    placeholder: typeof rec['placeholder'] === 'string' ? rec['placeholder'].trim() : undefined,
    parameters,
    env,
    orphaned: rec['orphaned'] === true,
  }
}
