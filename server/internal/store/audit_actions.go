package store

// 取消动作的"状态推进 + 审计"同事务版本（PR #33 #6）。
//
// 背景：门面（facades.go）的 apply 只推进状态；提交（submit.go）与重试（retry.go）
// 已经自己在事务里写审计，但 **cancel 是唯一漏网**——旧实现由 api 层在事务提交后
// 补一条审计，客户端断连/审计失败就会出现"取消了但没记"。这里把两者放进同一个
// 事务：状态推进成功 ⇔ 审计落库，任一失败整体回滚。

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// CancelAudited 执行 R8（任意非终态 → cancelling）并在同一事务里写审计。
func (m *RunMachine) CancelAudited(ctx context.Context, runID, actor string, e AuditEntry) (domain.Applied, error) {
	return m.applyAudited(ctx, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		From:    domain.RunStatuses(domain.RunNonTerminal()...),
		To:      domain.RunCancelling.Status(),
		Command: "cancel", Actor: actor,
	}, e)
}

// applyAudited 与 RunMachine.apply 相同，额外在事务里插入审计；
// Noop（已经是目标状态）不写审计——没有发生变更就不该留"已取消"的记录。
func (m *RunMachine) applyAudited(ctx context.Context, tr domain.Transition, e AuditEntry) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := m.s.Runs.Lock(ctx, tx, tr.ID); err != nil {
			return err
		}
		a, err := m.s.Apply(ctx, tx, tr)
		if err != nil {
			return err
		}
		if !a.Noop {
			if e.ObjectID == "" {
				e.ObjectID = tr.ID
			}
			if err := m.s.Audits.Insert(ctx, tx, e); err != nil {
				return fmt.Errorf("store: audited %s %s: %w: %w",
					tr.Command, tr.ID, domain.ErrTx, err)
			}
		}
		applied = a
		return nil
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, tr.ID)
	if !applied.Noop && domain.RunStatus(applied.To).IsTerminal() {
		m.s.fireRunFinished(tr.ID)
	}
	return applied, nil
}

// SkipAudited 执行 T2/T4（pending|queued → skipped）并在同一事务里写审计。
func (m *TaskMachine) SkipAudited(ctx context.Context, taskID, reason string, e AuditEntry) (domain.Applied, error) {
	return m.applyAudited(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPending.Status(), domain.TaskQueued.Status()},
		To:      domain.TaskSkipped.Status(),
		Command: "skip", Reason: reason, Actor: "system:orchestrator",
	}, e)
}

// CancelAudited 执行 T7/T10（preparing|running → canceled）并在同一事务里写审计。
func (m *TaskMachine) CancelAudited(ctx context.Context, taskID, actor string, e AuditEntry) (domain.Applied, error) {
	return m.applyAudited(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    []domain.Status{domain.TaskPreparing.Status(), domain.TaskRunning.Status()},
		To:      domain.TaskCanceled.Status(),
		Command: "cancel", Actor: actor,
	}, e)
}

// applyAudited 是 Task 版：先解析任务所属 Run、锁 Run，再推进状态并写审计（02 §3.8 锁序）。
func (m *TaskMachine) applyAudited(ctx context.Context, tr domain.Transition, e AuditEntry) (domain.Applied, error) {
	var (
		applied domain.Applied
		runID   string
	)
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		id, err := m.s.Tasks.RunIDOf(ctx, tx, tr.ID)
		if err != nil {
			return err
		}
		runID = id
		if _, err := m.s.Runs.Lock(ctx, tx, runID); err != nil {
			return err
		}
		a, err := m.s.Apply(ctx, tx, tr)
		if err != nil {
			return err
		}
		if !a.Noop {
			if e.ObjectID == "" {
				e.ObjectID = tr.ID
			}
			if err := m.s.Audits.Insert(ctx, tx, e); err != nil {
				return fmt.Errorf("store: audited %s %s: %w: %w",
					tr.Command, tr.ID, domain.ErrTx, err)
			}
		}
		applied = a
		return nil
	})
	if err != nil {
		return domain.Applied{}, err
	}
	afterCommit(m.notifier, applied, runID)
	return applied, nil
}
