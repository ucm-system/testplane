package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// Deploy 归档（D-70）：平台不直连 Genet——部署状态由内置 service-deploy runner 周期
// 写 `deploy-state.json`，平台按 attempt 归档副本并把**摘要增量合并**进 tasks.deploy
// （03 §4.4）。键名与 domain.DeployDoc 一致；调用方只填变化的键，合并由 jsonb `||`
// 完成（见 TaskRepo.SetDeploy：绝不能整段覆盖，否则 release_name 会被冲掉）。
//
// `queue.position` 恒为 null（D-46：Genet 只给水位计数、没有位次），
// 保留该键只为兼容前端与 openapi；不要删。
func (s *Store) RecordDeployState(ctx context.Context, taskID string, attemptNo int,
	deployStateRef string, snap domain.DeployDoc) error {
	// 恒显式写 null：Queue.Position 为 nil 时 MarshalJSON 输出 "position": null。
	if snap.Queue != nil {
		snap.Queue.Position = nil
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Tasks.Lock(ctx, tx, taskID); err != nil {
			return err
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("store: record deploy: marshal: %w", err)
		}
		if err := s.Tasks.SetDeploy(ctx, tx, taskID, raw); err != nil {
			return fmt.Errorf("store: record deploy %s: %w", taskID, err)
		}
		if err := s.Attempts.SetRefs(ctx, tx, taskID, attemptNo, "", "", deployStateRef); err != nil {
			return fmt.Errorf("store: record deploy %s: set deploy_state_ref: %w", taskID, err)
		}
		seq, err := s.nextSeq(ctx, tx, taskID)
		if err != nil {
			return err
		}
		// 事件 payload 用本次增量快照（完整列请看 tasks.deploy）。
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (task_id, attempt_no, seq, type, reason, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'system:orchestrator', $7)`,
			taskID, attemptNo, seq, domain.EventDeployStateChanged, nullableText(snap.Reason), raw,
			s.now()); err != nil {
			return fmt.Errorf("store: record deploy event %s: %w", taskID, err)
		}
		return nil
	})
}
