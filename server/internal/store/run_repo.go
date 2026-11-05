package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// RunRepo 是 runs 表的读写。所有 SQL 只在本包内出现（05 §3.2）。
type RunRepo struct{ s *Store }

const runCols = `id, name, kind, status, manifest, resolved, endpoint, deploy, cleanup_policy,
	submitted_by, submitted_at, started_at, finished_at, error, retry_of, last_event_seq, revision`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	if err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Status, &r.Manifest, &r.Resolved,
		&r.Endpoint, &r.Deploy, &r.CleanupPolicy, &r.SubmittedBy, &r.SubmittedAt,
		&r.StartedAt, &r.FinishedAt, &r.Error, &r.RetryOf, &r.LastEventSeq, &r.Revision); err != nil {
		return nil, err
	}
	return &r, nil
}

// Get 按 id 取 Run。
func (r *RunRepo) Get(ctx context.Context, tx pgx.Tx, id string) (*Run, error) {
	row := r.s.db(tx).QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id)
	run, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: run %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return run, nil
}

// RunFilter 是 Run 列表的过滤条件（07 §5.1）。
type RunFilter struct {
	Statuses  []string
	Kind      string
	Submitter string
	Tester    string // 命中任一 Task 的 tester_name
	Cursor    string
	Limit     int
}

// List 返回按 submitted_at DESC, id DESC 的游标分页结果。
func (r *RunRepo) List(ctx context.Context, tx pgx.Tx, f RunFilter) ([]Run, string, error) {
	limit := normalizeLimit(f.Limit)
	args := []any{}
	var where []string
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("r.status = ANY($%d)", len(args)))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		where = append(where, fmt.Sprintf("r.kind = $%d", len(args)))
	}
	if f.Submitter != "" {
		args = append(args, f.Submitter)
		where = append(where, fmt.Sprintf("r.submitted_by = $%d", len(args)))
	}
	if f.Tester != "" {
		args = append(args, f.Tester)
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM tasks t WHERE t.run_id = r.id AND t.tester_name = $%d)`, len(args)))
	}
	ts, id, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	if !ts.IsZero() {
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(r.submitted_at, r.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	q := `SELECT ` + runCols + ` FROM runs r`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY r.submitted_at DESC, r.id DESC LIMIT $%d", len(args))

	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: iterate runs: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(last.SubmittedAt, last.ID)
	}
	return out, next, nil
}

// Insert 插入一行 Run；调用方负责事务与后续事件。
func (r *RunRepo) Insert(ctx context.Context, tx pgx.Tx, run *Run) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO runs (id, name, kind, status, manifest, resolved, endpoint, deploy,
		                  cleanup_policy, submitted_by, submitted_at, started_at, finished_at,
		                  error, retry_of, last_event_seq, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,COALESCE($16,0),COALESCE($17,1))`,
		run.ID, run.Name, run.Kind, run.Status, run.Manifest, run.Resolved,
		nullableBytes(run.Endpoint), nullableBytes(run.Deploy), run.CleanupPolicy,
		run.SubmittedBy, run.SubmittedAt, run.StartedAt, run.FinishedAt,
		nullableBytes(run.Error), run.RetryOf, zeroToNil(run.LastEventSeq), zeroToNil(run.Revision))
	if err != nil {
		return fmt.Errorf("store: insert run %s: %w", run.ID, err)
	}
	return nil
}

// Lock 对 Run 行加 FOR UPDATE 锁（统一锁顺序的第一步，02 §3.8）。
func (r *RunRepo) Lock(ctx context.Context, tx pgx.Tx, id string) (domain.RunStatus, error) {
	var st domain.RunStatus
	err := r.s.db(tx).QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: lock run %s: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: lock run %s: %w", id, err)
	}
	return st, nil
}

// SetEndpoint 写入 runs.endpoint（Deploy ready / service 模式）。
func (r *RunRepo) SetEndpoint(ctx context.Context, tx pgx.Tx, id string, endpoint []byte) error {
	_, err := r.s.db(tx).Exec(ctx, `UPDATE runs SET endpoint = $2 WHERE id = $1`, id, endpoint)
	if err != nil {
		return fmt.Errorf("store: set endpoint %s: %w", id, err)
	}
	return nil
}

// SetDeploy 把 runs.deploy 快照**深合并**进已有 JSON（03-data-view §4.4）。
//
// 必须是合并而不是整段覆盖：release_name / provider / values_digest 等在 BeginDeploy
// 时写一次，之后每次状态推进只带"变化的键"；覆盖会把冻结信息冲掉，重启后就无法靠
// release_name 找回 Genet 侧的 release（execution §9.2）。
func (r *RunRepo) SetDeploy(ctx context.Context, tx pgx.Tx, id string, deploy []byte) error {
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE runs
		   SET deploy = COALESCE(deploy, '{}'::jsonb) || $2::jsonb
		 WHERE id = $1`, id, deploy)
	if err != nil {
		return fmt.Errorf("store: set deploy %s: %w", id, err)
	}
	return nil
}

// RunTaskCounts 是 Run 的 Task 状态聚合（前端列表 "3 / 4" 与看板用）。
//
// 口径（openapi Run.task_counts）：pending 含 queued；running 含 preparing；
// failed 含 canceled；skipped 单独一档。
type RunTaskCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// TaskCounts 批量取多个 Run 的 Task 计数（一条 GROUP BY，避免 N+1）。
func (r *RunRepo) TaskCounts(ctx context.Context, tx pgx.Tx, runIDs []string) (map[string]RunTaskCounts, error) {
	out := make(map[string]RunTaskCounts, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT run_id,
		       count(*),
		       count(*) FILTER (WHERE status IN ('pending','queued')),
		       count(*) FILTER (WHERE status IN ('preparing','running')),
		       count(*) FILTER (WHERE status = 'succeeded'),
		       count(*) FILTER (WHERE status IN ('failed','canceled')),
		       count(*) FILTER (WHERE status = 'skipped')
		  FROM tasks WHERE run_id = ANY($1)
		 GROUP BY run_id`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("store: run task counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var c RunTaskCounts
		if err := rows.Scan(&runID, &c.Total, &c.Pending, &c.Running,
			&c.Succeeded, &c.Failed, &c.Skipped); err != nil {
			return nil, fmt.Errorf("store: scan run task counts: %w", err)
		}
		out[runID] = c
	}
	return out, rows.Err()
}

// Summaries 返回 Run 的 Task 摘要（详情页用一条 JOIN 取，避免 N+1，03 §5）。
type TaskSummaryRow struct {
	ID            string  `json:"id"`
	Seq           int     `json:"seq"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	WaitingReason *string `json:"waiting_reason,omitempty"`
	Attempts      int     `json:"attempts"`
	Error         []byte  `json:"error,omitempty"`
}

// TaskSummaries 取 Run 的全部 Task 摘要，按 seq 升序。
func (r *RunRepo) TaskSummaries(ctx context.Context, tx pgx.Tx, runID string) ([]TaskSummaryRow, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT id, seq, name, status, waiting_reason, current_attempt, error
		  FROM tasks WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: task summaries %s: %w", runID, err)
	}
	defer rows.Close()
	var out []TaskSummaryRow
	for rows.Next() {
		var t TaskSummaryRow
		if err := rows.Scan(&t.ID, &t.Seq, &t.Name, &t.Status, &t.WaitingReason, &t.Attempts, &t.Error); err != nil {
			return nil, fmt.Errorf("store: scan task summary: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ActiveEndpoints 返回当前非终态 Run 的 endpoint 投影（GET /api/v1/endpoints 用）。
type EndpointRow struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	ModelName string `json:"model_name,omitempty"`
	ModelPath string `json:"model_path,omitempty"`
	Source    string `json:"source,omitempty"`
	RunID     string `json:"run_id"`
}

// ListActiveEndpoints 取非终态 Run 上登记的 Endpoint。
func (r *RunRepo) ListActiveEndpoints(ctx context.Context, tx pgx.Tx) ([]EndpointRow, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT id, endpoint->>'key', endpoint->>'url',
		       COALESCE(endpoint->>'model_name',''), COALESCE(endpoint->>'model_path',''),
		       COALESCE(endpoint->>'source','')
		  FROM runs
		 WHERE endpoint IS NOT NULL
		   AND status NOT IN ('succeeded','completed_with_errors','service_unavailable','failed','canceled')
		 ORDER BY submitted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}
	defer rows.Close()
	var out []EndpointRow
	for rows.Next() {
		var e EndpointRow
		if err := rows.Scan(&e.RunID, &e.Key, &e.URL, &e.ModelName, &e.ModelPath, &e.Source); err != nil {
			return nil, fmt.Errorf("store: scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func normalizeLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func zeroToNil(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
