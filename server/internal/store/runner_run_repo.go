package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// RunnerRunRepo 是 task_runners 表（runner 执行记录；v1 的内层 tasks + 物理执行列，D-68）。
// 执行记录由 (task_id, attempt_no, seq) 唯一定位；状态只能由 Kernel CAS 改。
type RunnerRunRepo struct{ s *Store }

const runnerRunCols = `task_id, attempt_no, seq, name, runner_name, runner_version, role,
	image_ref, image_digest, profile, input_ref, on_failure, timeout_seconds,
	status, status_reason, waiting_reason, endpoint_key, endpoint_exclusive,
	exit_code, error_code, error, exports_ref, result_ref, log_ref,
	job_id, job_namespace, phase, executor_ref, cancel_requested,
	started_at, finished_at, created_at, updated_at`

func scanRunnerRun(row pgx.Row) (*RunnerRun, error) {
	var r RunnerRun
	err := row.Scan(&r.TaskID, &r.AttemptNo, &r.Seq, &r.Name, &r.RunnerName, &r.RunnerVersion,
		&r.Role, &r.ImageRef, &r.ImageDigest, &r.Profile, &r.InputRef, &r.OnFailure,
		&r.TimeoutSeconds, &r.Status, &r.StatusReason, &r.WaitingReason, &r.EndpointKey,
		&r.EndpointExclusive, &r.ExitCode, &r.ErrorCode, &r.Error, &r.ExportsRef,
		&r.ResultRef, &r.LogRef, &r.JobID, &r.JobNamespace, &r.Phase, &r.ExecutorRef,
		&r.CancelRequested, &r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func runnerRunScanCols() string { return runnerRunCols }

// Get 按三元组取执行记录。
func (r *RunnerRunRepo) Get(ctx context.Context, tx pgx.Tx, k domain.RunnerKey) (*RunnerRun, error) {
	rr, err := scanRunnerRun(r.s.db(tx).QueryRow(ctx,
		`SELECT `+runnerRunCols+` FROM task_runners WHERE task_id = $1 AND attempt_no = $2 AND seq = $3`,
		k.TaskID, k.AttemptNo, k.Seq))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: task_runner %s: %w", k.ID(), domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task_runner %s: %w", k.ID(), err)
	}
	return rr, nil
}

// GetByJobID 按 job_id（内部幂等键）取执行记录（executor 回调与对账用）。
func (r *RunnerRunRepo) GetByJobID(ctx context.Context, tx pgx.Tx, jobID string) (*RunnerRun, error) {
	rr, err := scanRunnerRun(r.s.db(tx).QueryRow(ctx,
		`SELECT `+runnerRunCols+` FROM task_runners WHERE job_id = $1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: task_runner by job %s: %w", jobID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task_runner by job %s: %w", jobID, err)
	}
	return rr, nil
}

// Insert 插入一行执行记录（提交 / 重试事务内按 resolved 全量生成）。
func (r *RunnerRunRepo) Insert(ctx context.Context, tx pgx.Tx, rr *RunnerRun) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO task_runners (task_id, attempt_no, seq, name, runner_name, runner_version, role,
		                          image_ref, profile, input_ref, on_failure, timeout_seconds,
		                          status, waiting_reason, endpoint_key, endpoint_exclusive,
		                          job_id, job_namespace, executor_ref, exit_code, error_code, error,
		                          exports_ref, result_ref, log_ref, phase, image_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		rr.TaskID, rr.AttemptNo, rr.Seq, rr.Name, rr.RunnerName, rr.RunnerVersion, rr.Role,
		rr.ImageRef, rr.Profile, rr.InputRef, rr.OnFailure, rr.TimeoutSeconds,
		rr.Status, rr.WaitingReason, rr.EndpointKey, rr.EndpointExclusive,
		rr.JobID, rr.JobNamespace, nullableBytes(rr.ExecutorRef),
		intPtrToAny(rr.ExitCode), textPtrToAny(rr.ErrorCode), nullableBytes(rr.Error),
		textPtrToAny(rr.ExportsRef), textPtrToAny(rr.ResultRef), textPtrToAny(rr.LogRef),
		textPtrToAny(rr.Phase), textPtrToAny(rr.ImageDigest))
	if err != nil {
		return fmt.Errorf("store: insert task_runner %s@%d/%d: %w", rr.TaskID, rr.AttemptNo, rr.Seq, err)
	}
	return nil
}

// ListByAttempt 返回某 attempt 的全部执行记录（按 seq 升序；详情页一条查询）。
func (r *RunnerRunRepo) ListByAttempt(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int) ([]RunnerRun, error) {
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+runnerRunCols+`
		  FROM task_runners WHERE task_id = $1 AND attempt_no = $2 ORDER BY seq`, taskID, attemptNo)
	if err != nil {
		return nil, fmt.Errorf("store: list task_runners %s#%d: %w", taskID, attemptNo, err)
	}
	return collectRunnerRuns(rows)
}

// ListByTask 返回该 Task 全部 attempt 的执行记录（历史不被覆盖）。
func (r *RunnerRunRepo) ListByTask(ctx context.Context, tx pgx.Tx, taskID string) ([]RunnerRun, error) {
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+runnerRunCols+`
		  FROM task_runners WHERE task_id = $1 ORDER BY attempt_no, seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list task_runners %s: %w", taskID, err)
	}
	return collectRunnerRuns(rows)
}

// Candidate 是调度候选（C-4.2）：**不加行锁**，并发由后续 Task 行锁 + CAS 兜住。
// 串行管道：只取各 attempt 内最小非终态 seq。
type Candidate struct {
	TaskID            string
	AttemptNo         int
	Seq               int
	EndpointKey       *string
	EndpointExclusive bool
}

// Candidates 取一批可调度的执行记录候选（C-4.2 的串行管道候选查询）。
func (r *RunnerRunRepo) Candidates(ctx context.Context, tx pgx.Tx, limit int) ([]Candidate, error) {
	limit = normalizeLimit(limit)
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT rr.task_id, rr.attempt_no, rr.seq, rr.endpoint_key, rr.endpoint_exclusive
		  FROM task_runners rr
		  JOIN tasks t ON t.id = rr.task_id
		 WHERE rr.status = 'queued'
		   AND t.status = 'running'
		   AND rr.seq = (SELECT min(p.seq) FROM task_runners p
		                  WHERE p.task_id = rr.task_id AND p.attempt_no = rr.attempt_no
		                    AND p.status IN ('pending','queued'))
		 ORDER BY t.submitted_at, rr.seq
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: task_runner candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.TaskID, &c.AttemptNo, &c.Seq, &c.EndpointKey, &c.EndpointExclusive); err != nil {
			return nil, fmt.Errorf("store: scan task_runner candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetWaitingReason 写调度等待原因（仅展示，C-4.7）。
func (r *RunnerRunRepo) SetWaitingReason(ctx context.Context, tx pgx.Tx, k domain.RunnerKey, reason string) error {
	var v *string
	if reason != "" {
		if !domain.ValidWaitingReason(reason) {
			return fmt.Errorf("store: waiting_reason %q 非法: %w", reason, domain.ErrPrecondition)
		}
		v = &reason
	}
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE task_runners SET waiting_reason = $4, updated_at = now()
		 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3`,
		k.TaskID, k.AttemptNo, k.Seq, v)
	if err != nil {
		return fmt.Errorf("store: set waiting reason %s: %w", k.ID(), err)
	}
	return nil
}

// PhysicalFacts 是执行层回写的物理事实（不参与状态判定，03 §3.1）。
// 只补物理字段，不改 status（状态只能由 Kernel.Apply 改）。
type PhysicalFacts struct {
	ExitCode     *int
	ImageDigest  *string
	JobID        string
	JobNamespace *string
	Phase        *string
	ExecutorRef  []byte
	ExportsRef   *string
	ResultRef    *string
	LogRef       *string
	ErrorCode    *string
	Error        []byte
}

// SetPhysicalFacts 回写物理字段（COALESCE 语义：非空才覆盖）。
func (r *RunnerRunRepo) SetPhysicalFacts(ctx context.Context, tx pgx.Tx, k domain.RunnerKey, f PhysicalFacts) error {
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE task_runners
		   SET exit_code     = COALESCE($4, exit_code),
		       image_digest  = COALESCE($5, image_digest),
		       job_id        = COALESCE(NULLIF($6, ''), job_id),
		       job_namespace = COALESCE($7, job_namespace),
		       phase         = COALESCE($8, phase),
		       executor_ref  = COALESCE($9, executor_ref),
		       exports_ref   = COALESCE($10, exports_ref),
		       result_ref    = COALESCE($11, result_ref),
		       log_ref       = COALESCE($12, log_ref),
		       error_code    = COALESCE($13, error_code),
		       error         = COALESCE($14, error),
		       updated_at    = now()
		 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3`,
		k.TaskID, k.AttemptNo, k.Seq,
		intPtrToAny(f.ExitCode), textPtrToAny(f.ImageDigest), f.JobID, f.JobNamespace,
		f.Phase, nullableBytes(f.ExecutorRef), textPtrToAny(f.ExportsRef),
		textPtrToAny(f.ResultRef), textPtrToAny(f.LogRef), textPtrToAny(f.ErrorCode),
		nullableBytes(f.Error))
	if err != nil {
		return fmt.Errorf("store: set physical facts %s: %w", k.ID(), err)
	}
	return nil
}

// CountNonTerminal 返回某 attempt 内还没到终态的执行记录数
// （Task 终态判定与 finally 收口的守卫输入）。
func (r *RunnerRunRepo) CountNonTerminal(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int) (int, error) {
	var n int
	err := r.s.db(tx).QueryRow(ctx, `
		SELECT count(*) FROM task_runners
		 WHERE task_id = $1 AND attempt_no = $2 AND NOT (status = ANY($3))`,
		taskID, attemptNo, domain.TerminalRunnerStatusStrings()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count non-terminal task_runners %s#%d: %w", taskID, attemptNo, err)
	}
	return n, nil
}

// CountActive 全局进行中（preparing/running）的执行记录数（C-4.4 全局并发 permit）。
func (r *RunnerRunRepo) CountActive(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM task_runners WHERE status IN ('preparing','running')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active task_runners: %w", err)
	}
	return n, nil
}

// StatesByJobNames 批量把 Job 名映射成执行记录状态（GC 与对账：禁止 N+1）。
type RunnerJobState struct {
	Found    bool
	Terminal bool
}

func (r *RunnerRunRepo) StatesByJobNames(ctx context.Context, tx pgx.Tx, jobNames []string) (map[string]RunnerJobState, error) {
	out := make(map[string]RunnerJobState, len(jobNames))
	if len(jobNames) == 0 {
		return out, nil
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT job_id, status FROM task_runners WHERE job_id = ANY($1)`, jobNames)
	if err != nil {
		return nil, fmt.Errorf("store: task_runner states by job names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var job, status string
		if err := rows.Scan(&job, &status); err != nil {
			return nil, fmt.Errorf("store: scan task_runner job state: %w", err)
		}
		out[job] = RunnerJobState{
			Found:    true,
			Terminal: domain.TerminalRunnerStatus(domain.RunnerStatus(status)),
		}
	}
	return out, rows.Err()
}

// WaitingBreakdown 返回等待中的 runner 按 waiting_reason 分组计数（/queue-status 用）。
func (r *RunnerRunRepo) WaitingBreakdown(ctx context.Context, tx pgx.Tx) (map[string]int, error) {
	out := map[string]int{
		domain.WaitSlotWaiting: 0, domain.WaitGlobalLimit: 0, domain.WaitQueueFull: 0,
		domain.WaitInsufficientResources: 0, domain.WaitGangPending: 0,
	}
	// 只统计**非终态 Task** 下的执行记录：历史脏数据不得灌进 KPI/等待列表。
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT COALESCE(rr.waiting_reason, 'unknown'), count(*)
		  FROM task_runners rr JOIN tasks t ON t.id = rr.task_id
		 WHERE rr.status IN ('pending','queued')
		   AND NOT (t.status = ANY($1))
		 GROUP BY 1`, domain.TerminalTaskStatusStrings())
	if err != nil {
		return nil, fmt.Errorf("store: waiting breakdown: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, fmt.Errorf("store: scan waiting breakdown: %w", err)
		}
		if _, known := out[reason]; known {
			out[reason] = n
		} else {
			out["unknown"] += n
		}
	}
	return out, rows.Err()
}

// WaitingRunner 是等待中执行记录的明细行（/queue-status 的 waiting.runners，C-4.7）。
type WaitingRunner struct {
	TaskID        string     `json:"task_id"`
	AttemptNo     int        `json:"attempt_no"`
	Seq           int        `json:"seq"`
	Name          string     `json:"name"`
	RunnerName    string     `json:"runner_name"`
	Status        string     `json:"status"`
	WaitingReason string     `json:"waiting_reason"`
	EndpointKey   *string    `json:"endpoint_key"`
	Since         *time.Time `json:"since"` // 进入当前状态的 updated_at（C-4.6 FIFO 观测）
}

// ListWaiting 返回等待中（pending/queued）执行记录的明细，按 (t.submitted_at, seq)
// 升序 = 调度 FIFO 顺序（C-4.6，openapi WaitingSummary.runners 的排序口径）。
//
// WHERE 口径与 WaitingBreakdown 逐字一致（pending+queued 且 Task 非终态），
// 因此 by_reason 分桶与 runners 明细可对账。等待中的执行记录还没有 Job
// （Job 在 Claim 时才创建），天然没有 Task→Job→PodGroup 链路——明细只来自
// task_runners 本身，不造数（C-4.8.4 #4）。
func (r *RunnerRunRepo) ListWaiting(ctx context.Context, tx pgx.Tx, limit int) ([]WaitingRunner, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT rr.task_id, rr.attempt_no, rr.seq, rr.name, rr.runner_name, rr.status,
		       COALESCE(rr.waiting_reason, 'unknown'), rr.endpoint_key, rr.updated_at
		  FROM task_runners rr JOIN tasks t ON t.id = rr.task_id
		 WHERE rr.status IN ('pending','queued')
		   AND NOT (t.status = ANY($1))
		 ORDER BY t.submitted_at ASC, rr.seq ASC
		 LIMIT $2`, domain.TerminalTaskStatusStrings(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list waiting task_runners: %w", err)
	}
	defer rows.Close()
	var out []WaitingRunner
	for rows.Next() {
		var w WaitingRunner
		if err := rows.Scan(&w.TaskID, &w.AttemptNo, &w.Seq, &w.Name, &w.RunnerName,
			&w.Status, &w.WaitingReason, &w.EndpointKey, &w.Since); err != nil {
			return nil, fmt.Errorf("store: scan waiting task_runner: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ExpiredRunnerRun 是超时 watchdog 的输入（C-6.3）。
type ExpiredRunnerRun struct {
	Key       domain.RunnerKey
	JobName   string
	StartedAt *time.Time
}

// ExpiredRunnerRuns 返回 started_at + timeout_seconds 已到期的活跃执行记录。
func (r *RunnerRunRepo) ExpiredRunnerRuns(ctx context.Context, tx pgx.Tx, now time.Time) ([]ExpiredRunnerRun, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT task_id, attempt_no, seq, COALESCE(executor_ref->>'job', job_id, ''), started_at
		  FROM task_runners
		 WHERE status IN ('preparing','running')
		   AND started_at IS NOT NULL
		   AND started_at + make_interval(secs => timeout_seconds) < $1
		 LIMIT 100`, now)
	if err != nil {
		return nil, fmt.Errorf("store: expired task_runners: %w", err)
	}
	defer rows.Close()
	var out []ExpiredRunnerRun
	for rows.Next() {
		var e ExpiredRunnerRun
		var k domain.RunnerKey
		if err := rows.Scan(&k.TaskID, &k.AttemptNo, &k.Seq, &e.JobName, &e.StartedAt); err != nil {
			return nil, fmt.Errorf("store: scan expired task_runner: %w", err)
		}
		e.Key = k
		out = append(out, e)
	}
	return out, rows.Err()
}

func collectRunnerRuns(rows pgx.Rows) ([]RunnerRun, error) {
	defer rows.Close()
	var out []RunnerRun
	for rows.Next() {
		rr, err := scanRunnerRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task_runner: %w", err)
		}
		out = append(out, *rr)
	}
	return out, rows.Err()
}

func intPtrToAny(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func textPtrToAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
