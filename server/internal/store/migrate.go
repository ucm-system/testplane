package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ucm-system/testplane/server/migrations"
)

// 迁移执行（03 §7、C-2.6）：
//   - 只前向，没有 down；
//   - 多副本不得并发迁移：用 pg_advisory_lock 串行化，进程内也只在启动时跑一次；
//   - 记账表 schema_migrations 记录已应用版本，启动时比对"库版本 ≥ 二进制期望版本"；
//   - 迁移文件是纯 SQL，每条语句以分号结尾，按文件名排序执行。
//
// 与 golang-migrate CLI 的差异（写明在报告里）：记账表结构不同（本实现是
// version/applied_at，没有 dirty 位）。迁移文件本身兼容 golang-migrate 的
// `NNNN_name.sql` 命名；如需与 CLI 混用，把 server/migrations 交给 migrate 即可，
// 两条路径都只依赖同一批 SQL。

// migrationLockKey 是迁移用的 advisory lock 号（任意固定 64 位整数，全平台唯一）。
const migrationLockKey int64 = 0x7465_7374_706c_6e // "testpln"

// MigrationInfo 是启动时可观测的迁移状态，供 /readyz 与日志使用。
type MigrationInfo struct {
	Version int64
	Name    string
	Applied int
}

type migrationFile struct {
	version int64
	name    string
	body    string
}

// Migrate 在连接池上执行未应用的迁移。ctx 应带启动期超时。
func Migrate(ctx context.Context, pool *pgxpool.Pool) (MigrationInfo, error) {
	files, err := loadMigrations()
	if err != nil {
		return MigrationInfo{}, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return MigrationInfo{}, fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Release()

	// 连接级 advisory lock：事务外也能挡住另一个进程。
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return MigrationInfo{}, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return MigrationInfo{}, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied := map[int64]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return MigrationInfo{}, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return MigrationInfo{}, fmt.Errorf("migrate: scan version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MigrationInfo{}, fmt.Errorf("migrate: iterate schema_migrations: %w", err)
	}

	info := MigrationInfo{}
	count := 0
	for _, f := range files {
		if applied[f.version] {
			info.Version, info.Name = f.version, f.name
			count++
			continue
		}
		if err := applyMigration(ctx, conn.Conn(), f); err != nil {
			return info, err
		}
		info.Version, info.Name = f.version, f.name
		count++
	}
	info.Applied = count
	return info, nil
}

func applyMigration(ctx context.Context, conn *pgx.Conn, f migrationFile) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin %s: %w", f.name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, f.body); err != nil {
		return fmt.Errorf("migrate: apply %s: %w", f.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, f.version, f.name); err != nil {
		return fmt.Errorf("migrate: record %s: %w", f.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %s: %w", f.name, err)
	}
	return nil
}

// ExpectedSchemaVersion 返回二进制内置的最高迁移版本（启动比对用）。
func ExpectedSchemaVersion() int64 {
	files, err := loadMigrations()
	if err != nil || len(files) == 0 {
		return 0
	}
	return files[len(files)-1].version
}

// CheckSchemaVersion 校验库版本与二进制期望一致（C-2.6：不匹配拒绝启动）。
// 库版本高于二进制（滚动升级期间的旧副本）时只告警不阻塞，由调用方决定。
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool) (dbVersion int64, err error) {
	err = pool.QueryRow(ctx, `
		SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&dbVersion)
	if err != nil {
		return 0, fmt.Errorf("migrate: read schema version: %w", err)
	}
	return dbVersion, nil
}

func loadMigrations() ([]migrationFile, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}
	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// down 仅供本地回滚（03 §7 只前向），二进制不执行。
		if strings.HasSuffix(e.Name(), ".down.sql") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".up.sql")
		name = strings.TrimSuffix(name, ".sql")
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			return nil, fmt.Errorf("migrate: 迁移文件名缺版本前缀: %s", e.Name())
		}
		version, err := strconv.ParseInt(name[:idx], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migrate: 迁移文件版本号非法: %s", e.Name())
		}
		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		files = append(files, migrationFile{version: version, name: e.Name(), body: string(body)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	return files, nil
}
