package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
)

// RecordDeployState 把一次 deploy 观测**增量合并**进 runs.deploy，并追加一条
// deploy.state_changed 事件（C-5.3）。
//
// 键名与 03-data-view §4.4 一致；调用方只填变化的键，合并由 jsonb `||` 完成
// （见 RunRepo.SetDeploy：绝不能整段覆盖，否则 release_name 会被冲掉）。
//
// `queue_position` 恒为 null（D-46：Genet 只给水位计数、没有位次），
// 保留该键只为兼容前端与 openapi；不要删。
func (s *Store) RecordDeployState(ctx context.Context, runID string, snap domain.DeploySnapshot) error {
	// 恒显式写 null：QueuePosition 为 nil 时 MarshalJSON 输出 "queue_position": null。
	snap.QueuePosition = nil
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("store: record deploy: marshal: %w", err)
		}
		if err := s.Runs.SetDeploy(ctx, tx, runID, raw); err != nil {
			return fmt.Errorf("store: record deploy %s: %w", runID, err)
		}
		seq, err := s.nextSeq(ctx, tx, runID)
		if err != nil {
			return err
		}
		// 事件 payload 用本次增量快照（完整列请看 runs.deploy）。
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (run_id, seq, type, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, 'system:orchestrator', $6)`,
			runID, seq, domain.EventDeployStateChanged, nullableText(snap.QueueReason), raw,
			s.now()); err != nil {
			return fmt.Errorf("store: record deploy event %s: %w", runID, err)
		}
		return nil
	})
}

// AbortOtherTasks 是 abort_run 的内核出口：同事务锁 Run，收口其余未完成 Task（C-5.5）。
// 返回被跳过的 Task id 与仍需异步删 Job 的 Attempt id。
func (s *Store) AbortOtherTasks(ctx context.Context, runID, failedTaskID, reason string) ([]string, []string, error) {
	var skipped, activeAttempts []string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		var err error
		skipped, activeAttempts, err = s.abortOthers(ctx, tx, runID, failedTaskID, reason, s.now())
		return err
	})
	return skipped, activeAttempts, err
}

// MarkAttemptCanceled 把被取消的 Attempt 及其 Task 落成 canceled 终态（CAS 收口）。
func (s *Store) MarkAttemptCanceled(ctx context.Context, attemptID string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		taskID, runID, err := s.Attempts.Owner(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityAttempt, ID: attemptID,
			From:    []domain.Status{domain.AttemptPreparing.Status(), domain.AttemptRunning.Status()},
			To:      domain.AttemptCanceled.Status(),
			Command: "cancel", Reason: domain.SkipCanceled, Actor: "system:executor",
			At: s.now(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
			To:      domain.TaskCanceled.Status(),
			Command: "cancel", Reason: domain.SkipCanceled, Actor: "system:executor",
			At: s.now(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		return nil
	})
}

// ListDeployCleanupPending 返回"已终态但 deploy 还没卸载干净"的 Run，供编排层对账补偿（§9.7）。
//
// 判定条件：
//   - runs.status 是终态（succeeded / failed / completed_with_errors / service_unavailable）；
//   - runs.deploy 存在（deploy 形态）；
//   - cleanup_policy 允许（always，或 on_failure 且 Run 失败）；
//   - deploy->'uninstall'->>'state' 不是 verified / skipped（缺失也算待收口）。
func (s *Store) ListDeployCleanupPending(ctx context.Context, limit int) ([]Run, error) {
	limit = normalizeLimit(limit)
	terminal := []string{
		string(domain.RunSucceeded), string(domain.RunFailed),
		string(domain.RunCompletedWithErrors), string(domain.RunServiceUnavailable),
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+runCols+`
		  FROM runs
		 WHERE status = ANY($1)
		   AND deploy IS NOT NULL
		   AND COALESCE(deploy->'uninstall'->>'state', '') NOT IN ('verified', 'skipped')
		   AND (cleanup_policy = 'always'
		        OR (cleanup_policy = 'on_failure' AND status = ANY($2)))
		 ORDER BY finished_at NULLS FIRST
		 LIMIT $3`,
		terminal, []string{string(domain.RunFailed), string(domain.RunServiceUnavailable)}, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list deploy cleanup pending: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, serr := scanRun(rows)
		if serr != nil {
			return nil, fmt.Errorf("store: scan run: %w", serr)
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

// AppendRunEvent 追加一条 state_events 记录。**只**给 cleanup.failed 用（不开放通用事件面）：
// 载荷 {release_name, cleanup_errors[], attempt, verified_at}（03-data-view §4.6）。
func (s *Store) AppendRunEvent(ctx context.Context, runID, typ string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: append run event: marshal: %w", err)
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		seq, err := s.nextSeq(ctx, tx, runID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (run_id, seq, type, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, NULL, $4, 'system:orchestrator', $5)`,
			runID, seq, typ, raw, s.now()); err != nil {
			return fmt.Errorf("store: append run event %s: %w", runID, err)
		}
		return nil
	})
}

// AttemptStatesByJobNames 批量把 Job 名映射成 Attempt 状态（GC 用，票 #20-5：一次查库，不做 N+1）。
//
// SQL 侧用 `executor_ref->>'job' = ANY($1)` 一次拿全，再用 tasks.status 判终态
// （不变量：Task 终态 ⇒ 其 Attempt 已终态）。
func (s *Store) AttemptStatesByJobNames(ctx context.Context, jobNames []string) (map[string]executor.AttemptState, error) {
	out := make(map[string]executor.AttemptState, len(jobNames))
	if len(jobNames) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.executor_ref->>'job' AS job, t.status
		  FROM attempts a
		  JOIN tasks t ON t.id = a.task_id
		 WHERE a.executor_ref->>'job' = ANY($1)`, jobNames)
	if err != nil {
		return nil, fmt.Errorf("store: attempt states by job: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var job, taskStatus string
		if err := rows.Scan(&job, &taskStatus); err != nil {
			return nil, fmt.Errorf("store: scan attempt state: %w", err)
		}
		out[job] = executor.AttemptState{Found: true, Terminal: domain.TaskStatus(taskStatus).IsTerminal()}
	}
	return out, rows.Err()
}
