import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { ChevronDown } from 'lucide-react'
import { api } from '@/api'
import type { Runner, RunnerRole } from '@/api/types'
import { RunnerDrawer } from '@/components/app/RunnerDrawer'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardHeader } from '@/components/ui/card'
import { Segmented } from '@/components/ui/segmented'
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

/**
 * 角色徽章配色（用户 2026-11-11：deploy 也是 runner，目录里按角色一眼分清）：
 * deploy=info（服务来源卡的可选项）、clean=warn（自动注入）、pre_check=primary、test=muted。
 */
const ROLE_TONE: Record<RunnerRole, 'primary' | 'info' | 'warn' | 'muted'> = {
  deploy: 'info',
  clean: 'warn',
  pre_check: 'primary',
  test: 'muted',
}

const ROLE_FILTERS: Array<{ value: 'all' | RunnerRole; label: string }> = [
  { value: 'all', label: '全部' },
  { value: 'test', label: 'test' },
  { value: 'deploy', label: 'deploy' },
  { value: 'clean', label: 'clean' },
  { value: 'pre_check', label: 'pre_check' },
]

interface RunnerGroup {
  name: string
  /** 目录里的当前版本（后端默认只回最新；`latest` 缺省时按版本号取最大） */
  latest: Runner
  /** 历史版本（`?include_retired=true` 才会带回来） */
  older: Runner[]
}

/**
 * Runner 目录：**按 name 聚合**，一行一个 name；deploy / clean 与 test/pre_check 在同一个列表里
 * （v2：deploy 也是 runner，D-70/D-75 —— 角色徽章区分，role 筛选器过滤）。
 *
 * 票 #60 / D-60：`GET /runners` 会同时回 `aisbench-sla@0.1.0` 与 `@0.2.0`，并列展示时用户以为重复。
 * 主行只显示最新版本，历史版本收进「另有 N 个历史版本」——展开才看得到，并标 `已退役`。
 *
 * 历史 Task 的报告/详情里显示的 `name@version` **不改**：那是当时跑的版本，是对的。
 */
export function RunnersPage() {
  /**
   * 抽屉状态进 URL（C1）：打开写 `?open=<name>`，关闭删掉。
   * 直接访问 `/runners?open=aisbench-acc` 也会打开 —— 分享/刷新可复现。
   */
  const [params, setParams] = useSearchParams()
  const openName = params.get('open')
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const [roleFilter, setRoleFilter] = useState<'all' | RunnerRole>('all')

  const setOpenName = (name: string | null) => {
    const next = new URLSearchParams(params)
    if (name) next.set('open', name)
    else next.delete('open')
    setParams(next, { replace: true })
  }

  const runners = useAsync(() => api.listRunners({ include_retired: true }), [])
  const list: Runner[] = runners.data ?? []

  const groups = useMemo<RunnerGroup[]>(() => {
    const byName = new Map<string, Runner[]>()
    for (const runner of list) {
      byName.set(runner.name, [...(byName.get(runner.name) ?? []), runner])
    }
    const all = [...byName.entries()]
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
    return roleFilter === 'all' ? all : all.filter((group) => group.latest.role === roleFilter)
  }, [list, roleFilter])

  return (
    <>
      <Card>
        <CardHeader
          tone="primary"
          title="已注册 Runner"
          sub={
            roleFilter !== 'all'
              ? `${groups.length} 个 · 已按 ${roleFilter} 过滤`
              : `${groups.filter((group) => !group.latest.retired).length} 个`
          }
          actions={
            <Segmented<'all' | RunnerRole>
              ariaLabel="按角色筛选"
              testId="runner-role-filter"
              value={roleFilter}
              onChange={setRoleFilter}
              options={ROLE_FILTERS}
            />
          }
        />
        {runners.error ? (
          <div className="p-4">
            <ErrorState error={runners.error} onRetry={runners.reload} />
          </div>
        ) : runners.loading && list.length === 0 ? (
          <TableSkeleton rows={4} cols={8} />
        ) : groups.length === 0 ? (
          <EmptyState
            title={roleFilter === 'all' ? '目录里还没有 Runner' : `没有 ${roleFilter} 角色的 Runner`}
            description={
              roleFilter === 'all'
                ? '把 runner.yaml + entry 文件放进 runners/<name>/，平台重启后会自动发现。'
                : '换一个角色筛选项看看；deploy / clean 角色只随平台内置。'
            }
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
                  <TH>输入格式</TH>
                  <TH>独占 Endpoint</TH>
                  <TH>参数</TH>
                  <TH className="text-right">操作</TH>
                </TR>
              </THead>
              <TBody>
                {groups.map((group) => {
                  const runner = group.latest
                  const isOpen = Boolean(expanded[group.name])
                  return [
                    <TR key={`${group.name}-latest`}>
                      <TD className="font-mono text-2xs text-fg">{group.name}</TD>
                      <TD>
                        <button
                          type="button"
                          data-testid="runner-version"
                          className="inline-flex h-6 items-center rounded-sm font-mono text-2xs text-primary underline-offset-2 hover:underline"
                          onClick={() => setOpenName(group.name)}
                        >
                          {runner.version}
                        </button>
                        {runner.retired ? <Badge tone="muted" className="ml-1.5">已退役</Badge> : null}
                      </TD>
                      <TD>
                        <Badge tone={ROLE_TONE[runner.role]} data-testid="runner-role">
                          {runner.role}
                        </Badge>
                        {runner.clean_runner ? (
                          <Badge tone="muted" className="ml-1.5" data-testid="runner-clean-pair">
                            clean: {runner.clean_runner}
                          </Badge>
                        ) : null}
                      </TD>
                      <TD>
                        <Badge tone={runner.on_failure === 'abort_task' ? 'warn' : 'muted'}>{runner.on_failure}</Badge>
                      </TD>
                      <TD>
                        <Badge tone={runner.input_format === 'jsonl' ? 'info' : 'muted'}>{runner.input_format}</Badge>
                      </TD>
                      <TD className={cn('text-2xs', runner.exclusive ? 'text-fg' : 'text-fg-3')}>
                        {runner.exclusive ? '是' : '否'}
                      </TD>
                      <TD className="tabular-nums">{runner.parameters.length}</TD>
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
                              <Badge tone={ROLE_TONE[old.role]}>{old.role}</Badge>
                            </TD>
                            <TD>
                              <Badge tone={old.on_failure === 'abort_task' ? 'warn' : 'muted'}>{old.on_failure}</Badge>
                            </TD>
                            <TD>
                              <Badge tone="muted">{old.input_format}</Badge>
                            </TD>
                            <TD className="text-2xs text-fg-3">{old.exclusive ? '是' : '否'}</TD>
                            <TD className="tabular-nums">{old.parameters.length}</TD>
                            <TD className="text-right text-2xs text-fg-3">
                              目录里已移除；历史 Task 仍按 name@version 引用
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

      {/* 与提交页 runner 行的「详情」是同一个组件（RunnerDrawer）：内容只写一份 */}
      <RunnerDrawer name={openName} onClose={() => setOpenName(null)} />
    </>
  )
}
