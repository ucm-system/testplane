package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TaskRepo 是 tasks 表的读写。
type TaskRepo struct{ s *Store }

const taskCols = `id, run_id, seq, name, tester_name, tester_version, image_ref, profile,
	args_raw, args_argv, injected_argv, env, secret_env, on_failure, timeout_seconds,
	retry_policy, status, status_reason, waiting_reason, endpoint_key, endpoint_exclusive,
	current_attempt, available_at, cancel_requested, result_ref, reused_from_task_id, error,
	created_at, updated_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.RunID, &t.Seq, &t.Name, &t.TesterName, &t.TesterVersion,
		&t.ImageRef, &t.Profile, &t.ArgsRaw, &t.ArgsArgv, &t.InjectedArgv, &t.Env, &t.SecretEnv,
		&t.OnFailure, &t.TimeoutSeconds, &t.RetryPolicy, &t.Status, &t.StatusReason,
		&t.WaitingReason, &t.EndpointKey, &t.EndpointExclusive, &t.CurrentAttempt,
		&t.AvailableAt, &t.CancelRequested, &t.ResultRef, &t.ReusedFromTaskID, &t.Error,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Get 按 id 取 Task。
func (r *TaskRepo) Get(ctx context.Context, tx pgx.Tx, id string) (*Task, error) {
	t, err := scanTask(r.s.db(tx).QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: task %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task %s: %w", id, err)
	}
	return t, nil
}

// RunIDOf 返回 Task 所属 Run 的 id（门面开事务后先锁 Run 用，02 §3.8）。
func (r *TaskRepo) RunIDOf(ctx context.Context, tx pgx.Tx, taskID string) (string, error) {
	var runID string
	err := r.s.db(tx).QueryRow(ctx, `SELECT run_id FROM tasks WHERE id = $1`, taskID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: task %s: %w", taskID, domain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: run id of task %s: %w", taskID, err)
	}
	return runID, nil
}

// Lock 对 Task 行加 FOR UPDATE（调用方必须已锁 Run）。
func (r *TaskRepo) Lock(ctx context.Context, tx pgx.Tx, id string) (domain.TaskStatus, error) {
	var st domain.TaskStatus
	err := r.s.db(tx).QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: lock task %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: lock task %s: %w", id, err)
	}
	return st, nil
}

// Insert 插入一行 Task（提交事务内用）。
func (r *TaskRepo) Insert(ctx context.Context, tx pgx.Tx, t *Task) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO tasks (id, run_id, seq, name, tester_name, tester_version, image_ref, profile,
		                   args_raw, args_argv, injected_argv, env, secret_env, on_failure,
		                   timeout_seconds, retry_policy, status, status_reason, waiting_reason,
		                   endpoint_key, endpoint_exclusive, current_attempt, available_at,
		                   cancel_requested, result_ref, reused_from_task_id, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		t.ID, t.RunID, t.Seq, t.Name, t.TesterName, t.TesterVersion, t.ImageRef, t.Profile,
		t.ArgsRaw, jsonOrEmptyArray(t.ArgsArgv), jsonOrEmptyArray(t.InjectedArgv),
		jsonOrEmptyObject(t.Env), jsonOrEmptyArray(t.SecretEnv), t.OnFailure,
		t.TimeoutSeconds, jsonOrEmptyObject(t.RetryPolicy), t.Status, t.StatusReason,
		t.WaitingReason, t.EndpointKey, t.EndpointExclusive, t.CurrentAttempt, t.AvailableAt,
		t.CancelRequested, t.ResultRef, t.ReusedFromTaskID, nullableBytes(t.Error))
	if err != nil {
		return fmt.Errorf("store: insert task %s: %w", t.ID, err)
	}
	return nil
}

// ListByRun 按 (run_id, seq) 取 Task 列表，seq 游标分页。
func (r *TaskRepo) ListByRun(ctx context.Context, tx pgx.Tx, runID, cursor string, limit int) ([]Task, string, error) {
	limit = normalizeLimit(limit)
	// 空 cursor = 首屏：seq 从 -1 起（`seq > -1` 含 seq=0 的 Pre-Check）。
	// 若直接用 DecodeSeqCursor("")=0，`seq > 0` 会漏掉 precheck（QA 报的回归）。
	since := int64(-1)
	if cursor != "" {
		v, err := DecodeSeqCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		since = v
	}
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+taskCols+`
		  FROM tasks WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, runID, since, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: list tasks by run %s: %w", runID, err)
	}
	out, err := collectTasks(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = EncodeSeqCursor(int64(out[len(out)-1].Seq))
	}
	return out, next, nil
}

// TaskFilter 是跨 Run 的 Task 列表过滤（07 §5.2）。
type TaskFilter struct {
	Statuses []string
	Tester   string
	RunID    string
	Cursor   string
	Limit    int
}

// List 返回跨 Run 的 Task 列表，按 created_at DESC, id DESC 游标分页。
func (r *TaskRepo) List(ctx context.Context, tx pgx.Tx, f TaskFilter) ([]Task, string, error) {
	limit := normalizeLimit(f.Limit)
	args := []any{}
	var where []string
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	if f.Tester != "" {
		args = append(args, f.Tester)
		where = append(where, fmt.Sprintf("tester_name = $%d", len(args)))
	}
	if f.RunID != "" {
		args = append(args, f.RunID)
		where = append(where, fmt.Sprintf("run_id = $%d", len(args)))
	}
	ts, id, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	if !ts.IsZero() {
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	q := `SELECT ` + taskCols + ` FROM tasks`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list tasks: %w", err)
	}
	out, err := collectTasks(rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// Candidate 是调度候选（C-4.2）：**不加行锁**，并发由后续 Run 锁 + CAS 兜住。
type Candidate struct {
	ID                string
	RunID             string
	Seq               int
	EndpointKey       *string
	EndpointExclusive bool
}

// Candidates 取一批可调度的 Task 候选。
func (r *TaskRepo) Candidates(ctx context.Context, tx pgx.Tx, limit int) ([]Candidate, error) {
	limit = normalizeLimit(limit)
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT t.id, t.run_id, t.seq, t.endpoint_key, t.endpoint_exclusive
		  FROM tasks t JOIN runs r ON r.id = t.run_id
		 WHERE t.status = 'queued'
		   AND r.status = 'testing'
		   AND (t.available_at IS NULL OR t.available_at <= now())
		 ORDER BY r.submitted_at, t.seq
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.RunID, &c.Seq, &c.EndpointKey, &c.EndpointExclusive); err != nil {
			return nil, fmt.Errorf("store: scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetWaitingReason 写调度等待原因（仅展示，C-4.7）。
func (r *TaskRepo) SetWaitingReason(ctx context.Context, tx pgx.Tx, taskID, reason string) error {
	var v *string
	if reason != "" {
		if !domain.ValidWaitingReason(reason) {
			return fmt.Errorf("store: waiting_reason %q 非法: %w", reason, domain.ErrPrecondition)
		}
		v = &reason
	}
	_, err := r.s.db(tx).Exec(ctx, `UPDATE tasks SET waiting_reason = $2, updated_at = now() WHERE id = $1`, taskID, v)
	if err != nil {
		return fmt.Errorf("store: set waiting reason %s: %w", taskID, err)
	}
	return nil
}

// BackfillPublicParams 在 Deploy ready 时把 endpoint_key 与每个 Task 的 injected_argv
// 一次性回填（C-5.3：与状态转移同事务，避免"已 testing 但 Task 还没 key"的窗口）。
func (r *TaskRepo) BackfillPublicParams(ctx context.Context, tx pgx.Tx, runID, endpointKey string,
	injected map[string][]string) error {
	if endpointKey != "" {
		if _, err := r.s.db(tx).Exec(ctx,
			`UPDATE tasks SET endpoint_key = $2 WHERE run_id = $1 AND endpoint_key IS NULL`,
			runID, endpointKey); err != nil {
			return fmt.Errorf("store: backfill endpoint_key %s: %w", runID, err)
		}
	}
	for taskID, argv := range injected {
		if _, err := r.s.db(tx).Exec(ctx,
			`UPDATE tasks SET injected_argv = $2, updated_at = now() WHERE id = $1`,
			taskID, argv); err != nil {
			return fmt.Errorf("store: backfill injected_argv %s: %w", taskID, err)
		}
	}
	return nil
}

// SetResultRef 记录 Task 的结果引用（相对 Run 目录）。
func (r *TaskRepo) SetResultRef(ctx context.Context, tx pgx.Tx, taskID, ref string) error {
	_, err := r.s.db(tx).Exec(ctx, `UPDATE tasks SET result_ref = $2, updated_at = now() WHERE id = $1`, taskID, ref)
	if err != nil {
		return fmt.Errorf("store: set result ref %s: %w", taskID, err)
	}
	return nil
}

// CountNonTerminal 返回 Run 里还没到终态的 Task 数（Run 推进到 cleanup 的判定）。
func (r *TaskRepo) CountNonTerminal(ctx context.Context, tx pgx.Tx, runID string) (int, error) {
	var n int
	err := r.s.db(tx).QueryRow(ctx, `
		SELECT count(*) FROM tasks WHERE run_id = $1
		   AND NOT (status = ANY($2))`, runID, domain.TerminalTaskStatusStrings()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count non-terminal %s: %w", runID, err)
	}
	return n, nil
}

// QueueTasks 把 Run 里等待中的 Task 批量置 queued（Pre-Check 通过 / precheck off）。
// 只对 endpoint_key 非空的 pending Task 生效（C-4.5 调度守卫）。
func (r *TaskRepo) QueueTasks(ctx context.Context, tx pgx.Tx, runID string, onlySeqZero bool) ([]string, error) {
	q := `UPDATE tasks SET status = 'queued', waiting_reason = NULL, updated_at = now()
	       WHERE run_id = $1 AND status = 'pending' AND endpoint_key IS NOT NULL`
	if onlySeqZero {
		q += ` AND seq = 0`
	}
	q += ` RETURNING id`
	rows, err := r.s.db(tx).Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("store: queue tasks %s: %w", runID, err)
	}
	return collectIDs(rows)
}

// WaitingBreakdown 返回等待中的 Task 按 waiting_reason 分组计数（/queue-status 用）。
// 只返回 6 个合法取值；没有写 waiting_reason 的 Task 归入 "unknown" 桶，
// 由调用方决定如何呈现（openapi 的 by_reason 只允许 6 个键）。
func (r *TaskRepo) WaitingBreakdown(ctx context.Context, tx pgx.Tx) (map[string]int, error) {
	out := map[string]int{
		domain.WaitPrecheckGate: 0, domain.WaitSlotWaiting: 0, domain.WaitGlobalLimit: 0,
		domain.WaitQueueFull: 0, domain.WaitInsufficientResources: 0, domain.WaitGangPending: 0,
	}
	// 只统计**非终态 Run** 下的 Task：历史脏数据（Run 已终态但 Task 还挂在 pending/queued）
	// 不得灌进 KPI/等待列表（D-60）。
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT COALESCE(t.waiting_reason, 'unknown'), count(*)
		  FROM tasks t JOIN runs r ON r.id = t.run_id
		 WHERE t.status IN ('pending','queued')
		   AND NOT (r.status = ANY($1))
		 GROUP BY 1`, domain.TerminalRunStatusStrings())
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

// WaitingTaskRow 是 /queue-status 的排队明细（C-4.7）。
type WaitingTaskRow struct {
	TaskID        string
	RunID         string
	Name          string
	Seq           int
	Status        string
	WaitingReason *string
	EndpointKey   *string
	Since         *time.Time
}

// WaitingTasks 返回排队明细，按 (run.submitted_at, seq) 升序即调度 FIFO 顺序（C-4.6）。
func (r *TaskRepo) WaitingTasks(ctx context.Context, tx pgx.Tx, limit int) ([]WaitingTaskRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT t.id, t.run_id, t.name, t.seq, t.status, t.waiting_reason, t.endpoint_key,
		       COALESCE(t.available_at, t.updated_at)
		  FROM tasks t JOIN runs r ON r.id = t.run_id
		 WHERE t.status IN ('pending','queued')
		   AND NOT (r.status = ANY($2))
		 ORDER BY r.submitted_at, t.seq
		 LIMIT $1`, limit, domain.TerminalRunStatusStrings())
	if err != nil {
		return nil, fmt.Errorf("store: waiting tasks: %w", err)
	}
	defer rows.Close()
	var out []WaitingTaskRow
	for rows.Next() {
		var w WaitingTaskRow
		if err := rows.Scan(&w.TaskID, &w.RunID, &w.Name, &w.Seq, &w.Status,
			&w.WaitingReason, &w.EndpointKey, &w.Since); err != nil {
			return nil, fmt.Errorf("store: scan waiting task: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func collectTasks(rows pgx.Rows) ([]Task, error) {
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func jsonOrEmptyArray(b []byte) []byte {
	if len(b) == 0 {
		return []byte("[]")
	}
	return b
}

func jsonOrEmptyObject(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
