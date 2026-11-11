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

// 本文件是控制面内核给编排层（orchestrator）的"组合事务"出口（C-2.5）。
//
// 每个方法自己开事务并保证锁顺序 Task → runner 执行记录（02 §3.8），
// 对外只暴露事实（Applied / 受影响 key / Job 名），不暴露 pgx 类型。

// 全局并发 advisory 锁名（C-4.4：锁名常量只允许一处定义）。
const globalConcurrencyLockName = "argus:global-concurrency"

// ---- 调度 / 执行 ---------------------------------------------------------

// ClaimRequest 是调度器"取活"的原子请求（C-4.1/C-4.2/C-4.3/C-4.4）。
type ClaimRequest struct {
	Key domain.RunnerKey
	// EndpointKey 是该执行记录的 endpoint_key（仅外接模式非空；部署模式不建租约）。
	EndpointKey string
	// Exclusive 来自 Runner 声明的 concurrency.endpoint_exclusive（仅外接模式有效）。
	Exclusive bool
	// GlobalLimit 是 AG_CONCURRENCY_MAX；<=0 表示不限制。
	GlobalLimit int
	// JobID 可预分配；空则用 JobName 派生。
	JobID        string
	JobNamespace string
	ExecutorRef  []byte
}

// ClaimResult 是抢到之后的产物。
type ClaimResult struct {
	Key domain.RunnerKey
	// JobName 是派生的 K8s Job 名（幂等键 = (task_id, attempt_no, seq) 的派生，附录 B）。
	JobName string
	// Seq 是本次 queued→preparing 转移的事件序号。
	Seq int64
}

// ClaimForExecution 一个事务里按固定锁顺序完成（C-4.2）：
//
//	锁 Task → 端点 advisory 锁（仅外接模式有 key）→ 全局并发 advisory 锁 → 计数
//	→ CAS task_runners queued→preparing → 插入 endpoint_leases → 回填 Job 句柄。
//
// 抢不到是**正常结果**，不是错误：返回 claimed=false + reason
// （reason 取 domain.Wait* 之一，由调用方写回 waiting_reason）。
func (s *Store) ClaimForExecution(ctx context.Context, req ClaimRequest) (ClaimResult, bool, string, error) {
	if !req.Key.Valid() {
		return ClaimResult{}, false, "", fmt.Errorf("store: claim: 非法 RunnerKey %+v", req.Key)
	}

	tx, err := s.Begin(ctx)
	if err != nil {
		return ClaimResult{}, false, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	// 1. 统一锁顺序第一步：锁 Task（防与取消/abort 反向加锁死锁，C-4.2）。
	if _, err := s.Tasks.Lock(ctx, tx, req.Key.TaskID); err != nil {
		return ClaimResult{}, false, "", err
	}

	// 2. 端点槽位（仅外接模式：endpoint_key 非空才有租约，部署模式不建，C-4.3）。
	if req.EndpointKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, req.EndpointKey); err != nil {
			return ClaimResult{}, false, "", fmt.Errorf("store: claim: 端点 advisory 锁: %w", err)
		}
		holders, err := s.Leases.Holders(ctx, tx, req.EndpointKey)
		if err != nil {
			return ClaimResult{}, false, "", err
		}
		mine := false
		exclusiveHeld := false
		for _, l := range holders {
			if l.TaskID == req.Key.TaskID && l.AttemptNo == req.Key.AttemptNo && l.RunnerSeq == req.Key.Seq {
				mine = true
				continue
			}
			if l.Exclusive {
				exclusiveHeld = true
			}
		}
		if req.Exclusive && len(holders) > 0 && !mine {
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		}
		if !req.Exclusive && exclusiveHeld && !mine {
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		}
	}

	// 3. 全局并发 permit（排在端点锁之后，C-4.4）。
	if req.GlobalLimit > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, globalConcurrencyLockName); err != nil {
			return ClaimResult{}, false, "", fmt.Errorf("store: claim: 全局 advisory 锁: %w", err)
		}
		active, err := s.RunnerRuns.CountActive(ctx, tx)
		if err != nil {
			return ClaimResult{}, false, "", err
		}
		if active >= req.GlobalLimit {
			return ClaimResult{}, false, domain.WaitGlobalLimit, nil
		}
	}

	// 4. CAS task_runners queued→preparing（同 Task 的下一 runner 由管道推进置 queued）。
	applied, err := s.Apply(ctx, tx, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: req.Key.ID(),
		From:    []domain.Status{domain.RunnerQueued.Status()},
		To:      domain.RunnerPreparing.Status(),
		Command: "prepare", Actor: "system:scheduler",
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		case errors.Is(err, domain.ErrPrecondition):
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		}
		return ClaimResult{}, false, "", err
	}

	// 5. 端点租约（与状态推进同事务；执行记录终态时释放）。
	if req.EndpointKey != "" {
		if _, err := s.Leases.TryAcquire(ctx, tx, req.Key, req.EndpointKey, req.Exclusive); err != nil {
			return ClaimResult{}, false, "", err
		}
	}

	// 6. Job 句柄与状态推进同事务落库（C-2.5）。
	jobID := req.JobID
	if jobID == "" {
		jobID = JobName(req.Key)
	}
	if err := s.RunnerRuns.SetPhysicalFacts(ctx, tx, req.Key, PhysicalFacts{
		JobID:        jobID,
		JobNamespace: nullableText(req.JobNamespace),
		ExecutorRef:  req.ExecutorRef,
	}); err != nil {
		return ClaimResult{}, false, "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return ClaimResult{}, false, "", fmt.Errorf("store: claim: commit: %w", err)
	}
	committed = true

	// 提交后通知（02 §3.7）。
	if s.notifier != nil && applied.Seq > 0 {
		s.notifier.Notify(req.Key.TaskID, applied.Seq)
	}
	return ClaimResult{Key: req.Key, JobName: jobID, Seq: applied.Seq}, true, "", nil
}

// ---- runner 终态推进 -----------------------------------------------------

// RunnerFinish 是 FinishRunnerRun 的组合输入。
type RunnerFinish struct {
	Key domain.RunnerKey
	// Succeeded=false 时 Failure 必填（错误码用于终态判定与 abort 判定）。
	Succeeded bool
	Failure   *domain.Failure
	Phase     string
	ExitCode  *int
	// ImageDigest 是 K8s imageID 解析出的实际 digest（可得时写入，D-36）。
	ImageDigest string
	FinishedAt  time.Time

	// ---- 产物引用（03 §3.1；相对 Task 目录）----
	// 只填**真实存在**的文件：空字符串表示"没有这个产物"，落库保持 NULL。
	ExportsRef string
	ResultRef  string
	LogRef     string
}

// RunnerFinishResult 汇报推进结果。
type RunnerFinishResult struct {
	RunnerApplied domain.Applied
	// NextQueued 是串行放行的下一条执行记录（成功或失败(continue) 时恰一条）。
	NextQueued *domain.RunnerKey
	// AbortTriggered 表示 on_failure=abort_task 生效：后续 runner 已批量 SKIPPED。
	AbortTriggered bool
	Skipped        []domain.RunnerKey
	// TaskTerminal / TaskStatus：clean 之后（或无 clean 的管道收尾）Task 落终态。
	TaskTerminal bool
	TaskStatus   domain.TaskStatus
}

// FinishRunnerRun 落一次 runner 执行记录终态并推进 Task 管道（C-5.4/C-5.5/C-5.8）。
//
// 一个事务内：
//  1. CAS 执行记录到终态（先落库者胜；冲突按幂等忽略）；
//  2. 释放端点租约（effect）；
//  3. 失败且 on_failure=abort_task → 其余未执行 runner 批量 skipped（传播到 clean 为止）；
//  4. 失败且 continue（或成功）→ 下一条执行记录置 queued（严格串行推进）；
//  5. 若本次终态的是 role=clean（或管道已无未执行 runner）→ 全部终态时按
//     domain.DecideTaskOutcome 判 Task 终态（clean 失败不改终态）。
func (s *Store) FinishRunnerRun(ctx context.Context, in RunnerFinish) (*RunnerFinishResult, error) {
	out := &RunnerFinishResult{}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Tasks.Lock(ctx, tx, in.Key.TaskID); err != nil {
			return err
		}
		rr, err := s.RunnerRuns.Get(ctx, tx, in.Key)
		if err != nil {
			return err
		}

		// 1. 执行记录终态（CAS；先落库者胜）。
		payload := map[string]any{}
		if in.Phase != "" {
			payload["phase"] = in.Phase
		}
		if in.ExitCode != nil {
			payload["exit_code"] = *in.ExitCode
		}
		failure := domain.NewFailure(domain.CodeInternalError, "执行失败", nil)
		if in.Failure != nil {
			failure = *in.Failure
		}
		payload["error_code"] = failure.Code
		if !in.Succeeded || failure.Code != domain.CodeInternalError {
			payload["error"] = failure
		}
		if in.ExportsRef != "" {
			payload["exports_ref"] = in.ExportsRef
		}
		if in.ResultRef != "" {
			payload["result_ref"] = in.ResultRef
		}
		if in.LogRef != "" {
			payload["log_ref"] = in.LogRef
		}
		to := domain.RunnerFailed
		if in.Succeeded {
			to = domain.RunnerSucceeded
		}
		applied, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRunnerRun, ID: in.Key.ID(),
			From:    []domain.Status{domain.RunnerPreparing.Status(), domain.RunnerRunning.Status()},
			To:      to.Status(),
			Command: "finish", Reason: failure.Code, Actor: "system:executor",
			Payload: payload, At: in.FinishedAt,
		})
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return nil // 先落库者胜：不覆盖已有结论
			}
			return err
		}
		out.RunnerApplied = applied

		// 物理事实：退出码 / digest（不参与状态判定）。
		facts := PhysicalFacts{ExitCode: in.ExitCode, ErrorCode: &failure.Code}
		if in.ImageDigest != "" {
			d := in.ImageDigest
			facts.ImageDigest = &d
		}
		if in.ExportsRef != "" {
			facts.ExportsRef = &in.ExportsRef
		}
		if in.ResultRef != "" {
			facts.ResultRef = &in.ResultRef
		}
		if in.LogRef != "" {
			facts.LogRef = &in.LogRef
		}
		_ = s.RunnerRuns.SetPhysicalFacts(ctx, tx, in.Key, facts)

		// 2+3+4. 管道推进 / abort 传播。
		if !in.Succeeded && rr.OnFailure == domain.OnFailureAbortTask {
			skipped, err := s.skipUpstreamRunners(ctx, tx, in.Key, domain.SkipUpstreamAbort, in.FinishedAt)
			if err != nil {
				return err
			}
			out.AbortTriggered = true
			out.Skipped = skipped
		} else {
			next, err := s.queueNextRunner(ctx, tx, in.Key, in.FinishedAt)
			if err != nil {
				return err
			}
			out.NextQueued = next
		}

		// 5. 全部终态 → Task 终态判定（clean 失败只记 cleanup_failed，不改终态）。
		nonTerminal, err := s.RunnerRuns.CountNonTerminal(ctx, tx, in.Key.TaskID, in.Key.AttemptNo)
		if err != nil {
			return err
		}
		if nonTerminal == 0 {
			status, err := s.finishTaskLocked(ctx, tx, in.Key.TaskID, in.Key.AttemptNo, in.FinishedAt)
			if err != nil {
				return err
			}
			out.TaskTerminal = true
			out.TaskStatus = status
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Task 终态已提交：触发报告生成等外部动作（事务内禁止文件 IO）。
	if out.TaskTerminal {
		s.fireTaskFinished(in.Key.TaskID)
	}
	return out, nil
}

// queueNextRunner 串行放行下一条执行记录：seq+1 存在且 pending → queued（C-5.4）。
func (s *Store) queueNextRunner(ctx context.Context, tx pgx.Tx, cur domain.RunnerKey, at time.Time) (*domain.RunnerKey, error) {
	var exists bool
	next := domain.RunnerKey{TaskID: cur.TaskID, AttemptNo: cur.AttemptNo, Seq: cur.Seq + 1}
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM task_runners
		 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3 AND status = 'pending')`,
		next.TaskID, next.AttemptNo, next.Seq).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("store: queue next: %w", err)
	}
	if !exists {
		return nil, nil
	}
	if _, err := s.Apply(ctx, tx, domain.Transition{
		Entity: domain.EntityRunnerRun, ID: next.ID(),
		From:    []domain.Status{domain.RunnerPending.Status()},
		To:      domain.RunnerQueued.Status(),
		Command: "queue", Actor: "system:orchestrator", At: at,
	}); err != nil {
		return nil, err
	}
	return &next, nil
}

// skipUpstreamRunners 把 abort_task 传播到其余未执行的 runner（传播到 clean 为止，
// C-5.5）：pending/queued 且 role <> 'clean' 批量置 skipped(reason)。
func (s *Store) skipUpstreamRunners(ctx context.Context, tx pgx.Tx, failed domain.RunnerKey,
	reason string, at time.Time) ([]domain.RunnerKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT seq FROM task_runners
		 WHERE task_id = $1 AND attempt_no = $2 AND seq <> $3
		   AND status IN ('pending','queued') AND role <> 'clean'
		 ORDER BY seq`, failed.TaskID, failed.AttemptNo, failed.Seq)
	if err != nil {
		return nil, fmt.Errorf("store: skip upstream: %w", err)
	}
	var seqs []int
	for rows.Next() {
		var sq int
		if err := rows.Scan(&sq); err != nil {
			rows.Close()
			return nil, err
		}
		seqs = append(seqs, sq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var skipped []domain.RunnerKey
	for _, sq := range seqs {
		k := domain.RunnerKey{TaskID: failed.TaskID, AttemptNo: failed.AttemptNo, Seq: sq}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityRunnerRun, ID: k.ID(),
			From:    []domain.Status{domain.RunnerPending.Status(), domain.RunnerQueued.Status()},
			To:      domain.RunnerSkipped.Status(),
			Command: "skip", Reason: reason, Actor: "system:orchestrator", At: at,
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return nil, err
		}
		skipped = append(skipped, k)
	}
	return skipped, nil
}

// finishTaskLocked 在已持 Task 锁的事务里按 C-5.8 判 Task 终态，并落 attempt 聚合状态。
func (s *Store) finishTaskLocked(ctx context.Context, tx pgx.Tx, taskID string, attemptNo int,
	at time.Time) (domain.TaskStatus, error) {

	t, err := s.Tasks.Get(ctx, tx, taskID)
	if err != nil {
		return "", err
	}
	cur := domain.TaskStatus(t.Status)
	// attempt 触发的重试（terminal → queued）之后 Task 已回到 queued/running；
	// 这里只在 running 态做终态判定。
	if cur != domain.TaskRunning {
		return cur, nil
	}

	runs, err := s.RunnerRuns.ListByAttempt(ctx, tx, taskID, attemptNo)
	if err != nil {
		return "", err
	}
	input := domain.TaskOutcomeInput{}
	var platformTimeout bool
	for _, rr := range runs {
		summary := domain.RunnerSummary{Seq: rr.Seq, Role: rr.Role, Status: domain.RunnerStatus(rr.Status)}
		if len(rr.Error) > 0 {
			// 用指针接 retryable：只有实例真的带了该字段才算"实例给了值"。
			var f struct {
				Code      string `json:"code"`
				Retryable *bool  `json:"retryable"`
			}
			if err := json.Unmarshal(rr.Error, &f); err == nil {
				summary.ErrorCode = f.Code
				if f.Retryable != nil {
					summary.Retryable = *f.Retryable
					summary.HasRetryable = true
				}
			}
		}
		if summary.ErrorCode == "" && rr.ErrorCode != nil {
			summary.ErrorCode = *rr.ErrorCode
		}
		// 超时（watchdog 判定）属平台侧事实：pre_check 之外的 RUNNER_TIMEOUT 走
		// 平台错误（C-5.8 #2），Pre-Check 的超时仍走 #4（可重试 → service_unavailable）。
		if rr.Role != domain.RolePreCheck && summary.Status == domain.RunnerFailed &&
			summary.ErrorCode == domain.CodeRunnerTimeout {
			platformTimeout = true
		}
		input.Runners = append(input.Runners, summary)
	}
	input.PlatformError = platformTimeout
	status := domain.DecideTaskOutcome(input)

	// Task 终态要带 error（03 §3.1）：取第一个失败 runner 的 Failure。
	payload := map[string]any{}
	if status != domain.TaskSucceeded && status != domain.TaskCanceled {
		if f := firstRunnerFailure(runs); f != nil {
			payload["error"] = *f
		}
	}
	if _, err := s.Apply(ctx, tx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskRunning.Status()},
		To:      status.Status(),
		Command: "finish", Actor: "system:orchestrator", Payload: payload, At: at,
	}); err != nil {
		return "", err
	}

	// attempt 聚合状态落库（终态不覆盖）。
	attStatus := domain.AttemptSucceeded
	switch status {
	case domain.TaskFailed, domain.TaskServiceUnavailable:
		attStatus = domain.AttemptFailed
	case domain.TaskCanceled:
		attStatus = domain.AttemptCanceled
	}
	var attErr []byte
	if f := firstRunnerFailure(runs); f != nil {
		attErr, _ = json.Marshal(*f)
	}
	if err := s.Attempts.SetStatus(ctx, tx, taskID, attemptNo, attStatus, attErr); err != nil {
		return "", err
	}
	return status, nil
}

// firstRunnerFailure 取第一个可解释失败原因的 runner Failure（pre_check 优先）。
func firstRunnerFailure(runs []RunnerRun) *domain.Failure {
	var precheck, any *domain.Failure
	for i := range runs {
		rr := &runs[i]
		if len(rr.Error) == 0 {
			continue
		}
		var f domain.Failure
		if err := json.Unmarshal(rr.Error, &f); err != nil || f.Code == "" {
			continue
		}
		if rr.Role == domain.RolePreCheck && precheck == nil {
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

// ---- 取消 / abort / 重试 -------------------------------------------------

// CancelTask 取消 Task（C-6.1）：Task→cancelling（effects 批量 skip 未执行 runner +
// 置 cancel_requested），返回仍需异步删 Job 的执行记录。clean 执行完由调用方落 canceled。
func (s *Store) CancelTask(ctx context.Context, taskID, actor string) ([]domain.RunnerKey, error) {
	var active []domain.RunnerKey
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Tasks.Lock(ctx, tx, taskID); err != nil {
			return err
		}
		t, err := s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if domain.TaskStatus(t.Status).IsTerminal() {
			return fmt.Errorf("store: cancel task %s: 已终态: %w", taskID, domain.ErrConflict)
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    domain.TaskStatuses(domain.TaskNonTerminal()...),
			To:      domain.TaskCancelling.Status(),
			Command: "cancel", Actor: actor, At: s.now(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT attempt_no, seq FROM task_runners
			 WHERE task_id = $1 AND attempt_no = (SELECT current_attempt FROM tasks WHERE id = $1)
			   AND status IN ('preparing','running')`, taskID)
		if err != nil {
			return fmt.Errorf("store: cancel task %s: active runners: %w", taskID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var k domain.RunnerKey
			k.TaskID = taskID
			if err := rows.Scan(&k.AttemptNo, &k.Seq); err != nil {
				return err
			}
			active = append(active, k)
		}
		return rows.Err()
	})
	return active, err
}

// AbortTaskOthers 是 abort_task 的编排出口（C-5.5）：同事务锁 Task，收口其余未执行的
// runner（传播到 clean 为止），并置 cancel_requested。返回仍需异步删 Job 的执行记录。
func (s *Store) AbortTaskOthers(ctx context.Context, taskID string, failedKey domain.RunnerKey, reason string) ([]domain.RunnerKey, error) {
	var active []domain.RunnerKey
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Tasks.Lock(ctx, tx, taskID); err != nil {
			return err
		}
		at := s.now()
		if _, err := s.skipUpstreamRunners(ctx, tx, failedKey, reason, at); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT attempt_no, seq FROM task_runners
			 WHERE task_id = $1 AND attempt_no = $2 AND seq <> $3
			   AND status IN ('preparing','running')`,
			taskID, failedKey.AttemptNo, failedKey.Seq)
		if err != nil {
			return fmt.Errorf("store: abort others: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var k domain.RunnerKey
			k.TaskID = taskID
			if err := rows.Scan(&k.AttemptNo, &k.Seq); err != nil {
				return err
			}
			active = append(active, k)
		}
		return rows.Err()
	})
	return active, err
}

// ErrRetryConflict 表示重试前置条件不满足（Task 非终态 / attempt 达上限）。
// API 层统一映射成 409，detail.reason 说明具体原因。
var ErrRetryConflict = errors.New("store: retry conflict")

// RetryTask 执行整 Task 重试（§7.8 / 03 §4.5）：新 attempt 全量重跑，含 fresh deploy。
//
// 前置：Task 已终态（succeeded/failed/service_unavailable；canceled 不可重试）、
// attempt_no 未达上限（超限返回 ATTEMPT_EXHAUSTED）。新 attempt 的 task_runners
// 按**最后一个 attempt 的冻结快照**全量重建（seq/runner 定义/timeout 不变，
// fresh 快照：status 重置 pending、input_ref 重落档、job_id 重新派生）。
func (s *Store) RetryTask(ctx context.Context, taskID, actor, role string, attemptMax int) (int, error) {
	at := s.now()
	attemptNo := 0
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Tasks.Lock(ctx, tx, taskID); err != nil {
			return err
		}
		t, err := s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		// attempt 上限检查先于状态检查：已达上限时无论 Task 处于什么状态
		// 都返回 ATTEMPT_EXHAUSTED（含"上一次 retry 已把 Task 置回 queued"的并发窗口）。
		next := t.CurrentAttempt + 1
		if attemptMax > 0 && next > attemptMax {
			return domain.NewFailureError(domain.CodeAttemptExhausted,
				fmt.Sprintf("Task %s 的 attempt 数已达上限 %d", taskID, attemptMax),
				map[string]any{"task_id": taskID, "attempt_no": t.CurrentAttempt, "attempt_max": attemptMax})
		}
		status := domain.TaskStatus(t.Status)
		switch status {
		case domain.TaskSucceeded, domain.TaskFailed, domain.TaskServiceUnavailable:
			// 可重试。
		case domain.TaskCanceled:
			return domain.NewFailureError(domain.CodeConflict, "Task 已取消，不能重试", map[string]any{"task_id": taskID})
		default:
			return domain.NewFailureError(domain.CodeConflict, fmt.Sprintf("Task 尚未结束（%s），不能重试", status), map[string]any{"task_id": taskID, "status": string(status)})
		}

		// 冻结快照：取最后一个 attempt 的执行记录全量重建（fresh，D-69）。
		prev, err := s.RunnerRuns.ListByAttempt(ctx, tx, taskID, t.CurrentAttempt)
		if err != nil {
			return err
		}
		if len(prev) == 0 {
			return fmt.Errorf("store: retry task %s: 无历史 attempt 快照: %w", taskID, domain.ErrPrecondition)
		}

		// Task 回到 queued（终态 → queued 重试边；02 §2"重试做了什么"）。
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskSucceeded.Status(), domain.TaskFailed.Status(), domain.TaskServiceUnavailable.Status()},
			To:      domain.TaskQueued.Status(),
			Command: "retry", Actor: actorOf(actor), At: at,
		}); err != nil {
			return err
		}

		a, err := s.Attempts.Create(ctx, tx, CreateAttemptParams{TaskID: taskID, Trigger: "retry", AttemptNo: next})
		if err != nil {
			return err
		}
		attemptNo = a.AttemptNo
		for _, rr := range prev {
			fresh := rr // 复制定义快照
			fresh.AttemptNo = next
			fresh.Status = string(domain.RunnerPending)
			fresh.StatusReason = nil
			fresh.WaitingReason = nil
			fresh.ExitCode = nil
			fresh.ErrorCode = nil
			fresh.Error = nil
			fresh.ExportsRef = nil
			fresh.ResultRef = nil
			fresh.LogRef = nil
			fresh.Phase = nil
			fresh.ImageDigest = nil
			fresh.ExecutorRef = nil
			fresh.CancelRequested = false
			fresh.StartedAt = nil
			fresh.FinishedAt = nil
			fresh.CreatedAt = at
			fresh.UpdatedAt = at
			fresh.JobID = JobName(domain.RunnerKey{TaskID: taskID, AttemptNo: next, Seq: rr.Seq})
			fresh.JobNamespace = nil
			fresh.InputRef = fmt.Sprintf("attempts/%d/input/%d/", next, rr.Seq)
			if err := s.RunnerRuns.Insert(ctx, tx, &fresh); err != nil {
				return err
			}
		}
		return s.Audits.Insert(ctx, tx, AuditEntry{
			Actor: actorOf(actor), Role: defaultString(role, "operator"),
			Action: "retry", ObjectType: "task", ObjectID: taskID,
			Detail: map[string]any{"result": "ok", "attempt_no": attemptNo, "trigger": "retry"},
		})
	})
	if err != nil {
		return 0, err
	}
	if s.notifier != nil {
		s.notifier.Notify(taskID, 0)
	}
	return attemptNo, nil
}
