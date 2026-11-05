import { useState } from 'react'
import { api } from '@/api'
import type { Tester } from '@/api/types'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { Badge, Tag } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardHeader } from '@/components/ui/card'
import { Drawer } from '@/components/ui/drawer'
import { TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState, LoadingBlock } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useAsync } from '@/hooks/useAsync'
import { cn } from '@/lib/utils'

/** Tester 详情抽屉（只读）：参数清单与 env 声明供 Agent 与维护者阅读。 */
function TesterDrawer({ name, onClose }: { name: string | null; onClose: () => void }) {
  const detail = useAsync(
    () => (name ? api.getTester(name) : Promise.resolve(null)),
    [name],
  )

  return (
    <Drawer
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
        <div className="flex flex-col gap-4">
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
              <p className="text-xs font-medium text-fg-2">args 示例</p>
              <pre className="mt-1 overflow-x-auto rounded-md border border-line-soft bg-surface-2 px-2 py-1.5 font-mono text-[11px] leading-relaxed text-fg-2">
                {detail.data.placeholder}
              </pre>
            </div>
          ) : null}

          <div>
            <p className="mb-1 text-xs font-medium text-fg-2">参数清单（{detail.data.parameters.length}）</p>
            {detail.data.parameters.length === 0 ? (
              <p className="text-2xs text-fg-3">没有声明参数。</p>
            ) : (
              <TableWrap>
                <Table>
                  <THead>
                    <TR className="hover:bg-transparent">
                      <TH>参数</TH>
                      <TH>类型</TH>
                      <TH>必填</TH>
                      <TH>默认</TH>
                    </TR>
                  </THead>
                  <TBody>
                    {detail.data.parameters.map((param) => (
                      <TR key={param.name} className="hover:bg-transparent">
                        <TD className="align-top">
                          <span className="font-mono text-2xs text-fg">{param.name}</span>
                          {param.description ? (
                            <p className="mt-0.5 text-2xs leading-relaxed text-fg-3">{param.description}</p>
                          ) : null}
                        </TD>
                        <TD className="align-top font-mono text-2xs">{param.type}</TD>
                        <TD className="align-top">{param.required ? <Badge tone="warn">必填</Badge> : '—'}</TD>
                        <TD className="align-top font-mono text-2xs">{param.default ?? '—'}</TD>
                      </TR>
                    ))}
                  </TBody>
                </Table>
              </TableWrap>
            )}
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

export function TestersPage() {
  const [openName, setOpenName] = useState<string | null>(null)
  const testers = useAsync(() => api.listTesters(), [])
  const list: Tester[] = testers.data ?? []

  return (
    <>
      <RightRail>
        <RailCard title="注册规则" tone="info">
          <p>只认根目录的 tester.yaml + run.sh；role: pre_check 必须声明 on_failure: abort_run。</p>
          <p className="mt-2">Tester 是镜像内不可变元数据，没有运行时状态；目录在平台启动时扫描。</p>
        </RailCard>
        <RailCard title="参数语义" tone="muted">
          <p>parameters 只供 Agent 与维护者阅读，平台不渲染、不校验。</p>
          <p className="mt-2">公共参数（--url / --model_name / --model_path）由平台注入，Tester 不再重复声明来源。</p>
        </RailCard>
      </RightRail>

      <Card>
        <CardHeader
          tone="primary"
          title="已注册 Tester"
          sub={`${list.length} 个 · 随镜像内置，平台启动时扫描目录自动发现`}
        />
        {testers.error ? (
          <div className="p-4">
            <ErrorState error={testers.error} onRetry={testers.reload} />
          </div>
        ) : testers.loading && list.length === 0 ? (
          <TableSkeleton rows={4} cols={6} />
        ) : list.length === 0 ? (
          <EmptyState
            title="目录里还没有 Tester"
            description="把 tester.yaml + run.sh 放进 testers/<name>/，平台重启后会自动发现。"
          />
        ) : (
          <TableWrap>
            <Table>
              <THead>
                <TR className="hover:bg-transparent">
                  <TH>名称</TH>
                  <TH>版本</TH>
                  <TH>角色</TH>
                  <TH>失败策略</TH>
                  <TH>独占 Endpoint</TH>
                  <TH>参数</TH>
                  <TH>env</TH>
                  <TH className="text-right">操作</TH>
                </TR>
              </THead>
              <TBody>
                {list.map((tester) => (
                  <TR key={tester.name} className="cursor-pointer" onClick={() => setOpenName(tester.name)}>
                    <TD className="font-mono text-2xs text-fg">{tester.name}</TD>
                    <TD className="font-mono text-2xs">{tester.version}</TD>
                    <TD>
                      <Badge tone={tester.role === 'pre_check' ? 'primary' : 'muted'}>{tester.role}</Badge>
                    </TD>
                    <TD>
                      <Badge tone={tester.on_failure === 'abort_run' ? 'warn' : 'muted'}>{tester.on_failure}</Badge>
                    </TD>
                    <TD className={cn('text-2xs', tester.exclusive ? 'text-fg' : 'text-fg-3')}>
                      {tester.exclusive ? '是' : '否'}
                    </TD>
                    <TD className="tabular-nums">{tester.parameters.length}</TD>
                    <TD className="tabular-nums">{tester.env.length}</TD>
                    <TD className="text-right">
                      <Button size="sm" variant="ghost" onClick={() => setOpenName(tester.name)}>
                        详情
                      </Button>
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          </TableWrap>
        )}
      </Card>

      <TesterDrawer name={openName} onClose={() => setOpenName(null)} />
    </>
  )
}
