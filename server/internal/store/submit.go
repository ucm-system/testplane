package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 提交 Task（C-2.5"提交 Task"行）：
//
//	一个事务内：插入 task → 插入 attempt#1 → 按 resolved 全量插入 task_runners（含注入行）
//	→ 插入 state_event(submitted) → 插入 audit。
//
// **不做提交幂等**（D-37）：每次调用都创建新 Task，没有幂等键、不判重。
type SubmitParams struct {
	// Task 必填；Status 固定由本方法写 submitted。
	Task *Task
	// AttemptMax 是 attempt 上限校验所需的配置（0 跳过校验）。
	// Runners 是解析后的 runner 执行记录快照（含注入的 service-deploy / precheck / service-clean；
	// seq 从 1 递增）。可为 0（D-77：只跑 deploy/precheck/clean）；上限由提交校验（TASK_TOO_LARGE）在 orchestrator 做。
	Runners []*RunnerRun
	// Actor 是提交者（dev 模式下是用户名）。
	Actor string
	// Role 是提交者角色（audit 用）。
	Role string
	// SubmittedPayload 是 submitted 事件的附加载荷（不含凭据）。
	SubmittedPayload map[string]any
	// At 是事务时间；零值取 now。
	At time.Time
}

// SubmitTask 在一个事务里落库 Task + attempt#1 + 全部 task_runners + 提交事件 + 审计。
// attempt#1 的 trigger='initial'，task_runners 的 attempt_no 全部落 1。
func (s *Store) SubmitTask(ctx context.Context, p SubmitParams) (*Task, []*RunnerRun, error) {
	if p.Task == nil {
		return nil, nil, fmt.Errorf("store: submit task: task 不能为空")
	}
	at := p.At
	if at.IsZero() {
		at = s.now()
	}
	if p.Task.SubmittedAt.IsZero() {
		p.Task.SubmittedAt = at
	}
	p.Task.Status = string(domain.TaskSubmitted)
	for i, rr := range p.Runners {
		rr.TaskID = p.Task.ID
		rr.AttemptNo = 1
		if rr.Seq != i+1 {
			return nil, nil, fmt.Errorf("store: submit task %s: runners[%d].seq=%d 必须从 1 连续递增: %w",
				p.Task.ID, i, rr.Seq, domain.ErrPrecondition)
		}
		if rr.Status == "" {
			rr.Status = string(domain.RunnerPending)
		}
		if rr.JobID == "" {
			rr.JobID = JobName(domain.RunnerKey{TaskID: rr.TaskID, AttemptNo: 1, Seq: rr.Seq})
		}
	}

	var submittedSeq int64
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if err := s.Tasks.Insert(ctx, tx, p.Task); err != nil {
			return err
		}
		if _, err := s.Attempts.Create(ctx, tx, CreateAttemptParams{TaskID: p.Task.ID, Trigger: "initial", AttemptNo: 1}); err != nil {
			return err
		}
		for _, rr := range p.Runners {
			if err := s.RunnerRuns.Insert(ctx, tx, rr); err != nil {
				return err
			}
		}

		seq, err := s.nextSeq(ctx, tx, p.Task.ID)
		if err != nil {
			return err
		}
		submittedSeq = seq
		payload := p.SubmittedPayload
		if payload == nil {
			payload = map[string]any{}
		}
		payload["runner_count"] = len(p.Runners)
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("store: submit task: marshal submitted payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (task_id, seq, type, to_status, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.Task.ID, seq, domain.EventTaskSubmitted, string(domain.TaskSubmitted),
			raw, actorOf(p.Actor), at); err != nil {
			return fmt.Errorf("store: submit task: insert submitted event: %w", err)
		}

		return s.Audits.Insert(ctx, tx, AuditEntry{
			Actor:      actorOf(p.Actor),
			Role:       defaultString(p.Role, "operator"),
			Action:     "submit",
			ObjectType: "task",
			ObjectID:   p.Task.ID,
			Detail:     map[string]any{"result": "ok", "runner_count": len(p.Runners)},
		})
	})
	if err != nil {
		return nil, nil, err
	}
	if s.notifier != nil && submittedSeq > 0 {
		s.notifier.Notify(p.Task.ID, submittedSeq)
	}
	return p.Task, p.Runners, nil
}
