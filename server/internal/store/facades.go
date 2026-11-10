package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 类型化门面（02 §3.4）：每个方法只做三件事 ——
// 参数校验 → 构造 Transition（含 From 集合与 payload）→ 在事务里调 Kernel.Apply。
//
// 门面自己开事务并**先锁 Task**（统一锁顺序 Task → runner 执行记录，02 §3.8），
// 提交后 NOTIFY。门面不得自己拼 SQL 改状态。

// Machines 聚合两个实体的门面。
type Machines struct {
	Tasks   *TaskMachine
	Runners *RunnerMachine
}

// NewMachines 装配门面；notifier 可为 nil（此时只写库不发通知）。
func NewMachines(s *Store, notifier *Notifier) *Machines {
	return &Machines{
		Tasks:   &TaskMachine{s: s, notifier: notifier},
		Runners: &RunnerMachine{s: s, notifier: notifier},
	}
}

// afterCommit 在事务提交后通知（02 §3.7：NOTIFY 只在提交后发）。
func afterCommit(n *Notifier, a domain.Applied, taskID string) {
	if n == nil || a.Noop || a.Seq <= 0 {
		return
	}
	n.Notify(taskID, a.Seq)
}

// ============================================================================
// Task 门面（C-3.2）
// ============================================================================

// TaskMachine 是 Task 状态机门面。
type TaskMachine struct {
	s        *Store
	notifier *Notifier
}

// Enqueue 执行 submitted → queued（落库完成，进入编排）。
func (m *TaskMachine) Enqueue(ctx context.Context, taskID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskSubmitted.Status()},
		To:      domain.TaskQueued.Status(),
		Command: "enqueue", Actor: actor,
	})
}

// BeginPipeline 执行 queued → running：编排器开始推进 runner 管道（无部署阶段，D-70）。
func (m *TaskMachine) BeginPipeline(ctx context.Context, taskID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskQueued.Status()},
		To:      domain.TaskRunning.Status(),
		Command: "pipeline_start", Actor: actor,
	})
}

// TaskOutcome 是 Finish 的判定结果（终态 + 失败详情）。
type TaskOutcome struct {
	Status  domain.TaskStatus
	Failure *domain.Failure
	Reason  string
}

// Finish 执行 running → 终态（判定见 domain.DecideTaskOutcome，C-5.8）。
// 调用方必须先确认所有 runner 已终态（clean finally 完成）。
func (m *TaskMachine) Finish(ctx context.Context, taskID string, out TaskOutcome) (domain.Applied, error) {
	if !out.Status.IsTerminal() {
		return domain.Applied{}, fmt.Errorf("store: finish task %s: %s 不是终态: %w",
			taskID, out.Status, domain.ErrPrecondition)
	}
	switch out.Status {
	case domain.TaskSucceeded, domain.TaskFailed, domain.TaskServiceUnavailable:
	default:
		return domain.Applied{}, fmt.Errorf("store: finish task %s: running 不能直接到 %s: %w",
			taskID, out.Status, domain.ErrIllegalState)
	}
	payload := map[string]any{}
	if out.Failure != nil {
		payload["error"] = *out.Failure
	}
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskRunning.Status()},
		To:      out.Status.Status(),
		Command: "finish", Reason: out.Reason, Actor: "system:orchestrator",
		Payload: payload,
	})
}

// Cancel 执行任意非终态 → cancelling（clean 执行完才落 canceled，C-6.1）。
func (m *TaskMachine) Cancel(ctx context.Context, taskID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    domain.TaskStatuses(domain.TaskNonTerminal()...),
		To:      domain.TaskCancelling.Status(),
		Command: "cancel", Actor: actor,
	})
}

// Canceled 执行 cancelling → canceled（调用方确认 clean 已收口或超时兜底）。
func (m *TaskMachine) Canceled(ctx context.Context, taskID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskCancelling.Status()},
		To:      domain.TaskCanceled.Status(),
		Command: "canceled", Actor: actor,
	})
}

// Fail 执行任意非终态 → failed（平台侧不可恢复错误）。
func (m *TaskMachine) Fail(ctx context.Context, taskID string, f domain.Failure) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    domain.TaskStatuses(domain.TaskNonTerminal()...),
		To:      domain.TaskFailed.Status(),
		Command: "fail", Reason: f.Code, Actor: "system:orchestrator",
		Payload: map[string]any{"error": f},
	})
}

func (m *TaskMachine) apply(ctx context.Context, tr domain.Transition) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := m.s.Tasks.Lock(ctx, tx, tr.ID); err != nil {
			return err
		}
		a, err := m.s.Apply(ctx, tx, tr)
		if err != nil {
			return err
		}
		applied = a
		return nil
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, tr.ID)
	// Task 落终态：提交后异步触发报告生成等外部动作（事务内禁止文件 IO）。
	if !applied.Noop && domain.TaskStatus(applied.To).IsTerminal() {
		m.s.fireTaskFinished(tr.ID)
	}
	return applied, nil
}

// ============================================================================
// runner 执行记录门面（C-3.1）
// ============================================================================

// RunnerMachine 是 runner 执行记录状态机门面。
type RunnerMachine struct {
	s        *Store
	notifier *Notifier
}

// Queue 执行 pending → queued（串行管道推进，C-5.4）。
func (m *RunnerMachine) Queue(ctx context.Context, k domain.RunnerKey) (domain.Applied, error) {
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerPending.Status()},
		To:      domain.RunnerQueued.Status(),
		Command: "queue", Actor: "system:orchestrator",
	})
}

// PrepareParams 是 preparing 的输入：取槽位后建 Job 所需的信息。
type PrepareParams struct {
	// JobID 可预分配（与 JobName 对应）；空则由 executor 侧生成后经 SetPhysicalFacts 回填。
	JobID        string
	JobNamespace string
	ExecutorRef  []byte
	// AcquireLease=true 时同一事务里取端点槽位（仅外接模式 endpoint_exclusive），取不到返回 ErrPrecondition。
	AcquireLease bool
	Exclusive    bool
}

// Prepare 执行 queued → preparing，并在同一事务里（可选）取端点租约。
func (m *RunnerMachine) Prepare(ctx context.Context, k domain.RunnerKey, p PrepareParams) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := m.s.Tasks.Lock(ctx, tx, k.TaskID); err != nil {
			return err
		}
		rr, err := m.s.RunnerRuns.Get(ctx, tx, k)
		if err != nil {
			return err
		}
		if p.AcquireLease && rr.EndpointKey != nil && *rr.EndpointKey != "" {
			ok, err := m.s.Leases.TryAcquire(ctx, tx, k, *rr.EndpointKey, p.Exclusive)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("store: prepare task_runner %s: 端点 %s 无可用槽位: %w",
					k.ID(), *rr.EndpointKey, domain.ErrPrecondition)
			}
		}
		a, err := m.s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRunnerRun, ID: k.ID(),
			From:    []domain.Status{domain.RunnerQueued.Status()},
			To:      domain.RunnerPreparing.Status(),
			Command: "prepare", Actor: "system:scheduler",
		})
		if err != nil {
			return err
		}
		applied = a
		// Job 句柄与状态推进同事务落库（C-2.5：启动 runner = permit + preparing + 事件）。
		return m.s.RunnerRuns.SetPhysicalFacts(ctx, tx, k, PhysicalFacts{
			JobID:        p.JobID,
			JobNamespace: nullableText(p.JobNamespace),
			ExecutorRef:  p.ExecutorRef,
		})
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, k.TaskID)
	return applied, nil
}

// Start 执行 preparing → running（informer 观察到容器已启动；写 started_at）。
func (m *RunnerMachine) Start(ctx context.Context, k domain.RunnerKey) (domain.Applied, error) {
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerPreparing.Status()},
		To:      domain.RunnerRunning.Status(),
		Command: "start", Actor: "system:executor",
	})
}

// RunnerResult 是 runner 成功的附加事实（03 §3.1 的产物引用列；只填真实存在的文件）。
type RunnerResult struct {
	ExportsRef string
	ResultRef  string
	LogRef     string
}

// Succeed 执行 running → succeeded（退出码 0 且产物契约满足）。
func (m *RunnerMachine) Succeed(ctx context.Context, k domain.RunnerKey, r RunnerResult) (domain.Applied, error) {
	payload := map[string]any{}
	if r.ExportsRef != "" {
		payload["exports_ref"] = r.ExportsRef
	}
	if r.ResultRef != "" {
		payload["result_ref"] = r.ResultRef
	}
	if r.LogRef != "" {
		payload["log_ref"] = r.LogRef
	}
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerRunning.Status()},
		To:      domain.RunnerSucceeded.Status(),
		Command: "succeed", Actor: "system:executor",
		Payload: payload,
	})
}

// Fail 执行 preparing|running → failed（非零退出 / 超时 / 结果非法 / EXPORT_INVALID）。
func (m *RunnerMachine) Fail(ctx context.Context, k domain.RunnerKey, f domain.Failure) (domain.Applied, error) {
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerPreparing.Status(), domain.RunnerRunning.Status()},
		To:      domain.RunnerFailed.Status(),
		Command: "fail", Reason: f.Code, Actor: "system:executor",
		Payload: map[string]any{"error": f, "error_code": f.Code},
	})
}

// Cancel 执行 preparing|running → canceled（取消；删除 Job，释放租约）。
func (m *RunnerMachine) Cancel(ctx context.Context, k domain.RunnerKey, actor string) (domain.Applied, error) {
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerPreparing.Status(), domain.RunnerRunning.Status()},
		To:      domain.RunnerCanceled.Status(),
		Command: "cancel", Actor: actor,
	})
}

// Skip 执行 pending|queued → skipped（reason 必须是 upstream_abort|canceled）。
func (m *RunnerMachine) Skip(ctx context.Context, k domain.RunnerKey, reason, actor string) (domain.Applied, error) {
	return m.apply(ctx, k, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: k.ID(),
		From:    []domain.Status{domain.RunnerPending.Status(), domain.RunnerQueued.Status()},
		To:      domain.RunnerSkipped.Status(),
		Command: "skip", Reason: reason, Actor: actor,
	})
}

func (m *RunnerMachine) apply(ctx context.Context, k domain.RunnerKey, tr domain.Transition) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		// 锁顺序 Task → task_runners（02 §3.8）；同 Task 的并发推进入口全部经
		// Task 行锁串行化，避免与取消/abort 交叉。
		if _, err := m.s.Tasks.Lock(ctx, tx, k.TaskID); err != nil {
			return err
		}
		a, err := m.s.Apply(ctx, tx, tr)
		if err != nil {
			return err
		}
		applied = a
		return nil
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, k.TaskID)
	return applied, nil
}

// TaskTerminalStatus 供 API 层判定——直接转调 domain 的唯一真源，
// 不在 store/api 里各留一份终态表。
func TaskTerminalStatus(st domain.TaskStatus) bool { return domain.TerminalTaskStatus(st) }
