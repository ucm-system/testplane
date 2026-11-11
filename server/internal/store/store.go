package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// Store 是数据库访问的唯一入口（05 §3.2 硬规则 1：只有 store 执行 SQL）。
//
// 事务边界由调用方控制：事务型方法收一个 pgx.Tx，传 nil 表示用连接池自动提交。
// 跨实体推进必须由调用方先按 Task → runner 执行记录的顺序加锁（02 §3.8）。
type Store struct {
	pool *pgxpool.Pool

	Tasks      *TaskRepo
	Attempts   *AttemptRepo
	RunnerRuns *RunnerRunRepo
	Runners    *RunnerRepo
	Leases     *LeaseRepo
	Events     *EventRepo
	Clusters   *ClusterRepo
	Audits     *AuditRepo
	Profiles   *ProfileRepo
	Artifacts  *ArtifactRepo
	// SharedMounts 是平台级统一共享挂载（D-65，运行时可管理）。
	SharedMounts *SharedMountRepo

	// notifier 可为 nil：置位后，提交与 ClaimForExecution 会在提交后 NOTIFY。
	notifier *Notifier
	// clusterName 写进 task_runners.executor_ref.cluster，默认 cluster-01（与 clusters 种子对齐）。
	clusterName string
	// taskFinishedHook 在 Task 落终态**提交之后**异步触发（报告生成；事务内不做文件 IO）。
	taskFinishedHook func(ctx context.Context, taskID string)
	// clock 是业务时间来源（05 §6.5）：写库时间戳、超窗判定都走它，默认 SystemClock。
	clock domain.Clock
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

	s := &Store{pool: pool, clusterName: "cluster-01", clock: domain.SystemClock}
	s.Tasks = &TaskRepo{s: s}
	s.Attempts = &AttemptRepo{s: s}
	s.RunnerRuns = &RunnerRunRepo{s: s}
	s.Runners = &RunnerRepo{s: s}
	s.Leases = &LeaseRepo{s: s}
	s.Events = &EventRepo{s: s}
	s.Clusters = &ClusterRepo{s: s}
	s.Audits = &AuditRepo{s: s}
	s.Profiles = &ProfileRepo{s: s}
	s.Artifacts = &ArtifactRepo{s: s}
	s.SharedMounts = &SharedMountRepo{s: s}
	return s, nil
}

// Pool 暴露连接池给需要自己开事务的调用方（LISTEN、报表）。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// SetClock 注入业务时钟（默认 SystemClock；测试可换成可拨时钟）。
func (s *Store) SetClock(c domain.Clock) {
	if c != nil {
		s.clock = c
	}
}

// now 返回业务时间（UTC）。**不要**在 store 里直调 time.Now()。
func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return domain.SystemClock.Now()
}

// SetNotifier 注入事件通知器：提交与 ClaimForExecution 在事务提交后推送。
func (s *Store) SetNotifier(n *Notifier) { s.notifier = n }

// Notifier 返回已注入的通知器（可能为 nil）。
func (s *Store) NotifierRef() *Notifier { return s.notifier }

// SetClusterName 设置本实例对接的集群名（写进 task_runners.executor_ref.cluster）。
func (s *Store) SetClusterName(name string) {
	if name != "" {
		s.clusterName = name
	}
}

// SetTaskFinishedHook 注册 Task 终态回调（报告生成等外部动作，提交后异步执行）。
func (s *Store) SetTaskFinishedHook(h func(ctx context.Context, taskID string)) {
	s.taskFinishedHook = h
}

// fireTaskFinished 在事务提交后异步触发回调；事务内禁止外部 IO（C-2.5）。
func (s *Store) fireTaskFinished(taskID string) {
	h := s.taskFinishedHook
	if h == nil || taskID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		h(ctx, taskID)
	}()
}

// EmitTaskEvent 追加一条非状态转移事件（如 exports_harvested / report.written），并分配 seq。
func (s *Store) EmitTaskEvent(ctx context.Context, taskID, typ string, payload map[string]any, actor string) (int64, error) {
	var seq int64
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		seq, err = s.nextSeq(ctx, tx, taskID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (task_id, seq, type, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, now())`, taskID, seq, typ, raw, actorOf(actor)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if s.notifier != nil && seq > 0 {
		s.notifier.Notify(taskID, seq)
	}
	return seq, nil
}

// EmitRunnerEvent 追加一条带执行记录定位的事件（如 exports_harvested）。
func (s *Store) EmitRunnerEvent(ctx context.Context, k domain.RunnerKey, typ string, payload map[string]any, actor string) (int64, error) {
	var seq int64
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		seq, err = s.nextSeq(ctx, tx, k.TaskID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		no, sq := k.AttemptNo, k.Seq
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_events (task_id, attempt_no, runner_seq, seq, type, payload, actor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now())`,
			k.TaskID, no, sq, seq, typ, raw, actorOf(actor)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if s.notifier != nil && seq > 0 {
		s.notifier.Notify(k.TaskID, seq)
	}
	return seq, nil
}

// ClusterName 返回当前集群名。
func (s *Store) ClusterName() string {
	if s.clusterName == "" {
		return "cluster-01"
	}
	return s.clusterName
}

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
// t-YYYYMMDD-XXXX（Task，03 §1）/ j-<8hex>（Job）
// crypto 随机 + base32（去掉易混字符），禁止用名称或自增序列做对外标识（K-11）。
// runner 执行记录没有独立 ID：定位键 (task_id, attempt_no, seq)（D-68）。

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

// NewTaskID 生成 t-YYYYMMDD-XXXX（UTC 日期；03 §1 的 Task ID 形态）。
func NewTaskID(now time.Time) string {
	return "t-" + now.UTC().Format("20060102") + "-" + randomID(4)
}

// NewJobID 生成 j-<8hex>；幂等键，重复创建 Job 时由执行层复用。
func NewJobID() string { return "j-" + randomID(8) }

// JobName 按 (task_id, attempt_no, seq) 派生 K8s Job 名的**默认**前缀形态
// （附录 B：幂等键 = 三元组的派生；执行层波次负责最终命名，这里只提供一致的派生）。
func JobName(k domain.RunnerKey) string {
	// 与 executor/render.go 的派生保持逐字一致（argus- 前缀 + TaskID 小写）：
	// DB 的 job_id/executor_ref、GC 的按名反查、watch 的 Get 必须指向同一个对象。
	return fmt.Sprintf("argus-%s-%d-%d", strings.ToLower(k.TaskID), k.AttemptNo, k.Seq)
}

func actorOf(actor string) string {
	if actor == "" {
		return "system:api"
	}
	return actor
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
