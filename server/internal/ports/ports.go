// Package ports 是跨角色（控制面内核 / 编排 / 执行器 / 报告）之间的接口边界。
//
// 这是**接口约定文件**：由 Lead 维护，改它必须同时改两侧实现，并同步
// design/impl/control-plane.md §C-13 与 design/impl/execution.md §1。
//
// 约定：
//   - 接口由消费方声明（Go 惯例），这里集中放"跨团队"的那几条，避免各写一份签名对不上；
//   - 实现方在自己的包内实现，不需要 import ports（Go 结构化接口），但**方法签名必须逐字一致**；
//   - 外部 IO（K8s / Genet / NFS）只能在事务外调用，结果回来再开新事务。
package ports

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// Kernel 是唯一能改状态的地方；调用方必须已经在一个数据库事务里，
// 并且已经按 Run → Task → Attempt 的顺序锁好行（02 §3.3、§3.8）。
type Kernel interface {
	Apply(ctx context.Context, tx pgx.Tx, tr domain.Transition) (domain.Applied, error)
}

// Notifier 在事务提交后把 (run_id, seq) 推给 SSE 订阅者（02 §3.7）。
// 必须是"提交后通知"，不要在事务里调用。
type Notifier interface {
	Notify(runID string, seq int64)
}

// ---- 执行器（K8s Job）---------------------------------------------------

// JobSpec 是 Attempt → batch/v1 Job 的渲染输入。字段映射见
// design/impl/execution.md §2.1 与 design/ARCHITECTURE.md 附录 B。
type JobSpec struct {
	RunID     string
	TaskID    string
	AttemptID string
	AttemptNo int // 1-based，用于命名与 label

	TesterName    string
	TesterVersion string
	TesterRole    string // test | pre_check
	Image         string // 镜像引用原样交给 K8s，平台不解析（K-3）
	Entry         string // 容器内入口，如 /tester/run.sh
	Args          []string
	Env           map[string]string // 非敏感环境变量；凭据走 Secret，不放这里

	Namespace      string
	ServiceAccount string
	Labels         map[string]string
	Annotations    map[string]string

	Resources  domain.ResourceRequest
	Mounts     []domain.MountSpec
	OutputDir  string // 宿主视角的产物目录；渲染时按 §3.1 转成 subPath
	InputDir   string
	StoragePVC string
	StorageRO  bool

	TimeoutSeconds int

	// SecretEnv 是 tester.yaml 声明、由 Run 级 secrets 注入的变量名列表（值在 K8s Secret 里）。
	SecretEnv []string
}

// JobHandle 是执行器返回的 Job 句柄，由调用方存进 attempts.executor_ref（03 §4.8）。
type JobHandle struct {
	Namespace string `json:"namespace"`
	JobName   string `json:"job_name"`
	PodName   string `json:"pod_name,omitempty"`
}

// JobOutcome 是 Job 的终局观测结果。
type JobOutcome struct {
	Handle      JobHandle
	Phase       string // succeeded | failed | timeout | unknown
	ExitCode    *int32
	Message     string
	ImageDigest string // 从 Pod status.containerStatuses[].imageID 采集（execution §4.3.1）
	// K8sReason 是 Pod 异常终止的 K8s 原文原因（Evicted / OOMKilled / …）。
	// 只对 EXECUTOR_POD_ERROR 有意义；编排层落进 Failure.detail.k8s_reason。
	K8sReason  string
	FinishedAt time.Time
}

// JobObserver 由编排层实现，执行器在 Job 到终态时回调一次。
// 回调里只做"开新事务 + 状态转移"，不要做外部 IO。
type JobObserver interface {
	JobFinished(ctx context.Context, o JobOutcome) error
}

// Executor 是执行层的对外接口。MVP 实现 = client-go 直连 K8s API（list+watch），
// 不上 shared informer；周期 list 兜底（execution §4.1、§4.2）。
type Executor interface {
	// Submit 创建 Job 并把句柄落库（落库由调用方完成，见 executor_ref）。
	Submit(ctx context.Context, spec JobSpec) (JobHandle, error)
	// Watch 阻塞到 Job 终态或 ctx 取消；终态时回调 obs 恰好一次。
	Watch(ctx context.Context, h JobHandle, obs JobObserver) error
	// Cancel 置取消标记并删除 Job（foreground 级联删 Pod），等 Pod 消失（execution §5.2）。
	Cancel(ctx context.Context, h JobHandle) error
	// Logs 返回 Tester 的 stdout/stderr 流（明文，不做脱敏，K-5）。
	Logs(ctx context.Context, h JobHandle) (io.ReadCloser, error)
	// Delete 删除 Job（Pod 由 Job controller 级联删除）：正常结束后的回收与孤儿 GC
	// （execution §6）。调用方必须保证"Attempt 终态已落库"（§5.1 的顺序）；404 视为成功。
	Delete(ctx context.Context, h JobHandle, policy DeletePolicy) error
}

// DeletePolicy 是 Job 删除的级联策略（对应 K8s propagationPolicy）。
type DeletePolicy string

const (
	// DeleteBackground 让 Job controller 异步回收 Pod：正常结束后的清理用它
	// （此时不再需要 Pod 的实时状态，execution §6 的正常结束路径）。
	DeleteBackground DeletePolicy = "Background"
	// DeleteForeground 等 Pod 真正消失再返回：取消/超时收口用它，
	// 避免容器进程继续写 NFS（§5.2）。
	DeleteForeground DeletePolicy = "Foreground"
)

// ---- Deploy 阶段（MVP mock）------------------------------------------------

// DeployRequest 是一次部署请求。接口按真实 Genet 适配的形状定义，
// MVP 用 mocks.Genet 实现（不联网、按配置模拟耗时与失败），后续替换实现即可（D-14、§9）。
type DeployRequest struct {
	RunID    string
	DeployID string          // 幂等键：同一 run 的同一 deploy 只应有一次真实部署
	Manifest json.RawMessage // runs.deploy 的冻结内容
	Timeout  time.Duration
}

// DeployState 是轮询结果。Ready 时 Endpoint 必填（平台据此注入公共参数）。
//
// Phase 取值以 design/impl/control-plane.md §C-5.3 为准：
// queued | accepted | deploying | ready | blocked（没有 failed —— 部署失败体现为
// Run 的 error 字段 + Run 转 failed，Deploy 子状态本身不引入第二个失败态）。
type DeployState struct {
	Phase    string // queued | accepted | deploying | ready | blocked
	Ready    bool
	Endpoint domain.Endpoint
	Failure  *domain.Failure
	Reason   string // 排队位次、阻塞原因等给人看的一句话

	// ---- 落库用（runs.deploy §4.4）：适配层把 Genet 的原始观测一并带回，编排层只转存 ----
	// ReleaseStatus 是 helm 原值（pending-install|deployed|failed|superseded|…）。
	ReleaseStatus string
	// EndpointStatus 是 callInfo.endpoint.status 原值（pending|ready|blocked）。
	EndpointStatus string
	// Queue 是队列水位（只有计数，没有位次）。
	Queue *domain.DeployQueue
	// ObservedAt 是该观测的时间（写 last_polled_at）。
	ObservedAt time.Time
	// Namespace 是该 release 所在 namespace（Genet 原值，`Release.Namespace`）。
	//
	// 必须带回落库（`runs.deploy.namespace`）：Deploy 的 Pod 在 **Genet 的 namespace** 里，
	// 与平台自己的 namespace 不同（本地 `testplane-mock` / 生产 `testplane-svc`）。
	// 不写它，Deploy 日志端点只能回退到 TESTPLANE_NAMESPACE，结果"Pod 明明在跑却读不到日志"。
	Namespace string
	// ReleaseName 是 Genet 返回的 release 名（与平台冻结的幂等键一般一致，只用于对账）。
	ReleaseName string
	// PackageType / Chart / ChartVersion 是 Genet 侧的**实际**产物描述。
	// 用户 manifest 里可能没写、或写的是别名，Genet 会归一化；空值不覆盖库里的值（omitempty）。
	PackageType  string
	Chart        string
	ChartVersion string
}

// JobHolder 是 Executor 的**可选**能力：读取 Job 的 `testplane.io/hold-until` 守卫
// （日志 follower 未收尾时不许删 Job，execution §6.3）。
type JobHolder interface {
	HoldActive(ctx context.Context, h JobHandle) (bool, error)
}

// DeployCleaner 是 Deployer 的**可选**能力：卸载后校验"删干净"（execution §9.7）。
// 编排层用类型断言取用；不实现它的适配层（如老 mock）按"DELETE 成功即 verified"退化。
type DeployCleaner interface {
	// VerifyUninstalled 返回 true 表示 release 已不存在且其 Pod 列表为空。
	VerifyUninstalled(ctx context.Context, runID string) (bool, error)
}

// Deployer 是 Genet 适配层接口。MVP：mocks.Genet。
type Deployer interface {
	Start(ctx context.Context, req DeployRequest) error
	Poll(ctx context.Context, runID string) (DeployState, error)
	Cancel(ctx context.Context, runID string) error
}

// ---- 结果校验与报告 ------------------------------------------------------

// ResultValidator 校验一次 Attempt 的产物目录（execution §8、control-plane §C-7.1）。
// 返回 nil 表示通过；否则返回语义化的 Failure（RESULT_MISSING / RESULT_INVALID / TESTER_NONZERO_EXIT ...）。
type ResultValidator interface {
	Validate(ctx context.Context, runID, taskID, attemptID, outputDir string) *domain.Failure
}

// ReportWriter 生成 report.md / events.jsonl / 产物索引，写到 run 目录下（control-plane §C-7.2）。
type ReportWriter interface {
	WriteRunReport(ctx context.Context, runID string) (path string, err error)
}

// ---- Pod 日志落盘（D-59） -------------------------------------------------

// PodInfo 是执行器的 Pod 视图（Deploy 日志落盘与 pods.json 用）。
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
// 为什么单独一个接口：Tester 的 `Logs(handle)` 只覆盖"当前 Attempt 的 tester 容器"，
// 而 Deploy 日志落盘要对 Genet namespace 里**任意 Pod/容器**流式抓（含 previous 重启段）。
// 不实现它时编排层退化为"只实时读、不落盘"（旧行为），不影响主流程。
type PodLogSource interface {
	ListPods(ctx context.Context, namespace string) ([]PodInfo, error)
	StreamLogs(ctx context.Context, namespace, pod, container string, opts LogStreamOptions) (io.ReadCloser, error)
}
