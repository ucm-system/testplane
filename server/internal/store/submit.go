package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 提交 Run（C-2.5"提交 Run"行、C-5.1 收尾）：
//
//	一个事务内：插入 run → 插入全部 task → 插入 state_event(run.submitted) → 插入 audit。
//
// **不做提交幂等**（D-37）：每次调用都创建新 Run，没有幂等键、不判重。
type SubmitParams struct {
	// Run 必填；Status 固定由本方法写 submitted。
	Run *Run
	// Tasks 是解析后的 Task 快照（含 Pre-Check）；允许为空（独立 Task 形态也会有一条）。
	Tasks []*Task
	// Actor 是提交者（dev 模式下是用户名）。
	Actor string
	// Role 是提交者角色（audit 用）。
	Role string
	// SubmittedPayload 是 run.submitted 事件的附加载荷（不含凭据）。
	SubmittedPayload map[string]any
	// At 是事务时间；零值取 now。
	At time.Time
}

// CreateRun 在一个事务里落库 Run + Tasks + 提交事件 + 审计。
func (s *Store) CreateRun(ctx context.Context, p SubmitParams) (*Run, []*Task, error) {
	if p.Run == nil {
		return nil, nil, fmt.Errorf("store: create run: run 不能为空")
	}
	at := p.At
	if at.IsZero() {
		at = s.now()
	}
	if p.Run.SubmittedAt.IsZero() {
		p.Run.SubmittedAt = at
	}
	p.Run.Status = string(domain.RunSubmitted)
	p.Run.Kind = defaultString(p.Run.Kind, "run")
	p.Run.CleanupPolicy = defaultString(p.Run.CleanupPolicy, "always")

	for _, t := range p.Tasks {
		t.RunID = p.Run.ID
		if t.Status == "" {
			t.Status = string(domain.TaskPending)
		}
		if t.RetryPolicy == nil {
			t.RetryPolicy = []byte("{}")
		}
	}

	var submittedSeq int64
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if err := s.Runs.Insert(ctx, tx, p.Run); err != nil {
			return err
		}
		for _, t := range p.Tasks {
			if err := s.Tasks.Insert(ctx, tx, t); err != nil {
				return err
			}
		}

		seq, err := s.nextSeq(ctx, tx, p.Run.ID)
		if err != nil {
			return err
		}
		submittedSeq = seq
		payload := p.SubmittedPayload
		if payload == nil {
			payload = map[string]any{}
		}
		payload["kind"] = p.Run.Kind
		payload["task_count"] = len(p.Tasks)
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("store: create run: marshal submitted payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events
			  (run_id, seq, type, to_status, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.Run.ID, seq, domain.EventRunSubmitted, string(domain.RunSubmitted),
			raw, actorOf(p.Actor), at); err != nil {
			return fmt.Errorf("store: create run: insert submitted event: %w", err)
		}

		if err := s.Audits.Insert(ctx, tx, AuditEntry{
			Actor:      actorOf(p.Actor),
			Role:       defaultString(p.Role, "operator"),
			Action:     "submit",
			ObjectType: "run",
			ObjectID:   p.Run.ID,
			Detail:     map[string]any{"kind": p.Run.Kind, "task_count": len(p.Tasks)},
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if s.notifier != nil && submittedSeq > 0 {
		s.notifier.Notify(p.Run.ID, submittedSeq)
	}
	return p.Run, p.Tasks, nil
}

func actorOf(actor string) string {
	if actor == "" {
		return "system:api"
	}
	return actor
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
