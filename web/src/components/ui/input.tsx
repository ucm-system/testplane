import type { InputHTMLAttributes, LabelHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from 'react'
import { cn } from '@/lib/utils'

/** 输入控件的公共外观（原型 .fde-input / .fde-select / .fde-textarea）。 */
const FIELD =
  'w-full rounded-md border border-line bg-surface-2 px-3 text-sm text-fg placeholder:text-fg-3 ' +
  'transition-[background-color,border-color,box-shadow] duration-150 ease-out ' +
  'hover:border-line-strong focus:border-primary focus:bg-surface focus:outline-none ' +
  'focus:shadow-[0_0_0_3px_var(--color-ring)] disabled:cursor-not-allowed disabled:opacity-60'

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(FIELD, 'h-9', className)} {...rest} />
}

export function Textarea({ className, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn(FIELD, 'py-2 leading-relaxed', className)} {...rest} />
}

export function Select({ className, children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select className={cn(FIELD, 'h-9 cursor-pointer pr-2', className)} {...rest}>
      {children}
    </select>
  )
}

export function Label({ className, children, ...rest }: LabelHTMLAttributes<HTMLLabelElement>) {
  return (
    <label className={cn('block text-xs font-medium text-fg-2', className)} {...rest}>
      {children}
    </label>
  )
}

export interface FieldProps {
  label?: ReactNode
  hint?: ReactNode
  /** 字段级错误（来自错误模型 detail.issues[].path 的定位） */
  error?: ReactNode
  required?: boolean
  className?: string
  children: ReactNode
  htmlFor?: string
}

export function Field({ label, hint, error, required, className, children, htmlFor }: FieldProps) {
  return (
    <div className={cn('flex flex-col gap-1.5', className)}>
      {label ? (
        <Label htmlFor={htmlFor}>
          {label}
          {required ? <span className="ml-0.5 text-danger">*</span> : null}
        </Label>
      ) : null}
      {children}
      {error ? (
        <p role="alert" className="text-2xs text-danger-text">
          {error}
        </p>
      ) : hint ? (
        <p className="text-2xs leading-relaxed text-fg-3">{hint}</p>
      ) : null}
    </div>
  )
}

/** 只读键值对（详情页概要、Deploy 快照用）。 */
export function KV({ label, children, className }: { label: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={cn('flex min-w-0 items-baseline gap-3 text-sm', className)}>
      <dt className="w-24 shrink-0 text-xs text-fg-3">{label}</dt>
      <dd className="min-w-0 text-fg-2">{children}</dd>
    </div>
  )
}
