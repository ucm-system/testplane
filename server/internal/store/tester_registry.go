package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TesterRegistry 是 registry 包（backend-exec）需要的持久化出口。
//
// 语义（K-3、E-10、02 §2.4）：
//   - Tester 随镜像发布，启动期一次性扫描入库，**不做运行时注册**；
//   - 同一 (name, version) 内容相同 → 幂等（不写库、不报错）；
//   - 同一 (name, version) 内容不同 → 拒绝（TESTER_VERSION_PINNED），必须升版本；
//   - 目录里消失的版本只标记 orphaned，不删除（历史 Task 仍能解析）。
//
// 这是薄 adapter：不 import registry 包，只暴露 store 自己的行类型，避免反向依赖。
type TesterRegistry struct{ s *Store }

// NewTesterRegistry 构造 adapter。
func NewTesterRegistry(s *Store) *TesterRegistry { return &TesterRegistry{s: s} }

// UpsertTester 幂等写入一个 Tester 版本，created 表示是否新增。
// 同版本不同 content_sha 返回 *domain.FailureError（code=TESTER_VERSION_PINNED）。
func (r *TesterRegistry) UpsertTester(ctx context.Context, t *Tester) (created bool, err error) {
	if t == nil || t.Name == "" || t.Version == "" {
		return false, &domain.ValidationError{Issues: []domain.ValidationIssue{{
			Path: "tester", Code: domain.IssueCodeRequired, Message: "name 与 version 必填",
		}}}
	}
	if t.ContentSHA == "" {
		return false, fmt.Errorf("store: upsert tester %s@%s: 缺少 content_sha", t.Name, t.Version)
	}
	err = r.s.InTx(ctx, func(tx pgx.Tx) error {
		var existingSHA string
		qerr := tx.QueryRow(ctx,
			`SELECT content_sha FROM testers WHERE name = $1 AND version = $2 FOR UPDATE`,
			t.Name, t.Version).Scan(&existingSHA)
		switch {
		case errors.Is(qerr, pgx.ErrNoRows):
			// 首次登记
			created = true
			return r.s.Testers.Upsert(ctx, tx, t)
		case qerr != nil:
			return fmt.Errorf("store: upsert tester %s@%s: %w", t.Name, t.Version, qerr)
		case existingSHA != t.ContentSHA:
			return domain.NewFailureError(domain.CodeTesterVersionPinned,
				fmt.Sprintf("Tester %s@%s 已发布且内容不同（%s != %s），必须升版本",
					t.Name, t.Version, existingSHA, t.ContentSHA),
				map[string]any{
					"tester": t.Name, "version": t.Version,
					"registered_sha": existingSHA, "incoming_sha": t.ContentSHA,
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

// GetTester 取指定版本；version 为空表示最新版本（按注册时间倒序）。
func (r *TesterRegistry) GetTester(ctx context.Context, name, version string) (*Tester, error) {
	if version == "" {
		return r.s.Testers.GetLatest(ctx, nil, name)
	}
	return r.s.Testers.Get(ctx, nil, name, version)
}

// ListTesters 返回全部 Tester（含 orphaned，供 registry 比对 diff）。
func (r *TesterRegistry) ListTesters(ctx context.Context) ([]Tester, error) {
	return r.s.Testers.List(ctx, nil, TesterFilter{IncludeOrphaned: true})
}

// MarkTesterOrphaned 标记目录里消失的版本（不删除）。
func (r *TesterRegistry) MarkTesterOrphaned(ctx context.Context, name, version string) error {
	return r.s.Testers.MarkOrphaned(ctx, nil, name, version)
}
