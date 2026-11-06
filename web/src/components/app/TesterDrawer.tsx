import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { api } from '@/api'
import type { TesterParameterDecl } from '@/api/types'
import { Badge, Tag } from '@/components/ui/badge'
import { Drawer } from '@/components/ui/drawer'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { LoadingBlock, ErrorState } from '@/components/ui/states'
import { useAsync } from '@/hooks/useAsync'

/**
 * 参数示例片段：`--name=<值>`。
 *
 * 「示例」优先取 `tester.yaml.parameters.<name>.default`（那是 Tester 作者写的真实默认值），
 * 没有默认值就按类型给一个能直接改的例子 —— 用户点的目的就是「先有一个能跑的片段」，
 * 所以不拼 `--name` 这种没法直接用的空壳。
 */
export function paramSnippet(name: string, param: TesterParameterDecl): string {
  const fallback = (): string => {
    const type = param.type.trim()
    if (type === 'boolean') return 'true'
    if (type.startsWith('list')) return '[512,1024]'
    if (type === 'integer') return '1'
    if (type === 'number' || type === 'float') return '1.0'
    return 'value'
  }
  return `--${name}=${param.default ?? fallback()}`
}

/** 作为「参数说明表」共用：右栏与详情抽屉是同一张表（含复制） */
export function TesterParamTable({
  parameters,
  onCopy,
  copyTestId = 'copy-param',
}: {
  parameters: Array<TesterParameterDecl & { name: string }>
  onCopy?: (snippet: string) => void
  copyTestId?: string
}) {
  const [copied, setCopied] = useState<string | null>(null)
  if (parameters.length === 0) return <p className="text-2xs text-fg-3">没有声明参数。</p>

  return (
    <TableWrap>
      <Table>
        <THead>
          <TR className="hover:bg-transparent">
            <TH>参数</TH>
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
            const snippet = paramSnippet(param.name, param)
            return (
              <TR key={param.name} className="hover:bg-transparent">
                <TD className="w-[15rem] align-top">
                  <span className="font-mono text-2xs text-fg">{param.name}</span>
                  <p className="mt-0.5 font-mono text-2xs text-fg-3">{snippet}</p>
                </TD>
                <TD className="align-top text-2xs leading-relaxed text-fg-3 whitespace-normal">
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
                      title={`把 ${snippet} 追加到参数输入框`}
                      onClick={() => {
                        onCopy(snippet)
                        setCopied(param.name)
                        window.setTimeout(() => setCopied((prev) => (prev === param.name ? null : prev)), 1500)
                      }}
                      className="inline-flex items-center gap-1 rounded-sm px-1.5 py-0.5 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
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

/**
 * Tester 详情抽屉（只读）：参数清单、env 声明、args 示例、镜像。
 *
 * **一个组件两处用**（`/testers?open=` 与提交页 Task 行的「详情」）：
 * 内容只写一份，差别只在 `onCopy` —— 给了就把示例追加进 args，没给就复制到剪贴板。
 */
export function TesterDrawer({
  name,
  onClose,
  /** 提交页：把片段追加到选中 Task 的 args；缺省时复制到剪贴板 */
  onCopyParam,
}: {
  name: string | null
  onClose: () => void
  onCopyParam?: (snippet: string) => void
}) {
  const detail = useAsync(() => (name ? api.getTester(name) : Promise.resolve(null)), [name])
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
      title={detail.data?.display_name ?? name ?? 'Tester'}
      description={`${name ?? ''} · 平台只认 tester.yaml + run.sh`}
    >
      {detail.loading && !detail.data ? (
        <LoadingBlock label="加载 Tester 详情…" />
      ) : detail.error ? (
        <ErrorState error={detail.error} onRetry={detail.reload} />
      ) : detail.data ? (
        <div className="flex flex-col gap-4" data-testid="tester-drawer">
          {detail.data.description ? (
            <p className="text-xs leading-relaxed text-fg-2">{detail.data.description}</p>
          ) : null}

          <dl className="grid grid-cols-[5.5rem_minmax(0,1fr)] gap-x-3 gap-y-2 text-xs">
            <dt className="text-fg-3">名称</dt>
            <dd className="font-mono text-fg-2">{detail.data.name}</dd>
            <dt className="text-fg-3">版本</dt>
            <dd className="font-mono text-fg-2">{detail.data.version}</dd>
            <dt className="text-fg-3">角色</dt>
            <dd>
              <Badge tone={detail.data.role === 'pre_check' ? 'primary' : 'muted'}>{detail.data.role}</Badge>
            </dd>
            <dt className="text-fg-3">失败策略</dt>
            <dd className="font-mono text-fg-2">{detail.data.on_failure}</dd>
            <dt className="text-fg-3">独占 Endpoint</dt>
            <dd className="text-fg-2">{detail.data.exclusive ? '是（不与其它 Task 并行）' : '否'}</dd>
            <dt className="text-fg-3">镜像</dt>
            <dd className="break-all font-mono text-2xs text-fg-2">{detail.data.image}</dd>
            <dt className="text-fg-3">入口</dt>
            <dd className="font-mono text-2xs text-fg-2">{detail.data.entry ?? '—'}</dd>
          </dl>

          {detail.data.placeholder ? (
            <div>
              <div className="flex items-center gap-2">
                <p className="text-xs font-medium text-fg-2">args 示例</p>
                <button
                  type="button"
                  data-testid="drawer-copy-placeholder"
                  aria-label="复制 args 示例"
                  onClick={() => copy(detail.data?.placeholder ?? '')}
                  className="inline-flex items-center gap-1 rounded-sm px-1.5 py-0.5 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
                >
                  {copiedKey === detail.data.placeholder ? (
                    <Check className="size-3 text-success" aria-hidden />
                  ) : (
                    <Copy className="size-3" aria-hidden />
                  )}
                  {copiedKey === detail.data.placeholder ? (onCopyParam ? '已加入参数' : '已复制') : '复制'}
                </button>
              </div>
              <pre className="mt-1 overflow-x-auto rounded-md border border-line-soft bg-surface-2 px-2 py-1.5 font-mono text-[11px] leading-relaxed text-fg-2">
                {detail.data.placeholder}
              </pre>
            </div>
          ) : null}

          <div>
            <p className="mb-1 text-xs font-medium text-fg-2">参数清单（{detail.data.parameters.length}）</p>
            <TesterParamTable
              parameters={detail.data.parameters}
              onCopy={copy}
              copyTestId="drawer-copy-param"
            />
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

          <p className="rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-relaxed text-fg-3">
            Tester 随镜像内置，平台启动时扫描目录自动发现 —— 控制台不提供上传或注册入口。
          </p>
        </div>
      ) : null}
    </Drawer>
  )
}
