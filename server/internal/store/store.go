package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 是数据库访问的唯一入口（05 §3.2 硬规则 1：只有 store 执行 SQL）。
//
// 事务边界由调用方控制：事务型方法收一个 pgx.Tx，传 nil 表示用连接池自动提交。
// 跨实体推进必须由调用方先按 Run → Task → Attempt 的顺序加锁（02 §3.8）。
type Store struct {
	pool *pgxpool.Pool

	Runs     *RunRepo
	Tasks    *TaskRepo
	Attempts *AttemptRepo
	Testers  *TesterRepo
	Leases   *LeaseRepo
	Events   *EventRepo
	Clusters *ClusterRepo
	Audits   *AuditRepo
	Profiles *ProfileRepo
}

// DBTX 是 *pgxpool.Pool 与 pgx.Tx 的公共子集：让同一个 repo 方法既能跑在
// 自动提交上，也能跑在调用方的事务里。
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New 打开连接池、跑迁移、装配各 repo。调用方负责 Close。
// 迁移失败直接返回错误：启动期 fail fast（03 §7）。
func New(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	s := &Store{pool: pool}
	s.Runs = &RunRepo{s: s}
	s.Tasks = &TaskRepo{s: s}
	s.Attempts = &AttemptRepo{s: s}
	s.Testers = &TesterRepo{s: s}
	s.Leases = &LeaseRepo{s: s}
	s.Events = &EventRepo{s: s}
	s.Clusters = &ClusterRepo{s: s}
	s.Audits = &AuditRepo{s: s}
	s.Profiles = &ProfileRepo{s: s}
	return s, nil
}

// Pool 暴露连接池给需要自己开事务的调用方（LISTEN、报表）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close 关闭连接池。
func (s *Store) Close() { s.pool.Close() }

// Ping 检查数据库连通性（/readyz 用）。
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Begin 开启一个事务；调用方 defer Rollback。
func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	return tx, nil
}

// InTx 在事务里执行 fn：fn 返回 nil 才提交，否则回滚并原样返回错误。
func (s *Store) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// db 把可选的 tx 折叠成查询目标：tx 非 nil 用它，否则用连接池。
func (s *Store) db(tx pgx.Tx) DBTX {
	if tx != nil {
		return tx
	}
	return s.pool
}

// ---- ID 生成（05 §6.5）---------------------------------------------------
//
// r-YYYYMMDD-XXXX / t-XXXXXX / <task>-a<n> / j-<8hex>
// crypto 随机 + base32（去掉易混字符），禁止用名称或自增序列做对外标识（K-11）。

// idEncoding 用 Crockford base32 字母表（去掉 I/L/O/U），避免人工转录出错。
var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func randomID(n int) string {
	buf := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败只可能是系统级异常；用时间派生值兜底，避免 panic。
		return "0000" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	s := idEncoding.EncodeToString(buf)
	if len(s) < n {
		return s
	}
	return s[:n]
}

// NewRunID 生成 r-YYYYMMDD-XXXX（UTC 日期）。
func NewRunID(now time.Time) string {
	return "r-" + now.UTC().Format("20060102") + "-" + randomID(4)
}

// NewTaskID 生成 t-XXXXXX。
func NewTaskID() string { return "t-" + randomID(6) }

// AttemptID 生成 <task>-a<n>（03 §3.1）。
func AttemptID(taskID string, idx int) string { return fmt.Sprintf("%s-a%d", taskID, idx) }

// NewJobID 生成 j-<8hex>；幂等键，重复创建 Job 时由执行层复用。
func NewJobID() string { return "j-" + randomID(8) }
