/**
 * 提交页输入框的**客户端即时校验**（仅 dry-run 性质，不替代提交时服务端校验）。
 *
 * jsonl（D-67）：按 Runner 声明的 `parameters` 做**字段类型**与 **required 存在性**校验，逐行报错
 * （行号 + 字段名 + 原因）——与平台提交校验器同一规则；枚举值、范围、正则、跨字段约束归 Runner
 * （§5.4），`parameters` 里若带了 `enum` 提示（additionalProperties 允许）则顺带校验。
 * yaml / text：只做语法校验，语义完全归 Runner。
 */
import { parse as parseYaml } from 'yaml'
import type { RunnerParameter } from '@/api/types'

export interface InputIssue {
  /** 1-based 行号（yaml 语法错误也有行号） */
  line: number
  /** 出错字段名；纯语法错误时缺省 */
  field?: string
  message: string
  kind: 'syntax' | 'type' | 'required' | 'enum'
}

/** 该行值与声明的 JSON 类型是否匹配（integer = number 且为整数）。 */
function matchesType(value: unknown, type: string): boolean {
  switch (type.trim()) {
    case 'string':
      return typeof value === 'string'
    case 'integer':
      return typeof value === 'number' && Number.isInteger(value)
    case 'number':
      return typeof value === 'number'
    case 'boolean':
      return typeof value === 'boolean'
    case 'object':
      return typeof value === 'object' && value !== null && !Array.isArray(value)
    case 'array':
      return Array.isArray(value)
    default:
      // 未知类型名（Runner 自定义）不拦：语义归 Runner
      return true
  }
}

/** 该行值的人话描述（错误信息用）。 */
function describeValue(value: unknown): string {
  if (value === null) return 'null'
  if (Array.isArray(value)) return 'array'
  return typeof value
}

/**
 * 逐行校验 JSONL 输入。空行跳过；每行必须是一个 JSON 对象（一条记录）。
 * `parameters` 为空（yaml/text 或未声明参数）时只做语法校验。
 */
export function validateJsonlInput(text: string, parameters: RunnerParameter[]): InputIssue[] {
  const issues: InputIssue[] = []
  const required = parameters.filter((p) => p.required === true)
  const typed = parameters.filter((p) => Boolean(p.type))

  const lines = text.split('\n')
  for (let index = 0; index < lines.length; index += 1) {
    const raw = lines[index] ?? ''
    const lineNo = index + 1
    if (!raw.trim()) continue

    let record: unknown
    try {
      record = JSON.parse(raw)
    } catch (e) {
      issues.push({
        line: lineNo,
        kind: 'syntax',
        message: `JSON 语法错误：${e instanceof Error ? e.message : String(e)}`,
      })
      continue
    }
    if (record === null || typeof record !== 'object' || Array.isArray(record)) {
      issues.push({
        line: lineNo,
        kind: 'syntax',
        message: `每行必须是一个 JSON 对象（一条记录），实际是 ${describeValue(record)}`,
      })
      continue
    }

    const map = record as Record<string, unknown>
    for (const param of required) {
      if (!(param.name in map) || map[param.name] === undefined) {
        issues.push({
          line: lineNo,
          field: param.name,
          kind: 'required',
          message: `缺少必填字段 ${param.name}`,
        })
      }
    }
    for (const param of typed) {
      const value = map[param.name]
      if (value === undefined || value === null) continue // 缺省/null 交给 required 与 Runner 兜底
      if (!matchesType(value, param.type)) {
        issues.push({
          line: lineNo,
          field: param.name,
          kind: 'type',
          message: `字段 ${param.name} 期望 ${param.type}，实际 ${describeValue(value)}`,
        })
        continue
      }
      // enum 是文档语义之外的"提示"（附录 C 允许 additionalProperties）；有就顺带校验
      const hint = (param as RunnerParameter & { enum?: unknown[] }).enum
      if (Array.isArray(hint) && hint.length > 0 && !hint.some((item) => item === value)) {
        issues.push({
          line: lineNo,
          field: param.name,
          kind: 'enum',
          message: `字段 ${param.name} 应为 ${hint.map((item) => JSON.stringify(item)).join(' | ')} 之一`,
        })
      }
    }
  }
  return issues
}

/** YAML 语法校验（只语法，不校验语义）。 */
export function validateYamlInput(text: string): InputIssue[] {
  if (!text.trim()) return []
  try {
    parseYaml(text)
    return []
  } catch (e) {
    const first = (e as { linePos?: Array<{ line: number; col: number }> }).linePos?.[0]
    return [
      {
        line: first?.line ?? 1,
        kind: 'syntax',
        message: `YAML 语法错误：${(e as Error).message}${first ? `（第 ${first.line} 行第 ${first.col} 列）` : ''}`,
      },
    ]
  }
}

/* ---------------------------------------------------------------- 格式化 / 压缩 */

/** 单行 JSON 序列化（键与值之间 `: `、元素之间 `, `）：保持"一条记录一行"的 JSONL 形态。 */
function stringifyOneLine(value: unknown): string {
  if (value === null || typeof value === 'number' || typeof value === 'boolean') return String(value)
  if (typeof value === 'string') return JSON.stringify(value)
  if (Array.isArray(value)) return `[${value.map((item) => stringifyOneLine(item)).join(', ')}]`
  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>).map(
      ([key, item]) => `${JSON.stringify(key)}: ${stringifyOneLine(item)}`,
    )
    return `{${entries.join(', ')}}`
  }
  return JSON.stringify(value)
}

export interface JsonlRewriteResult {
  text: string
  /** 解析失败、原样保留的行数 */
  keptInvalid: number
}

/** JSONL 逐行格式化：每行解析成功后按"单行 + 空格"重排；解析失败的行原样保留。 */
export function formatJsonl(text: string): JsonlRewriteResult {
  let keptInvalid = 0
  const out = text.split('\n').map((raw) => {
    if (!raw.trim()) return raw
    try {
      return stringifyOneLine(JSON.parse(raw))
    } catch {
      keptInvalid += 1
      return raw
    }
  })
  return { text: out.join('\n'), keptInvalid }
}

/** JSONL 逐行压缩：去掉记录内的全部多余空白（仍是每行一条记录）。 */
export function minifyJsonl(text: string): JsonlRewriteResult {
  let keptInvalid = 0
  const out = text.split('\n').map((raw) => {
    if (!raw.trim()) return raw
    try {
      return JSON.stringify(JSON.parse(raw))
    } catch {
      keptInvalid += 1
      return raw
    }
  })
  return { text: out.join('\n'), keptInvalid }
}
