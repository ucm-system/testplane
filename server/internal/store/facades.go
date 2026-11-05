package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 类型化门面（02 §3.4）：每个方法只做三件事 ——
// 参数校验 → 构造 Transition（含 From 集合与 payload）→ 在事务里调 Kernel.Apply。
//
// 门面自己开事务并**先锁 Run**（统一锁顺序 Run → Task → Attempt，02 §3.8），
// 提交后 NOTIFY。门面不得自己拼 SQL 改状态。

// Machines 聚合三个实体的门面。
type Machines struct {
	Runs     *RunMachine
	Tasks    *TaskMachine
	Attempts *AttemptMachine
}

// NewMachines 装配门面；notifier 可为 nil（此时只写库不发通知）。
func NewMachines(s *Store, notifier *Notifier) *Machines {
	return &Machines{
		Runs:     &RunMachine{s: s, notifier: notifier},
		Tasks:    &TaskMachine{s: s, notifier: notifier},
		Attempts: &AttemptMachine{s: s, notifier: notifier},
	}
}

// afterCommit 在事务提交后通知（02 §3.7：NOTIFY 只在提交后发）。
func afterCommit(n *Notifier, a domain.Applied, runID string) {
	if n == nil || a.Noop || a.Seq <= 0 {
		return
	}
	n.Notify(runID, a.Seq)
}

// ============================================================================
// Run 门面（R1–R10）
// ============================================================================

// RunMachine 是 Run 状态机门面。
type RunMachine struct {
	s        *Store
	notifier *Notifier
}

// RunOutcome 是 Finish 的判定结果（终态 + 失败详情）。
type RunOutcome struct {
	Status  domain.RunStatus
	Failure *domain.Failure
	Reason  string
}

// Enqueue 执行 R1：submitted → queued。
func (m *RunMachine) Enqueue(ctx context.Context, runID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunSubmitted.Status()},
		To:      domain.RunQueued.Status(),
		Command: "enqueue", Actor: actor,
	})
}

// BeginDeploy 执行 R2：queued → deploying（Genet 调用在事务外，由编排层先做）。
func (m *RunMachine) BeginDeploy(ctx context.Context, runID, deployID string) (domain.Applied, error) {
	deploy := map[string]any{
		"deployment_id": deployID,
		"state":         "queued",
		"queued_at":     time.Now().UTC().Format(time.RFC3339),
	}
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunQueued.Status()},
		To:      domain.RunDeploying.Status(),
		Command: "begin_deploy", Actor: "system:orchestrator",
		Payload: map[string]any{"deploy": deploy, "deployment_id": deployID},
	})
}

// BeginTesting 执行 R3：queued → testing（无 deploy 或 service 模式）。
func (m *RunMachine) BeginTesting(ctx context.Context, runID string, ep domain.Endpoint) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunQueued.Status()},
		To:      domain.RunTesting.Status(),
		Command: "begin_testing", Actor: "system:orchestrator",
		Payload: endpointPayload(ep, "service"),
	})
}

// DeployReady 执行 R4：deploying → testing，并在**同一事务**里回填 runs.endpoint、
// 全部 Task 的 endpoint_key 与注入后的 injected_argv（C-5.3 的硬要求）。
func (m *RunMachine) DeployReady(ctx context.Context, runID string, ep domain.Endpoint) (domain.Applied, error) {
	payload := endpointPayload(ep, "genet")
	payload["endpoint_key"] = ep.Key
	payload["print_argv"] = domain.PublicParamArgv(ep)
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunDeploying.Status()},
		To:      domain.RunTesting.Status(),
		Command: "deploy_ready", Actor: "system:orchestrator",
		Payload: payload,
	})
}

// DeployBlocked 执行 R5：deploying → service_unavailable。
func (m *RunMachine) DeployBlocked(ctx context.Context, runID string, f domain.Failure) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunDeploying.Status()},
		To:      domain.RunServiceUnavailable.Status(),
		Command: "deploy_blocked", Reason: f.Code, Actor: "system:orchestrator",
		Payload: map[string]any{"error": f, "deploy": map[string]any{"state": "blocked"}},
	})
}

// BeginCleanup 执行 R6：testing → cleanup（调用方先确认全部 Task 终态）。
func (m *RunMachine) BeginCleanup(ctx context.Context, runID, reason string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunTesting.Status()},
		To:      domain.RunCleanup.Status(),
		Command: "begin_cleanup", Reason: reason, Actor: "system:orchestrator",
	})
}

// Finish 执行 R7：cleanup → 终态（判定见 domain.DecideRunOutcome）。
func (m *RunMachine) Finish(ctx context.Context, runID string, out RunOutcome) (domain.Applied, error) {
	if !out.Status.IsTerminal() {
		return domain.Applied{}, fmt.Errorf("store: finish run %s: %s 不是终态: %w",
			runID, out.Status, domain.ErrPrecondition)
	}
	switch out.Status {
	case domain.RunSucceeded, domain.RunCompletedWithErrors, domain.RunServiceUnavailable, domain.RunFailed:
	default:
		return domain.Applied{}, fmt.Errorf("store: finish run %s: cleanup 不能直接到 %s: %w",
			runID, out.Status, domain.ErrIllegalState)
	}
	payload := map[string]any{}
	if out.Failure != nil {
		payload["error"] = *out.Failure
	}
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunCleanup.Status()},
		To:      out.Status.Status(),
		Command: "finish", Reason: out.Reason, Actor: "system:orchestrator",
		Payload: payload,
	})
}

// Cancel 执行 R8：任意非终态 → cancelling。
func (m *RunMachine) Cancel(ctx context.Context, runID, actor string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    domain.RunStatuses(domain.RunNonTerminal()...),
		To:      domain.RunCancelling.Status(),
		Command: "cancel", Actor: actor,
	})
}

// Canceled 执行 R9：cancelling → canceled（调用方确认 Attempt 已收口或超时兜底）。
func (m *RunMachine) Canceled(ctx context.Context, runID string) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    []domain.Status{domain.RunCancelling.Status()},
		To:      domain.RunCanceled.Status(),
		Command: "canceled", Actor: "system:orchestrator",
	})
}

// Fail 执行 R10：任意非终态 → failed（平台侧不可恢复错误）。
func (m *RunMachine) Fail(ctx context.Context, runID string, f domain.Failure) (domain.Applied, error) {
	return m.apply(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    domain.RunStatuses(domain.RunNonTerminal()...),
		To:      domain.RunFailed.Status(),
		Command: "fail", Reason: f.Code, Actor: "system:orchestrator",
		Payload: map[string]any{"error": f},
	})
}

func (m *RunMachine) apply(ctx context.Context, tr domain.Transition) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := m.s.Runs.Lock(ctx, tx, tr.ID); err != nil {
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
	// Run 落终态：提交后异步触发报告生成等外部动作（事务内禁止文件 IO）。
	if !applied.Noop && domain.RunStatus(applied.To).IsTerminal() {
		m.s.fireRunFinished(tr.ID)
	}
	return applied, nil
}

// ============================================================================
// Task 门面（T1–T11）
// ============================================================================

// TaskMachine 是 Task 状态机门面。
type TaskMachine struct {
	s        *Store
	notifier *Notifier
}

// PreparingTask 是 T3 的输入：取槽位后创建 Attempt 所需的信息。
type PreparingTask struct {
	JobID        string
	JobNamespace string
	ExecutorRef  []byte
	// Exclusive 来自 tester.yaml 的 concurrency.endpoint_exclusive。
	Exclusive bool
	// AcquireLease=true 时同一事务里取端点槽位，取不到返回 ErrPrecondition。
	AcquireLease bool
}

// Queue 执行 T1：pending → queued。
func (m *TaskMachine) Queue(ctx context.Context, taskID string) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPending.Status()},
		To:      domain.TaskQueued.Status(),
		Command: "queue", Actor: "system:orchestrator",
	})
}

// Prepare 执行 T3：queued → preparing，并在同一事务里创建 Attempt、取端点租约。
func (m *TaskMachine) Prepare(ctx context.Context, taskID string, p PreparingTask) (domain.Applied, *Attempt, error) {
	var (
		applied domain.Applied
		attempt *Attempt
		runID   string
	)
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		t, err := m.s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		runID = t.RunID
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		if p.AcquireLease && t.EndpointKey != nil && *t.EndpointKey != "" {
			ok, err := m.s.Leases.TryAcquire(ctx, tx, taskID, *t.EndpointKey, p.Exclusive)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("store: prepare task %s: 端点 %s 无可用槽位: %w",
					taskID, *t.EndpointKey, domain.ErrPrecondition)
			}
		}
		a, err := m.s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskQueued.Status()},
			To:      domain.TaskPreparing.Status(),
			Command: "prepare", Actor: "system:scheduler",
		})
		if err != nil {
			return err
		}
		applied = a
		created, err := m.s.Attempts.Create(ctx, tx, CreateAttemptParams{
			TaskID:       taskID,
			JobID:        p.JobID,
			JobNamespace: p.JobNamespace,
			ExecutorRef:  p.ExecutorRef,
		})
		if err != nil {
			return err
		}
		attempt = created
		return nil
	})
	if err != nil {
		return domain.Applied{}, nil, err
	}
	afterCommit(m.notifier, applied, runID)
	return applied, attempt, nil
}

// Start 执行 T5：preparing → running。
func (m *TaskMachine) Start(ctx context.Context, taskID string) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPreparing.Status()},
		To:      domain.TaskRunning.Status(),
		Command: "start", Actor: "system:executor",
	})
}

// TaskResult 是 Task 成功的附加事实。
type TaskResult struct {
	ResultRef string
}

// Succeed 执行 T8：running → succeeded。
func (m *TaskMachine) Succeed(ctx context.Context, taskID string, r TaskResult) (domain.Applied, error) {
	payload := map[string]any{}
	if r.ResultRef != "" {
		payload["result_ref"] = r.ResultRef
	}
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskRunning.Status()},
		To:      domain.TaskSucceeded.Status(),
		Command: "succeed", Actor: "system:executor",
		Payload: payload,
	})
}

// Fail 执行 T6/T9：preparing|running → failed。
func (m *TaskMachine) Fail(ctx context.Context, taskID string, f domain.Failure) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
		To:      domain.TaskFailed.Status(),
		Command: "fail", Reason: f.Code, Actor: "system:executor",
		Payload: map[string]any{"error": f},
	})
}

// Cancel 执行 T7/T10：preparing|running → canceled。
func (m *TaskMachine) Cancel(ctx context.Context, taskID, actor string) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
		To:      domain.TaskCanceled.Status(),
		Command: "cancel", Actor: actor,
	})
}

// Skip 执行 T2/T4：pending|queued → skipped（reason 必须是五值枚举之一）。
func (m *TaskMachine) Skip(ctx context.Context, taskID, reason string) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPending.Status(), domain.TaskQueued.Status()},
		To:      domain.TaskSkipped.Status(),
		Command: "skip", Reason: reason, Actor: "system:orchestrator",
	})
}

// Retry 执行 T11：failed → queued（available_at = now + backoff）。
func (m *TaskMachine) Retry(ctx context.Context, taskID string) (domain.Applied, error) {
	return m.apply(ctx, taskID, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskFailed.Status()},
		To:      domain.TaskQueued.Status(),
		Command: "retry", Actor: "system:orchestrator",
	})
}

func (m *TaskMachine) apply(ctx context.Context, taskID string, tr domain.Transition) (domain.Applied, error) {
	var (
		applied domain.Applied
		runID   string
	)
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		id, err := m.s.Tasks.RunIDOf(ctx, tx, taskID)
		if err != nil {
			return err
		}
		runID = id
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
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
	afterCommit(m.notifier, applied, runID)
	return applied, nil
}

// ============================================================================
// Attempt 门面（02 §2.3）
// ============================================================================

// AttemptMachine 是 Attempt 状态机门面。
type AttemptMachine struct {
	s        *Store
	notifier *Notifier
}

// AttemptResult 是 Attempt 结束的事实。
type AttemptResult struct {
	Status    domain.AttemptStatus
	Failure   *domain.Failure
	Phase     string
	ExitCode  *int32
	LogRef    string
	ResultRef string
}

// Create 插入一次 Attempt（idx 由调用方给或按 current_attempt+1）。
func (m *AttemptMachine) Create(ctx context.Context, taskID string, idx int, jobID string) (*Attempt, error) {
	var out *Attempt
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		runID, err := m.s.Tasks.RunIDOf(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		a, err := m.s.Attempts.Create(ctx, tx, CreateAttemptParams{TaskID: taskID, Idx: idx, JobID: jobID})
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	return out, err
}

// Start 推进 Attempt preparing → running；同事务把 Task preparing → running（T5）。
func (m *AttemptMachine) Start(ctx context.Context, attemptID string) (domain.Applied, error) {
	var (
		applied domain.Applied
		runID   string
	)
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		taskID, id, err := m.s.Attempts.Owner(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		runID = id
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		a, err := m.s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityAttempt, ID: attemptID,
			From:    []domain.Status{domain.AttemptPreparing.Status()},
			To:      domain.AttemptRunning.Status(),
			Command: "start", Actor: "system:executor",
		})
		if err != nil {
			return err
		}
		applied = a
		// Task 由 T5 同步推进；已不是 preparing 说明并发推进过，忽略冲突。
		_, err = m.s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskPreparing.Status()},
			To:      domain.TaskRunning.Status(),
			Command: "start", Actor: "system:executor",
		})
		if err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, runID)
	return applied, nil
}

// Finish 推进 Attempt 到终态（CAS；与自然结束/取消竞态时先落库者胜）。
func (m *AttemptMachine) Finish(ctx context.Context, attemptID string, r AttemptResult) (domain.Applied, error) {
	if !r.Status.IsTerminal() {
		return domain.Applied{}, fmt.Errorf("store: finish attempt %s: %s 不是终态: %w",
			attemptID, r.Status, domain.ErrPrecondition)
	}
	payload := map[string]any{}
	if r.Failure != nil {
		payload["error"] = *r.Failure
	}
	if r.Phase != "" {
		payload["phase"] = r.Phase
	}
	if r.ExitCode != nil {
		payload["exit_code"] = *r.ExitCode
	}
	if r.LogRef != "" {
		payload["log_ref"] = r.LogRef
	}
	if r.ResultRef != "" {
		payload["result_ref"] = r.ResultRef
	}
	return m.apply(ctx, attemptID, domain.Transition{
		Entity: domain.EntityAttempt, ID: attemptID,
		From:    []domain.Status{domain.AttemptPreparing.Status(), domain.AttemptRunning.Status()},
		To:      r.Status.Status(),
		Command: "finish", Actor: "system:executor",
		Payload: payload,
	})
}

// Cancel 取消一次 Attempt。
func (m *AttemptMachine) Cancel(ctx context.Context, attemptID, reason string) (domain.Applied, error) {
	return m.apply(ctx, attemptID, domain.Transition{
		Entity: domain.EntityAttempt, ID: attemptID,
		From:    []domain.Status{domain.AttemptPreparing.Status(), domain.AttemptRunning.Status()},
		To:      domain.AttemptCanceled.Status(),
		Command: "cancel", Reason: reason, Actor: "system:executor",
	})
}

func (m *AttemptMachine) apply(ctx context.Context, attemptID string, tr domain.Transition) (domain.Applied, error) {
	var (
		applied domain.Applied
		runID   string
	)
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		_, id, err := m.s.Attempts.Owner(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		runID = id
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
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
	afterCommit(m.notifier, applied, runID)
	return applied, nil
}

// endpointPayload 把 Endpoint 投影成 runs.endpoint 的 jsonb 形状（03 §4.3）。
func endpointPayload(ep domain.Endpoint, source string) map[string]any {
	endpoint := map[string]any{
		"url":        ep.BaseURL,
		"model_name": ep.ModelName,
		"model_path": ep.ModelPath,
		"key":        ep.Key,
		"source":     source,
	}
	return map[string]any{"endpoint": endpoint}
}
