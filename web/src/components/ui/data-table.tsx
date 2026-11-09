import { flexRender, type ColumnDef, type Table as TanstackTable } from '@tanstack/react-table'
import { useReactTable, getCoreRowModel } from '@tanstack/react-table'
import type { ReactNode } from 'react'
import { Table, TableWrap, TBody, TD, TH, THead, TR } from '@/components/ui/table'
import { cn } from '@/lib/utils'

/**
 * 表格组件（`@tanstack/react-table` v8，与 shadcn DataTable 同源）。
 *
 * **我们只用它的表格模型，不用它管数据**：分页、列筛选、搜索全部是**服务端**行为，
 * 所以开 `manualPagination` / `manualFiltering`，行数据就是后端这一页的 items ——
 * 不重新发明排序 / 筛选 / 分页状态机，也不做本地全量缓存。
 *
 * 列定义里可用 `meta.filter` 挂一个筛选控件（`FacetFilter`），渲染在列头右侧。
 */

declare module '@tanstack/react-table' {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData, TValue> {
    /** 列头右侧的筛选控件（Excel 式漏斗） */
    filter?: ReactNode
    /** 表头/单元格附加 class */
    className?: string
    /** 表头对齐 */
    align?: 'left' | 'right'
  }
}

export interface DataTableProps<TData> {
  columns: ColumnDef<TData, unknown>[]
  data: TData[]
  /** 空态（一整行） */
  empty?: ReactNode
  /** 行级 testid（便于用例定位某一行） */
  rowTestId?: (row: TData) => string | undefined
  /** 行上额外属性 */
  rowProps?: (row: TData) => Record<string, unknown>
  className?: string
  tableClassName?: string
}

export function DataTable<TData>({
  columns,
  data,
  empty,
  rowTestId,
  rowProps,
  className,
  tableClassName,
}: DataTableProps<TData>) {
  const table: TanstackTable<TData> = useReactTable({
    data,
    columns,
    getCoreRowModel: getCoreRowModel(),
    // 服务端分页 / 筛选：这里只负责渲染
    manualPagination: true,
    manualFiltering: true,
    manualSorting: true,
    enableSorting: false,
  })

  return (
    <TableWrap className={className}>
      <Table className={tableClassName} data-testid="data-table">
        <THead>
          {table.getHeaderGroups().map((headerGroup) => (
            <TR key={headerGroup.id} className="hover:bg-transparent">
              {headerGroup.headers.map((header) => {
                const meta = header.column.columnDef.meta
                return (
                  <TH
                    key={header.id}
                    colSpan={header.colSpan}
                    className={cn(meta?.align === 'right' && 'text-right', meta?.className)}
                  >
                    {header.isPlaceholder ? null : (
                      <span className="inline-flex items-center">
                        {flexRender(header.column.columnDef.header, header.getContext())}
                        {meta?.filter ?? null}
                      </span>
                    )}
                  </TH>
                )
              })}
            </TR>
          ))}
        </THead>
        <TBody>
          {table.getRowModel().rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="px-6 py-10 text-center text-sm text-fg-3">
                {empty ?? '没有数据'}
              </td>
            </tr>
          ) : (
            table.getRowModel().rows.map((row) => (
              <TR key={row.id} data-testid={rowTestId?.(row.original)} {...(rowProps?.(row.original) ?? {})}>
                {row.getVisibleCells().map((cell) => (
                  <TD
                    key={cell.id}
                    className={cn(
                      cell.column.columnDef.meta?.align === 'right' && 'text-right',
                      cell.column.columnDef.meta?.className,
                    )}
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </TD>
                ))}
              </TR>
            ))
          )}
        </TBody>
      </Table>
    </TableWrap>
  )
}
