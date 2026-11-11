package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TaskRepo 是 tasks 表（一次评测任务；v1 的 runs 表，D-66）的读写。
type TaskRepo struct{ s *Store }

const taskCols = `id, name, status, manifest, resolved, endpoint, deploy, env, secret_env,
	submitted_by, submitted_at, started_at, finished_at, error, retry_of,
	current_attempt, cancel_requested, last_event_seq, revision`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	if err := row.Scan(&t.ID, &t.Name, &t.Status, &t.Manifest, &t.Resolved,
		&t.Endpoint, &t.Deploy, &t.Env, &t.SecretEnv, &t.SubmittedBy, &t.SubmittedAt,
		&t.StartedAt, &t.FinishedAt, &t.Error, &t.RetryOf,
		&t.CurrentAttempt, &t.CancelRequested, &t.LastEventSeq, &t.Revision); err != nil {
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

// TaskFilter 是 Task 列表的过滤条件（07 §5.1）。
// WHERE 的唯一来源是本文件的 conds —— 列表/计数共用，不要在调用方再拼一遍。
type TaskFilter struct {
	Statuses []string
	// Runners 多值：命中任一 runner 执行记录的 runner_name 即算（`runner=aisbench-acc`）。
	Runners    []string
	Submitters []string
	Cursor     string
	Limit      int
}

// taskFilterConds 把 TaskFilter 的三个过滤维度编译成 WHERE 片段与参数。
// exclude 指定"本次不计入"的维度（facets 按维度独立计数：其它维度生效、自己不生效），
// 取值 "" | "status" | "submitter" | "runner"。
//
// WHERE 的唯一来源是本函数 —— List / Count / Facets 共用，保证计数口径与列表逐字一致
// （tasks 表没有软删除列；状态口径 = tasks.status，与 v1 的 runs 软删无涉）。
func taskFilterConds(f TaskFilter, exclude string) ([]string, []any) {
	args := []any{}
	var where []string
	if exclude != "status" && len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("t.status = ANY($%d)", len(args)))
	}
	if exclude != "submitter" && len(f.Submitters) > 0 {
		args = append(args, f.Submitters)
		where = append(where, fmt.Sprintf("t.submitted_by = ANY($%d)", len(args)))
	}
	if exclude != "runner" && len(f.Runners) > 0 {
		args = append(args, f.Runners)
		where = append(where, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM task_runners rr WHERE rr.task_id = t.id AND rr.runner_name = ANY($%d))", len(args)))
	}
	return where, args
}

// List 返回按 submitted_at DESC, id DESC 的游标分页结果。
func (r *TaskRepo) List(ctx context.Context, tx pgx.Tx, f TaskFilter) ([]Task, string, error) {
	limit := normalizeLimit(f.Limit)
	where, args := taskFilterConds(f, "")
	ts, id, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	if !ts.IsZero() {
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(t.submitted_at, t.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	q := `SELECT ` + taskCols + ` FROM tasks t`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY t.submitted_at DESC, t.id DESC LIMIT $%d", len(args))

	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: scan task: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = EncodeCursor(last.SubmittedAt, last.ID)
	}
	return out, next, nil
}

// Count 用与 List 完全相同的过滤口径做 SELECT count(*)（offset 分页的 total，D-61）。
// f.Cursor 不参与计数：total 是"满足过滤条件的总条数"，与游标翻页无关。
// 软删除/状态口径与 taskFilterConds（即 List）完全一致。
func (r *TaskRepo) Count(ctx context.Context, tx pgx.Tx, f TaskFilter) (int, error) {
	where, args := taskFilterConds(f, "")
	q := `SELECT count(*) FROM tasks t`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := r.s.db(tx).QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count tasks: %w", err)
	}
	return n, nil
}

// FacetBucket 是一个维度取值的计数（openapi FacetBucket）。
type FacetBucket struct {
	Value string
	Count int
}

// TaskFacets 是 GET /tasks/facets 的三维精确计数（D-61）。
type TaskFacets struct {
	Status    []FacetBucket
	Submitter []FacetBucket
	Runner    []FacetBucket
}

// facetValueCap 是单维度返回桶数上限（防异常基数把响应撑爆；与旧内存聚合的
// 列表上限口径一致）。
const facetValueCap = 200

// Facets 用 store 侧 GROUP BY 输出三维精确计数（替代旧的"拉一页内存聚合"——
// 那个实现数据量超过列表上限时只是下界）。
//
// 口径：每个维度按"其它维度的过滤条件"聚合（自己不生效），与列表过滤互洽——
// 用任一 facet 值过滤列表，命中的条数就等于该桶的计数。runner 维度按 Task 去重
// （count(DISTINCT t.id)），与 List 的 runner 过滤（EXISTS task_runners 匹配任一
// attempt）逐字一致。排序 count DESC, value ASC（精确计数后按热度排更有用）。
func (r *TaskRepo) Facets(ctx context.Context, tx pgx.Tx, f TaskFilter) (*TaskFacets, error) {
	out := &TaskFacets{
		Status:    []FacetBucket{},
		Submitter: []FacetBucket{},
		Runner:    []FacetBucket{},
	}
	scanBuckets := func(rows pgx.Rows, qerr error, dst *[]FacetBucket) error {
		if qerr != nil {
			return fmt.Errorf("store: task facets: %w", qerr)
		}
		defer rows.Close()
		for rows.Next() {
			var b FacetBucket
			if err := rows.Scan(&b.Value, &b.Count); err != nil {
				return fmt.Errorf("store: scan task facet: %w", err)
			}
			*dst = append(*dst, b)
		}
		return rows.Err()
	}

	// status：status/submitter/runner 过滤中除 status 外全部生效。
	where, args := taskFilterConds(f, "status")
	q := `SELECT t.status, count(*) FROM tasks t`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" GROUP BY t.status ORDER BY 2 DESC, 1 ASC LIMIT %d", facetValueCap)
	rows, qerr := r.s.db(tx).Query(ctx, q, args...)
	if err := scanBuckets(rows, qerr, &out.Status); err != nil {
		return nil, err
	}

	// submitter：除 submitter 外全部生效。
	where, args = taskFilterConds(f, "submitter")
	q = `SELECT t.submitted_by, count(*) FROM tasks t`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" GROUP BY t.submitted_by ORDER BY 2 DESC, 1 ASC LIMIT %d", facetValueCap)
	rows, qerr = r.s.db(tx).Query(ctx, q, args...)
	if err := scanBuckets(rows, qerr, &out.Submitter); err != nil {
		return nil, err
	}

	// runner：除 runner 外全部生效；JOIN task_runners 并按 Task 去重。
	where, args = taskFilterConds(f, "runner")
	q = `SELECT rr.runner_name, count(DISTINCT t.id)
	       FROM tasks t JOIN task_runners rr ON rr.task_id = t.id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" GROUP BY rr.runner_name ORDER BY 2 DESC, 1 ASC LIMIT %d", facetValueCap)
	rows, qerr = r.s.db(tx).Query(ctx, q, args...)
	if err := scanBuckets(rows, qerr, &out.Runner); err != nil {
		return nil, err
	}
	return out, nil
}

// Insert 插入一行 Task；调用方负责事务与后续事件（SubmitTask）。
func (r *TaskRepo) Insert(ctx context.Context, tx pgx.Tx, t *Task) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO tasks (id, name, status, manifest, resolved, endpoint, deploy,
		                   env, secret_env, submitted_by, submitted_at, started_at, finished_at,
		                   error, retry_of, current_attempt, cancel_requested,
		                   last_event_seq, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
		        COALESCE($16,0),COALESCE($17,false),COALESCE($18,0),COALESCE($19,1))`,
		t.ID, t.Name, t.Status, t.Manifest, t.Resolved,
		nullableBytes(t.Endpoint), nullableBytes(t.Deploy),
		jsonOrEmptyObject(t.Env), jsonOrEmptyArray(t.SecretEnv),
		t.SubmittedBy, t.SubmittedAt, t.StartedAt, t.FinishedAt,
		nullableBytes(t.Error), t.RetryOf, intToAny(t.CurrentAttempt), boolToAny(t.CancelRequested),
		zeroToNil(t.LastEventSeq), zeroToNil(t.Revision))
	if err != nil {
		return fmt.Errorf("store: insert task %s: %w", t.ID, err)
	}
	return nil
}

// Lock 对 Task 行加 FOR UPDATE 锁（统一锁顺序的第一步，02 §3.8）。
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

// SetEndpoint 写入 tasks.endpoint（deploy exports 收割回填 / service 静态提交，03 §4.3）。
func (r *TaskRepo) SetEndpoint(ctx context.Context, tx pgx.Tx, id string, endpoint []byte) error {
	_, err := r.s.db(tx).Exec(ctx, `UPDATE tasks SET endpoint = $2 WHERE id = $1`, id, endpoint)
	if err != nil {
		return fmt.Errorf("store: set endpoint %s: %w", id, err)
	}
	return nil
}

// SetDeploy 把 tasks.deploy 快照**深合并**进已有 JSON（03 §4.4）。
//
// 必须是合并而不是整段覆盖：release_name / provider / spec_digest 等冻结键只在提交时
// 写一次，之后每次归档只带"变化的键"；覆盖会把冻结信息冲掉，重启后无法靠 release_name
// 找回 Genet 侧的 release（§9.3 重放的输入）。
func (r *TaskRepo) SetDeploy(ctx context.Context, tx pgx.Tx, id string, deploy []byte) error {
	_, err := r.s.db(tx).Exec(ctx, `
		UPDATE tasks
		   SET deploy = COALESCE(deploy, '{}'::jsonb) || $2::jsonb
		 WHERE id = $1`, id, deploy)
	if err != nil {
		return fmt.Errorf("store: set deploy %s: %w", id, err)
	}
	return nil
}

// RunnerCounts 是 Task 的 runner 状态聚合（前端列表与看板用）。
//
// 口径：pending 含 queued；running 含 preparing；failed 含 canceled；skipped 单独一档。
type RunnerCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// RunnerCountsByTask 批量取多个 Task（按其 current_attempt）的 runner 计数
// （一条 GROUP BY，避免 N+1）。
func (r *TaskRepo) RunnerCountsByTask(ctx context.Context, tx pgx.Tx, taskIDs []string) (map[string]RunnerCounts, error) {
	out := make(map[string]RunnerCounts, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT rr.task_id,
		       count(*),
		       count(*) FILTER (WHERE rr.status IN ('pending','queued')),
		       count(*) FILTER (WHERE rr.status IN ('preparing','running')),
		       count(*) FILTER (WHERE rr.status = 'succeeded'),
		       count(*) FILTER (WHERE rr.status IN ('failed','canceled')),
		       count(*) FILTER (WHERE rr.status = 'skipped')
		  FROM task_runners rr
		 WHERE rr.task_id = ANY($1)
		   AND rr.attempt_no = (SELECT t.current_attempt FROM tasks t WHERE t.id = rr.task_id)
		 GROUP BY rr.task_id`, taskIDs)
	if err != nil {
		return nil, fmt.Errorf("store: task runner counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		var c RunnerCounts
		if err := rows.Scan(&taskID, &c.Total, &c.Pending, &c.Running,
			&c.Succeeded, &c.Failed, &c.Skipped); err != nil {
			return nil, fmt.Errorf("store: scan task runner counts: %w", err)
		}
		out[taskID] = c
	}
	return out, rows.Err()
}

// ActiveEndpoints 返回当前非终态 Task 的 endpoint 投影（GET /api/v1/endpoints 用）。
type EndpointRow struct {
	Key       string `json:"key"`
	URL       string `json:"url"`
	ModelName string `json:"model_name,omitempty"`
	ModelPath string `json:"model_path,omitempty"`
	Source    string `json:"source,omitempty"`
	TaskID    string `json:"task_id"`
}

// ListActiveEndpoints 取非终态 Task 上登记的 Endpoint。
func (r *TaskRepo) ListActiveEndpoints(ctx context.Context, tx pgx.Tx) ([]EndpointRow, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT id, endpoint->>'key', endpoint->>'url',
		       COALESCE(endpoint->>'model_name',''), COALESCE(endpoint->>'model_path',''),
		       COALESCE(endpoint->>'source','')
		  FROM tasks
		 WHERE endpoint IS NOT NULL
		   AND status <> ALL($1)
		 ORDER BY submitted_at DESC`, domain.TerminalTaskStatusStrings())
	if err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}
	defer rows.Close()
	var out []EndpointRow
	for rows.Next() {
		var e EndpointRow
		if err := rows.Scan(&e.TaskID, &e.Key, &e.URL, &e.ModelName, &e.ModelPath, &e.Source); err != nil {
			return nil, fmt.Errorf("store: scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CleanupRequiredTask 是 clean 兜底对账扫描（§9.3 兜底的 MVP 形态）的命中行。
type CleanupRequiredTask struct {
	TaskID        string
	AttemptNo     int
	DeployRelease string // tasks.deploy->>'release_name'（日志可读，可能为空）
}

// ListCleanupRequired 扫描「Task 已终态 + tasks.deploy 非空 + 当前 attempt 没有
// succeeded 的 clean 记录」的行（§9.3：deploy 泄漏候选）。
//
// 判据是**结构性的**（deploy/clean 解耦裁决，平台不按 runner 名/provider 特判）：
// 该 attempt 存在 role=clean 的执行记录 = deploy runner 声明了 clean_runner =
// 存在可拆除对象。没有 clean 行的 attempt（如 existing-deploy，未声明 clean_runner）
// 不进清单——它出现在重放清单里只会是误报。
//
// 排除已写过 cleanup_required 事件（task 级 state_changed + payload.reason）的
// (task, attempt)，扫描可周期重跑而不重复告警。**只读出口**：不自动重放 clean
// （§14.2 P1），重放只走 POST /tasks/{id}/clean。
func (r *TaskRepo) ListCleanupRequired(ctx context.Context, tx pgx.Tx, limit int) ([]CleanupRequiredTask, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT t.id, t.current_attempt, COALESCE(t.deploy->>'release_name', '')
		  FROM tasks t
		 WHERE t.status = ANY($1)
		   AND t.deploy IS NOT NULL
		   AND EXISTS (
		        SELECT 1 FROM task_runners cr
		         WHERE cr.task_id = t.id
		           AND cr.attempt_no = t.current_attempt
		           AND cr.role = 'deploy')
		   AND EXISTS (
		        SELECT 1 FROM task_runners cr
		         WHERE cr.task_id = t.id
		           AND cr.attempt_no = t.current_attempt
		           AND cr.role = 'clean')
		   AND NOT EXISTS (
		        SELECT 1 FROM task_runners rr
		         WHERE rr.task_id = t.id
		           AND rr.attempt_no = t.current_attempt
		           AND rr.role = 'clean'
		           AND rr.status = 'succeeded')
		   AND NOT EXISTS (
		        SELECT 1 FROM state_events se
		         WHERE se.task_id = t.id
		           AND se.type = 'state_changed'
		           AND se.payload->>'reason' = 'cleanup_required'
		           AND se.attempt_no IS NULL
		           AND se.runner_seq IS NULL)
		 ORDER BY t.submitted_at DESC
		 LIMIT $2`, domain.TerminalTaskStatusStrings(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list cleanup-required tasks: %w", err)
	}
	defer rows.Close()
	var out []CleanupRequiredTask
	for rows.Next() {
		var row CleanupRequiredTask
		if err := rows.Scan(&row.TaskID, &row.AttemptNo, &row.DeployRelease); err != nil {
			return nil, fmt.Errorf("store: scan cleanup-required task: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---- 小工具 --------------------------------------------------------------

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

func intToAny(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func boolToAny(b bool) any {
	if !b {
		return nil
	}
	return b
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

// ResolvedPrecheck 解析 resolved 快照里的 precheck 判据（03 §4.2）：
// injected 是"这个 Task 到底有没有平台注入的 pre_check runner"的**唯一判据**
// （precheck: "off" 时为 {"injected": false}，只有这一个键）。
func ResolvedPrecheck(resolved []byte) (injected bool, runner string, version string, err error) {
	if len(resolved) == 0 {
		return false, "", "", nil
	}
	var snap struct {
		Precheck struct {
			Injected bool   `json:"injected"`
			Runner   string `json:"runner"`
			Version  string `json:"version"`
		} `json:"precheck"`
	}
	if err := json.Unmarshal(resolved, &snap); err != nil {
		return false, "", "", fmt.Errorf("store: parse resolved: %w", err)
	}
	return snap.Precheck.Injected, snap.Precheck.Runner, snap.Precheck.Version, nil
}
