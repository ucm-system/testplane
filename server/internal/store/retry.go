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

// 重试（retry）系列的存储支持（07-api.md §5.1/§8、02 §P1 跨 Run 续跑）。
//
// 关键裁决（02-logical-view.md:430）：retry **不改状态机**，也不做"同 Run 内重跑"；
// 它创建一条**新 Run**（`runs.retry_of` 指向原 Run），可复用的 Task 直接落成
// `succeeded` 并把 `tasks.reused_from_task_id` 指向旧 Task。

// ErrRetryConflict 表示重试前置条件不满足（Run 非终态 / 已在执行 / 该 Task 不可重试）。
// API 层统一映射成 409 CONFLICT，detail.reason 说明具体原因。
var ErrRetryConflict = errors.New("store: retry conflict")

// RetryConflict 带一个稳定的原因码（写进 detail.reason，客户端可判）。
func retryConflict(reason, msg string) error {
	return fmt.Errorf("%w: %s: %s", ErrRetryConflict, reason, msg)
}

// RetrySource 是生成 retry-plan 所需的原始数据（不含文件系统判定）。
type RetrySource struct {
	Run   *Run
	Tasks []*Task
	// ActiveRetryID 非空表示已存在一条 retry_of=该 Run 且非终态的 Run（→ 409）。
	ActiveRetryID string
}

// LoadRetrySource 读 Run 与全部 Task，并检查是否已有进行中的 retry。
func (s *Store) LoadRetrySource(ctx context.Context, runID string) (*RetrySource, error) {
	out := &RetrySource{}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		run, err := s.Runs.Get(ctx, tx, runID)
		if err != nil {
			return err
		}
		out.Run = run
		// 分页拉全（seq 游标），直到没有下一页。
		var tasks []*Task
		cursor := ""
		for {
			batch, next, err := s.Tasks.ListByRun(ctx, tx, runID, cursor, 200)
			if err != nil {
				return err
			}
			for i := range batch {
				tasks = append(tasks, &batch[i])
			}
			if next == "" || len(batch) == 0 {
				break
			}
			cursor = next
		}
		out.Tasks = tasks
		var active string
		err = tx.QueryRow(ctx, `
			SELECT id FROM runs
			 WHERE retry_of = $1
			   AND status NOT IN ('succeeded','failed','canceled','service_unavailable')
			 ORDER BY submitted_at DESC
			 LIMIT 1`, runID).Scan(&active)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("store: retry: active retry lookup: %w", err)
		}
		out.ActiveRetryID = active
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RetryCreateParams 是创建 retry Run 的输入（全部由 API 层算好后传入）。
type RetryCreateParams struct {
	Source   *Run
	Tasks    []*Task // 新 Task 行（ID 已生成；复用项 status=succeeded 且 ReusedFromTaskID 非空）
	Manifest []byte  // 原样
	Resolved []byte  // 重新解析后的 resolved（precheck 判定已刷新）
	Actor    string
	Role     string
	At       time.Time
}

// CreateRetryRun 在一个事务里落库：新 Run（retry_of=源 Run）+ 全部 Task +
// run.submitted 事件 + audit(action=retry)。**不做**文件 IO 与产物索引（由调用方在
// 事务外做：硬链接/索引属于外部副作用）。
func (s *Store) CreateRetryRun(ctx context.Context, p RetryCreateParams) (*Run, error) {
	if p.Source == nil || p.Tasks == nil {
		return nil, retryConflict("invalid_plan", "缺少源 Run 或 Task 计划")
	}
	at := p.At
	if at.IsZero() {
		at = s.now()
	}
	run := &Run{
		ID:            p.Tasks[0].RunID,
		Name:          p.Source.Name,
		Kind:          p.Source.Kind,
		Status:        string(domain.RunSubmitted),
		Manifest:      p.Manifest,
		Resolved:      p.Resolved,
		CleanupPolicy: p.Source.CleanupPolicy,
		SubmittedBy:   p.Actor,
		SubmittedAt:   at,
		RetryOf:       &p.Source.ID,
	}
	for _, t := range p.Tasks {
		if t.RetryPolicy == nil {
			t.RetryPolicy = []byte("{}")
		}
	}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		// 事务内二次确认：源 Run 仍终态、且没有并发起来的 retry（双保险，防并发提交）。
		status, err := s.Runs.Lock(ctx, tx, p.Source.ID)
		if err != nil {
			return err
		}
		if !isTerminalRun(status) {
			return retryConflict("run_not_terminal", "Run 尚未结束，不能重试")
		}
		var active string
		err = tx.QueryRow(ctx, `
			SELECT id FROM runs
			 WHERE retry_of = $1
			   AND status NOT IN ('succeeded','failed','canceled','service_unavailable')
			 LIMIT 1`, p.Source.ID).Scan(&active)
		if err == nil {
			return retryConflict("retry_in_progress", "已有一次重试在执行（"+active+"）")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("store: retry: active retry lookup: %w", err)
		}

		if err := s.Runs.Insert(ctx, tx, run); err != nil {
			return err
		}
		for _, t := range p.Tasks {
			if err := s.Tasks.Insert(ctx, tx, t); err != nil {
				return err
			}
		}
		seq, err := s.nextSeq(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		reused, rerun := 0, 0
		for _, t := range p.Tasks {
			if t.ReusedFromTaskID != nil {
				reused++
			} else {
				rerun++
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"kind": run.Kind, "retry_of": p.Source.ID,
			"task_count": len(p.Tasks), "reused": reused, "rerun": rerun,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (run_id, seq, type, to_status, payload, actor, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			run.ID, seq, domain.EventRunSubmitted, string(domain.RunSubmitted),
			payload, actorOf(p.Actor), at); err != nil {
			return fmt.Errorf("store: retry: submitted event: %w", err)
		}
		return s.Audits.Insert(ctx, tx, AuditEntry{
			Actor: actorOf(p.Actor), Role: defaultString(p.Role, "operator"),
			Action: "retry", ObjectType: "run", ObjectID: run.ID,
			Detail: map[string]any{
				"retry_of": p.Source.ID, "reused": reused, "rerun": rerun,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// SetTaskReuseRef 给复用的 Task 回填 result_ref（指向新 Run 目录下的产物）。
func (s *Store) SetTaskReuseRef(ctx context.Context, taskID, ref string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET result_ref = $2, updated_at = $3 WHERE id = $1`,
			taskID, ref, s.now()); err != nil {
			return fmt.Errorf("store: retry: set reuse ref %s: %w", taskID, err)
		}
		return nil
	})
}

// RetryTask 为单个 Task 重开一次执行（T11：failed → queued）。
//
// 前置（07 §8）：Run 非 canceled、Task 终态且可重试（failed）、当前无进行中的 Attempt。
// 手动重试**不受** retry_policy.backoff 影响：available_at 立即清空，由调度器马上拾取。
func (s *Store) RetryTask(ctx context.Context, taskID, actor, role string) (int, error) {
	at := s.now()
	attemptNo := 0
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		runID, err := s.Tasks.RunIDOf(ctx, tx, taskID)
		if err != nil {
			return err
		}
		runStatus, err := s.Runs.Lock(ctx, tx, runID)
		if err != nil {
			return err
		}
		if runStatus == domain.RunCanceled {
			return retryConflict("run_canceled", "Run 已取消，不能重试其 Task")
		}
		var (
			status      domain.TaskStatus
			current     int
			cancelReq   bool
			activeCount int
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, current_attempt, cancel_requested
			  FROM tasks WHERE id = $1 FOR UPDATE`, taskID).
			Scan(&status, &current, &cancelReq); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("store: retry task %s: %w", taskID, domain.ErrNotFound)
			}
			return fmt.Errorf("store: retry task %s: lock: %w", taskID, err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM attempts
			 WHERE task_id = $1 AND status IN ('preparing','running')`, taskID).
			Scan(&activeCount); err != nil {
			return fmt.Errorf("store: retry task %s: attempts: %w", taskID, err)
		}
		if activeCount > 0 || status == domain.TaskPreparing || status == domain.TaskRunning {
			return retryConflict("task_running", "Task 正在执行，不能重试（等它结束或先取消）")
		}
		if status == domain.TaskCanceled || cancelReq {
			return retryConflict("task_canceled", "Task 已取消，不能重试")
		}
		if status != domain.TaskFailed {
			return retryConflict("task_not_failed",
				fmt.Sprintf("Task 当前状态是 %s，只有 failed 的 Task 支持重试", status))
		}
		attemptNo = current + 1
		if _, err := s.Apply(ctx, tx, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:    []domain.Status{domain.TaskFailed.Status()},
			To:      domain.TaskQueued.Status(),
			Command: "retry", Actor: actorOf(actor), At: at,
		}); err != nil {
			return err
		}
		// 手动重试立即排队：忽略 backoff，清空 available_at 与失败残留。
		if _, err := tx.Exec(ctx, `
			UPDATE tasks
			   SET available_at = NULL, error = NULL, status_reason = NULL, updated_at = $2
			 WHERE id = $1`, taskID, at); err != nil {
			return fmt.Errorf("store: retry task %s: clear backoff: %w", taskID, err)
		}
		return s.Audits.Insert(ctx, tx, AuditEntry{
			Actor: actorOf(actor), Role: defaultString(role, "operator"),
			Action: "retry", ObjectType: "task", ObjectID: taskID,
			Detail: map[string]any{"run_id": runID, "attempt_no": attemptNo},
		})
	})
	if err != nil {
		return 0, err
	}
	return attemptNo, nil
}

// isTerminalRun 报告 Run 状态是否终态（可重试的前提）。
func isTerminalRun(st domain.RunStatus) bool {
	switch st {
	case domain.RunSucceeded, domain.RunFailed, domain.RunCanceled,
		domain.RunServiceUnavailable, domain.RunCompletedWithErrors:
		return true
	default:
		return false
	}
}

// TerminalRunStatus 供 API 层判定（复用同一张表，避免两处判断漂移）。
func TerminalRunStatus(st domain.RunStatus) bool { return isTerminalRun(st) }
