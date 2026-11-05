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
import { yaml } from '@codemirror/lang-yaml'
import { cn } from '@/lib/utils'

/**
 * YAML 编辑器 —— **CodeMirror 6**（`@uiw/react-codemirror` + `@codemirror/lang-yaml`）。
 *
 * 为什么换掉手搓的「透明 textarea 叠高亮 pre」：
 *  - 那套只能靠正则上色，**报错没法标到行**；用户看到「YAML 解析失败」却不知道该改第几行；
 *  - 长值（超长 model 名）在 textarea 里要手动横滚两层，容易错位。
 *
 * 现在：真语法高亮 + 行号 + 横向滚动（默认就有，**不要**开 lineWrapping）；
 * 解析失败时把 `line`/`col` 标成 lint 样的错误：红色波浪线 + 左侧 gutter 圆点 + 整行淡红底，
 * 并把编辑器滚到该行（`EditorView.scrollIntoView`）。
 *
 * 对外 API 与旧实现兼容：`value` / `onChange` / `rows`（高度）/ `aria-label` / `showLineNumbers`
 * / `placeholder` / `className` 全保留，页面不用改；新增可选 `error`（行号来源见页面里的 `yaml` `LinePos`）。
 *
 * 说明（刻意不引第三个包）：`@codemirror/lint` 目前只是 `@uiw/react-codemirror` 的传递 peer
 * （`package.json` 没有直接声明），从 `src/` 直接 `import` 解析不到；本文件用 CM6 自带的
 * `StateField + Decoration + gutter` 复刻同一套「lint error」呈现，不新增依赖。
 */

export interface YamlEditorError {
  /** 1-based 行号（`yaml` 包 `LinePos.line`） */
  line: number
  /** 1-based 列号（`yaml` 包 `LinePos.col`） */
  col: number
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
  /** YAML 解析错误位置：给了就把该行/该列标成错误并滚过去 */
  error?: YamlEditorError | null
}

/* ================================================================ 错误标注（lint 呈现） */

interface YamlDiag {
  /** 出错行的行首（gutter 圆点 + 整行底色挂在这里） */
  lineFrom: number
  /** 出错位置起止（列级波浪线） */
  from: number
  to: number
}

/** 唯一的写入通道：`null` = 清掉标注 */
const setYamlDiag = StateEffect.define<YamlDiag | null>()

const yamlDiagField = StateField.define<YamlDiag | null>({
  create: () => null,
  update(value, tr) {
    for (const effect of tr.effects) if (effect.is(setYamlDiag)) return effect.value
    // 文档一改，旧坐标就作废：先清掉，等页面重新解析后再标（避免标到错行）
    return tr.docChanged ? null : value
  },
})

class ErrorDot extends GutterMarker {
  override toDOM(): Node {
    const dot = document.createElement('span')
    dot.className = 'cm-yaml-error-dot'
    dot.textContent = '●'
    dot.setAttribute('aria-hidden', 'true')
    return dot
  }
}

/** 占位圆点：只用来撑出 gutter 宽度，本身是隐藏的（别让它被当成「有错误」） */
class SpacerDot extends GutterMarker {
  override toDOM(): Node {
    const dot = document.createElement('span')
    dot.className = 'cm-yaml-error-spacer'
    dot.textContent = '●'
    dot.setAttribute('aria-hidden', 'true')
    return dot
  }
}

const errorDot = new ErrorDot()

const errorLineDeco = Decoration.line({ class: 'cm-yaml-error-line' })
const errorMarkDeco = Decoration.mark({ class: 'cm-yaml-error-mark' })

/** 行底 + 列级波浪线（两者都必须按 from 升序塞进 RangeSetBuilder） */
const yamlErrorDecorations = EditorView.decorations.from(yamlDiagField, (diag) => {
  if (!diag) return Decoration.none
  const builder = new RangeSetBuilder<Decoration>()
  builder.add(diag.lineFrom, diag.lineFrom, errorLineDeco)
  if (diag.to > diag.from) builder.add(diag.from, diag.to, errorMarkDeco)
  return builder.finish()
})

const yamlErrorGutter = gutter({
  class: 'cm-yaml-error-gutter',
  markers: (view) => {
    const diag = view.state.field(yamlDiagField, false)
    return diag ? RangeSet.of([errorDot.range(diag.lineFrom)]) : RangeSet.empty
  },
  // 标注由 StateField 驱动：只有它变了才需要重画 gutter（否则每次输入都重排）
  lineMarkerChange: (update) => update.transactions.some((tr) => tr.effects.some((e) => e.is(setYamlDiag))),
  initialSpacer: () => new SpacerDot(),
})

/* ================================================================ 编辑手感 */

const INDENT = '  '

/**
 * Tab 一律插 **2 个空格**（YAML 的缩进不允许 Tab）；
 * 有选区时整段右移，和上一版手搓编辑器的行为一致。
 */
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

const YAML_KEYMAP = keymap.of([{ key: 'Tab', run: insertIndent }])

/* ================================================================ 外观 */

const YAML_THEME = EditorView.theme({
  '&': {
    height: '100%',
    fontSize: '12.5px',
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
  '.cm-lineNumbers .cm-gutterElement': { padding: '0 6px 0 8px', minWidth: '24px' },
  '.cm-yaml-error-gutter': { width: '14px' },
  '.cm-yaml-error-gutter .cm-gutterElement': { display: 'grid', placeItems: 'center', padding: 0 },
  '.cm-yaml-error-dot': { color: 'var(--color-danger)', fontSize: '9px', lineHeight: '1.7' },
  '.cm-yaml-error-line': { backgroundColor: 'rgba(239, 68, 68, 0.08)' },
  '.cm-yaml-error-mark': {
    textDecoration: 'underline wavy var(--color-danger)',
    textUnderlineOffset: '2px',
  },
  '.cm-placeholder': { color: 'var(--color-fg-3)' },
})

/* ================================================================ 组件 */

export function YamlEditor({
  value,
  onChange,
  rows = 7,
  className,
  placeholder,
  showLineNumbers = true,
  'aria-label': ariaLabel,
  error = null,
}: YamlEditorProps) {
  const viewRef = useRef<EditorView | null>(null)
  const [mounted, setMounted] = useState(false)

  const extensions = useMemo(
    () => [
      yaml(),
      YAML_KEYMAP,
      yamlDiagField,
      yamlErrorDecorations,
      yamlErrorGutter,
      YAML_THEME,
      EditorView.contentAttributes.of({
        'aria-label': ariaLabel ?? 'YAML',
        ...(error ? { 'aria-invalid': 'true' } : {}),
      }),
    ],
    [ariaLabel, error],
  )

  // 解析错误 → 标行 + 滚过去（错误位置由页面的 `yaml` LinePos 给出）
  useEffect(() => {
    const view = viewRef.current
    if (!mounted || !view) return
    if (!error) {
      view.dispatch({ effects: setYamlDiag.of(null) })
      return
    }
    const doc = view.state.doc
    const lineNo = Math.min(Math.max(1, Math.round(error.line)), doc.lines)
    const line = doc.line(lineNo)
    const col = Math.min(Math.max(1, Math.round(error.col)), line.length + 1)
    // 列可能落在行尾之后（例如缺收尾括号：报错指向文末）
    let from = Math.min(line.from + col - 1, doc.length)
    let to = Math.min(Math.max(from + 1, line.to), doc.length)
    if (to <= from) {
      // 标不出一段就退回整行：至少让人看见是哪一行错了
      from = line.from
      to = line.to
    }
    view.dispatch({
      effects: [setYamlDiag.of({ lineFrom: line.from, from, to }), EditorView.scrollIntoView(line.from, { y: 'center' })],
    })
  }, [error, value, mounted])

  return (
    <div
      className={cn(
        'relative overflow-hidden rounded-md border bg-surface-2',
        'transition-[border-color,box-shadow] duration-150 focus-within:shadow-[0_0_0_3px_var(--color-ring)]',
        error ? 'border-danger focus-within:border-danger' : 'border-line focus-within:border-primary',
        className,
      )}
      style={{ height: `calc(${rows} * 1.7 * 12.5px + 16px)` }}
      data-testid="yaml-editor"
      data-yaml-error={error ? `${error.line}:${error.col}` : undefined}
    >
      <CodeMirror
        value={value}
        onChange={(next) => onChange(next)}
        height="100%"
        style={{ height: '100%' }}
        className="h-full"
        placeholder={placeholder}
        theme="light"
        // 默认的 indentWithTab 会插 "\t"（YAML 里是非法缩进），改用上面的 YAML_KEYMAP
        indentWithTab={false}
        basicSetup={{
          lineNumbers: showLineNumbers,
          foldGutter: false,
          highlightActiveLine: false,
          highlightActiveLineGutter: false,
          autocompletion: false,
          bracketMatching: false,
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
  )
}
