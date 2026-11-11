package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventRepo 读 state_events（append-only 事实流）。写入只由 Kernel 与其 effects 做，
// 因此本 repo 没有 Insert —— 事件是状态转移的副产物，不允许旁路写。
type EventRepo struct{ s *Store }

const eventCols = `id, task_id, attempt_no, runner_seq, seq, type, from_status, to_status,
	reason, payload, actor, created_at`

func scanEvent(row pgx.Row) (*Event, error) {
	var e Event
	if err := row.Scan(&e.ID, &e.TaskID, &e.AttemptNo, &e.RunnerSeq, &e.Seq, &e.Type,
		&e.FromStatus, &e.ToStatus, &e.Reason, &e.Payload, &e.Actor, &e.CreatedAt); err != nil {
		return nil, err
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	}
	return &e, nil
}

// ListByRun 返回 seq > sinceSeq 的事件（SSE 回放与 events.jsonl 共用）。
func (r *EventRepo) ListByTask(ctx context.Context, tx pgx.Tx, taskID string, sinceSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+eventCols+`
		  FROM state_events WHERE task_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
		taskID, sinceSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// AllByRun 返回该 Run 的全量事件（按 seq；events.jsonl 用）。
func (r *EventRepo) AllByTask(ctx context.Context, tx pgx.Tx, taskID string) ([]Event, error) {
	rows, err := r.s.db(tx).Query(ctx, `SELECT `+eventCols+`
		  FROM state_events WHERE task_id = $1 ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: all events %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// OldestSeqSince 返回 since 之后（含）最小的事件 seq；没有事件返回 0。
// SSE 判断"回放起点是否越窗"用（C-9.2）。
func (r *EventRepo) OldestSeqSince(ctx context.Context, tx pgx.Tx, taskID string, since time.Time) (int64, error) {
	var seq int64
	err := r.s.db(tx).QueryRow(ctx, `
		SELECT COALESCE(min(seq), 0) FROM state_events
		 WHERE task_id = $1 AND created_at >= $2`, taskID, since).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: oldest event seq %s: %w", taskID, err)
	}
	return seq, nil
}

// LatestSeq 返回该 Run 已落库的最大事件 seq（0 表示没有事件）。
func (r *EventRepo) LatestSeq(ctx context.Context, tx pgx.Tx, taskID string) (int64, error) {
	var seq int64
	err := r.s.db(tx).QueryRow(ctx,
		`SELECT COALESCE(max(seq), 0) FROM state_events WHERE task_id = $1`, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: latest event seq %s: %w", taskID, err)
	}
	return seq, nil
}
