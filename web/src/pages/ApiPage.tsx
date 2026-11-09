import { useMemo, useState } from 'react'
import { ChevronDown, Download, Search } from 'lucide-react'
import { parse as parseYaml } from 'yaml'
import specSource from '@/generated/openapi.yaml?raw'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardHeader } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'
import { saveBlob } from '@/lib/download'

/**
 * API 页（`/api`）。
 *
 * 数据**只来自** `server/api/openapi.yaml`：构建前由 `scripts/sync-spec.mjs` 单向投影到
 * `src/generated/openapi.yaml`，这里 `?raw` 读进来用 `yaml` 解析 —— 页面里没有任何手抄的端点表，
 * 「下载 openapi.yaml」下发的也是同一份字节，所以展示与契约不会漂移。
 *
 * 展示粒度按用户要求：**按 tag 分组**列 `METHOD path — summary`，点行展开 parameters / responses 摘要
 * （不做完整 schema 树；要看全的下源文件）。
 */

interface Operation {
  method: string
  path: string
  summary: string
  operationId?: string
  tags: string[]
  parameters: Array<{ name: string; in?: string; required?: boolean; description?: string }>
  responses: Array<{ status: string; description?: string }>
}

const METHOD_ORDER = ['get', 'post', 'put', 'patch', 'delete']

const METHOD_TONE: Record<string, 'ok' | 'info' | 'warn' | 'err' | 'muted'> = {
  get: 'info',
  post: 'ok',
  put: 'warn',
  patch: 'warn',
  delete: 'err',
}

/** 从 `#/components/...` 引用里取末段，给参数/响应摘要用 */
function refName(ref: string): string {
  return ref.split('/').pop() ?? ref
}

function describeSchema(schema: unknown): string {
  if (!schema || typeof schema !== 'object') return ''
  const record = schema as Record<string, unknown>
  if (typeof record.$ref === 'string') return refName(record.$ref)
  if (record.type) {
    const type = record.type
    if (Array.isArray(type)) return type.join(' | ')
    if (type === 'array' && record.items && typeof record.items === 'object') {
      return `${describeSchema(record.items)}[]`
    }
    return String(type)
  }
  return ''
}

function buildOperations(spec: Record<string, unknown>): Operation[] {
  const paths = (spec.paths ?? {}) as Record<string, Record<string, unknown>>
  const operations: Operation[] = []
  for (const [path, item] of Object.entries(paths)) {
    for (const method of METHOD_ORDER) {
      const operation = item[method]
      if (!operation || typeof operation !== 'object') continue
      const rec = operation as Record<string, unknown>
      const parametersRaw = (rec.parameters ?? []) as Array<Record<string, unknown>>
      const responsesRaw = (rec.responses ?? {}) as Record<string, Record<string, unknown>>
      operations.push({
        method: method.toUpperCase(),
        path,
        summary: String(rec.summary ?? ''),
        operationId: rec.operationId ? String(rec.operationId) : undefined,
        tags: Array.isArray(rec.tags) ? rec.tags.map(String) : ['未分组'],
        parameters: parametersRaw.map((parameter) => {
          if (typeof parameter.$ref === 'string') {
            return { name: refName(parameter.$ref), in: 'components' }
          }
          return {
            name: String(parameter.name ?? ''),
            in: parameter.in ? String(parameter.in) : undefined,
            required: parameter.required === true,
            description: parameter.description ? String(parameter.description) : undefined,
          }
        }),
        responses: Object.entries(responsesRaw).map(([status, response]) => ({
          status,
          description:
            (typeof response.description === 'string' && response.description) ||
            (typeof response.$ref === 'string' ? refName(response.$ref) : '') ||
            describeSchema((response.content as Record<string, unknown> | undefined)?.['application/json']),
        })),
      })
    }
  }
  return operations
}

export function ApiPage() {
  const { operations, info } = useMemo(() => {
    const spec = parseYaml(specSource) as Record<string, unknown>
    return {
      operations: buildOperations(spec),
      info: (spec.info ?? {}) as { title?: string; version?: string },
    }
  }, [])

  const [keyword, setKeyword] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)

  const groups = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    const filtered = kw
      ? operations.filter(
          (operation) =>
            operation.path.toLowerCase().includes(kw) ||
            operation.summary.toLowerCase().includes(kw) ||
            operation.method.toLowerCase().includes(kw) ||
            (operation.operationId ?? '').toLowerCase().includes(kw),
        )
      : operations
    const byTag = new Map<string, Operation[]>()
    for (const operation of filtered) {
      const tag = operation.tags[0] ?? '未分组'
      byTag.set(tag, [...(byTag.get(tag) ?? []), operation])
    }
    return [...byTag.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [operations, keyword])

  const shown = groups.reduce((sum, [, items]) => sum + items.length, 0)

  const download = () => {
    saveBlob(new Blob([specSource], { type: 'application/yaml' }), 'openapi.yaml')
  }

  return (
    <div className="flex min-w-0 flex-col gap-4" data-testid="api-page">
      <Card>
        <CardHeader
          tone="primary"
          title="API 参考"
          sub={`${info.title ?? 'openapi.yaml'}${info.version ? ` · v${info.version}` : ''}`}
          actions={
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative">
                <Search
                  className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-fg-3"
                  aria-hidden
                />
                <Input
                  value={keyword}
                  onChange={(event) => setKeyword(event.target.value)}
                  placeholder="按路径 / 摘要过滤"
                  aria-label="按路径或摘要过滤端点"
                  className="h-9 w-56 pl-8"
                />
              </div>
              <Button
                size="sm"
                variant="primary"
                data-testid="download-openapi"
                icon={<Download className="size-3" aria-hidden />}
                onClick={download}
              >
                下载 openapi.yaml
              </Button>
            </div>
          }
        />
        <CardBody className="flex flex-wrap items-center gap-3 text-2xs text-fg-3">
          <span data-testid="api-endpoint-count">共 {operations.length} 个端点</span>
          <span aria-hidden>·</span>
          <span>
            {groups.length} 个分组
            {keyword ? ` · 当前匹配 ${shown} 个` : ''}
          </span>
          <span className="ml-auto">
            本页数据来自 <code className="font-mono">server/api/openapi.yaml</code>（构建前由{' '}
            <code className="font-mono">scripts/sync-spec.mjs</code> 同步，未手抄）
          </span>
        </CardBody>
      </Card>

      {groups.length === 0 ? (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-sm text-fg-3" data-testid="api-empty">
              没有匹配「{keyword}」的端点
            </p>
          </CardBody>
        </Card>
      ) : (
        groups.map(([tag, items]) => (
          <Card key={tag} data-testid={`api-group-${tag}`}>
            <CardHeader tone="muted" title={tag} sub={`${items.length} 个端点`} />
            <ul className="divide-y divide-line-soft">
              {items.map((operation) => {
                const key = `${operation.method} ${operation.path}`
                const open = expanded === key
                return (
                  <li key={key}>
                    <button
                      type="button"
                      data-testid="api-endpoint-row"
                      aria-expanded={open}
                      onClick={() => setExpanded(open ? null : key)}
                      className="flex w-full min-w-0 items-center gap-3 px-5 py-2.5 text-left transition-colors hover:bg-surface-2"
                    >
                      <Badge tone={METHOD_TONE[operation.method.toLowerCase()] ?? 'muted'} className="w-16 justify-center font-mono">
                        {operation.method}
                      </Badge>
                      <code className="min-w-0 shrink-0 font-mono text-xs text-fg">{operation.path}</code>
                      <span className="min-w-0 flex-1 truncate text-xs text-fg-3" title={operation.summary}>
                        {operation.summary}
                      </span>
                      <ChevronDown
                        className={cn('size-3.5 shrink-0 text-fg-3 transition-transform duration-150', open && 'rotate-180')}
                        aria-hidden
                      />
                    </button>

                    {open ? (
                      <div className="flex flex-col gap-3 border-t border-line-soft bg-surface-2/60 px-5 py-3 text-2xs">
                        {operation.operationId ? (
                          <p className="text-fg-3">
                            operationId：<code className="font-mono text-fg-2">{operation.operationId}</code>
                          </p>
                        ) : null}
                        <div>
                          <p className="mb-1 font-medium text-fg-2">参数（{operation.parameters.length}）</p>
                          {operation.parameters.length === 0 ? (
                            <p className="text-fg-3">无</p>
                          ) : (
                            <ul className="flex flex-col gap-1">
                              {operation.parameters.map((parameter) => (
                                <li key={`${parameter.name}-${parameter.in ?? ''}`} className="flex flex-wrap items-baseline gap-2">
                                  <code className="font-mono text-fg-2">{parameter.name}</code>
                                  {parameter.in ? <Badge tone="muted">{parameter.in}</Badge> : null}
                                  {parameter.required ? <Badge tone="warn">必填</Badge> : null}
                                  {parameter.description ? (
                                    <span className="min-w-0 flex-1 text-fg-3">{parameter.description}</span>
                                  ) : null}
                                </li>
                              ))}
                            </ul>
                          )}
                        </div>
                        <div>
                          <p className="mb-1 font-medium text-fg-2">响应（{operation.responses.length}）</p>
                          <ul className="flex flex-wrap gap-2">
                            {operation.responses.map((response) => (
                              <li key={response.status} className="flex items-baseline gap-1.5">
                                <Badge tone={response.status.startsWith('2') ? 'ok' : response.status.startsWith('4') ? 'warn' : 'err'}>
                                  {response.status}
                                </Badge>
                                <span className="text-fg-3">{response.description}</span>
                              </li>
                            ))}
                          </ul>
                        </div>
                      </div>
                    ) : null}
                  </li>
                )
              })}
            </ul>
          </Card>
        ))
      )}
    </div>
  )
}
