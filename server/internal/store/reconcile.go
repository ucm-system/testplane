package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 对账（C-3.5「重启恢复与对账」）需要的只读/幂等操作：
// 活跃执行记录集合、活跃 attempt、僵尸租约清理。

// ListActiveRunnerRuns 返回 status ∈ {preparing, running} 的执行记录（对账输入集合）。
func (s *Store) ListActiveRunnerRuns(ctx context.Context, tx pgx.Tx, limit int) ([]RunnerRun, error) {
	limit = normalizeLimit(limit)
	rows, err := s.db(tx).Query(ctx, `SELECT `+runnerRunScanCols()+`
		  FROM task_runners WHERE status IN ('preparing','running')
		 ORDER BY task_id, attempt_no, seq LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list active task_runners: %w", err)
	}
	return collectRunnerRuns(rows)
}

// ListActiveAttemptKeys 返回全部活跃 attempt（对账：重启恢复先读库，02 §4）。
func (s *Store) ListActiveAttemptKeys(ctx context.Context, tx pgx.Tx, limit int) ([]domain.RunnerKey, error) {
	limit = normalizeLimit(limit)
	rows, err := s.db(tx).Query(ctx, `
		SELECT task_id, attempt_no FROM task_attempts WHERE status = 'running'
		 ORDER BY task_id, attempt_no LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list active attempts: %w", err)
	}
	defer rows.Close()
	var out []domain.RunnerKey
	for rows.Next() {
		var k domain.RunnerKey
		if err := rows.Scan(&k.TaskID, &k.AttemptNo); err != nil {
			return nil, fmt.Errorf("store: scan active attempt: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListStaleLeases 返回"执行记录已终态（或已不存在）但租约还在"的三元组（僵尸租约清理）。
func (s *Store) ListStaleLeases(ctx context.Context, tx pgx.Tx, limit int) ([]domain.RunnerKey, error) {
	limit = normalizeLimit(limit)
	rows, err := s.db(tx).Query(ctx, `
		SELECT l.task_id, l.attempt_no, l.runner_seq
		  FROM endpoint_leases l
		  LEFT JOIN task_runners r
		         ON r.task_id = l.task_id AND r.attempt_no = l.attempt_no AND r.seq = l.runner_seq
		 WHERE r.seq IS NULL OR NOT (r.status = ANY($2))
		 ORDER BY l.acquired_at
		 LIMIT $1`, limit, domain.TerminalRunnerStatusStrings())
	if err != nil {
		return nil, fmt.Errorf("store: list stale leases: %w", err)
	}
	defer rows.Close()
	var out []domain.RunnerKey
	for rows.Next() {
		var k domain.RunnerKey
		if err := rows.Scan(&k.TaskID, &k.AttemptNo, &k.Seq); err != nil {
			return nil, fmt.Errorf("store: scan stale lease: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
