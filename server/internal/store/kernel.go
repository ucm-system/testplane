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

// Kernel 是唯一能改状态的地方（02 §3.3）。Store 实现 ports.Kernel：
//
//	Apply(ctx, tx, Transition) (Applied, error)
//
// 事务内动作顺序固定：
//  1. 校验 (entity, from→to) 在转移表内，否则 ErrIllegalState；
//  2. SELECT ... FOR UPDATE 锁实体行（**调用方必须先锁 Task**——统一顺序
//     Task → runner 执行记录，02 §3.8；Kernel 不重复锁 Task）；
//  3. 当前状态 == To → Noop；不在 From 集合 → ErrConflict；
//  4. UPDATE ... WHERE key=? AND status IN (from) 做 CAS 双保险；
//  5. UPDATE tasks SET last_event_seq = last_event_seq+1 RETURNING 分配 seq；
//  6. INSERT state_events；
//  7. 执行本转移的 effects（只允许 DB 写）；
//  8. 返回；提交后由调用方 NOTIFY。
//
// 编译期断言：签名与 ports.Kernel 逐字一致。
var _ interface {
	Apply(ctx context.Context, tx pgx.Tx, tr domain.Transition) (domain.Applied, error)
} = (*Store)(nil)

// Apply 见文件头的事务内动作顺序。
func (s *Store) Apply(ctx context.Context, tx pgx.Tx, tr domain.Transition) (domain.Applied, error) {
	if tx == nil {
		return domain.Applied{}, fmt.Errorf("store: kernel apply %s %s: %w", tr.Entity, tr.ID, domain.ErrTx)
	}
	if tr.ID == "" {
		return domain.Applied{}, fmt.Errorf("store: kernel apply %s: 空 id: %w", tr.Entity, domain.ErrIllegalState)
	}
	at := tr.At
	if at.IsZero() {
		// 业务时间走注入时钟（05 §6.5）；显式传入的 At 优先。
		at = s.now()
	}

	// 1. 转移表校验：From 集合里每个前置状态都必须有到 To 的合法边。
	if err := validateTransitionTable(tr); err != nil {
		return domain.Applied{}, err
	}

	switch tr.Entity {
	case domain.EntityTask:
		return s.applyTask(ctx, tx, tr, at)
	case domain.EntityRunnerRun:
		key, ok := domain.ParseRunnerKey(tr.ID)
		if !ok {
			return domain.Applied{}, fmt.Errorf("store: kernel task_runner: 非法 key %q: %w", tr.ID, domain.ErrIllegalState)
		}
		return s.applyRunnerRun(ctx, tx, tr, key, at)
	}
	return domain.Applied{}, fmt.Errorf("store: kernel: 未知实体 %q: %w", tr.Entity, domain.ErrIllegalState)
}

// validateTransitionTable 做步骤 1。语义是"这组 From 里有没有一条到 To 的合法边"：
//   - From 为空表示"不校验前置状态"，只要求目标状态在表里有任意入边；
//   - From 非空时至少有一条合法边即可（同一组前置状态里允许某几个状态到不了 To，
//     真正的判定在读到当前状态后做：不在集合里 → ErrConflict）。
func validateTransitionTable(tr domain.Transition) error {
	var legal int
	for _, f := range tr.From {
		if _, ok := domain.LookupTransition(tr.Entity, f, tr.To); ok {
			legal++
		}
	}
	if legal > 0 {
		return nil
	}
	if len(tr.From) == 0 {
		for _, from := range allStatuses(tr.Entity) {
			if _, ok := domain.LookupTransition(tr.Entity, from, tr.To); ok {
				return nil
			}
		}
	}
	return fmt.Errorf("store: kernel %s %s: 转移表不允许 %v -> %s: %w",
		tr.Entity, tr.ID, tr.From, tr.To, domain.ErrIllegalState)
}

// requireEdge 在读到当前状态后确认 current→To 在转移表内；不在则说明调用方给的
// From 集合与库内事实不符，按 ErrConflict 处理（先落库者胜）。
func requireEdge(entity domain.EntityType, id string, current, to domain.Status) error {
	if _, ok := domain.LookupTransition(entity, current, to); ok {
		return nil
	}
	return fmt.Errorf("store: kernel %s %s: 当前状态 %s 没有到 %s 的合法边: %w",
		entity, id, current, to, domain.ErrConflict)
}

// allStatuses 枚举某实体的全部状态值，只给 From 为空时的兜底校验用。
func allStatuses(entity domain.EntityType) []domain.Status {
	switch entity {
	case domain.EntityTask:
		return domain.TaskStatuses(domain.TaskSubmitted, domain.TaskQueued, domain.TaskRunning,
			domain.TaskCancelling, domain.TaskSucceeded, domain.TaskFailed,
			domain.TaskServiceUnavailable, domain.TaskCanceled)
	case domain.EntityRunnerRun:
		return domain.RunnerStatuses(domain.RunnerPending, domain.RunnerQueued, domain.RunnerPreparing,
			domain.RunnerRunning, domain.RunnerSucceeded, domain.RunnerFailed,
			domain.RunnerSkipped, domain.RunnerCanceled)
	}
	return nil
}

// ---- Task ----------------------------------------------------------------

func (s *Store) applyTask(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) (domain.Applied, error) {
	var cur domain.TaskStatus
	err := tx.QueryRow(ctx,
		`SELECT status FROM tasks WHERE id = $1 FOR UPDATE`, tr.ID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: %w", tr.ID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: lock: %w", tr.ID, err)
	}

	to := domain.TaskStatus(tr.To)

	// 3. 幂等与前置状态
	if cur == to {
		return domain.Applied{From: cur.Status(), To: to.Status(), Noop: true}, nil
	}
	if len(tr.From) > 0 && !containsStatus(tr.From, cur.Status()) {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: 当前状态 %s 不在允许集合 %v: %w",
			tr.ID, cur, tr.From, domain.ErrConflict)
	}
	if err := requireEdge(tr.Entity, tr.ID, cur.Status(), to.Status()); err != nil {
		return domain.Applied{}, err
	}

	// 4. CAS
	from := casFrom(tr.From, cur.Status())
	setStarted := to == domain.TaskRunning
	setFinished := to.IsTerminal()
	errJSON := payloadJSON(tr.Payload, "error")
	if f, ok := tr.Payload["error"].(domain.Failure); ok {
		errJSON, _ = json.Marshal(f)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET status      = $2,
		       revision    = revision + 1,
		       started_at  = CASE WHEN $3 THEN COALESCE(started_at, $4) ELSE started_at END,
		       finished_at = CASE WHEN $5 THEN $4 ELSE finished_at END,
		       error       = COALESCE($6, error)
		 WHERE id = $1 AND status = ANY($7)`,
		tr.ID, string(to), setStarted, at, setFinished,
		nullableBytes(errJSON), from)
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: cas: %w", tr.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: cas 未命中（并发修改）: %w", tr.ID, domain.ErrConflict)
	}

	// 5+6. seq 与事件
	spec, _ := domain.LookupTransition(tr.Entity, cur.Status(), to.Status())
	seq, err := s.insertTransitionEvent(ctx, tx, tr, spec, cur.Status(), to.Status(), at,
		evIDs{taskID: tr.ID})
	if err != nil {
		return domain.Applied{}, err
	}

	// 7. effects
	if err := s.applyEffects(ctx, tx, tr, cur.Status(), to.Status(), at); err != nil {
		return domain.Applied{}, err
	}

	return domain.Applied{From: cur.Status(), To: to.Status(), Seq: seq}, nil
}

// ---- runner 执行记录 -----------------------------------------------------

func (s *Store) applyRunnerRun(ctx context.Context, tx pgx.Tx, tr domain.Transition,
	k domain.RunnerKey, at time.Time) (domain.Applied, error) {

	var cur domain.RunnerStatus
	var endpointKey *string
	err := tx.QueryRow(ctx, `
		SELECT status, endpoint_key
		  FROM task_runners
		 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3
		 FOR UPDATE`, k.TaskID, k.AttemptNo, k.Seq).
		Scan(&cur, &endpointKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: %w", k.ID(), domain.ErrNotFound)
	}
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: lock: %w", k.ID(), err)
	}

	to := domain.RunnerStatus(tr.To)

	if cur == to {
		return domain.Applied{From: cur.Status(), To: to.Status(), Noop: true}, nil
	}
	if len(tr.From) > 0 && !containsStatus(tr.From, cur.Status()) {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: 当前状态 %s 不在允许集合 %v: %w",
			k.ID(), cur, tr.From, domain.ErrConflict)
	}
	if err := requireEdge(tr.Entity, k.ID(), cur.Status(), to.Status()); err != nil {
		return domain.Applied{}, err
	}

	// 守卫 1：外接模式（Task 无 deploy）下 queued 的 runner 必须有 endpoint_key
	// （C-3.4 / C-4.5，不得用占位键绕过；部署模式无租约，不校验）。
	if to == domain.RunnerQueued && endpointKey != nil && *endpointKey == "" {
		// 空串键 = 声明了但没值：按守卫失败处理。
		var hasDeploy bool
		if err := tx.QueryRow(ctx,
			`SELECT (deploy IS NOT NULL) FROM tasks WHERE id = $1`, k.TaskID).Scan(&hasDeploy); err != nil {
			return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: guard deploy: %w", k.ID(), err)
		}
		if !hasDeploy {
			return domain.Applied{}, fmt.Errorf(
				"store: kernel task_runner %s: 外接模式下 endpoint_key 为空，不能进入 queued: %w",
				k.ID(), domain.ErrPrecondition)
		}
	}
	// 守卫 2：skipped 必须有合法 reason（03 §3.2 CHECK）。
	if to == domain.RunnerSkipped && !domain.ValidSkipReason(tr.Reason) {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: skip reason %q 非法: %w",
			k.ID(), tr.Reason, domain.ErrPrecondition)
	}

	// 4. CAS
	from := casFrom(tr.From, cur.Status())
	errJSON := payloadJSON(tr.Payload, "error")
	if f, ok := tr.Payload["error"].(domain.Failure); ok {
		errJSON, _ = json.Marshal(f)
	}
	errorCode := payloadString(tr.Payload, "error_code")
	if errorCode == "" {
		if f, ok := tr.Payload["error"].(domain.Failure); ok {
			errorCode = f.Code
		}
	}
	setStarted := to == domain.RunnerRunning
	setFinished := to.IsTerminal()
	tag, err := tx.Exec(ctx, `
		UPDATE task_runners
		   SET status          = $2,
		       status_reason   = NULLIF($3, ''),
		       waiting_reason  = CASE WHEN $2 = 'queued' OR $2 = ANY($8) THEN NULL ELSE waiting_reason END,
		       error_code      = CASE WHEN $2 = 'succeeded' THEN NULL
		                              ELSE COALESCE(NULLIF($4, ''), error_code) END,
		       error           = CASE WHEN $2 = 'succeeded' THEN NULL
		                              ELSE COALESCE($5, error) END,
		       started_at      = CASE WHEN $6 THEN COALESCE(started_at, $9) ELSE started_at END,
		       finished_at     = CASE WHEN $7 THEN $9 ELSE finished_at END,
		       updated_at      = $9
		 WHERE task_id = $1 AND attempt_no = $10 AND seq = $11 AND status = ANY($12)`,
		k.TaskID, string(to), tr.Reason, errorCode, nullableBytes(errJSON),
		setStarted, setFinished, domain.TerminalRunnerStatusStrings(), at,
		k.AttemptNo, k.Seq, from)
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: cas: %w", k.ID(), err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Applied{}, fmt.Errorf("store: kernel task_runner %s: cas 未命中（并发修改）: %w",
			k.ID(), domain.ErrConflict)
	}

	spec, _ := domain.LookupTransition(tr.Entity, cur.Status(), to.Status())
	seq, err := s.insertTransitionEvent(ctx, tx, tr, spec, cur.Status(), to.Status(), at,
		evIDs{taskID: k.TaskID, attemptNo: &k.AttemptNo, runnerSeq: &k.Seq})
	if err != nil {
		return domain.Applied{}, err
	}

	if err := s.applyEffects(ctx, tx, tr, cur.Status(), to.Status(), at); err != nil {
		return domain.Applied{}, err
	}

	return domain.Applied{From: cur.Status(), To: to.Status(), Seq: seq}, nil
}

// ---- 事件写入 ------------------------------------------------------------

type evIDs struct {
	taskID    string
	attemptNo *int
	runnerSeq *int
}

// insertTransitionEvent 分配 seq（tasks.last_event_seq 行级自增）并插入 state_events。
func (s *Store) insertTransitionEvent(ctx context.Context, tx pgx.Tx, tr domain.Transition,
	spec domain.TransitionSpec, from, to domain.Status, at time.Time, ids evIDs) (int64, error) {

	seq, err := s.nextSeq(ctx, tx, ids.taskID)
	if err != nil {
		return 0, err
	}

	payload := tr.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("store: kernel: marshal payload: %w", err)
	}

	evType := spec.Event
	if evType == "" {
		evType = domain.EventTaskStateChanged
	}
	actor := tr.Actor
	if actor == "" {
		actor = "system:kernel"
	}
	reason := nullableText(tr.Reason)
	fromS, toS := string(from), string(to)

	_, err = tx.Exec(ctx, `
		INSERT INTO state_events
		  (task_id, attempt_no, runner_seq, seq, type, from_status, to_status, reason, payload, actor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		ids.taskID, ids.attemptNo, ids.runnerSeq, seq, evType, fromS, toS, reason, raw, actor, at)
	if err != nil {
		return 0, fmt.Errorf("store: kernel: insert state_event: %w", err)
	}
	return seq, nil
}

// nextSeq 原子分配该 Task 的下一个事件序号（不用 max(seq)+1，避免读-改-写竞态）。
func (s *Store) nextSeq(ctx context.Context, tx pgx.Tx, taskID string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx,
		`UPDATE tasks SET last_event_seq = last_event_seq + 1 WHERE id = $1 RETURNING last_event_seq`,
		taskID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: nextSeq: task %s: %w", taskID, domain.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("store: nextSeq: task %s: %w", taskID, err)
	}
	return seq, nil
}

// ---- 小工具 --------------------------------------------------------------

func casFrom(declared []domain.Status, cur domain.Status) []string {
	out := make([]string, 0, len(declared)+1)
	for _, s := range declared {
		out = append(out, string(s))
	}
	if !containsStatus(declared, cur) {
		out = append(out, string(cur))
	}
	return out
}

func containsStatus(set []domain.Status, s domain.Status) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func nullableText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func nullableBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// payloadValue 取 payload 里的原值。
func payloadValue(p map[string]any, key string) any {
	if p == nil {
		return nil
	}
	return p[key]
}

// payloadJSON 把 payload[key] 序列化成 jsonb 绑定值。
func payloadJSON(p map[string]any, key string) []byte {
	v, ok := p[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case json.RawMessage:
		return t
	case []byte:
		return t
	case string:
		if t == "" {
			return nil
		}
		return []byte(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func payloadString(p map[string]any, key string) string {
	v, ok := p[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
