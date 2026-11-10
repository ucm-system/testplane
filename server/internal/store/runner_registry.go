package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// RunnerRegistry 是 registry 包需要的持久化出口（store 自有行类型，避免反向依赖）。
//
// 语义（K-3、§5.6.1）：
//   - Runner 随镜像发布，启动期一次性扫描入库，**不做运行时注册**；
//   - 同一 (name, version) 内容相同 → 幂等（不写库、不报错）；
//   - 同一 (name, version) 内容不同 → 拒绝（RUNNER_VERSION_PINNED），必须升版本；
//   - 目录里消失的版本只标记 orphaned，不删除（历史执行记录仍能解析）。
type RunnerRegistry struct{ s *Store }

// NewRunnerRegistry 构造 adapter。
func NewRunnerRegistry(s *Store) *RunnerRegistry { return &RunnerRegistry{s: s} }

// UpsertRunner 幂等写入一个 Runner 版本，created 表示是否新增。
// 同版本不同 content_sha 返回 *domain.FailureError（code=RUNNER_VERSION_PINNED）。
func (r *RunnerRegistry) UpsertRunner(ctx context.Context, n *Runner) (created bool, err error) {
	if n == nil || n.Name == "" || n.Version == "" {
		return false, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "runner", Code: domain.IssueCodeRequired, Message: "name 与 version 必填",
		}}}
	}
	if n.ContentSHA == "" {
		return false, fmt.Errorf("store: upsert runner %s@%s: 缺少 content_sha", n.Name, n.Version)
	}
	err = r.s.InTx(ctx, func(tx pgx.Tx) error {
		var existingSHA string
		qerr := tx.QueryRow(ctx,
			`SELECT content_sha FROM runners WHERE name = $1 AND version = $2 FOR UPDATE`,
			n.Name, n.Version).Scan(&existingSHA)
		switch {
		case errors.Is(qerr, pgx.ErrNoRows):
			// 首次登记
			created = true
			return r.s.Runners.Upsert(ctx, tx, n)
		case qerr != nil:
			return fmt.Errorf("store: upsert runner %s@%s: %w", n.Name, n.Version, qerr)
		case existingSHA != n.ContentSHA:
			return domain.NewFailureError(domain.CodeRunnerVersionPinned,
				fmt.Sprintf("Runner %s@%s 已发布且内容不同（%s != %s），必须升版本",
					n.Name, n.Version, existingSHA, n.ContentSHA),
				map[string]any{
					"runner": n.Name, "version": n.Version,
					"registered_sha": existingSHA, "incoming_sha": n.ContentSHA,
				})
		default:
			// 内容一致：幂等，不写库（保留首次的 registered_at/registered_by）。
			return nil
		}
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// GetRunner 取指定版本；version 为空表示最高版本。
func (r *RunnerRegistry) GetRunner(ctx context.Context, name, version string) (*Runner, error) {
	if version == "" {
		return r.s.Runners.GetLatest(ctx, nil, name)
	}
	return r.s.Runners.Get(ctx, nil, name, version)
}

// ListRunners 返回全部 Runner（含 orphaned，供 registry 比对 diff）。
func (r *RunnerRegistry) ListRunners(ctx context.Context) ([]Runner, error) {
	return r.s.Runners.List(ctx, nil, RunnerFilter{IncludeOrphaned: true})
}

// MarkRunnerOrphaned 标记目录里消失的版本（不删除）。
func (r *RunnerRegistry) MarkRunnerOrphaned(ctx context.Context, name, version string) error {
	return r.s.Runners.MarkOrphaned(ctx, nil, name, version)
}
