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
// 这里只做 DB 写：外部动作（删 Job、卸载 Genet、写报告文件）由 orchestrator 在事务后执行。
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
		case domain.EffectScheduleRetry:
			if err := s.effectScheduleRetry(ctx, tx, tr); err != nil {
				return err
			}
		case domain.EffectWriteReportIntent:
			if err := s.effectWriteReportIntent(ctx, tx, tr, at); err != nil {
				return err
			}
		case domain.EffectBackfillEndpoint:
			if err := s.effectBackfillEndpoint(ctx, tx, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("store: effects: 未知副作用 %q: %w", kind, domain.ErrIllegalState)
		}
	}
	return nil
}

// effectReleaseLeases 删除端点租约（Attempt 终态时删它所属 Task 的租约）。
func (s *Store) effectReleaseLeases(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	taskID := tr.ID
	if tr.Entity == domain.EntityAttempt {
		var tid string
		err := tx.QueryRow(ctx, `SELECT task_id FROM attempts WHERE id = $1`, tr.ID).Scan(&tid)
		if err != nil {
			return fmt.Errorf("store: effects: release leases: lookup attempt %s: %w", tr.ID, err)
		}
		taskID = tid
	}
	if _, err := tx.Exec(ctx, `DELETE FROM endpoint_leases WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("store: effects: release leases %s: %w", taskID, err)
	}
	return nil
}

// effectSkipUnfinished 把 Run 里未开始的 Task（pending/queued）批量置 skipped，
// 每条 Task 转移各写一条事件（§2 规则：每次转移写一条事件）。
func (s *Store) effectSkipUnfinished(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	if tr.Entity != domain.EntityRun {
		return nil
	}
	reason := domain.SkipReasonForRunTerminal(domain.RunStatus(tr.To))
	rows, err := tx.Query(ctx,
		`SELECT id FROM tasks WHERE run_id = $1 AND status IN ('pending','queued') ORDER BY id`, tr.ID)
	if err != nil {
		return fmt.Errorf("store: effects: skip unfinished %s: %w", tr.ID, err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var cur domain.TaskStatus
		err := tx.QueryRow(ctx, `
			UPDATE tasks SET status = 'skipped', status_reason = $2, updated_at = $3
			 WHERE id = $1 AND status IN ('pending','queued')
			 RETURNING status`, id, reason, at).Scan(&cur)
		if err != nil {
			// 行已被并发推走：先落库者胜，跳过。
			continue
		}
		taskID := id
		seq, err := s.nextSeq(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reason": reason, "cause": "run_" + string(tr.To)})
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (run_id, task_id, seq, type, from_status, to_status, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, 'pending', 'skipped', $5, $6, 'system:kernel', $7)`,
			tr.ID, taskID, seq, domain.EventTaskStateChanged, reason, payload, at); err != nil {
			return fmt.Errorf("store: effects: skip unfinished event %s: %w", taskID, err)
		}
	}
	return nil
}

// effectMarkCancelRequested 给进行中的 Task 置 cancel_requested（不改变状态）。
func (s *Store) effectMarkCancelRequested(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	if tr.Entity != domain.EntityRun {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET cancel_requested = true
		 WHERE run_id = $1 AND status IN ('preparing','running')`, tr.ID); err != nil {
		return fmt.Errorf("store: effects: mark cancel requested %s: %w", tr.ID, err)
	}
	return nil
}

// effectCloseActive 强制收口进行中的 Task/Attempt（Run → canceled/failed）。
func (s *Store) effectCloseActive(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	if tr.Entity != domain.EntityRun {
		return nil
	}
	taskReason := domain.SkipReasonForRunTerminal(domain.RunStatus(tr.To))

	rows, err := tx.Query(ctx, `
		SELECT id FROM tasks WHERE run_id = $1 AND status IN ('preparing','running') ORDER BY id`, tr.ID)
	if err != nil {
		return fmt.Errorf("store: effects: close active %s: %w", tr.ID, err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var curTask domain.TaskStatus
		err := tx.QueryRow(ctx, `
			UPDATE tasks SET status = 'canceled', status_reason = $2, cancel_requested = true, updated_at = $3
			 WHERE id = $1 AND status IN ('preparing','running')
			 RETURNING status`, id, taskReason, at).Scan(&curTask)
		if err != nil {
			continue // 竞态：先落库者胜
		}
		taskID := id
		seq, err := s.nextSeq(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"reason": taskReason, "cause": "run_" + string(tr.To)})
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (run_id, task_id, seq, type, from_status, to_status, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, 'running', 'canceled', $5, $6, 'system:kernel', $7)`,
			tr.ID, taskID, seq, domain.EventTaskStateChanged, taskReason, payload, at); err != nil {
			return fmt.Errorf("store: effects: close active event %s: %w", taskID, err)
		}

		// 该 Task 的活跃 Attempt 一并收口（一个 Task 至多一个活跃 Attempt）。
		arows, err := tx.Query(ctx, `
			UPDATE attempts SET status = 'canceled', finished_at = $2
			 WHERE task_id = $1 AND status IN ('preparing','running')
			 RETURNING id`, taskID, at)
		if err != nil {
			return fmt.Errorf("store: effects: close attempts %s: %w", taskID, err)
		}
		attemptIDs, err := collectIDs(arows)
		if err != nil {
			return err
		}
		for _, aid := range attemptIDs {
			attemptID := aid
			aseq, err := s.nextSeq(ctx, tx, tr.ID)
			if err != nil {
				return err
			}
			apayload, _ := json.Marshal(map[string]any{"reason": taskReason})
			if _, err := tx.Exec(ctx, `
				INSERT INTO state_events
				  (run_id, task_id, attempt_id, seq, type, from_status, to_status, reason, payload, actor, created_at)
				VALUES ($1, $2, $3, $4, $5, 'running', 'canceled', $6, $7, 'system:kernel', $8)`,
				tr.ID, taskID, attemptID, aseq, domain.EventAttemptFinished, taskReason, apayload, at); err != nil {
				return fmt.Errorf("store: effects: close attempt event %s: %w", attemptID, err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM endpoint_leases WHERE task_id = $1`, taskID); err != nil {
			return fmt.Errorf("store: effects: release lease %s: %w", taskID, err)
		}
	}
	return nil
}

// effectScheduleRetry 按 retry_policy.backoff_seconds 设置 available_at。
func (s *Store) effectScheduleRetry(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET available_at = now() + make_interval(secs =>
		         COALESCE((retry_policy->>'backoff_seconds')::double precision, 0)),
		       waiting_reason = NULL,
		       updated_at     = now()
		 WHERE id = $1`, tr.ID); err != nil {
		return fmt.Errorf("store: effects: schedule retry %s: %w", tr.ID, err)
	}
	return nil
}

// effectWriteReportIntent 写一条 report.pending 事件，报告 writer 据此生成 Task 报告。
func (s *Store) effectWriteReportIntent(ctx context.Context, tx pgx.Tx, tr domain.Transition, at time.Time) error {
	var runID string
	if err := tx.QueryRow(ctx, `SELECT run_id FROM tasks WHERE id = $1`, tr.ID).Scan(&runID); err != nil {
		return fmt.Errorf("store: effects: report intent %s: %w", tr.ID, err)
	}
	seq, err := s.nextSeq(ctx, tx, runID)
	if err != nil {
		return err
	}
	taskID := tr.ID
	payload, _ := json.Marshal(map[string]any{"task_id": taskID})
	if _, err := tx.Exec(ctx, `
		INSERT INTO state_events
		  (run_id, task_id, seq, type, payload, actor, created_at)
		VALUES ($1, $2, $3, $4, $5, 'system:kernel', $6)`,
		runID, taskID, seq, domain.EventReportPendingIntent, payload, at); err != nil {
		return fmt.Errorf("store: effects: report intent event %s: %w", taskID, err)
	}
	return nil
}

// effectBackfillEndpoint 把 endpoint_key 与注入后的公共参数回填到该 Run 的全部 Task
// （C-5.3：回填与状态转移同事务，避免"已 testing 但 Task 还没 key"的窗口）。
//
// payload.print_argv 是平台注入的公共参数 token（domain.PublicParamArgv 的结果）；
// injected_argv = args_argv || 注入 token，与提交时 domain.BuildArgv 的口径一致。
func (s *Store) effectBackfillEndpoint(ctx context.Context, tx pgx.Tx, tr domain.Transition) error {
	if tr.Entity != domain.EntityRun {
		return nil
	}
	key := payloadString(tr.Payload, "endpoint_key")
	if key == "" {
		// 没有显式 key 时从 runs.endpoint->>'key' 取。
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(endpoint->>'key', '') FROM runs WHERE id = $1`, tr.ID).Scan(&key); err != nil {
			return fmt.Errorf("store: effects: backfill endpoint %s: %w", tr.ID, err)
		}
	}
	if key == "" {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET endpoint_key = $2, updated_at = now()
		  WHERE run_id = $1 AND endpoint_key IS NULL`, tr.ID, key); err != nil {
		return fmt.Errorf("store: effects: backfill endpoint %s: %w", tr.ID, err)
	}

	tokens := payloadStrings(tr.Payload, "print_argv")
	if len(tokens) == 0 {
		return nil
	}
	extra, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("store: effects: marshal print_argv %s: %w", tr.ID, err)
	}
	// 只有"还没注入过"的 Task 才拼（injected_argv 为空数组的 deploy 模式 Task）。
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		   SET injected_argv = args_argv || $2::jsonb, updated_at = now()
		 WHERE run_id = $1 AND jsonb_array_length(injected_argv) = 0`, tr.ID, extra); err != nil {
		return fmt.Errorf("store: effects: backfill injected_argv %s: %w", tr.ID, err)
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
