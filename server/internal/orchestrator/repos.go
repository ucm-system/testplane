// Package orchestrator 是编排层：调度、Pre-Check 门控、Deploy 阶段、Attempt 终态推进与取消
// （design/impl/control-plane.md §C-4 ~ §C-6）。
//
// 分层约定（C-2.5、C-13）：
//   - 所有外部 IO（K8s、Genet）在事务外调用，结果回来再开新事务；
//   - 状态转移只由 kernel 落库，编排层只发起命令并处理返回的事实；
//   - 编排层不 import internal/store 的具体类型：持久化能力由本包的 Repos 接口表达，
//     backend-core 在 main 里写一个薄 adapter（或直接用 MemoryRepos 本地跑通）。
package orchestrator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// AttemptRef 指向一次物理执行。
type AttemptRef struct {
	AttemptID string
	TaskID    string
	RunID     string
	AttemptNo int // 1-based
	// JobName 是 K8s Job 名（tp-<task_id>-a<n>），来自 attempts.executor_ref.job。
	JobName string
	// JobID 是过渡期兼容字段：wire.go 里仍有旧字面量在用。新代码只写 JobName，
	// 读取一律走 Job()，backend-core 清理完旧字面量后这里可以删。
	JobID string
}

// Job 返回实际可用的 K8s Job 名（JobName 优先，兼容旧的 JobID）。
func (a AttemptRef) Job() string {
	if a.JobName != "" {
		return a.JobName
	}
	return a.JobID
}

// RunView 是编排层看到的 Run 快照（不含无关字段）。
type RunView struct {
	ID            string
	Status        domain.RunStatus
	Kind          string // run | task
	CleanupPolicy string
	HasDeploy     bool
	DeployID      string
	DeployState   string
	// DeployJSON 是 `runs.deploy` 的原样快照（adapter 读一个字段，不查第二次库）。
	// BeginDeploy 落库后由编排层作为 ports.DeployRequest.Manifest 交给 Deployer.Start
	// （execution §9.2：先冻结落库再 POST——POST 超时不代表没创建）。
	DeployJSON    json.RawMessage
	QueuePosition int
	QueueReason   string
	Endpoint      *domain.Endpoint
	Failure       *domain.Failure
	SubmittedAt   time.Time
}

// TaskView 是编排层看到的 Task 快照。
type TaskView struct {
	ID              string
	RunID           string
	Seq             int
	Name            string
	TesterName      string
	TesterVersion   string
	TesterRole      string // test | pre_check（仅展示/审计用，**不是**门控判据）
	// IsPrecheck 是唯一的门控与终态判据：runs.resolved.precheck.injected == true 且 seq == 0
	// （03-data-view.md §4.2）。由 adapter 用一条 JOIN 填好，编排层只读它。
	IsPrecheck      bool
	Image           string
	Entry           string
	Args            []string
	Env             map[string]string
	SecretEnv       []string
	OnFailure       string
	TimeoutSeconds  int
	Resources       domain.ResourceRequest
	Mounts          []domain.MountSpec
	Status          domain.TaskStatus
	StatusReason    string
	WaitingReason   string
	EndpointKey     string
	Exclusive       bool
	CancelRequested bool
	AttemptNo       int // current_attempt（已完成/进行中的次数）
	Failure         *domain.Failure
}

// Candidate 是调度候选（不加行锁的读，C-4.2）。
type Candidate struct {
	TaskID      string
	RunID       string
	Seq         int
	EndpointKey string
	Exclusive   bool
	TesterRole  string
	SubmittedAt time.Time
}

// ClaimRequest 是"取 permit + 建 Attempt"的原子入口输入（C-4.2/C-4.3/C-4.4）。
//
// store 侧的 `ClaimForExecution(ctx, ClaimRequest)` 在**一个事务**里完成
// 「锁 Run → 端点 advisory → 全局 advisory → 计数 → CAS Task→preparing → 建 Attempt + 租约」；
// 抢不到返回 claimed=false + reason（slot_waiting / global_limit），err 为 nil。
// 编排层不在 adapter 里再包事务，也不自己拼锁或计数（Lead 裁决 Q2）。
type ClaimRequest struct {
	TaskID      string
	EndpointKey string
	Exclusive   bool
	// GlobalLimit <= 0 表示不限制（配置项 TESTPLANE_MAX_GLOBAL_CONCURRENCY）。
	GlobalLimit int
	// AttemptIdx <= 0 表示用 current_attempt+1。
	AttemptIdx int
	// JobID 这里传 K8s Job 名（`tp-<task_id>-a<n>`，execution §2.3）；空则 store 自己派生。
	JobID        string
	JobNamespace string
	// ExecutorRef 是落库形状 {cluster,namespace,job}（03-data-view §4.8）。
	ExecutorRef []byte
}

// ClaimResult 是抢到 permit 后建出来的 Attempt 句柄。
type ClaimResult struct {
	AttemptID string
	// JobName 与 ClaimRequest.JobID 同源（`tp-<task_id>-a<n>`）。
	JobName   string
	AttemptNo int
}

// Attempt 把 ClaimResult 转成编排层内部句柄。
func (r ClaimResult) Attempt(taskID, runID string) AttemptRef {
	return AttemptRef{
		AttemptID: r.AttemptID,
		TaskID:    taskID,
		RunID:     runID,
		AttemptNo: r.AttemptNo,
	}
}

// FinishInput 是 Attempt 终态输入（JobObserver 回调里组装）。
type FinishInput struct {
	AttemptID   string
	TaskID      string
	RunID       string
	Phase       string // succeeded | failed | timeout | unknown
	ExitCode    *int32
	Message     string
	ImageDigest string
	Failure     *domain.Failure // 已由 ResultValidator 校验过的语义失败（可为 nil）
	FinishedAt  time.Time
}

// FinishOutcome 汇报推进结果，供编排层决定后续动作。
type FinishOutcome struct {
	Applied         domain.Applied
	TaskTerminal    bool
	RunAllTerminal  bool
	// AbortTriggered 表示该 Task 的失败触发了 abort_run（其余 Task 已处理）。
	AbortTriggered bool
	// CancelJobs 是需要异步删除 Job 的 Task（取消/abort 的尽力而为部分，C-5.5）。
	CancelJobs []AttemptRef
	// PreCheckPassed 且未 abort 时，编排层需要把 pending Task 入队。
	NeedsQueueUnblocked bool
}

// AbortInput 描述一次 abort_run。
type AbortInput struct {
	RunID       string
	FailedTask  string
	Reason      string // domain.SkipUpstreamAbort 等
	Failure     *domain.Failure
}

// AbortOutcome 汇报 abort 的落库结果。
type AbortOutcome struct {
	Skipped    []string
	CancelJobs []AttemptRef
	RunAllTerminal bool
}

// CancelOutcome 汇报取消的落库结果。
type CancelOutcome struct {
	Skipped    []string
	CancelJobs []AttemptRef
	AllStopped bool // 是否已无进行中的 Attempt（可推进到 canceled）
}

// Repos 是编排层对持久化的全部依赖。方法签名即 backend-core 需要的 adapter 契约。
//
// 事务边界由实现负责：方法内部自开事务并保证 Run → Task → endpoint advisory →
// global advisory → CAS Task 的锁顺序（02 §3.8、C-4.2、C-4.3、C-4.4）。
type Repos interface {
	// ---- 读（不加行锁）----
	GetRun(ctx context.Context, runID string) (RunView, error)
	GetTask(ctx context.Context, taskID string) (TaskView, error)
	ListTasks(ctx context.Context, runID string) ([]TaskView, error)
	Candidates(ctx context.Context, limit int) ([]Candidate, error)
	CountActiveAttempts(ctx context.Context) (int, error)
	ListExpiredAttempts(ctx context.Context, now time.Time) ([]AttemptRef, error)
	// ListRunsQueued 返回 queued 且需要进入 deploying/testing 的 Run（有 deploy 走 deploying）。
	ListRunsQueued(ctx context.Context, limit int) ([]RunView, error)
	// BeginTesting 处理无 deploy 的 Run：queued → testing + 回填 endpoint_key（C-5.3 的 service 模式）。
	BeginTesting(ctx context.Context, runID string) (int, error)
	// AttemptByJobName 用 K8s Job 名反查 Attempt（JobObserver 回调里用）。
	AttemptByJobName(ctx context.Context, jobName string) (AttemptRef, error)
	// ListRunsInDeploy 返回处于 deploying 且需要继续轮询 Genet 的 Run。
	ListRunsInDeploy(ctx context.Context, limit int) ([]RunView, error)
	// ListCancellingRuns 返回处于 cancelling 的 Run（用于兜底收口）。
	ListCancellingRuns(ctx context.Context, limit int) ([]RunView, error)

	// ---- 调度 ----
	// ClaimForExecution 是原子入口：claimed=false 时 reason 为 C-4.7 的等待原因
	// （slot_waiting / global_limit / precheck_gate ...），编排层写回 waiting_reason 后下轮重试。
	ClaimForExecution(ctx context.Context, req ClaimRequest) (res ClaimResult, claimed bool, reason string, err error)
	SetWaitingReason(ctx context.Context, taskID, reason string) error
	// MarkAttemptRunning 执行 T5：Submit 成功后 task→running、attempt→running、记 Job 句柄与 started_at。
	MarkAttemptRunning(ctx context.Context, attemptID string, h ports.JobHandle) error
	// MarkCanceled 把被取消的 Attempt/Task 落成 canceled 终态（Cancel 成功后的 CAS 收口）。
	MarkCanceled(ctx context.Context, attemptID string) error

	// ---- Task / Run 推进 ----
	QueueTasks(ctx context.Context, runID string, onlySeqZero bool) ([]string, error)
	FinishAttempt(ctx context.Context, in FinishInput) (FinishOutcome, error)
	AbortRun(ctx context.Context, in AbortInput) (AbortOutcome, error)

	// ---- Deploy（C-5.3）----
	BeginDeploy(ctx context.Context, runID, deployID string) error
	RecordDeployState(ctx context.Context, runID string, st ports.DeployState) error
	DeployReady(ctx context.Context, runID string, ep domain.Endpoint) (int, error)
	DeployBlocked(ctx context.Context, runID string, reason string, f *domain.Failure) error
	// FinishDeployAsFailed 处理"平台调不通 Genet"（Run → failed）。
	FinishDeployAsFailed(ctx context.Context, runID string, f *domain.Failure) error

	// ---- 收口 ----
	BeginCleanup(ctx context.Context, runID, reason string) error
	FinishRun(ctx context.Context, runID string, status domain.RunStatus, f *domain.Failure) error

	// ---- 取消 ----
	RequestCancelRun(ctx context.Context, runID, actor string) (CancelOutcome, error)
	RequestCancelTask(ctx context.Context, taskID, actor string) (CancelOutcome, error)
}

// ReconcileRepos 是「重启恢复与对账」（C-3.5）需要的额外查询。它与 Repos 分开声明，
// 因为对账属于**可选能力**：adapter 还没补齐时 controller 仍能启动（会打显式 WARN，
// 对账为 no-op），不会把整条链路卡住；补齐后自动生效。
//
// backend-core 的 adapter 需实现这 4 个方法（等价 SQL 即可）：
//
//	ListActiveTasks(ctx, limit) ([]TaskView, error)   -- tasks WHERE status IN ('preparing','running')
//	AttemptByTask(ctx, taskID)  (AttemptRef, error)   -- 进行中的 attempt；JobID 取 executor_ref->>'job'
//	DeleteLeaseByTask(ctx, taskID) error              -- DELETE FROM endpoint_leases WHERE task_id=$1
//	ListStaleLeases(ctx, limit) ([]string, error)     -- 租约 JOIN tasks，取已终态/已删除的 task_id
type ReconcileRepos interface {
	// ListActiveTasks 返回 status ∈ {preparing, running} 的 Task（对账的输入集合）。
	ListActiveTasks(ctx context.Context, limit int) ([]TaskView, error)
	// AttemptByTask 读该 Task 当前进行中的 Attempt（含 executor_ref 里的 Job 名）。
	AttemptByTask(ctx context.Context, taskID string) (AttemptRef, error)
	// DeleteLeaseByTask 幂等删除该 Task 的端点租约（C-4.3 的泄漏兜底）。
	DeleteLeaseByTask(ctx context.Context, taskID string) error
	// ListStaleLeases 返回"Task 已终态（或已不存在）但租约还在"的 task_id，
	// 用于清理旧版本/历史遗留的僵尸租约。
	ListStaleLeases(ctx context.Context, limit int) ([]string, error)
}