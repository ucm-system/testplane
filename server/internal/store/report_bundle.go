package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// pgxTx 是本包内 pgx.Tx 的别名。
type pgxTx = pgx.Tx

// IsNotFound 报告 err 是否由 ErrNotFound 包装而来（api 层映射 404 用）。
func IsNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }

// TaskReportBundle 是报告生成（C-7.2）需要的一次性读取集（一条事务，禁止 N+1）。
type TaskReportBundle struct {
	Task    *Task
	Attempt *Attempt    // 最新 attempt（Task 级报告默认取最后一个 attempt，§7.2）
	Runners []RunnerRun // 该 attempt 的执行记录（按 seq）
	Events  []Event     // 全量事件（events.jsonl 按它重建，C-7.4）
}

// ReportBundle 读取报告生成的输入（库是唯一权威，不依赖"文件先于状态存在"）。
func (s *Store) ReportBundle(ctx context.Context, taskID string) (*TaskReportBundle, error) {
	out := &TaskReportBundle{}
	err := s.InTx(ctx, func(tx pgxTx) error {
		t, err := s.Tasks.Get(ctx, tx, taskID)
		if err != nil {
			return err
		}
		out.Task = t
		att, err := s.Attempts.GetLatest(ctx, tx, taskID)
		if err == nil {
			out.Attempt = att
		} else if !isNotFound(err) {
			return err
		}
		no := t.CurrentAttempt
		if att != nil {
			no = att.AttemptNo
		}
		if no > 0 {
			runs, err := s.RunnerRuns.ListByAttempt(ctx, tx, taskID, no)
			if err != nil {
				return err
			}
			out.Runners = runs
		}
		events, err := s.Events.AllByTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		out.Events = events
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func isNotFound(err error) bool { return IsNotFound(err) }
