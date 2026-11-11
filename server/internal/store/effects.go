package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// Effects 是转移的同事务副作用（02 §3.5、domain.Effects 给出每种转移要执行哪些）。
// 这里只做 DB 写：外部动作（删 Job、写报告文件）由 orchestrator 在事务后执行。
func (s *Store) applyEffects(ctx context.Context, tx pgx.Tx, tr domain.Transition,
	from, to domain.Status, at time.Time) error {

	for _, kind := range domain.Effects(tr.Entity, from, to) {
		switch kind {
		case domain.EffectReleaseLeases:
			if err := s.effectReleaseLeases(ctx, tx, tr); err != nil {
				return err
			}
		case domain.EffectSkipUnfinished:
			if err := s.effectSkipUnfinished(ctx, tx, tr, at); err != nil {
				return err
			}
		case domain.EffectMarkCancelRequested:
			if err := s.effectMarkCancelRequested(ctx, tx, tr); err != nil {
				return err
			}
		case domain.EffectCloseActive:
			if err := s.effectCloseActive(ctx, tx, tr, at); err != nil {
				return err
			}
		case domain.EffectWriteReportIntent:
			if err := s.effectWriteReportIntent(ctx, tx, tr, at); err != nil {
				return err
			}
		default:
			return fmt.Errorf("store: effects: 未知副作用 %q: %w", kind, domain.ErrIllegalState)
		}
	}
	return nil
}

// effectReleaseLeases 删除该 runner 执行记录的端点租约（仅外接模式有租约）。
func (s *Store) effectReleaseLeases(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	if tr.Entity != domain.EntityRunnerRun {
		return nil
	}
	k, ok := domain.ParseRunnerKey(tr.ID)
	if !ok {
		return fmt.Errorf("store: effects: release leases: 非法 key %q: %w", tr.ID, domain.ErrIllegalState)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM endpoint_leases WHERE task_id = $1 AND attempt_no = $2 AND runner_seq = $3`,
		k.TaskID, k.AttemptNo, k.Seq); err != nil {
		return fmt.Errorf("store: effects: release leases %s: %w", k.ID(), err)
	}
	return nil
}

// effectSkipUnfinished 把 Task 当前 attempt 里未开始的 runner（pending/queued）批量置
// skipped，每条转移各写一条事件。**传播到 clean 为止**：role=clean 的 runner 永不被
// SKIPPED（finally 语义，§7.3/§7.6/C-5.7）。
func (s *Store) effectSkipUnfinished(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	if tr.Entity != domain.EntityTask {
		return nil
	}
	reason := domain.SkipReasonForTaskTerminal(domain.TaskStatus(tr.To))
	rows, err := tx.Query(ctx, `
		SELECT attempt_no, seq FROM task_runners
		 WHERE task_id = $1 AND attempt_no = (SELECT current_attempt FROM tasks WHERE id = $1)
		   AND status IN ('pending','queued') AND role <> 'clean'
		 ORDER BY attempt_no, seq`, tr.ID)
	if err != nil {
		return fmt.Errorf("store: effects: skip unfinished %s: %w", tr.ID, err)
	}
	type key struct{ no, seq int }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.no, &k.seq); err != nil {
			rows.Close()
			return fmt.Errorf("store: effects: scan skip unfinished: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: effects: skip unfinished %s: %w", tr.ID, err)
	}
	for _, k := range keys {
		var cur domain.RunnerStatus
		err := tx.QueryRow(ctx, `
			UPDATE task_runners
			   SET status = 'skipped', status_reason = $4, waiting_reason = NULL, updated_at = $5
			 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3 AND status IN ('pending','queued')
			 RETURNING status`, tr.ID, k.no, k.seq, reason, at).Scan(&cur)
		if err != nil {
			// 行已被并发推走：先落库者胜，跳过。
			continue
		}
		seq, err := s.nextSeq(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		no, sq := k.no, k.seq
		payload, _ := json.Marshal(map[string]any{"reason": reason, "cause": "task_" + string(tr.To)})
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (task_id, attempt_no, runner_seq, seq, type, from_status, to_status, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'skipped', $7, $8, 'system:kernel', $9)`,
			tr.ID, no, sq, seq, domain.EventRunnerStateChanged, string(cur), reason, payload, at); err != nil {
			return fmt.Errorf("store: effects: skip unfinished event %s@%d/%d: %w", tr.ID, k.no, k.seq, err)
		}
	}
	return nil
}

// effectMarkCancelRequested 给进行中的 runner 置 cancel_requested（不改变状态）。
func (s *Store) effectMarkCancelRequested(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	if tr.Entity != domain.EntityTask {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE task_runners SET cancel_requested = true, updated_at = now()
		 WHERE task_id = $1 AND attempt_no = (SELECT current_attempt FROM tasks WHERE id = $1)
		   AND status IN ('preparing','running')`, tr.ID); err != nil {
		return fmt.Errorf("store: effects: mark cancel requested %s: %w", tr.ID, err)
	}
	return nil
}

// effectCloseActive 强制收口进行中的 runner（Task → canceled / failed 等终态前）。
// 保证"Task 终态时所有 runner 终态"（02 §5）。
func (s *Store) effectCloseActive(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	if tr.Entity != domain.EntityTask {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT attempt_no, seq FROM task_runners
		 WHERE task_id = $1 AND attempt_no = (SELECT current_attempt FROM tasks WHERE id = $1)
		   AND status IN ('preparing','running')
		 ORDER BY attempt_no, seq`, tr.ID)
	if err != nil {
		return fmt.Errorf("store: effects: close active %s: %w", tr.ID, err)
	}
	type key struct{ no, seq int }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.no, &k.seq); err != nil {
			rows.Close()
			return fmt.Errorf("store: effects: scan close active: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: effects: close active %s: %w", tr.ID, err)
	}
	for _, k := range keys {
		var cur domain.RunnerStatus
		err := tx.QueryRow(ctx, `
			UPDATE task_runners
			   SET status = 'canceled', status_reason = NULL, cancel_requested = true,
			       waiting_reason = NULL, finished_at = $4, updated_at = $4
			 WHERE task_id = $1 AND attempt_no = $2 AND seq = $3 AND status IN ('preparing','running')
			 RETURNING status`, tr.ID, k.no, k.seq, at).Scan(&cur)
		if err != nil {
			continue // 竞态：先落库者胜
		}
		seq, err := s.nextSeq(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		no, sq := k.no, k.seq
		payload, _ := json.Marshal(map[string]any{"cause": "task_" + string(tr.To)})
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (task_id, attempt_no, runner_seq, seq, type, from_status, to_status, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'canceled', NULL, $7, 'system:kernel', $8)`,
			tr.ID, no, sq, seq, domain.EventRunnerStateChanged, string(cur), payload, at); err != nil {
			return fmt.Errorf("store: effects: close active event %s@%d/%d: %w", tr.ID, k.no, k.seq, err)
		}
		// 租约一并释放（与状态推进同事务，避免泄漏）。
		if _, err := tx.Exec(ctx,
			`DELETE FROM endpoint_leases WHERE task_id = $1 AND attempt_no = $2 AND runner_seq = $3`,
			tr.ID, k.no, k.seq); err != nil {
			return fmt.Errorf("store: effects: release lease %s@%d/%d: %w", tr.ID, k.no, k.seq, err)
		}
	}
	return nil
}

// effectWriteReportIntent 写一条 report.written 前置事件，报告 writer 据此生成 Task 报告。
func (s *Store) effectWriteReportIntent(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	if tr.Entity != domain.EntityTask {
		return nil
	}
	seq, err := s.nextSeq(ctx, tx, tr.ID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"task_id": tr.ID})
	if _, err := tx.Exec(ctx, `
		INSERT INTO state_events
		  (task_id, seq, type, payload, actor, created_at)
		VALUES ($1, $2, $3, $4, 'system:kernel', $5)`,
		tr.ID, seq, domain.EventReportWritten, payload, at); err != nil {
		return fmt.Errorf("store: effects: report intent event %s: %w", tr.ID, err)
	}
	return nil
}

func collectIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
