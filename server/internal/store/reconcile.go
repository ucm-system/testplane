package store

import (
	"context"
	"fmt"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 对账（C-3.5「重启恢复与对账」）需要的三组只读/幂等操作：
// 活跃 Task 集合、Task 当前活跃 Attempt、僵尸租约清理。

// ListActiveTasks 返回 status ∈ {preparing, running} 的 Task（对账输入集合）。
func (s *Store) ListActiveTasks(ctx context.Context, limit int) ([]Task, error) {
	limit = normalizeLimit(limit)
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+`
		  FROM tasks WHERE status IN ('preparing','running')
		 ORDER BY run_id, seq LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list active tasks: %w", err)
	}
	return collectTasks(rows)
}

// ActiveAttemptOfTask 返回该 Task 当前进行中的 Attempt 与其 K8s Job 名。
// 没有活跃 Attempt 时返回 ErrNotFound。
func (s *Store) ActiveAttemptOfTask(ctx context.Context, taskID string) (*Attempt, string, error) {
	var a Attempt
	var jobName string
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.task_id, a.idx, a.status, a.job_id,
		       COALESCE(a.executor_ref->>'job', '')
		  FROM attempts a
		 WHERE a.task_id = $1 AND a.status IN ('preparing','running')
		 ORDER BY a.idx DESC LIMIT 1`, taskID).
		Scan(&a.ID, &a.TaskID, &a.Idx, &a.Status, &a.JobID, &jobName)
	if err != nil {
		return nil, "", fmt.Errorf("store: active attempt of task %s: %w", taskID, domain.ErrNotFound)
	}
	return &a, jobName, nil
}

// ListStaleLeases 返回"Task 已终态（或已不存在）但租约还在"的 task_id（僵尸租约清理）。
func (s *Store) ListStaleLeases(ctx context.Context, limit int) ([]string, error) {
	limit = normalizeLimit(limit)
	rows, err := s.pool.Query(ctx, `
		SELECT l.task_id
		  FROM endpoint_leases l
		  LEFT JOIN tasks t ON t.id = l.task_id
		 WHERE t.id IS NULL
		    OR t.status = ANY($2)
		 ORDER BY l.acquired_at
		 LIMIT $1`, limit, domain.TerminalTaskStatusStrings())
	if err != nil {
		return nil, fmt.Errorf("store: list stale leases: %w", err)
	}
	return collectIDs(rows)
}
