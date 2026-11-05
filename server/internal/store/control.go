package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 本文件是控制面内核给编排层（orchestrator.Repos）的"组合事务"出口。
//
// 每个方法自己开事务并保证锁顺序 Run → Task → Attempt（02 §3.8），
// 对外只暴露事实（Applied / 受影响 id / Attempt 句柄），不暴露 pgx 类型。

// ---- 读（不加行锁）-------------------------------------------------------

// ListRunsByStatus 取指定状态的 Run（编排循环用）。
func (s *Store) ListRunsByStatus(ctx context.Context, statuses []string, limit int) ([]Run, error) {
	limit = normalizeLimit(limit)
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+`
		  FROM runs WHERE status = ANY($1) ORDER BY submitted_at LIMIT $2`, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list runs by status: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CandidateRow 是调度候选 + 编排层需要的附加字段。
type CandidateRow struct {
	TaskID      string
	RunID       string
	Seq         int
	EndpointKey string
	Exclusive   bool
	TesterRole  string
	SubmittedAt time.Time
}

// CandidatesDetailed 取调度候选（不加行锁，C-4.2），附带 Tester 角色与提交时间。
func (s *Store) CandidatesDetailed(ctx context.Context, limit int) ([]CandidateRow, error) {
	limit = normalizeLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.run_id, t.seq, COALESCE(t.endpoint_key, ''), t.endpoint_exclusive,
		       COALESCE(ts.role, 'test'), r.submitted_at
		  FROM tasks t
		  JOIN runs r ON r.id = t.run_id
		  LEFT JOIN testers ts ON ts.name = t.tester_name AND ts.version = t.tester_version
		 WHERE t.status = 'queued'
		   AND r.status = 'testing'
		   AND (t.available_at IS NULL OR t.available_at <= now())
		 ORDER BY r.submitted_at, t.seq
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: candidates detailed: %w", err)
	}
	defer rows.Close()
	var out []CandidateRow
	for rows.Next() {
		var c CandidateRow
		if err := rows.Scan(&c.TaskID, &c.RunID, &c.Seq, &c.EndpointKey, &c.Exclusive,
			&c.TesterRole, &c.SubmittedAt); err != nil {
			return nil, fmt.Errorf("store: scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpiredAttempt 是超时 watchdog 的输入（C-6.3）。
type ExpiredAttempt struct {
	AttemptID string
	TaskID    string
	RunID     string
	AttemptNo int
	JobName   string
}

// ExpiredAttempts 返回 started_at + timeout_seconds 已到期的活跃 Attempt。
func (s *Store) ExpiredAttempts(ctx context.Context, now time.Time) ([]ExpiredAttempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.task_id, t.run_id, a.idx, COALESCE(a.executor_ref->>'job', '')
		  FROM attempts a
		  JOIN tasks t ON t.id = a.task_id
		 WHERE a.status IN ('preparing','running')
		   AND a.started_at IS NOT NULL
		   AND a.started_at + make_interval(secs => t.timeout_seconds) < $1
		 LIMIT 100`, now)
	if err != nil {
		return nil, fmt.Errorf("store: expired attempts: %w", err)
	}
	defer rows.Close()
	var out []ExpiredAttempt
	for rows.Next() {
		var e ExpiredAttempt
		if err := rows.Scan(&e.AttemptID, &e.TaskID, &e.RunID, &e.AttemptNo, &e.JobName); err != nil {
			return nil, fmt.Errorf("store: scan expired attempt: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AttemptByJobName 用 K8s Job 名反查 Attempt（executor_ref->>'job'）。
// attempts 表没有 created_at 列（03 §3.2），排序用 idx（同一 Job 名只会命中一行）。
func (s *Store) AttemptByJobName(ctx context.Context, jobName string) (*Attempt, string, string, error) {
	var a Attempt
	var runID string
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.task_id, t.run_id, a.idx, a.status
		  FROM attempts a JOIN tasks t ON t.id = a.task_id
		 WHERE a.executor_ref->>'job' = $1 OR a.job_id = $1
		 ORDER BY a.idx DESC
		 LIMIT 1`, jobName).
		Scan(&a.ID, &a.TaskID, &runID, &a.Idx, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", fmt.Errorf("store: attempt by job %s: %w", jobName, domain.ErrNotFound)
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("store: attempt by job %s: %w", jobName, err)
	}
	return &a, a.TaskID, runID, nil
}

// ---- 调度 / 执行 ---------------------------------------------------------

// MarkAttemptRunning 执行 T5：同事务里
// 写 attempts 的 job_namespace/executor_ref + attempts preparing→running + tasks preparing→running。
func (s *Store) MarkAttemptRunning(ctx context.Context, attemptID, jobNamespace, jobName string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		taskID, runID, err := s.Attempts.Owner(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		ref, _ := json.Marshal(map[string]any{
			"namespace": jobNamespace, "job": jobName,
		})
		if err := s.Attempts.SetPhysicalFacts(ctx, tx, attemptID, PhysicalFacts{
			JobNamespace: nullableText(jobNamespace),
			ExecutorRef:  ref,
		}); err != nil {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityAttempt, ID: attemptID,
			From:    []domain.Status{domain.AttemptPreparing.Status(), domain.AttemptRunning.Status()},
			To:      domain.AttemptRunning.Status(),
			Command: "start", Actor: "system:executor",
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		_, err = s.Apply(ctx, tx, domain.Transition{
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
}

// QueueTasksTransition 把 Run 里的 pending Task 批量置 queued（走 Kernel，逐条写事件）。
// onlySeqZero=true 时只放 Pre-Check（C-5.4）。
func (s *Store) QueueTasksTransition(ctx context.Context, runID string, onlySeqZero bool) ([]string, error) {
	var queued []string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		// Pre-Check 放行判据（03-data-view.md §4.2）：
		// runs.resolved.precheck.injected == true **且** tasks.seq = 0。
		// 不能用 seq=0 单独判定 —— precheck:"off" 时测试 Task 也从 0 开始，
		// 会把 seq>0 的 Task 永久卡在 precheck_gate（评审 #14）。
		rows, err := tx.Query(ctx, `
			SELECT id FROM tasks
			 WHERE run_id = $1 AND status = 'pending' AND endpoint_key IS NOT NULL
			   AND ($2 = false OR (seq = 0 AND EXISTS (
			         SELECT 1 FROM runs r
			          WHERE r.id = $1
			            AND COALESCE((r.resolved -> 'precheck' ->> 'injected')::boolean, false))))
			 ORDER BY seq`, runID, onlySeqZero)
		if err != nil {
			return fmt.Errorf("store: queue tasks %s: %w", runID, err)
		}
		ids, err := collectIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := s.Apply(ctx, tx, domain.Transition{
				Entity: domain.EntityTask, ID: id,
				From:    []domain.Status{domain.TaskPending.Status()},
				To:      domain.TaskQueued.Status(),
				Command: "queue", Actor: "system:orchestrator",
			}); err != nil {
				if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPrecondition) {
					continue
				}
				return err
			}
			queued = append(queued, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queued, nil
}

// ---- 终态推进 ------------------------------------------------------------

// AttemptFinish 是 FinishAttempt 的组合输入。
type AttemptFinish struct {
	AttemptID   string
	TaskID      string
	RunID       string
	Succeeded   bool
	Phase       string
	ExitCode    *int32
	ImageDigest string
	Failure     *domain.Failure
	FinishedAt  time.Time
}

// AttemptFinishResult 汇报推进结果。
type AttemptFinishResult struct {
	AttemptApplied  domain.Applied
	TaskStatus      domain.TaskStatus
	TaskTerminal    bool
	RunAllTerminal  bool
	RunStatus       domain.RunStatus
	AbortTriggered  bool
	SkippedTasks    []string
	CancelAttempts  []string
	PreCheckPassed  bool
	RetryScheduled  bool
}

// FinishAttempt 落一次 Attempt 终态并推进 Task / Run（C-5.4、C-5.8）。
func (s *Store) FinishAttempt(ctx context.Context, in AttemptFinish) (*AttemptFinishResult, error) {
	out := &AttemptFinishResult{}
	var finishedRunID string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		taskID := in.TaskID
		runID := in.RunID
		if taskID == "" || runID == "" {
			tid, rid, err := s.Attempts.Owner(ctx, tx, in.AttemptID)
			if err != nil {
				return err
			}
			taskID, runID = tid, rid
		}
		finishedRunID = runID
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}

		// 1. Attempt 终态（CAS）
		attemptTo := domain.AttemptSucceeded
		if !in.Succeeded {
			attemptTo = domain.AttemptFailed
		}
		payload := map[string]any{}
		if in.Phase != "" {
			payload["phase"] = in.Phase
		}
		if in.ExitCode != nil {
			payload["exit_code"] = *in.ExitCode
		}
		if in.Failure != nil {
			payload["error"] = *in.Failure
		}
		applied, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityAttempt, ID: in.AttemptID,
			From:    []domain.Status{domain.AttemptPreparing.Status(), domain.AttemptRunning.Status()},
			To:      attemptTo.Status(),
			Command: "finish", Actor: "system:executor", Payload: payload,
			At: in.FinishedAt,
		})
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				// 先落库者胜：不覆盖已有结论。
				return nil
			}
			return err
		}
		out.AttemptApplied = applied

		if in.ImageDigest != "" {
			digest := in.ImageDigest
			_ = s.Attempts.SetPhysicalFacts(ctx, tx, in.AttemptID, PhysicalFacts{ImageDigest: &digest})
		}

		// 2. Task 终态
		t, err := s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if in.Succeeded {
			if _, err := s.Apply(ctx, tx, domain.Transition{
				Entity: domain.EntityTask, ID: taskID,
				From:    []domain.Status{domain.TaskRunning.Status()},
				To:      domain.TaskSucceeded.Status(),
				Command: "succeed", Actor: "system:executor", Payload: payload,
				At: in.FinishedAt,
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
			out.TaskStatus = domain.TaskSucceeded
			out.TaskTerminal = true
			// Pre-Check 通过：其余 Task 由调用方放行（NeedsQueueUnblocked）。
			if t.Seq == 0 && s.isPrecheck(ctx, tx, t) {
				out.PreCheckPassed = true
			}
		} else {
			failure := domain.NewFailure(domain.CodeInternalError, "执行失败", nil)
			if in.Failure != nil {
				failure = *in.Failure
			}
			if _, err := s.Apply(ctx, tx, domain.Transition{
				Entity: domain.EntityTask, ID: taskID,
				From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
				To:      domain.TaskFailed.Status(),
				Command: "fail", Reason: failure.Code, Actor: "system:executor",
				Payload: map[string]any{"error": failure}, At: in.FinishedAt,
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
			out.TaskStatus = domain.TaskFailed
			out.TaskTerminal = true
			// 重试：错误码在 retry_on 且 idx < max_attempts → failed→queued（T11）。
			if s.retryAllowed(t, failure.Code) {
				applied, err := s.Apply(ctx, tx, domain.Transition{
					Entity: domain.EntityTask, ID: taskID,
					From:    []domain.Status{domain.TaskFailed.Status()},
					To:      domain.TaskQueued.Status(),
					Command: "retry", Actor: "system:orchestrator",
					Payload: map[string]any{"error": failure}, At: in.FinishedAt,
				})
				if err == nil {
					out.RetryScheduled = true
					out.TaskTerminal = false
					_ = applied
				}
			}
		}

		// 3. abort_run：失败且 on_failure=abort_run → 终止其余未完成 Task（C-5.5）
		if !in.Succeeded && t.OnFailure == domain.OnFailureAbortRun {
			skipped, cancelAttempts, err := s.abortOthers(ctx, tx, runID, taskID, domain.SkipUpstreamAbort, in.FinishedAt)
			if err != nil {
				return err
			}
			out.AbortTriggered = true
			out.SkippedTasks = skipped
			out.CancelAttempts = cancelAttempts
		}

		// 4. 全部 Task 终态 → Run 进 cleanup 并判终态（C-5.8）
		nonTerminal, err := s.Tasks.CountNonTerminal(ctx, tx, runID)
		if err != nil {
			return err
		}
		run, err := s.Runs.Get(ctx, tx, runID)
		if err != nil {
			return err
		}
		out.RunStatus = domain.RunStatus(run.Status)
		if nonTerminal == 0 {
			out.RunAllTerminal = true
			out.RunStatus = s.finishRunLocked(ctx, tx, run, in.FinishedAt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Run 终态已提交：触发报告生成等外部动作（事务内禁止文件 IO）。
	// 覆盖"最后一个 Attempt 结束 → Run 终态"这条最常见路径（不只 RunMachine.Finish）。
	if out.RunStatus.IsTerminal() {
		s.fireRunFinished(finishedRunID)
	}
	return out, nil
}

// finishRunLocked 在已持 Run 锁的事务里做 testing→cleanup→终态判定（C-5.8）。
func (s *Store) finishRunLocked(ctx context.Context, tx pgx.Tx, run *Run, at time.Time) domain.RunStatus {
	cur := domain.RunStatus(run.Status)
	if cur == domain.RunTesting {
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRun, ID: run.ID,
			From:    []domain.Status{domain.RunTesting.Status()},
			To:      domain.RunCleanup.Status(),
			Command: "begin_cleanup", Actor: "system:orchestrator", At: at,
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return cur
		}
		cur = domain.RunCleanup
	}
	if cur != domain.RunCleanup {
		return cur
	}
	tasks, err := s.listTasks(ctx, tx, run.ID)
	if err != nil {
		return cur
	}
	// Pre-Check 的判别用 Tester 角色，**不能用 seq==0**：`precheck: "off"` 时唯一的
	// 测试 Task 也从 seq=0 开始，误判会把它的 TASK_TIMEOUT 当成 Pre-Check 可重试失败，
	// 把本该 failed 的 Run 判成 service_unavailable（QA 的 r-20261105-3J4R）。
	precheckIDs, err := s.PrecheckTaskIDs(ctx, tx, run.ID)
	if err != nil {
		return cur
	}
	input := domain.RunOutcomeInput{}
	var platformTimeout bool
	for _, t := range tasks {
		summary := domain.TaskSummary{Seq: t.Seq, Status: domain.TaskStatus(t.Status)}
		if len(t.Error) > 0 {
			// 用指针接 retryable：只有实例真的带了该字段才算"实例给了值"
			// （07-api.md §3：终态判定以实例字段为准，码级默认只做兜底）。
			var f struct {
				Code      string `json:"code"`
				Retryable *bool  `json:"retryable"`
			}
			if err := json.Unmarshal(t.Error, &f); err == nil {
				summary.ErrorCode = f.Code
				if f.Retryable != nil {
					summary.Retryable = *f.Retryable
					summary.HasRetryable = true
				}
			}
		}
		isPrecheck := precheckIDs[t.ID]
		if isPrecheck {
			pre := summary
			input.Precheck = &pre
		}
		// TASK_TIMEOUT 属 C-5.8 #3 的平台侧错误 → Run failed；但 Pre-Check 的超时
		// 仍走 #4（可重试 → service_unavailable），不能被这里的平台错误抢走。
		if !isPrecheck && summary.Status == domain.TaskFailed && summary.ErrorCode == domain.CodeTaskTimeout {
			platformTimeout = true
		}
		input.Tasks = append(input.Tasks, summary)
	}
	input.PlatformError = platformTimeout
	status := domain.DecideRunOutcome(input)
	payload := map[string]any{}
	if run.Endpoint != nil && len(run.Endpoint) > 0 {
		payload["endpoint"] = json.RawMessage(run.Endpoint)
	}
	// Run 终态要带 error（03 §3.1「终态/失败」）：取第一个失败的 Task 的 Failure，
	// Pre-Check 失败时优先用它（它决定 service_unavailable 的语义）。
	if status != domain.RunSucceeded && status != domain.RunCanceled {
		if f := firstTaskFailure(tasks, precheckIDs); f != nil {
			payload["error"] = *f
		}
	}
	if _, err := s.Apply(ctx, tx, domain.Transition{
		Entity: domain.EntityRun, ID: run.ID,
		From:    []domain.Status{domain.RunCleanup.Status()},
		To:      status.Status(),
		Command: "finish", Actor: "system:orchestrator", Payload: payload, At: at,
	}); err != nil && !errors.Is(err, domain.ErrConflict) {
		return cur
	}
	return status
}

// firstTaskFailure 取第一个可解释失败原因的 Task Failure（Pre-Check 优先，按角色判别）。
func firstTaskFailure(tasks []Task, precheckIDs map[string]bool) *domain.Failure {
	var precheck *domain.Failure
	var any *domain.Failure
	for i := range tasks {
		t := &tasks[i]
		if len(t.Error) == 0 {
			continue
		}
		var f domain.Failure
		if err := json.Unmarshal(t.Error, &f); err != nil || f.Code == "" {
			continue
		}
		if precheckIDs[t.ID] && precheck == nil {
			precheck = &f
		}
		if any == nil {
			any = &f
		}
	}
	if precheck != nil {
		return precheck
	}
	return any
}

// abortOthers 把同 Run 其余未完成的 Task 收口（C-5.5）。
func (s *Store) abortOthers(ctx context.Context, tx pgx.Tx, runID, failedTaskID, reason string,
	at time.Time) (skipped []string, activeAttempts []string, err error) {
	rows, err := tx.Query(ctx, `
		SELECT id, status FROM tasks
		 WHERE run_id = $1 AND id <> $2 AND status IN ('pending','queued') ORDER BY seq`,
		runID, failedTaskID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: abort others: %w", err)
	}
	type item struct{ id, status string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.status); err != nil {
			rows.Close()
			return nil, nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: it.id,
			From:    []domain.Status{domain.TaskPending.Status(), domain.TaskQueued.Status()},
			To:      domain.TaskSkipped.Status(),
			Command: "skip", Reason: reason, Actor: "system:orchestrator", At: at,
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return nil, nil, err
		}
		skipped = append(skipped, it.id)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET cancel_requested = true, updated_at = $2
		 WHERE run_id = $1 AND id <> $3 AND status IN ('preparing','running')`,
		runID, at, failedTaskID); err != nil {
		return nil, nil, fmt.Errorf("store: abort others: mark cancel: %w", err)
	}
	arows, err := tx.Query(ctx, `
		SELECT a.id FROM attempts a JOIN tasks t ON t.id = a.task_id
		 WHERE t.run_id = $1 AND t.id <> $2 AND a.status IN ('preparing','running')`,
		runID, failedTaskID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: abort others: attempts: %w", err)
	}
	activeAttempts, err = collectIDs(arows)
	return skipped, activeAttempts, err
}

func (s *Store) isPrecheck(ctx context.Context, tx pgx.Tx, t *Task) bool {
	var role string
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(role, '') FROM testers WHERE name = $1 AND version = $2`,
		t.TesterName, t.TesterVersion).Scan(&role)
	return err == nil && role == domain.RolePreCheck
}

// retryAllowed 判断 T11 是否成立：错误码在 retry_on 且 idx < max_attempts（03 §4.5）。
func (s *Store) retryAllowed(t *Task, code string) bool {
	if code == "" || len(t.RetryPolicy) == 0 {
		return false
	}
	var policy struct {
		MaxAttempts int      `json:"max_attempts"`
		RetryOn     []string `json:"retry_on"`
	}
	if err := json.Unmarshal(t.RetryPolicy, &policy); err != nil {
		return false
	}
	if policy.MaxAttempts <= 0 || t.CurrentAttempt >= policy.MaxAttempts {
		return false
	}
	for _, c := range policy.RetryOn {
		if c == code {
			return true
		}
	}
	return false
}

// ---- Run 阶段 ------------------------------------------------------------

// BeginTestingService 处理无 deploy 的 Run：queued → testing（endpoint 来自提交时的 service）。
// 返回该 Run 的 Task 总数。回填 endpoint_key/injected_argv 由 Kernel effect 完成。
func (s *Store) BeginTestingService(ctx context.Context, runID string) (int, error) {
	var count int
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		run, err := s.Runs.Get(ctx, tx, runID)
		if err != nil {
			return err
		}
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRun, ID: runID,
			From:    []domain.Status{domain.RunQueued.Status()},
			To:      domain.RunTesting.Status(),
			Command: "begin_testing", Actor: "system:orchestrator",
			Payload: map[string]any{"endpoint": json.RawMessage(run.Endpoint),
				"endpoint_key": endpointKeyOf(run.Endpoint)},
			At: time.Now().UTC(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE run_id = $1`, runID).Scan(&count)
	})
	return count, err
}

func endpointKeyOf(endpointJSON []byte) string {
	if len(endpointJSON) == 0 {
		return ""
	}
	var ep struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(endpointJSON, &ep); err != nil {
		return ""
	}
	return ep.Key
}

// RequestCancelTaskTx 取消单个 Task（C-6.2）：未开始 → skipped(canceled)，进行中 → canceled。
func (s *Store) RequestCancelTaskTx(ctx context.Context, taskID, actor string) ([]string, []string, error) {
	var skipped, activeAttempts []string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		t, err := s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if _, err := s.Runs.Lock(ctx, tx, t.RunID); err != nil {
			return err
		}
		switch domain.TaskStatus(t.Status) {
		case domain.TaskPending, domain.TaskQueued:
			if _, err := s.Apply(ctx, tx, domain.Transition{
				Entity: domain.EntityTask, ID: taskID,
				From:    []domain.Status{domain.TaskPending.Status(), domain.TaskQueued.Status()},
				To:      domain.TaskSkipped.Status(),
				Command: "skip", Reason: domain.SkipCanceled, Actor: actor,
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
			skipped = append(skipped, taskID)
		case domain.TaskPreparing, domain.TaskRunning:
			if _, err := s.Apply(ctx, tx, domain.Transition{
				Entity: domain.EntityTask, ID: taskID,
				From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
				To:      domain.TaskCanceled.Status(),
				Command: "cancel", Reason: domain.SkipCanceled, Actor: actor,
			}); err != nil && !errors.Is(err, domain.ErrConflict) {
				return err
			}
			rows, err := tx.Query(ctx, `
				SELECT id FROM attempts WHERE task_id = $1 AND status IN ('preparing','running')`, taskID)
			if err != nil {
				return fmt.Errorf("store: cancel task attempts: %w", err)
			}
			activeAttempts, err = collectIDs(rows)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return skipped, activeAttempts, err
}

// RequestCancelRunTx 取消 Run（C-6.1）：Run→cancelling，pending/queued 收口为 skipped，
// 进行中的 Task 置 cancel_requested 并返回其 Attempt（由调用方异步删 Job）。
func (s *Store) RequestCancelRunTx(ctx context.Context, runID, actor string) ([]string, []string, error) {
	var skipped, activeAttempts []string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRun, ID: runID,
			From: domain.RunStatuses(domain.RunNonTerminal()...),
			To:   domain.RunCancelling.Status(),
			Command: "cancel", Actor: actor, At: time.Now().UTC(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id FROM tasks WHERE run_id = $1 AND status = 'skipped'
			   AND status_reason = $2`, runID, domain.SkipCanceled)
		if err != nil {
			return fmt.Errorf("store: cancel run skipped: %w", err)
		}
		skipped, err = collectIDs(rows)
		if err != nil {
			return err
		}
		arows, err := tx.Query(ctx, `
			SELECT a.id FROM attempts a JOIN tasks t ON t.id = a.task_id
			 WHERE t.run_id = $1 AND a.status IN ('preparing','running')`, runID)
		if err != nil {
			return fmt.Errorf("store: cancel run attempts: %w", err)
		}
		activeAttempts, err = collectIDs(arows)
		return err
	})
	return skipped, activeAttempts, err
}

// listTasks 读 Run 的全部 Task（事务内）。
func (s *Store) listTasks(ctx context.Context, tx pgx.Tx, runID string) ([]Task, error) {
	rows, err := s.db(tx).Query(ctx, `SELECT `+taskCols+`
		  FROM tasks WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks %s: %w", runID, err)
	}
	return collectTasks(rows)
}
