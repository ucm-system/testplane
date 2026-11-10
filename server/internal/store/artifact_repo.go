package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ArtifactRepo 是 artifacts 表（派生索引，可重建；03 §3.2、C-7.5）。
// 产物路径相对 <storage-root>/<task_id>/（如 attempts/1/runners/3/result.json）。
type ArtifactRepo struct{ s *Store }

// Artifact 是 artifacts 表的一行；path 相对 <storage-root>/<task_id>/。
type Artifact struct {
	TaskID    string     `json:"task_id"`
	AttemptNo *int       `json:"attempt_no,omitempty"`
	RunnerSeq *int       `json:"runner_seq,omitempty"`
	Path      string     `json:"path"`
	SizeBytes int64      `json:"size_bytes"`
	SHA256    *string    `json:"sha256,omitempty"`
	Kind      string     `json:"kind"` // result|raw|log|report|export|other
	MTime     *time.Time `json:"mtime,omitempty"`
	IndexedAt time.Time  `json:"indexed_at"`
}

// UpsertBatch 在一个事务里替换某 Task 的产物索引（幂等重建）。
func (r *ArtifactRepo) UpsertBatch(ctx context.Context, tx pgx.Tx, taskID string, items []Artifact) error {
	if _, err := r.s.db(tx).Exec(ctx, `DELETE FROM artifacts WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("store: artifacts: clear %s: %w", taskID, err)
	}
	for _, a := range items {
		_, err := r.s.db(tx).Exec(ctx, `
			INSERT INTO artifacts (task_id, attempt_no, runner_seq, path, size_bytes, sha256, kind, mtime, indexed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
			ON CONFLICT (task_id, path) DO UPDATE
			   SET size_bytes = EXCLUDED.size_bytes,
			       sha256 = EXCLUDED.sha256,
			       kind = EXCLUDED.kind,
			       mtime = EXCLUDED.mtime,
			       indexed_at = now()`,
			a.TaskID, intPtrToAny(a.AttemptNo), intPtrToAny(a.RunnerSeq),
			a.Path, a.SizeBytes, a.SHA256, a.Kind, a.MTime)
		if err != nil {
			return fmt.Errorf("store: artifacts: upsert %s: %w", a.Path, err)
		}
	}
	return nil
}

// ListByTask 返回某 Task 的产物索引。
func (r *ArtifactRepo) ListByTask(ctx context.Context, tx pgx.Tx, taskID string) ([]Artifact, error) {
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT task_id, attempt_no, runner_seq, path, size_bytes, sha256, kind, mtime, indexed_at
		  FROM artifacts WHERE task_id = $1 ORDER BY path`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.TaskID, &a.AttemptNo, &a.RunnerSeq, &a.Path, &a.SizeBytes,
			&a.SHA256, &a.Kind, &a.MTime, &a.IndexedAt); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListByRunner 返回某个执行记录的产物（07 §5.2）。attemptNo/seq 为 0 时只按 task 过滤。
func (r *ArtifactRepo) ListByRunner(ctx context.Context, tx pgx.Tx, taskID string, attemptNo, seq int) ([]Artifact, error) {
	args := []any{taskID}
	where := "task_id = $1"
	if attemptNo > 0 {
		args = append(args, attemptNo)
		where += fmt.Sprintf(" AND attempt_no = $%d", len(args))
	}
	if seq > 0 {
		args = append(args, seq)
		where += fmt.Sprintf(" AND runner_seq = $%d", len(args))
	}
	rows, err := r.s.db(tx).Query(ctx, `
		SELECT task_id, attempt_no, runner_seq, path, size_bytes, sha256, kind, mtime, indexed_at
		  FROM artifacts WHERE `+where+` ORDER BY path`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts %s#%d/%d: %w", taskID, attemptNo, seq, err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.TaskID, &a.AttemptNo, &a.RunnerSeq, &a.Path, &a.SizeBytes,
			&a.SHA256, &a.Kind, &a.MTime, &a.IndexedAt); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteByTask 清掉某 Task 的产物索引（人工清理 / 保留期清理；磁盘删除在事务外）。
func (r *ArtifactRepo) DeleteByTask(ctx context.Context, tx pgx.Tx, taskID string) error {
	if _, err := r.s.db(tx).Exec(ctx, `DELETE FROM artifacts WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("store: artifacts: delete by task %s: %w", taskID, err)
	}
	return nil
}

// TaskQuotaBytes 返回该 Task 当前 attempt 已索引产物的总字节数
// （ARTIFACT_QUOTA_EXCEEDED 判定的输入，§12.7）。
func (r *ArtifactRepo) TaskQuotaBytes(ctx context.Context, tx pgx.Tx, taskID string) (int64, error) {
	var total int64
	err := r.s.db(tx).QueryRow(ctx, `
		SELECT COALESCE(sum(size_bytes), 0) FROM artifacts WHERE task_id = $1`, taskID).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: artifact quota %s: %w", taskID, err)
	}
	return total, nil
}
