# 执行层实现（execution）
> **范围：** 从"拿到一份 Job Spec"到"证据落盘、可被报告引用"之间的实现：Job 渲染与生命周期观测、挂载目录、超时取消、清理回收、日志采集、产物校验与配额、Genet 适配、Tester Registry、Volcano 只读可见性。
> **不在范围：** Task/Run 状态机与转移、调度与 Endpoint 槽位、API 形状、报告结构与前端——归 `control-plane.md`。本文只给执行层对外的接口，不重复状态机细节。
> **基线：** `ARCHITECTURE.md`（v0.2）。冲突以架构为准；本文新增的默认值都标了出处，架构未定义的标"待裁决"。
> **既定选型：** Go 单二进制（`api` / `controller` / `collector`）、PostgreSQL 唯一权威状态源、NFS（RWX）产物存储、K8s Job 唯一 Executor、不加 sidecar、不依赖集群日志系统（§8.6）。
> **编号：** 本文节号即 `IMPLEMENTATION.md` §6 的 `E-N`（§2–§6 Executor、§7 日志、§8 产物、§9 Genet、§10 Registry、§11 可见性）。

## 1 边界与职责
| 组件 | 只做什么 | 明确不做 |
|---|---|---|
| **Executor**（`controller`） | Job Spec → `batch/v1 Job`；创建、观测（informer + 对账）、取消、删除；目录预创建；返回执行事实 | 不解析 `args` 语义、不拼 Tester 命令、不判指标、不写状态表 |
| **日志采集**（`collector`） | Tester stdout/stderr 与 Deploy 各 Pod 日志流式落盘；重启/缺口标记；分段索引 | 不解析日志内容、不因日志体积改判 Task、不脱敏（K-5）、不依赖 Loki/ES |
| **Genet 适配**（`controller`） | 部署 / 状态查询 / 卸载；Endpoint 归一化；Pod 拓扑快照 | 不做 Helm 渲染、不改 Genet 参数、不解释拓扑 |
| **Tester Registry**（`controller`） | 扫描 `testers/`、校验、与库同步、暴露参数清单 | 不构建镜像、不渲染表单、不做运行时参数校验 |
| **Volcano 视图**（`api`） | 只读查 Queue / PodGroup / Job / Node，算 `waiting_reason` 输入 | 不改权重、不抢占、不写回调度器、不部署第二套监控 |

**接口边界：状态只由 control-plane 写，事实只由执行层产出。** 执行层不持有 `database/sql` 连接，不 import 状态机包。

| 数据 | 写入方 | 说明 |
|---|---|---|
| `runs` / `tasks` / `attempts` 状态字段 | control-plane | 执行层只返回事实，事务内落库（§12.5） |
| `attempts.exit_code` / `error_code` / `log_ref` 等物理事实 | 执行层经 store 窄接口回写 | 不写 `tasks` / `runs` 状态字段（IMPLEMENTATION §5 依赖方向）；状态推进仍由 control-plane 在事务内做 |
| `artifacts` 索引 | control-plane | 执行层只给候选清单（§8.5） |
| `events` 表 + `events.jsonl` | control-plane | 执行层调 `EventSink`，不自行落库 |
| NFS 上日志、`pods.json`、`deploy/manifest.yaml` | 执行层 | 直接写共享存储 |
| K8s Job / NetworkPolicy / Lease | 执行层 | label + annotation 自描述，便于 GC |

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
	FinishAttempt(ctx context.Context, k AttemptKey, r AttemptResult) error
	FinishAttemptAsCanceled(ctx context.Context, k AttemptKey, r CancelResult) error
}
```

## 2 Job Spec → batch/v1 Job 渲染
### 2.1 字段映射（附录 B）
| Job Spec 字段 | 渲染位置 | 规则 |
|---|---|---|
| `spec_version` | 不渲染 | 只接受 `1.0`；未知版本返回不可重试错误（映射 `VALIDATION_ERROR`），因为 Executor 是进程内组件、没有"退出码 2" |
| `job_id` / `run_id` / `task_id` / `attempt` | 名称、label、env、subPath | 见 §2.3、§3.1 |
| `image` | `containers[0].image` | 原样透传，不解析 tag/digest（§5.6） |
| `command` | `containers[0].command` | 单元素 `["/tester/run.sh"]`；显式写，不依赖镜像 ENTRYPOINT |
| `args` | `containers[0].args` | 数组逐元素透传；**永不拼字符串、永不经 shell**（§5.3） |
| `env` | `containers[0].env` | 按 key 字典序；`TESTPLANE_*`、`TZ`、`HOME=/tmp` 由执行层强制覆写 |
| `secret_env[]` | `valueFrom.secretKeyRef` | 按 control-plane 的 `secret_refs` 记录（C-2.3）映射到目标 namespace 的 Secret 名与 key；引用名不参与推导，未知引用在提交期就被拒绝 |
| `resources.cpu/memory/gpu` | `requests` + `limits` | cpu/memory 两者相等（Guaranteed QoS，`limits_scale` 默认 1.0）；`gpu.count: 0` 不渲染，扩展资源必须 requests==limits |
| `mounts[]` | `volumeMounts` + PVC | `output` 必须带 `subPath`（§3.1）、RW；`profile_mount` RO 且渲染期校验供给（§3.4） |
| `network.egress_allow` | 同名 NetworkPolicy | 每 Job 一条，label 与 Job 一致，随 Job 删除 |
| `node_selector` / `tolerations` | Pod spec 同名 | `tester.yaml.k8s` 白名单，与 Profile 合并；平台字段不可覆盖（§6.8） |
| `security_context` | Pod + container 两级 | 默认非 root / 只读根 / drop ALL；`run_as_user`、`fs_group` 可被 Tester 覆盖 |
| `labels` / `annotations` | 元数据 | §2.3，另加 `testplane.io/managed=true` |

固定渲染项（附录 B 未列，但必须存在）：`backoffLimit: 0`、`completions/parallelism: 1`、`restartPolicy: Never`、**不设** `ttlSecondsAfterFinished`、`automountServiceAccountToken: false`（Tester 不得调 K8s API，§5.10）、`enableServiceLinks: false`、`terminationGracePeriodSeconds: 30`、`capabilities.drop: ["ALL"]`、`seccompProfile: RuntimeDefault`、`/tmp` 挂 `emptyDir{256Mi}`（配合只读根）。

### 2.2 渲染结果（输入：`testers/endpoint-probe/tester.yaml`）
Run `r-20261103-0001`、pre_check Task `t-0001`、attempt 1、`timeout_seconds: 900`。

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: tp-t-0001-a1
  namespace: testplane
  labels: { testplane.io/managed: "true", testplane.io/job-id: j-9f3a1c,
            testplane.io/run: r-20261103-0001, testplane.io/task: t-0001,
            testplane.io/attempt: "1", testplane.io/role: pre_check }
  annotations: { testplane.io/spec-hash: "sha256:5c1e9a…", testplane.io/tester-version: "1.0.0",
                 testplane.io/timeout-seconds: "900" }        # spec-hash 供幂等判定（§2.4）
spec:
  backoffLimit: 0
  completions: 1
  parallelism: 1
  activeDeadlineSeconds: 960                              # 900+60，仅兜底（§5.1）
  template:
    metadata:
      labels: { testplane.io/managed: "true", testplane.io/job-id: j-9f3a1c }
      annotations: { testplane.io/attempt-id: t-0001-a1 }
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      enableServiceLinks: false
      terminationGracePeriodSeconds: 30
      securityContext: { runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, fsGroup: 10001,
                         seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: tester
          image: registry.dev.huawei.com/testplane/endpoint-probe:1.0.0
          imagePullPolicy: IfNotPresent
          command: ["/tester/run.sh"]
          args: ["--wait_timeout_seconds=600", "--retry_interval_seconds=15",   # 显式 precheck 对象给的额外参数；auto 模式下 args 为空
                 "--url=http://10.0.0.10:8000/v1", "--model_name=Qwen3-32B"]   # 平台追加的公共参数
          env:
            - { name: TESTPLANE_RUN_ID, value: r-20261103-0001 }
            - { name: TESTPLANE_TASK_ID, value: t-0001 }
            - { name: TESTPLANE_ATTEMPT_ID, value: t-0001-a1 }
            - { name: TZ, value: Asia/Shanghai }
            - { name: HOME, value: /tmp }
            - name: ENDPOINT_API_KEY
              valueFrom: { secretKeyRef: { name: tp-secret-endpoint-prod-qwen, key: api_key } }
          resources: { requests: { cpu: "1", memory: 256Mi }, limits: { cpu: "1", memory: 256Mi } }
          securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                             capabilities: { drop: ["ALL"] } }
          volumeMounts:
            - { name: output, mountPath: /workspace/output,
                subPath: r-20261103-0001/tasks/t-0001/attempts/1 }
            - { name: tmp, mountPath: /tmp }
      volumes:
        - { name: output, persistentVolumeClaim: { claimName: testplane-storage } }   # RWX
        - { name: tmp, emptyDir: { sizeLimit: 256Mi } }
```

### 2.3 命名、label、annotation、幂等
| 项 | 规则 |
|---|---|
| Job/Pod 名 | `tp-<task_id>-a<attempt>`（如 `tp-t-0007-a1`，与附录 B 的 `executor_ref.job` 一致）；DNS-1123、总长 ≤ 63；Pod 名由 Job controller 生成，平台不改 |
| `job_id` | `j-` + `sha256(task_id + "/" + attempt)[:8]`：**不透明幂等键**（附录 B `j-9f3a1c` 同形），与 Job 名同源派生 |
| 必带 label | `testplane.io/{managed,job-id,run,task,attempt,tester,role}`；值 ≤63 字符且匹配 `[a-z0-9A-Z]([-_.a-z0-9A-Z]*[a-z0-9A-Z])?`，渲染前再校验一次 |
| 必带 annotation | `spec-hash`、`tester-version`、`timeout-seconds`、`requested-by`；取消后补 `canceled-by`、`reason` |
| `ownerReference` | **不设**：控制面不是 K8s 对象，没有可作 owner 的父资源；级联删除靠 label 扫描 + 平台 GC（§6.3）。Pod 的 owner 由 Job controller 自动设置 |

**幂等：** 名称由 `(task_id, attempt)` 唯一决定，`job_id` 与它同源派生，因此天然幂等。流程 = `GET` → 比对 `spec-hash`：相同则直接返回既有 `JobRef`（不创建、不报错）；不同则 `CONFLICT`（409）；不存在则 `Create`，并发 `AlreadyExists` 回落 `GET`。不用 `GenerateName`、不"删了重建"——重建会让日志与退出码错位。

### 2.4 为什么不设这几个字段
| 决策 | 理由 | 代价（已接受） |
|---|---|---|
| `backoffLimit: 0` | 重试是 Task 语义（新建 Attempt，§7.3），不是 Pod 语义；K8s 重拉会让"一个 Attempt 一个 exit code"失效，产物被覆盖 | Pod 失败即 Job 失败，由平台判 |
| `restartPolicy: Never` | 同上：容器重启会续写同一 stdout、覆盖 `result.json`，Attempt 边界丢失 | 崩溃即失败，不做进程内自愈 |
| 不设 `ttlSecondsAfterFinished` | TTL 在结束后删 Job/Pod，而平台此时还要读 `containerStatuses` 判 `OOMKilled`/`IMAGE_PULL_FAILED`、补 `pods.json`、等日志 follower 收尾 | 自己 GC（§6.3），时机可控 |
| 镜像拉取 | `imagePullPolicy`：digest 或非 `:latest` tag → `IfNotPresent`，`:latest`/无 tag → `Always`。`imagePullSecrets` 附录 B 未定义，默认取 Profile 的 `image_pull_secrets`，为空不渲染（E-Q3） | — |

## 3 挂载与目录
### 3.1 subPath
`storage_path` = `<storage-root>/<run_id>/tasks/<task_id>/attempts/<n>`（§6.8），挂载时必须换算成 PVC 内相对路径，**不得**整体挂 `<storage-root>`。

```go
// rel(storage_root, storage_path) → subPath；越界或不合规直接拒绝渲染，不"尽力纠正"。
rel, err := filepath.Rel(root, filepath.Clean(storagePath))
if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) { return "", errBadPath }
subPath = filepath.ToSlash(rel) // 必须匹配 ^<run>/tasks/<task>/attempts/<n>$：禁止 `..`、`/`、空白、控制字符
```

PVC 名不在 Job Spec 里，由 Execution Profile 的 `output_pvc` 提供，渲染期与 `mounts[type=output]` 合并。`profile_mount` 可带 Profile 侧配置的 `sub_path` 前缀，但不得指向另一个 Run 的目录。

### 3.2 目录预创建
| 项 | 默认 |
|---|---|
| 谁建 | `controller` 的 `storage.PrepareAttempt`：控制面 Pod 以同一 RWX PVC 挂到 `/storage`，只做 `mkdir`/`chown`，不写业务文件 |
| 建哪些 | `attempts/<n>/`、`logs/`、`raw/`（后两者是 Tester 与 collector 的写入点，提前建避免 UID 竞态） |
| 属主/权限 | `10001:10001`、`0770`；父目录同属主 |
| chown 失败 | 只记 warning 继续：交给 `fsGroup: 10001` 与 Tester 自己 `mkdir -p` 兜底（§6.8 第 3 条要求两边都成立） |
| 为什么不用 initContainer | initContainer 与主容器共享同一 `subPath`，chown 不到父目录；平台侧预创建还能在建 Job 前失败 |

### 3.3 只读根文件系统与 `/workspace/output`
根文件系统只读（§12.2），可写面仅三处：`/workspace/output`（PVC subPath）、`/tmp`（emptyDir）、Profile 挂载（只读）。`/workspace` 本身不挂卷，保持只读。`HOME=/tmp`：pip / python / jq 缓存会写 `$HOME`，否则只读根会变成"随机失败"。Tester 可写 `logs/`、`raw/`，与 collector 的 `logs/stdout.log`、`logs/stderr.log` 不冲突——**collector 只拥有这两个文件名**。

### 3.4 Profile 挂载校验与 `MOUNT_UNAVAILABLE`
渲染期（不是运行期）逐项检查 `tester.yaml.mounts`：Profile 存在同名供给（`required: false` 缺失则跳过并记 `mount_skipped`）；`path` 为绝对路径且不与 `/workspace/output`、`/tmp`、`/tester` 冲突；挂载点之间不得前缀重叠；PVC 在目标 namespace 存在（一次 `GET`）。任一失败 → `MOUNT_UNAVAILABLE`（不可重试），**必须在创建 Job 之前暴露**，不能等 Pod 卡在 `ContainerCreating`。

### 3.5 NFS 权限风险
| 风险 | 默认应对 |
|---|---|
| 服务端 `root_squash` 使 `chown` 无效 | chown 只当优化；`fsGroup: 10001` + `02770`（setgid）保证组继承；Tester 以 10001 运行 |
| `fsGroup` 在部分 CSI 组合上不生效 | 目录预创建成为主路径；Tester 侧 `mkdir -p` 仍必须能工作 |
| UID 不统一（Tester 覆盖 `run_as_user`） | 目录属主取该 Attempt 的实际 `run_as_user`，取不到用 10001（E-Q5） |
| 多节点时钟漂移 | 日志时间戳一律取 apiserver 返回的时间，不用节点本地时钟 |
| stale NFS handle | `PrepareAttempt` 失败 → `EXECUTOR_UNAVAILABLE`（可重试），不创建 Job |

## 4 生命周期观测
### 4.1 informer 为主
- **每集群一个** `SharedInformerFactory`（按 cluster + namespace 复用），watch `batch/v1 Jobs` 与 `core/v1 Pods`。
- **list/watch 都带 label 过滤** `testplane.io/managed=true`；Pod 侧再按 `testplane.io/job-id` 二级过滤，不依赖 controller 注入的 `job-name`。
- handler 不写库、不阻塞：只更新内存 `statusCache` 并经有界 channel（1024，满则丢弃并置脏）通知 reconciler。
- **`HasSynced` 之前不做任何判定**，启动时先等同步再用 `Reconcile` 补齐，避免"刚重启就把在跑的 Job 判失败"（§12.5）；`resyncPeriod = 5m` 只用于对齐，不触发业务动作，410 Gone 与重连由 Reflector 负责。

### 4.2 周期对账兜底
每 30s 取 control-plane 的"非终态 Attempt"集合与 cache 比对：cache 缺项 → 直接 `GET job`，404 进入 §4.4 复核；cache 有而 DB 已终态 → 只清 cache，不动状态。单轮上限 500 条、单次 5s 超时，失败不阻塞下一轮。

### 4.3 映射表
| 观测 | phase | 错误码 |
|---|---|---|
| Job `Complete=True` | `succeeded` | —（还要过 §8） |
| Job `Failed=True`，`reason=DeadlineExceeded` | `timeout` | `TASK_TIMEOUT` |
| Job `Failed=True`，`reason=BackoffLimitExceeded` | `failed` | 按 Pod 终止原因归类 |
| Pod `Succeeded`，`exitCode==0` | `succeeded` | — |
| Pod `Failed`，`terminated.exitCode != 0` | `failed` | `TESTER_NONZERO_EXIT` |
| `terminated.reason=OOMKilled` / `Evicted` | `failed` | `INTERNAL_ERROR` + `detail.k8s_reason`（§4.5） |
| `waiting.reason ∈ {ErrImagePull, ImagePullBackOff, InvalidImageName, ErrImageNeverPull}` | `failed` | `IMAGE_PULL_FAILED` |
| `PodScheduled=False/Unschedulable` 且 message 含 `volume`/`mount`/`FailedMount` | `failed` | `MOUNT_UNAVAILABLE` |
| 事件 `FailedMount` / `FailedAttachVolume` / `MountVolume.SetUp failed` | `failed` | `MOUNT_UNAVAILABLE` |
| Pod `Pending`（`Unschedulable` / `ContainerCreating` 等，**不判失败**） | `pending` | — → 记 `waiting_reason`（§11） |
| Pod `phase=Unknown` / Job 404 且无取消标记 / 其余 | `unknown` | 复核（§4.4） |

判定顺序固定：先 Job 终态，再 Pod；矛盾时以更细的 Pod 终止原因为准并记 `detail.conflict=true`。`exit_code` 仅在单容器、单次终止时返回。

### 4.3.1 镜像事实采集（image digest）

Pod 首次 `Running` 与终态各读一次 `containerStatuses[].imageID`（形如 `docker-pullable://repo@sha256:…`），提取 `sha256:` 后的 64 位写入 `attempts.image_digest`。它与提交的 `image_ref` 分开存：引用是"要什么"，digest 是"实际拉到什么"。

- 用途：回答"同一个 tag 当时到底跑的是哪份代码"；P1 续跑复用优先比较 digest（IMPLEMENTATION G-16）。
- 采集失败（字段缺失/格式不符）只记 `detail.image_digest=unknown` 的 warning，不重试、不影响状态。
- 不解析 tag、不做 digest 校验（§5.6 平台不解析镜像引用）；只记录 K8s 给出的事实。

### 4.4 `unknown` 的复核（不得直接判失败）
第 0 次 `GET Job`（已终态直接出结论）→ +5s `GET Job` + `LIST` Pod → +20s 查 Pod events → +60s 查 Node 状态 → 仍无法判定才 `failed(INTERNAL_ERROR)`，`detail.review=exhausted` 并附全部原始观测。复核期间 Task 保持 `RUNNING` 并发 `executor_observation_unknown` 事件（UI 显示"待确认"）；复核不重置 `activeDeadlineSeconds`，watchdog 到点仍按 `TASK_TIMEOUT` 收口；总耗时 ≤ 90s。

### 4.5 错误码枚举不足
`OOMKilled` / `Evicted` / `Preempted` / 节点 NotReady 在附录 D 没有专门码。原则：**不新增枚举**（§15 要求先改文档），先归语义最近的码（资源类 `INTERNAL_ERROR`、挂载 `MOUNT_UNAVAILABLE`、镜像 `IMAGE_PULL_FAILED`）；**原始原因永不丢**（`reason`、`detail.k8s_reason`、`detail.exit_code` 一并落库）；这类码不进 `retry_on` 白名单。E-Q2 建议下一版增加 `CONTAINER_OOM_KILLED` / `POD_EVICTED`（可重试）/ `NODE_LOST`。

## 5 超时与取消
### 5.1 两层超时，平台权威
`activeDeadlineSeconds = timeout_seconds + 60` 只保证"不会无限占资源"，60s 缓冲避免与平台判定同时触发、日志被抢断。**权威是平台 watchdog**：每 10s 扫 `RUNNING`（以及超过 10 分钟仍 `PREPARING` 的）Attempt，`now - started_at > timeout_seconds` → 终态 `FAILED(TASK_TIMEOUT)`。顺序恒定：先落库终态，再 `Cancel`——Job 自己到点只是被 K8s 杀掉，写不了 `TASK_TIMEOUT`，也不会收尾 `pods.json` 与日志流。渲染期校验 `timeout_seconds` ∈ (0, 86400]（§12.7）。

### 5.2 取消 = 置标记 + 删 Job + 等 Pod 消失
1. control-plane 置 `cancel_requested_at` 并记 `canceled-by`。
2. `delete job`，`propagation=Background`，`gracePeriod=30`（Pod 由 Job controller 异步删，不阻塞 API）。
3. 每 2s 查 Pod（按 `job-id` label），最长 60s；未消失 → `delete pod`，`propagation=Foreground`，grace 0（必须确认删掉，否则仍有进程写 NFS）。
4. 再过 30s 仍残留 → `CLEANUP_FAILED` 事件 + 告警，交 GC，**不悬挂在 `CANCELLING`**。

### 5.3 与自然结束的竞态：CAS，先落库者胜
```sql
UPDATE attempts SET status = $new, error_code = $code, finished_at = now()
 WHERE id = $id AND status NOT IN ('succeeded','failed','canceled');
```

返回 1 行 = 生效；返回 0 行 = 已终态，只记 `state_race_lost` 事件（带双方候选状态），不覆盖、不报错（§6.6）。因此一个已被取消的 Attempt 随后收到 `succeeded`，只补 `exit_code` 等执行事实，**不改状态**。watchdog 与 informer 应用结论前都要重读 DB，不能凭 cache 直接写。

### 5.4 清理失败
结果不变（§6.6），只发 `CLEANUP_FAILED` + `cleanup_failures_total`；进重试队列 1m/5m/15m 最多 5 次，之后交 GC。执行层本就不删产物目录，不存在"目录清理失败"。

## 6 清理与回收
| 路径 | Job | Pod | 产物目录 | 日志 |
|---|---|---|---|---|
| 正常结束（succeeded/failed） | 删，`Background` | 随 Job 消失 | **保留**（失败也保留，§6.8 第 5 条） | 保留，follower 读到 EOF 关闭 |
| 取消 | §5.2 全流程 | 必要时 `Foreground` + grace 0 | **保留** | 保留，写 `--- [canceled at …] ---` |
| 超时 | 同取消 | 同取消 | **保留** | 保留，写 `--- [deadline exceeded at …] ---` |

删除时机在**结论落库成功之后**：落库失败就先不删 Job，让对账重来——多留一个 Job 比丢一份结论便宜。`CLEANUP` 阶段执行层只做两件事：停该 Run 的日志采集、让 GC 可回收 Job；产物归档/删除由 `cleanup.policy` 与 control-plane 的保留任务负责，**执行层不删任何产物目录**。

**孤儿 GC（每 10 分钟，纯 label 驱动）：**

| 条件 | 动作 |
|---|---|
| `managed=true` 但 DB 无对应 attempt | 立即删 Job |
| attempt 已终态且 Job 创建 > 1h | 删 Job（正常路径没删干净） |
| 运行中且 `now - creationTimestamp > 25h` | 删 Job + 经 `ResultSink` 修状态为 `FAILED(TASK_TIMEOUT)` |
| annotation `testplane.io/hold-until` 未过期 | **跳过**（日志 follower 未收尾） |

GC 只删 Job/Pod/NetworkPolicy/Lease，不碰 NFS；`tp-` 前缀但 label 不符的一律不动（可能是人工调试）。

## 7 日志采集
### 7.1 Tester stdout/stderr
数据源 `pods/log?container=tester&follow=true&timestamps=true`。**游标**用服务端时间戳做 `sinceTime`，落盘前剥离前缀（Tester 看到干净日志），游标存内存并随 64KiB flush 记 checkpoint。**断线重连**：`sinceTime=lastTS`（不减容差），重连后写 `--- [log resumed at …] ---`，2s 内完全相同的行去重（窗口 200 行）。**落盘**：`logs/stdout.log`、`logs/stderr.log` 分离，`bufio` 4KiB + 1s flush，不用 `O_SYNC`（NFS 代价过高），丢失窗口 ≤1s。**结束**：EOF → 写 `--- [container exited at …, exit code N] ---` 后关闭。

### 7.2 Deploy 多 Pod
| 项 | 默认 |
|---|---|
| 发现 | 用 Genet 的 `PodSelector` `LIST pods`，每 30s 重新发现；新增自动跟随，消失的写 `--- [pod terminated at …] ---` |
| 一 Pod 一文件 | `deploy/logs/<pod>.log`；多容器 Pod 用 `<pod>.<container>.log`，默认 `server` |
| 重启 | 监视 `restartCount` 增加 → `--- [container restarted #3 at 06:44:11, previous exit: OOMKilled] ---`，**续写同一文件** |
| 分段 | 单文件 > 200MB 滚到 `<pod>.1.log` / `<pod>.2.log`；`pods.json` 记每段 `{file, start, end, restarts, truncated}` |
| gap | `--- [log gap: 06:52:10 – 06:55:40, collector restarted] ---`；**宁可标缺口也不补**（§8.6） |
| 停止 | Run 进入 `CLEANUP` → `--- [collection stopped: run entered CLEANUP at …] ---` 并关闭；**不是**测试一结束就停 |
| `pods.json` | 每 30s 原子写（temp + rename）：Pod 名、角色、节点、重启次数、就绪、日志区间 |

### 7.3 选主与故障语义
全局 Lease `testplane-collector-leader`（TTL 15s / 5s 续约）负责分配归属、写 `pods.json`、清理死 follower；**每个 Job 一个** Lease `tp-log-<jobhash>`（TTL 30s / 10s 续约），持有者才能写该 Attempt 的 stdout/stderr。Lease 过期后新持有者从 checkpoint 续采并写 gap 标记；旧持有者恢复后发现 Lease 被占即停写。同一文件同一时刻只有一个写者（`O_APPEND` + Lease）。

允许丢日志，**不允许伪造连续**：崩溃、apiserver 不可达、NFS 短时不可写，一律恢复后写 gap 标记（统一格式 `--- [<event>: <detail> at <RFC3339>] ---`，UI 可正则识别）。埋点统一 `collector_` 前缀：`collector_log_lines_total`、`collector_log_bytes_total`、`collector_gaps_total{reason}`、`collector_log_lease_lost_total`（与 web-and-delivery §12 一致）。

**日志明文，不脱敏**（团队决策 K-5）：平台不读 Secret 值、不做替换，Tester 与 Deploy 日志原样落盘；`redactions_total` 指标取消。Tester 契约里"不得主动打印凭据"保留，但平台不再兜底。§12.4 已按 v0.3 修订（IMPLEMENTATION G-9）。

## 8 产物校验与配额
### 8.1 时机与判定
只有 `exit_code == 0` 才读 `result.json`：

| 条件 | 结论 |
|---|---|
| `exit != 0` | 先尝试读 `result.json`：存在且合法、`error.code` 属于附录 D → 用该码与该实例的 `retryable`；否则回退 `TESTER_NONZERO_EXIT`。Task 一律 `failed`，`raw/` 保留 |
| `exit == 0`，文件不存在 / 0 字节 / JSON 非法 / schema 不过 | `RESULT_MISSING`（不存在或 0 字节）或 `RESULT_INVALID`（非法或不合 schema，`detail.path` 到字段级） |

> 非零退出仍读结果，是为了让 Pre-Check 能区分"未就绪（可重试）"与"4xx/认证失败（不可重试）"（§5.11）：Tester 在失败时按 §5.9 写 `error.code`（`ENDPOINT_UNREACHABLE` / `PRECHECK_FAILED`）与 `retryable`，平台只接受附录 D 枚举，读不到就回退通用码。`result.json` 只用于**归类错误**，不改变"非零退出 = Task 失败"这一事实。

校验发生在落库**之前**，与结论同一次 `FinishAttempt` 提交，避免"先 SUCCEEDED 再补错误码"。

### 8.2 schema 校验范围（附录 A / `schemas/result.schema.json`）
| 检查 | 级别 |
|---|---|
| `schema_version=="1.0"`、`task_id`、`tester.name`、`metrics` 存在 | 必须（附录 A 明示） |
| `tester.name`/`version` 与注册表一致；`task_id` 与本次 Attempt 一致 | 必须 |
| `status ∈ {succeeded, failed}`；`started_at`/`finished_at` 可解析且 `finished >= started` | 必须 |
| 每个 metric 有 `name`/`value`/`unit`/`statistic`，`statistic` ∈ 9 值 | 必须（§5.9） |
| 同 `labels` 下 `name+statistic` 唯一 | 必须 |
| `metrics` 为空时 `notes` 非空；`status=failed` 时 `error` 非空 | 建议（warning，不判失败） |
| 未知字段 **允许**（前向兼容），顶层未知 key 记 `detail.extra_keys` | — |
| 文件 ≤ `TESTPLANE_RESULT_MAX_BYTES`（默认 100MiB，仅防 OOM 的技术上限） | 必须（超限 `RESULT_INVALID`，`detail=too_large`；K-10 不设业务限制） |

### 8.3 配额（单 Attempt 10 GiB）
collector 每 60s 对 `attempts/<n>/` 全量 walk（按 inode 去重硬链接，单次 < 50ms 量级），超 `artifact_quota_bytes`（默认 10 GiB）→ 复用取消路径删 Job → 终态 `FAILED(ARTIFACT_QUOTA_EXCEEDED)`，**不可重试**。结束后再算一次精确值，已结束但超限同样改判（在落库前完成，不产生两次状态转移）。walk 失败（NFS 超时）只记 `quota_check_skipped`，不判失败。

### 8.4 sha256：惰性，默认不算
`artifacts.hash` 默认 `lazy`：不在收集阶段计算——大文件在 NFS 顺序读一遍可能比测试本身还慢，而唯一消费方（下载校验、续跑去重）都不在热路径。可选 `eager`（异步算，>1GiB 跳过并置 `null`）与 `off`。算一次即缓存，缓存键 `mtime + size`。

### 8.5 索引调用契约
```go
type ArtifactRecord struct {
	Path      string // 相对 attempts/<n>/，禁止绝对路径与 ..
	SizeBytes int64
	SHA256    *string // lazy 模式为 nil
	Kind      string  // result | stdout | stderr | tester_log | raw | other
	MTime     time.Time
}
// Candidates 幂等（按 Path 排序）；不索引目录、.tmp、>2GiB 文件（仍记 size）。
func (v *Validator) Candidates(ctx context.Context, a AttemptRef) ([]ArtifactRecord, error)
```

control-plane 据此写 `artifacts(attempt_id, path, size_bytes, sha256, kind, mtime, indexed_at)`，`(attempt_id, path)` 唯一。`result.json.artifacts[]` 与磁盘取交集：声明了但没有文件 → warning（`detail.dangling_artifact`），不判失败。

## 9 Genet 适配

> **本节已按真实 Genet 后端代码重写**（调研见 `.data/research/genet-lifecycle.md`，下称 `R-G:行`；`<G>` = `.data/genet` 只读克隆）。旧版"假定 Genet 提供 `POST /deployments`、`GET /deployments/{id}`"**已被证伪**：Genet 没有"部署单"，**一次部署 = 一次 Helm release**，定位符只有 `namespace + releaseName`（`<G>/backend/internal/handlers/helm.go:2992-3000` 的 202 响应只有 `name`；全后端没有 `request_id/job_id/release_id`，`R-G:28`）。字段名集中在 `server/internal/genet/dto.go`（骨架见 §9.9），MVP 实现仍是 `internal/genet/mocks`。

### 9.1 事实基线

| 事实 | 对设计的约束 | 证据 |
|---|---|---|
| 一次部署 = 一次 Helm release；定位符 `namespace + releaseName` | 平台主键仍是 `deploy_id`，但**对外可定位键是 `(namespace, release_name)`**，必须落库 | `R-G:11`、`R-G:94` |
| `namespace` 由 **API Key 绑定**，请求体不能指定；Key 未绑定 → 403 | 多租户隔离靠 Key，不靠调用方参数；`runs.deploy.namespace` 记调用时的绑定值 | `<G>/backend/internal/handlers/openapi_helm.go:39-56`；`R-G:75` |
| `releaseName` 由调用方自造，正则 `^[a-z][a-z0-9-]{0,52}$`，**Genet 不加 namespace 前缀** | 幂等键由我们派生（§9.2）；`openapi.yaml:4175` 的 "adds a namespace prefix" 是过时描述 | `<G>/backend/internal/handlers/helm.go:36`、`:2696-2698`；`R-G:74`、`R-G:417` |
| 拉起是**异步**的：202 之后后台 goroutine 跑 `helm upgrade --install`，调用方拿不到作业号 | 只能凭自己起的 `releaseName` 轮询；**POST 超时不代表没创建** | `helm.go:2981-2990`；`R-G:70`、`R-G:320` |
| 排队**不是 Genet 状态**：排队发生在 Volcano/PodGroup 层，Genet 只给水位计数、**没有位次** | `runs.deploy` **不存位次**（§9.8）；`queue_full/gang_pending/insufficient_resources` 从 CRD 读（§11） | `R-G:98-106`、`R-G:294`；`<G>/backend/internal/models/volcano_views.go:13-39` |
| 鉴权 `Authorization: Bearer <API Key>`（scope `read`/`write`）；无限流、无请求体大小限制（未找到） | 客户端 5s connect / 10s 总超时；`values` 控在 100KB 内并做 digest（R11） | `<G>/backend/internal/auth/apikey.go:37-49`；`R-G:239-249` |
| `GET /api/open/helm/releases`（列表）会逐 release 富化 `helm get values` + `list pods`，后端注释自认会超时 | 任何列表/对账必须 `light=true` | `helm.go:3345-3384`；`R-G:423` |

### 9.2 拉起（Deploy = Helm release install）

| 项 | 值 |
|---|---|
| 方法 / 路径 | `POST /api/open/helm/uc-packages/{stack\|pd}/releases`（按包类型路由） |
| 必填 | 仅 `releaseName`；`values` / `templatePath` / `templateId` 三选一 |
| 可选 | `image`、`modelPath`（必须 `/mnt/model` 前缀）、`set[]{name,value,type}`、`noWait` |
| 响应 | **202** + `{name, requestedName, namespace, packageType, chart, chartVersion, status:"pending-install"}` |
| 同名再 POST | **400**（`...already has protected metadata; install cannot replace an existing release`），不是幂等成功 |
| 并发 install 同 release | 409 `protected Helm operation is in progress` |
| chart / version | **不可指定**，由 Genet 配置 `helm.ucPackages.{stack\|pd}` 固定 |
| 证据 | `R-G:23-27`、`R-G:86-89`、`R-G:258`；`<G>/backend/internal/handlers/helm.go:2906-3001` |

**`noWait=true` 是硬要求。** `timeoutSec` 是 helm `--wait` 的 deadline（出厂 600s），排队超过它 release 会被判 `failed` 而 Pod 还在排队（`R-G:194-196`、`R-G:299`）。平台**恒传 `noWait=true`**，就绪改由平台轮询判（C-5.3）；若传 `timeoutSec`，它只能当后台 goroutine 上限，不能当排队窗口。

**values 合并顺序（"部署模板"的全部含义）**：chart 默认 values → `templateId`/`templatePath`/`values`（三选一）→ `image`/`modelPath` 覆盖 → `set` 覆盖 → chart 内 `values-genet.yaml` overlay（调度字段）→ 再覆盖一次 image/modelPath（`helm.go:2143-2196`，`R-G:76`）。平台**不做 Helm 渲染**，只把这六项原样透传并冻结（§9.8）。

**幂等键**：`release_name = "tp-" + hex(sha256(run_id + ":" + deploy_id))[:12]`；同 run 同 deploy 恒等、不同 deploy 必不同，满足 `^[a-z][a-z0-9-]{0,52}$`（`R-G:319`）。调用顺序固定：

1. `GET /releases/{name}` → 200：直接进入 Poll（重启/重试场景）；
2. 404：`POST`（`noWait=true`）；**先落库 `release_name` 再 POST**（POST 超时可能已 apply，`R-G:320`）；
3. POST 返回 400 且文案含 `already has protected metadata` → 视为已存在，回到 1；
4. POST 403（访问时间窗 / 无可用节点 / Key 未绑 namespace）或 424（chart 仓库未配置）→ `blocked` + 原文进 `failure.message`；网络/5xx/鉴权失败 → 平台侧错误，Run `failed`（C-5.3）。

### 9.3 状态合成（Poll）

Genet 的就绪判定是**两根互不相干的轴**（`R-G:121-143`）：

- 轴 1 `release.status`（helm 原值）：`pending-install|pending-upgrade|pending-rollback` / `deployed` / `failed` / `uninstalling|uninstalled` / `superseded`；
- 轴 2 `callInfo.endpoint.status`：`pending`（无可用地址）/ `ready`（**地址已解析**，不代表能推理）/ `blocked`（入口不可用，终态）。

**合成规则**（每次 Poll 按序判定，命中即返回；`Phase` 只用 `ports.DeployState` 的五值 `queued|accepted|deploying|ready|blocked`）：

| # | Genet 观测 | `DeployState.Phase` | 平台动作 |
|---|---|---|---|
| 1 | `GET` 404，且 `now - accepted_at < 180s` 宽限 | `accepted` | 记 `state_detail="release not yet visible"`；继续轮询 |
| 2 | `GET` 404，超过宽限 | `blocked` | Run → `service_unavailable`；`failure.message` 带 404 原文 |
| 3 | `status` 以 `pending-` 开头，且 `pods[]` 为空或全部 `nodeName == ""` | `queued` | `reason` 取 `/api/open/queue/podgroups` 的 `phase` + 最新 condition（**无位次**）；队列查询失败不判 Run 失败 |
| 4 | `status` 以 `pending-` 开头，Pod 已调度（`nodeName != ""`）或已有 `ready=false` 的 Pod | `deploying` | 记 `pods[]` 快照；开始发现 Deploy 日志 Pod（§7.2） |
| 5 | `endpoint.status == "ready"` 且 `url` 非空 | `ready` | 写 `runs.endpoint`（URL **只取 Genet 返回值**）；**随后仍要跑 Pre-Check 才放行 Task**（§9.4） |
| 6 | `endpoint.status == "blocked"` | `blocked` | 终态；`failure.message = endpoint.message`（自由文本，无错误码） |
| 7 | `status ∈ {failed, superseded, uninstalling, uninstalled}` | `blocked` | 终态；`failure.message = release.description` + `operation-history?status=failed` 的 `message`（异步 install 的 helm stderr 只在这里） |
| 8 | `status == "deployed"` 但 `endpoint.status == "pending"` | `deploying` | **不可当 ready**：`noWait` 下 `deployed` 早于地址出现（`R-G:289`） |
| 9 | 超 `deploy_timeout_seconds` 仍非终态 | `blocked` | Run → `service_unavailable`（不是 `failed`，§7.5） |

**映射时的一致性约束：**

- `failed` 必须折叠成 `blocked`：**平台没有 Deploy `failed` 子态**（`ports` 五值 + C-5.3），部署失败一律由 Run 的 `error` 表达（`R-G:297`）；只有"平台调不通 Genet"（网络/5xx/鉴权/请求构造）才判 Run `failed`（C-5.3 表）。
- Poll 里看到 `uninstalling/uninstalled`，说明有人（对账/上一次的清理）正在卸载同一 release → `blocked` + `failure.message="release is being uninstalled"`，**不要**再发起 install。
- 状态合成是**启发式**：`accepted`/`deploying` 的分界靠"Pod 是否被调度"这一代理判据，Genet 没有"受理"事件（`R-G:295`）。这一条要写进 UI 文案与排障手册（§9.10 R3）。
- 404 宽限用**时间窗**（`accepted_at + 180s`，现网 36×5s 的经验值，`R-G:328`），不要用"连续 N 次"——退避后次数与时间不再线性。

### 9.4 ready ≠ 可用：ready 之后必须过 Pre-Check

`endpoint.status=ready` **只代表地址解析完成**：优先级 router → 有 hostIP 的 master Pod → ClusterIP Service（`<G>/backend/internal/handlers/helm.go:855-928`；`<G>/docs/HELM_RELEASE_CALL_ENDPOINT.md:44`）。Pod Ready 只体现在展示字段 `external[].ready` / `pods[].ready`，**不参与** endpoint 决策（`R-G:181`）。PD/router 分离时"router 先可达、model server 后注册"是常态（`R-G:411`）。

因此平台**不得**把 `deploy ready` 当作"可以跑测试"，放行判据仍是 C-4.5 的 Pre-Check 门控：`ready` → Run `deploying → testing` → 只把 `seq=0`（`resolved.precheck.injected=true` 的 Task）入队 → Pre-Check 通过后才放行其余 Task。这也是 D-32 与 R1 在实现上的唯一正确落点：**等待就绪**（平台轮询）与**确认可用**（Tester 探活）是两件事，后者必须在 Tester 里做（`R-G:435` 的现网做法：`GET /v1/models` 诊断 + `POST /v1/chat/completions` 轮询）。

### 9.5 轮询与退避

| 参数 | 默认 | 说明 |
|---|---|---|
| 单飞 | 每 release 一个 poller | `Poll` 幂等但**不得并发**：Genet 后端是 helm CLI 子进程，重复打会拖垮（`R-G:249`、`R-G:325`） |
| 节奏 | 前 30s 每 1s；之后 5s 起指数退避，上限 15s | 与 `control-plane §C-5.3` 一致；单请求 **10s** 超时 |
| 队列查询 | 每 5 次 Poll 一次（≈75s 量级） | `/api/open/queue/podgroups` 失败**不判 Run 失败**，只影响 `reason` 展示（`R-G:327`） |
| 总窗口 | `deploy_timeout_seconds` 默认 72h（K-6） | 排队计入窗口；取消不受此限制 |
| 列表接口 | 必须 `light=true` | 否则每个 release 触发 `helm get values` + `list pods`（`R-G:423`） |
| 请求超时建议 | connect 5s / 总 10s（Poll）；卸载 360s（force 后 120s） | `R-G:247` |

### 9.6 取消

Genet **没有 cancel 端点**，唯一手段是 `DELETE /releases/{name}`（本质是卸载，`R-G:198`）。两个竞态必须处理：

1. **release 还没出现**（异步 install 尚未落 release）：DELETE 只会 404 → "取消"不能靠一次 DELETE 就算完成；
2. **install 在飞**：DELETE 可能 409/500（metadata 操作 token 被占，`R-G:199`）。

因此取消流程固定为：**先落取消意图（`runs.deploy.cancel_requested_at`），再（可失败地）发起 DELETE，由对账循环持续补偿**：

| 步骤 | 动作 | 失败语义 |
|---|---|---|
| 1 | 写 `cancel_requested_at`（事务内，随 Run → `cancelling`） | 写库失败 = 取消失败，直接返回错误 |
| 2 | 事务外 `DELETE ?keepHistory=false&force=true` | 404 = release 未出现或已删：**保留意图**，等第 3 步 |
| 3 | 对账循环（每 60s）看到 `cancel_requested_at != null` 且 Run 仍 `cancelling` → 重试第 2 步 | 连续失败只记 `CLEANUP_FAILED`；Run 2 分钟兜底后仍进 `canceled`（C-6.1 第 4 条），**不悬挂** |
| 4 | 确认"删干净"（§9.7） | 未确认干净只记事件，不改 Run 终态（§6） |

### 9.7 卸载与"删干净"

| 项 | 事实 |
|---|---|
| 接口 | `DELETE /api/open/helm/releases/{name}?keepHistory=<bool>&force=<bool>`，同步阻塞 |
| 超时 | 常规 **10 分钟**；`force=true` **2 分钟** |
| 响应 | `{message, forcedPods[], secretsDeleted, cleanupErrors[]}` |
| 404 | release 不存在（先验 owner metadata）→ 视为"已删" |
| 前置 | 会校验当前 revision 的 manifest/hooks digest；`status` 仍 `pending-*` 时应先等其脱离（否则 409/500） |
| 证据 | `R-G:205-221`、`R-G:333-335`；`<G>/backend/internal/handlers/helm.go:3930-4131` |

**"删干净"的唯一可信判据（两个条件同时满足）**：`GET /releases/{name}` 返回 404（或 `status ∈ {uninstalled, uninstalling, ""}`）**且** `GET /releases/{name}/pods` 的 `items == []`（且该请求本身成功）。**不能**用 DELETE 的 200 当成功（`R-G:218-221`）；`pods` 查询失败 ≠ pods 为空（R9）。

清理顺序（对齐现网 `CLEANUP_VERIFY_TIMEOUT_SEC=300`）：常规 DELETE → 5 分钟未确认 → `force=true` 重试 → 再确认 2 分钟 → 仍不干净记 `CLEANUP_FAILED` + 告警，**不改 Run 终态**（§6、`control-plane §C-5.7`）。`cleanup.policy=never` 不调用卸载。

### 9.8 落库字段（`runs.deploy`）

字段的**完整形状与逐字段理由以 [`views/03-data-view.md §4.4`](../views/03-data-view.md) 为唯一权威**（已按本节结论改完）。本节只列适配层必须写入/读取的键，避免两处漂移：

| 分组 | 键 | 写入时机 |
|---|---|---|
| 定位与幂等 | `deploy_id`、`provider`、`namespace`、`release_name` | Start 前（先落库再 POST） |
| 包与版本 | `package_type`、`chart`、`chart_version` | 首次 Poll 回读后冻结 |
| 冻结输入 | `values`、`values_digest`、`template_path`、`image`、`model_path`、`model_name`、`set[]`、`no_wait`（恒 `true`） | Start 前 |
| 两层状态 | `state`（平台五值）、`genet_release_status`、`endpoint_status` | 每次 Poll |
| 队列 | `queue{phase,pending,inqueue,running}`（**无位次**） | 队列接口取到时 |
| Endpoint | `endpoint{url,model_name,model_path,protocol}` | `ready` 时回填；`endpoint_key` 由它算（C-8.5） |
| 时间线 | `accepted_at`、`queued_at`、`ready_at`、`last_polled_at` | 各阶段首次进入时 |
| 失败 / 取消 / 卸载 | `failure{code,message,retryable}`、`cancel_requested_at`、`uninstall{state,verified_at,force,cleanup_errors[]}` | 终态 / 取消 / 清理阶段 |

> **与 03 §4.4 的对齐结论（截至本节重写时）**：键名一致，无冲突。唯一需要注意的差异是 **`queue_position` 已被移除**（§4.4 明确"没有位次"），`ports.DeployState` 同样没有该字段；`DeployState.Reason` 只装 `phase` 与 PodGroup 阻塞原文（展示用），**不是位次**。

### 9.9 代码骨架（`internal/genet/dto.go`）

接口形状保留，语义按真实接口补。`Genet` 是**外部 API 客户端**；`ports.Deployer` 是编排层看到的三个方法，映射见本节表。

```go
// Genet 是 Genet OpenAPI 的最小客户端（execution §9）。
type Genet interface {
	// Install 发起一次 Helm release 安装（noWait=true）。幂等键 = releaseName（§9.2）。
	// 返回 202 的精简响应；同名已存在时返回 ErrReleaseExists。
	Install(ctx context.Context, req ReleaseInstall) (ReleaseReceipt, error)
	// GetRelease 是主轮询：status + callInfo.endpoint + pods（§9.3）。
	GetRelease(ctx context.Context, namespace, releaseName string) (ReleaseStatus, error)
	// ListPods 只取 Pod 列表（日志跟随与拓扑快照；release 已删后仍可查，返回空）。
	ListPods(ctx context.Context, namespace, releaseName string) ([]PodRef, error)
	// Uninstall 同步卸载（10min；force 2min）。“删干净”另由 GetRelease + ListPods 判定（§9.7）。
	Uninstall(ctx context.Context, req UninstallRequest) (UninstallResult, error)
	// QueueSnapshot 是队列水位（只有计数，没有位次）；best-effort，失败不判 Run 失败（§9.5）。
	QueueSnapshot(ctx context.Context) (QueueSnapshot, error)
}

type ReleaseInstall struct {
	ReleaseName  string     `json:"releaseName"`              // 必填，^[a-z][a-z0-9-]{0,52}$
	PackageType  string     `json:"-"`                        // stack | pd → 路径里的 {type}
	Values       string     `json:"values,omitempty"`         // 最终 values.yaml 文本
	TemplatePath string     `json:"templatePath,omitempty"`   // chart 内 models/*.yaml
	Image        string     `json:"image,omitempty"`
	ModelPath    string     `json:"modelPath,omitempty"`      // 必须 /mnt/model 前缀
	Set          []SetValue `json:"set,omitempty"`
	NoWait       bool       `json:"noWait"`                   // 恒 true（§9.2）
}

type SetValue struct{ Name, Value, Type string }

type ReleaseReceipt struct {
	ReleaseName   string
	Namespace     string
	PackageType   string
	Chart         string
	ChartVersion  string
	ReleaseStatus string // 202 响应体里恒 "pending-install"
}

type ReleaseStatus struct {
	ReleaseName    string
	Namespace      string
	PackageType    string
	Chart          string
	ChartVersion   string
	ReleaseStatus  string       // helm 原值：pending-install|deployed|failed|superseded|uninstalling|uninstalled
	Description    string       // helm 原文（失败取证）
	Revision       int
	Endpoint       EndpointInfo // callInfo.endpoint，权威接入点
	EndpointStatus string       // pending|ready|blocked（callInfo.endpoint.status 原值）
	Pods           []PodRef
	Model          string // callInfo.model → runs.deploy.model_name
	UpdatedAt      time.Time
}

type EndpointInfo struct {
	Status  string // pending|ready|blocked
	Source  string // external|router|service
	URL     string // OpenAI 兼容 base URL；平台原样使用，不解析、不改写
	Host    string
	Port    int
	Message string // blocked 时的自由文本原因
}

type PodRef struct {
	Name          string
	Phase         string
	DisplayStatus string
	NodeName      string
	PodIP         string
	HostIP        string
	Image         string
	Ready         bool
}

type UninstallRequest struct {
	Namespace   string
	ReleaseName string
	KeepHistory bool
	Force       bool
}

type UninstallResult struct {
	Message        string
	ForcedPods     []string
	SecretsDeleted int
	CleanupErrors  []string
}

type QueueSnapshot struct {
	QueueName string
	Phase     string // PodGroup phase：Pending|Inqueue|Running|Unknown|Completed
	Pending   int
	Inqueue   int
	Running   int
	Reason    string // PodGroup conditions 最新一条的 message/reason（原文）
	// 没有 QueuePosition：Genet 只给水位计数（R-G:106）；位次留给 Volcano CRD（§11）
}
```

**与 `ports.Deployer` 的映射**（适配层 `internal/genet/dto.go` + 客户端；MVP 用 `internal/genet/mocks`）：

| `ports.Deployer` | Genet 落点 | 必须补齐的语义 |
|---|---|---|
| `Start(ctx, DeployRequest{RunID, DeployID, Manifest, Timeout})` | `GET /releases/{name}` → `POST /uc-packages/{type}/releases` | 由 `DeployID` 派生确定性 `release_name`；先 GET 再 POST；把 `values/packageType/templatePath/image/modelPath/set` 从 `Manifest` 解出并冻结（§9.2） |
| `Poll(ctx, runID)` | `GET /releases/{name}`（+ 低频 `/queue/podgroups`） | 按 §9.3 合成 `Phase`；维护 `pods` 快照、`endpoint`、`reason`、`last_polled_at`、404 宽限 |
| `Cancel(ctx, runID)` | `DELETE /releases/{name}?keepHistory=false&force=true` | 404 视为"还没出现"→ 保留取消意图等对账补偿；409/500 退避重试（§9.6） |

不需要的能力（与 §1 的边界一致）：Helm 渲染、chart 选择、拓扑判断、Endpoint 自行推导——`URL` 只取 Genet 返回值（§9.3 #5）。

### 9.10 未确认与风险

> 以下均来自调研（`R-G:407-425`）的原始结论，未削弱。

| # | 风险 | 对平台的影响 | 应对 |
|---|---|---|---|
| R1 | **`ready` 不等于可服务**（`endpoint.status` 只看地址是否解析出来，`helm.go:855-928`） | 不额外探活就会把 404/5xx 派给 Tester，表现为"测试全挂" | 保留 Pre-Check 门控；Tester 侧要有 warmup 重试预算（§9.4） |
| R2 | **无排队位次**（只有 `running/pending/inqueue` 计数，`volcano_views.go:13-39`） | `queue_position` 只能为 null；UI 承诺"第 N 位"做不出来 | `runs.deploy` 不存位次；UI 展示"已等待时长 + 水位"（§9.8） |
| R3 | **`queued`/`accepted`/`deploying` 靠推断**（Genet 无受理事件、无队列状态） | 状态机可能"卡在 accepted"或"跳过 deploying" | 文档/UI 标注启发式；排障手册写明代理判据（§9.3 末） |
| R4 | **每日 23:00 Genet GC 可能挂起长驻服务**：`cleanup/lifecycle.go:119-334` 扫 `user-*` namespace 的单副本 Deployment/StatefulSet 与无主 Pod，**代码里未见对 Helm release 的豁免**（该文件 `grep helm` 无命中） | 长测跨夜后服务可能消失 | **必须实测**：建一个 release 跨 23:00 观察；必要时让 Genet 侧加 `genet.io/protected-until`（`lifecycle.go:41-52`） |
| R5 | **取消只在 DELETE 一条路上**（release 未出现时 404、install 在飞时 409/500） | 只发一次 DELETE 就认为取消成功 → 留下长驻 release | 先落取消意图 + 对账补偿（§9.6） |
| R6 | **共享池限制在 `/open/helm` 路由上是否生效存疑**：`openapi_helm.go:17` 注释称由 `auth.RequireNonSharedPool` 完成，但 `main.go:691-738` 的路由**未挂**该中间件 | 鉴权模型与注释不一致 | 不据此设计鉴权；以"Key 的 namespace/pool 绑定 + 访问时间窗"为准，向 Genet 团队确认 |
| R7 | **`releaseName` 无命名空间前缀**（与 `openapi.yaml:4175` 描述矛盾） | 同名 release 在不同 namespace 可共存 | `namespace + release_name` 一起当定位键（§9.1） |
| R8 | **Helm release 的 Service 靠命名约定**（`-router-service`/`-pod0-service`/`-engine-service`/`-ray-head-svc`） | 换 chart 版本若改名，`endpoint` 会退化成 `pending` 或选错 Service | 平台只被动接受 Genet 返回值，禁止自行推导 |
| R9 | **"已删干净"依赖两次查询的时序**（GET 404 + pods 为空；pods 查询失败 ≠ 空） | 误报清理成功 → 长驻 release 泄漏 | 区分"查询失败"与"为空"（§9.7） |
| R10 | **Genet 没有"服务租期"概念** | Run 结束后不删就永久占卡 | 清理兜底：`cleanup.policy` 三值 + 对账补偿 + `CLEANUP_FAILED` 告警 |
| R11 | **请求体大小/限流未知**（无 `MaxBytesReader`、无限流中间件） | 大 values（含证书）可能撞反向代理限制 | `values` 控在 100KB 内 + `values_digest` 校验 |
| R12 | **`GET /releases` 富化开销大** | 列表/对账拖垮后端 | 一律 `light=true` |
| R13 | **文档与实现漂移**（`docs/HELM_RELEASE_CALL_ENDPOINT.md` 未提 `noWait`/`release.status`/400-409-424 语义） | 按文档实现会错 | **以代码为准**；本节结论全部带源码行号 |
| R14 | **Genet 同时是"个人 Pod 平台"与"服务托管平台"，但服务保障很弱**（无 SLA、无租期、无健康检查、GC 可能挂起） | 不能当"服务编排器"用 | 平台自己承担生命周期收敛与清理兜底 |

### 9.11 被证伪的旧假设（回写清单）

| 旧假设（本节重写前） | 现状 | 处置 |
|---|---|---|
| `POST /deployments`、`GET /deployments/{id}` | 不存在；拉起/查询/卸载都在 `/api/open/helm/...`（§9.2/§9.3/§9.7） | 本节重写；`internal/genet/dto.go` 按 §9.9 重建 |
| `DeployRequest.parameters` 原样透传 | Genet 只认 `values/templatePath/image/modelPath/set`，`parameters` 会被 400 | `Manifest.deploy` 的 schema 必须含这几项；`parameters` 只在平台内部展开 |
| 幂等键 = `run_id` | Genet 无幂等头、无 request_id；重复 POST 同名 → **400** | 幂等键 = `release_name`（`sha256(run_id:deploy_id)[:12]`）+ 先 GET 再 POST |
| Genet 提供 `queue_position` | 只有水位计数，没有位次 | 删掉位次承诺；`runs.deploy.queue{phase,pending,inqueue,running}` |
| 部署有 `blocked` / `failed` 两态 | 平台 Deploy 子态只有五值，`failed` 折叠成 `blocked` | Run 终态由 `error` 表达（C-5.3） |
| "受理后超窗 → `service_unavailable`" 已足够 | 还要处理 `endpoint.status=ready` 但服务不可用 | Pre-Check 门控是必需路径（§9.4） |

## 10 Tester Registry
| 项 | 默认 |
|---|---|
| 来源与触发 | 平台镜像内 `/app/testers`（Dockerfile `COPY testers/ /app/testers/`；`TESTPLANE_TESTERS_DIR` 仅开发/灰度覆盖）。**不做运行时更新**（K-3）：启动时扫描并校验一次，不提供同步 API；坏 `tester.yaml` 在 CI 就该被拦住，运行时校验失败则**拒绝启动**（fail fast） |
| 范围 | 只看 `testers/*/` 根的 `tester.yaml` + `run.sh`；`build/`、`tests/`、`run-local.sh`、`.local/` 不参与（§5.5） |
| 输出 | 启动时 `SyncDiff{added, unchanged, changed, removed}` + 逐条 `FieldError{path, message}`；**同版本不同内容 → 拒绝启动**（`TESTER_VERSION_PINNED`，K-3） |

| # | 校验项（不过即 `TESTER_INVALID`，带字段路径） |
|---|---|
| 1–2 | 目录名 = `name`；`name` 小写 `[a-z0-9-]` 且 ≤63；`version` 语义化版本 `^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$` |
| 3 | `image` 非空（形态不限，不解析 tag/digest，§5.6） |
| 4 | `entry` **必须**等于 `/tester/run.sh` |
| 5 | `role ∈ {test, pre_check}`；`role=pre_check` 时 `on_failure` **必须** `abort_run`（§7.3） |
| 6 | `on_failure ∈ {continue, abort_run}` |
| 7 | 禁止字段 `requires` / `timeout_seconds` / `outputs` 出现即拒绝（附录 C） |
| 8 | `k8s` 仅允许 `node_selector`/`tolerations`/`annotations`/`security_context{run_as_user, fs_group}`；出现 `privileged`/`hostPath`/`hostNetwork`/`image`/`command` 即拒绝 |
| 9 | `resources.cpu`/`memory` 是合法 quantity，`gpu.count >= 0` |
| 10 | `concurrency.endpoint_exclusive` 为布尔；`pre_check` 必须 `true` |
| 11 | `mounts[].path` 绝对路径，不与平台保留路径冲突 |
| 12–13 | `parameters` 显式存在（可空对象）且每项有中文 `description`；`env.*` 有 `required`/`secret`/`description`，`secret: true` 必须有 `description` |
| 14 | `run.sh` 存在且可执行位（embed 检查 `fs.ModePerm & 0111`） |
| 15 | 目录内不得有 `*.yaml` 形态的 K8s 清单（`job.yaml`/`deployment.yaml` 等，§6.8） |

- **JSON Schema：** `schemas/tester.schema.json`（2020-12），手写 schema 为唯一源；附录 C 字段表由 `make doc-fields` 从 schema 抽取后人工校对，CI 校验两边字段集合一致。校验顺序：先 schema（结构）再 §10.2 业务规则（语义）。
- **版本不可覆盖：** `content_hash = sha256(规范化 tester.yaml + run.sh 字节)`。同 `name+version` 同 hash → **no-op**（不写库、不改 `updated_at`）；不同 hash → `TESTER_VERSION_PINNED`（409，§5.7）并**拒绝整批**。新版本只新增行，不覆盖旧行。
- **同步幂等与回滚：** 整批一个事务（upsert → insert 新版本 → 失败 `ROLLBACK`），重复调用得同一 diff；目录里消失的 Tester 不自动删，只标 `orphaned=true`，由 `admin` 显式下线。

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
| `slot_waiting` | 平台自身 Endpoint 槽位未拿到（非 K8s 侧） | control-plane（§6.7） |
| `queue_full` | `queue.status.state=Closed`，或 `allocated + 本次请求 > deserved` 且队列内 Pending > 0 | 本模块 |
| `insufficient_resources` | Pod `Unschedulable` 且 message 命中 `insufficient (cpu\|memory\|nvidia.com/gpu\|huawei.com/Ascend910)` | 本模块 |
| `gang_pending` | PodGroup `Pending` 且 `minMember > running`，或事件含 `gated by gang` | 本模块 |

计算顺序 `slot_waiting → queue_full → gang_pending → insufficient_resources`；都判定不了时给 `reason=unknown` + `detail.scheduler=unavailable`（`unknown` 只落在 `detail`，不作为枚举值——§6.9 只有四个值）；输出 `{reason, detail}`，`detail` 保留原始 message（截断 256B）。`QUEUED` **必须带 reason**（§6.9），不给空。

## 12 对应 §14.1 的测试点
| 验收项 | 测试点 | 方式 / 环境 |
|---|---|---|
| K8s Executor | 渲染字段齐全、`backoffLimit/restartPolicy/activeDeadlineSeconds` 正确；超时可终止（`sleep 600`，`timeout_seconds=20`）；取消可删 Job；同 `job_id` 重复提交只建一个 Job、spec 不同返回 `CONFLICT` | golden YAML 单测 + kind（断言 `TASK_TIMEOUT` 且 Pod 消失） |
| 产物持久化 | subPath 隔离（两 Run 并发互不可见）、删 Job 后产物仍可读 | kind + NFS（`local-path` 模拟） |
| Tester 契约 / 独立运行 | `result.json` 4 个反例（缺失/空/非法 JSON/缺 `metrics`）、唯一性、`statistic` 枚举、未知字段放行；`docker run <image> --url=… --model_name=…` 产出合法结果 | fixture 单测 + kind + CI docker + mock OpenAI |
| 日志 | stdout 落盘、杀 collector 后 gap 不伪造、**明文不替换**（K-5）；多 Pod 一 Pod 一文件、重启标记、200MB 分段、`pods.json` 区间 | kind（删副本 / 阈值调 1MiB）+ mock Genet（3 Pod + 人为 OOM） |
| Genet 适配 | 受理→ready / blocked / 调用失败 / 超时未就绪四条路径；多 Pod 主 URL | mock Genet（httptest + kind stub） |
| Tester Registry | 15 条校验逐条拒绝且给字段级错误；同版本不同内容 `TESTER_VERSION_PINNED`；同内容 no-op | 表驱动单测 + testcontainers PG |
| Task 状态机 | 杀 `controller` 重启后对账纠正，不误判 `FAILED` | kind |
| 失败策略 | Pre-Check 失败（mock OpenAI 返回 401）终止其余 Task 并清 Job | kind + mock Genet/OpenAI |
| 调度可见性（P1） | `waiting_reason` 四值：Queue 满 / gang 未满足 / 资源不足 / 槽位 | kind + Volcano chart |
| 配额与清理 | 超 10 GiB → `ARTIFACT_QUOTA_EXCEEDED`；孤儿 GC 与 `hold-until` 保护 | 单测（配额改 1MiB、fake clock）+ kind |

环境统一：kind（1 主 3 工）+ mock Genet + mock OpenAI server（可返回 401/5xx/正常 SSE）+ Volcano（仅调度可见性用例）。

## 13 未决问题
| 编号 | 问题 | 默认建议（未确认前按此实现） |
|---|---|---|
| **E-Q1** | ~~Genet REST 契约未定~~ **已关闭**：真实接口是 Helm release 语义（`POST /api/open/helm/uc-packages/{stack\|pd}/releases`、`GET/DELETE /api/open/helm/releases/{name}`），无部署单、无 request_id、无位次，详见 §9 与 `.data/research/genet-lifecycle.md` | 契约收敛在 `internal/genet/dto.go`（§9.9）；幂等键 = `release_name`；`noWait=true`；总窗口默认 **72h**（K-6）不进 Manifest；残留未确认项见 §9.10 |
| **E-Q2** | Pod 级失败原因与错误码枚举不足：`OOMKilled`/`Evicted`/`Preempted`/节点 NotReady 无专门码 | 归 `INTERNAL_ERROR` + `detail.k8s_reason` 保留原文，不自动重试；下版附录 D 增 `CONTAINER_OOM_KILLED`、`POD_EVICTED`（可重试）、`NODE_LOST` |
| **E-Q3** | 镜像拉取 secret 来源：附录 B 无 `imagePullSecrets` | 由 Execution Profile 的 `image_pull_secrets` 提供（Profile 是平台对象，不动 Job Spec 契约）；为空则不渲染 |
| **E-Q4** | 日志明文的安全接受 | **已拍板（K-5）**：平台不读 Secret、不做脱敏；Tester 不得主动打印凭据的契约保留，残余风险由团队接受 |
| **E-Q5** | NFS UID 约定：镜像 UID 不统一 | 默认 10001；有 `run_as_user` 时按该值建目录；都失败依赖 `fsGroup` + `0770`，并把"容忍目录已存在"写进契约 |
| **E-Q6** | 配额统计成本：NFS 上每分钟全量 walk 在数百并发下开销可观 | 默认 60s/Attempt，>200 活跃分 10 批；后续可改 Tester 上报 + 平台抽查 |
| **E-Q7** | Deploy 日志保留期与容量（日志不随 Pod 回收，磁盘有限） | 与 Run 产物同保留期，不提前删；单 Run Deploy 日志软上限 2 GiB，超出后续分段只记 head 并标 `truncated` |