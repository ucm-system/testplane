import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { AlignLeft, Info, ListChecks, Minimize2, Plus, Trash2, Wand2 } from 'lucide-react'
import { parse as parseYaml } from 'yaml'
import { api, writeBlockedReason } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, TaskManifest, ValidationIssue } from '@/api/types'
import { RunnerDrawer, RunnerParamTable } from '@/components/app/RunnerDrawer'
import { Badge, Tag } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { Field, Input, Select, Textarea } from '@/components/ui/input'
import { YamlEditor, type YamlEditorError } from '@/components/ui/yaml-editor'
import { Segmented } from '@/components/ui/segmented'
import { Banner } from '@/components/ui/states'
import { FormErrorSummary } from '@/components/app/FormErrorSummary'
import { useToast } from '@/components/ui/toast'
import { useAsync } from '@/hooks/useAsync'
import { useIdentity } from '@/hooks/useIdentity'
import { inputHint, inputFormatLabel } from '@/lib/runner-format'
import { validateJsonlInput, validateYamlInput, formatJsonl, minifyJsonl, type InputIssue } from '@/lib/input-validate'
import { cn } from '@/lib/utils'

/**
 * 服务来源两形态（deploy/clean 解耦裁决，XOR 由提交校验把关）：
 *  - deploy：**runner 引用**（与 runners[] 同构）——从 GET /runners?role=deploy 拉取列表选择
 *    （genet-deploy / mock-deploy / existing-deploy / 未来新增的 deploy runner，平台不感知具体
 *    名字），输入框按该 runner 的 input_format 给对应编辑器；clean 是否注入由该 runner 的
 *    runner.yaml（clean_runner 声明）决定，提交体不出现 clean。
 *  - service：不部署·直接注入（service 块静态三元组 = 原"外接"模式，作为静态导出注入全部 runner）。
 */
type SourceMode = 'deploy' | 'service'
type PrecheckMode = 'auto' | 'off'

/** runner 列表的一项草稿：**只有一个输入框**（placeholder 来自 runner.input_format 对应片段，D-67）。 */
interface RunnerDraft {
  key: string
  name: string
  runner: string
  input: string
}

interface KeyValueRow {
  key: string
  value: string
}

/** `deploy.input` 是 deploy runner 的输入原文（genet-deploy 为 Genet 部署描述，平台不解析，§10.1）—— 只给示例，不预填语义。 */
const DEFAULT_DEPLOY_INPUT = `model: Qwen3-32B
replicas: 1
resources:
  gpu: 8`

let draftKey = 0
function newKey(): string {
  draftKey += 1
  return `r${draftKey}`
}

/** D-58：name 类字段上限按 **rune**（中文名 200 个字算 200，不是 600）。 */
const NAME_MAX_RUNES = 200

function runeLength(value: string): number {
  return [...value].length
}

/** 把一条被删掉的记录塞回**原位置**（撤销用）。 */
function insertAt<T>(list: readonly T[], index: number, item: T): T[] {
  const next = [...list]
  next.splice(Math.min(Math.max(index, 0), next.length), 0, item)
  return next
}

/**
 * 把 YAML 文本解析成 deploy runner 的输入 —— **只做语法层**校验：
 * 平台不解析部署描述的语义（§10.1），前端也只保证"它是一段合法 YAML 映射"。
 */
function parseDeployInput(text: string): { error: string | null; pos: YamlEditorError | null } {
  if (!text.trim()) return { error: null, pos: null } // 空输入交给后端按 runner 契约校验（400）
  try {
    const parsed = parseYaml(text)
    if (parsed == null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return { error: '部署描述必须是一个 YAML 映射（model / replicas / resources…）', pos: { line: 1, col: 1 } }
    }
    return { error: null, pos: null }
  } catch (e) {
    // YAMLParseError 带 linePos[0] = { line, col }：编辑器靠它画红色波浪 + gutter 圆点
    const first = (e as { linePos?: Array<{ line: number; col: number }> }).linePos?.[0]
    return {
      error: `YAML 语法错误：${(e as Error).message}`,
      pos: first ? { line: first.line, col: first.col } : { line: 1, col: 1 },
    }
  }
}

export function SubmitPage() {
  const navigate = useNavigate()
  const toast = useToast()
  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)

  const runners = useAsync(() => api.listRunners(), [])
  const runnerList = runners.data ?? []
  const defaultRunner = runnerList.find((r) => r.role === 'test')?.name ?? ''

  /**
   * 默认值一律留空，只给 placeholder（§11.2：平台不渲染表单、不预填）：
   * 空表单点提交会被**前端**拦下并给字段级错误。
   */
  const [name, setName] = useState('')
  /**
   * deploy runner 选择：null = runner 列表未到（未初始化）；'' = 用户显式选「外接」；
   * 非空 = deploy 模式，提交体是 deploy:{runner, version, input}（runner 引用，与 runners[] 同构）。
   * 列表到达后默认选第一个 input_format=yaml 的 deploy runner（保持"默认部署"入口）。
   */
  const [deployRunnerName, setDeployRunnerName] = useState<string | null>(null)
  const [deployInput, setDeployInput] = useState(DEFAULT_DEPLOY_INPUT)
  const [serviceUrl, setServiceUrl] = useState('')
  const [modelName, setModelName] = useState('')
  const [modelPath, setModelPath] = useState('')
  const [precheck, setPrecheck] = useState<PrecheckMode>('auto')
  /** 执行环境 profile：RunnerSpec.execution.profile 必填；MVP 没有 /profiles 端点，给可编辑默认值 */
  const [profile, setProfile] = useState('k8s-npu')
  /** 默认 0 行：点「+ 环境变量 / + 凭据引用」才出现（§12.4：Task 级填一次，注入全部 runner） */
  const [envRows, setEnvRows] = useState<KeyValueRow[]>([])
  const [secretRows, setSecretRows] = useState<KeyValueRow[]>([])
  const [runnerDrafts, setRunnerDrafts] = useState<RunnerDraft[]>([{ key: newKey(), name: '', runner: '', input: '' }])

  const [issues, setIssues] = useState<ValidationIssue[]>([])
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [dryRunning, setDryRunning] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [dryRunOk, setDryRunOk] = useState<boolean | null>(null)

  /**
   * 右栏说明针对**当前 runner**：点 runner 卡片 / 选 Runner / 添加 runner 都会把它设为当前。
   */
  const [activeKey, setActiveKey] = useState<string | null>(null)
  const activeIndex = Math.max(
    0,
    runnerDrafts.findIndex((draft) => draft.key === (activeKey ?? runnerDrafts[0]?.key)),
  )
  const activeDraft = runnerDrafts[activeIndex]
  const activeRunnerName = activeDraft ? activeDraft.runner || defaultRunner : ''
  const activeRunner = runnerList.find((runner) => runner.name === activeRunnerName)

  /** Runner 详情抽屉（与 `/runners?open=` 同一个组件） */
  const [detailRunner, setDetailRunner] = useState<string | null>(null)

  /** 每个 runner 的输入框 ref（就地报错点击 → 跳到那一行） */
  const inputRefs = useRef<Record<string, HTMLTextAreaElement | null>>({})

  /**
   * 客户端**即时校验**（dry-run 性质，不替代提交时服务端校验，D-67）：
   *  - jsonl：按 Runner 的 parameters 逐行校验类型 + required（+ parameters 里带的 enum 提示），行号级报错；
   *  - yaml：只做语法校验；text：不校验（语义完全归 Runner）。
   */
  const draftIssues = useMemo(() => {
    const map: Record<string, InputIssue[]> = {}
    for (const draft of runnerDrafts) {
      const runner = runnerList.find((r) => r.name === (draft.runner || defaultRunner))
      if (!runner) {
        map[draft.key] = []
        continue
      }
      if (runner.input_format === 'jsonl') map[draft.key] = validateJsonlInput(draft.input, runner.parameters)
      else if (runner.input_format === 'yaml') map[draft.key] = validateYamlInput(draft.input)
      else map[draft.key] = []
    }
    return map
  }, [runnerDrafts, runnerList, defaultRunner])

  /** 就地报错点击：把光标跳到那一行并选中（textarea 的行 → 字符偏移）。 */
  const jumpToLine = (draftKey: string, line: number) => {
    const el = inputRefs.current[draftKey]
    if (!el) return
    const lines = el.value.split('\n')
    let offset = 0
    for (let i = 0; i < Math.min(line - 1, lines.length); i += 1) offset += (lines[i]?.length ?? 0) + 1
    const target = lines[Math.min(line - 1, lines.length - 1)] ?? ''
    el.focus()
    el.setSelectionRange(offset, offset + target.length)
    el.scrollIntoView({ block: 'center', behavior: 'smooth' })
  }

  /** 「校验」按钮：滚动/聚焦第一条问题；无问题给一条成功 toast。 */
  const validateDraftInput = (draft: RunnerDraft, issues: InputIssue[]) => {
    if (issues.length === 0) {
      toast({
        title: '输入校验通过',
        description: '与服务端 D-67 同规则（类型 + required + enum 提示）；最终以提交时服务端校验为准',
        tone: 'ok',
      })
      return
    }
    const first = issues[0]!
    jumpToLine(draft.key, first.line)
    toast({
      title: `发现 ${issues.length} 条输入问题`,
      description: `第 ${first.line} 行${first.field ? ` · ${first.field}` : ''}：${first.message}`,
      tone: 'err',
    })
  }

  /** 「填示例」：填入 runner.placeholder；已写的内容用撤销 toast 兜底（不静默丢失）。 */
  const fillExample = (draft: RunnerDraft, placeholder: string) => {
    const previous = draft.input
    setRunnerDraft(draft.key, { input: placeholder })
    toast({
      title: '已填入示例（placeholder 来自 runner.yaml）',
      tone: 'ok',
      durationMs: 7000,
      action: previous.trim() ? { label: '撤销', onClick: () => setRunnerDraft(draft.key, { input: previous }) } : undefined,
    })
  }

  /** JSONL 逐行格式化 / 压缩（保持一条记录一行；解析失败的行原样保留并提示）。 */
  const rewriteJsonl = (draft: RunnerDraft, mode: 'format' | 'minify') => {
    const result = mode === 'format' ? formatJsonl(draft.input) : minifyJsonl(draft.input)
    setRunnerDraft(draft.key, { input: result.text })
    toast({
      title: mode === 'format' ? '已逐行格式化' : '已逐行压缩',
      description: result.keptInvalid > 0 ? `${result.keptInvalid} 行不是合法 JSON，已原样保留` : '全部行重排成功',
      tone: result.keptInvalid > 0 ? 'warn' : 'ok',
    })
  }

  const runnerOptions = runnerList.filter((r) => r.role === 'test')
  const precheckRunner = runnerList.find((r) => r.role === 'pre_check')
  /** deploy 角色的 runner 目录（服务来源卡的选项；平台不感知具体名字，列表即真相） */
  const deployRunners = runnerList.filter((r) => r.role === 'deploy')
  const deployRunnerObj = deployRunners.find((r) => r.name === deployRunnerName)
  /** yaml 输入（genet-deploy / mock-deploy）→ YAML 编辑器；jsonl（existing-deploy）→ 普通输入框 */
  const deployIsYaml = deployRunnerObj?.input_format !== 'jsonl'

  /** deploy 输入的语法校验只在 yaml 格式的 runner 上做（jsonl 走逐行 schema 校验，D-67） */
  const deployParse = useMemo(
    () => (deployIsYaml ? parseDeployInput(deployInput) : { error: null as string | null, pos: null as YamlEditorError | null }),
    [deployIsYaml, deployInput],
  )
  /** jsonl 格式的 deploy runner（existing-deploy）逐行校验（与 runner 输入框同规则，D-67） */
  const deployJsonlIssues = useMemo(
    () => (deployIsYaml ? [] : validateJsonlInput(deployInput, deployRunnerObj?.parameters ?? [])),
    [deployIsYaml, deployInput, deployRunnerObj],
  )

  /** 卡片形态：deployRunnerName 非空 = 部署；''/null = 外接（列表未到或用户显式选外接） */
  const mode: SourceMode = deployRunnerName ? 'deploy' : 'service'
  /** 记住最近一次选中的 deploy runner：切去外接再切回来时恢复 */
  const lastDeployPick = useRef('')
  const setMode = (next: SourceMode) => {
    if (next === 'deploy') {
      const candidate =
        deployRunners.find((r) => r.name === lastDeployPick.current) ??
        deployRunners.find((r) => r.input_format === 'yaml') ??
        deployRunners[0]
      setDeployRunnerName(candidate?.name ?? '')
    } else {
      if (deployRunnerName) lastDeployPick.current = deployRunnerName
      setDeployRunnerName('')
    }
  }

  // 列表到达后的默认选择：第一个 yaml 格式的 deploy runner（genet-deploy 在前）；没有 deploy runner 时回外接。
  useEffect(() => {
    if (deployRunnerName !== null) return
    if (runners.loading) return
    const firstYaml = deployRunners.find((r) => r.input_format === 'yaml')
    setDeployRunnerName(firstYaml?.name ?? '')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runners.loading, runnerList])

  /**
   * 编辑器标红/滚行用**防抖后**的位置：边打字边滚会把光标甩走。
   * 校验结论（是否可提交）仍用即时的 `deployParse`，所以「停手 250ms」后标注与结论一致。
   */
  const [lintPos, setLintPos] = useState<YamlEditorError | null>(null)
  useEffect(() => {
    const timer = window.setTimeout(() => setLintPos(deployParse.pos), deployParse.pos ? 250 : 0)
    return () => window.clearTimeout(timer)
  }, [deployParse])

  const issueFor = (path: string): string | undefined => issues.find((issue) => issue.path === path)?.message

  /**
   * A4 / B10：两种「点下去也一定失败」的情况，直接在本页拦住（不靠后端 400）：
   *  - 0 个 runner（D-57：至少 1 条；注入的 deploy / precheck / clean 不计入）；
   *  - 目录里没有 role=test 的 Runner（deploy / clean 角色不可在 runners[] 引用，D-70）。
   * 目录还在加载时**不算**「没有」：`loading` 期间不置灰，避免把加载中误判成空目录。
   */
  const noRunners = runnerDrafts.length === 0
  const noRunnersRegistered = !runners.loading && runnerOptions.length === 0

  /** C8：按 rune 计数（中文名 200 个字算 200，不是 600） */
  const nameRunes = runeLength(name)
  const nameTooLong = nameRunes > NAME_MAX_RUNES
  const nameError = nameTooLong ? `name 超过 ${NAME_MAX_RUNES} 字符（当前 ${nameRunes}）` : issueFor('task.name')

  /**
   * `task.runners` 这类「整个 runner 列表」级的 issue：就地显示在 runner 区，
   * 而不是只出现在顶部摘要（后端 400 的 path 就是 `task.runners`）。
   */
  const runnerAreaIssues = issues.filter((issue) => /(^|\.)runners$/.test(issue.path))

  const submitBlockers: string[] = []
  if (blocked) submitBlockers.push(blocked)
  if (noRunners) submitBlockers.push('至少需要 1 个 runner（deploy / precheck / clean 由平台注入，不计入）')
  if (noRunnersRegistered) submitBlockers.push('当前没有可用的 Runner；deploy / clean 角色不能在 runner 列表里引用')
  if (nameTooLong) submitBlockers.push(`Task 名称超过 ${NAME_MAX_RUNES} 字符（当前 ${nameRunes}）`)
  const submitDisabled = submitBlockers.length > 0
  const submitTitle = submitDisabled ? submitBlockers.join('；') : undefined

  /**
   * 字段级错误：把 `task.name` / `task.runners[0].execution.profile` 这类 path 映射到真实输入框，
   * 用于「摘要里点定位 → 滚过去并聚焦」。
   */
  const fieldRefs = useRef<Record<string, HTMLElement | null>>({})
  const registerField = (path: string) => (el: HTMLElement | null) => {
    fieldRefs.current[path] = el
  }
  const focusField = (path: string) => {
    const el =
      fieldRefs.current[path] ??
      // 退一步：按前缀找（例如 `task.runners[0].runner` 找不到就落回该 runner 那块）
      Object.entries(fieldRefs.current).find(([key]) => key.startsWith(path.split('.').slice(0, 2).join('.')))?.[1]
    if (!el) return
    el.scrollIntoView({ block: 'center', behavior: 'smooth' })
    window.setTimeout(() => el.focus({ preventScroll: true }), 150)
  }

  const setRunnerDraft = (key: string, patch: Partial<RunnerDraft>) => {
    setRunnerDrafts((prev) => prev.map((draft) => (draft.key === key ? { ...draft, ...patch } : draft)))
  }

  /** 复制/点参数 schema：一律追加到「当前 runner」的输入框，已有内容时换行接上（不覆盖）。 */
  const appendToInput = (draftKey: string | undefined, snippet: string) => {
    if (!draftKey) return
    setRunnerDrafts((prev) =>
      prev.map((draft) => {
        if (draft.key !== draftKey) return draft
        const base = draft.input.trimEnd()
        const joined = base
          ? `${base}\n${snippet}`
          : snippet
        return { ...draft, input: joined }
      }),
    )
  }

  const addRunner = () => {
    const key = newKey()
    setRunnerDrafts((prev) => [...prev, { key, name: '', runner: defaultRunner, input: '' }])
    setActiveKey(key)
  }

  /**
   * C5：破坏性动作不弹二次确认，删掉后给一个 **undo toast**（7s）。
   * 最后一个 runner 被删时 A4 的置灰立刻生效，撤销回来后按钮也随之恢复。
   */
  const removeKeyValueRow = (kind: 'env' | 'secret', index: number) => {
    const rows = kind === 'env' ? envRows : secretRows
    const removed = rows[index]
    if (!removed) return
    if (kind === 'env') setEnvRows((prev) => prev.filter((_, i) => i !== index))
    else setSecretRows((prev) => prev.filter((_, i) => i !== index))
    const label = removed.key.trim() || (kind === 'env' ? '环境变量' : '凭据引用')
    toast({
      title: `已删除 ${label}`,
      tone: 'warn',
      durationMs: 7000,
      action: {
        label: '撤销',
        onClick: () => {
          if (kind === 'env') setEnvRows((prev) => insertAt(prev, index, removed))
          else setSecretRows((prev) => insertAt(prev, index, removed))
        },
      },
    })
  }

  const removeRunner = (index: number) => {
    const removed = runnerDrafts[index]
    if (!removed) return
    setRunnerDrafts((prev) => prev.filter((item) => item.key !== removed.key))
    toast({
      title: `已删除 runner ${removed.name.trim() || `#${index + 1}`}`,
      tone: 'warn',
      durationMs: 7000,
      action: {
        label: '撤销',
        onClick: () =>
          setRunnerDrafts((prev) =>
            prev.some((item) => item.key === removed.key) ? prev : insertAt(prev, index, removed),
          ),
      },
    })
  }

  const buildManifest = (): TaskManifest | null => {
    if (deployIsYaml && deployParse.error) return null
    const env: Record<string, string> = {}
    for (const row of envRows) {
      if (row.key.trim()) env[row.key.trim()] = row.value
    }
    const secrets: Record<string, string> = {}
    for (const row of secretRows) {
      if (row.key.trim() && row.value.trim()) secrets[row.key.trim()] = row.value.trim()
    }
    // 三元组：外接（service 静态注入）用（url 必填，model_name / model_path 给了才带）
    const serviceTriple = () => ({
      url: serviceUrl.trim(),
      ...(modelName.trim() ? { model_name: modelName.trim() } : {}),
      ...(modelPath.trim() ? { model_path: modelPath.trim() } : {}),
    })

    return {
      task: {
        name: name.trim(),
        // 服务来源的提交体映射（deploy XOR service 由 assemble 校验，§10.1）：
        //   选了 deploy runner → deploy:{runner, version, input}（runner 引用，与 runners[] 同构；
        //     clean 是否注入由该 runner 的 clean_runner 声明决定，提交体不出现 clean）；
        //   外接 → service:{...}（顶层 service = "序号 0 的导出"，静态注入）。
        // 切换形态时各自草稿保留，只提交当前形态。
        ...(deployRunnerName
          ? {
              deploy: {
                runner: deployRunnerName,
                version: deployRunnerObj?.version ?? '',
                ...(deployInput.trim() ? { input: deployInput } : {}),
              },
            }
          : { service: serviceTriple() }),
        precheck,
        env,
        secrets,
        runners: runnerDrafts.map((draft) => {
          const runnerName = draft.runner || defaultRunner
          const runner = runnerList.find((r) => r.name === runnerName)
          return {
            name: draft.name.trim(),
            runner: runnerName,
            version: runner?.version ?? '',
            execution: { profile: profile.trim() || 'k8s-npu' },
            // 输入原文：按 Runner 的 input_format 原样落档（平台校验后注入 AG_PARAMS_FILE，D-66）
            ...(draft.input.trim() ? { input: draft.input } : {}),
          }
        }),
      },
    }
  }

  const runDryRun = async (manifest: TaskManifest): Promise<boolean> => {
    const result = await api.dryRun(manifest)
    setIssues(result.issues ?? [])
    setDryRunOk(result.valid)
    // 校验结果不走 toast：问题清单进表单内摘要（用户要看到"是哪 N 个问题"）
    return result.valid
  }

  /** 前端校验：同样产出 path 化的 issues，与后端 400 共用一套渲染 */
  const frontendIssues = (): ValidationIssue[] => {
    const found: ValidationIssue[] = []
    if (deployRunnerName) {
      // deploy 模式：runner 引用 + 输入校验（issue.path 与后端一致：task.deploy.runner / task.deploy.input）
      if (!deployRunnerObj) {
        found.push({
          path: 'task.deploy.runner',
          code: 'REQUIRED',
          message: '必须选择 deploy runner',
        })
      } else {
        if (deployIsYaml && deployParse.error) {
          found.push({ path: 'task.deploy.input', code: 'INVALID_YAML', message: deployParse.error })
        }
        const firstJsonlIssue = deployJsonlIssues[0]
        if (firstJsonlIssue) {
          found.push({
            path: 'task.deploy.input',
            code: 'INPUT_INVALID',
            message: `第 ${firstJsonlIssue.line} 行${firstJsonlIssue.field ? ` · ${firstJsonlIssue.field}` : ''}：${firstJsonlIssue.message}`,
          })
        }
      }
    } else if (!serviceUrl.trim()) {
      // 外接模式：service 三元组（后端按同 path 回错）
      found.push({ path: 'task.service.url', code: 'REQUIRED', message: '服务 base URL 必填' })
    }
    if (!name.trim()) found.push({ path: 'task.name', code: 'REQUIRED', message: 'name 必填' })
    // D-58：上限 200 rune（不是字节、不是 UTF-16 单元）
    if (runeLength(name.trim()) > NAME_MAX_RUNES) {
      found.push({
        path: 'task.name',
        code: 'TOO_LONG',
        message: `name 超过 ${NAME_MAX_RUNES} 字符（当前 ${runeLength(name.trim())}）`,
      })
    }
    if (!profile.trim()) {
      found.push({
        path: 'task.runners[0].execution.profile',
        code: 'REQUIRED',
        message: 'execution.profile 必填',
      })
    }
    // D-57：与后端同一句口径（path=task.runners 会被映射到 runner 区就地显示）
    if (runnerDrafts.length === 0) {
      found.push({
        path: 'task.runners',
        code: 'REQUIRED',
        message: '至少需要 1 个 runner（deploy / precheck / clean 由平台注入，不计入）',
      })
    }
    runnerDrafts.forEach((draft, index) => {
      if (!draft.name.trim())
        found.push({ path: `task.runners[${index}].name`, code: 'REQUIRED', message: 'runner 名称必填' })
      if (!draft.runner && !defaultRunner) {
        found.push({ path: `task.runners[${index}].runner`, code: 'REQUIRED', message: '必须选择 Runner' })
      }
    })
    return found
  }

  const onDryRun = async () => {
    const manifest = buildManifest()
    setError(null)
    const local = frontendIssues()
    if (local.length > 0) {
      setIssues(local)
      setDryRunOk(false)
      return
    }
    if (!manifest) {
      setDryRunOk(false)
      return
    }
    setDryRunning(true)
    try {
      await runDryRun(manifest)
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setDryRunning(false)
    }
  }

  const onSubmit = async () => {
    const manifest = buildManifest()
    setError(null)
    setIssues([])
    const local = frontendIssues()
    if (local.length > 0) {
      setIssues(local)
      setDryRunOk(false)
      return
    }
    if (!manifest) {
      setDryRunOk(false)
      return
    }
    setSubmitting(true)
    try {
      // 提交前先 dry-run：有错就停在表单上，不弹一堆 toast
      const valid = await runDryRun(manifest)
      if (!valid) {
        setSubmitting(false)
        return
      }
      const result = await api.submitTask(manifest)
      /**
       * B8：提交成功**只弹一个** toast（校验通过与已受理不同时出现）。
       * 里面既有结果也有下一步动作：复制 ID / 跳详情。
       */
      const taskId = result.task.id
      toast({
        title: `已受理 ${taskId}`,
        description: (
          <span className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              data-testid="toast-copy-task-id"
              className="inline-flex h-6 items-center rounded-sm bg-white/15 px-2 text-2xs text-white hover:bg-white/25"
              onClick={() => void navigator.clipboard?.writeText(taskId)}
            >
              复制 ID
            </button>
            <button
              type="button"
              data-testid="toast-open-task"
              className="inline-flex h-6 items-center rounded-sm bg-white/15 px-2 text-2xs text-white hover:bg-white/25"
              onClick={() => navigate(`/tasks/${taskId}`)}
            >
              查看详情
            </button>
          </span>
        ),
        tone: 'ok',
        // ≥8000ms：两个动作都要来得及点；可手动关闭（dismissible 默认 true）
        durationMs: 9000,
      })
      navigate(`/tasks/${taskId}`)
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      {/* 提交/校验失败：表单内摘要（不自动消失、自己滚进视口并聚焦、trace_id 可复制） */}
      <FormErrorSummary error={error} issues={issues} onJumpTo={focusField} />

      {!error && issues.length === 0 && dryRunOk === true ? <Banner tone="info" title="dry-run 通过" /> : null}

      <Card>
        <CardHeader tone="ok" title="Task" />
        <CardBody>
          {/*
            C8：maxlength=200（后端 D-58，按 rune 上限）+ 实时计数。
            粘贴可以绕过 maxlength，所以超限时这里也会立刻报红并把「提交」置灰（不等后端 400）。
          */}
          <Field
            label="Task 名称"
            required
            htmlFor="task-name"
            hint="同名不合并：每次提交都创建新 Task（不做幂等，D-37）。"
            error={nameError}
          >
            <div id="task-name-row" className="flex items-center gap-2">
              <Input
                id="task-name"
                ref={registerField('task.name')}
                value={name}
                maxLength={NAME_MAX_RUNES}
                onChange={(e) => setName(e.target.value)}
                placeholder="如 qwen3-32b-daily"
                aria-invalid={Boolean(nameError)}
                className="flex-1"
              />
              <span
                data-testid="task-name-counter"
                aria-live="polite"
                className={cn(
                  'shrink-0 font-mono text-2xs tabular-nums',
                  nameTooLong ? 'font-medium text-danger-text' : 'text-fg-3',
                )}
              >
                已输入 {nameRunes} / {NAME_MAX_RUNES}
              </span>
            </div>
          </Field>
        </CardBody>
      </Card>

      {/* 服务来源：一张卡两形态 XOR（D-27）：都填或都不填提交被拒；切换时保留各自草稿不清空 */}
      <Card>
        <CardHeader
          tone="muted"
          title="服务来源"
          sub="一张卡两形态：部署（选一个 deploy runner 并给输入）XOR 外接（直接给已有服务）；clean 是否注入由 deploy runner 的 clean_runner 声明决定"
          actions={
            <Segmented<SourceMode>
              ariaLabel="服务来源"
              value={mode}
              onChange={setMode}
              options={[
                { value: 'deploy', label: '部署' },
                { value: 'service', label: '外接' },
              ]}
            />
          }
        />
        <CardBody className="flex flex-col gap-2">
          {mode === 'deploy' ? (
            <>
              {/* deploy runner 选择器：列表来自 GET /runners?role=deploy（平台不感知具体名字） */}
              <Field
                label="deploy runner"
                required
                htmlFor="deploy-runner-select"
                hint="role=deploy 的 Runner 引用（与 runner 列表同构）；版本自动取目录里的当前版"
                error={issueFor('task.deploy.runner')}
              >
                <div className="flex items-center gap-2">
                  <Select
                    id="deploy-runner-select"
                    aria-label="deploy runner"
                    data-testid="deploy-runner"
                    value={deployRunnerName ?? ''}
                    onChange={(e) => {
                      lastDeployPick.current = e.target.value
                      setDeployRunnerName(e.target.value)
                    }}
                    className="h-8 w-64 text-xs"
                  >
                    {deployRunners.length === 0 ? <option value="">（目录里没有 role=deploy 的 Runner）</option> : null}
                    {deployRunners.map((runner) => (
                      <option key={runner.name} value={runner.name}>
                        {runner.name} v{runner.version}
                      </option>
                    ))}
                  </Select>
                  {deployRunnerObj ? (
                    <span className="flex items-center gap-1">
                      <Tag mono>{deployRunnerObj.input_format}</Tag>
                      {deployRunnerObj.clean_runner ? <Tag>clean: {deployRunnerObj.clean_runner}</Tag> : <Tag>无 clean 阶段</Tag>}
                    </span>
                  ) : null}
                </div>
              </Field>
              {deployIsYaml ? (
                <>
                  {/* CodeMirror 6：真语法高亮 + 行号 + 横向滚动；语法错误按行列标行 */}
                  <YamlEditor
                    value={deployInput}
                    onChange={setDeployInput}
                    rows={7}
                    aria-label="Genet 部署描述 YAML"
                    title="deploy.input（该 runner 的输入原文，genet-deploy 为部署描述）"
                    hint="2 空格缩进 · 平台不解析语义"
                    error={lintPos}
                  />
                  <p
                    data-testid="deploy-yaml-error"
                    data-state={deployParse.error ? 'error' : 'ok'}
                    role="status"
                    aria-live="polite"
                    className={cn('text-2xs', deployParse.error ? 'text-danger-text' : 'text-success-text')}
                  >
                    {deployParse.error
                      ? `${deployParse.pos ? `第 ${deployParse.pos.line} 行第 ${deployParse.pos.col} 列：` : ''}${deployParse.error}`
                      : '✓ YAML 语法有效（语义归 deploy runner，平台不解析）'}
                  </p>
                </>
              ) : (
                <>
                  <Field
                    label="deploy 输入（JSONL）"
                    hint={inputHint(deployRunnerObj?.input_format ?? 'jsonl')}
                    error={issueFor('task.deploy.input')}
                  >
                    <Textarea
                      value={deployInput}
                      rows={3}
                      spellCheck={false}
                      aria-label="deploy runner 输入"
                      aria-invalid={(deployJsonlIssues.length ?? 0) > 0}
                      placeholder={deployRunnerObj?.placeholder ?? '{"url":"http://10.0.0.10:8000/v1","model_name":"Qwen3-32B"}'}
                      className="font-mono text-xs"
                      onChange={(e) => setDeployInput(e.target.value)}
                    />
                  </Field>
                  {deployJsonlIssues.length > 0 ? (
                    <ul
                      data-testid="deploy-input-issues"
                      role="alert"
                      className="flex flex-col gap-0.5 rounded-md border border-danger/25 bg-danger-weak px-2.5 py-1.5"
                    >
                      {deployJsonlIssues.slice(0, 8).map((issue, issueIndex) => (
                        <li key={`${issue.line}-${issue.field ?? issue.kind}-${issueIndex}`}>
                          <span className="text-2xs leading-[16px] text-danger-text">
                            第 {issue.line} 行
                            {issue.field ? <code className="mx-1 font-mono">{issue.field}</code> : null}· {issue.message}
                          </span>
                        </li>
                      ))}
                    </ul>
                  ) : null}
                </>
              )}
              {deployRunnerObj?.name === 'mock-deploy' ? (
                <p
                  data-testid="deploy-mock-hint"
                  className="rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs leading-[16px] text-warn-text"
                >
                  联调环境：mock-deploy 与 genet-deploy 是同一份脚本，接入点由它的 runner.yaml env default 指向
                  fake-genet；需要别的接入点时在下方「环境变量」里给 AG_GENET_BASE_URL。
                </p>
              ) : null}
              <p className="text-2xs leading-[16px] text-fg-3">
                这段原文会作为 {deployRunnerObj?.name ?? 'deploy runner'} 的输入（AG_PARAMS_FILE）；namespace 由 Genet API Key 绑定，
                请求体不能指定（D-44/D-52）。部署被阻塞会收口为 service_unavailable（D-47）。
              </p>
            </>
          ) : (
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label="服务 base URL" required error={issueFor('task.service.url')} className="sm:col-span-2">
                <Input
                  ref={registerField('task.service.url')}
                  value={serviceUrl}
                  onChange={(e) => setServiceUrl(e.target.value)}
                  placeholder="http://10.0.0.10:8000/v1"
                  aria-invalid={Boolean(issueFor('task.service.url'))}
                />
              </Field>
              <Field label="模型名">
                <Input value={modelName} onChange={(e) => setModelName(e.target.value)} placeholder="Qwen3-32B" />
              </Field>
              <Field label="模型路径（可选）" className="sm:col-span-3">
                <Input value={modelPath} onChange={(e) => setModelPath(e.target.value)} placeholder="/models/Qwen3-32B" />
              </Field>
              <p className="text-2xs leading-[16px] text-fg-3 sm:col-span-3">
                这三个字段就是平台要注入的公共参数来源（AG_SERVED_URL / AG_SERVED_MODEL_NAME / AG_MODEL_PATH）：
                不部署、不拉服务，由你来填。
              </p>
            </div>
          )}
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          tone="warn"
          title="环境变量与凭据"
          sub="Task 粒度，注入该 Task 下全部 runner（§12.4）"
          actions={
            <>
              <Button
                size="sm"
                data-testid="add-env"
                icon={<Plus className="size-3" aria-hidden />}
                onClick={() => setEnvRows((p) => [...p, { key: '', value: '' }])}
              >
                环境变量
              </Button>
              <Button
                size="sm"
                data-testid="add-secret"
                icon={<Plus className="size-3" aria-hidden />}
                onClick={() => setSecretRows((p) => [...p, { key: '', value: '' }])}
              >
                凭据引用
              </Button>
            </>
          }
        />
        <CardBody className="flex flex-col gap-2">
          {/* 默认 0 行：不预填示例（那是要提交的内容，不是装饰） */}
          {envRows.length === 0 && secretRows.length === 0 ? (
            <p data-testid="env-empty-hint" className="text-2xs text-fg-3">
              还没有环境变量或凭据引用。需要时点右上角「+ 环境变量 / + 凭据引用」再加一行。
            </p>
          ) : null}
          {envRows.map((row, index) => (
            <div key={`env-${index}`} className="flex items-center gap-2">
              <Input
                value={row.key}
                aria-label={`env 键 ${index + 1}`}
                placeholder="LOG_LEVEL"
                className="w-48"
                onChange={(e) =>
                  setEnvRows((prev) => prev.map((item, i) => (i === index ? { ...item, key: e.target.value } : item)))
                }
              />
              <Input
                value={row.value}
                aria-label={`env 值 ${index + 1}`}
                placeholder="info"
                onChange={(e) =>
                  setEnvRows((prev) => prev.map((item, i) => (i === index ? { ...item, value: e.target.value } : item)))
                }
              />
              <Button size="sm" variant="ghost" aria-label="删除该环境变量" onClick={() => removeKeyValueRow('env', index)}>
                <Trash2 className="size-3.5" aria-hidden />
              </Button>
            </div>
          ))}
          {secretRows.map((row, index) => (
            <div key={`secret-${index}`} className="flex items-center gap-2">
              <Input
                value={row.key}
                aria-label={`secret 键 ${index + 1}`}
                placeholder="ENDPOINT_API_KEY"
                className="w-48"
                onChange={(e) =>
                  setSecretRows((prev) => prev.map((item, i) => (i === index ? { ...item, key: e.target.value } : item)))
                }
              />
              <Input
                value={row.value}
                aria-label={`secret 引用 ${index + 1}`}
                placeholder="endpoint/prod-qwen（只填引用名，不填明文）"
                onChange={(e) =>
                  setSecretRows((prev) => prev.map((item, i) => (i === index ? { ...item, value: e.target.value } : item)))
                }
              />
              <Button size="sm" variant="ghost" aria-label="删除该凭据引用" onClick={() => removeKeyValueRow('secret', index)}>
                <Trash2 className="size-3.5" aria-hidden />
              </Button>
            </div>
          ))}
          <p className="text-2xs text-fg-3">凭据只填引用，明文不会出现在 Manifest 里（C-10）；env 明文可回显可编辑（不脱敏、不折叠）。</p>
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          tone="primary"
          title="runner 列表"
          sub="每个 runner 一个输入框（placeholder 按 input_format 给对应格式的片段，D-67）；严格串行执行"
          actions={
            <>
              <span className="text-2xs text-fg-3">执行环境</span>
              <Input
                ref={registerField('task.runners[0].execution.profile')}
                value={profile}
                onChange={(e) => setProfile(e.target.value)}
                aria-label="执行环境 profile"
                aria-invalid={Boolean(issueFor('task.runners[0].execution.profile'))}
                title={issueFor('task.runners[0].execution.profile') ?? undefined}
                placeholder="k8s-npu"
                className="h-8 w-32 font-mono text-2xs"
              />
              <span className="text-2xs text-fg-3">precheck</span>
              <Segmented<PrecheckMode>
                ariaLabel="precheck 模式"
                value={precheck}
                onChange={setPrecheck}
                options={[
                  { value: 'auto', label: 'auto 注入' },
                  { value: 'off', label: 'off' },
                ]}
              />
              <Button size="sm" data-testid="add-runner" icon={<Plus className="size-3" aria-hidden />} onClick={addRunner}>
                添加 runner
              </Button>
            </>
          }
        />
        <CardBody className="flex flex-col gap-3">
          {/* A4：0 个 runner 时说清「为什么不能提交」，就在会被删空的那块下面 */}
          {noRunners ? (
            <p
              data-testid="runner-empty-hint"
              className="rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs leading-[16px] text-warn-text"
            >
              至少需要 1 个 runner（deploy / precheck / clean 由平台注入，不计入）
            </p>
          ) : null}

          {/* B10：只有内置角色时，下拉里只剩一句占位，提交/校验都不该放开 */}
          {noRunnersRegistered ? (
            <p
              data-testid="runner-empty-registered"
              className="rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs leading-[16px] text-warn-text"
            >
              当前没有可用的 Runner；deploy / clean 角色仅内置（D-70），不能作为测试 runner
            </p>
          ) : null}

          {/* 后端仍可能返回 `task.runners` 级的 issue（并发/边界）：就地显示在 runner 区 */}
          {runnerAreaIssues.length > 0 ? (
            <div
              data-testid="runner-area-issues"
              role="alert"
              className="rounded-md border border-danger/30 bg-danger-weak px-3 py-2"
            >
              {runnerAreaIssues.map((issue, index) => (
                <p key={`${issue.path}-${index}`} className="text-2xs leading-[16px] text-danger-text">
                  <code className="mr-1.5 font-mono">{issue.path}</code>
                  {issue.message}
                </p>
              ))}
            </div>
          ) : null}

          {precheck === 'auto' ? (
            <div className="rounded-md border border-line-soft bg-surface-2 px-3 py-2 text-2xs leading-[16px] text-fg-2">
              <span className="font-medium text-fg">auto：</span>
              平台取注册表中唯一一个 <code className="font-mono">role=pre_check</code> 的 Runner，注入为第一个测试类
              runner；0 个或多个都拒绝提交。
              {precheckRunner ? (
                <Tag mono className="ml-2">
                  {precheckRunner.name} · {precheckRunner.version}
                </Tag>
              ) : null}
            </div>
          ) : null}

          {runnerDrafts.map((draft, index) => {
            const resolvedRunner = runnerList.find((runner) => runner.name === (draft.runner || defaultRunner))
            const nameIssue = issueFor(`task.runners[${index}].name`)
            return (
              <div
                key={draft.key}
                data-testid={`runner-row-${index + 1}`}
                className={cn(
                  'rounded-md border p-3 transition-colors duration-150',
                  activeDraft?.key === draft.key ? 'border-primary/40 bg-primary-weak/30' : 'border-line-soft',
                )}
                onClick={() => setActiveKey(draft.key)}
                onFocusCapture={() => setActiveKey(draft.key)}
              >
                <div className="flex flex-wrap items-center gap-2">
                  <span className="grid size-5 shrink-0 place-items-center rounded-full bg-surface-3 text-2xs text-fg-2">
                    {index + 1}
                  </span>
                  <Input
                    value={draft.name}
                    aria-label={`runner ${index + 1} 名称`}
                    aria-invalid={Boolean(nameIssue)}
                    title={nameIssue ?? undefined}
                    placeholder="如 accuracy-dataset-a"
                    className="h-8 w-56"
                    onChange={(e) => setRunnerDraft(draft.key, { name: e.target.value })}
                  />
                  <Select
                    value={draft.runner || defaultRunner}
                    aria-label={`runner ${index + 1} Runner`}
                    className="h-8 w-48"
                    onChange={(e) => {
                      setActiveKey(draft.key)
                      setRunnerDraft(draft.key, { runner: e.target.value })
                    }}
                  >
                    {runnerOptions.length === 0 ? <option value="">（还没有 Runner）</option> : null}
                    {runnerOptions.map((runner) => (
                      <option key={runner.name} value={runner.name}>
                        {runner.name} v{runner.version}
                      </option>
                    ))}
                  </Select>
                  {/* 详情：复用 `/runners?open=` 的同一个抽屉组件（不复制第二份） */}
                  <Button
                    size="sm"
                    variant="ghost"
                    data-testid="runner-runner-detail"
                    data-runner-index={index + 1}
                    disabled={!resolvedRunner}
                    title={resolvedRunner ? `查看 ${resolvedRunner.name} 的参数与镜像` : '还没有可选的 Runner'}
                    aria-label={`查看 runner ${index + 1} 的 Runner 详情`}
                    onClick={() => {
                      if (!resolvedRunner) return
                      setActiveKey(draft.key)
                      setDetailRunner(resolvedRunner.name)
                    }}
                  >
                    <Info className="size-3.5" aria-hidden />
                    详情
                  </Button>
                  {resolvedRunner ? (
                    <span className="flex items-center gap-1">
                      <Tag mono>{resolvedRunner.input_format}</Tag>
                      {resolvedRunner.exclusive ? <Tag>独占</Tag> : null}
                      <Tag>{resolvedRunner.on_failure}</Tag>
                    </span>
                  ) : null}
                  {issues.some((issue) => issue.path.startsWith(`task.runners[${index}]`)) ? (
                    <Badge tone="err">有校验问题</Badge>
                  ) : null}
                  <Button
                    size="sm"
                    variant="ghost"
                    className="ml-auto"
                    aria-label={`移除 runner ${index + 1}`}
                    data-testid={`remove-runner-${index + 1}`}
                    onClick={() => removeRunner(index)}
                  >
                    移除
                  </Button>
                </div>
                {nameIssue ? (
                  <p data-testid={`runner-name-error-${index + 1}`} role="alert" className="mt-1.5 text-2xs text-danger-text">
                    {nameIssue}
                  </p>
                ) : null}
                <div className="mt-2">
                  <div className="flex flex-wrap items-center gap-1.5">
                    <span className="text-2xs text-fg-3">
                      输入{resolvedRunner ? `（${inputFormatLabel(resolvedRunner.input_format)}）` : ''}
                    </span>
                    {/* 就地小按钮组（校验 / 格式化 / 压缩 / 填示例）：点按就地生效、行内报错；仅客户端 dry-run，最终以提交时服务端校验为准 */}
                    <Button
                      size="sm"
                      variant="ghost"
                      data-testid={`input-validate-${index + 1}`}
                      icon={<ListChecks className="size-3" aria-hidden />}
                      onClick={() => validateDraftInput(draft, draftIssues[draft.key] ?? [])}
                    >
                      校验
                    </Button>
                    {resolvedRunner?.input_format === 'jsonl' ? (
                      <>
                        <Button
                          size="sm"
                          variant="ghost"
                          data-testid={`input-format-${index + 1}`}
                          icon={<AlignLeft className="size-3" aria-hidden />}
                          onClick={() => rewriteJsonl(draft, 'format')}
                        >
                          格式化
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          data-testid={`input-minify-${index + 1}`}
                          icon={<Minimize2 className="size-3" aria-hidden />}
                          onClick={() => rewriteJsonl(draft, 'minify')}
                        >
                          压缩
                        </Button>
                      </>
                    ) : null}
                    {resolvedRunner?.placeholder ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        data-testid={`input-fill-example-${index + 1}`}
                        icon={<Wand2 className="size-3" aria-hidden />}
                        onClick={() => fillExample(draft, resolvedRunner.placeholder ?? '')}
                      >
                        填示例
                      </Button>
                    ) : null}
                  </div>
                  <Field
                    className="mt-1"
                    error={issueFor(`task.runners[${index}].input`) ?? issueFor(`task.runners[${index}].runner`)}
                    hint={resolvedRunner ? inputHint(resolvedRunner.input_format) : undefined}
                  >
                    <Textarea
                      ref={(el) => {
                        inputRefs.current[draft.key] = el
                      }}
                      value={draft.input}
                      rows={3}
                      spellCheck={false}
                      aria-label={`runner ${index + 1} 输入`}
                      aria-invalid={(draftIssues[draft.key]?.length ?? 0) > 0}
                      placeholder={resolvedRunner?.placeholder ?? '{"dataset":"/data/gsm8k.jsonl","limit":200}'}
                      className="font-mono text-xs"
                      onChange={(e) => setRunnerDraft(draft.key, { input: e.target.value })}
                    />
                  </Field>
                  {/* 行内报错（行号 + 字段名 + 原因）：点一条跳到那一行；规则与服务端 D-67 一致 */}
                  {(draftIssues[draft.key]?.length ?? 0) > 0 ? (
                    <ul
                      data-testid={`input-issues-${index + 1}`}
                      role="alert"
                      className="mt-1 flex flex-col gap-0.5 rounded-md border border-danger/25 bg-danger-weak px-2.5 py-1.5"
                    >
                      {(draftIssues[draft.key] ?? []).slice(0, 8).map((issue, issueIndex) => (
                        <li key={`${issue.line}-${issue.field ?? issue.kind}-${issueIndex}`}>
                          <button
                            type="button"
                            className="text-left text-2xs leading-[16px] text-danger-text hover:underline"
                            onClick={() => jumpToLine(draft.key, issue.line)}
                          >
                            第 {issue.line} 行
                            {issue.field ? <code className="mx-1 font-mono">{issue.field}</code> : null}· {issue.message}
                          </button>
                        </li>
                      ))}
                      {(draftIssues[draft.key]?.length ?? 0) > 8 ? (
                        <li className="text-2xs text-danger-text">还有 {(draftIssues[draft.key] ?? []).length - 8} 条…</li>
                      ) : null}
                    </ul>
                  ) : null}
                </div>
              </div>
            )
          })}
        </CardBody>
        <CardFooter>
          <span>
            {blocked
              ? `当前角色 ${auth.role ?? 'viewer'} 只读：${blocked}（服务端也会再判一次）`
              : submitDisabled
                ? submitTitle
                : '提交前会先 dry-run；校验通过才真正 POST /tasks（每次提交都创建新 Task，不做幂等 D-37）。'}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button
              size="sm"
              data-testid="validate-task"
              loading={dryRunning}
              disabled={submitDisabled}
              aria-disabled={submitDisabled}
              title={submitTitle}
              onClick={() => void onDryRun()}
            >
              校验
            </Button>
            <Button
              size="sm"
              variant="primary"
              data-testid="submit-task"
              loading={submitting}
              disabled={submitDisabled}
              aria-disabled={submitDisabled}
              title={submitTitle}
              onClick={() => void onSubmit()}
            >
              提交
            </Button>
          </div>
        </CardFooter>
      </Card>

      {/* 与 Runner 目录页是同一个抽屉组件：参数 schema 里也能直接复制进当前 runner 的输入框 */}
      <RunnerDrawer
        name={detailRunner}
        onClose={() => setDetailRunner(null)}
        onCopyParam={(snippet) => appendToInput(activeDraft?.key, snippet)}
      />

      {/* 参数说明：就地搬到主内容，默认折叠（说明性内容，不占首屏） */}
      <details
        data-testid="submit-runner-help"
        className="group rounded-xl border border-line bg-surface shadow-xs"
      >
        <summary className="flex cursor-pointer list-none items-center gap-2 px-5 py-3 text-base font-semibold text-fg [&::-webkit-details-marker]:hidden">
          参数说明
          <span className="text-2xs font-normal text-fg-3">
            {activeRunner ? `${activeRunner.name} v${activeRunner.version}` : '跟着当前 runner 选中的 Runner 走'}
          </span>
          <span className="ml-auto text-2xs font-normal text-fg-3 group-open:hidden">点开</span>
        </summary>
        <div className="flex flex-col gap-3 border-t border-line-soft px-5 py-4">
          <p className="rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-[16px] text-fg-2">
            每个 runner 只有一个「输入」框：jsonl 按 Runner 声明的参数 schema 逐行写 JSON；yaml / text 原样写，
            语义完全归 Runner（D-67）。点参数旁的「复制」会把它追加到当前 runner 的输入末尾，不会覆盖已写的内容。
          </p>

          {!activeDraft ? (
            <p data-testid="rail-no-runner" className="text-2xs text-fg-3">
              先在左侧添加一个 runner，再选 Runner。
            </p>
          ) : !activeRunner ? (
            <p data-testid="rail-no-runner" className="text-2xs text-fg-3">
              先在左侧选一个 Runner
            </p>
          ) : (
            <div data-testid="rail-runner" data-runner={activeRunner.name} className="flex flex-col gap-3">
              <div className="flex flex-wrap items-center gap-1.5">
                <Tag mono>{activeRunner.name}</Tag>
                <span className="text-2xs text-fg-3">
                  v{activeRunner.version} · {activeRunner.role} · {activeRunner.input_format}
                </span>
                {activeRunner.exclusive ? <Tag>独占</Tag> : null}
                <Tag>{activeRunner.on_failure}</Tag>
                <span className="text-2xs text-fg-3">runner {activeIndex + 1}</span>
              </div>

              {activeRunner.placeholder ? (
                <div>
                  <div className="flex flex-wrap items-center gap-2">
                    <p className="text-2xs font-medium text-fg-2">输入示例（placeholder 来自 runner.yaml）</p>
                    <button
                      type="button"
                      data-testid="copy-placeholder"
                      aria-label="复制输入示例到输入框"
                      onClick={() => {
                        appendToInput(activeDraft.key, activeRunner.placeholder ?? '')
                      }}
                      className="inline-flex h-6 items-center gap-1 rounded-sm px-2 text-2xs text-fg-3 transition-colors hover:bg-line-soft hover:text-fg"
                    >
                      加入输入
                    </button>
                  </div>
                  <pre className="mt-1 overflow-x-auto rounded-md border border-line-soft bg-surface-2 px-2 py-1.5 font-mono text-2xs leading-[16px] text-fg-2">
                    {activeRunner.placeholder}
                  </pre>
                </div>
              ) : null}

              <div>
                <p className="mb-1 text-2xs font-medium text-fg-2">参数 schema（{activeRunner.parameters.length}）</p>
                <RunnerParamTable
                  parameters={activeRunner.parameters}
                  copyTestId="copy-param"
                  onCopy={(snippet) => appendToInput(activeDraft.key, snippet)}
                />
              </div>
            </div>
          )}

          <p className="border-t border-line-soft pt-3 text-2xs leading-[16px] text-fg-3">
            不用自己写的公共参数：<code className="font-mono">AG_SERVED_URL</code> /{' '}
            <code className="font-mono">AG_SERVED_MODEL_NAME</code> / <code className="font-mono">AG_MODEL_PATH</code>{' '}
            由平台经环境变量注入（§5.4.1）；运行时的 <code className="font-mono">AG_TASK_ID</code> /{' '}
            <code className="font-mono">AG_ATTEMPT</code> / <code className="font-mono">AG_RUNNER_SEQ</code> /{' '}
            <code className="font-mono">AG_PARAMS_FILE</code> / <code className="font-mono">AG_OUTPUT_DIR</code>{' '}
            也是平台注入，不要写在输入里。
          </p>
        </div>
      </details>
    </>
  )
}
