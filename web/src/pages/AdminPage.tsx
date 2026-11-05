import { useMemo, useState } from 'react'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { api } from '@/api'
import { toApiError } from '@/api/client'
import type {
  AdminCluster,
  ApiErrorBody,
  AuditEntry,
  ClusterWrite,
  ExecutionProfile,
  ProfileWrite,
  SecretWrite,
} from '@/api/types'
import { RightRail, RailCard } from '@/components/app/RightRail'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardBody, CardFooter, CardHeader } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { TableSkeleton } from '@/components/ui/skeleton'
import { Banner, EmptyState, ErrorState } from '@/components/ui/states'
import { Table, TBody, TD, TH, THead, TR, TableWrap } from '@/components/ui/table'
import { Tabs, tabPanelProps } from '@/components/ui/tabs'
import { useToast } from '@/components/ui/toast'
import { useAsync } from '@/hooks/useAsync'
import { useIdentity } from '@/hooks/useIdentity'
import { formatDateTime, formatDuration } from '@/lib/utils'

type TabKey = 'profiles' | 'clusters' | 'secrets' | 'audit'

const TAB_ITEMS: Array<{ value: TabKey; label: string }> = [
  { value: 'profiles', label: 'Profiles' },
  { value: 'clusters', label: 'Clusters' },
  { value: 'secrets', label: 'Secrets' },
  { value: 'audit', label: 'Audit' },
]

/* ---------------------------------------------------------------- Profiles */

const EMPTY_PROFILE: ProfileWrite = {
  name: '',
  cluster: '',
  namespace: 'testplane',
  scheduler: 'volcano',
  queue: 'npu-batch',
  deploy_timeout_seconds: 259200,
}

function ProfilesTab() {
  const toast = useToast()
  const profiles = useAsync(() => api.listProfiles(), [])
  const [draft, setDraft] = useState<ProfileWrite | null>(null)
  const [editing, setEditing] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)

  const save = async () => {
    if (!draft) return
    setSaving(true)
    setError(null)
    try {
      if (editing) await api.updateProfile(editing, draft)
      else await api.createProfile(draft)
      toast({ title: editing ? 'Profile 已更新' : 'Profile 已创建', tone: 'ok' })
      setDraft(null)
      setEditing(null)
      profiles.reload()
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSaving(false)
    }
  }

  /** 删除要二次确认（评审 #14）：弹窗里写明将被删的对象名 */
  const [pendingDelete, setPendingDelete] = useState<ExecutionProfile | null>(null)
  const [deleting, setDeleting] = useState(false)

  const remove = async () => {
    if (!pendingDelete) return
    setDeleting(true)
    try {
      await api.deleteProfile(pendingDelete.name)
      toast({ title: `已删除 Profile：${pendingDelete.name}`, tone: 'warn' })
      setPendingDelete(null)
      profiles.reload()
    } catch (e) {
      const err = toApiError(e)
      toast({ title: '删除失败', description: `${err.code}：${err.message}`, tone: 'err' })
    } finally {
      setDeleting(false)
    }
  }

  return (
    <Card>
      <CardHeader
        tone="primary"
        title="Execution Profiles"
        sub="提交 Run 时 execution.profile 必须命中这里"
        actions={
          <Button
            size="sm"
            icon={<Plus className="size-3" aria-hidden />}
            onClick={() => {
              setEditing(null)
              setDraft({ ...EMPTY_PROFILE })
            }}
          >
            新建 Profile
          </Button>
        }
      />
      {profiles.error ? (
        <CardBody>
          <ErrorState error={profiles.error} onRetry={profiles.reload} />
        </CardBody>
      ) : profiles.loading && !profiles.data ? (
        <TableSkeleton rows={3} cols={6} />
      ) : (profiles.data ?? []).length === 0 ? (
        <EmptyState title="没有 Profile" description="至少需要一个，否则提交 Run 会 EXECUTION_PROFILE_NOT_FOUND。" />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <TH>名称</TH>
                <TH>集群</TH>
                <TH>命名空间</TH>
                <TH>调度器</TH>
                <TH>队列</TH>
                <TH>部署窗口</TH>
                <TH className="text-right">操作</TH>
              </TR>
            </THead>
            <TBody>
              {(profiles.data ?? []).map((profile) => (
                <TR key={profile.name}>
                  <TD className="font-mono text-2xs text-fg">{profile.name}</TD>
                  <TD className="font-mono text-2xs">{profile.cluster}</TD>
                  <TD className="font-mono text-2xs">{profile.namespace}</TD>
                  <TD>
                    <Badge tone={profile.scheduler === 'volcano' ? 'primary' : 'muted'}>{profile.scheduler}</Badge>
                  </TD>
                  <TD className="font-mono text-2xs">{profile.queue ?? '—'}</TD>
                  <TD className="tabular-nums">{formatDuration(profile.deploy_timeout_seconds)}</TD>
                  <TD className="text-right">
                    <span className="inline-flex gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`编辑 ${profile.name}`}
                        onClick={() => {
                          setEditing(profile.name)
                          setDraft({
                            name: profile.name,
                            cluster: profile.cluster,
                            namespace: profile.namespace,
                            scheduler: profile.scheduler,
                            queue: profile.queue ?? '',
                            deploy_timeout_seconds: profile.deploy_timeout_seconds,
                          })
                        }}
                      >
                        <Pencil className="size-3.5" aria-hidden />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`删除 ${profile.name}`}
                        data-testid="delete-profile"
                        onClick={() => setPendingDelete(profile)}
                      >
                        <Trash2 className="size-3.5" aria-hidden />
                      </Button>
                    </span>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <Dialog
        open={draft !== null}
        onClose={() => setDraft(null)}
        title={editing ? `编辑 Profile：${editing}` : '新建 Profile'}
        footer={
          <>
            <Button onClick={() => setDraft(null)}>取消</Button>
            <Button variant="primary" loading={saving} onClick={() => void save()}>
              保存
            </Button>
          </>
        }
      >
        {error ? <ErrorState error={error} compact className="mb-3" /> : null}
        {draft ? (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="name" required>
              <Input
                value={draft.name}
                disabled={Boolean(editing)}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
            </Field>
            <Field label="cluster" required>
              <Input value={draft.cluster} onChange={(e) => setDraft({ ...draft, cluster: e.target.value })} />
            </Field>
            <Field label="namespace" required>
              <Input value={draft.namespace} onChange={(e) => setDraft({ ...draft, namespace: e.target.value })} />
            </Field>
            <Field label="scheduler" required>
              <Select value={draft.scheduler} onChange={(e) => setDraft({ ...draft, scheduler: e.target.value })}>
                <option value="volcano">volcano</option>
                <option value="default">default</option>
              </Select>
            </Field>
            <Field label="queue">
              <Input value={draft.queue ?? ''} onChange={(e) => setDraft({ ...draft, queue: e.target.value })} />
            </Field>
            <Field label="deploy_timeout_seconds" required>
              <Input
                type="number"
                value={draft.deploy_timeout_seconds}
                onChange={(e) => setDraft({ ...draft, deploy_timeout_seconds: Number(e.target.value) })}
              />
            </Field>
          </div>
        ) : null}
      </Dialog>

      {/* 删除二次确认（评审 #14） */}
      <Dialog
        open={pendingDelete !== null}
        onClose={() => setPendingDelete(null)}
        title="删除这个 Profile？"
        description="删除只影响之后的提交：历史 Task 冻结了 profile 名，不会因此失败。"
        footer={
          <>
            <Button onClick={() => setPendingDelete(null)}>取消</Button>
            <Button variant="danger" data-testid="confirm-delete" loading={deleting} onClick={() => void remove()}>
              确认删除
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-xs text-fg-2">
          <p>
            将被删除：<span className="font-mono text-fg">{pendingDelete?.name}</span>
          </p>
          {pendingDelete ? (
            <p className="text-2xs text-fg-3">
              集群 <span className="font-mono">{pendingDelete.cluster}</span> · 命名空间{' '}
              <span className="font-mono">{pendingDelete.namespace}</span> · 调度器{' '}
              <span className="font-mono">{pendingDelete.scheduler}</span>
            </p>
          ) : null}
        </div>
      </Dialog>
    </Card>
  )
}

const EMPTY_CLUSTER: ClusterWrite = { name: '', api_server: '', kubeconfig_key: '', description: '' }

function ClustersTab() {
  const toast = useToast()
  const clusters = useAsync(() => api.listAdminClusters(), [])
  const [draft, setDraft] = useState<ClusterWrite | null>(null)
  const [editing, setEditing] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)
  const [refs, setRefs] = useState<string[]>([])

  /** 删除要二次确认（评审 #14）；被引用时把后端 409 的 `detail.profiles[]` 显示在弹窗里 */
  const [pendingDelete, setPendingDelete] = useState<AdminCluster | null>(null)
  const [deleting, setDeleting] = useState(false)

  const remove = async () => {
    if (!pendingDelete) return
    setRefs([])
    setError(null)
    setDeleting(true)
    try {
      await api.deleteAdminCluster(pendingDelete.name)
      toast({ title: `已删除集群：${pendingDelete.name}`, tone: 'warn' })
      setPendingDelete(null)
      clusters.reload()
    } catch (e) {
      const err = toApiError(e)
      // 被引用时后端 409，detail.profiles[] 列出引用者
      const detail = (err.detail ?? {}) as { profiles?: unknown }
      const profiles = Array.isArray(detail.profiles) ? detail.profiles.map(String) : []
      setRefs(profiles)
      setError(err)
      toast({ title: '删除被拒绝', description: `${err.code}：${err.message}`, tone: 'err' })
    } finally {
      setDeleting(false)
    }
  }

  const save = async () => {
    if (!draft) return
    setSaving(true)
    setError(null)
    try {
      if (editing) await api.updateAdminCluster(editing, draft)
      else await api.createAdminCluster(draft)
      toast({ title: editing ? '集群已更新' : '集群已创建', tone: 'ok' })
      setDraft(null)
      setEditing(null)
      clusters.reload()
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardHeader
        tone="info"
        title="集群"
        sub="kubeconfig 放 Secret；这里只维护定位信息"
        actions={
          <Button
            size="sm"
            icon={<Plus className="size-3" aria-hidden />}
            onClick={() => {
              setEditing(null)
              setDraft({ ...EMPTY_CLUSTER })
            }}
          >
            新建集群
          </Button>
        }
      />
      {error ? (
        <CardBody className="pb-0">
          <ErrorState error={error} compact />
          {refs.length > 0 ? (
            <p className="mt-2 text-2xs text-fg-2">
              仍被这些 Profile 引用：<span className="font-mono">{refs.join(', ')}</span>
            </p>
          ) : null}
        </CardBody>
      ) : null}
      {clusters.error ? (
        <CardBody>
          <ErrorState error={clusters.error} onRetry={clusters.reload} />
        </CardBody>
      ) : clusters.loading && !clusters.data ? (
        <TableSkeleton rows={3} cols={5} />
      ) : (clusters.data ?? []).length === 0 ? (
        <EmptyState title="没有集群" description="先登记一个可用的 K8s 集群，Profile 才有地方落。" />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <TH>名称</TH>
                <TH>API Server</TH>
                <TH>kubeconfig</TH>
                <TH>上次检查</TH>
                <TH className="text-right">操作</TH>
              </TR>
            </THead>
            <TBody>
              {(clusters.data ?? []).map((cluster) => (
                <TR key={cluster.name}>
                  <TD className="font-mono text-2xs text-fg">{cluster.name}</TD>
                  <TD className="break-all font-mono text-2xs">{cluster.api_server}</TD>
                  <TD className="font-mono text-2xs text-fg-3">{cluster.kubeconfig_key ?? '—'}</TD>
                  <TD className="whitespace-nowrap text-2xs text-fg-3">
                    {cluster.last_check_at ? formatDateTime(cluster.last_check_at) : '—'}
                    {cluster.last_check_result ? (
                      <span className="ml-1 text-fg-2">{cluster.last_check_result}</span>
                    ) : null}
                  </TD>
                  <TD className="text-right">
                    <span className="inline-flex gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`编辑 ${cluster.name}`}
                        onClick={() => {
                          setEditing(cluster.name)
                          setDraft({
                            name: cluster.name,
                            api_server: cluster.api_server,
                            kubeconfig_key: cluster.kubeconfig_key ?? '',
                            description: cluster.description ?? '',
                          })
                        }}
                      >
                        <Pencil className="size-3.5" aria-hidden />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`删除 ${cluster.name}`}
                        data-testid="delete-cluster"
                        onClick={() => setPendingDelete(cluster)}
                      >
                        <Trash2 className="size-3.5" aria-hidden />
                      </Button>
                    </span>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <Dialog
        open={draft !== null}
        onClose={() => setDraft(null)}
        title={editing ? `编辑集群：${editing}` : '新建集群'}
        footer={
          <>
            <Button onClick={() => setDraft(null)}>取消</Button>
            <Button variant="primary" loading={saving} onClick={() => void save()}>
              保存
            </Button>
          </>
        }
      >
        {draft ? (
          <div className="grid gap-3">
            <Field label="name" required>
              <Input
                value={draft.name}
                disabled={Boolean(editing)}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              />
            </Field>
            <Field label="api_server" required>
              <Input value={draft.api_server} onChange={(e) => setDraft({ ...draft, api_server: e.target.value })} />
            </Field>
            <Field label="kubeconfig_key" hint="挂载目录里的文件名；这里只存名字，不存凭据">
              <Input
                value={draft.kubeconfig_key ?? ''}
                onChange={(e) => setDraft({ ...draft, kubeconfig_key: e.target.value })}
              />
            </Field>
            <Field label="description">
              <Input
                value={draft.description ?? ''}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </Field>
          </div>
        ) : null}
      </Dialog>

      {/* 删除二次确认（评审 #14）：被 Profile 引用时 409，引用者直接列在弹窗里 */}
      <Dialog
        open={pendingDelete !== null}
        onClose={() => {
          setPendingDelete(null)
          setError(null)
          setRefs([])
        }}
        title="删除这个集群？"
        description="仍有 Profile 引用时后端会 409，不会静默级联删除。"
        footer={
          <>
            <Button
              onClick={() => {
                setPendingDelete(null)
                setError(null)
                setRefs([])
              }}
            >
              取消
            </Button>
            <Button variant="danger" data-testid="confirm-delete" loading={deleting} onClick={() => void remove()}>
              确认删除
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-xs text-fg-2">
          <p>
            将被删除：<span className="font-mono text-fg">{pendingDelete?.name}</span>
          </p>
          {pendingDelete ? (
            <p className="break-all text-2xs text-fg-3">
              API Server <span className="font-mono">{pendingDelete.api_server}</span> · kubeconfig{' '}
              <span className="font-mono">{pendingDelete.kubeconfig_key ?? '—'}</span>
            </p>
          ) : null}
          {error ? (
            <div className="mt-1 flex flex-col gap-1">
              <ErrorState error={error} compact />
              {refs.length > 0 ? (
                <p className="text-2xs text-fg-2">
                  仍被这些 Profile 引用：<span className="font-mono">{refs.join(', ')}</span>
                </p>
              ) : null}
            </div>
          ) : null}
        </div>
      </Dialog>
    </Card>
  )
}

/* ---------------------------------------------------------------- Secrets */

const EMPTY_SECRET: SecretWrite = { name: '', namespace: 'testplane', secret_name: '', key: '', description: '' }

function SecretsTab() {
  const toast = useToast()
  const secrets = useAsync(() => api.listAdminSecrets(), [])
  const [draft, setDraft] = useState<SecretWrite | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<ApiErrorBody | null>(null)

  const save = async () => {
    if (!draft) return
    setSaving(true)
    setError(null)
    try {
      await api.createAdminSecret(draft)
      toast({ title: `已登记引用：${draft.name}`, tone: 'ok' })
      setDraft(null)
      secrets.reload()
    } catch (e) {
      setError(toApiError(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardHeader
        tone="warn"
        title="Secrets 引用"
        sub="只登记引用：不显示、也不回写任何 Secret 值"
        actions={
          <Button size="sm" icon={<Plus className="size-3" aria-hidden />} onClick={() => setDraft({ ...EMPTY_SECRET })}>
            登记引用
          </Button>
        }
      />
      {secrets.error ? (
        <CardBody>
          <ErrorState error={secrets.error} onRetry={secrets.reload} />
        </CardBody>
      ) : secrets.loading && !secrets.data ? (
        <TableSkeleton rows={3} cols={5} />
      ) : (secrets.data ?? []).length === 0 ? (
        <EmptyState title="没有登记任何 Secret 引用" description="Run 的 secrets 只能引用这里登记的条目。" />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <TH>名称</TH>
                <TH>命名空间</TH>
                <TH>Secret 名</TH>
                <TH>键</TH>
                <TH>说明</TH>
              </TR>
            </THead>
            <TBody>
              {(secrets.data ?? []).map((secret) => (
                <TR key={secret.name}>
                  <TD className="font-mono text-2xs text-fg">{secret.name}</TD>
                  <TD className="font-mono text-2xs">{secret.namespace}</TD>
                  <TD className="font-mono text-2xs">{secret.secret_name}</TD>
                  <TD className="font-mono text-2xs">{secret.key}</TD>
                  <TD className="text-2xs text-fg-3">{secret.description ?? '—'}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <CardFooter>
        <span>值永远不经过前端：这里只有定位信息（namespace / secret_name / key）。</span>
      </CardFooter>

      <Dialog
        open={draft !== null}
        onClose={() => setDraft(null)}
        title="登记 Secret 引用"
        footer={
          <>
            <Button onClick={() => setDraft(null)}>取消</Button>
            <Button variant="primary" loading={saving} onClick={() => void save()}>
              登记
            </Button>
          </>
        }
      >
        {error ? <ErrorState error={error} compact className="mb-3" /> : null}
        {draft ? (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="name（Run 里引用名）" required>
              <Input value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </Field>
            <Field label="namespace" required>
              <Input value={draft.namespace} onChange={(e) => setDraft({ ...draft, namespace: e.target.value })} />
            </Field>
            <Field label="secret_name" required>
              <Input value={draft.secret_name} onChange={(e) => setDraft({ ...draft, secret_name: e.target.value })} />
            </Field>
            <Field label="key" required>
              <Input value={draft.key} onChange={(e) => setDraft({ ...draft, key: e.target.value })} />
            </Field>
            <Field label="description" className="sm:col-span-2">
              <Input
                value={draft.description ?? ''}
                onChange={(e) => setDraft({ ...draft, description: e.target.value })}
              />
            </Field>
          </div>
        ) : null}
      </Dialog>
    </Card>
  )
}

/* ---------------------------------------------------------------- Audit */

function AuditTab() {
  const [action, setAction] = useState('')
  const audit = useAsync(() => api.listAudit({ action: action || undefined, limit: 100 }), [action])
  const rows: AuditEntry[] = audit.data?.items ?? []

  const actions = useMemo(() => {
    const set = new Set(rows.map((row) => row.action))
    return [...set].sort()
  }, [rows])

  return (
    <Card>
      <CardHeader
        tone="muted"
        title="审计日志"
        sub={`${rows.length} 条 · 只读`}
        actions={
          <Select aria-label="按 action 过滤" value={action} onChange={(e) => setAction(e.target.value)} className="h-8 w-44">
            <option value="">全部 action</option>
            {actions.map((item) => (
              <option key={item} value={item}>
                {item}
              </option>
            ))}
          </Select>
        }
      />
      {audit.error ? (
        <CardBody>
          <ErrorState error={audit.error} onRetry={audit.reload} />
        </CardBody>
      ) : audit.loading && !audit.data ? (
        <TableSkeleton rows={5} cols={6} />
      ) : rows.length === 0 ? (
        <EmptyState title="没有审计记录" description="换个 action 过滤，或等有写操作发生。" />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <TH>时间</TH>
                <TH>操作者</TH>
                <TH>角色</TH>
                <TH>动作</TH>
                <TH>对象类型</TH>
                <TH>对象 ID</TH>
              </TR>
            </THead>
            <TBody>
              {rows.map((entry) => (
                <TR key={entry.id}>
                  <TD className="whitespace-nowrap text-2xs text-fg-3">{formatDateTime(entry.at)}</TD>
                  <TD className="text-fg">{entry.actor}</TD>
                  <TD>
                    <Badge tone={entry.role === 'admin' ? 'warn' : entry.role === 'operator' ? 'info' : 'muted'}>
                      {entry.role}
                    </Badge>
                  </TD>
                  <TD className="font-mono text-2xs">{entry.action}</TD>
                  <TD className="font-mono text-2xs text-fg-3">{entry.object_type}</TD>
                  <TD className="font-mono text-2xs">{entry.object_id}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}
      <CardFooter>
        <span>审计只读；写操作的 actor/role 由服务端按会话写入。</span>
      </CardFooter>
    </Card>
  )
}

/* ---------------------------------------------------------------- 页面 */

export function AdminPage() {
  const { auth } = useIdentity()
  const [tab, setTab] = useState<TabKey>('profiles')

  if (auth.ready && auth.role !== 'admin') {
    return (
      <>
        <Banner role="alert" tone="warn" title="无权限：本页仅 admin 可见">
          当前角色 {auth.role ?? '未知'}；需要 admin 角色才能维护 Profile / 集群 / Secret 与查看审计。
        </Banner>
        <Card>
          <CardHeader tone="muted" title="管理台" />
          <CardBody>
            <p className="text-xs text-fg-2">
              这一页不会把你跳走：留在原地，等你换成 admin 身份（或让管理员加组）后刷新即可。
            </p>
          </CardBody>
        </Card>
      </>
    )
  }

  return (
    <>
      <RightRail>
        <RailCard title="边界" tone="warn">
          <p>管理台只维护「定位信息」：Profile 指向集群与队列、Cluster 指向 kubeconfig 文件名、Secret 只登记引用。</p>
          <p className="mt-2">任何凭据值都不经过前端。</p>
        </RailCard>
        <RailCard title="动作都会进审计" tone="muted">
          <p>新建 / 编辑 / 删除都会写审计（Audit Tab 可见），actor 与 role 由服务端按会话记录。</p>
        </RailCard>
      </RightRail>

      <Tabs<TabKey>
        ariaLabel="管理台分区"
        idBase="admin"
        value={tab}
        onChange={setTab}
        items={TAB_ITEMS}
        className="rounded-lg border border-line-soft bg-surface px-3 pt-1 shadow-card"
      />

      <section {...tabPanelProps('admin', tab)}>
        {tab === 'profiles' ? <ProfilesTab /> : null}
        {tab === 'clusters' ? <ClustersTab /> : null}
        {tab === 'secrets' ? <SecretsTab /> : null}
        {tab === 'audit' ? <AuditTab /> : null}
      </section>
    </>
  )
}