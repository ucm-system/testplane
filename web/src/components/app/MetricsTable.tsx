import { Fragment, useMemo } from 'react'
import type { Result, ResultMetric } from '@/api/types'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'

/**
 * 一张指标表（附录 A schema 2.0）：列 = labels 键 + 指标名 + 统计口径 + 值 + 单位。
 * 从报告页搬出来公用（Task 详情的「结果」视图按 runner 内联渲染 result.json 也用同一份）。
 */

function formatMetricValue(value: number | null): string {
  if (value == null) return '—'
  if (Number.isInteger(value)) return value.toLocaleString('zh-CN')
  const abs = Math.abs(value)
  const digits = abs >= 100 ? 1 : abs >= 1 ? 2 : 4
  return value.toLocaleString('zh-CN', { maximumFractionDigits: digits })
}

interface MetricGroup {
  labels: Array<[string, string]>
  metrics: ResultMetric[]
}

/** 按 `labels` 分组（同组参数的指标排在一起，列里不重复贴标签；labels 值允许非字符串） */
function groupByLabels(metrics: ResultMetric[]): MetricGroup[] {
  const groups = new Map<string, MetricGroup>()
  const order: string[] = []
  for (const metric of metrics) {
    const raw = metric.labels ?? {}
    const pairs: Array<[string, string]> = Object.keys(raw)
      .sort()
      .map((name) => [name, String(raw[name])])
    const key = JSON.stringify(pairs)
    let group = groups.get(key)
    if (!group) {
      group = { labels: pairs, metrics: [] }
      groups.set(key, group)
      order.push(key)
    }
    group.metrics.push(metric)
  }
  return order.map((key) => groups.get(key)!).filter(Boolean)
}

export function MetricsTable({ result }: { result: Result }) {
  const groups = useMemo(() => groupByLabels(result.metrics), [result.metrics])
  const labelKeys = useMemo(() => {
    const keys: string[] = []
    for (const group of groups) {
      for (const [key] of group.labels) if (!keys.includes(key)) keys.push(key)
    }
    return keys
  }, [groups])

  return (
    <TableWrap className="rounded-md border border-line-soft">
      <Table data-testid="metrics-table">
        <THead>
          <TR className="hover:bg-transparent">
            {labelKeys.map((key) => (
              <TH key={key}>{key}</TH>
            ))}
            <TH>指标</TH>
            <TH>值</TH>
            <TH>单位</TH>
          </TR>
        </THead>
        <TBody>
          {groups.map((group) => {
            const groupKey = JSON.stringify(group.labels)
            return (
              <Fragment key={groupKey}>
                {group.metrics.map((metric, index) => (
                  <TR key={`${groupKey}-${metric.name}-${metric.statistic}-${index}`}>
                    {index === 0
                      ? group.labels.map(([labelKey, labelValue]) => (
                          <TD
                            key={labelKey}
                            rowSpan={group.metrics.length}
                            className="align-top font-mono text-2xs text-fg-2"
                          >
                            {labelValue}
                          </TD>
                        ))
                      : null}
                    <TD className="text-fg">
                      <span className="font-mono text-xs">{metric.name}</span>
                      <span className="ml-1.5 text-2xs text-fg-3">{metric.statistic}</span>
                    </TD>
                    <TD className="tabular-nums">{formatMetricValue(metric.value)}</TD>
                    <TD className="font-mono text-2xs text-fg-3">{metric.unit}</TD>
                  </TR>
                ))}
              </Fragment>
            )
          })}
        </TBody>
      </Table>
    </TableWrap>
  )
}
