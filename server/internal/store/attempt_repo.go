package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// AttemptRepo 是 attempts 表的读写。Attempt 是物理执行事实，状态由执行层的
// ResultSink 经 Kernel CAS 写；本 repo 只做读取与"物理字段回写"。
type AttemptRepo struct{ s *Store }

const attemptCols = `id, task_id, idx, status, job_id, job_namespace, phase, image_digest,
	exit_code, started_at, finished_at, log_ref, artifact_ref, result_ref, error, executor_ref`

func scanAttempt(row pgx.Row) (*Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.TaskID, &a.Idx, &a.Status, &a.JobID, &a.JobNamespace,
		&a.Phase, &a.ImageDigest, &a.ExitCode, &a.StartedAt, &a.FinishedAt,
		&a.LogRef, &a.ArtifactRef, &a.ResultRef, &a.Error, &a.ExecutorRef)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Get 按 id 取 Attempt。
func (r *AttemptRepo) Get(ctx context.Context, tx pgx.Tx, id string) (*Attempt, error) {
	a, err := scanAttempt(r.s.db(tx).QueryRow(ctx, `SELECT `+attemptCols+` FROM attempts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get attempt %s: %w", id, err)
	}
	return a, nil
}

// Owner 返回 Attempt 所属的 (task_id, run_id)，门面开事务后先锁 Run 用。
func (r *AttemptRepo) Owner(ctx context.Context, tx pgx.Tx, attemptID string) (taskID, runID string, err error) {
	e := r.s.db(tx).QueryRow(ctx, `
		SELECT a.task_id, t.run_id FROM attempts a JOIN tasks t ON t.id = a.task_id WHERE a.id = $1`, attemptID)
	if err = e.Scan(&taskID, &runID); errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("store: attempt %s: %w", attemptID, domain.ErrNotFound)
	}
	if err != nil {
		return "", "", fmt.Errorf("store: owner of attempt %s: %w", attemptID, err)
	}
	return taskID, runID, nil
}

// GetByJobID 按 job_id（内部幂等键）取 Attempt。
func (r *AttemptRepo) GetByJobID(ctx context.Context, tx pgx.Tx, jobID string) (*Attempt, error) {
	a, err := scanAttempt(r.s.db(tx).QueryRow(ctx, `SELECT `+attemptCols+` FROM attempts WHERE job_id = $1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt by job %s: %w", jobID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get attempt by job %s: %w", jobID, err)
	}
	return a, nil
}

// ListByTask 返回某 Task 的全部 Attempt（按 idx 升序，历史不被覆盖）。
func (r *AttemptRepo) ListByTask(ctx context.Context, tx pgx.Tx, taskID string) ([]Attempt, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT `+attemptCols+` FROM attempts WHERE task_id = $1 ORDER BY idx`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list attempts %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan attempt: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ListActive 返回全部进行中的 Attempt（对账用）。
func (r *AttemptRepo) ListActive(ctx context.Context, tx pgx.Tx, limit int) ([]Attempt, error) {
	limit = normalizeLimit(limit)
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+attemptCols+`
		  FROM attempts WHERE status IN ('preparing','running') ORDER BY started_at NULLS FIRST LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list active attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan attempt: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// CountActive 返回全局进行中的 Attempt 数（全局并发 permit 的判定输入，C-4.4）。
func (r *AttemptRepo) CountActive(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM attempts WHERE status IN ('preparing','running')`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active attempts: %w", err)
	}
	return n, nil
}

// CreateAttemptParams 是启动一次 Attempt 的输入（T3 的副作用）。
type CreateAttemptParams struct {
	TaskID       string
	JobID        string // 空则生成新 job_id
	JobNamespace string
	ExecutorRef  []byte
	Idx          int // 0 表示用 current_attempt+1
}

// Create 插入一行 Attempt 并把 tasks.current_attempt 推进到同一 idx。
// 调用方必须在事务里、且已持有该 Task 的 Run 锁。
func (r *AttemptRepo) Create(ctx context.Context, tx pgx.Tx, p CreateAttemptParams) (*Attempt, error) {
	idx := p.Idx
	if idx <= 0 {
		if err := r.s.db(tx).QueryRow(ctx,
			`UPDATE tasks SET current_attempt = current_attempt + 1, updated_at = now()
			  WHERE id = $1 RETURNING current_attempt`, p.TaskID).Scan(&idx); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("store: create attempt: task %s: %w", p.TaskID, domain.ErrNotFound)
			}
			return nil, fmt.Errorf("store: create attempt: bump current_attempt: %w", err)
		}
	} else {
		if _, err := r.s.db(tx).Exec(ctx,
			`UPDATE tasks SET current_attempt = GREATEST(current_attempt, $2), updated_at = now()
			  WHERE id = $1`, p.TaskID, idx); err != nil {
			return nil, fmt.Errorf("store: create attempt: set current_attempt: %w", err)
		}
	}

	jobID := p.JobID
	if jobID == "" {
		jobID = NewJobID()
	}
	a := &Attempt{
		ID:           AttemptID(p.TaskID, idx),
		TaskID:       p.TaskID,
		Idx:          idx,
		Status:       string(domain.AttemptPreparing),
		JobID:        jobID,
		JobNamespace: nullableText(p.JobNamespace),
		ExecutorRef:  p.ExecutorRef,
	}
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO attempts (id, task_id, idx, status, job_id, job_namespace, executor_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.TaskID, a.Idx, a.Status, a.JobID, a.JobNamespace, nullableBytes(a.ExecutorRef))
	if err != nil {
		return nil, fmt.Errorf("store: insert attempt %s: %w", a.ID, err)
	}
	return a, nil
}

// PhysicalFacts 是执行层回写的物理事实（不参与状态判定，03 §3.1）。
type PhysicalFacts struct {
	Phase        *string
	ExitCode     *int32
	ImageDigest  *string
	JobNamespace *string
	LogRef       *string
	ArtifactRef  *string
	ResultRef    *string
	ExecutorRef  []byte
}

// SetPhysicalFacts 只补物理字段，不改 status（状态只能由 Kernel.Apply 改）。
func (r *AttemptRepo) SetPhysicalFacts(ctx context.Context, tx pgx.Tx, id string, f PhysicalFacts) error {
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE attempts
		   SET phase         = COALESCE($2, phase),
		       exit_code     = COALESCE($3, exit_code),
		       image_digest  = COALESCE($4, image_digest),
		       job_namespace = COALESCE($5, job_namespace),
		       log_ref       = COALESCE($6, log_ref),
		       artifact_ref  = COALESCE($7, artifact_ref),
		       result_ref    = COALESCE($8, result_ref),
		       executor_ref  = COALESCE($9, executor_ref)
		 WHERE id = $1`,
		id, f.Phase, f.ExitCode, f.ImageDigest, f.JobNamespace, f.LogRef, f.ArtifactRef, f.ResultRef,
		nullableBytes(f.ExecutorRef))
	if err != nil {
		return fmt.Errorf("store: set physical facts %s: %w", id, err)
	}
	return nil
}
