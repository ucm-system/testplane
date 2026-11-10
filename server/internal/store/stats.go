package store

import (
	"context"
	"fmt"
)

// MetricsSnapshot 是 /metrics 需要的库内计数（一次采样三条聚合查询）。
type MetricsSnapshot struct {
	TasksByStatus   map[string]int
	RunnersActive   int
	AttemptsActive  int
	Leases          int
	WaitingByReason map[string]int
}

// MetricsSnapshot 采样当前计数。只读、不走事务：/metrics 是运维端点，
// 单条查询失败只影响对应指标（由调用方决定是否降级）。
func (s *Store) MetricsSnapshot(ctx context.Context) (*MetricsSnapshot, error) {
	out := &MetricsSnapshot{
		TasksByStatus:   map[string]int{},
		WaitingByReason: map[string]int{},
	}
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: metrics tasks: %w", err)
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan metrics tasks: %w", err)
		}
		out.TasksByStatus[st] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: metrics tasks: %w", err)
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM task_runners WHERE status IN ('preparing','running')`).
		Scan(&out.RunnersActive); err != nil {
		return nil, fmt.Errorf("store: metrics active runners: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM task_attempts WHERE status = 'running'`).
		Scan(&out.AttemptsActive); err != nil {
		return nil, fmt.Errorf("store: metrics active attempts: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM endpoint_leases`).
		Scan(&out.Leases); err != nil {
		return nil, fmt.Errorf("store: metrics leases: %w", err)
	}
	rows, err = s.pool.Query(ctx, `
		SELECT COALESCE(waiting_reason, 'unknown'), count(*)
		  FROM task_runners WHERE status = 'queued' GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("store: metrics waiting: %w", err)
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan metrics waiting: %w", err)
		}
		out.WaitingByReason[reason] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: metrics waiting: %w", err)
	}
	return out, nil
}
