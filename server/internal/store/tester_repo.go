package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TesterRepo 是 testers 表的读写。Tester 是启动期一次性同步的只读目录
// （K-3：无运行时同步）；状态用 orphaned 布尔位表达，不落 status 列。
type TesterRepo struct{ s *Store }

const testerCols = `name, version, display_name, description, role, on_failure, entry, image,
	resources, concurrency, mounts, k8s, parameters, env_decl, placeholder, source_file,
	content_sha, orphaned, removed_at, registered_at, registered_by`

func scanTester(row pgx.Row) (*Tester, error) {
	var t Tester
	err := row.Scan(&t.Name, &t.Version, &t.DisplayName, &t.Description, &t.Role, &t.OnFailure,
		&t.Entry, &t.Image, &t.Resources, &t.Concurrency, &t.Mounts, &t.K8s, &t.Parameters,
		&t.EnvDecl, &t.Placeholder, &t.SourceFile, &t.ContentSHA, &t.Orphaned, &t.RemovedAt,
		&t.RegisteredAt, &t.RegisteredBy)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Upsert 按 (name, version) 写入或更新一行（同版本不同内容由 registry 在调用前拒绝）。
func (r *TesterRepo) Upsert(ctx context.Context, tx pgx.Tx, t *Tester) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO testers (name, version, display_name, description, role, on_failure, entry, image,
		                     resources, concurrency, mounts, k8s, parameters, env_decl, placeholder,
		                     source_file, content_sha, orphaned, removed_at, registered_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (name, version) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       description  = EXCLUDED.description,
		       role         = EXCLUDED.role,
		       on_failure   = EXCLUDED.on_failure,
		       entry        = EXCLUDED.entry,
		       image        = EXCLUDED.image,
		       resources    = EXCLUDED.resources,
		       concurrency  = EXCLUDED.concurrency,
		       mounts       = EXCLUDED.mounts,
		       k8s          = EXCLUDED.k8s,
		       parameters   = EXCLUDED.parameters,
		       env_decl     = EXCLUDED.env_decl,
		       placeholder  = EXCLUDED.placeholder,
		       source_file  = EXCLUDED.source_file,
		       content_sha  = EXCLUDED.content_sha,
		       orphaned     = false,
		       removed_at   = NULL`,
		t.Name, t.Version, t.DisplayName, t.Description, t.Role, t.OnFailure, t.Entry, t.Image,
		jsonOrEmptyObject(t.Resources), jsonOrEmptyObject(t.Concurrency), jsonOrEmptyArray(t.Mounts),
		jsonOrEmptyObject(t.K8s), jsonOrEmptyObject(t.Parameters), jsonOrEmptyObject(t.EnvDecl),
		t.Placeholder, t.SourceFile, t.ContentSHA, t.Orphaned, t.RemovedAt, t.RegisteredBy)
	if err != nil {
		return fmt.Errorf("store: upsert tester %s@%s: %w", t.Name, t.Version, err)
	}
	return nil
}

// Get 取指定版本；不存在返回 ErrNotFound。
func (r *TesterRepo) Get(ctx context.Context, tx pgx.Tx, name, version string) (*Tester, error) {
	t, err := scanTester(r.s.db(tx).QueryRow(ctx,
		`SELECT `+testerCols+` FROM testers WHERE name = $1 AND version = $2`, name, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: tester %s@%s: %w", name, version, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get tester %s@%s: %w", name, version, err)
	}
	return t, nil
}

// GetLatest 取某 Tester 的最新版本（按注册时间倒序）。
func (r *TesterRepo) GetLatest(ctx context.Context, tx pgx.Tx, name string) (*Tester, error) {
	t, err := scanTester(r.s.db(tx).QueryRow(ctx,
		`SELECT `+testerCols+` FROM testers WHERE name = $1 ORDER BY registered_at DESC, version DESC LIMIT 1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: tester %s: %w", name, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get latest tester %s: %w", name, err)
	}
	return t, nil
}

// TesterFilter 是目录过滤（07 §5.3）。
type TesterFilter struct {
	Role            string
	IncludeOrphaned bool
}

// List 返回 Tester 目录。
func (r *TesterRepo) List(ctx context.Context, tx pgx.Tx, f TesterFilter) ([]Tester, error) {
	args := []any{}
	var where []string
	if f.Role != "" {
		args = append(args, f.Role)
		where = append(where, fmt.Sprintf("role = $%d", len(args)))
	}
	if !f.IncludeOrphaned {
		where = append(where, "NOT orphaned")
	}
	q := `SELECT ` + testerCols + ` FROM testers`
	if len(where) > 0 {
		cond := " WHERE "
		for i, w := range where {
			if i > 0 {
				cond += " AND "
			}
			cond += w
		}
		q += cond
	}
	q += ` ORDER BY name, version`
	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list testers: %w", err)
	}
	defer rows.Close()
	var out []Tester
	for rows.Next() {
		t, err := scanTester(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan tester: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// MarkOrphaned 标记目录里消失的 Tester（不删除，历史 Task 仍能解析，02 §2.4）。
func (r *TesterRepo) MarkOrphaned(ctx context.Context, tx pgx.Tx, name, version string) error {
	_, err := r.s.db(tx).Exec(ctx,
		`UPDATE testers SET orphaned = true, removed_at = now() WHERE name = $1 AND version = $2`,
		name, version)
	if err != nil {
		return fmt.Errorf("store: mark orphaned %s@%s: %w", name, version, err)
	}
	return nil
}
