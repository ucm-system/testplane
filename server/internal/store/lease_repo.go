package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// LeaseRepo 是 endpoint_leases 表的读写（端点互斥槽位，C-4.3；仅外接模式写入，D-69）。
// 租约的定位键是 runner 执行记录三元组 (task_id, attempt_no, runner_seq)。
//
// 判定规则：独占型在"任何持有者存在"时拒绝；共享型在"存在独占持有者"时拒绝。
// 用 endpoint_key 的 advisory 事务锁串行化同一端点的判定。acquired_at 事务结束自动释放。
type LeaseRepo struct{ s *Store }

// List 返回全部租约。
func (r *LeaseRepo) List(ctx context.Context, tx pgx.Tx) ([]Lease, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT task_id, attempt_no, runner_seq, endpoint_key, exclusive, acquired_at
		  FROM endpoint_leases ORDER BY endpoint_key, task_id, attempt_no, runner_seq`)
	if err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.TaskID, &l.AttemptNo, &l.RunnerSeq, &l.EndpointKey, &l.Exclusive, &l.AcquiredAt); err != nil {
			return nil, fmt.Errorf("store: scan lease: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// TryAcquire 尝试为 runner 执行记录取 endpointKey 上的租约；拿到返回 true。
// 必须在事务内调用（advisory 锁是 xact 级的）。
func (r *LeaseRepo) TryAcquire(ctx context.Context, tx pgx.Tx, k domain.RunnerKey, endpointKey string, exclusive bool) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("store: lease: 必须在事务内获取槽位")
	}
	// 同一端点的判定串行化；冲突只影响并发度，不影响正确性。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, endpointKey); err != nil {
		return false, fmt.Errorf("store: lease: advisory lock %q: %w", endpointKey, err)
	}

	var anyHeld, exclusiveHeld bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM endpoint_leases WHERE endpoint_key = $1),
		       EXISTS (SELECT 1 FROM endpoint_leases WHERE endpoint_key = $1 AND exclusive)`,
		endpointKey).Scan(&anyHeld, &exclusiveHeld)
	if err != nil {
		return false, fmt.Errorf("store: lease: inspect %q: %w", endpointKey, err)
	}
	if exclusive && anyHeld {
		return false, nil
	}
	if !exclusive && exclusiveHeld {
		return false, nil
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO endpoint_leases (task_id, attempt_no, runner_seq, endpoint_key, exclusive)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (task_id, attempt_no, runner_seq) DO UPDATE SET endpoint_key = EXCLUDED.endpoint_key,
		                                    exclusive = EXCLUDED.exclusive,
		                                    acquired_at = now()`,
		k.TaskID, k.AttemptNo, k.Seq, endpointKey, exclusive)
	if err != nil {
		// 唯一索引冲突（23505）= 另一个独占持有者先落库，按"未拿到"返回；
		// 其它错误必须上报，否则调用方会以为只是"槽位被占"而静默失败。
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return false, fmt.Errorf("store: lease: insert %s: %w", endpointKey, err)
	}
	return true, nil
}

// Delete 释放某执行记录的租约（与状态推进同事务，避免泄漏）。
func (r *LeaseRepo) Delete(ctx context.Context, tx pgx.Tx, k domain.RunnerKey) error {
	if _, err := r.s.db(tx).Exec(ctx,
		`DELETE FROM endpoint_leases WHERE task_id = $1 AND attempt_no = $2 AND runner_seq = $3`,
		k.TaskID, k.AttemptNo, k.Seq); err != nil {
		return fmt.Errorf("store: lease: delete %s: %w", k.ID(), err)
	}
	return nil
}

// DeleteByTask 释放某 Task 的全部租约（Task 收口用）。
func (r *LeaseRepo) DeleteByTask(ctx context.Context, tx pgx.Tx, taskID string) error {
	if _, err := r.s.db(tx).Exec(ctx, `DELETE FROM endpoint_leases WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("store: lease: delete by task %s: %w", taskID, err)
	}
	return nil
}

// Holders 返回某 endpoint_key 上的持有者。
func (r *LeaseRepo) Holders(ctx context.Context, tx pgx.Tx, endpointKey string) ([]Lease, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT task_id, attempt_no, runner_seq, endpoint_key, exclusive, acquired_at FROM endpoint_leases
		  WHERE endpoint_key = $1 ORDER BY task_id, attempt_no, runner_seq`, endpointKey)
	if err != nil {
		return nil, fmt.Errorf("store: lease: holders %q: %w", endpointKey, err)
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.TaskID, &l.AttemptNo, &l.RunnerSeq, &l.EndpointKey, &l.Exclusive, &l.AcquiredAt); err != nil {
			return nil, fmt.Errorf("store: scan lease: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
