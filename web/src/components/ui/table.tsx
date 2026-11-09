import type { HTMLAttributes, ReactNode, TdHTMLAttributes, ThHTMLAttributes } from 'react'
import { cn } from '@/lib/utils'

/**
 * 表格（对齐 shadcn Table，见 docs/research/shadcn-visual-specs.md §2.3）：
 *  - 表头**不染底色**，靠 `h-10` 的高度与 `border-b` 分隔（原来那层灰底是"糙"的来源之一）；
 *  - 单元格 `px-3 py-2`（12/8），行高由 `text-sm` 撑到 ~40px；
 *  - 行 hover 用半透明的 `bg-muted/60`，深浅不跳；末行不画 border。
 * 偏离一处（有意）：表头字号用 `text-xs`(12) 而不是 shadcn 的 14——我们是 6+ 列的密集数据表，
 * 12px 表头 + 14px 正文的对比更清楚。见报告里的"没做到的部分"。
 */
export function TableWrap({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('relative w-full overflow-x-auto', className)} {...rest} />
}

export function Table({ className, ...rest }: HTMLAttributes<HTMLTableElement>) {
  return <table className={cn('w-full border-collapse text-left text-sm', className)} {...rest} />
}

export function THead({ className, ...rest }: HTMLAttributes<HTMLTableSectionElement>) {
  return <thead className={cn('[&_tr]:border-b [&_tr]:border-line', className)} {...rest} />
}

export function TBody({ className, ...rest }: HTMLAttributes<HTMLTableSectionElement>) {
  return <tbody className={cn('[&_tr:last-child]:border-0', className)} {...rest} />
}

export function TR({ className, ...rest }: HTMLAttributes<HTMLTableRowElement>) {
  return (
    <tr
      className={cn(
        'border-b border-line-soft transition-colors duration-150 hover:bg-muted/60',
        className,
      )}
      {...rest}
    />
  )
}

export function TH({ className, ...rest }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      scope="col"
      className={cn(
        'h-10 whitespace-nowrap px-3 text-left align-middle text-xs font-medium text-fg-2',
        className,
      )}
      {...rest}
    />
  )
}

export function TD({ className, ...rest }: TdHTMLAttributes<HTMLTableCellElement>) {
  return <td className={cn('px-3 py-2 align-middle text-sm text-fg-2', className)} {...rest} />
}

/** 空表提示（列表空态）：表格内一行占满宽度。 */
export function TableEmpty({ colSpan, children }: { colSpan: number; children: ReactNode }) {
  return (
    <tbody>
      <tr>
        <td colSpan={colSpan} className="px-6 py-10 text-center text-sm text-fg-3">
          {children}
        </td>
      </tr>
    </tbody>
  )
}
