import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { ChevronDown } from 'lucide-react'
import { api } from '@/api'
import type { Tester } from '@/api/types'
import { TesterDrawer } from '@/components/app/TesterDrawer'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardHeader } from '@/components/ui/card'
import { TableSkeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { useAsync } from '@/hooks/useAsync'
import { cn } from '@/lib/utils'

/** 版本比较：`0.10.0 > 0.9.0`（按数字段比，别用字符串排序） */
function compareVersion(a: string, b: string): number {
  const pa = a.split(/[.-]/).map((part) => Number.parseInt(part, 10))
  const pb = b.split(/[.-]/).map((part) => Number.parseInt(part, 10))
  for (let i = 0; i < Math.max(pa.length, pb.length); i += 1) {
    const x = Number.isNaN(pa[i]) ? 0 : (pa[i] ?? 0)
    const y = Number.isNaN(pb[i]) ? 0 : (pb[i] ?? 0)
    if (x !== y) return y - x // 大的在前
  }
  return 0
}

interface TesterGroup {
  name: string
  /** 目录里的当前版本（后端默认只回最新；`latest` 缺省时按版本号取最大） */
  latest: Tester
  /** 历史版本（`?include_retired=true` 才会带回来） */
  older: Tester[]
}

/**
 * Tester 目录：**按 name 聚合**，一行一个 name。
 *
 * 票 #60：`GET /testers` 会同时回 `aisbench-sla@0.1.0` 与 `@0.2.0`，并列展示时用户以为重复。
 * 现在主行只显示最新版本，历史版本收进「另有 N 个历史版本」——展开才看得到，
 * 并标 `已退役`（"目录里已移除；历史 Run 仍按 name@version 引用"）。
 *
 * 历史 Run 的报告/Task 详情里显示的 `name@version` **不改**：那是当时跑的版本，是对的。
 */
export function TestersPage() {
  /**
   * 抽屉状态进 URL（C1）：打开写 `?open=<name>`，关闭删掉。
   * 直接访问 `/testers?open=aisbench-acc` 也会打开 —— 分享/刷新可复现。
   */
  const [params, setParams] = useSearchParams()
  const openName = params.get('open')
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})

  const setOpenName = (name: string | null) => {
    const next = new URLSearchParams(params)
    if (name) next.set('open', name)
    else next.delete('open')
    setParams(next, { replace: true })
  }

  const testers = useAsync(() => api.listTesters({ include_retired: true }), [])
  const list: Tester[] = testers.data ?? []

  const groups = useMemo<TesterGroup[]>(() => {
    const byName = new Map<string, Tester[]>()
    for (const tester of list) {
      byName.set(tester.name, [...(byName.get(tester.name) ?? []), tester])
    }
    return [...byName.entries()]
      .map(([name, versions]) => {
        const sorted = [...versions].sort((a, b) => compareVersion(a.version, b.version))
        const latest = sorted.find((item) => item.latest) ?? sorted.find((item) => !item.retired) ?? sorted[0]!
        return {
          name,
          latest,
          older: sorted.filter((item) => item.version !== latest.version),
        }
      })
      .sort((a, b) => a.name.localeCompare(b.name))
  }, [list])

  return (
    <>
      <p className="text-2xs leading-relaxed text-fg-3">
        Tester 随镜像内置，平台启动时扫描目录自动发现；参数清单是写给写 Run 的人看的说明书，平台不会照着它做校验。
      </p>

      <Card>
        <CardHeader
          tone="primary"
          title="已注册 Tester"
          sub={`${groups.length} 个名字 · 随镜像内置，平台启动时扫描目录自动发现`}
        />
        {testers.error ? (
          <div className="p-4">
            <ErrorState error={testers.error} onRetry={testers.reload} />
          </div>
        ) : testers.loading && list.length === 0 ? (
          <TableSkeleton rows={4} cols={7} />
        ) : groups.length === 0 ? (
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
                {groups.map((group) => {
                  const tester = group.latest
                  const isOpen = Boolean(expanded[group.name])
                  return [
                    <TR key={`${group.name}-latest`}>
                      <TD className="font-mono text-2xs text-fg">{group.name}</TD>
                      <TD>
                        <button
                          type="button"
                          data-testid="tester-version"
                          className="rounded-sm font-mono text-2xs text-primary underline-offset-2 hover:underline"
                          onClick={() => setOpenName(group.name)}
                        >
                          {tester.version}
                        </button>
                        {tester.retired ? <Badge tone="muted" className="ml-1.5">已退役</Badge> : null}
                      </TD>
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
                      <TD className="flex flex-wrap items-center justify-end gap-1.5 text-right">
                        {group.older.length > 0 ? (
                          <Button
                            size="sm"
                            variant="ghost"
                            data-testid="toggle-older-versions"
                            aria-expanded={isOpen}
                            onClick={() => setExpanded((prev) => ({ ...prev, [group.name]: !prev[group.name] }))}
                            icon={
                              <ChevronDown
                                className={cn('size-3 transition-transform duration-150', isOpen && 'rotate-180')}
                                aria-hidden
                              />
                            }
                          >
                            另有 {group.older.length} 个历史版本
                          </Button>
                        ) : null}
                        <Button size="sm" variant="ghost" onClick={() => setOpenName(group.name)}>
                          详情
                        </Button>
                      </TD>
                    </TR>,

                    ...(isOpen
                      ? group.older.map((old) => (
                          <TR key={`${group.name}-${old.version}`} className="bg-surface-2/60">
                            <TD className="pl-6 text-2xs text-fg-3">↳ {group.name}</TD>
                            <TD>
                              <span className="font-mono text-2xs text-fg-2">{old.version}</span>
                              <Badge tone="muted" className="ml-1.5">
                                已退役
                              </Badge>
                            </TD>
                            <TD>
                              <Badge tone="muted">{old.role}</Badge>
                            </TD>
                            <TD>
                              <Badge tone={old.on_failure === 'abort_run' ? 'warn' : 'muted'}>{old.on_failure}</Badge>
                            </TD>
                            <TD className="text-2xs text-fg-3">{old.exclusive ? '是' : '否'}</TD>
                            <TD className="tabular-nums">{old.parameters.length}</TD>
                            <TD className="tabular-nums">{old.env.length}</TD>
                            <TD className="text-right text-2xs text-fg-3">
                              目录里已移除；历史 Run 仍按 name@version 引用
                            </TD>
                          </TR>
                        ))
                      : []),
                  ]
                })}
              </TBody>
            </Table>
          </TableWrap>
        )}
      </Card>

      {/* 与提交页 Task 行的「详情」是同一个组件（TesterDrawer）：内容只写一份 */}
      <TesterDrawer name={openName} onClose={() => setOpenName(null)} />
    </>
  )
}
