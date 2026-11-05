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
	FinishedAt  time.Time
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
}

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
type DeployState struct {
	Phase    string // queued | deploying | ready | blocked | failed
	Ready    bool
	Endpoint domain.Endpoint
	Failure  *domain.Failure
	Reason   string // 排队位次、阻塞原因等给人看的一句话
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
