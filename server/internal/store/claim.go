package store

import (
	"context"
	"errors"
	"fmt"


	"github.com/ucm-system/testplane/server/internal/domain"
)

// ClaimForExecution 是调度器唯一的"取活"原子入口（control-plane §C-4）。
//
// 一个事务里按固定锁顺序完成：
//
//	锁 Run → 端点 advisory 锁 → 全局并发 advisory 锁 → 计数
//	→ CAS Task queued→preparing → 插入 endpoint_leases → 建 Attempt
//
// 候选查询（不加行锁）留在 orchestrator：它只负责"选出候选 → ClaimForExecution
// → 抢不到就下一轮"。抢不到是**正常结果**，不是错误：返回 claimed=false + reason
// （reason 取 tasks.waiting_reason 的六值之一，由调用方写回 Task）。
type ClaimRequest struct {
	TaskID      string
	EndpointKey string
	// Exclusive 来自 tester.yaml 的 concurrency.endpoint_exclusive（K-12）。
	Exclusive bool
	// GlobalLimit 是 TESTPLANE_MAX_GLOBAL_CONCURRENCY；<=0 表示不限制。
	GlobalLimit int
	// AttemptIdx 是本次 Attempt 的序号；<=0 时用 current_attempt+1。
	AttemptIdx int
	// JobID 可预分配（与 JobName 对应）；空则新建。
	JobID string
	// JobNamespace 与 ExecutorRef 落库到 attempts。
	JobNamespace string
	ExecutorRef  []byte
}

// ClaimResult 是抢到之后的产物。
type ClaimResult struct {
	AttemptID string
	// JobName 是预分配的 K8s Job 名：tp-<task_id>-a<n>（execution §2.3）。
	JobName string
	Attempt *Attempt
	// Seq 是本次 Task 转移的事件序号。
	Seq int64
}

// globalConcurrencyLockKey 是全局并发 advisory 锁的哈希输入（锁名只允许一处定义，C-4.4）。
const globalConcurrencyLockName = "testplane:global-concurrency"

// ClaimForExecution 见类型注释。第二个返回值表示是否抢到，第三个是抢不到的原因
// （取 domain.Wait* 之一，抢到时为空串）。
func (s *Store) ClaimForExecution(ctx context.Context, req ClaimRequest) (ClaimResult, bool, string, error) {
	if req.TaskID == "" {
		return ClaimResult{}, false, "", fmt.Errorf("store: claim: 空 task id")
	}

	tx, err := s.Begin(ctx)
	if err != nil {
		return ClaimResult{}, false, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	// 1. 统一锁顺序第一步：锁 Run（防与取消/abort 反向加锁死锁，C-4.2）。
	runID, err := s.Tasks.RunIDOf(ctx, tx, req.TaskID)
	if err != nil {
		return ClaimResult{}, false, "", err
	}
	if _, err := s.Runs.Lock(ctx, tx, runID); err != nil {
		return ClaimResult{}, false, "", err
	}
	if runID == "" {
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}

	// 2. 端点槽位
	if req.EndpointKey == "" {
		// queued 的 Task 必须有 endpoint_key（C-4.5 守卫）；没有就是不可调度。
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, req.EndpointKey); err != nil {
		return ClaimResult{}, false, "", fmt.Errorf("store: claim: 端点 advisory 锁: %w", err)
	}
	holders, err := s.Leases.Holders(ctx, tx, req.EndpointKey)
	if err != nil {
		return ClaimResult{}, false, "", err
	}
	exclusiveHeld := false
	for _, l := range holders {
		if l.Exclusive && l.TaskID != req.TaskID {
			exclusiveHeld = true
		}
	}
	if req.Exclusive && len(holders) > 0 && !(len(holders) == 1 && holders[0].TaskID == req.TaskID) {
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}
	if !req.Exclusive && exclusiveHeld {
		return ClaimResult{}, false, domain.WaitSlotWaiting, nil
	}

	// 3. 全局并发 permit（排在端点锁之后）
	if req.GlobalLimit > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, globalConcurrencyLockName); err != nil {
			return ClaimResult{}, false, "", fmt.Errorf("store: claim: 全局 advisory 锁: %w", err)
		}
		active, err := s.Attempts.CountActive(ctx, tx)
		if err != nil {
			return ClaimResult{}, false, "", err
		}
		if active >= req.GlobalLimit {
			return ClaimResult{}, false, domain.WaitGlobalLimit, nil
		}
	}

	// 4. CAS Task queued→preparing
	applied, err := s.Apply(ctx, tx, domain.Transition{
		Entity: domain.EntityTask, ID: req.TaskID,
		From:    []domain.Status{domain.TaskQueued.Status()},
		To:      domain.TaskPreparing.Status(),
		Command: "prepare", Actor: "system:scheduler",
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		case errors.Is(err, domain.ErrPrecondition):
			return ClaimResult{}, false, domain.WaitSlotWaiting, nil
		}
		return ClaimResult{}, false, "", err
	}

	// 5. 端点租约（与状态推进同事务；Attempt 终态时释放）
	if _, err := s.Leases.TryAcquire(ctx, tx, req.TaskID, req.EndpointKey, req.Exclusive); err != nil {
		return ClaimResult{}, false, "", err
	}

	// 6. 建 Attempt（idx = current_attempt+1）
	attempt, err := s.Attempts.Create(ctx, tx, CreateAttemptParams{
		TaskID:       req.TaskID,
		Idx:          req.AttemptIdx,
		JobID:        req.JobID,
		JobNamespace: req.JobNamespace,
		ExecutorRef:  req.ExecutorRef,
	})
	if err != nil {
		return ClaimResult{}, false, "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return ClaimResult{}, false, "", fmt.Errorf("store: claim: commit: %w", err)
	}
	committed = true

	// 提交后通知（02 §3.7）。
	if s.notifier != nil && applied.Seq > 0 {
		s.notifier.Notify(runID, applied.Seq)
	}
	note := ClaimResult{
		AttemptID: attempt.ID,
		JobName:   fmt.Sprintf("tp-%s-a%d", req.TaskID, attempt.Idx),
		Attempt:   attempt,
		Seq:       applied.Seq,
	}
	return note, true, "", nil
}
