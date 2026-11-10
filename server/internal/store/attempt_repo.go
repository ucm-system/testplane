package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// AttemptRepo 是 task_attempts 表（Task 级 attempt 计数与落档归组行，D-68）的读写。
// attempt 行只做**归组与计数**：状态由 Task 推进派生落库；物理执行列在 task_runners。
type AttemptRepo struct{ s *Store }

const attemptCols = `task_id, attempt_no, status, trigger, input_ref, exports_ref,
	deploy_state_ref, error, started_at, finished_at`

func scanAttempt(row pgx.Row) (*Attempt, error) {
	var a Attempt
	err := row.Scan(&a.TaskID, &a.AttemptNo, &a.Status, &a.Trigger, &a.InputRef,
		&a.ExportsRef, &a.DeployStateRef, &a.Error, &a.StartedAt, &a.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Get 取指定 attempt 行。
func (r *AttemptRepo) Get(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int) (*Attempt, error) {
	a, err := scanAttempt(r.s.db(tx).QueryRow(ctx,
		`SELECT `+attemptCols+` FROM task_attempts WHERE task_id = $1 AND attempt_no = $2`,
		taskID, attemptNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt %s#%d: %w", taskID, attemptNo, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get attempt %s#%d: %w", taskID, attemptNo, err)
	}
	return a, nil
}

// GetLatest 取该 Task 的最新 attempt（不存在时返回 ErrNotFound）。
func (r *AttemptRepo) GetLatest(ctx context.Context, tx pgx.Tx, taskID string) (*Attempt, error) {
	a, err := scanAttempt(r.s.db(tx).QueryRow(ctx,
		`SELECT `+attemptCols+` FROM task_attempts WHERE task_id = $1 ORDER BY attempt_no DESC LIMIT 1`,
		taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt latest %s: %w", taskID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get latest attempt %s: %w", taskID, err)
	}
	return a, nil
}

// GetActive 取该 Task 的活跃 attempt（至多一个；部分唯一索引保证）。
func (r *AttemptRepo) GetActive(ctx context.Context, tx pgx.Tx, taskID string) (*Attempt, error) {
	a, err := scanAttempt(r.s.db(tx).QueryRow(ctx,
		`SELECT `+attemptCols+` FROM task_attempts WHERE task_id = $1 AND status = 'running'`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: attempt active %s: %w", taskID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get active attempt %s: %w", taskID, err)
	}
	return a, nil
}

// ListByTask 返回该 Task 的全部 attempt（按 attempt_no 升序，历史不被覆盖）。
func (r *AttemptRepo) ListByTask(ctx context.Context, tx pgx.Tx, taskID string) ([]Attempt, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT `+attemptCols+` FROM task_attempts WHERE task_id = $1 ORDER BY attempt_no`, taskID)
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

// CreateAttemptParams 是启动一次 attempt 的输入（提交 / POST /tasks/{id}/retry）。
type CreateAttemptParams struct {
	TaskID    string
	Trigger   string // initial | retry；空按 initial
	AttemptNo int    // 0 表示用 tasks.current_attempt+1
}

// Create 插入一行 attempt（status='running'）并把 tasks.current_attempt 推进到同一序号。
// 调用方必须在事务里、且已持有该 Task 行锁。
func (r *AttemptRepo) Create(ctx context.Context, tx pgx.Tx, p CreateAttemptParams) (*Attempt, error) {
	no := p.AttemptNo
	if no <= 0 {
		if err := r.s.db(tx).QueryRow(ctx,
			`UPDATE tasks SET current_attempt = current_attempt + 1 WHERE id = $1
			  RETURNING current_attempt`, p.TaskID).Scan(&no); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("store: create attempt: task %s: %w", p.TaskID, domain.ErrNotFound)
			}
			return nil, fmt.Errorf("store: create attempt: bump current_attempt: %w", err)
		}
	} else {
		if _, err := r.s.db(tx).Exec(ctx,
			`UPDATE tasks SET current_attempt = GREATEST(current_attempt, $2) WHERE id = $1`,
			p.TaskID, no); err != nil {
			return nil, fmt.Errorf("store: create attempt: set current_attempt: %w", err)
		}
	}
	trigger := defaultString(p.Trigger, "initial")
	a := &Attempt{
		TaskID:    p.TaskID,
		AttemptNo: no,
		Status:    string(domain.AttemptRunning),
		Trigger:   trigger,
	}
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO task_attempts (task_id, attempt_no, status, trigger)
		VALUES ($1,$2,$3,$4)`,
		a.TaskID, a.AttemptNo, a.Status, trigger)
	if err != nil {
		return nil, fmt.Errorf("store: insert attempt %s#%d: %w", a.TaskID, a.AttemptNo, err)
	}
	return a, nil
}

// SetRefs 回填 attempt 的落档引用（input_ref / exports_ref / deploy_state_ref）。
// 空字符串表示"该落档不存在"，不覆盖已有值（不造死链）。
func (r *AttemptRepo) SetRefs(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int,
	inputRef, exportsRef, deployStateRef string) error {
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE task_attempts
		   SET input_ref        = COALESCE(NULLIF($3, ''), input_ref),
		       exports_ref      = COALESCE(NULLIF($4, ''), exports_ref),
		       deploy_state_ref = COALESCE(NULLIF($5, ''), deploy_state_ref)
		 WHERE task_id = $1 AND attempt_no = $2`,
		taskID, attemptNo, inputRef, exportsRef, deployStateRef)
	if err != nil {
		return fmt.Errorf("store: set attempt refs %s#%d: %w", taskID, attemptNo, err)
	}
	return nil
}

// SetStatus 落 attempt 聚合状态（由 Task 推进派生；**终态不覆盖**，03 §3.1）。
func (r *AttemptRepo) SetStatus(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int,
	status domain.AttemptStatus, errJSON []byte) error {
	if !domain.ValidAttemptStatus(status.Status()) {
		return fmt.Errorf("store: attempt status %q 非法: %w", status, domain.ErrPrecondition)
	}
	finished := status.IsTerminal()
	tag, err := r.s.db(tx).Exec(ctx, `
		UPDATE task_attempts
		   SET status      = $3,
		       error       = COALESCE($4, error),
		       finished_at = CASE WHEN $5 THEN now() ELSE finished_at END
		 WHERE task_id = $1 AND attempt_no = $2
		   AND (status = 'running' OR ($3 = 'running' AND status <> 'running'))`,
		taskID, attemptNo, string(status), nullableBytes(errJSON), finished)
	if err != nil {
		return fmt.Errorf("store: set attempt status %s#%d: %w", taskID, attemptNo, err)
	}
	if tag.RowsAffected() == 0 {
		// 终态不覆盖；'running' 回写只在行不存在时未命中——按无操作处理。
		return nil
	}
	return nil
}

// CountActive 全局进行中的 attempt 数（全局并发 permit 的参考输入，C-4.4 用
// task_runners 计数；本查询供指标与对账）。
func (r *AttemptRepo) CountActive(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := r.s.db(tx).QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE status = 'running'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active attempts: %w", err)
	}
	return n, nil
}
