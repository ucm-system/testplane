import { useEffect, useMemo, useRef, useState } from 'react'
import CodeMirror, {
  Decoration,
  EditorView,
  GutterMarker,
  RangeSet,
  RangeSetBuilder,
  StateEffect,
  StateField,
  gutter,
  keymap,
} from '@uiw/react-codemirror'
import { json } from '@codemirror/lang-json'
import { Check, Copy, FileJson } from 'lucide-react'
import { cn } from '@/lib/utils'
import { editorAutoHeight, editorContentLines, useEditorAutoGrow } from '@/lib/editor-autogrow'

/**
 * JSONL 编辑器 —— 与 YamlEditor 同一套 CodeMirror 6 架构（用户 2026-11-12：
 * jsonl 输入框样式/校验体验对齐 YAML）。
 *
 * 与 textarea 的差别：真语法高亮（@codemirror/lang-json）+ 行号 + 横向滚动 +
 * 错误行标注（红波浪 / gutter 圆点 / 整行淡红底 + 自动滚过去）。
 *
 * `error` 由页面层的 jsonl 校验（lib/input-validate，D-67）给出：取**第一条 issue 的
 * 行号**（跨行 JSON / 语法 / required / type 都有行号），多字段错误仍由页面下方的
 * 行内错误列表完整展示。
 *
 * 表单/测试镜像：渲染一个 `sr-only` 的受控 textarea（同 aria-label 之外的独立 label），
 * 同步 value——`toHaveValue` / 提交序列化都读它；编辑交互在 CodeMirror 里做。
 *
 * 刻意不引 `@codemirror/lint`（与 yaml-editor 同理：只是传递 peer，直接 import 解析不到），
 * 用 CM6 自带 StateField + Decoration + gutter 复刻 lint 呈现，不新增运行时依赖。
 */

export interface JsonlEditorError {
  /** 1-based 行号（issue.line） */
  line: number
}

export interface JsonlEditorProps {
  value: string
  onChange: (next: string) => void
  rows?: number
  'aria-label'?: string
  className?: string
  placeholder?: string
  showLineNumbers?: boolean
  /** 顶部工具条标题（缺省用 aria-label） */
  title?: string
  /** 底部提示行右侧文案 */
  hint?: string
  /** 校验错误位置（第一条 issue 的行号）：给了就标行并滚过去 */
  error?: JsonlEditorError | null
  /** 隐藏镜像 textarea 的可访问名（默认 `${ariaLabel} 值`）；页面测试用它做 toHaveValue */
  mirrorLabel?: string
  /** 根节点 data-testid（同页多个编辑器时定位用） */
  testId?: string
}

/* ================================================================ 错误标注（与 yaml-editor 同构） */

interface JsonlDiag {
  lineFrom: number
  from: number
  to: number
}

const setJsonlDiag = StateEffect.define<JsonlDiag | null>()

const jsonlDiagField = StateField.define<JsonlDiag | null>({
  create: () => null,
  update(value, tr) {
    for (const effect of tr.effects) if (effect.is(setJsonlDiag)) return effect.value
    return tr.docChanged ? null : value
  },
})

class ErrorDot extends GutterMarker {
  override toDOM(): Node {
    const dot = document.createElement('span')
    dot.className = 'cm-jsonl-error-dot'
    dot.textContent = '●'
    dot.setAttribute('aria-hidden', 'true')
    return dot
  }
}

class SpacerDot extends GutterMarker {
  override toDOM(): Node {
    const dot = document.createElement('span')
    dot.className = 'cm-jsonl-error-spacer'
    dot.textContent = '●'
    dot.setAttribute('aria-hidden', 'true')
    return dot
  }
}

const errorDot = new ErrorDot()

const errorLineDeco = Decoration.line({ class: 'cm-jsonl-error-line' })
const errorMarkDeco = Decoration.mark({ class: 'cm-jsonl-error-mark' })

const jsonlErrorDecorations = EditorView.decorations.from(jsonlDiagField, (diag) => {
  if (!diag) return Decoration.none
  const builder = new RangeSetBuilder<Decoration>()
  builder.add(diag.lineFrom, diag.lineFrom, errorLineDeco)
  if (diag.to > diag.from) builder.add(diag.from, diag.to, errorMarkDeco)
  return builder.finish()
})

const jsonlErrorGutter = gutter({
  class: 'cm-jsonl-error-gutter',
  markers: (view) => {
    const diag = view.state.field(jsonlDiagField, false)
    return diag ? RangeSet.of([errorDot.range(diag.lineFrom)]) : RangeSet.empty
  },
  lineMarkerChange: (update) => update.transactions.some((tr) => tr.effects.some((e) => e.is(setJsonlDiag))),
  initialSpacer: () => new SpacerDot(),
})

/* ================================================================ 编辑手感 */

const INDENT = '  '

/** Tab 插 2 个空格（与 YAML 编辑器一致；JSONL 里 Tab 同样不该出现在记录内）。 */
function insertIndent(view: EditorView): boolean {
  const { state } = view
  const changes: Array<{ from: number; to: number; insert: string }> = []
  for (const range of state.selection.ranges) {
    if (range.empty) {
      changes.push({ from: range.from, to: range.from, insert: INDENT })
      continue
    }
    for (let n = state.doc.lineAt(range.from).number; n <= state.doc.lineAt(range.to).number; n += 1) {
      const line = state.doc.line(n)
      changes.push({ from: line.from, to: line.from, insert: INDENT })
    }
  }
  if (changes.length === 0) return false
  view.dispatch({ changes, userEvent: 'input', scrollIntoView: true })
  return true
}

const JSONL_KEYMAP = keymap.of([{ key: 'Tab', run: insertIndent }])

const JSONL_THEME = EditorView.theme({
  '&': {
    height: '100%',
    fontSize: '12px',
    lineHeight: '1.7',
    backgroundColor: 'transparent',
    color: 'var(--color-fg)',
  },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'var(--font-mono)', overflow: 'auto' },
  '.cm-content': { padding: '8px 0', caretColor: 'var(--color-fg)' },
  '.cm-line': { padding: '0 10px' },
  '.cm-gutters': {
    backgroundColor: 'var(--color-surface-3)',
    color: 'var(--color-fg-3)',
    border: 'none',
    borderRight: '1px solid var(--color-line-soft)',
  },
  '.cm-lineNumbers .cm-gutterElement': { padding: '0 6px 0 8px', minWidth: '24px', fontSize: '12px' },
  '.cm-jsonl-error-gutter': { width: '14px' },
  '.cm-jsonl-error-gutter .cm-gutterElement': { display: 'grid', placeItems: 'center', padding: 0 },
  '.cm-jsonl-error-dot': { color: 'var(--color-danger)', fontSize: '9px', lineHeight: '1.7' },
  '.cm-jsonl-error-line': { backgroundColor: 'rgba(239, 68, 68, 0.08)' },
  '.cm-jsonl-error-mark': {
    textDecoration: 'underline wavy var(--color-danger)',
    textUnderlineOffset: '2px',
  },
  '.cm-placeholder': { color: 'var(--color-fg-3)' },
})

/* ================================================================ 组件 */

export function JsonlEditor({
  value,
  onChange,
  rows = 4,
  className,
  placeholder,
  showLineNumbers = true,
  'aria-label': ariaLabel,
  title,
  hint,
  error = null,
  mirrorLabel,
  testId,
}: JsonlEditorProps) {
  const viewRef = useRef<EditorView | null>(null)
  const [mounted, setMounted] = useState(false)
  const boxRef = useRef<HTMLDivElement | null>(null)
  // 高度随内容自动长（封顶半屏），用户拖过就以用户为准 —— 机制见 lib/editor-autogrow
  const autoHeight = useMemo(() => editorAutoHeight(rows, editorContentLines(value, rows)), [rows, value])
  useEditorAutoGrow(boxRef, autoHeight)
  const [cursor, setCursor] = useState({ line: 1, col: 1 })
  const [copied, setCopied] = useState(false)

  const extensions = useMemo(
    () => [
      json(),
      JSONL_KEYMAP,
      jsonlDiagField,
      jsonlErrorDecorations,
      jsonlErrorGutter,
      JSONL_THEME,
      EditorView.updateListener.of((update) => {
        if (!update.selectionSet && !update.docChanged) return
        const head = update.state.selection.main.head
        const line = update.state.doc.lineAt(head)
        setCursor({ line: line.number, col: head - line.from + 1 })
      }),
      EditorView.contentAttributes.of({
        ...(error ? { 'aria-invalid': 'true' } : {}),
      }),
    ],
    [ariaLabel, error],
  )

  // 第一条错误的行号 → 标行 + 滚过去（列级细节交给页面下方的行内错误列表）
  useEffect(() => {
    const view = viewRef.current
    if (!mounted || !view) return
    if (!error) {
      view.dispatch({ effects: setJsonlDiag.of(null) })
      return
    }
    const doc = view.state.doc
    const lineNo = Math.min(Math.max(1, Math.round(error.line)), doc.lines)
    const line = doc.line(lineNo)
    view.dispatch({
      effects: [setJsonlDiag.of({ lineFrom: line.from, from: line.from, to: line.to }), EditorView.scrollIntoView(line.from, { y: 'center' })],
    })
  }, [error, value, mounted])

  const copyValue = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      // 剪贴板被浏览器策略拒绝时不打扰用户
    }
  }

  return (
    <div
      className={cn(
        'relative flex flex-col overflow-hidden rounded-lg border bg-surface',
        'resize-y min-h-28 max-h-[70vh]',
        'transition-[border-color,box-shadow] duration-150 focus-within:shadow-[0_0_0_3px_var(--color-ring)]',
        error ? 'border-danger focus-within:border-danger' : 'border-line focus-within:border-primary',
        className,
      )}
      ref={boxRef}
      data-testid={testId ?? 'jsonl-editor'}
      data-jsonl-error={error ? `${error.line}` : undefined}
    >
      {/* 隐藏镜像：表单/测试的 value 通道（toHaveValue 读这里；编辑交互在 CodeMirror） */}
      <textarea
        aria-label={mirrorLabel ?? `${ariaLabel ?? 'JSONL'} 值`}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="sr-only"
        tabIndex={-1}
        spellCheck={false}
      />

      <div className="flex shrink-0 items-center gap-2 border-b border-line bg-surface-2 px-3 py-1.5">
        <FileJson className="size-3.5 shrink-0 text-fg-3" aria-hidden />
        <span className="truncate font-mono text-xs font-medium text-fg">{title ?? ariaLabel ?? 'JSONL'}</span>
        <span className="ml-auto shrink-0 text-2xs tabular-nums text-fg-3" data-testid="jsonl-editor-cursor">
          第 {cursor.line} 行，第 {cursor.col} 列
        </span>
        <button
          type="button"
          onClick={() => void copyValue()}
          aria-label={copied ? '已复制' : '复制'}
          className="inline-flex size-6 shrink-0 items-center justify-center rounded-sm text-fg-3 transition-colors hover:bg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          {copied ? <Check className="size-3.5 text-success-text" aria-hidden /> : <Copy className="size-3.5" aria-hidden />}
        </button>
      </div>

      <div className="min-h-0 flex-1">
        <CodeMirror
          value={value}
          onChange={(next) => onChange(next)}
          height="100%"
          style={{ height: '100%' }}
          className="h-full"
          placeholder={placeholder}
          theme="light"
          indentWithTab={false}
          basicSetup={{
            lineNumbers: showLineNumbers,
            foldGutter: false,
            highlightActiveLine: false,
            highlightActiveLineGutter: false,
            autocompletion: false,
            bracketMatching: true,
            closeBrackets: false,
            highlightSelectionMatches: false,
            searchKeymap: false,
          }}
          extensions={extensions}
          onCreateEditor={(view) => {
            viewRef.current = view
            setMounted(true)
          }}
        />
      </div>

      <div className="flex shrink-0 items-center gap-2 border-t border-line bg-surface-2 px-3 py-1.5 text-2xs">
        {error ? (
          <span className="text-danger-text">JSONL 校验失败：第 {error.line} 行（详见下方错误列表）</span>
        ) : (
          <span className="inline-flex items-center gap-1 text-fg-3">
            <span className="size-1.5 rounded-full bg-success" aria-hidden />
            JSONL 每行一个 JSON 对象
          </span>
        )}
        <span className="ml-auto truncate text-fg-3">{hint ?? '拖动右下角可调整高度'}</span>
      </div>
    </div>
  )
}
