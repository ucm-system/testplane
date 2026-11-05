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

// RecordDeployState 把 Genet 的轮询结果写进 runs.deploy 并追加一条 deploy.state_changed 事件（C-5.3）。
// state 取值：queued / accepted / deploying / ready / blocked。
//
// queuePosition 只为兼容调用方保留：Genet 只给队列水位计数、**没有位次**，
// 所以 `queue_position` 键保留但值恒为 null（D-46 已把契约改成"恒 null、保留字段仅兼容"）。
// 删键会让前端与 openapi 不一致，不要删。
func (s *Store) RecordDeployState(ctx context.Context, runID, state, reason string, queuePosition int) error {
	_ = queuePosition // 恒不落库，见上方 D-46
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		deploy := map[string]any{"state": state, "queue_position": nil}
		if reason != "" {
			deploy["queue_reason"] = reason
		}
		if state == "queued" || state == "accepted" {
			deploy["queued_at"] = time.Now().UTC().Format(time.RFC3339)
		}
		raw, err := json.Marshal(deploy)
		if err != nil {
			return fmt.Errorf("store: record deploy: marshal: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET deploy = $2 WHERE id = $1`, runID, raw); err != nil {
			return fmt.Errorf("store: record deploy %s: %w", runID, err)
		}
		seq, err := s.nextSeq(ctx, tx, runID)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(deploy)
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (run_id, seq, type, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, 'system:orchestrator', $6)`,
			runID, seq, domain.EventDeployStateChanged, nullableText(reason), payload,
			time.Now().UTC()); err != nil {
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
		skipped, activeAttempts, err = s.abortOthers(ctx, tx, runID, failedTaskID, reason, time.Now().UTC())
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
			At: time.Now().UTC(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
			To:      domain.TaskCanceled.Status(),
			Command: "cancel", Reason: domain.SkipCanceled, Actor: "system:executor",
			At: time.Now().UTC(),
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			return err
		}
		return nil
	})
}
