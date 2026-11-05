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
//  2. SELECT ... FOR UPDATE 锁实体行（调用方必须先锁 Run，Kernel 不重复锁）；
//  3. 当前状态 == To → Noop；不在 From 集合 → ErrConflict；
//  4. UPDATE ... WHERE id=? AND status IN (from) 做 CAS 双保险；
//  5. UPDATE runs SET last_event_seq = last_event_seq+1 RETURNING 分配 seq；
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
	case domain.EntityRun:
		return s.applyRun(ctx, tx, tr, at)
	case domain.EntityTask:
		return s.applyTask(ctx, tx, tr, at)
	case domain.EntityAttempt:
		return s.applyAttempt(ctx, tx, tr, at)
	case domain.EntityTester:
		// Tester 是启动期注册表状态（02 §2.4），不落 status 列、不走运行期 Kernel。
		return domain.Applied{}, fmt.Errorf("store: kernel: tester 状态机只在注册表同步期使用: %w", domain.ErrPrecondition)
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
	case domain.EntityRun:
		return domain.RunStatuses(domain.RunSubmitted, domain.RunQueued, domain.RunDeploying,
			domain.RunTesting, domain.RunCleanup, domain.RunCancelling, domain.RunSucceeded,
			domain.RunCompletedWithErrors, domain.RunServiceUnavailable, domain.RunFailed, domain.RunCanceled)
	case domain.EntityTask:
		return domain.TaskStatuses(domain.TaskPending, domain.TaskQueued, domain.TaskPreparing,
			domain.TaskRunning, domain.TaskSucceeded, domain.TaskFailed, domain.TaskCanceled, domain.TaskSkipped)
	case domain.EntityAttempt:
		return domain.AttemptStatuses(domain.AttemptPreparing, domain.AttemptRunning,
			domain.AttemptSucceeded, domain.AttemptFailed, domain.AttemptCanceled)
	}
	return nil
}

// ---- Run ----------------------------------------------------------------

func (s *Store) applyRun(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) (domain.Applied, error) {
	var cur domain.RunStatus
	var hasEndpoint bool
	err := tx.QueryRow(ctx,
		`SELECT status, (endpoint IS NOT NULL) FROM runs WHERE id = $1 FOR UPDATE`, tr.ID).
		Scan(&cur, &hasEndpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: %w", tr.ID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: lock: %w", tr.ID, err)
	}

	to := domain.RunStatus(tr.To)

	// 3. 幂等与前置状态
	if cur == to {
		return domain.Applied{From: cur.Status(), To: to.Status(), Noop: true}, nil
	}
	if len(tr.From) > 0 && !containsStatus(tr.From, cur.Status()) {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: 当前状态 %s 不在允许集合 %v: %w",
			tr.ID, cur, tr.From, domain.ErrConflict)
	}
	if err := requireEdge(tr.Entity, tr.ID, cur.Status(), to.Status()); err != nil {
		return domain.Applied{}, err
	}

	// 守卫：有 Endpoint 才能进 testing（C-3.4）。
	if to == domain.RunTesting && !hasEndpoint && payloadValue(tr.Payload, "endpoint") == nil {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: 缺少 endpoint，不能进入 testing: %w",
			tr.ID, domain.ErrPrecondition)
	}
	// 守卫：由 cleanup 判定终态时，所有 Task 必须已终态（C-3.4、C-5.8）。
	if cur == domain.RunCleanup && (to == domain.RunSucceeded ||
		to == domain.RunCompletedWithErrors || to == domain.RunServiceUnavailable) {
		var pending int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE run_id = $1 AND NOT (status = ANY($2))`,
			tr.ID, domain.TerminalTaskStatusStrings()).Scan(&pending); err != nil {
			return domain.Applied{}, fmt.Errorf("store: kernel run %s: guard tasks: %w", tr.ID, err)
		}
		if pending > 0 {
			return domain.Applied{}, fmt.Errorf("store: kernel run %s: 仍有 %d 个未终态 Task: %w",
				tr.ID, pending, domain.ErrPrecondition)
		}
	}

	// 4. CAS
	from := casFrom(tr.From, cur.Status())
	setStarted := to == domain.RunDeploying || to == domain.RunTesting || to == domain.RunCleanup
	setFinished := to.IsTerminal()
	errJSON := payloadJSON(tr.Payload, "error")
	endpointJSON := payloadJSON(tr.Payload, "endpoint")
	deployJSON := payloadJSON(tr.Payload, "deploy")
	if len(errJSON) > 0 && len(tr.Payload) > 0 {
		// 允许直接传 domain.Failure
		if f, ok := tr.Payload["error"].(domain.Failure); ok {
			errJSON, _ = json.Marshal(f)
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE runs
		   SET status      = $2,
		       revision    = revision + 1,
		       started_at  = CASE WHEN $3 THEN COALESCE(started_at, $4) ELSE started_at END,
		       finished_at = CASE WHEN $5 THEN $4 ELSE finished_at END,
		       error       = COALESCE($6, error),
		       endpoint    = COALESCE($7, endpoint),
		       -- deploy 必须**合并**：冻结字段（release_name/values_digest/…）写一次，
		       -- 之后每次状态推进只带变化的键；整段覆盖会让重启后无法找回 Genet release（§9.2）。
		       deploy      = CASE
		                       WHEN $8::jsonb IS NULL THEN deploy
		                       ELSE COALESCE(deploy, '{}'::jsonb) || $8::jsonb
		                     END
		 WHERE id = $1 AND status = ANY($9)`,
		tr.ID, string(to), setStarted, at, setFinished,
		nullableBytes(errJSON), nullableBytes(endpointJSON), nullableBytes(deployJSON), from)
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: cas: %w", tr.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Applied{}, fmt.Errorf("store: kernel run %s: cas 未命中（并发修改）: %w", tr.ID, domain.ErrConflict)
	}

	// 5+6. seq 与事件
	spec, _ := domain.LookupTransition(tr.Entity, cur.Status(), to.Status())
	seq, err := s.insertTransitionEvent(ctx, tx, tr, spec, cur.Status(), to.Status(), at, evIDs{runID: tr.ID})
	if err != nil {
		return domain.Applied{}, err
	}

	// 7. effects
	if err := s.applyEffects(ctx, tx, tr, cur.Status(), to.Status(), at); err != nil {
		return domain.Applied{}, err
	}

	return domain.Applied{From: cur.Status(), To: to.Status(), Seq: seq}, nil
}

// ---- Task ---------------------------------------------------------------

func (s *Store) applyTask(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) (domain.Applied, error) {
	var cur domain.TaskStatus
	var runID string
	var seed int
	var endpointKey *string
	err := tx.QueryRow(ctx, `
		SELECT status, run_id, seq, endpoint_key
		  FROM tasks WHERE id = $1 FOR UPDATE`, tr.ID).
		Scan(&cur, &runID, &seed, &endpointKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: %w", tr.ID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: lock: %w", tr.ID, err)
	}

	to := domain.TaskStatus(tr.To)

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

	// 守卫 1：queued 的 Task 必须有 endpoint_key（C-4.5，不得用占位键绕过）。
	if to == domain.TaskQueued && (endpointKey == nil || *endpointKey == "") {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: endpoint_key 为空，不能进入 queued: %w",
			tr.ID, domain.ErrPrecondition)
	}
	// 守卫 2：skipped 必须有合法 reason（03 §3.2 CHECK）。
	if to == domain.TaskSkipped && !domain.ValidSkipReason(tr.Reason) {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: skip reason %q 非法: %w",
			tr.ID, tr.Reason, domain.ErrPrecondition)
	}

	// 4. CAS
	from := casFrom(tr.From, cur.Status())
	errJSON := payloadJSON(tr.Payload, "error")
	if f, ok := tr.Payload["error"].(domain.Failure); ok {
		errJSON, _ = json.Marshal(f)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET status          = $2,
		       status_reason   = NULLIF($3, ''),
		       updated_at      = $4,
		       waiting_reason  = CASE WHEN $2 = 'queued' OR $2 = ANY($7)
		                              THEN NULL ELSE waiting_reason END,
		       error           = COALESCE($5, error)
		 WHERE id = $1 AND status = ANY($6)`,
		tr.ID, string(to), tr.Reason, at, nullableBytes(errJSON), from,
		domain.TerminalTaskStatusStrings())
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: cas: %w", tr.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Applied{}, fmt.Errorf("store: kernel task %s: cas 未命中（并发修改）: %w", tr.ID, domain.ErrConflict)
	}

	spec, _ := domain.LookupTransition(tr.Entity, cur.Status(), to.Status())
	taskID := tr.ID
	seq, err := s.insertTransitionEvent(ctx, tx, tr, spec, cur.Status(), to.Status(), at,
		evIDs{runID: runID, taskID: &taskID})
	if err != nil {
		return domain.Applied{}, err
	}

	if err := s.applyEffects(ctx, tx, tr, cur.Status(), to.Status(), at); err != nil {
		return domain.Applied{}, err
	}

	return domain.Applied{From: cur.Status(), To: to.Status(), Seq: seq}, nil
}

// ---- Attempt ------------------------------------------------------------

func (s *Store) applyAttempt(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) (domain.Applied, error) {
	var cur domain.AttemptStatus
	var taskID, runID string
	err := tx.QueryRow(ctx, `
		SELECT a.status, a.task_id, t.run_id
		  FROM attempts a JOIN tasks t ON t.id = a.task_id
		 WHERE a.id = $1 FOR UPDATE OF a`, tr.ID).
		Scan(&cur, &taskID, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Applied{}, fmt.Errorf("store: kernel attempt %s: %w", tr.ID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel attempt %s: lock: %w", tr.ID, err)
	}

	to := domain.AttemptStatus(tr.To)
	if cur == to {
		return domain.Applied{From: cur.Status(), To: to.Status(), Noop: true}, nil
	}
	if len(tr.From) > 0 && !containsStatus(tr.From, cur.Status()) {
		return domain.Applied{}, fmt.Errorf("store: kernel attempt %s: 当前状态 %s 不在允许集合 %v: %w",
			tr.ID, cur, tr.From, domain.ErrConflict)
	}
	if err := requireEdge(tr.Entity, tr.ID, cur.Status(), to.Status()); err != nil {
		return domain.Applied{}, err
	}

	from := casFrom(tr.From, cur.Status())
	errJSON := payloadJSON(tr.Payload, "error")
	if f, ok := tr.Payload["error"].(domain.Failure); ok {
		errJSON, _ = json.Marshal(f)
	}
	phase := payloadString(tr.Payload, "phase")
	execRef := payloadJSON(tr.Payload, "executor_ref")
	exitCode := payloadInt(tr.Payload, "exit_code")
	logRef := payloadString(tr.Payload, "log_ref")
	artifactRef := payloadString(tr.Payload, "artifact_ref")
	resultRef := payloadString(tr.Payload, "result_ref")

	setStarted := to == domain.AttemptRunning
	setFinished := to.IsTerminal()

	tag, err := tx.Exec(ctx, `
		UPDATE attempts
		   SET status       = $2,
		       phase        = COALESCE($3, phase),
		       exit_code    = COALESCE($4, exit_code),
		       started_at   = CASE WHEN $5 THEN COALESCE(started_at, $6) ELSE started_at END,
		       finished_at  = CASE WHEN $7 THEN $6 ELSE finished_at END,
		       error        = COALESCE($8, error),
		       executor_ref = COALESCE($9, executor_ref),
		       log_ref      = COALESCE($10, log_ref),
		       artifact_ref = COALESCE($11, artifact_ref),
		       result_ref   = COALESCE($12, result_ref)
		 WHERE id = $1 AND status = ANY($13)`,
		tr.ID, string(to), nullableText(phase), exitCode, setStarted, at, setFinished,
		nullableBytes(errJSON), nullableBytes(execRef), nullableText(logRef),
		nullableText(artifactRef), nullableText(resultRef), from)
	if err != nil {
		return domain.Applied{}, fmt.Errorf("store: kernel attempt %s: cas: %w", tr.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Applied{}, fmt.Errorf("store: kernel attempt %s: cas 未命中（并发修改）: %w", tr.ID, domain.ErrConflict)
	}

	spec, _ := domain.LookupTransition(tr.Entity, cur.Status(), to.Status())
	attemptID := tr.ID
	seq, err := s.insertTransitionEvent(ctx, tx, tr, spec, cur.Status(), to.Status(), at,
		evIDs{runID: runID, taskID: &taskID, attemptID: &attemptID})
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
	runID     string
	taskID    *string
	attemptID *string
}

// insertTransitionEvent 分配 seq（runs.last_event_seq 行级自增）并插入 state_events。
func (s *Store) insertTransitionEvent(ctx context.Context, tx pgx.Tx, tr domain.Transition,
	spec domain.TransitionSpec, from, to domain.Status, at time.Time, ids evIDs) (int64, error) {

	seq, err := s.nextSeq(ctx, tx, ids.runID)
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
		evType = domain.EventRunStateChanged
	}
	actor := tr.Actor
	if actor == "" {
		actor = "system:kernel"
	}
	reason := nullableText(tr.Reason)
	fromS, toS := string(from), string(to)

	_, err = tx.Exec(ctx, `
		INSERT INTO state_events
		  (run_id, task_id, attempt_id, seq, type, from_status, to_status, reason, payload, actor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		ids.runID, ids.taskID, ids.attemptID, seq, evType, fromS, toS, reason, raw, actor, at)
	if err != nil {
		return 0, fmt.Errorf("store: kernel: insert state_event: %w", err)
	}
	return seq, nil
}

// nextSeq 原子分配该 Run 的下一个事件序号（不用 max(seq)+1，避免读-改-写竞态）。
func (s *Store) nextSeq(ctx context.Context, tx pgx.Tx, runID string) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx,
		`UPDATE runs SET last_event_seq = last_event_seq + 1 WHERE id = $1 RETURNING last_event_seq`,
		runID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: nextSeq: run %s: %w", runID, domain.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("store: nextSeq: run %s: %w", runID, err)
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

// payloadStrings 取 []string（或 []any 的字符串元素）型 payload 字段。
func payloadStrings(p map[string]any, key string) []string {
	v, ok := p[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func payloadInt(p map[string]any, key string) *int32 {
	v, ok := p[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case int:
		c := int32(n)
		return &c
	case int32:
		return &n
	case int64:
		c := int32(n)
		return &c
	case float64:
		c := int32(n)
		return &c
	}
	return nil
}
