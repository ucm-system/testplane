package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// RunnerRepo 是 runners 表（Runner 注册表；v1 testers 表，D-66）的读写。
// Runner 是启动期一次性同步的只读目录（K-3：无运行时注册）；
// 状态用 orphaned 布尔位表达，不落 status 列。
type RunnerRepo struct{ s *Store }

const runnerCols = `name, version, display_name, description, role, builtin, on_failure, entry,
	input_format, image, resources, concurrency, mounts, k8s, parameters, exports, consumes,
	env_decl, placeholder, clean_runner, source_file, content_sha, orphaned, removed_at,
	registered_at, registered_by`

func scanRunner(row pgx.Row) (*Runner, error) {
	var r Runner
	err := row.Scan(&r.Name, &r.Version, &r.DisplayName, &r.Description, &r.Role, &r.Builtin,
		&r.OnFailure, &r.Entry, &r.InputFormat, &r.Image, &r.Resources, &r.Concurrency,
		&r.Mounts, &r.K8s, &r.Parameters, &r.Exports, &r.Consumes, &r.EnvDecl,
		&r.Placeholder, &r.CleanRunner, &r.SourceFile, &r.ContentSHA, &r.Orphaned, &r.RemovedAt,
		&r.RegisteredAt, &r.RegisteredBy)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Upsert 按 (name, version) 写入或更新一行（同版本不同内容由 registry 在调用前拒绝）。
func (r *RunnerRepo) Upsert(ctx context.Context, tx pgx.Tx, n *Runner) error {
	_, err := r.s.db(tx).Exec(ctx, `
		INSERT INTO runners (name, version, display_name, description, role, builtin, on_failure,
		                     entry, input_format, image, resources, concurrency, mounts, k8s,
		                     parameters, exports, consumes, env_decl, placeholder, clean_runner,
		                     source_file, content_sha, orphaned, removed_at, registered_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (name, version) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       description  = EXCLUDED.description,
		       role         = EXCLUDED.role,
		       builtin      = EXCLUDED.builtin,
		       on_failure   = EXCLUDED.on_failure,
		       entry        = EXCLUDED.entry,
		       input_format = EXCLUDED.input_format,
		       image        = EXCLUDED.image,
		       resources    = EXCLUDED.resources,
		       concurrency  = EXCLUDED.concurrency,
		       mounts       = EXCLUDED.mounts,
		       k8s          = EXCLUDED.k8s,
		       parameters   = EXCLUDED.parameters,
		       exports      = EXCLUDED.exports,
		       consumes     = EXCLUDED.consumes,
		       env_decl     = EXCLUDED.env_decl,
		       placeholder  = EXCLUDED.placeholder,
		       clean_runner = EXCLUDED.clean_runner,
		       source_file  = EXCLUDED.source_file,
		       content_sha  = EXCLUDED.content_sha,
		       orphaned     = false,
		       removed_at   = NULL`,
		n.Name, n.Version, n.DisplayName, n.Description, n.Role, n.Builtin, n.OnFailure,
		jsonOrEmptyArray(n.Entry), defaultString(n.InputFormat, domain.InputFormatJSONL), n.Image,
		jsonOrEmptyObject(n.Resources), jsonOrEmptyObject(n.Concurrency), jsonOrEmptyArray(n.Mounts),
		jsonOrEmptyObject(n.K8s), jsonOrEmptyArray(n.Parameters), jsonOrEmptyArray(n.Exports),
		jsonOrEmptyArray(n.Consumes), jsonOrEmptyObject(n.EnvDecl),
		n.Placeholder, n.CleanRunner, n.SourceFile, n.ContentSHA, n.Orphaned, n.RemovedAt, n.RegisteredBy)
	if err != nil {
		return fmt.Errorf("store: upsert runner %s@%s: %w", n.Name, n.Version, err)
	}
	return nil
}

// Get 取指定版本；不存在返回 ErrNotFound。
func (r *RunnerRepo) Get(ctx context.Context, tx pgx.Tx, name, version string) (*Runner, error) {
	n, err := scanRunner(r.s.db(tx).QueryRow(ctx,
		`SELECT `+runnerCols+` FROM runners WHERE name = $1 AND version = $2`, name, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: runner %s@%s: %w", name, version, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get runner %s@%s: %w", name, version, err)
	}
	return n, nil
}

// GetLatest 取某 Runner 的最高版本（latest = 该 name 的最高版本，含 retired 行；
// 版本比较用 domain.VersionLess——排序、API、CLI 共用同一份，§5.6.1）。
func (r *RunnerRepo) GetLatest(ctx context.Context, tx pgx.Tx, name string) (*Runner, error) {
	rows, err := r.s.db(tx).Query(ctx,
		`SELECT `+runnerCols+` FROM runners WHERE name = $1`, name)
	if err != nil {
		return nil, fmt.Errorf("store: get latest runner %s: %w", name, err)
	}
	defer rows.Close()
	var best *Runner
	for rows.Next() {
		n, err := scanRunner(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan runner: %w", err)
		}
		if best == nil || domain.VersionGreater(n.Version, best.Version) {
			best = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if best == nil {
		return nil, fmt.Errorf("store: runner %s: %w", name, domain.ErrNotFound)
	}
	return best, nil
}

// RunnerFilter 是目录过滤（07 §5.3）。
type RunnerFilter struct {
	Role            string
	IncludeOrphaned bool
}

// List 返回 Runner 目录。
func (r *RunnerRepo) List(ctx context.Context, tx pgx.Tx, f RunnerFilter) ([]Runner, error) {
	args := []any{}
	var where []string
	if f.Role != "" {
		args = append(args, f.Role)
		where = append(where, fmt.Sprintf("role = $%d", len(args)))
	}
	if !f.IncludeOrphaned {
		where = append(where, "NOT orphaned")
	}
	q := `SELECT ` + runnerCols + ` FROM runners`
	for i, w := range where {
		if i == 0 {
			q += " WHERE "
		} else {
			q += " AND "
		}
		q += w
	}
	q += ` ORDER BY name, version`
	rows, err := r.s.db(tx).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runners: %w", err)
	}
	defer rows.Close()
	var out []Runner
	for rows.Next() {
		n, err := scanRunner(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan runner: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// MarkOrphaned 标记目录里消失的版本（不删除，历史执行记录仍能解析，§5.6.1）。
func (r *RunnerRepo) MarkOrphaned(ctx context.Context, tx pgx.Tx, name, version string) error {
	_, err := r.s.db(tx).Exec(ctx,
		`UPDATE runners SET orphaned = true, removed_at = now() WHERE name = $1 AND version = $2`,
		name, version)
	if err != nil {
		return fmt.Errorf("store: mark orphaned %s@%s: %w", name, version, err)
	}
	return nil
}
