# 执行层实现（execution）
> **范围：** 从"拿到一份 Job Spec"到"证据落盘、可被报告引用"之间的实现：Job 渲染与生命周期观测、挂载目录、超时取消、清理回收、日志采集、产物校验与配额、**内置 deploy/clean runner 与 Genet 交互契约（deploy-state.json）**、Runner Registry、Volcano 只读可见性。
> **不在范围：** Task / attempt / runner 状态机与转移、管道编排、调度与外接模式端点槽位、API 形状、报告结构与前端——归 `control-plane.md`。本文只给执行层对外的接口，不重复状态机细节。
> **基线：** `ARCHITECTURE.md`（**v2.0**，本文已按 D-66～D-73 同步）。冲突以架构为准；本文新增的默认值都标了出处，架构未定义的标"待裁决"。
> **既定选型：** Go 单二进制（`api` / `controller` / `collector`）、PostgreSQL 唯一权威状态源、NFS（RWX）产物存储、K8s Job 唯一 Executor、不加 sidecar、不依赖集群日志系统（§8.6）；**平台不直连 Genet**——Genet 交互在内置 runner 内（D-70）。
> **编号：** 本文节号即 `IMPLEMENTATION.md` §6 的 `E-N`（§2–§6 Executor、§7 日志、§8 产物、§9 内置 deploy/clean runner 与 Genet 契约、§10 Registry、§11 可见性）。

## 1 边界与职责
| 组件 | 只做什么 | 明确不做 |
|---|---|---|
| **Executor**（`controller`） | Job Spec → `batch/v1 Job`；创建、观测（informer + 对账）、取消、删除；目录预创建；返回执行事实 | 不解析输入语义、不拼 Runner 命令、不判指标、不写状态表 |
| **日志采集**（`collector`） | runner stdout/stderr 与部署 Pod（Genet 渲染）日志流式落盘；重启/缺口标记；分段索引 | 不解析日志内容、不因日志体积改判 runner、不脱敏（K-5）、不依赖 Loki/ES |
| **deploy-state 解析**（`controller`，原 Genet 适配瘦化） | 读取并校验 service-deploy 产出的 `deploy-state.json`、归档副本、给 UI 投影 | **不再直连 Genet**（D-70）；不做 Helm 渲染、不解释拓扑；Genet HTTP 交互全部在 runner 内 |
| **内置 deploy/clean Runner**（`runners/service-deploy`、`runners/service-clean`） | 调 Genet OpenAPI 拉起 / 拆除；周期写 `deploy-state.json`；导出 `AG_SERVED_*` / `AG_DEPLOY_REF` | 不是平台代码路径，是随平台仓库发版的 Runner（§12.1）；平台只消费其产物 |
| **Runner Registry**（`controller`） | 扫描 `runners/`、校验、与库同步、暴露参数清单 | 不构建镜像、不渲染表单、不做运行时参数校验 |
| **Volcano 视图**（`api`） | 只读查 Queue / PodGroup / Job / Node，算 `waiting_reason` 输入 | 不改权重、不抢占、不写回调度器、不部署第二套监控 |

**接口边界：状态只由 control-plane 写，事实只由执行层产出。** 执行层不持有 `database/sql` 连接，不 import 状态机包。

| 数据 | 写入方 | 说明 |
|---|---|---|
| `tasks` / `attempts` / `task_runners` 状态字段 | control-plane | 执行层只返回事实，事务内落库（§12.5） |
| `task_runners.exit_code` / `error` / `log_ref` / `exports_ref` 等物理事实 | 执行层经 store 窄接口回写 | 不写 `tasks` 状态字段（IMPLEMENTATION §5 依赖方向）；状态推进仍由 control-plane 在事务内做 |
| `artifacts` 索引 | control-plane | 执行层只给候选清单（§8.5） |
| `events` 表 + `events.jsonl` | control-plane | 执行层调 `EventSink`，不自行落库 |
| NFS 上 runner 产物（含 `exports.env`、`deploy-state.json`）、日志、`pods.json` | 执行层 / runner 容器 | 直接写共享存储最终路径（§6.8） |
| K8s Job / NetworkPolicy / Lease | 执行层 | label + annotation 自描述，便于 GC |
| `attempts/<n>/deploy-state.json` 归档副本 | control-plane（读 runner 输出后同步） | 数据源是 runner 产物，平台只复制（D-70） |

```go
// Executor 是平台唯一执行适配层（§6.2）。实现：k8sExecutor；测试：fakeExecutor。
type Executor interface {
	Submit(ctx context.Context, spec JobSpec) (JobRef, error)      // 同 job_id 幂等（§2.4）
	Cancel(ctx context.Context, ref JobRef, mode CancelMode) error // 删 Job 并推动 Pod 消失
	Status(ctx context.Context, ref JobRef) (JobStatus, error)     // 命中 cache 不打 apiserver
	Reconcile(ctx context.Context, refs []JobRef) ([]JobStatus, error)
}
type JobStatus struct {
	JobID, Reason, ErrorCode string // Reason 是 k8s 原文，永不丢
	Phase                    JobPhase
	ExitCode                 *int32
	StartedAt, FinishedAt    *time.Time
	Pods                     []PodStatus
}
type EventSink interface{ Emit(ctx context.Context, ev Event) error }
type ResultSink interface { // 结论只调一次；取消走 CAS（§5.3）
	FinishRunnerRun(ctx context.Context, k RunnerRunKey, r RunnerRunResult) error
	FinishRunnerRunAsCanceled(ctx context.Context, k RunnerRunKey, r CancelResult) error
}
type RunnerRunKey struct{ TaskID string; AttemptNo, Seq int } // 执行记录定位三元组（D-68）
```

## 2 Job Spec → batch/v1 Job 渲染
### 2.1 字段映射（附录 B）
| Job Spec 字段 | 渲染位置 | 规则 |
|---|---|---|
| `spec_version` | 不渲染 | 只接受 `2.0`（附录 B v2）；未知版本返回不可重试错误（映射 `VALIDATION_ERROR`），因为 Executor 是进程内组件、没有"退出码 2" |
| `job_id` / `task_id` / `attempt` / `seq` | 名称、label、env、subPath | 见 §2.3、§3.1 |
| `image` | `containers[0].image` | 原样透传，不解析 tag/digest（§5.6） |
| `command` | `containers[0].command` | **= `runner.yaml.entry`（exec form 数组）**，逐元素透传；显式写，不依赖镜像 ENTRYPOINT；平台**不追加任何动态 argv**（§5.2） |
| `args` | 不渲染 | **恒为空数组（v2）**：业务输入走输入文件 + `AG_PARAMS_FILE`（D-66）；字段保留以维持结构稳定（附录 B） |
| `env` | `containers[0].env` | 按 key 字典序；`AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` / `AG_PARAMS_FILE` / `AG_OUTPUT_DIR` + 公共参数 `AG_SERVED_*`（来源见 C-8.5）+ `TZ`、`HOME=/tmp` 由执行层强制注入/覆写 |
| `secret_env[]` | `valueFrom.secretKeyRef` | 按 control-plane 的 `secret_refs` 记录（C-2.3）映射到目标 namespace 的 Secret 名与 key；引用名不参与推导，未知引用在提交期就被拒绝。**Genet Open API Key 也走本通道**（注入 deploy/clean runner，D-72） |
| `resources.cpu/memory/gpu` | `requests` + `limits` | cpu/memory 两者相等（Guaranteed QoS，`limits_scale` 默认 1.0）；`gpu.count: 0` 不渲染，扩展资源必须 requests==limits |
| `mounts[]` | `volumeMounts` + PVC | 三种（附录 B v2）：`input`（**只读**，`attempts/<n>/input/<seq>/` → `/workspace/input`，§3.1）、`output`（RW，`attempts/<n>/runners/<seq>/` → `/workspace/output`，必须 `subPath`）、`profile_mount`（RO 且渲染期校验供给，§3.4） |
| `network.egress_allow` | 同名 NetworkPolicy | 每 Job 一条，label 与 Job 一致，随 Job 删除；deploy/clean 另含 Genet API 网段（附录 B） |
| `node_selector` / `tolerations` | Pod spec 同名 | `runner.yaml.k8s` 白名单，与 Profile 合并；平台字段不可覆盖（§6.8） |
| `security_context` | Pod + container 两级 | 默认非 root / 只读根 / drop ALL；`run_as_user`、`fs_group` 可被 Runner 覆盖 |
| `labels` / `annotations` | 元数据 | §2.3（`argus.io/*`），另加 `argus.io/managed=true` |

固定渲染项（附录 B 未列，但必须存在）：`backoffLimit: 0`、`completions/parallelism: 1`、`restartPolicy: Never`、**不设** `ttlSecondsAfterFinished`、`automountServiceAccountToken: false`（Runner 不得调 K8s API，§5.10）、`enableServiceLinks: false`、`terminationGracePeriodSeconds: 30`、`capabilities.drop: ["ALL"]`、`seccompProfile: RuntimeDefault`、`/tmp` 挂 `emptyDir{256Mi}`（配合只读根）。

### 2.2 渲染结果（输入：`runners/endpoint-probe/runner.yaml`）
Task `t-20261110-0001`、attempt 1、pre_check runner `seq=2`、`timeout_seconds: 900`。

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: argus-t-20261110-0001-1-2        # argus-<task>-<attempt>-<seq>（附录 B executor_ref 同形）
  namespace: argus
  labels: { argus.io/managed: "true", argus.io/job-id: j-9f3a1c,
            argus.io/task: t-20261110-0001, argus.io/attempt: "1",
            argus.io/runner-seq: "2", argus.io/role: pre_check }
  annotations: { argus.io/spec-hash: "sha256:5c1e9a…", argus.io/runner-version: "1.0.0",
                 argus.io/timeout-seconds: "900" }        # spec-hash 供幂等判定（§2.3）
spec:
  backoffLimit: 0
  completions: 1
  parallelism: 1
  activeDeadlineSeconds: 960                              # 900+60，仅兜底（§5.1）
  template:
    metadata:
      labels: { argus.io/managed: "true", argus.io/job-id: j-9f3a1c }
      annotations: { argus.io/runner-key: "t-20261110-0001:1:2" }
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false                 # runner 一律不绑 ServiceAccount（§12.2 / D-72）
      enableServiceLinks: false
      terminationGracePeriodSeconds: 30
      securityContext: { runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001,
                         seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: runner
          image: registry.dev.huawei.com/argus/endpoint-probe:1.0.0
          imagePullPolicy: IfNotPresent
          command: ["/runner/run.sh"]                     # = runner.yaml.entry（exec form 数组）
          # args 恒空：业务输入走 /workspace/input/params.jsonl + AG_PARAMS_FILE（D-66）
          env:
            - { name: AG_TASK_ID, value: t-20261110-0001 }
            - { name: AG_ATTEMPT, value: "1" }
            - { name: AG_RUNNER_SEQ, value: "2" }
            - { name: AG_PARAMS_FILE, value: /workspace/input/params.jsonl }
            - { name: AG_OUTPUT_DIR, value: /workspace/output }
            - { name: AG_SERVED_URL, value: "http://10.0.0.10:8000/v1" }        # 外接模式静态 service 或 deploy exports（C-8.5）
            - { name: AG_SERVED_MODEL_NAME, value: Qwen3-32B }
            - { name: TZ, value: Asia/Shanghai }
            - { name: HOME, value: /tmp }
            - name: ENDPOINT_API_KEY
              valueFrom: { secretKeyRef: { name: argus-secret-endpoint-prod-qwen, key: api_key } }
          resources: { requests: { cpu: "1", memory: 256Mi }, limits: { cpu: "1", memory: 256Mi } }
          securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                             capabilities: { drop: ["ALL"] } }
          volumeMounts:
            - { name: input, mountPath: /workspace/input, readOnly: true,
                subPath: t-20261110-0001/attempts/1/input/2 }
            - { name: output, mountPath: /workspace/output,
                subPath: t-20261110-0001/attempts/1/runners/2 }
            - { name: tmp, mountPath: /tmp }
      volumes:
        - { name: input, persistentVolumeClaim: { claimName: argus-storage } }     # RWX，只读 subPath
        - { name: output, persistentVolumeClaim: { claimName: argus-storage } }    # RWX
        - { name: tmp, emptyDir: { sizeLimit: 256Mi } }
```

### 2.3 命名、label、annotation、幂等
| 项 | 规则 |
|---|---|
| Job/Pod 名 | `argus-<task_id>-<attempt>-<seq>`（如 `argus-t-0007-1-3`，与附录 B 的 `executor_ref.job` 一致）；DNS-1123、总长 ≤ 63（超长截断 task 段并保留 hash 后缀防撞）；Pod 名由 Job controller 生成，平台不改 |
| `job_id` | `j-` + `sha256(task_id + "/" + attempt + "/" + seq)[:8]`：**不透明幂等键**，幂等键 = `(task_id, attempt_no, seq)` 的派生（附录 B / D-68） |
| 必带 label | `argus.io/{managed,job-id,task,attempt,runner-seq,role}`；值 ≤63 字符且匹配 `[a-z0-9A-Z]([-_.a-z0-9A-Z]*[a-z0-9A-Z])?`，渲染前再校验一次 |
| 必带 annotation | `spec-hash`、`runner-version`、`timeout-seconds`、`requested-by`；取消后补 `canceled-by`、`reason` |
| `ownerReference` | **不设**：控制面不是 K8s 对象，没有可作 owner 的父资源；级联删除靠 label 扫描 + 平台 GC（§6.3）。Pod 的 owner 由 Job controller 自动设置。**v2 硬规则（D-72 / §9.3）：任何组件不得对 Genet 管理的资源设置指向 runner Job 的 ownerReferences**——Job 回收的级联删除会杀掉活着的被测服务（runner 进程生命周期 ≠ 资源使用生命周期） |

**幂等：** 名称由 `(task_id, attempt_no, seq)` 唯一决定，`job_id` 与它同源派生，因此天然幂等。流程 = `GET` → 比对 `spec-hash`：相同则直接返回既有 `JobRef`（不创建、不报错）；不同则 `CONFLICT`（409）；不存在则 `Create`，并发 `AlreadyExists` 回落 `GET`。不用 `GenerateName`、不"删了重建"——重建会让日志与退出码错位。

### 2.4 为什么不设这几个字段
| 决策 | 理由 | 代价（已接受） |
|---|---|---|
| `backoffLimit: 0` | 重试是 **Task 级**语义（新 attempt 全量重跑，§7.8），不存在 runner 级重试；K8s 重拉会让"一个执行记录一个 exit code"失效，产物被覆盖 | Pod 失败即 Job 失败，由平台判 |
| `restartPolicy: Never` | 同上：容器重启会续写同一 stdout、覆盖 `result.json`，执行记录边界丢失 | 崩溃即失败，不做进程内自愈（抖动自愈归 entry 内部，§5.3） |
| 不设 `ttlSecondsAfterFinished` | TTL 在结束后删 Job/Pod，而平台此时还要读 `containerStatuses` 判 `OOMKilled`/`IMAGE_PULL_FAILED`、补 `pods.json`、等日志 follower 收尾 | 自己 GC（§6.3），时机可控 |
| 镜像拉取 | `imagePullPolicy`：digest 或非 `:latest` tag → `IfNotPresent`，`:latest`/无 tag → `Always`。`imagePullSecrets` 附录 B 未定义，默认取 Profile 的 `image_pull_secrets`，为空不渲染（E-Q3） | — |

## 3 挂载与目录
### 3.1 subPath（v2 路径：attempts/<n>/input/<seq> 与 attempts/<n>/runners/<seq>）

两个固定挂载源（§6.8 / D-68）：

| mounts[].type | storage_path（PVC 内相对路径） | 容器路径 | 权限 |
|---|---|---|---|
| `input` | `<storage-root>/<task_id>/attempts/<n>/input/<seq>/` | `/workspace/input` | **只读**（平台预写 `params.jsonl` / `params.yaml`，runner 不得写入） |
| `output` | `<storage-root>/<task_id>/attempts/<n>/runners/<seq>/` | `/workspace/output` | 读写（**就是最终落盘位置**，平台不搬运字节；`exports.env` 由平台在成功后读取收割） |

挂载时必须换算成 PVC 内相对路径，**不得**整体挂 `<storage-root>`。

```go
// rel(storage_root, storage_path) → subPath；越界或不合规直接拒绝渲染，不"尽力纠正"。
rel, err := filepath.Rel(root, filepath.Clean(storagePath))
if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) { return "", errBadPath }
// input  必须匹配 ^<task>/attempts/<n>/input/<seq>$
// output 必须匹配 ^<task>/attempts/<n>/runners/<seq>$   —— 禁止 `..`、空白、控制字符；task/seq 由平台生成并做路径安全校验
subPath = filepath.ToSlash(rel)
```

PVC 名统一 `argus-storage`（D-41；Profile 的 `output_pvc` 仅允许覆盖为登记过的别名）。`profile_mount` 可带 Profile 侧配置的 `sub_path` 前缀，但不得指向另一个 Task 的目录。**attempt 之间互不可见**：重试写新的 `attempts/<n+1>/`，历史 attempt 产物不被覆盖（§6.8 第 4 条）。

### 3.2 目录预创建
| 项 | 默认 |
|---|---|
| 谁建 | `controller` 的 `storage.PrepareRunnerRun`：控制面 Pod 以同一 RWX PVC 挂到 `/storage`，只做 `mkdir`/`chown` 与**输入落盘**（写 `input/<seq>/params.<ext>`），不写其他业务文件 |
| 建哪些 | `attempts/<n>/`、`input/<seq>/`、`exports/`、`runners/<seq>/`、`runners/<seq>/logs/`、`runners/<seq>/raw/`（后两者是 Runner 与 collector 的写入点，提前建避免 UID 竞态） |
| 属主/权限 | `10001:10001`、`0770`；父目录同属主；`input/<seq>/` 落盘后置只读位（挂载侧再强制 `readOnly`） |
| chown 失败 | 只记 warning 继续：交给 `fsGroup: 10001` 与 Runner 自己 `mkdir -p` 兜底（§6.8 第 3 条要求两边都成立） |
| 为什么不用 initContainer | initContainer 与主容器共享同一 `subPath`，chown 不到父目录；平台侧预创建还能在建 Job 前失败 |

### 3.3 只读根文件系统与 `/workspace/output`
根文件系统只读（§12.2），可写面仅三处：`/workspace/output`（PVC subPath）、`/tmp`（emptyDir）、Profile 挂载（只读）。`/workspace/input` 只读挂载（平台预写输入文件，runner 不得写入，§6.8）。`HOME=/tmp`：pip / python / jq 缓存会写 `$HOME`，否则只读根会变成"随机失败"。Runner 可写 `logs/`、`raw/`，与 collector 的 `logs/stdout.log`、`logs/stderr.log` 不冲突——**collector 只拥有这两个文件名**。

### 3.4 Profile 挂载校验与 `MOUNT_UNAVAILABLE`
渲染期（不是运行期）逐项检查 `runner.yaml.mounts`：Profile 存在同名供给（`required: false` 缺失则跳过并记 `mount_skipped`）；`path` 为绝对路径且不与平台保留路径（`/workspace/output`、`/workspace/input`、`/tmp`、`/runner`）冲突；挂载点之间不得前缀重叠；PVC 在目标 namespace 存在（一次 `GET`）。任一失败 → `MOUNT_UNAVAILABLE`（不可重试），**必须在创建 Job 之前暴露**，不能等 Pod 卡在 `ContainerCreating`。

### 3.5 NFS 权限风险
| 风险 | 默认应对 |
|---|---|
| 服务端 `root_squash` 使 `chown` 无效 | chown 只当优化；`fsGroup: 10001` + `02770`（setgid）保证组继承；Runner 以 10001 运行 |
| `fsGroup` 在部分 CSI 组合上不生效 | 目录预创建成为主路径；Runner 侧 `mkdir -p` 仍必须能工作 |
| UID 不统一（Runner 覆盖 `run_as_user`） | 目录属主取该执行记录的实际 `run_as_user`，取不到用 10001（E-Q5） |
| 多节点时钟漂移 | 日志时间戳一律取 apiserver 返回的时间，不用节点本地时钟 |
| stale NFS handle | `PrepareRunnerRun` 失败 → `EXECUTOR_UNAVAILABLE`（可重试），不创建 Job |

## 4 生命周期观测
### 4.1 informer 为主
- **每集群一个** `SharedInformerFactory`（按 cluster + namespace 复用），watch `batch/v1 Jobs` 与 `core/v1 Pods`。
- **list/watch 都带 label 过滤** `argus.io/managed=true`；Pod 侧再按 `argus.io/job-id` 二级过滤，不依赖 controller 注入的 `job-name`。
- handler 不写库、不阻塞：只更新内存 `statusCache` 并经有界 channel（1024，满则丢弃并置脏）通知 reconciler。
- **`HasSynced` 之前不做任何判定**，启动时先等同步再用 `Reconcile` 补齐，避免"刚重启就把在跑的 Job 判失败"（§12.5）；`resyncPeriod = 5m` 只用于对齐，不触发业务动作，410 Gone 与重连由 Reflector 负责。

### 4.2 周期对账兜底
每 30s 取 control-plane 的"非终态 runner 执行记录"集合与 cache 比对：cache 缺项 → 直接 `GET job`，404 进入 §4.4 复核；cache 有而 DB 已终态 → 只清 cache，不动状态。单轮上限 500 条、单次 5s 超时，失败不阻塞下一轮。

### 4.3 映射表
| 观测 | phase | 错误码 |
|---|---|---|
| Job `Complete=True` | `succeeded` | —（还要过 §8） |
| Job `Failed=True`，`reason=DeadlineExceeded` | `timeout` | `RUNNER_TIMEOUT` |
| Job `Failed=True`，`reason=BackoffLimitExceeded` | `failed` | 按 Pod 终止原因归类 |
| Pod `Succeeded`，`exitCode==0` | `succeeded` | — |
| Pod `Failed`，`terminated.exitCode != 0` | `failed` | `RUNNER_NONZERO_EXIT` |
| `terminated.reason=OOMKilled` / `Evicted` | `failed` | `INTERNAL_ERROR` + `detail.k8s_reason`（§4.5） |
| `waiting.reason ∈ {ErrImagePull, ImagePullBackOff, InvalidImageName, ErrImageNeverPull}` | `failed` | `IMAGE_PULL_FAILED` |
| `PodScheduled=False/Unschedulable` 且 message 含 `volume`/`mount`/`FailedMount` | `failed` | `MOUNT_UNAVAILABLE` |
| 事件 `FailedMount` / `FailedAttachVolume` / `MountVolume.SetUp failed` | `failed` | `MOUNT_UNAVAILABLE` |
| Pod `Pending`（`Unschedulable` / `ContainerCreating` 等，**不判失败**） | `pending` | — → 记 `waiting_reason`（§11） |
| Pod `phase=Unknown` / Job 404 且无取消标记 / 其余 | `unknown` | 复核（§4.4） |

判定顺序固定：先 Job 终态，再 Pod；矛盾时以更细的 Pod 终止原因为准并记 `detail.conflict=true`。`exit_code` 仅在单容器、单次终止时返回。

### 4.3.1 镜像事实采集（image digest）

Pod 首次 `Running` 与终态各读一次 `containerStatuses[].imageID`（形如 `docker-pullable://repo@sha256:…`），提取 `sha256:` 后的 64 位写入 `task_runners.image_digest`。它与提交的 `image_ref` 分开存：引用是"要什么"，digest 是"实际拉到什么"。

- 用途：回答"同一个 tag 当时到底跑的是哪份代码"；P1 续跑复用优先比较 digest（IMPLEMENTATION G-16）。
- 采集失败（字段缺失/格式不符）只记 `detail.image_digest=unknown` 的 warning，不重试、不影响状态。
- 不解析 tag、不做 digest 校验（§5.6 平台不解析镜像引用）；只记录 K8s 给出的事实。

### 4.4 `unknown` 的复核（不得直接判失败）
第 0 次 `GET Job`（已终态直接出结论）→ +5s `GET Job` + `LIST` Pod → +20s 查 Pod events → +60s 查 Node 状态 → 仍无法判定才 `failed(INTERNAL_ERROR)`，`detail.review=exhausted` 并附全部原始观测。复核期间 runner 保持 `running` 并发 `executor_observation_unknown` 事件（UI 显示"待确认"）；复核不重置 `activeDeadlineSeconds`，watchdog 到点仍按 `RUNNER_TIMEOUT` 收口；总耗时 ≤ 90s。

### 4.5 错误码枚举不足
`OOMKilled` / `Evicted` / `Preempted` / 节点 NotReady 在附录 D 没有专门码。原则：**不新增枚举**（§15 要求先改文档），先归语义最近的码（资源类 `INTERNAL_ERROR`、挂载 `MOUNT_UNAVAILABLE`、镜像 `IMAGE_PULL_FAILED`）；**原始原因永不丢**（`reason`、`detail.k8s_reason`、`detail.exit_code` 一并落库）；这类码不参与重试自动化判断（v2 无 runner 级重试；`retryable` 仅用于 §9.3 重放与告警分级，附录 D）。E-Q2 建议下一版增加 `CONTAINER_OOM_KILLED` / `POD_EVICTED`（可重试）/ `NODE_LOST`。

## 5 超时与取消
### 5.1 两层超时，平台权威
`activeDeadlineSeconds = timeout_seconds + 60` 只保证"不会无限占资源"，60s 缓冲避免与平台判定同时触发、日志被抢断。**权威是平台 watchdog**：每 10s 扫 `running`（以及超过 10 分钟仍 `preparing` 的）执行记录，`now - started_at > timeout_seconds` → 终态 `FAILED(RUNNER_TIMEOUT)`。service-deploy 的部署总窗口同理（时限 = `AG_DEPLOY_TIMEOUT`，默认 72h，超窗 `DEPLOY_BLOCKED`）。顺序恒定：先落库终态，再 `Cancel`——Job 自己到点只是被 K8s 杀掉，写不了 `RUNNER_TIMEOUT`，也不会收尾 `pods.json` 与日志流。渲染期校验 `timeout_seconds` ∈ (0, 86400]（§12.7）。

### 5.2 取消 = 置标记 + 删 Job + 等 Pod 消失
1. control-plane 置 `cancel_requested_at` 并记 `canceled-by`。
2. `delete job`，`propagation=Background`，`gracePeriod=30`（Pod 由 Job controller 异步删，不阻塞 API）。
3. 每 2s 查 Pod（按 `job-id` label），最长 60s；未消失 → `delete pod`，`propagation=Foreground`，grace 0（必须确认删掉，否则仍有进程写 NFS）。
4. 再过 30s 仍残留 → `CLEANUP_FAILED` 事件 + 告警，交 GC，**不悬挂在 `CANCELLING`**。

### 5.3 与自然结束的竞态：CAS，先落库者胜
```sql
UPDATE task_runners SET status = $new, error = $err, finished_at = now()
 WHERE task_id = $task AND attempt_no = $att AND seq = $seq
   AND status NOT IN ('succeeded','failed','canceled','skipped');
```

返回 1 行 = 生效；返回 0 行 = 已终态，只记 `state_race_lost` 事件（带双方候选状态），不覆盖、不报错（§6.6）。因此一个已被取消的执行记录随后收到 `succeeded`，只补 `exit_code` 等执行事实，**不改状态**。watchdog 与 informer 应用结论前都要重读 DB，不能凭 cache 直接写。

### 5.4 清理失败
结果不变（§6.6），只发 `CLEANUP_FAILED` + `cleanup_failures_total`；进重试队列 1m/5m/15m 最多 5 次，之后交 GC。执行层本就不删产物目录，不存在"目录清理失败"。

## 6 清理与回收
| 路径 | Job | Pod | 产物目录 | 日志 |
|---|---|---|---|---|
| 正常结束（succeeded/failed） | 删，`Background` | 随 Job 消失 | **保留**（失败也保留，§6.8 第 5 条） | 保留，follower 读到 EOF 关闭 |
| 取消 | §5.2 全流程 | 必要时 `Foreground` + grace 0 | **保留** | 保留，写 `--- [canceled at …] ---` |
| 超时 | 同取消 | 同取消 | **保留** | 保留，写 `--- [deadline exceeded at …] ---` |

删除时机在**结论落库成功之后**：落库失败就先不删 Job，让对账重来——多留一个 Job 比丢一份结论便宜。**v2 无平台 `CLEANUP` 阶段（D-70）**：拆除由 service-clean runner 承载（finally，见 C-5.7）；执行层在 Task 收尾期只做两件事：停该 Task 的部署 Pod 日志采集（clean 完成或 Task 终态，§8.6）、让 GC 可回收 Job；产物归档/删除由 control-plane 的保留任务负责（`AG_RETENTION_DAYS`），**执行层不删任何产物目录**。

**孤儿 GC（每 10 分钟，纯 label 驱动）：**

| 条件 | 动作 |
|---|---|
| `managed=true` 但 DB 无对应执行记录 | 立即删 Job |
| 执行记录已终态且 Job 创建 > 1h | 删 Job（正常路径没删干净） |
| 运行中且 `now - creationTimestamp > 25h` | 删 Job + 经 `ResultSink` 修状态为 `FAILED(RUNNER_TIMEOUT)` |
| annotation `argus.io/hold-until` 未过期 | **跳过**（日志 follower 未收尾） |

GC 只删 Job/Pod/NetworkPolicy/Lease，不碰 NFS；`argus-` 前缀但 label 不符的一律不动（可能是人工调试）。**GC 不可能级联删除 Genet 管理的资源**：deploy/clean 对 Genet 资源无 ownerReferences（§2.3、D-72）。

## 7 日志采集
### 7.1 Runner stdout/stderr
数据源 `pods/log?container=runner&follow=true&timestamps=true`。**游标**用服务端时间戳做 `sinceTime`，落盘前剥离前缀（Runner 看到干净日志），游标存内存并随 64KiB flush 记 checkpoint。**断线重连**：`sinceTime=lastTS`（不减容差），重连后写 `--- [log resumed at …] ---`，2s 内完全相同的行去重（窗口 200 行）。**落盘**：`attempts/<n>/runners/<seq>/logs/stdout.log`、`logs/stderr.log` 分离，`bufio` 4KiB + 1s flush，不用 `O_SYNC`（NFS 代价过高），丢失窗口 ≤1s。**结束**：EOF → 写 `--- [container exited at …, exit code N] ---` 后关闭。

### 7.2 部署 Pod 日志（Genet 渲染的服务 Pod；v2 落盘位置移入 deploy runner 产物目录，D-71）
| 项 | 默认 |
|---|---|
| 发现 | 用 **deploy-state.json 的 `label_selector`**（service-deploy 周期写入）`LIST pods`，每 30s 重新发现；新增自动跟随，消失的写 `--- [pod terminated at …] ---`。平台不直连 Genet，Pod 清单快照同样读 deploy-state.json 的 `pods[]`（§9.5） |
| 一 Pod 一文件 | `attempts/<n>/runners/<deploy_seq>/logs/<pod>.log`；多容器 Pod 用 `<pod>.<container>.log`，默认 `server` |
| 重启 | 监视 `restartCount` 增加 → `--- [container restarted #3 at 06:44:11, previous exit: OOMKilled] ---`，**续写同一文件** |
| 分段 | 单文件 > 200MB 滚到 `<pod>.1.log` / `<pod>.2.log`；`pods.json`（同目录）记每段 `{file, start, end, restarts, truncated}` |
| gap | `--- [log gap: 06:52:10 – 06:55:40, collector restarted] ---`；**宁可标缺口也不补**（§8.6） |
| 停止 | **clean 完成或 Task 终态** → `--- [collection stopped: clean finished at …] ---` 并关闭；**不是**测试一结束就停（采集范围：deploy runner 成功起，到 clean 完成或 Task 终态止，§8.6） |
| `pods.json` | 每 30s 原子写（temp + rename）：Pod 名、角色、节点、重启次数、就绪、日志区间 |

### 7.3 选主与故障语义
全局 Lease `argus-collector-leader`（TTL 15s / 5s 续约）负责分配归属、写 `pods.json`、清理死 follower；**每个 Job 一个** Lease `arg-log-<jobhash>`（TTL 30s / 10s 续约），持有者才能写该执行记录的 stdout/stderr。Lease 过期后新持有者从 checkpoint 续采并写 gap 标记；旧持有者恢复后发现 Lease 被占即停写。同一文件同一时刻只有一个写者（`O_APPEND` + Lease）。

允许丢日志，**不允许伪造连续**：崩溃、apiserver 不可达、NFS 短时不可写，一律恢复后写 gap 标记（统一格式 `--- [<event>: <detail> at <RFC3339>] ---`，UI 可正则识别）。埋点统一 `collector_` 前缀：`collector_log_lines_total`、`collector_log_bytes_total`、`collector_gaps_total{reason}`、`collector_log_lease_lost_total`（与 web-and-delivery §12 一致）。

**日志明文，不脱敏**（团队决策 K-5）：平台不读 Secret 值、不做替换，Runner 与 Deploy 日志原样落盘；`redactions_total` 指标取消。Runner 契约里"不得主动打印凭据"保留，但平台不再兜底。§12.4 已按 v0.3 修订（IMPLEMENTATION G-9）。

## 8 产物校验与配额
### 8.1 时机与判定（含 exports 收割）

| 条件 | 结论 |
|---|---|
| `exit != 0` | 先尝试读 `result.json`：存在且合法、`error.code` 属于附录 D → 用该码与该实例的 `retryable`；否则回退 `RUNNER_NONZERO_EXIT`（D-74：失败时 result.json 是软要求，缺失不额外加罚）。runner 一律 `failed`，`raw/` 保留；**不收割 exports**（§5.12） |
| `exit == 0`，`result.json` / `result.html` / `result.csv` **全部缺失或全部 0 字节** | **`RESULT_MISSING`**（D-74 重定义：三选一全缺才算；test / pre_check 角色） |
| `exit == 0`，任一产物存在，但 `result.json` **存在且** JSON 非法 / schema 不过 | `RESULT_INVALID`（`detail.path` 到字段级；**仅适用**于 result.json 已产出的情形，D-74） |
| `exit == 0`，仅产出 `result.html` / `result.csv`（无 result.json） | `succeeded`（产物契约满足，D-74）；平台不解析内容，只进产物索引（`kind=result`）；报告页以产物预览/下载为主 |
| `exit == 0`，产物合格但 `exports.env` **词法不合格**（非 `KEY=VALUE` 行、值含换行） | **`EXPORT_INVALID`**（v2 新增，附录 D）：解析失败的行整体拒绝收割，runner 判 `failed`（C-5.4） |
| `exit == 0`，产物与 exports 都合格 | `succeeded`；收割 exports → 落库落档 `attempts/<n>/exports/<seq>.exports.env` → 合并注入后续 runner（C-8.5） |

> 非零退出仍读结果，是为了让 pre_check 能区分"未就绪（可重试）"与"4xx/认证失败（不可重试）"（§5.11）：Runner 在失败时按 §5.9 写 `error.code`（`ENDPOINT_UNREACHABLE` / `PRECHECK_FAILED`）与 `retryable`，平台只接受附录 D 枚举，读不到就回退通用码。`result.json` 只用于**归类错误**，不改变"非零退出 = runner 失败"这一事实。deploy / clean 角色无 `result.json` 契约（产出 deploy-state.json / 退出码），校验器按 role 分派。

校验发生在落库**之前**，与结论同一次 `FinishRunnerRun` 提交，避免"先 SUCCEEDED 再补错误码"。

### 8.2 schema 校验范围（附录 A / `schemas/result.schema.json`；仅当 `result.json` 产出时执行本表，D-74）
| 检查 | 级别 |
|---|---|
| `schema_version=="2.0"`、`task_id`、`attempt_no`、`runner_seq`、`runner{}`、`metrics` 存在 | 必须（附录 A v2 明示） |
| `runner.name`/`version` 与注册表一致；`task_id`/`attempt_no`/`runner_seq` 与本次执行记录一致 | 必须 |
| `status ∈ {succeeded, failed}`；`started_at`/`finished_at` 可解析且 `finished >= started` | 必须 |
| 每个 metric 有 `name`/`value`/`unit`/`statistic`，`statistic` ∈ 9 值 | 必须（§5.9） |
| 同 `labels` 下 `name+statistic` 唯一 | 必须 |
| `metrics` 为空时 `notes` 非空；`status=failed` 时 `error` 非空 | 建议（warning，不判失败） |
| 未知字段 **允许**（前向兼容），顶层未知 key 记 `detail.extra_keys` | — |
| 文件 ≤ `AG_RESULT_MAX_BYTES`（默认 100MiB，仅防 OOM 的技术上限） | 必须（超限 `RESULT_INVALID`，`detail=too_large`；K-10 不设业务限制） |

### 8.3 配额（单 attempt 10 GiB）
collector 每 60s 对 `attempts/<n>/` 全量 walk（按 inode 去重硬链接，单次 < 50ms 量级），超 `artifact_quota_bytes`（默认 10 GiB）→ 复用取消路径删 Job → 终态 `FAILED(ARTIFACT_QUOTA_EXCEEDED)`，**不可重试**。结束后再算一次精确值，已结束但超限同样改判（在落库前完成，不产生两次状态转移）。walk 失败（NFS 超时）只记 `quota_check_skipped`，不判失败。

### 8.4 sha256：惰性，默认不算
`artifacts.hash` 默认 `lazy`：不在收集阶段计算——大文件在 NFS 顺序读一遍可能比测试本身还慢，而唯一消费方（下载校验、续跑去重）都不在热路径。可选 `eager`（异步算，>1GiB 跳过并置 `null`）与 `off`。算一次即缓存，缓存键 `mtime + size`。

### 8.5 索引调用契约
```go
type ArtifactRecord struct {
	Path      string // 相对 attempts/<n>/，禁止绝对路径与 ..
	SizeBytes int64
	SHA256    *string // lazy 模式为 nil
	Kind      string  // result | stdout | stderr | runner_log | raw | other
	MTime     time.Time
}
// Candidates 幂等（按 Path 排序）；不索引目录、.tmp、>2GiB 文件（仍记 size）。
func (v *Validator) Candidates(ctx context.Context, k RunnerRunKey) ([]ArtifactRecord, error)
```

control-plane 据此写 `artifacts(task_id, attempt_no, seq, path, size_bytes, sha256, kind, mtime, indexed_at)`，`(task_id, path)` 唯一。`result.json.artifacts[]` 与磁盘取交集：声明了但没有文件 → warning（`detail.dangling_artifact`），不判失败。

## 9 内置 deploy/clean runner 与 Genet 交互契约（v2 重写，D-70）

> **职责重划**：v1 的"平台 Genet 适配层（`ports.Deployer` + `internal/genet` HTTP 客户端 + 平台轮询）"**取消**。与 Genet 的全部交互由**内置 service-deploy / service-clean runner** 执行（与 test / pre_check 共用同一套执行契约：K8s Job、`/workspace/input` 只读、`/workspace/output` 读写、`AG_*` env、退出码语义）；平台不再直连 Genet，只读取 runner 产出的 **`deploy-state.json`** 与 exports。原 `server/internal/genet` 适配层**瘦化为 `internal/deploystate`**（deploy-state.json 解析 + 归档 + 投影，不发任何 HTTP）。
>
> 本节因此从"平台如何调 Genet"改写为"**内置 runner 如何与 Genet 交互（契约）+ 平台如何消费其产物**"。Genet 后端事实的调研证据（`docs/research/genet-lifecycle.md`，下称 `R-G:行`；`<G>` = 上游 Genet 仓库 `<G>/path:行号`）全部保留，它们现在是 **runner 实现者**的依据。genet-local（假 Genet）继续作为本地联调后端（§9.7）。

### 9.1 职责与边界（v2）

| 主体 | 职责 | 明确不做 |
|---|---|---|
| service-deploy（`runners/service-deploy/`，role: deploy，仅内置） | 读 `params.yaml`（Genet 部署描述）；调 Genet 安装 release；轮询状态并**周期改写 `deploy-state.json`**；ready 后导出四件套 | 不做 Helm 渲染；不解析部署描述语义；不直连 K8s API（一切经 Genet，§12.2） |
| service-clean（`runners/service-clean/`，role: clean，仅内置） | 消费 `AG_DEPLOY_REF` + deploy-state 副本；调 Genet 卸载；判干净；幂等 | 同上；不改变 Task 终态（失败只打 `cleanup_failed`） |
| 平台（controller / `internal/deploystate`） | 读 runner 输出目录的 deploy-state.json → 结构校验 → 归档 `attempts/<n>/deploy-state.json` → UI 投影与 `deploy.state_changed` 事件 | 不发 Genet HTTP；不解释 `genet_status` 原值；不重试部署（fresh-deploy 只经整 Task 重试，§7.8） |
| genet-local（假 Genet） | 本地联调后端：实现与真实 Genet 相同形状的 OpenAPI（install/get/pods/uninstall/queue） | 不模拟排队时长之外的故障注入细节（E2E 用 httptest 注入 401/5xx） |

内置 runner 的注册边界：随平台镜像发版（`AG_RUNNERS_DIR` 扫描），用户目录声明 `role: deploy` / `role: clean` 一律拒绝注册（§5.1）；`on_failure` 必须 `abort_task`（§7.3）；不绑 ServiceAccount，Genet Open API Key 走 Task 级 `secret_env`（§12.2 / D-72）。

### 9.2 Genet 事实基线（runner 实现依据，调研证据保留）

| 事实 | 对 runner 的约束 | 证据 |
|---|---|---|
| 一次部署 = 一次 Helm release；定位符 `namespace + releaseName` | runner 用 `(namespace, release_name)` 作为 deploy-state.json 的定位键，写入 `AG_DEPLOY_REF` | `R-G:11`、`R-G:94` |
| `namespace` 由 API Key 绑定，请求体不能指定；Key 未绑定 → 403 | Key 的绑定值写进 deploy-state.json（`namespace`），clean 依赖它 | `<G>/backend/internal/handlers/openapi_helm.go:39-56`；`R-G:75` |
| `releaseName` 由调用方自造，正则 `^[a-z][a-z0-9-]{0,52}$`，Genet 不加 namespace 前缀 | runner 派生确定性 releaseName：`arg- + sha256(task_id:attempt_no)[:12]`（**D-44，v1 前缀 `tp-` 废止**）；同 Task 同 attempt 恒等、跨 attempt 必不同——泄漏重放（§9.6）靠它 | `<G>/backend/internal/handlers/helm.go:36`、`:2696-2698`；`R-G:74` |
| 拉起异步：202 后后台 goroutine 跑 `helm upgrade --install`，无作业号 | runner 只能凭自起 releaseName 轮询；POST 超时不代表没创建（先落 deploy-state 再轮询） | `helm.go:2981-2990`；`R-G:70`、`R-G:320` |
| 同名再 POST → 400（`already has protected metadata`），不是幂等成功 | runner 恢复流程：先 `GET /releases/{name}`，200 → 直接轮询；404 且未过宽限 → 等待不重发 POST | `helm.go:2700-2718`；`R-G:320` |
| 排队发生在 Volcano/PodGroup 层，Genet 只给水位计数、无位次 | deploy-state.json 不存位次（`queue{phase,pending,inqueue,running}`）；`queue_full/gang_pending/insufficient_resources` 平台从 CRD 读（§11） | `R-G:98-106`、`R-G:294` |
| 鉴权 `Authorization: Bearer <Open API Key>`（scope read/write）；无限流 | runner 5s connect / 10s 总超时；`values` 控在 100KB 内并写 digest 进 deploy-state | `<G>/backend/internal/auth/apikey.go:37-49`；`R-G:239-249` |
| `values` / `templatePath` / `templateId` 三者不能全空（本地实测 400） | 平台提交校验不拦（语义归 runner），runner 在参数预检期发现全空 → 退出码 2（`DEPLOY_FAILED`，detail 记 Genet 原文） | §9.2 旧实测；`R-G` |
| `package_type` 决定装哪个固定 chart（`stack` / `pd`） | `deploy` 描述里的包类型原样透传；本地/CI 一律 `stack`（D-53） | `<G>/helm.go`；实测 202→deployed→ready |
| **`noWait=true` 恒真**：`timeoutSec` 是 helm `--wait` deadline，排队超时会误判 failed | runner 恒传 `noWait=true`，就绪由自己的轮询判定；部署窗口由平台超时（`AG_DEPLOY_TIMEOUT`，默认 72h）兜底 | `R-G:194-196`、`R-G:299` |

### 9.3 service-deploy 行为契约（deploy-state.json 是唯一状态出口）

**输入**：`/workspace/input/params.yaml`（`input_format: yaml`，平台不解析语义）；env：`AG_TASK_ID` / `AG_ATTEMPT` / `AG_RUNNER_SEQ` / `AG_OUTPUT_DIR` + `secret_env`（Genet Open API Key、genet-local 地址等）。

**行为序列**：

1. 派生 `release_name = "arg-" + hex(sha256(task_id + ":" + attempt_no))[:12]`；
2. `GET /api/open/helm/releases/{name}`：200 → 进入轮询（恢复场景）；404 → `POST /api/open/helm/uc-packages/{stack|pd}/releases`（`noWait=true`）；POST 400 且文案含 `already has protected metadata` → 视为已存在，回到 GET；
3. **轮询**（单飞；前 30s 每 1s，之后 5s 起指数退避上限 15s；单请求 10s；404 宽限 `accepted_at + 180s` 按时间窗）每次改写 `$AG_OUTPUT_DIR/deploy-state.json`（temp + rename，≈30s 一份保底）：

```json
{
  "release_name": "arg-9f3a1c2b7d40", "namespace": "<key 绑定值>",
  "genet_status": "pending-install", "endpoint_status": "pending",
  "phase": "queued",                       // queued | accepted | deploying | ready | blocked（五值，D-46/D-47 沿用）
  "reason": "<PodGroup condition 原文>", "waited_seconds": 4210,
  "label_selector": "helm-release-name=arg-9f3a1c2b7d40",
  "pods": [{ "name": "...", "role": "engine", "node": "...", "restarts": 0, "ready": false }],
  "queue": { "phase": "Pending", "pending": 3, "inqueue": 1, "running": 2 },   // 无位次（D-46）
  "exports": null,
  "failure": null
}
```

4. 状态合成沿用 v1 调研结论（两根轴：helm `release.status` × `callInfo.endpoint.status`；`deployed` + `endpoint.status=pending` **不可**当 ready；`failed/superseded/uninstalling/uninstalled` 折叠为 `blocked`——平台五值没有 deploy `failed` 子态，D-47）；
5. **ready**：`exports = {AG_SERVED_URL, AG_SERVED_MODEL_NAME, AG_MODEL_PATH, AG_DEPLOY_REF(=release_name+namespace)}` 写进 deploy-state 并同时写 `$AG_OUTPUT_DIR/exports.env`，退出 **0**；
6. **失败**：`failure = {code: DEPLOY_FAILED | DEPLOY_BLOCKED, reason, message}` 写进 deploy-state，退出 **1**（blocked / 不可用类）或 **2**（参数/凭据类错误）；平台按 §7.5 映射 Task 终态（`DEPLOY_FAILED` → `failed`；`DEPLOY_BLOCKED` → `service_unavailable`）。

**超窗**：runner 自己不再判超时——部署窗口是 runner 的执行时限（`AG_DEPLOY_TIMEOUT` 默认 72h），由平台 watchdog 终止（`RUNNER_TIMEOUT`）并按 deploy-state 最近快照归类 `DEPLOY_BLOCKED(detail.reason=timeout)`（C-5.3）。

### 9.4 service-clean 行为契约（finally 语义，§7.6 / §9.1）

1. 消费 `AG_DEPLOY_REF`（exports）与 `attempts/<n>/deploy-state.json` 副本（挂进 `/workspace/input` 或经 env 给路径）；
2. 调 `DELETE /api/open/helm/releases/{name}?keepHistory=false&force=true`——**只能**走 Genet OpenAPI，直接 `helm uninstall` 会被 admission webhook 拒绝（D-54）；`AG_DEPLOY_REF` 缺失（deploy 未导出即被取消/失败）时用确定性 releaseName 兜底，`GET 404` = 幂等成功；
3. **判干净 = `GET /releases/{name}` 404 且 `GET /releases/{name}/pods` 的 `items == []`**（两个条件同时满足、查询本身成功；DELETE 的 200 不算数，`R-G:218-221`）；
4. 常规 DELETE → 5 分钟未确认 → `force=true` 重试 → 再确认 2 分钟；仍不干净退出 **1**（`CLEANUP_FAILED` 语义）；
5. **finally**：成功 / 失败 / 取消 / 超时四条路径都会被排程执行（C-5.7）；资源不存在 = 成功；失败不改 Task 终态，进 §9.6 重放清单。

### 9.5 deploy-state.json 的平台消费（`internal/deploystate`）

```go
// internal/deploystate：解析 + 归档 + 投影；不发 HTTP（v1 internal/genet 客户端删除）。
type DeployState struct {
    ReleaseName, Namespace        string
    GenetStatus, EndpointStatus   string   // 原值保留：排障要能区分"Genet 说 deployed、我们说 deploying"
    Phase                         string   // queued|accepted|deploying|ready|blocked
    Reason                        string
    WaitedSeconds                 int
    LabelSelector                 string   // 部署 Pod 日志采集（E-7.2）与 pods.json 的发现依据
    Pods                          []PodRef
    Queue                         QueueWatermark   // 无位次
    Exports                       map[string]string
    Failure                       *DeployFailure
}
func Parse(b []byte) (*DeployState, error)          // 词法/结构校验；坏文件不判 runner 失败，下轮重读
func (s *Store) ArchiveDeployState(ctx, taskID string, attemptNo int, b []byte) error // 归档 attempts/<n>/deploy-state.json
```

- controller 在 deploy runner `running` 期间每 ≈30s 读一次 runner 输出目录（NFS 直读）；成功读取即归档 + 比对 `phase` 变化发 `deploy.state_changed` 事件（水位/等待时长进 payload，无位次）；
- 部署排队可见性（§6.9）由此承载：平台 `QUEUED` 的 `waiting_reason` 只表达平台侧等待，Genet 排队在 deploy 节点详情呈现；
- `endpoint.status=ready` ≠ 可用（D-45）：服务门由管道下一个 runner（pre_check）承担（C-5.3.3）。

### 9.6 泄漏兜底：重放内置 clean（§9.3 v2 裁决）

- exports 与 deploy-state.json 按 attempt 落库落档，信息不丢；
- `argus clean --task <id> [--attempt <n>]`（API：`POST /tasks/{id}/clean?attempt=`）：用落档导出**重新拉起一次内置 service-clean Job**（正常 runner 通道，幂等键 `(task_id, attempt_no, clean_seq)`）；平台仍是哑平台，只是重放一个 Job；
- **v1.5（P1）**：周期扫描"已终态、含部署、无 clean 成功记录"的 attempt，自动重放并告警（沿用 v1 的 `ListDeployCleanupPending` 思路，判据改为"无 role=clean 的 succeeded 执行记录"）；
- **硬规则（D-72）**：任何组件不得对 Genet 管理的资源设置指向 runner Job 的 K8s ownerReferences——Job 回收的级联删除会杀掉活着的被测服务；deploy runner 在服务 ready 时退出，服务必须存活到 clean 完成或重放完成（runner 进程生命周期 ≠ 资源使用生命周期）；runner 一律不绑 ServiceAccount，Genet 凭据走 `secret_env`。

### 9.7 genet-local（本地联调后端，D-43 沿用）

- 形态：与真实 Genet 相同形状的 OpenAPI 假后端（`POST /api/open/helm/uc-packages/{type}/releases`、`GET/DELETE /api/open/helm/releases/{name}`、`/pods`、`/queue`），kind 环境里以 Pod 或宿主进程运行；
- runner 经 env 指向它（`AG_GENET_LOCAL_URL` 类 runner 级配置，不是平台配置）；E2E 注入 401 / 5xx / 排队超时验证 §9.10 风险路径；
- 真实 Genet 的接入只改 runner 的 env（地址 + Key），平台与契约零变化——这是"平台不直连 Genet"带来的解耦收益。

### 9.8 未确认与风险（原 §9.10 调研结论，责任主体改为 runner 实现者）

| # | 风险 | 影响 | 应对 |
|---|---|---|---|
| R1 | **`ready` 不等于可服务**（endpoint.status 只看地址解析，`helm.go:855-928`） | 不探活会把 404/5xx 派给测试 runner | pre_check runner 是必需管道节点（C-5.3.3）；Runner 侧 warmup 预算（§5.11） |
| R2 | **无排队位次**（只有水位计数） | UI 承诺"第 N 位"做不出来 | deploy-state 不存位次；展示"已等待时长 + 水位"（D-46） |
| R3 | **`queued/accepted/deploying` 靠推断**（Genet 无受理事件） | 状态可能"卡在 accepted"或"跳过 deploying" | deploy-state `state_detail` 写明判据；UI/排障手册标注启发式 |
| R4 | **每日 23:00 Genet GC 可能挂起长驻服务**（`cleanup/lifecycle.go`，未见 Helm release 豁免） | 长测跨夜服务消失 | 必须实测 release 跨 23:00；必要时 Genet 侧加 protected-until |
| R5 | **取消只在 DELETE 一条路上**（release 未出现 404、install 在飞 409/500） | 残留长驻 release | clean 幂等重试 + 平台对账扫描重放（§9.6） |
| R6 | 共享池限制在 `/open/helm` 是否生效存疑 | 鉴权模型不一致 | 不据此设计；以 Key 绑定 + 时间窗为准，向 Genet 团队确认 |
| R7 | `releaseName` 无 namespace 前缀 | 同名可跨 namespace 共存 | `namespace + release_name` 一起当定位键 |
| R8 | Service 靠命名约定（`-router-service` 等） | chart 改名会退化 endpoint | runner 只被动接受 Genet 返回值，禁止自行推导 |
| R9 | "已删干净"依赖两次查询时序 | 误报清理成功 → 泄漏 | 区分"查询失败"与"为空"（§9.4 第 3 条） |
| R10 | Genet 没有"服务租期"概念 | Task 结束不删就永久占卡 | clean finally + 重放兜底 + `CLEANUP_FAILED` 告警（不做平台 GC，D-72） |
| R11 | 请求体大小/限流未知 | 大 values 撞反代限制 | values ≤100KB + digest；deploy-state 记 `values_digest` |
| R12 | `GET /releases` 富化开销大 | 拖垮后端 | runner 一律 `light=true` |
| R13 | 文档与实现漂移 | 按文档实现会错 | 以代码为准；调研证据带源码行号 |
| R14 | Genet 服务保障弱（无 SLA、GC 可能挂起） | 不能当"服务编排器" | 生命周期收敛与清理兜底由 runner 契约 + 平台重放承担 |

### 9.9 被证伪/被取代的 v1 设计（回写清单）

| v1 设计 | v2 现状 | 处置 |
|---|---|---|
| 平台 Deploy 阶段 + `ports.Deployer`（Start/Poll/Cancel） | 取消：service-deploy runner 承载；平台只读 deploy-state.json | 本节重写；`internal/genet` 瘦化为 `internal/deploystate` |
| `runs.deploy` JSON 承载 release 运行态（`genet_release_status`/`queue`/`accepted_at`…） | 运行态归 `attempts/<n>/deploy-state.json` 归档副本；`tasks.deploy` 只留提交快照 | 字段全部进 deploy-state schema（§9.3）；数据视图待回写 |
| 平台轮询 Genet 的 poller（单飞/退避/404 宽限） | 移入 service-deploy runner 内部（§9.3 第 2-3 步）；平台只读文件 | 参数（退避节奏、宽限窗）不变，责任主体变更 |
| 幂等键 `release_name = tp- + sha256(run_id:deploy_id)` | `arg- + sha256(task_id:attempt_no)`（D-44 改前缀；attempt 级派生） | §9.2/§9.6 |
| `cleanup.policy` 三值 | 取消：有部署必有 clean（finally）；保留期只由 `AG_RETENTION_DAYS` 决定 | C-5.7 |

## 10 Runner Registry
| 项 | 默认 |
|---|---|
| 来源与触发 | 平台镜像内 `/app/runners`（Dockerfile `COPY runners/ /app/runners/`；`AG_RUNNERS_DIR` 仅开发/灰度覆盖）。**不做运行时更新**（K-3）：启动时扫描并校验一次，不提供同步 API；坏 `runner.yaml` 在 CI 就该被拦住，运行时校验失败则**拒绝启动**（fail fast） |
| 范围 | 只看 `runners/*/` 根的 `runner.yaml`；entry 文件由 `runner.yaml.entry` 指向（文件名不限，§5.2）。`build/`、`tests/`、`run-local.sh`、`.local/` 不参与（§5.5） |
| 内置角色 | `service-deploy` / `service-clean` / `endpoint-probe` 随平台镜像同目录扫描（`role: deploy/clean` 仅允许出现在这三个内置目录，见下 #5）；用户目录声明这两个角色一律拒绝 |
| 输出 | 启动时 `SyncDiff{added, unchanged, changed, removed}` + 逐条 `FieldError{path, message}`；**同版本不同内容 → 拒绝启动**（`RUNNER_VERSION_PINNED`，K-3） |

| # | 校验项（不过即 `RUNNER_INVALID`，带字段路径） |
|---|---|
| 1–2 | 目录名 = `name`；`name` 小写 `[a-z0-9-]` 且 ≤63；`version` 语义化版本 `^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$` |
| 3 | `image` 非空（形态不限，不解析 tag/digest，§5.6） |
| 4 | `entry` **非空 exec form 数组**（如 `["/runner/run.sh"]`，允许含解释器；文件名不限，§5.2）——取代 v1 的"必须等于 /runner/run.sh" |
| 5 | `role ∈ {test, pre_check, deploy, clean}`；`deploy`/`clean` 仅内置三目录（service-deploy / service-clean）允许，其余路径出现即拒绝（§5.1）；`role=pre_check` 与内置 deploy/clean 时 `on_failure` **必须** `abort_task`（§7.3） |
| 6 | `on_failure ∈ {continue, abort_task}` |
| 7 | 禁止字段 `requires` / `timeout_seconds` / `outputs` 出现即拒绝（附录 C v2） |
| 8 | `k8s` 仅允许 `node_selector`/`tolerations`/`security_context{run_as_user, fs_group}`；出现 `privileged`/`hostPath`/`hostNetwork`/`image`/`command` 即拒绝 |
| 9 | `resources.cpu`/`memory` 是合法 quantity，`gpu.count >= 0` |
| 10 | `concurrency.endpoint_exclusive` 为布尔（**仅外接模式有效**，§6.7 / D-69；建议 pre_check 与性能类为 `true`，非强制） |
| 11 | `mounts[].path` 绝对路径，不与平台保留路径（含 `/workspace/input`）冲突 |
| 12–13 | `parameters` 显式存在（`input_format: yaml/text` 时必须 `[]`）且每项有中文 `description`；`env.*` 有 `required`/`secret`/`description`；`input_format ∈ {jsonl, yaml, text}`；`placeholder` 与 `input_format` 同格式（§5.4 / D-67） |
| 14 | `exports` / `consumes` 为字符串数组（可空）；`consumes` 的 key 不得与本项目 `exports` 重复（组合期闭合校验见 C-5.1，注册期只查结构，§5.12） |
| 15 | `entry[0]` 指向的文件存在且可执行位（embed 检查 `fs.ModePerm & 0111`） |
| 16 | 目录内不得有 `*.yaml` 形态的 K8s 清单（`job.yaml`/`deployment.yaml` 等，§6.8） |

- **JSON Schema：** `schemas/runner.schema.json`（2020-12），手写 schema 为唯一源；附录 C 字段表由 `make doc-fields` 从 schema 抽取后人工校对，CI 校验两边字段集合一致。校验顺序：先 schema（结构）再上表业务规则（语义）。
- **版本不可覆盖：** `content_hash = sha256(规范化 runner.yaml + entry 文件字节)`。同 `name+version` 同 hash → **no-op**（不写库、不改 `updated_at`）；不同 hash → `RUNNER_VERSION_PINNED`（409，§5.7）并**拒绝整批**。新版本只新增行，不覆盖旧行。
- **同步幂等与回滚：** 整批一个事务（upsert → insert 新版本 → 失败 `ROLLBACK`），重复调用得同一 diff；目录里消失的 Runner 不自动删，只标 `orphaned=true`，由 `admin` 显式下线。

## 11 Volcano 可见性
用 `dynamic.Interface` + unstructured（不引 Volcano Go 依赖，避免版本耦合），**只读**，缓存 15s，单查 5s 超时；不可达时返回 `scheduler_unavailable` 而不阻塞页面。

| 资源 | GVR | 取的字段 |
|---|---|---|
| Queue | `scheduling.volcano.sh/v1beta1, queues` | `spec.capability`、`status.state`、`status.allocated` |
| PodGroup | `…, podgroups` | `spec.minMember`、`spec.queue`、`status.phase`、`status.running` |
| Volcano Job | `…, jobs` | `status.state`、`status.running`、`spec.minAvailable` |

Pod / Node 走 `v1` 原生资源（Pod 资源请求与 conditions、节点 `allocatable`/`capacity`），不引入第二套模型。

`deserved` = proportion 插件下 `min(capability, 份额)`，关闭时等于 `capability`；`allocated` = 按 queue 聚合的 `sum(containers[].resources.requests)`（initContainer 取 max）；排队 Job 数 = 该 queue 下 `PodGroup.phase=Pending` 的数量；最长等待 = `now - min(podgroup.creationTimestamp)`；gang 未满足 = `minMember > status.running`。

| `waiting_reason` | 判定输入 | 归属 |
|---|---|---|
| `slot_waiting` | 外接模式端点槽位未拿到（非 K8s 侧；部署模式不产生此值，D-69） | control-plane（§6.7） |
| `queue_full` | `queue.status.state=Closed`，或 `allocated + 本次请求 > deserved` 且队列内 Pending > 0 | 本模块 |
| `insufficient_resources` | Pod `Unschedulable` 且 message 命中 `insufficient (cpu\|memory\|nvidia.com/gpu\|huawei.com/Ascend910)` | 本模块 |
| `gang_pending` | PodGroup `Pending` 且 `minMember > running`，或事件含 `gated by gang` | 本模块 |

计算顺序 `slot_waiting → queue_full → gang_pending → insufficient_resources`；都判定不了时给 `reason=unknown` + `detail.scheduler=unavailable`（`unknown` 只落在 `detail`，不作为枚举值——§6.9 只有四个值）；输出 `{reason, detail}`，`detail` 保留原始 message（截断 256B）。`QUEUED` **必须带 reason**（§6.9），不给空。

## 12 对应 §14.1 的测试点
| 验收项 | 测试点 | 方式 / 环境 |
|---|---|---|
| K8s Executor | Job Spec 2.0 渲染字段齐全（`args` 恒空、`input`/`output` 挂载、`AG_*` env、`argus.io/*` label）；超时可终止（`sleep 600`，`timeout_seconds=20`）；取消可删 Job；同 `job_id` 重复提交只建一个 Job、spec 不同返回 `CONFLICT` | golden YAML 单测 + kind（断言 `RUNNER_TIMEOUT` 且 Pod 消失） |
| 产物持久化 | subPath 隔离（两 Task 并发互不可见；`input` 只读）、删 Job 后产物仍可读；attempt 之间互不可见 | kind + NFS（`local-path` 模拟） |
| Runner 契约 / 独立运行 | D-74 三态：三选一全缺 → `RESULT_MISSING`、`result.json` 非法 → `RESULT_INVALID`（缺失 metrics 等反例）、仅 `result.csv` → `succeeded`；唯一性、`statistic` 枚举、未知字段放行；`exports.env` 合法/非法（`EXPORT_INVALID`）两例；`docker run <image> <entry> --params-file … --url=…` 产出至少一份三选一产物 | fixture 单测 + kind + CI docker + mock OpenAI |
| 日志 | runner stdout 落盘、杀 collector 后 gap 不伪造、**明文不替换**（K-5）；部署 Pod 一 Pod 一文件（经 deploy-state `label_selector` 发现）、重启标记、200MB 分段、`pods.json` 区间 | kind（删副本 / 阈值调 1MiB）+ genet-local（3 Pod + 人为 OOM） |
| 内置 deploy/clean runner | 受理→ready（exports 四件套齐全）/ `DEPLOY_BLOCKED` / `DEPLOY_FAILED` / 超窗四条路径；clean 幂等（重复执行、release 未存在 = 成功）；泄漏重放 `argus clean --task` | genet-local（httptest + kind stub） |
| Runner Registry | 16 条校验逐条拒绝且给字段级错误；`role: deploy/clean` 用户路径拒绝注册；同版本不同内容 `RUNNER_VERSION_PINNED`；同内容 no-op | 表驱动单测 + testcontainers PG |
| runner 状态机 | 杀 `controller` 重启后对账纠正，不误判 `FAILED`；无 `failed → queued` 回路（转移表全覆盖） | kind + 单测 |
| 失败策略 | pre_check 失败（mock OpenAI 返回 401）→ 后续 SKIPPED（传播到 clean 为止）→ service-clean 执行 → Task 终态正确 | kind + genet-local/mock OpenAI |
| 调度可见性（P1） | `waiting_reason` 四值：Queue 满 / gang 未满足 / 资源不足 / 槽位 | kind + Volcano chart |
| 配额与清理 | 超 10 GiB → `ARTIFACT_QUOTA_EXCEEDED`；孤儿 GC 与 `hold-until` 保护 | 单测（配额改 1MiB、fake clock）+ kind |

环境统一：kind（1 主 3 工）+ genet-local（假 Genet）+ mock OpenAI server（可返回 401/5xx/正常 SSE）+ Volcano（仅调度可见性用例）。

## 13 未决问题
| 编号 | 问题 | 默认建议（未确认前按此实现） |
|---|---|---|
| **E-Q1** | ~~Genet REST 契约未定~~ **已关闭**：真实接口是 Helm release 语义（`POST /api/open/helm/uc-packages/{stack\|pd}/releases`、`GET/DELETE /api/open/helm/releases/{name}`），无部署单、无 request_id、无位次，详见 §9 与 [`docs/research/genet-lifecycle.md`](../../docs/research/genet-lifecycle.md) | 契约收敛为 service-deploy runner 的实现依据 + deploy-state.json schema（§9.3）；幂等键 = `release_name`（`arg-` 前缀）；`noWait=true`；总窗口默认 **72h**（`AG_DEPLOY_TIMEOUT`，K-6）不进 Manifest；残留未确认项见 §9.8 |
| **E-Q2** | Pod 级失败原因与错误码枚举不足：`OOMKilled`/`Evicted`/`Preempted`/节点 NotReady 无专门码 | 归 `INTERNAL_ERROR` + `detail.k8s_reason` 保留原文，不自动重试；下版附录 D 增 `CONTAINER_OOM_KILLED`、`POD_EVICTED`（可重试）、`NODE_LOST` |
| **E-Q3** | 镜像拉取 secret 来源：附录 B 无 `imagePullSecrets` | 由 Execution Profile 的 `image_pull_secrets` 提供（Profile 是平台对象，不动 Job Spec 契约）；为空则不渲染 |
| **E-Q4** | 日志明文的安全接受 | **已拍板（K-5）**：平台不读 Secret、不做脱敏；Runner 不得主动打印凭据的契约保留，残余风险由团队接受 |
| **E-Q5** | NFS UID 约定：镜像 UID 不统一 | 默认 10001；有 `run_as_user` 时按该值建目录；都失败依赖 `fsGroup` + `0770`，并把"容忍目录已存在"写进契约 |
| **E-Q6** | 配额统计成本：NFS 上每分钟全量 walk 在数百并发下开销可观 | 默认 60s/attempt，>200 活跃分 10 批；后续可改 Runner 上报 + 平台抽查 |
| **E-Q7** | 部署 Pod 日志保留期与容量（日志不随 Pod 回收，磁盘有限） | 与 Task 产物同保留期，不提前删；单 Task 部署日志软上限 2 GiB，超出后续分段只记 head 并标 `truncated` |
| **E-Q8** | **`attempts/<n>/input/` 的多 runner 组织**：ARCHITECTURE §8.1 画的是单文件 `input/params.jsonl`，但一个 Task 有多个 runner、各自有输入 | 按 seq 子目录组织：`attempts/<n>/input/<seq>/params.<ext>`（本文 §3.1 / C-2.3 已按此实现），挂载 subPath 指到对应 seq 目录；待 ARCHITECTURE 回写确认 |
### §7.4 统一共享挂载的渲染注入与启动 preflight（D-63）

- **渲染**：`executor/render.go` 把平台级 `sharedMounts` 每条渲染成 `volume`（**PVC source**，`persistentVolumeClaim.claimName`）+ `volumeMount`；**不生成**内联 `volumes[].nfs`（`restricted:latest` 会拒绝，研究 #69 实测）。`Profile.sharedMounts=false` 时整段跳过。
- **volume 名去重**：`sanitizeLabel` 之后不同 name 可能撞名（K8s 会以 `Duplicate value` 拒绝整份 Pod），渲染期统一分配唯一名；平台保留 + 统一挂载路径并成一套集合（`domain.ReservedMountPaths`），Runner `mounts[]` 撞任一 → 渲染期 `ErrSpecInvalid`、提交期 400。
- **启动 preflight**：按 `AG_SHARED_MOUNTS` 逐条 `get pvc`，**只告警不 fail-fast**（D-63 ④），日志带 namespace；缺 PVC 时 Pod 会 Pending，由 Runner 报"权重路径不存在"。
- **权限**：NFS 导出侧目录 `0755`、文件 `0644`（uid 10001 原样透传；`root_squash` 不压 10001；`fsGroup` 对普通 NFS 卷无效）。