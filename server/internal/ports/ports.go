// Package ports 是跨角色（控制面内核 / 编排 / 执行器 / 报告）之间的接口边界。
//
// 这是**接口约定文件**：由 Lead 维护，改它必须同时改两侧实现，并同步
// design/impl/control-plane.md §C-13 与 design/impl/execution.md §1。
//
// 约定：
//   - 接口由消费方声明（Go 惯例），这里集中放"跨团队"的那几条，避免各写一份签名对不上；
//   - 实现方在自己的包内实现，不需要 import ports（Go 结构化接口），但**方法签名必须逐字一致**；
//   - 外部 IO（K8s / NFS）只能在事务外调用，结果回来再开新事务。
//
// v2 模型（D-66～D-74）：Run→Task、Task→runner 执行记录（(task_id, attempt_no, seq) 定位）、
// Tester→Runner；业务输入走输入文件（AG_PARAMS_FILE），argv 恒空；产物三选一。
package ports

import (
	"context"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// ---- 状态机内核（02 §3.3）-------------------------------------------------

// Kernel 是唯一能改状态的地方；调用方必须已经在一个数据库事务里，
// 并且已经按 Task → runner 执行记录的顺序锁好行（02 §3.3、§3.8）。
type Kernel interface {
	Apply(ctx context.Context, tx pgx.Tx, tr domain.Transition) (domain.Applied, error)
}

// Notifier 在事务提交后把 (task_id, seq) 推给 SSE 订阅者（02 §3.7）。
// 必须是"提交后通知"，不要在事务里调用。
type Notifier interface {
	Notify(taskID string, seq int64)
}

// ---- 类型化门面（02 §3.4）-------------------------------------------------

// TaskMachine 是 Task（平台层）状态机门面。实现：store.Machines.Tasks。
type TaskMachine interface {
	// Enqueue: submitted → queued（落库完成，进入编排）。
	Enqueue(ctx context.Context, taskID, actor string) error
	// BeginPipeline: queued → running（首个 runner 入队；无部署阶段，D-70）。
	BeginPipeline(ctx context.Context, taskID, actor string) error
	// Finish: running → 终态（按 C-5.8 优先级判定；调用方先确认全管道终态）。
	Finish(ctx context.Context, taskID string, outcome TaskOutcome) error
	// Cancel: 任意非终态 → cancelling（clean 执行完才落 canceled，C-6.1）。
	Cancel(ctx context.Context, taskID, actor string) error
	// Fail: 任意非终态 → failed（平台侧不可恢复错误）。
	Fail(ctx context.Context, taskID string, f domain.Failure) error
}

// TaskOutcome 是 Finish 的判定结果（终态 + 失败详情）。
type TaskOutcome struct {
	Status  domain.TaskStatus
	Failure *domain.Failure
	Reason  string
}

// RunnerMachine 是 runner 执行记录（实例层）状态机门面。实现：store.Machines.Runners。
type RunnerMachine interface {
	// Queue: pending → queued（串行管道推进，C-5.4）。
	Queue(ctx context.Context, k domain.RunnerKey) error
	// Prepare: queued → preparing（取 permit/槽位 + 建 Job 句柄落库）。
	Prepare(ctx context.Context, k domain.RunnerKey, p PrepareParams) error
	// Start: preparing → running（容器已启动）。
	Start(ctx context.Context, k domain.RunnerKey) error
	// Succeed: running → succeeded（exit 0 且产物契约满足）。
	Succeed(ctx context.Context, k domain.RunnerKey, r RunnerResult) error
	// Fail: preparing|running → failed（非零退出 / 超时 / 结果非法 / EXPORT_INVALID）。
	Fail(ctx context.Context, k domain.RunnerKey, f domain.Failure) error
	// Cancel: preparing|running → canceled（取消）。
	Cancel(ctx context.Context, k domain.RunnerKey, actor string) error
	// Skip: pending|queued → skipped（reason: upstream_abort | canceled）。
	Skip(ctx context.Context, k domain.RunnerKey, reason, actor string) error
}

// PrepareParams 是 Prepare 的输入。
type PrepareParams struct {
	JobID        string
	JobNamespace string
	ExecutorRef  []byte
	// AcquireLease=true 时同一事务里取端点槽位（仅外接模式 endpoint_exclusive）。
	AcquireLease bool
	Exclusive    bool
}

// RunnerResult 是 runner 成功的附加事实（产物引用；只填真实存在的文件）。
type RunnerResult struct {
	ExportsRef string
	ResultRef  string
	LogRef     string
}

// ---- 执行器（K8s Job，附录 B spec 2.0）------------------------------------

// JobSpec 是 runner 执行记录 → batch/v1 Job 的渲染输入（附录 B：spec 2.0）。
// 幂等键 = (task_id, attempt_no, seq) 的派生；`args` **恒为空数组**（D-66：业务输入走
// 输入文件，AG_PARAMS_FILE 注入路径，平台不追加任何动态 argv）。
type JobSpec struct {
	SpecVersion string // 恒 "2.0"；Executor 遇到未知版本必须拒绝
	JobID       string
	TaskID      string
	AttemptNo   int
	Seq         int

	RunnerName    string
	RunnerVersion string
	Role          string   // test | pre_check | deploy | clean
	Image         string   // 镜像引用原样交给 K8s，平台不解析（K-3）
	Command       []string // Runner 的 entry（exec form 数组），不经 shell

	Env       map[string]string // 非敏感环境变量（含 AG_TASK_ID / AG_ATTEMPT / AG_RUNNER_SEQ / AG_PARAMS_FILE / AG_OUTPUT_DIR / AG_SERVED_*）
	SecretEnv []SecretEnvRef    // 敏感变量，引用 Secret Store 键，不传明文（§12.4）

	Namespace   string
	Labels      map[string]string // argus.io/task / argus.io/attempt / argus.io/runner-seq（D-66）
	Annotations map[string]string

	Resources domain.ResourceRequest
	Mounts    []domain.MountSpec
	// SharedMounts 是平台级统一共享挂载（D-63）：PVC source，渲染时读启用中的行（D-65）。
	SharedMounts []domain.SharedMountSpec
	// SharedMountsDisabled 由 Execution Profile 的 `sharedMounts: false` 置位（D-63 ③）。
	SharedMountsDisabled bool
	// OutputDir / InputDir 是宿主视角的产物/输入目录（§6.8：输出**就是**最终落盘位置）。
	// 渲染时按 §6.8 转 subPath；InputDir = attempts/<n>/input/<seq>/。
	OutputDir  string
	InputDir   string
	StoragePVC string
	StorageRO  bool

	TimeoutSeconds int
}

// SecretEnvRef 是 secret_env 的一项（附录 B：{name, secret_ref}，不传明文）。
type SecretEnvRef struct {
	Name      string `json:"name"`
	SecretRef string `json:"secret_ref"`
}

// JobSpecVersion2 是 spec 2.0 的版本 token。
const JobSpecVersion2 = "2.0"

// JobHandle 是执行器返回的 Job 句柄，由调用方存进 task_runners.executor_ref（03 §4.8）。
type JobHandle struct {
	Namespace string `json:"namespace"`
	JobName   string `json:"job"`
	PodName   string `json:"pod_name,omitempty"`
}

// JobOutcome 是 Job 的终局观测结果（附录 B Executor 返回结果）。
type JobOutcome struct {
	Handle      JobHandle
	Phase       string // succeeded | failed | timeout | canceled | unknown（附录 B 枚举）
	ExitCode    *int32
	Message     string
	ImageDigest string // 从 Pod status.containerStatuses[].imageID 采集（D-36：引用是要什么，digest 是实际跑了什么）
	// K8sReason 是 Pod 异常终止的 K8s 原文原因（Evicted / OOMKilled / …）。
	// 只对 EXECUTOR_POD_ERROR 有意义；编排层落进 Failure.detail.k8s_reason。
	K8sReason  string
	FinishedAt time.Time
	// LogRef / ArtifactRef 是执行器采集的落盘引用（相对 Task 目录）。
	LogRef      string
	ArtifactRef string
}

// JobObserver 由编排层实现，执行器在 Job 到终态时回调一次。
// 回调里只做"开新事务 + 状态转移"，不要做外部 IO。
type JobObserver interface {
	JobFinished(ctx context.Context, o JobOutcome) error
}

// Executor 是执行层的对外接口。实现 = client-go 直连 K8s API（附录 B）。
type Executor interface {
	// Submit 创建 Job 并把句柄落库（落库由调用方完成，见 executor_ref）。
	// 同一 job_id 重复提交返回既有作业（幂等键 = (task_id, attempt_no, seq) 的派生）。
	Submit(ctx context.Context, spec JobSpec) (JobHandle, error)
	// Watch 阻塞到 Job 终态或 ctx 取消；终态时回调 obs 恰好一次。
	Watch(ctx context.Context, h JobHandle, obs JobObserver) error
	// Cancel 置取消标记并删除 Job（foreground 级联删 Pod），等 Pod 消失（execution §5.2）。
	Cancel(ctx context.Context, h JobHandle) error
	// Logs 返回 runner 的 stdout/stderr 流（明文，不做脱敏，K-5）。
	Logs(ctx context.Context, h JobHandle) (io.ReadCloser, error)
	// Delete 删除 Job（Pod 由 Job controller 级联删除）：正常结束后的回收与 GC。
	// 调用方必须保证"执行记录终态已落库"；404 视为成功。
	Delete(ctx context.Context, h JobHandle, policy DeletePolicy) error
}

// DeletePolicy 是 Job 删除的级联策略（对应 K8s propagationPolicy）。
type DeletePolicy string

const (
	// DeleteBackground 让 Job controller 异步回收 Pod：正常结束后的清理用它。
	DeleteBackground DeletePolicy = "Background"
	// DeleteForeground 等 Pod 真正消失再返回：取消/超时收口用它，
	// 避免容器进程继续写 NFS（§5.2）。
	DeleteForeground DeletePolicy = "Foreground"
)

// ---- 结果校验与报告（C-7）-------------------------------------------------

// ResultValidator 校验一个 runner 执行记录的产物目录（C-7.1 / D-74）。
// 返回 nil 表示通过；否则返回语义化的 Failure（RESULT_MISSING / RESULT_INVALID /
// RUNNER_NONZERO_EXIT / EXPORT_INVALID ...）。
type ResultValidator interface {
	ValidateRunnerOutput(ctx context.Context, k domain.RunnerKey, role, outputDir string, exitCode *int32) *domain.Failure
}

// ReportWriter 生成 Task 终态后的 report.json / report.md / metadata.json /
// events.jsonl（C-7.2），写到 <storage-root>/<task_id>/ 下。
type ReportWriter interface {
	WriteTaskReport(ctx context.Context, taskID string) (path string, err error)
}

// ---- Runner 目录（02 §3.4 RunnerCatalog）----------------------------------

// RunnerFilter 是目录查询过滤。
type RunnerFilter struct {
	Role            string
	IncludeOrphaned bool
}

// RunnerCatalog 是 Runner 注册表的只读目录（02 §3.4：不是状态机门面）。
// 实现：registry.Registry（包一层即可）。
type RunnerCatalog interface {
	// Sync 扫描目录并同步进注册表（启动时一次，K-3）；有非法 runner.yaml /
	// 同版本内容变化 → 返回错误，平台拒绝启动。
	Sync(ctx context.Context, dir string) error
	// Resolve 按 name+version 取 RunnerSpec；version 为空取最高版本。
	Resolve(ctx context.Context, name, version string) (domain.RunnerSpec, error)
	// List 列出注册表。
	List(ctx context.Context, f RunnerFilter) ([]domain.RunnerSpec, error)
}

// ---- Pod 日志落盘（D-59） -------------------------------------------------

// PodInfo 是执行器的 Pod 视图（Deploy 日志落盘与拓扑快照用）。
type PodInfo struct {
	Name       string
	Namespace  string
	Phase      string
	Node       string
	Ready      bool
	CreatedAt  time.Time
	Containers []ContainerInfo
}

// ContainerInfo 是容器级事实（restarts 与上次退出原因决定"要不要补 previous 段"）。
type ContainerInfo struct {
	Name         string
	Restarts     int32
	LastExitCode int32
	LastExit     string // K8s reason：OOMKilled | Error | Completed | …
	StartedAt    time.Time
	FinishedAt   time.Time
}

// LogStreamOptions 对应 K8s PodLogOptions 的子集（只读）。
type LogStreamOptions struct {
	Container  string
	Follow     bool
	Timestamps bool
	Previous   bool // 重启前的上一份容器日志
	TailLines  *int64
	SinceTime  *time.Time
}

// PodLogSource 是 Executor 的**可选**能力：按 namespace 列 Pod、流式读任意容器日志。
//
// 为什么单独一个接口：runner 的 `Logs(handle)` 只覆盖"当前执行记录的容器"，
// 而部署 Pod 日志落盘要对 Genet namespace 里**任意 Pod/容器**流式抓（含 previous
// 重启段，§8.6）。不实现它时编排层退化为"只实时读、不落盘"，不影响主流程。
type PodLogSource interface {
	ListPods(ctx context.Context, namespace string) ([]PodInfo, error)
	StreamLogs(ctx context.Context, namespace, pod, container string, opts LogStreamOptions) (io.ReadCloser, error)
}
