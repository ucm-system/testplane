import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { api } from '@/api'
import type { RunnerParameter } from '@/api/types'
import { Badge, Tag } from '@/components/ui/badge'
import { Drawer } from '@/components/ui/drawer'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { LoadingBlock, ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'
import { inputFormatLabel } from '@/lib/runner-format'

const INPUT_FORMAT_TONE = { jsonl: 'primary', yaml: 'info', text: 'muted' } as const

/**
 * 参数 schema 表（附录 C v2）：`parameters[]` 是 **jsonl 输入的结构校验 schema** ——
 * 平台提交期校验**字段类型**与 **required 存在性**（D-67，报错带行号）；
 * 枚举值、范围、正则、跨字段约束仍归 Runner（§5.4），`default` / `minimum` 只是文档语义。
 */
export function RunnerParamTable({
  parameters,
  onCopy,
  copyTestId = 'copy-param',
}: {
  parameters: RunnerParameter[]
  onCopy?: (snippet: string) => void
  copyTestId?: string
}) {
  const [copied, setCopied] = useState<string | null>(null)
  if (parameters.length === 0)
    return <p className="text-2xs text-fg-3">没有声明参数（input_format 为 yaml / text 时参数完全归 Runner）。</p>

  return (
    <TableWrap>
      <Table>
        <THead>
          <TR className="hover:bg-transparent">
            <TH>参数</TH>
            <TH>类型</TH>
            <TH>说明</TH>
            {onCopy ? (
              <TH className="w-[5.5rem] text-right">
                <span className="sr-only">操作</span>
              </TH>
            ) : null}
          </TR>
        </THead>
        <TBody>
          {parameters.map((param) => {
            const snippet = jsonlSnippet(param)
            return (
              <TR key={param.name} className="hover:bg-transparent">
                <TD className="w-[13rem] align-top">
                  <span className="font-mono text-2xs text-fg">{param.name}</span>
                  <p className="mt-0.5 font-mono text-2xs text-fg-3">{snippet}</p>
                </TD>
                <TD className="align-top">
                  <span className="font-mono text-2xs text-fg-2">{param.type}</span>
                </TD>
                <TD className="align-top text-2xs leading-[16px] text-fg-3 whitespace-normal">
                  {param.description ?? '—'}
                  {param.required ? <Badge tone="warn" className="ml-1.5">必填</Badge> : null}
                </TD>
                {onCopy ? (
                  <TD className="align-top text-right whitespace-nowrap">
                    <button
                      type="button"
                      data-testid={copyTestId}
                      data-param={param.name}
                      aria-label={`复制参数 ${param.name}`}
                      title={`把 ${snippet} 加进输入框`}
                      onClick={() => {
                        onCopy(snippet)
                        setCopied(param.name)
                        window.setTimeout(() => setCopied((prev) => (prev === param.name ? null : prev)), 1500)
                      }}
                      className="inline-flex h-6 items-center gap-1 rounded-sm px-2 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
                    >
                      {copied === param.name ? (
                        <Check className="size-3 text-success" aria-hidden />
                      ) : (
                        <Copy className="size-3" aria-hidden />
                      )}
                      {copied === param.name ? '已加' : '复制'}
                    </button>
                  </TD>
                ) : null}
              </TR>
            )
          })}
        </TBody>
      </Table>
    </TableWrap>
  )
}

/** jsonl 参数片段（复制进输入框用）：`"name": <default|按类型的示例值>`。 */
function jsonlSnippet(param: RunnerParameter): string {
  const fallback = (): string => {
    const type = (param.type ?? 'string').trim()
    if (type === 'boolean') return 'true'
    if (type === 'integer' || type === 'number') return '1'
    if (type === 'array') return '[]'
    if (type === 'object') return '{}'
    return '"value"'
  }
  const value =
    param.default == null ? fallback() : typeof param.default === 'string' ? `"${param.default}"` : String(param.default)
  return `"${param.name}": ${value}`
}

/**
 * Runner 详情抽屉（只读）：参数 schema、env 声明、placeholder、exports/consumes、镜像与 entry。
 *
 * **一个组件两处用**（`/runners?open=` 与提交页 runner 行的「详情」）：
 * 内容只写一份，差别只在 `onCopy` —— 给了就把片段追加进输入框，没给就复制到剪贴板。
 */
export function RunnerDrawer({
  name,
  onClose,
  onCopyParam,
}: {
  name: string | null
  onClose: () => void
  onCopyParam?: (snippet: string) => void
}) {
  const detail = useAsync(() => (name ? api.getRunner(name) : Promise.resolve(null)), [name])
  const [copiedKey, setCopiedKey] = useState<string | null>(null)

  const copy = (snippet: string) => {
    if (onCopyParam) {
      onCopyParam(snippet)
      setCopiedKey(snippet)
      window.setTimeout(() => setCopiedKey((prev) => (prev === snippet ? null : prev)), 1500)
      return
    }
    // 剪贴板可能被浏览器策略拒绝（非 https / 无权限）：静默降级，不弹未处理的 rejection
    void navigator.clipboard?.writeText(snippet).catch(() => undefined)
    setCopiedKey(snippet)
    window.setTimeout(() => setCopiedKey((prev) => (prev === snippet ? null : prev)), 1500)
  }

  return (
    <Drawer
      size="xl"
      open={name !== null}
      onClose={onClose}
      title={detail.data?.display_name ?? name ?? 'Runner'}
      description={`${name ?? ''} · 平台只认 runner.yaml（附录 C v2），注册靠目录扫描`}
    >
      {detail.loading && !detail.data ? (
        <LoadingBlock label="加载 Runner 详情…" />
      ) : detail.error ? (
        <ErrorState error={detail.error} onRetry={detail.reload} />
      ) : detail.data ? (
        <div className="flex flex-col gap-4" data-testid="runner-drawer">
          {detail.data.description ? (
            <p className="text-xs leading-[16px] text-fg-2">{detail.data.description}</p>
          ) : null}

          <dl className="grid grid-cols-[5.5rem_minmax(0,1fr)] gap-x-3 gap-y-2 text-xs">
            <dt className="text-fg-3">名称</dt>
            <dd className="font-mono text-fg-2">{detail.data.name}</dd>
            <dt className="text-fg-3">版本</dt>
            <dd className="font-mono text-fg-2">{detail.data.version}</dd>
            <dt className="text-fg-3">角色</dt>
            <dd>
              <Badge
                tone={detail.data.role === 'pre_check' ? 'primary' : detail.data.role === 'test' ? 'muted' : 'info'}
              >
                {detail.data.role}
              </Badge>
              {detail.data.role === 'deploy' || detail.data.role === 'clean' ? (
                <span className="ml-2 text-2xs text-fg-3">内置（不可手动引用）</span>
              ) : null}
            </dd>
            <dt className="text-fg-3">失败策略</dt>
            <dd className="font-mono text-fg-2">{detail.data.on_failure}</dd>
            <dt className="text-fg-3">输入格式</dt>
            <dd>
              <Badge tone={INPUT_FORMAT_TONE[detail.data.input_format]}>
                {inputFormatLabel(detail.data.input_format)}
              </Badge>
            </dd>
            <dt className="text-fg-3">独占 Endpoint</dt>
            <dd className="text-fg-2">{detail.data.exclusive ? '是' : '否'}</dd>
            <dt className="text-fg-3">镜像</dt>
            <dd className="break-all font-mono text-2xs text-fg-2">{detail.data.image}</dd>
            <dt className="text-fg-3">入口</dt>
            <dd className="break-all font-mono text-2xs text-fg-2">
              {detail.data.entry ? JSON.stringify(detail.data.entry) : '—'}
            </dd>
            <dt className="text-fg-3">exports</dt>
            <dd className="break-all font-mono text-2xs text-fg-2">{detail.data.exports?.join('、') || '—'}</dd>
            <dt className="text-fg-3">consumes</dt>
            <dd className="break-all font-mono text-2xs text-fg-2">{detail.data.consumes?.join('、') || '—'}</dd>
          </dl>

          {detail.data.placeholder ? (
            <div>
              <div className="flex items-center gap-2">
                <p className="text-xs font-medium text-fg-2">输入示例（与 input_format 同格式）</p>
                <button
                  type="button"
                  data-testid="drawer-copy-placeholder"
                  aria-label="复制输入示例"
                  onClick={() => copy(detail.data?.placeholder ?? '')}
                  className="inline-flex h-6 items-center gap-1 rounded-sm px-2 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
                >
                  {copiedKey === detail.data.placeholder ? (
                    <Check className="size-3 text-success" aria-hidden />
                  ) : (
                    <Copy className="size-3" aria-hidden />
                  )}
                  {copiedKey === detail.data.placeholder ? (onCopyParam ? '已加入输入' : '已复制') : '复制'}
                </button>
              </div>
              <pre className="mt-1 overflow-x-auto rounded-md border border-line-soft bg-surface-2 px-2 py-1.5 font-mono text-2xs leading-[16px] text-fg-2">
                {detail.data.placeholder}
              </pre>
            </div>
          ) : null}

          <div>
            <p className="mb-1 text-xs font-medium text-fg-2">参数 schema（{detail.data.parameters.length}）</p>
            <RunnerParamTable parameters={detail.data.parameters} onCopy={copy} copyTestId="drawer-copy-param" />
          </div>

          <div>
            <p className="mb-1 text-xs font-medium text-fg-2">env 声明</p>
            {detail.data.env.length === 0 ? (
              <p className="text-2xs text-fg-3">没有声明环境变量。</p>
            ) : (
              <ul className="flex flex-col gap-1.5">
                {detail.data.env.map((item) => (
                  <li key={item.name} className="flex flex-wrap items-center gap-2 text-2xs">
                    <Tag mono>{item.name}</Tag>
                    {item.secret ? <Badge tone="warn">凭据</Badge> : null}
                    {item.required ? <Badge tone="info">必填</Badge> : <span className="text-fg-3">可选</span>}
                    <span className="text-fg-3">{item.description}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>

          <p className="rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-[16px] text-fg-3">
            Runner 随平台发版走目录扫描自动注册 —— 控制台不提供上传或注册入口；版本发布后不可覆盖
            （同 name+version 不同内容会被拒）。
          </p>
        </div>
      ) : null}
    </Drawer>
  )
}
