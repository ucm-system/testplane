// Package dbtest 是**测试专用**的数据库引导：把 `AG_TEST_DATABASE_URL`
// 归一化到独立的 `*_test*` 库，并在不同测试包之间做隔离。
//
// 背景（Lead 报的 P0）：测试里的迁移/清表如果跑在 dev 库（`argus`）上，会把真数据
// drop 掉。所以这里的硬规则是：
//
//   - 目标库名**必须**以 `_test` 结尾（自动派生），否则一律拒绝 DDL/DROP；
//   - 每个测试包用自己的库（`<base>_test_<purpose>`），包之间互不清表；
//   - 库不存在时自动 `CREATE DATABASE`，已存在则忽略；
//   - 生产代码不 import 本包（只被 `_test.go` 引用）。
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvVar 是测试库连接串的环境变量名。
const EnvVar = "AG_TEST_DATABASE_URL"

// URL 返回该测试包独占的库连接串。没设环境变量时返回 "", nil（调用方应 t.Skip）。
//
// purpose 建议用包名（store/api/report），用来派生库名后缀。
func URL(purpose string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(EnvVar))
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("dbtest: parse %s: %w", EnvVar, err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		return "", fmt.Errorf("dbtest: %s 缺少库名", EnvVar)
	}
	target := deriveName(dbName, purpose)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ensureDatabase(ctx, raw, target); err != nil {
		return "", err
	}
	u.Path = "/" + target
	return u.String(), nil
}

// deriveName 派生测试库名：**始终**含 `_test`（安全闸门据此判断），
// 不同 purpose 用不同后缀，避免不同测试包互相清表。
//
//	argus          + store  -> argus_test_store
//	argus_test     + store  -> argus_test_store
//	argus          + ""     -> argus_test
func deriveName(dbName, purpose string) string {
	base := dbName
	if !strings.Contains(base, "_test") {
		base += "_test"
	}
	if purpose == "" {
		return base
	}
	purpose = strings.ReplaceAll(strings.ReplaceAll(purpose, "-", "_"), "/", "_")
	if strings.HasSuffix(base, "_"+purpose) {
		return base // 幂等：已经是本 purpose 的库
	}
	return base + "_" + purpose
}

// ensureDatabase 在维护连接上建库（已存在则忽略）。
// 只对 `*_test*` 目标库执行，防止误连 dev 库。
func ensureDatabase(ctx context.Context, raw, target string) error {
	if !strings.Contains(target, "_test") {
		return fmt.Errorf("dbtest: 拒绝在非测试库 %q 上建库/迁移（测试库名必须以 _test 结尾）", target)
	}
	conn, err := connectMaintenance(ctx, raw)
	if err != nil {
		return fmt.Errorf("dbtest: 连接维护库: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, target).Scan(&exists); err != nil {
		return fmt.Errorf("dbtest: 检查库 %s: %w", target, err)
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE "`+strings.ReplaceAll(target, `"`, `""`)+`"`); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // duplicate_database
			return nil
		}
		return fmt.Errorf("dbtest: 建库 %s: %w", target, err)
	}
	return nil
}

// connectMaintenance 拿到一个可执行 `CREATE DATABASE` 的连接：
//   - 先试环境变量给的 DSN（多数环境里那个库已存在）；
//   - 若那个库本身不存在（SQLSTATE 3D000 invalid_catalog_name），退到同实例的
//     `postgres`（再退 `template1`）维护库——新环境因此不需要手工预建库。
func connectMaintenance(ctx context.Context, raw string) (*pgx.Conn, error) {
	if conn, err := pgx.Connect(ctx, raw); err == nil {
		return conn, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("dbtest: parse dsn: %w", err)
	}
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		u2 := *u
		u2.Path = "/" + maintenance
		conn, err := pgx.Connect(ctx, u2.String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// ResetSchema 重建 public schema（只允许在测试库上执行）。
// 返回目标库名供日志用。
func ResetSchema(ctx context.Context, dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("dbtest: parse dsn: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(name, "_test") {
		return name, fmt.Errorf("dbtest: 拒绝 DROP SCHEMA 非测试库 %q", name)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return name, fmt.Errorf("dbtest: connect %s: %w", name, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		return name, fmt.Errorf("dbtest: reset schema %s: %w", name, err)
	}
	return name, nil
}

// IsTestDatabase 报告库名是否是测试库（供断言/日志）。
func IsTestDatabase(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	return strings.Contains(strings.TrimPrefix(u.Path, "/"), "_test")
}
