import { useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Plus, Trash2 } from 'lucide-react'
import { parse as parseYaml } from 'yaml'
import { api, writeBlockedReason } from '@/api'
import { toApiError } from '@/api/client'
import type { ApiErrorBody, DeployManifest, RunManifest, ValidationIssue } from '@/api/types'
import { RightRail, RailCard } from '@/components/app/RightRail'
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
import { cn } from '@/lib/utils'

type SourceMode = 'deploy' | 'service'
type PrecheckMode = 'auto' | 'off'

interface TestDraft {
  key: string
  name: string
  tester: string
  args: string
}

interface KeyValueRow {
  key: string
  value: string
}

const DEFAULT_DEPLOY_YAML = `provider: genet
parameters:
  model: Qwen3-32B
  replicas: 1`

let draftKey = 0
function newKey(): string {
  draftKey += 1
  return `t${draftKey}`
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

interface DeployParse {
  deploy: DeployManifest | null
  error: string | null
  /** YAML 解析失败的位置（`yaml` 包 `LinePos`，1-based）；交给编辑器标行 */
  pos: YamlEditorError | null
}

/** 把 YAML 文本解析成 deploy 段；返回错误信息而不是抛。 */
function parseDeploy(text: string): DeployParse {
  if (!text.trim()) return { deploy: null, error: 'deploy 段不能为空', pos: null }
  let parsed: unknown
  try {
    parsed = parseYaml(text)
  } catch (e) {
    // YAMLParseError 带 linePos[0] = { line, col }：编辑器靠它画红色波浪 + gutter 圆点
    const first = (e as { linePos?: Array<{ line: number; col: number }> }).linePos?.[0]
    return {
      deploy: null,
      error: `YAML 解析失败：${(e as Error).message}`,
      pos: first ? { line: first.line, col: first.col } : null,
    }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { deploy: null, error: 'deploy 段必须是一个对象（含 provider / parameters）', pos: null }
  }
  const rec = parsed as Record<string, unknown>
  const provider = typeof rec['provider'] === 'string' ? rec['provider'] : ''
  if (!provider) return { deploy: null, error: '缺少顶层 provider（平台按 provider 转交 Genet）', pos: null }
  const parameters =
    rec['parameters'] && typeof rec['parameters'] === 'object'
      ? (rec['parameters'] as Record<string, unknown>)
      : {}
  return { deploy: { provider, parameters }, error: null, pos: null }
}

export function SubmitPage() {
  const navigate = useNavigate()
  const toast = useToast()
  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)

  const testers = useAsync(() => api.listTesters(), [])
  const testerList = testers.data ?? []
  const defaultTester = testerList.find((t) => t.role === 'test')?.name ?? ''

  const [name, setName] = useState('qwen3-32b-daily')
  const [mode, setMode] = useState<SourceMode>('deploy')
  const [deployYaml, setDeployYaml] = useState(DEFAULT_DEPLOY_YAML)
  const [serviceUrl, setServiceUrl] = useState('http://10.0.0.10:8000/v1')
  const [modelName, setModelName] = useState('Qwen3-32B')
  const [modelPath, setModelPath] = useState('')
  const [precheck, setPrecheck] = useState<PrecheckMode>('auto')
  /** 执行环境 profile：TestSpec.execution.profile 必填；MVP 没有 /profiles 端点，给可编辑默认值 */
  const [profile, setProfile] = useState('k8s-npu')
  const [envRows, setEnvRows] = useState<KeyValueRow[]>([{ key: 'LOG_LEVEL', value: 'info' }])
  const [secretRows, setSecretRows] = useState<KeyValueRow[]>([{ key: 'ENDPOINT_API_KEY', value: '' }])
  const [tests, setTests] = useState<TestDraft[]>([
    { key: newKey(), name: 'accuracy-dataset-a', tester: '', args: '--dataset=/data/gsm8k.jsonl --limit=200' },
  ])

  const [issues, setIssues] = useState<ValidationIssue[]>([])
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [dryRunning, setDryRunning] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [dryRunOk, setDryRunOk] = useState<boolean | null>(null)

  const deployParse = useMemo(() => parseDeploy(deployYaml), [deployYaml])

  const testerOptions = testerList.filter((t) => t.role === 'test')
  const precheckTester = testerList.find((t) => t.role === 'pre_check')

  const issueFor = (path: string): string | undefined => issues.find((issue) => issue.path === path)?.message

  /**
   * A4 / B10：两种「点下去也一定失败」的情况，直接在本页拦住（不靠后端 400）：
   *  - 0 个测试 Task（D-57：Pre-Check 是平台注入的，不算测试）；
   *  - 目录里没有 role=test 的 Tester（只有 pre_check 的占位）。
   * 目录还在加载时**不算**「没有」：`loading` 期间不置灰，避免把加载中误判成空目录。
   */
  const noTasks = tests.length === 0
  const noTesters = !testers.loading && testerOptions.length === 0

  /** C8：按 rune 计数（中文名 200 个字算 200，不是 600） */
  const nameRunes = runeLength(name)
  const nameTooLong = nameRunes > NAME_MAX_RUNES
  const nameError = nameTooLong ? `name 超过 ${NAME_MAX_RUNES} 字符（当前 ${nameRunes}）` : issueFor('run.name')

  /**
   * `run.tests` / `tests` 这类「整个 Task 列表」级的 issue：就地显示在 Task 区，
   * 而不是只出现在顶部摘要（D-57 后端 400 的 path 就是 `run.tests`）。
   */
  const taskAreaIssues = issues.filter((issue) => /(^|\.)tests$/.test(issue.path))

  const submitBlockers: string[] = []
  if (blocked) submitBlockers.push(blocked)
  if (noTasks) submitBlockers.push('至少需要 1 个测试 Task（Pre-Check 由平台注入，不计入测试）')
  if (noTesters) submitBlockers.push('当前没有可用的 Tester；只有 pre_check 角色的 Tester 不能作为测试 Task')
  if (nameTooLong) submitBlockers.push(`Run 名称超过 ${NAME_MAX_RUNES} 字符（当前 ${nameRunes}）`)
  const submitDisabled = submitBlockers.length > 0
  const submitTitle = submitDisabled ? submitBlockers.join('；') : undefined

  /**
   * 字段级错误：把 `run.name` / `run.tests[0].execution.profile` 这类 path 映射到真实输入框，
   * 用于「摘要里点定位 → 滚过去并聚焦」。
   */
  const fieldRefs = useRef<Record<string, HTMLElement | null>>({})
  const registerField = (path: string) => (el: HTMLElement | null) => {
    fieldRefs.current[path] = el
  }
  const focusField = (path: string) => {
    const el =
      fieldRefs.current[path] ??
      // 退一步：按前缀找（例如 `run.tests[0].tester` 找不到就落回该 Task 那块）
      Object.entries(fieldRefs.current).find(([key]) => key.startsWith(path.split('.').slice(0, 2).join('.')))?.[1]
    if (!el) return
    el.scrollIntoView({ block: 'center', behavior: 'smooth' })
    window.setTimeout(() => el.focus({ preventScroll: true }), 150)
  }

  const setTest = (key: string, patch: Partial<TestDraft>) => {
    setTests((prev) => prev.map((test) => (test.key === key ? { ...test, ...patch } : test)))
  }

  /**
   * C5：破坏性动作不弹二次确认，删掉后给一个 **undo toast**（7s）。
   * 撤销是真的放回**原位置原值**，不是只改文案；最后一个 Task 被删时 A4 的置灰立刻生效，
   * 撤销回来后按钮也随之恢复。
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

  const removeTask = (index: number) => {
    const removed = tests[index]
    if (!removed) return
    setTests((prev) => prev.filter((item) => item.key !== removed.key))
    toast({
      title: `已删除 Task ${removed.name.trim() || `#${index + 1}`}`,
      tone: 'warn',
      durationMs: 7000,
      action: {
        label: '撤销',
        onClick: () =>
          setTests((prev) => (prev.some((item) => item.key === removed.key) ? prev : insertAt(prev, index, removed))),
      },
    })
  }

  const buildManifest = (): RunManifest | null => {
    if (mode === 'deploy' && !deployParse.deploy) return null
    const env: Record<string, string> = {}
    for (const row of envRows) {
      if (row.key.trim()) env[row.key.trim()] = row.value
    }
    const secrets: Record<string, string> = {}
    for (const row of secretRows) {
      if (row.key.trim() && row.value.trim()) secrets[row.key.trim()] = row.value.trim()
    }

    return {
      run: {
        name: name.trim(),
        ...(mode === 'deploy'
          ? { deploy: deployParse.deploy ?? undefined }
          : {
              service: {
                url: serviceUrl.trim(),
                ...(modelName.trim() ? { model_name: modelName.trim() } : {}),
                ...(modelPath.trim() ? { model_path: modelPath.trim() } : {}),
              },
            }),
        precheck,
        env,
        secrets,
        cleanup: { policy: 'always' },
        tests: tests.map((test) => {
          const testerName = test.tester || defaultTester
          const tester = testerList.find((t) => t.name === testerName)
          return {
            name: test.name.trim(),
            tester: testerName,
            version: tester?.version ?? '',
            execution: { profile: profile.trim() || 'k8s-npu' },
            args: test.args,
          }
        }),
      },
    }
  }

  const runDryRun = async (manifest: RunManifest): Promise<boolean> => {
    const result = await api.dryRun(manifest)
    setIssues(result.issues ?? [])
    setDryRunOk(result.valid)
    // 校验结果不走 toast：问题清单进表单内摘要（用户要看到"是哪 N 个问题"）
    return result.valid
  }

  /** 前端校验：同样产出 path 化的 issues，与后端 400 共用一套渲染 */
  const frontendIssues = (): ValidationIssue[] => {
    const found: ValidationIssue[] = []
    if (mode === 'deploy' && deployParse.error) {
      found.push({ path: 'run.deploy', code: 'INVALID_YAML', message: deployParse.error })
    }
    if (!name.trim()) found.push({ path: 'run.name', code: 'REQUIRED', message: 'name 必填' })
    // D-58：上限 200 rune（不是字节、不是 UTF-16 单元）
    if (runeLength(name.trim()) > NAME_MAX_RUNES) {
      found.push({
        path: 'run.name',
        code: 'TOO_LONG',
        message: `name 超过 ${NAME_MAX_RUNES} 字符（当前 ${runeLength(name.trim())}）`,
      })
    }
    if (mode === 'service' && !serviceUrl.trim()) {
      found.push({ path: 'run.service.url', code: 'REQUIRED', message: '服务 base URL 必填' })
    }
    if (!profile.trim()) {
      found.push({ path: 'run.tests[0].execution.profile', code: 'REQUIRED', message: 'execution.profile 必填' })
    }
    // D-57：与后端同一句口径（path=run.tests 会被映射到 Task 卡片区就地显示）
    if (tests.length === 0) {
      found.push({
        path: 'run.tests',
        code: 'REQUIRED',
        message: '至少需要 1 个测试 Task（Pre-Check 由平台注入，不计入测试）',
      })
    }
    tests.forEach((test, index) => {
      if (!test.name.trim()) found.push({ path: `run.tests[${index}].name`, code: 'REQUIRED', message: 'Task 名称必填' })
      if (!test.tester && !defaultTester) {
        found.push({ path: `run.tests[${index}].tester`, code: 'REQUIRED', message: '必须选择 Tester' })
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
      const result = await api.submitRun(manifest)
      /**
       * B8：提交成功**只弹一个** toast（之前是「校验通过」+「已受理」两条，用户只来得及看一半）。
       * 里面既有结果也有下一步动作：复制 ID / 查看详情。
       * 提交内部的 dry-run 不弹 toast —— 「校验通过」是另一件事（手动点「校验」时才有），
       * 两条永远不同时出现。
       */
      const runId = result.run.id
      const queued = result.tasks.length
      toast({
        title: `已受理 ${runId} · ${queued} 个 Task 已入队`,
        description: (
          <span className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              data-testid="toast-copy-run-id"
              className="rounded-sm bg-white/15 px-1.5 py-0.5 text-2xs text-white hover:bg-white/25"
              onClick={() => void navigator.clipboard?.writeText(runId)}
            >
              复制 ID
            </button>
            <button
              type="button"
              data-testid="toast-open-run"
              className="rounded-sm bg-white/15 px-1.5 py-0.5 text-2xs text-white hover:bg-white/25"
              onClick={() => navigate(`/runs/${runId}`)}
            >
              查看详情
            </button>
          </span>
        ),
        tone: 'ok',
        // ≥8000ms：两个动作都要来得及点；可手动关闭（dismissible 默认 true）
        durationMs: 9000,
      })
      navigate(`/runs/${runId}`)
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <RightRail>
        <RailCard title="怎么填参数" tone="muted" sub="每个 Tester 一段字符串">
          <p className="rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-relaxed text-fg-2">
            每个 Task 只有一个「参数」输入框，照 Tester 文档写成一整串即可（平台只做切分，不校验含义）。
            这一栏的清单是给人看的说明，不是表单。
          </p>
          <div className="mt-3 space-y-2">
            {testerList.slice(0, 3).map((tester) => (
              <div key={tester.name}>
                <p className="flex flex-wrap items-center gap-1.5">
                  <Tag mono>{tester.name}</Tag>
                  <span className="text-2xs text-fg-3">{tester.role}</span>
                </p>
                <p className="mt-1 font-mono text-2xs text-fg-3">{tester.placeholder ?? '—'}</p>
              </div>
            ))}
          </div>
        </RailCard>

        <RailCard title="不用自己写的参数" tone="warn">
          <p>
            <code className="font-mono">--url</code> / <code className="font-mono">--model_name</code> /{' '}
            <code className="font-mono">--model_path</code> 由平台自动补在参数末尾。再自己写一遍会被拒绝——删掉它就好。
          </p>
        </RailCard>
      </RightRail>

      {/* 提交/校验失败：表单内摘要（不自动消失、自己滚进视口并聚焦、trace_id 可复制） */}
      <FormErrorSummary error={error} issues={issues} onJumpTo={focusField} />

      {!error && issues.length === 0 && dryRunOk === true ? <Banner tone="info" title="dry-run 通过" /> : null}

      <Card>
        <CardHeader tone="ok" title="运行" />
        <CardBody>
          {/*
            C8：maxlength=200（后端 D-58，按 rune 上限）+ 实时计数。
            粘贴可以绕过 maxlength，所以超限时这里也会立刻报红并把「提交」置灰（不等后端 400）。
          */}
          <Field
            label="Run 名称"
            required
            htmlFor="run-name"
            hint="同名不合并：每次提交都创建新 Run（不做幂等）。"
            error={nameError}
          >
            <div id="run-name-row" className="flex items-center gap-2">
              <Input
                id="run-name"
                ref={registerField('run.name')}
                value={name}
                maxLength={NAME_MAX_RUNES}
                onChange={(e) => setName(e.target.value)}
                placeholder="qwen3-32b-daily"
                aria-invalid={Boolean(nameError)}
                className="flex-1"
              />
              <span
                data-testid="run-name-counter"
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

      <Card>
        <CardHeader
          tone="muted"
          title="服务来源"
          sub="一张卡两形态：部署由 Genet 拉起；不部署就直接跑已有服务"
          actions={
            <Segmented<SourceMode>
              ariaLabel="服务来源"
              value={mode}
              onChange={setMode}
              options={[
                { value: 'deploy', label: '部署' },
                { value: 'service', label: '不部署' },
              ]}
            />
          }
        />
        <CardBody className="flex flex-col gap-2">
          {mode === 'deploy' ? (
            <>
              {/* CodeMirror 6：真语法高亮 + 行号 + 横向滚动；解析失败按 LinePos 标行 */}
              <YamlEditor
                value={deployYaml}
                onChange={setDeployYaml}
                rows={7}
                aria-label="deploy 段 YAML"
                error={deployParse.pos}
              />
              <p className={cn('text-2xs', deployParse.error ? 'text-danger-text' : 'text-success-text')}>
                {deployParse.error ? `✗ ${deployParse.error}` : '✓ YAML 结构有效（provider + parameters）'}
              </p>
              <p className="text-2xs leading-relaxed text-fg-3">
                平台只校 YAML 结构并转交 Genet；部署失败会终止整个 Run，不会创建任何 Task。一个 Deploy 可能拉起多个 Pod
                （多节点 / PD 分离），各自有日志且测试期间仍在追加。
              </p>
            </>
          ) : (
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label="服务 base URL" required error={issueFor('run.service.url')} className="sm:col-span-2">
                <Input ref={registerField('run.service.url')} value={serviceUrl} onChange={(e) => setServiceUrl(e.target.value)} placeholder="http://10.0.0.10:8000/v1" aria-invalid={Boolean(issueFor('run.service.url'))} />
              </Field>
              <Field label="模型名">
                <Input value={modelName} onChange={(e) => setModelName(e.target.value)} placeholder="Qwen3-32B" />
              </Field>
              <Field label="模型路径（可选）" className="sm:col-span-3">
                <Input value={modelPath} onChange={(e) => setModelPath(e.target.value)} placeholder="/data/models/qwen3-32b" />
              </Field>
              <p className="text-2xs leading-relaxed text-fg-3 sm:col-span-3">
                这三个字段就是平台要注入的公共参数来源：不部署时它们由你来填。
              </p>
            </div>
          )}
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          tone="warn"
          title="环境变量与凭据"
          sub="Run 粒度，注入该 Run 下全部 Task"
          actions={
            <>
              <Button size="sm" icon={<Plus className="size-3" aria-hidden />} onClick={() => setEnvRows((p) => [...p, { key: '', value: '' }])}>
                环境变量
              </Button>
              <Button size="sm" icon={<Plus className="size-3" aria-hidden />} onClick={() => setSecretRows((p) => [...p, { key: '', value: '' }])}>
                凭据引用
              </Button>
            </>
          }
        />
        <CardBody className="flex flex-col gap-2">
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
          <p className="text-2xs text-fg-3">凭据只填引用，明文不会出现在 Manifest 里；env 明文可回显可编辑（不脱敏、不折叠）。</p>
        </CardBody>
      </Card>

      <Card>
        <CardHeader
          tone="primary"
          title="Task 列表"
          sub="每个 Task 一段参数字符串；执行环境按 Task 指定"
          actions={
            <>
              <span className="text-2xs text-fg-3">执行环境</span>
              <Input
                ref={registerField('run.tests[0].execution.profile')}
                value={profile}
                onChange={(e) => setProfile(e.target.value)}
                aria-label="执行环境 profile"
                aria-invalid={Boolean(issueFor('run.tests[0].execution.profile'))}
                title={issueFor('run.tests[0].execution.profile') ?? undefined}
                placeholder="k8s-npu"
                className="h-8 w-32 font-mono text-2xs"
              />
              <span className="text-2xs text-fg-3">Pre-Check</span>
              <Segmented<PrecheckMode>
                ariaLabel="Pre-Check 模式"
                value={precheck}
                onChange={setPrecheck}
                options={[
                  { value: 'auto', label: 'auto 注入' },
                  { value: 'off', label: 'off' },
                ]}
              />
              <Button
                size="sm"
                icon={<Plus className="size-3" aria-hidden />}
                onClick={() =>
                  setTests((prev) => [
                    ...prev,
                    { key: newKey(), name: `task-${prev.length + 1}`, tester: defaultTester, args: '' },
                  ])
                }
              >
                添加 Task
              </Button>
            </>
          }
        />
        <CardBody className="flex flex-col gap-3">
          {/* A4：0 个测试 Task 时说清「为什么不能提交」，就在会被删空的那块下面 */}
          {noTasks ? (
            <p
              data-testid="task-empty-hint"
              className="rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs leading-relaxed text-warn-text"
            >
              至少需要 1 个测试 Task（Pre-Check 由平台注入，不计入测试）
            </p>
          ) : null}

          {/* B10：只有 pre_check 角色的 Tester 时，下拉里只剩一句占位，提交/校验都不该放开 */}
          {noTesters ? (
            <p
              data-testid="tester-empty-hint"
              className="rounded-md border border-warn/40 bg-warn-weak px-3 py-2 text-2xs leading-relaxed text-warn-text"
            >
              当前没有可用的 Tester；只有 pre_check 角色的 Tester 不能作为测试 Task
            </p>
          ) : null}

          {/* 后端仍可能返回 `run.tests` 级的 issue（并发/边界）：就地显示在 Task 区 */}
          {taskAreaIssues.length > 0 ? (
            <div
              data-testid="task-area-issues"
              role="alert"
              className="rounded-md border border-danger/30 bg-danger-weak px-3 py-2"
            >
              {taskAreaIssues.map((issue, index) => (
                <p key={`${issue.path}-${index}`} className="text-2xs leading-relaxed text-danger-text">
                  <code className="mr-1.5 font-mono">{issue.path}</code>
                  {issue.message}
                </p>
              ))}
            </div>
          ) : null}

          {precheck === 'auto' ? (
            <div className="rounded-md border border-line-soft bg-surface-2 px-3 py-2 text-2xs leading-relaxed text-fg-2">
              <span className="font-medium text-fg">auto：</span>
              平台取注册表中唯一一个 <code className="font-mono">role=pre_check</code> 的 Tester 注入为 Task#1；0 个或多个都拒绝提交。
              {precheckTester ? (
                <Tag mono className="ml-2">
                  {precheckTester.name} · {precheckTester.version}
                </Tag>
              ) : null}
            </div>
          ) : null}

          {tests.map((test, index) => (
            <div key={test.key} className="rounded-md border border-line-soft p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span className="grid size-5 shrink-0 place-items-center rounded-full bg-surface-3 text-2xs text-fg-2">
                  {index + 1}
                </span>
                <Input
                  value={test.name}
                  aria-label={`Task ${index + 1} 名称`}
                  aria-invalid={Boolean(issueFor(`run.tests[${index}].name`))}
                  title={issueFor(`run.tests[${index}].name`) ?? undefined}
                  placeholder="accuracy-dataset-a"
                  className="h-8 w-56"
                  onChange={(e) => setTest(test.key, { name: e.target.value })}
                />
                <Select
                  value={test.tester || defaultTester}
                  aria-label={`Task ${index + 1} Tester`}
                  className="h-8 w-48"
                  onChange={(e) => setTest(test.key, { tester: e.target.value })}
                >
                  {testerOptions.length === 0 ? <option value="">（还没有 Tester）</option> : null}
                  {testerOptions.map((tester) => (
                    <option key={tester.name} value={tester.name}>
                      {tester.name} v{tester.version}
                    </option>
                  ))}
                </Select>
                {(() => {
                  const selected = testerList.find((t) => t.name === (test.tester || defaultTester))
                  return selected ? (
                    <span className="flex items-center gap-1">
                      <Tag mono>{selected.version}</Tag>
                      {selected.exclusive ? <Tag>独占</Tag> : null}
                      <Tag>{selected.on_failure}</Tag>
                    </span>
                  ) : null
                })()}
                {issues.some((issue) => issue.path.startsWith(`tests[${index}]`)) ? (
                  <Badge tone="err">有校验问题</Badge>
                ) : null}
                <Button
                  size="sm"
                  variant="ghost"
                  className="ml-auto"
                  aria-label={`移除 Task ${index + 1}`}
                  data-testid={`remove-task-${index + 1}`}
                  onClick={() => removeTask(index)}
                >
                  移除
                </Button>
              </div>
              <div className="mt-2">
                <Field
                  label="参数（原样填，平台只做词法切分）"
                  error={issueFor(`tests[${index}].args`) ?? issueFor(`tests[${index}].tester`)}
                >
                  <Textarea
                    value={test.args}
                    rows={2}
                    spellCheck={false}
                    aria-label={`Task ${index + 1} 参数`}
                    placeholder={
                      testerList.find((t) => t.name === (test.tester || defaultTester))?.placeholder ?? '--dataset=...'
                    }
                    className="font-mono text-xs"
                    onChange={(e) => setTest(test.key, { args: e.target.value })}
                  />
                </Field>
              </div>
            </div>
          ))}
        </CardBody>
        <CardFooter>
          <span>
            {blocked
              ? `当前角色 ${auth.role ?? 'viewer'} 只读：${blocked}（服务端也会再判一次）`
              : submitDisabled
                ? submitTitle
                : '提交前会先 dry-run；校验通过才真正 POST /runs（每次提交都创建新 Run）。'}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button
              size="sm"
              data-testid="validate-run"
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
              data-testid="submit-run"
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
    </>
  )
}