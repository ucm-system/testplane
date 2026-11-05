package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// ArtifactRepo 是 artifacts 表（派生索引，可重建；03 §3.2、C-7.5）。
type ArtifactRepo struct{ s *Store }

// Artifact 是 artifacts 表的一行；path 相对 <storage-root>。
type Artifact struct {
	RunID     string     `json:"run_id"`
	TaskID    *string    `json:"task_id,omitempty"`
	AttemptID *string    `json:"attempt_id,omitempty"`
	Path      string     `json:"path"`
	SizeBytes int64      `json:"size_bytes"`
	SHA256    *string    `json:"sha256,omitempty"`
	Kind      string     `json:"kind"`
	MTime     *time.Time `json:"mtime,omitempty"`
	IndexedAt time.Time  `json:"indexed_at"`
}

// UpsertBatch 在一个事务里替换某 Run 的产物索引（幂等重建）。
func (r *ArtifactRepo) UpsertBatch(ctx context.Context, tx pgx.Tx, runID string, items []Artifact) error {
	if _, err := r.s.db(tx).Exec(ctx, `DELETE FROM artifacts WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("store: artifacts: clear %s: %w", runID, err)
	}
	for _, a := range items {
		_, err := r.s.db(tx).Exec(ctx, `
			INSERT INTO artifacts (run_id, task_id, attempt_id, path, size_bytes, sha256, kind, mtime, indexed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
			ON CONFLICT (run_id, path) DO UPDATE
			   SET size_bytes = EXCLUDED.size_bytes,
			       sha256 = EXCLUDED.sha256,
			       kind = EXCLUDED.kind,
			       mtime = EXCLUDED.mtime,
			       indexed_at = now()`,
			runID, a.TaskID, a.AttemptID, a.Path, a.SizeBytes, a.SHA256, a.Kind, a.MTime)
		if err != nil {
			return fmt.Errorf("store: artifacts: upsert %s: %w", a.Path, err)
		}
	}
	return nil
}

// ListByRun 返回某 Run 的产物索引。
func (r *ArtifactRepo) ListByRun(ctx context.Context, tx pgx.Tx, runID string) ([]Artifact, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT run_id, task_id, attempt_id, path, size_bytes, sha256, kind, mtime, indexed_at
		  FROM artifacts WHERE run_id = $1 ORDER BY path`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts %s: %w", runID, err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.RunID, &a.TaskID, &a.AttemptID, &a.Path, &a.SizeBytes,
			&a.SHA256, &a.Kind, &a.MTime, &a.IndexedAt); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunBundle 是一次报告生成需要的全部库内事实。
type RunBundle struct {
	Run      *Run
	Tasks    []Task
	Attempts map[string][]Attempt // task_id -> attempts（按 idx 升序）
	Events   []Event
}

// ReportBundle 读一个 Run 的报告数据（报告模块的唯一数据来源；库是权威）。
func (s *Store) ReportBundle(ctx context.Context, runID string) (*RunBundle, error) {
	run, err := s.Runs.Get(ctx, nil, runID)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, 16)
	cursor := ""
	for {
		batch, next, err := s.Tasks.ListByRun(ctx, nil, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	attempts := make(map[string][]Attempt, len(tasks))
	for i := range tasks {
		list, err := s.Attempts.ListByTask(ctx, nil, tasks[i].ID)
		if err != nil {
			return nil, err
		}
		attempts[tasks[i].ID] = list
	}
	events, err := s.Events.AllByRun(ctx, nil, runID)
	if err != nil {
		return nil, err
	}
	return &RunBundle{Run: run, Tasks: tasks, Attempts: attempts, Events: events}, nil
}

// IsNotFound 报告错误是否是"对象不存在"。
func IsNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
