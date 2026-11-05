import { useLayoutEffect, useMemo, useRef, type CSSProperties } from 'react'
import { cn } from '@/lib/utils'

/**
 * 轻量 YAML 编辑器：**透明 textarea 叠在高亮 `<pre>` 上**。
 *
 * 为什么不用 CodeMirror/Monaco：为了两处小输入框（Deploy YAML、Task 参数）引入 300KB+ 的
 * 编辑器内核不划算；这里用正则给 key / 字符串 / 注释 / 数字上色，滚动与光标天然同步
 * （两层尺寸完全一致，textarea 文字透明但 caret 可见）。
 *
 * 保留的能力：可编辑、可粘贴、**Tab 缩进 2 空格**（有选区时整体缩进）、`aria-label` 透传、
 * 行号（可选）。字体与原型一致：等宽 12.5px / 1.7 行高。
 */

const FONT: CSSProperties = {
  fontFamily: 'var(--font-mono)',
  fontSize: '12.5px',
  lineHeight: '1.7',
  tabSize: 2,
}

const TOKEN = {
  key: 'text-[#2563eb]',
  string: 'text-[#047857]',
  comment: 'text-fg-3 italic',
  number: 'text-[#b45309]',
  bool: 'text-[#7c3aed]',
  dash: 'text-fg-3',
  plain: 'text-fg',
} as const

interface Piece {
  text: string
  tone: keyof typeof TOKEN
}

/** 一行 YAML → 若干带颜色的片段（顺序拼接后必须与原行完全一致，否则会错位） */
function tokenizeLine(line: string): Piece[] {
  const pieces: Piece[] = []
  let rest = line

  // 整行注释
  const commentAt = rest.indexOf('#')
  let comment = ''
  if (commentAt >= 0) {
    comment = rest.slice(commentAt)
    rest = rest.slice(0, commentAt)
  }

  // 缩进 + 列表短横线
  const indentMatch = /^(\s*)(- )?/.exec(rest)
  const indent = indentMatch?.[1] ?? ''
  const dash = indentMatch?.[2] ?? ''
  if (indent) pieces.push({ text: indent, tone: 'plain' })
  if (dash) pieces.push({ text: dash, tone: 'dash' })
  let body = rest.slice(indent.length + dash.length)

  // key:
  const keyMatch = /^([\w.$/-]+)(\s*:)(\s*)(.*)$/.exec(body)
  if (keyMatch) {
    pieces.push({ text: keyMatch[1]!, tone: 'key' })
    pieces.push({ text: keyMatch[2]!, tone: 'plain' })
    if (keyMatch[3]) pieces.push({ text: keyMatch[3], tone: 'plain' })
    body = keyMatch[4] ?? ''
  }

  if (body) {
    if (/^['"].*['"]$/.test(body)) pieces.push({ text: body, tone: 'string' })
    else if (/^-?\d+(\.\d+)?$/.test(body)) pieces.push({ text: body, tone: 'number' })
    else if (/^(true|false|null|yes|no)$/i.test(body)) pieces.push({ text: body, tone: 'bool' })
    else if (body.includes('#')) {
      const idx = body.indexOf('#')
      pieces.push({ text: body.slice(0, idx), tone: 'string' })
      pieces.push({ text: body.slice(idx), tone: 'comment' })
    } else pieces.push({ text: body, tone: 'string' })
  }

  if (comment) pieces.push({ text: comment, tone: 'comment' })
  return pieces
}

export interface YamlEditorProps {
  value: string
  onChange: (next: string) => void
  rows?: number
  'aria-label'?: string
  className?: string
  placeholder?: string
  /** 左侧行号（默认开） */
  showLineNumbers?: boolean
}

export function YamlEditor({
  value,
  onChange,
  rows = 7,
  className,
  placeholder,
  showLineNumbers = true,
  'aria-label': ariaLabel,
}: YamlEditorProps) {
  const taRef = useRef<HTMLTextAreaElement>(null)
  const preRef = useRef<HTMLPreElement>(null)
  const gutterRef = useRef<HTMLDivElement>(null)

  const lines = useMemo(() => value.split('\n'), [value])

  // 两层滚动同步（用户滚哪边都跟上）
  useLayoutEffect(() => {
    const ta = taRef.current
    if (!ta) return
    const sync = () => {
      const { scrollTop, scrollLeft } = ta
      if (preRef.current) {
        preRef.current.scrollTop = scrollTop
        preRef.current.scrollLeft = scrollLeft
      }
      if (gutterRef.current) gutterRef.current.scrollTop = scrollTop
    }
    ta.addEventListener('scroll', sync)
    return () => ta.removeEventListener('scroll', sync)
  }, [])

  /** Tab 缩进 2 空格；有选区时整段缩进/反缩进。用 setRangeText 保留浏览器撤销栈。 */
  const handleKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== 'Tab') return
    event.preventDefault()
    const ta = event.currentTarget
    const { selectionStart, selectionEnd } = ta
    const before = value.slice(0, selectionStart)
    const selected = value.slice(selectionStart, selectionEnd)

    if (!selected.includes('\n') && !event.shiftKey) {
      ta.setRangeText('  ', selectionStart, selectionEnd, 'end')
      onChange(ta.value)
      return
    }
    const next = selected
      .split('\n')
      .map((line) => (event.shiftKey ? line.replace(/^ {1,2}/, '') : `  ${line}`))
      .join('\n')
    ta.setRangeText(next, selectionStart, selectionEnd, 'select')
    onChange(ta.value)
    void before
  }

  return (
    <div
      className={cn(
        'relative flex overflow-hidden rounded-md border border-line bg-surface-2',
        'transition-[border-color,box-shadow] duration-150 focus-within:border-primary focus-within:shadow-[0_0_0_3px_var(--color-ring)]',
        className,
      )}
      style={{ height: `calc(${rows} * 1.7 * 12.5px + 16px)` }}
    >
      {showLineNumbers ? (
        <div
          ref={gutterRef}
          aria-hidden
          className="select-none overflow-hidden border-r border-line-soft bg-surface-3/60 px-2 py-2 text-right text-fg-3"
          style={{ ...FONT, minWidth: 34 }}
        >
          {lines.map((_, index) => (
            <div key={index}>{index + 1}</div>
          ))}
        </div>
      ) : null}

      <div className="relative min-w-0 flex-1">
        {/* 高亮层：与 textarea 完全同尺寸、同字体、同 padding，文字本身不接收事件 */}
        <pre
          ref={preRef}
          aria-hidden
          className="pointer-events-none absolute inset-0 overflow-hidden px-3 py-2"
          style={FONT}
        >
          {lines.map((line, index) => (
            <div key={index} className="whitespace-pre">
              {tokenizeLine(line).map((piece, pieceIndex) => (
                <span key={pieceIndex} className={TOKEN[piece.tone]}>
                  {piece.text}
                </span>
              ))}
              {line.length === 0 ? ' ' : null}
            </div>
          ))}
        </pre>

        <textarea
          ref={taRef}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          onKeyDown={handleKeyDown}
          rows={rows}
          spellCheck={false}
          aria-label={ariaLabel}
          placeholder={placeholder}
          className="absolute inset-0 w-full resize-none overflow-auto bg-transparent px-3 py-2 text-transparent caret-fg outline-none placeholder:text-fg-3"
          style={FONT}
        />
      </div>
    </div>
  )
}
