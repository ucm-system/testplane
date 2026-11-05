package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// MemoryRepos 是 Repos 的内存实现：单测的 fake，也是本地无 PG 跑通闭环的兜底。
//
// 它把"事务"简化为一把互斥锁，但保持与真实 store 相同的**语义承诺**：
//   - 取 permit 是原子的（Run 状态 → 门控 → 端点租约 → 全局并发 → CAS Task → 建 Attempt）；
//   - 终态落库先到先得（重复 finish 只记录一次）；
//   - Task 终态时释放租约；
//   - Run 在所有 Task 终态后按 C-5.8 的优先级收口。
type MemoryRepos struct {
	mu       sync.Mutex
	now      func() time.Time
	runs     map[string]*memRun
	tasks    map[string]*memTask
	attempts map[string]*memAttempt
	leases   map[string]string // taskID -> endpointKey
	// MaxGlobalConcurrencyOverride 覆盖请求里的上限（测试用）；0 表示按请求。
	MaxGlobalConcurrencyOverride int
}

type memRun struct {
	RunView
}

type memTask struct {
	TaskView
	// Precheck 表示 role=pre_check（与 TesterView.TesterRole 冗余，便于判定）。
	Precheck bool
}

type memAttempt struct {
	AttemptRef
	Status     domain.AttemptStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Timeout    time.Duration
}

// NewMemoryRepos 构造空的内存 Repos。
func NewMemoryRepos() *MemoryRepos {
	return &MemoryRepos{
		now:      time.Now,
		runs:     make(map[string]*memRun),
		tasks:    make(map[string]*memTask),
		attempts: make(map[string]*memAttempt),
		leases:   make(map[string]string),
	}
}

var _ Repos = (*MemoryRepos)(nil)

// SeedRun 预置一个 Run（测试与本地演示）。
func (m *MemoryRepos) SeedRun(r RunView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Status == "" {
		r.Status = domain.RunQueued
	}
	if r.CleanupPolicy == "" {
		r.CleanupPolicy = "always"
	}
	if r.SubmittedAt.IsZero() {
		r.SubmittedAt = m.now()
	}
	cp := r
	m.runs[r.ID] = &memRun{RunView: cp}
}

// SeedTask 预置一个 Task。
func (m *MemoryRepos) SeedTask(t TaskView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Status == "" {
		t.Status = domain.TaskPending
	}
	if t.TimeoutSeconds == 0 {
		t.TimeoutSeconds = 3600
	}
	if t.OnFailure == "" {
		t.OnFailure = domain.OnFailureContinue
	}
	cp := t
	precheck := t.IsPrecheck
	m.tasks[t.ID] = &memTask{TaskView: cp, Precheck: precheck}
}

// Attempts 返回全部 Attempt 快照（测试断言用）。
func (m *MemoryRepos) Attempts() []AttemptRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AttemptRef, 0, len(m.attempts))
	for _, a := range m.attempts {
		out = append(out, a.AttemptRef)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out
}

// Leases 返回当前持有的端点租约（测试断言"取消释放"）。
func (m *MemoryRepos) Leases() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.leases))
	for k, v := range m.leases {
		out[k] = v
	}
	return out
}

// SetNow 注入时钟。
func (m *MemoryRepos) SetNow(f func() time.Time) { m.now = f }

// ---- 读 --------------------------------------------------------------------

func (m *MemoryRepos) GetRun(ctx context.Context, runID string) (RunView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return RunView{}, fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	return r.RunView, nil
}

func (m *MemoryRepos) GetTask(ctx context.Context, taskID string) (TaskView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return TaskView{}, fmt.Errorf("memory: task %s: %w", taskID, domain.ErrNotFound)
	}
	return t.TaskView, nil
}

func (m *MemoryRepos) ListTasks(ctx context.Context, runID string) ([]TaskView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskView, 0, 8)
	for _, t := range m.tasks {
		if t.RunID == runID {
			out = append(out, t.TaskView)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *MemoryRepos) Candidates(ctx context.Context, limit int) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Candidate, 0, len(m.tasks))
	for _, t := range m.tasks {
		if t.Status != domain.TaskQueued {
			continue
		}
		r, ok := m.runs[t.RunID]
		if !ok || r.Status != domain.RunTesting {
			continue
		}
		out = append(out, Candidate{
			TaskID: t.ID, RunID: t.RunID, Seq: t.Seq,
			EndpointKey: t.EndpointKey, Exclusive: t.Exclusive,
			TesterRole: t.TesterRole, SubmittedAt: r.SubmittedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		if out[i].RunID != out[j].RunID {
			return out[i].RunID < out[j].RunID
		}
		return out[i].Seq < out[j].Seq
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryRepos) CountActiveAttempts(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.attempts {
		if a.Status == domain.AttemptPreparing || a.Status == domain.AttemptRunning {
			n++
		}
	}
	return n, nil
}

func (m *MemoryRepos) ListExpiredAttempts(ctx context.Context, now time.Time) ([]AttemptRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AttemptRef
	for _, a := range m.attempts {
		if a.Status != domain.AttemptPreparing && a.Status != domain.AttemptRunning {
			continue
		}
		if a.StartedAt.IsZero() {
			continue
		}
		if now.Sub(a.StartedAt) > a.Timeout {
			out = append(out, a.AttemptRef)
		}
	}
	return out, nil
}

func (m *MemoryRepos) ListRunsInDeploy(ctx context.Context, limit int) ([]RunView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RunView
	for _, r := range m.runs {
		if r.Status == domain.RunDeploying {
			out = append(out, r.RunView)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.Before(out[j].SubmittedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryRepos) ListCancellingRuns(ctx context.Context, limit int) ([]RunView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RunView
	for _, r := range m.runs {
		if r.Status == domain.RunCancelling {
			out = append(out, r.RunView)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---- 对账（C-3.5）----------------------------------------------------------

// ListActiveTasks 返回 status ∈ {preparing, running} 的 Task。
func (m *MemoryRepos) ListActiveTasks(ctx context.Context, limit int) ([]TaskView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TaskView
	for _, t := range m.tasks {
		if t.Status == domain.TaskPreparing || t.Status == domain.TaskRunning {
			out = append(out, t.TaskView)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AttemptByTask 读该 Task 进行中的 Attempt（没有则 ErrNotFound）。
func (m *MemoryRepos) AttemptByTask(ctx context.Context, taskID string) (AttemptRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.attempts {
		if a.TaskID == taskID && !a.Status.IsTerminal() {
			return a.AttemptRef, nil
		}
	}
	return AttemptRef{}, fmt.Errorf("memory: no active attempt for task %s: %w", taskID, domain.ErrNotFound)
}

// DeleteLeaseByTask 幂等删除租约。
func (m *MemoryRepos) DeleteLeaseByTask(ctx context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, taskID)
	return nil
}

// ListStaleLeases 返回"租约还在但 Task 已终态/不存在"的 task_id。
func (m *MemoryRepos) ListStaleLeases(ctx context.Context, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for taskID := range m.leases {
		t, ok := m.tasks[taskID]
		if !ok || t.Status.IsTerminal() {
			out = append(out, taskID)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SeedActiveTask 预置一个"僵尸"：Task 处于 running/preparing、Attempt 未终态、
// 且持有端点租约（用于对账单测）。
func (m *MemoryRepos) SeedActiveTask(t TaskView, attempt AttemptRef, leaseKey string, startedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := t
	cp.Status = domain.TaskRunning
	cp.AttemptNo = attempt.AttemptNo
	m.tasks[t.ID] = &memTask{TaskView: cp, Precheck: cp.IsPrecheck}
	m.attempts[attempt.AttemptID] = &memAttempt{
		AttemptRef: attempt,
		Status:     domain.AttemptRunning,
		StartedAt:  startedAt,
		Timeout:    time.Duration(t.TimeoutSeconds) * time.Second,
	}
	if leaseKey != "" {
		m.leases[t.ID] = leaseKey
	}
}

// ---- 调度 ------------------------------------------------------------------

// ClaimForExecution 原子地取 permit：Run 状态 → Pre-Check 门控 → 端点租约 →
// 全局并发 → CAS Task(queued→preparing) → 建 Attempt（C-4.2~C-4.5）。
func (m *MemoryRepos) ClaimForExecution(ctx context.Context, req ClaimRequest) (ClaimResult, bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[req.TaskID]
	if !ok {
		return ClaimResult{}, false, "", fmt.Errorf("memory: task %s: %w", req.TaskID, domain.ErrNotFound)
	}
	r, ok := m.runs[t.RunID]
	if !ok {
		return ClaimResult{}, false, "", fmt.Errorf("memory: run %s: %w", t.RunID, domain.ErrNotFound)
	}
	// 1. CAS：只有 queued 才能被领取（并发下第二个领取者拿不到）。
	if t.Status != domain.TaskQueued {
		return ClaimResult{}, false, "", nil
	}
	// 2. Run 必须处于 testing。
	if r.Status != domain.RunTesting {
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}
	// 3. endpoint_key 守卫（C-4.5）：没有 key 不允许调度，不得用占位键绕过。
	if t.EndpointKey == "" {
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}
	// 4. Pre-Check 门控：同 Run 的 pre_check 未成功前，其他 Task 不得领取。
	if !t.Precheck && m.hasPendingPrecheckLocked(r.ID) {
		return ClaimResult{}, false, domain.WaitPrecheckGate, nil
	}
	// 5. 端点槽位（C-4.3）：独占型在"任何持有者存在"时拒绝；共享型在"存在独占持有者"时拒绝。
	for taskID, key := range m.leases {
		if key != t.EndpointKey {
			continue
		}
		holder := m.tasks[taskID]
		if t.Exclusive || (holder != nil && holder.Exclusive) {
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		}
	}
	// 6. 全局并发上限（C-4.4）。
	limit := req.GlobalLimit
	if m.MaxGlobalConcurrencyOverride > 0 {
		limit = m.MaxGlobalConcurrencyOverride
	}
	if limit > 0 {
		active := 0
		for _, a := range m.attempts {
			if a.Status == domain.AttemptPreparing || a.Status == domain.AttemptRunning {
				active++
			}
		}
		if active >= limit {
			return ClaimResult{}, false, domain.WaitGlobalLimit, nil
		}
	}

	// 7. 落库：Task → preparing，建 Attempt，取租约。
	attemptNo := req.AttemptIdx
	if attemptNo <= 0 {
		attemptNo = t.AttemptNo + 1
	}
	attemptID := fmt.Sprintf("%s-a%d", t.ID, attemptNo)
	jobName := req.JobID
	if jobName == "" {
		jobName = executor.Naming(t.RunID, t.ID, attemptNo)
	}
	t.Status = domain.TaskPreparing
	t.WaitingReason = ""
	t.AttemptNo = attemptNo
	m.leases[t.ID] = t.EndpointKey
	m.attempts[attemptID] = &memAttempt{
		AttemptRef: AttemptRef{
			AttemptID: attemptID, TaskID: t.ID, RunID: t.RunID,
			AttemptNo: attemptNo, JobName: jobName,
		},
		Status:  domain.AttemptPreparing,
		Timeout: time.Duration(t.TimeoutSeconds) * time.Second,
	}
	return ClaimResult{AttemptID: attemptID, JobName: jobName, AttemptNo: attemptNo}, true, "", nil
}

// hasPendingPrecheckLocked 报告该 Run 是否还有未成功的 pre_check Task。
func (m *MemoryRepos) hasPendingPrecheckLocked(runID string) bool {
	for _, t := range m.tasks {
		if t.RunID != runID || !t.Precheck {
			continue
		}
		if t.Status != domain.TaskSucceeded {
			return true
		}
	}
	return false
}

func (m *MemoryRepos) SetWaitingReason(ctx context.Context, taskID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("memory: task %s: %w", taskID, domain.ErrNotFound)
	}
	if t.Status != domain.TaskQueued {
		return nil
	}
	t.WaitingReason = reason
	return nil
}

// MarkAttemptRunning 把 Task 推到 running（T5），并记录 Job 句柄与启动时间。
func (m *MemoryRepos) MarkAttemptRunning(ctx context.Context, attemptID string, h ports.JobHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[attemptID]
	if !ok {
		return fmt.Errorf("memory: attempt %s: %w", attemptID, domain.ErrNotFound)
	}
	if a.Status != domain.AttemptPreparing {
		return nil
	}
	a.Status = domain.AttemptRunning
	a.StartedAt = m.now()
	if t, ok := m.tasks[a.TaskID]; ok && t.Status == domain.TaskPreparing {
		t.Status = domain.TaskRunning
	}
	return nil
}

// ---- 推进 ------------------------------------------------------------------

// QueueTasks 把 pending → queued（Pre-Check 通过后 / precheck off）。
func (m *MemoryRepos) QueueTasks(ctx context.Context, runID string, onlySeqZero bool) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var queued []string
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		if onlySeqZero && t.Seq != 0 {
			continue
		}
		if t.Status != domain.TaskPending {
			continue
		}
		if t.EndpointKey == "" {
			// 守卫：queued 必须有 endpoint_key（C-4.5）。
			continue
		}
		t.Status = domain.TaskQueued
		t.WaitingReason = ""
		queued = append(queued, t.ID)
	}
	sort.Strings(queued)
	return queued, nil
}

// FinishAttempt 落库 Attempt/Task 终态，并处理门控放行、abort_run 与 Run 收口（C-5.4）。
func (m *MemoryRepos) FinishAttempt(ctx context.Context, in FinishInput) (FinishOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := FinishOutcome{}
	a, ok := m.attempts[in.AttemptID]
	if !ok {
		return out, fmt.Errorf("memory: attempt %s: %w", in.AttemptID, domain.ErrNotFound)
	}
	// 先落库者胜：已终态则只记竞态，不覆盖（execution §5.3）。
	if a.Status.IsTerminal() {
		return out, nil
	}
	t := m.tasks[a.TaskID]
	if t == nil {
		return out, fmt.Errorf("memory: task %s: %w", a.TaskID, domain.ErrNotFound)
	}

	var taskStatus domain.TaskStatus
	switch in.Phase {
	case "succeeded":
		a.Status = domain.AttemptSucceeded
		taskStatus = domain.TaskSucceeded
	case "timeout":
		a.Status = domain.AttemptFailed
		taskStatus = domain.TaskFailed
	default:
		a.Status = domain.AttemptFailed
		taskStatus = domain.TaskFailed
	}
	a.FinishedAt = in.FinishedAt
	if taskStatus == domain.TaskFailed {
		if in.Failure != nil {
			t.Failure = in.Failure
		} else {
			t.Failure = &domain.Failure{Code: domain.CodeInternalError, Message: in.Message}
		}
	}
	t.Status = taskStatus
	t.WaitingReason = ""
	out.TaskTerminal = true
	out.Applied = domain.Applied{From: domain.Status(domain.TaskRunning), To: domain.Status(taskStatus), Seq: 0}

	// 释放端点租约（与状态推进同事务，避免泄漏，C-4.3）。
	delete(m.leases, t.ID)

	// aborted：失败 + on_failure=abort_run → 其余 Task skipped（C-5.5）。
	if taskStatus == domain.TaskFailed && t.OnFailure == domain.OnFailureAbortRun {
		out.AbortTriggered = true
		out.CancelJobs = m.abortRunLocked(t.RunID, t.ID, in.Failure)
	}
	// Pre-Check 成功 → 其余 pending → queued。
	if taskStatus == domain.TaskSucceeded && t.Precheck {
		if n := m.queuePendingLocked(t.RunID); n > 0 {
			out.NeedsQueueUnblocked = true
		}
	}

	// Run 收口：全部 Task 终态 → cleanup → 终态（C-5.4 第 4 条、C-5.8）。
	if m.allTasksTerminalLocked(t.RunID) {
		out.RunAllTerminal = true
		m.finishRunLocked(t.RunID)
	}
	return out, nil
}

func (m *MemoryRepos) abortRunLocked(runID, failedTaskID string, f *domain.Failure) []AttemptRef {
	var cancelJobs []AttemptRef
	for _, t := range m.tasks {
		if t.RunID != runID || t.ID == failedTaskID {
			continue
		}
		switch t.Status {
		case domain.TaskPending, domain.TaskQueued:
			t.Status = domain.TaskSkipped
			t.StatusReason = domain.SkipUpstreamAbort
		case domain.TaskPreparing, domain.TaskRunning:
			t.CancelRequested = true
			for _, a := range m.attempts {
				if a.TaskID == t.ID && !a.Status.IsTerminal() {
					cancelJobs = append(cancelJobs, a.AttemptRef)
				}
			}
		}
	}
	return cancelJobs
}

func (m *MemoryRepos) queuePendingLocked(runID string) int {
	n := 0
	for _, t := range m.tasks {
		if t.RunID != runID || t.Status != domain.TaskPending || t.EndpointKey == "" {
			continue
		}
		t.Status = domain.TaskQueued
		n++
	}
	return n
}

func (m *MemoryRepos) allTasksTerminalLocked(runID string) bool {
	any := false
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		any = true
		if !t.Status.IsTerminal() {
			return false
		}
	}
	return any
}

// finishRunLocked 按 C-5.8 的优先级收口 Run。
func (m *MemoryRepos) finishRunLocked(runID string) {
	r := m.runs[runID]
	if r == nil || r.Status.IsTerminal() {
		return
	}
	if r.Status == domain.RunCancelling {
		// C-6.1 第 4 条：所有 Attempt 结束 → Run canceled。
		r.Status = domain.RunCanceled
		return
	}
	status, failure := TerminalRunStatus(m.taskViewsLocked(runID))
	r.Status = status
	if failure != nil {
		r.Failure = failure
	}
}

// taskViewsLocked 取该 Run 的 Task 快照（调用方已持锁）。
func (m *MemoryRepos) taskViewsLocked(runID string) []TaskView {
	out := make([]TaskView, 0, len(m.tasks))
	for _, t := range m.tasks {
		if t.RunID == runID {
			out = append(out, t.TaskView)
		}
	}
	return out
}

// AbortRun 由编排层显式触发（例如结果校验失败后发现 abort_run）。
func (m *MemoryRepos) AbortRun(ctx context.Context, in AbortInput) (AbortOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := AbortOutcome{CancelJobs: m.abortRunLocked(in.RunID, in.FailedTask, in.Failure)}
	if m.allTasksTerminalLocked(in.RunID) {
		out.RunAllTerminal = true
		m.finishRunLocked(in.RunID)
	}
	return out, nil
}

// ---- Deploy ---------------------------------------------------------------

func (m *MemoryRepos) BeginDeploy(ctx context.Context, runID, deployID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	if r.Status != domain.RunQueued {
		return nil
	}
	r.Status = domain.RunDeploying
	r.HasDeploy = true
	r.DeployID = deployID
	r.DeployState = "queued"
	return nil
}

func (m *MemoryRepos) RecordDeployState(ctx context.Context, runID string, st ports.DeployState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		r.DeployState = st.Phase
		r.QueueReason = st.Reason
	}
	return nil
}

func (m *MemoryRepos) DeployReady(ctx context.Context, runID string, ep domain.Endpoint) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return 0, fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	if ep.Key == "" {
		ep.Key = domain.NormalizeEndpointKey(ep.BaseURL)
	}
	r.Status = domain.RunTesting
	r.Endpoint = &ep
	r.DeployState = "ready"
	// 同事务回填全部 Task 的 endpoint_key（C-5.3）。
	backfilled := 0
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		t.EndpointKey = ep.Key
		backfilled++
	}
	// 有 Pre-Check 时只放 seq=0，其余保持 pending（C-5.4）。
	hasPrecheck := false
	for _, t := range m.tasks {
		if t.RunID == runID && t.Precheck {
			hasPrecheck = true
			break
		}
	}
	_ = hasPrecheck
	return backfilled, nil
}

func (m *MemoryRepos) DeployBlocked(ctx context.Context, runID, reason string, f *domain.Failure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	r.Status = domain.RunServiceUnavailable
	r.DeployState = "blocked"
	r.QueueReason = reason
	r.Failure = f
	m.skipAllPendingLocked(runID, domain.SkipServiceUnavailable)
	return nil
}

func (m *MemoryRepos) FinishDeployAsFailed(ctx context.Context, runID string, f *domain.Failure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	r.Status = domain.RunFailed
	r.Failure = f
	m.skipAllPendingLocked(runID, domain.SkipDeployFailed)
	return nil
}

func (m *MemoryRepos) skipAllPendingLocked(runID, reason string) {
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		if t.Status == domain.TaskPending || t.Status == domain.TaskQueued {
			t.Status = domain.TaskSkipped
			t.StatusReason = reason
			delete(m.leases, t.ID)
		}
	}
}

// ---- 收口 -----------------------------------------------------------------

// ListRunsQueued 返回 queued 状态的 Run（controller 据此进入 deploying/testing）。
func (m *MemoryRepos) ListRunsQueued(ctx context.Context, limit int) ([]RunView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RunView
	for _, r := range m.runs {
		if r.Status == domain.RunQueued {
			out = append(out, r.RunView)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.Before(out[j].SubmittedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// BeginTesting 处理无 deploy 的 Run：queued → testing 并回填 endpoint_key。
func (m *MemoryRepos) BeginTesting(ctx context.Context, runID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return 0, fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	if r.Status != domain.RunQueued {
		return 0, nil
	}
	key := ""
	if r.Endpoint != nil {
		key = r.Endpoint.Key
		if key == "" {
			key = domain.NormalizeEndpointKey(r.Endpoint.BaseURL)
			r.Endpoint.Key = key
		}
	}
	r.Status = domain.RunTesting
	backfilled := 0
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		if key != "" {
			t.EndpointKey = key
		}
		backfilled++
	}
	return backfilled, nil
}

// AttemptByJobName 用 Job 名反查 Attempt（名字规则与 executor 一致）。
func (m *MemoryRepos) AttemptByJobName(ctx context.Context, jobName string) (AttemptRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.attempts {
		if executor.Naming(a.RunID, a.TaskID, a.AttemptNo) == jobName {
			return a.AttemptRef, nil
		}
	}
	return AttemptRef{}, fmt.Errorf("memory: no attempt for job %q: %w", jobName, domain.ErrNotFound)
}

func (m *MemoryRepos) BeginCleanup(ctx context.Context, runID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok && r.Status == domain.RunTesting {
		r.Status = domain.RunCleanup
	}
	return nil
}

func (m *MemoryRepos) FinishRun(ctx context.Context, runID string, status domain.RunStatus, f *domain.Failure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	r.Status = status
	if f != nil {
		r.Failure = f
	}
	return nil
}

// ---- 取消 -----------------------------------------------------------------

func (m *MemoryRepos) RequestCancelRun(ctx context.Context, runID, actor string) (CancelOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := CancelOutcome{}
	r, ok := m.runs[runID]
	if !ok {
		return out, fmt.Errorf("memory: run %s: %w", runID, domain.ErrNotFound)
	}
	if r.Status.IsTerminal() {
		out.AllStopped = true
		return out, nil
	}
	active := 0
	for _, t := range m.tasks {
		if t.RunID != runID {
			continue
		}
		switch t.Status {
		case domain.TaskPending, domain.TaskQueued:
			t.Status = domain.TaskSkipped
			t.StatusReason = domain.SkipCanceled
			out.Skipped = append(out.Skipped, t.ID)
			delete(m.leases, t.ID)
		case domain.TaskPreparing, domain.TaskRunning:
			if !m.hasActiveAttemptLocked(t.ID) {
				// 库里"活着"但没有活动 Attempt（事务中断/旧版本遗留）：Task 直接收口 canceled，
				// 不给自己留一个永远等不到 Watch 的 cancel_requested（C-6.1）。
				t.Status = domain.TaskCanceled
				delete(m.leases, t.ID)
				out.Skipped = append(out.Skipped, t.ID)
				continue
			}
			t.CancelRequested = true
			active++
			for _, a := range m.attempts {
				if a.TaskID == t.ID && !a.Status.IsTerminal() {
					out.CancelJobs = append(out.CancelJobs, a.AttemptRef)
				}
			}
		}
	}
	sort.Strings(out.Skipped)
	if active == 0 {
		// C-6.1：没有正在跑的 Attempt → 取消直接收口 canceled，不停在 cancelling。
		r.Status = domain.RunCanceled
		out.AllStopped = true
		return out, nil
	}
	r.Status = domain.RunCancelling
	return out, nil
}

func (m *MemoryRepos) RequestCancelTask(ctx context.Context, taskID, actor string) (CancelOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := CancelOutcome{}
	t, ok := m.tasks[taskID]
	if !ok {
		return out, fmt.Errorf("memory: task %s: %w", taskID, domain.ErrNotFound)
	}
	switch t.Status {
	case domain.TaskPending, domain.TaskQueued:
		t.Status = domain.TaskSkipped
		t.StatusReason = domain.SkipCanceled
		out.Skipped = []string{t.ID}
		delete(m.leases, t.ID)
	case domain.TaskPreparing, domain.TaskRunning:
		if !m.hasActiveAttemptLocked(t.ID) {
			// 没有活动 Attempt：直接 canceled，不留 cancel_requested（C-6.2）。
			t.Status = domain.TaskCanceled
			delete(m.leases, t.ID)
			break
		}
		t.CancelRequested = true
		for _, a := range m.attempts {
			if a.TaskID == t.ID && !a.Status.IsTerminal() {
				out.CancelJobs = append(out.CancelJobs, a.AttemptRef)
			}
		}
	}
	out.AllStopped = true
	if m.allTasksTerminalLocked(t.RunID) {
		m.finishRunLocked(t.RunID)
	}
	return out, nil
}

// hasActiveAttemptLocked 报告该 Task 是否还有进行中的 Attempt（调用方已持锁）。
func (m *MemoryRepos) hasActiveAttemptLocked(taskID string) bool {
	for _, a := range m.attempts {
		if a.TaskID == taskID && !a.Status.IsTerminal() {
			return true
		}
	}
	return false
}
// MarkCanceled 把被取消的 Attempt/Task 落成 canceled 终态（Cancel 成功回调）。
func (m *MemoryRepos) MarkCanceled(ctx context.Context, attemptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attempts[attemptID]
	if !ok {
		return fmt.Errorf("memory: attempt %s: %w", attemptID, domain.ErrNotFound)
	}
	if a.Status.IsTerminal() {
		return nil
	}
	a.Status = domain.AttemptCanceled
	a.FinishedAt = m.now()
	if t, ok := m.tasks[a.TaskID]; ok && !t.Status.IsTerminal() {
		t.Status = domain.TaskCanceled
		delete(m.leases, t.ID)
	}
	if m.allTasksTerminalLocked(a.RunID) {
		m.finishRunLocked(a.RunID)
	}
	return nil
}

// 确保 encoding/json 被使用（ExecutorRef 是 JSONB 形状）。
var _ = json.Marshal
var _ = time.Now
