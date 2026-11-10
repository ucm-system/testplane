package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// precheck 判据（03 §4.2）：`tasks.resolved.precheck.injected` 是"这个 Task 到底有没有
// 平台注入的 pre_check runner"的**唯一判据**（`precheck: "off"` 时为 {"injected": false}，
// 只有这一个键）。判定某个 runner 是不是注入的 pre_check 必须用它，**不能**用
// `role='pre_check'`：用户可以显式提交 pre_check 角色 runner（§10.1）。
//
// 注入的行在 resolved.runners[] 里带 `injected: true`（含 service-deploy / service-clean）。

// InjectedPrecheckSeq 返回平台注入的 pre_check runner 的 seq（未注入时返回 0）。
// 判据：resolved.precheck.injected == true 且 resolved.runners[] 中 injected=true 且
// role=pre_check 的那一行（`precheck: auto` 注入时固定为测试类 runner 最前面的 seq）。
func (s *Store) InjectedPrecheckSeq(ctx context.Context, tx pgx.Tx, taskID string) (int, error) {
	t, err := s.Tasks.Get(ctx, tx, taskID)
	if err != nil {
		return 0, err
	}
	injected, _, _, err := ResolvedPrecheck(t.Resolved)
	if err != nil || !injected {
		return 0, err
	}
	seq, err := injectedRunnerSeq(t.Resolved, "pre_check")
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// InjectedRunnerSeqs 返回 resolved 快照里全部注入行（injected=true）的 seq → role。
// 供编排层识别注入的 service-deploy / precheck / service-clean。
func (s *Store) InjectedRunnerSeqs(ctx context.Context, tx pgx.Tx, taskID string) (map[int]string, error) {
	t, err := s.Tasks.Get(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	return resolvedInjectedRunnerSeqs(t.Resolved)
}

// injectedRunnerSeq 从 resolved 里取指定角色的注入行 seq。
func injectedRunnerSeq(resolved []byte, role string) (int, error) {
	m, err := resolvedInjectedRunnerSeqs(resolved)
	if err != nil {
		return 0, err
	}
	for seq, r := range m {
		if r == role {
			return seq, nil
		}
	}
	return 0, nil
}

// jsonUnmarshalStrict 是 json.Unmarshal 的薄包装（容错未知字段）。
func jsonUnmarshalStrict(b []byte, v any) error { return json.Unmarshal(b, v) }

// resolvedInjectedRunnerSeqs 解析 resolved.runners[] 的注入行（03 §4.2 的快照形状）。
func resolvedInjectedRunnerSeqs(resolved []byte) (map[int]string, error) {
	var snap struct {
		Runners []struct {
			Seq      int    `json:"seq"`
			Role     string `json:"role"`
			Injected bool   `json:"injected"`
		} `json:"runners"`
	}
	if err := jsonUnmarshalStrict(resolved, &snap); err != nil {
		return nil, fmt.Errorf("store: parse resolved.runners: %w", err)
	}
	out := map[int]string{}
	for _, r := range snap.Runners {
		if r.Injected {
			out[r.Seq] = r.Role
		}
	}
	return out, nil
}
