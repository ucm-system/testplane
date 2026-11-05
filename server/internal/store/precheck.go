package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PrecheckTaskIDs 返回该 Run **真的注入了平台 Pre-Check** 时对应的那个 Task id 集合。
//
// 判别依据是提交时冻结的解析快照，不是"测试用了哪个 Tester"：`endpoint-probe` 本身是
// `role=pre_check`，但用户可以把它当普通测试用（`precheck: "off"`）。只有
//
//	runs.resolved -> 'precheck' ->> 'injected'  == true   且   tasks.seq = 0
//
// 才是平台注入的 Pre-Check（注入时固定 seq=0；见 api/submit_prepare.go 的 resolved 快照）。
// 叠加 `testers.role='pre_check'` 做双保险：注入路径在提交校验里已强制该角色；
// 注册表行缺失（历史数据）时按"可能是"放行，不影响主判据。
//
// 反例（QA r-20261105-TA6M）：`precheck:"off"` + 唯一测试 Task 用了 endpoint-probe，
// 此时 injected=false → 返回空集 → 全部 Task 走"测试"语义，TASK_TIMEOUT 按平台侧错误处理。
func (s *Store) PrecheckTaskIDs(ctx context.Context, tx pgx.Tx, runID string) (map[string]bool, error) {
	rows, err := s.db(tx).Query(ctx, `
		SELECT t.id
		  FROM tasks t
		  JOIN runs r ON r.id = t.run_id
		  LEFT JOIN testers ts
		         ON ts.name = t.tester_name AND ts.version = t.tester_version
		 WHERE t.run_id = $1
		   AND t.seq = 0
		   AND COALESCE((r.resolved -> 'precheck' ->> 'injected')::boolean, false)
		   AND COALESCE(ts.role, 'pre_check') = 'pre_check'`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: precheck task ids %s: %w", runID, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan precheck task ids: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}