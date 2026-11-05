import { useMemo, useState } from 'react'
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
import { Segmented } from '@/components/ui/segmented'
import { Banner, ErrorState } from '@/components/ui/states'
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

/** 把 YAML 文本解析成 deploy 段；返回错误信息而不是抛。 */
function parseDeploy(text: string): { deploy: DeployManifest | null; error: string | null } {
  if (!text.trim()) return { deploy: null, error: 'deploy 段不能为空' }
  let parsed: unknown
  try {
    parsed = parseYaml(text)
  } catch (e) {
    return { deploy: null, error: `YAML 解析失败：${(e as Error).message}` }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { deploy: null, error: 'deploy 段必须是一个对象（含 provider / parameters）' }
  }
  const rec = parsed as Record<string, unknown>
  const provider = typeof rec['provider'] === 'string' ? rec['provider'] : ''
  if (!provider) return { deploy: null, error: '缺少顶层 provider（平台按 provider 转交 Genet）' }
  const parameters =
    rec['parameters'] && typeof rec['parameters'] === 'object'
      ? (rec['parameters'] as Record<string, unknown>)
      : {}
  return { deploy: { provider, parameters }, error: null }
}

export function SubmitPage() {
  const navigate = useNavigate()
  const toast = useToast()
  const { auth } = useIdentity()
  /** viewer 只读：写按钮置灰 + title 提示（不靠后端 403 兜底） */
  const blocked = writeBlockedReason(auth)
  const blockedTitle = blocked ?? undefined

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

  const setTest = (key: string, patch: Partial<TestDraft>) => {
    setTests((prev) => prev.map((test) => (test.key === key ? { ...test, ...patch } : test)))
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
    const issueCount = result.issues?.length ?? 0
    if (result.valid) {
      toast({ title: '校验通过', description: '没有发现问题，可以提交。', tone: 'ok' })
    } else {
      toast({ title: `校验未通过：${issueCount} 个问题`, tone: 'warn' })
    }
    return result.valid
  }

  const onDryRun = async () => {
    const manifest = buildManifest()
    setError(null)
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
    if (!manifest) {
      setDryRunOk(false)
      setError({
        code: 'VALIDATION_ERROR',
        message: deployParse.error ?? '表单还不完整',
        retryable: false,
      })
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
      toast({ title: `已受理：${result.run.id}`, description: `${result.tasks.length} 个 Task 已入队`, tone: 'ok' })
      navigate(`/runs/${result.run.id}`)
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <RightRail>
        <RailCard title="参数说明" tone="muted" sub="只读文档语义">
          <p className="rounded-md border border-line-soft bg-surface-2 p-3 text-2xs leading-relaxed text-fg-2">
            平台不渲染表单、不校验参数。每个 Task 只有一个 args 输入框，这一页的清单是给人看的说明书，不是表单定义。
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

        <RailCard title="公共参数" tone="warn">
          <p>
            <code className="font-mono">--url</code> / <code className="font-mono">--model_name</code> /{' '}
            <code className="font-mono">--model_path</code> 由平台注入到 argv 末尾。args 里再写一遍会被拒绝（
            <code className="font-mono">PUBLIC_PARAM_CONFLICT</code>）。
          </p>
        </RailCard>
      </RightRail>

      {error ? <ErrorState error={error} compact /> : null}

      {issues.length > 0 ? (
        <Banner tone="err" title={`校验未通过（${issues.length} 个问题）`}>
          <ul className="mt-1 list-disc space-y-1 pl-4">
            {issues.slice(0, 6).map((issue, index) => (
              <li key={`${issue.path}-${index}`}>
                <code className="font-mono text-2xs">{issue.path}</code> · {issue.code} · {issue.message}
              </li>
            ))}
          </ul>
        </Banner>
      ) : dryRunOk === true ? (
        <Banner tone="info" title="dry-run 通过" />
      ) : null}

      <Card>
        <CardHeader tone="ok" title="运行" />
        <CardBody>
          <Field label="Run 名称" required hint="同名不合并：每次提交都创建新 Run（不做幂等）。" error={issueFor('run.name')}>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="qwen3-32b-daily" />
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
              <Textarea
                value={deployYaml}
                onChange={(e) => setDeployYaml(e.target.value)}
                rows={7}
                spellCheck={false}
                aria-label="deploy 段 YAML"
                className="font-mono text-xs leading-relaxed"
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
                <Input value={serviceUrl} onChange={(e) => setServiceUrl(e.target.value)} placeholder="http://10.0.0.10:8000/v1" />
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
              <Button size="sm" variant="ghost" aria-label="删除该环境变量" onClick={() => setEnvRows((p) => p.filter((_, i) => i !== index))}>
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
              <Button size="sm" variant="ghost" aria-label="删除该凭据引用" onClick={() => setSecretRows((p) => p.filter((_, i) => i !== index))}>
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
                value={profile}
                onChange={(e) => setProfile(e.target.value)}
                aria-label="执行环境 profile"
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
                  onClick={() => setTests((prev) => prev.filter((item) => item.key !== test.key))}
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
              : '提交前会先 dry-run；校验通过才真正 POST /runs（每次提交都创建新 Run）。'}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button
              size="sm"
              data-testid="validate-run"
              loading={dryRunning}
              disabled={Boolean(blocked)}
              title={blockedTitle}
              onClick={() => void onDryRun()}
            >
              校验
            </Button>
            <Button
              size="sm"
              variant="primary"
              data-testid="submit-run"
              loading={submitting}
              disabled={Boolean(blocked)}
              title={blockedTitle}
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
