package store

// 取消动作的"状态推进 + 审计"同事务版本（PR #33 #6）。
//
// 背景：门面（facades.go）的 apply 只推进状态；提交（submit.go）与重试（control.go）
// 已经自己在事务里写审计，但 **cancel 是唯一漏网**——旧实现由 api 层在事务提交后
// 补一条审计，客户端断连/审计失败就会出现"取消了但没记"。这里把两者放进同一个
// 事务：状态推进成功 ⇔ 审计落库，任一失败整体回滚。

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// CancelAudited 执行任意非终态 → cancelling，并在同一事务里写审计。
func (m *TaskMachine) CancelAudited(ctx context.Context, taskID, actor string, e AuditEntry) (domain.Applied, error) {
	return m.applyAudited(ctx, domain.Transition{
		Entity: domain.EntityTask, ID: taskID,
		From:    domain.TaskStatuses(domain.TaskNonTerminal()...),
		To:      domain.TaskCancelling.Status(),
		Command: "cancel", Actor: actor,
	}, e)
}

// applyAudited 与 TaskMachine.apply 相同，额外在事务里插入审计；
// Noop（已经是目标状态）不写审计——没有发生变更就不该留"已取消"的记录。
func (m *TaskMachine) applyAudited(ctx context.Context, tr domain.Transition, e AuditEntry) (domain.Applied, error) {
	var applied domain.Applied
	err := m.s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := m.s.Tasks.Lock(ctx, tx, tr.ID); err != nil {
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
	if !applied.Noop && domain.TaskStatus(applied.To).IsTerminal() {
		m.s.fireTaskFinished(tr.ID)
	}
	return applied, nil
}
