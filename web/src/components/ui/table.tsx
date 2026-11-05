import type { HTMLAttributes, ReactNode, TdHTMLAttributes, ThHTMLAttributes } from 'react'
import { cn } from '@/lib/utils'

export function TableWrap({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn('w-full overflow-x-auto', className)} {...rest} />
}

export function Table({ className, ...rest }: HTMLAttributes<HTMLTableElement>) {
  return <table className={cn('w-full border-collapse text-left', className)} {...rest} />
}

export function THead({ className, ...rest }: HTMLAttributes<HTMLTableSectionElement>) {
  return <thead className={cn('bg-surface-2', className)} {...rest} />
}

export function TBody({ className, ...rest }: HTMLAttributes<HTMLTableSectionElement>) {
  return <tbody className={className} {...rest} />
}

export function TR({ className, ...rest }: HTMLAttributes<HTMLTableRowElement>) {
  return (
    <tr
      className={cn('border-b border-line-soft transition-colors duration-150 hover:bg-surface-2', className)}
      {...rest}
    />
  )
}

export function TH({ className, ...rest }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      scope="col"
      className={cn(
        'whitespace-nowrap border-b border-line px-4 py-2 text-2xs font-medium text-fg-2',
        className,
      )}
      {...rest}
    />
  )
}

export function TD({ className, ...rest }: TdHTMLAttributes<HTMLTableCellElement>) {
  return <td className={cn('px-4 py-2.5 text-sm text-fg-2 align-middle', className)} {...rest} />
}

/** 空表提示（列表空态）：表格内一行占满宽度。 */
export function TableEmpty({ colSpan, children }: { colSpan: number; children: ReactNode }) {
  return (
    <tbody>
      <tr>
        <td colSpan={colSpan} className="px-4 py-10 text-center text-sm text-fg-3">
          {children}
        </td>
      </tr>
    </tbody>
  )
}
