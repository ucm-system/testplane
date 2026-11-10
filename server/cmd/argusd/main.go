// Command argusd 是 Argus 控制面的唯一入口（05 §2.2）。
//
//	--role / AG_ROLE = api | controller | collector | all
//
// 装配顺序：config → logger → store(迁移) → notifier → seed → machines → registry
// → executor → orchestrator（线性管道推进器）→ report → api(+webui) → 优雅退出。
//
// 执行层：*executor.Executor 已对齐 ports.JobSpec / ports.JobHandle 冻结接口
// （S2 波次落地），本文件只按 ports.Executor / ports.ResultValidator 编程。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ucm-system/testplane/server/internal/api"
	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/k8s"
	"github.com/ucm-system/testplane/server/internal/orchestrator"
	"github.com/ucm-system/testplane/server/internal/registry"
	"github.com/ucm-system/testplane/server/internal/report"
	"github.com/ucm-system/testplane/server/internal/store"
	"github.com/ucm-system/testplane/server/internal/webui"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		slog.Default().Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// 子命令/参数：`--role=...`、`--auth=dev`、`migrate up`（Helm pre-upgrade hook，06 §6.2）。
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "migrate" {
		return runMigrate(args[1:])
	}
	flags := parseFlags(args)

	cfg, err := config.Load()
	if err != nil {
		// migrate 之外的场景：配置非法直接退出（fail fast）。
		return err
	}
	if v, ok := flags["role"]; ok && v != "" {
		cfg.Role = config.Role(v)
		if !cfg.Role.Valid() {
			return fmt.Errorf("--role=%q 非法，允许 api|controller|collector|all", v)
		}
	}
	if v, ok := flags["auth"]; ok && v != "" {
		cfg.AuthMode = v
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("argusd 启动",
		"version", version, "role", cfg.Role, "addr", cfg.HTTPAddr,
		"storage_root", cfg.StorageRoot, "dsn", cfg.RedactedDSN(),
		"max_global_concurrency", cfg.MaxGlobalConcurrency)
	if cfg.AuthMode != "dev" {
		logger.Warn("AG_AUTH_MODE=oidc 在 MVP 未实现，暂按 dev 模式运行（无鉴权）")
	}

	// ---- store（含启动迁移）----
	st, err := store.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer st.Close()

	notifier := store.NewNotifier(st.Pool())
	st.SetNotifier(notifier)
	notifier.StartListener(ctx)

	if err := st.SeedDefaults(ctx); err != nil {
		logger.Warn("种子数据失败（继续启动）", "err", err)
	}
	st.SetClusterName(cfg.ClusterName)
	machines := store.NewMachines(st, notifier)

	// ---- Runner 注册表（启动扫描一次，K-3）----
	reg := registry.New(&registryStorage{reg: store.NewRunnerRegistry(st)}, registry.Options{
		Logf: func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...), "component", "registry") },
	})
	runnersDir := resolveRunnersDir(cfg.RunnersDir, logger)
	if _, err := reg.Load(ctx, runnersDir); err != nil {
		// K-3：注册表有错 → 拒绝启动（只有 controller 角色需要强校验）。
		if cfg.Role.RunsController() {
			return fmt.Errorf("Runner 注册表校验失败（%s）: %w", runnersDir, err)
		}
		logger.Warn("Runner 注册表校验失败，api 继续启动", "err", err, "dir", runnersDir)
	}
	logger.Info("Runner 注册表就绪", "dir", runnersDir, "count", reg.Len())

	// ---- 统一共享挂载（D-65）：表是运行时真源，env 只做首次启动的种子 ----
	if imported, err := st.SharedMounts.SeedIfEmpty(ctx, cfg.SharedMounts); err != nil {
		logger.Warn("统一共享挂载种子导入失败（继续启动，管理面仍可用）", "err", err)
	} else if imported > 0 {
		logger.Info("统一共享挂载种子已导入（AG_SHARED_MOUNTS → shared_mounts）", "imported", imported)
	} else if len(cfg.SharedMounts) > 0 {
		logger.Info("统一共享挂载已有管理面记录，跳过 env 种子（D-65：DB 是真源）")
	}
	// 渲染/提交校验的运行时来源：读 DB 里启用中的行。
	sharedMountsFn := sharedMountsFromStore(st)

	// ---- 统一共享挂载 preflight（D-63 ④ / D-65：缺失只告警，不拒绝启动、不阻断 Pod）----
	preflightSharedMounts(ctx, cfg, st, logger)

	// ---- 报告 ----
	reportWriter := &report.Writer{
		Store: st, Root: cfg.StorageRoot,
		MaxReportBytes: 20 << 20, Logger: logger,
	}
	// Task 落终态（事务提交后）异步生成 report.json / report.md / metadata.json / events.jsonl。
	st.SetTaskFinishedHook(func(ctx context.Context, taskID string) {
		path, err := reportWriter.WriteTaskReport(ctx, taskID)
		if err != nil {
			logger.Error("报告生成失败（不回滚终态）", "err", err, "task_id", taskID)
			return
		}
		if _, err := st.EmitTaskEvent(ctx, taskID, domain.EventReportWritten,
			map[string]any{"path": path}, "system:report"); err != nil {
			logger.Warn("写 report.written 事件失败", "err", err, "task_id", taskID)
		}
	})
	resultValidator := &report.Validator{MaxBytes: cfg.ResultMaxBytes}

	// ---- 编排器（线性管道推进器；执行器不可用时只跑 api）----
	var (
		scheduler *orchestrator.Scheduler
		runErr    chan error
	)
	if cfg.Role.RunsController() {
		sched, err := buildScheduler(ctx, cfg, st, machines, reg, resultValidator, logger)
		if err != nil {
			logger.Warn("controller 未启用（K8s/执行器不可用）", "err", err)
		} else {
			scheduler = sched
		}
	}
	if scheduler != nil {
		runErr = make(chan error, 2)
		go func() {
			logger.Info("orchestrator 循环启动",
				"concurrency", cfg.MaxGlobalConcurrency, "attempt_max", cfg.AttemptMax)
			runErr <- scheduler.Run(ctx)
		}()
	}

	// ---- Volcano 队列快照（E-11、C-4.8）----
	// 只读：轮询 queues + podgroups，15s TTL 缓存 + 单飞 + 失败退避 60s。
	// 构造失败就留 nil，/queue-status 的 volcano 块自动降级 scheduler_unavailable。
	var volcanoProvider api.VolcanoProvider
	if cfg.VolcanoEnabled {
		v, err := k8s.NewVolcano(k8s.Options{Kubeconfig: cfg.Kubeconfig}, k8s.VolcanoOptions{
			Enabled:    true,
			Cluster:    cfg.ClusterName,
			Namespaces: cfg.VolcanoNamespaces,
			TTL:        cfg.VolcanoTTL,
			Logger:     logger,
		})
		if err != nil {
			logger.Warn("Volcano 队列快照未接入（降级 scheduler_unavailable）", "err", err)
		} else {
			volcanoProvider = &volcanoBridge{v: v}
			logger.Info("Volcano 队列快照就绪",
				"cluster", cfg.ClusterName, "namespaces", cfg.VolcanoNamespaces,
				"ttl", cfg.VolcanoTTL)
		}
	} else {
		// 配置关闭 ≠ 集群没装：四态里的 disabled（不是 crd_missing，也不是"0 排队"）。
		volcanoProvider = &volcanoBridge{v: k8s.NewVolcanoWithDynamic(nil, k8s.VolcanoOptions{Enabled: false})}
		logger.Info("Volcano 队列快照已关闭（detail=disabled）")
	}

	// ---- HTTP ----
	srv := api.New(api.Deps{
		Config: cfg, Store: st, Machines: machines,
		Catalog:       reg,
		CleanReplayer: scheduler,
		Logger:        logger,
		WebUI:         webui.FS(), Version: version,
		Volcano:        volcanoProvider,
		SharedMountsFn: sharedMountsFn,
	})
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Role.RunsAPI() {
		go func() {
			logger.Info("API 监听", "addr", cfg.HTTPAddr, "prefix", cfg.APIPrefix)
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				runErr = pushErr(runErr, err)
			}
		}()
	} else {
		logger.Info("ROLE 不含 api，跳过 HTTP 监听", "role", cfg.Role)
	}

	if cfg.Role.RunsCollector() {
		logger.Warn("collector 角色在 MVP 未实现（日志采集由 executor/logcollect 负责）")
	}
	if runErr == nil {
		runErr = make(chan error, 1)
	}

	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅退出")
	case err := <-runErr:
		stop()
		if err != nil {
			logger.Error("子任务退出", "err", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 关闭超时", "err", err)
	}
	logger.Info("argusd 退出")
	return nil
}

func pushErr(ch chan error, err error) chan error {
	if ch == nil {
		ch = make(chan error, 1)
	}
	select {
	case ch <- err:
	default:
	}
	return ch
}

// buildScheduler 装配编排层；没有可用的 K8s 客户端时返回错误（由调用方降级）。
func buildScheduler(ctx context.Context, cfg *config.Config, st *store.Store,
	machines *store.Machines, reg *registry.Registry, validator *report.Validator, logger *slog.Logger) (*orchestrator.Scheduler, error) {

	client, namespace, err := k8s.New(k8s.Options{Kubeconfig: cfg.Kubeconfig})
	if err != nil {
		return nil, err
	}
	// 执行层装配（S2 已落地：*executor.Executor 满足 ports.Executor）。
	exec := executor.NewWithNamespace(client, namespace, executor.Options{})

	// GC 接回（execution §6.3）：维护 tick 周期调 executor.CollectGarbage。
	// stale 回调引用 sched 变量（New 之后赋值；GC tick 首轮在 GCInterval 之后，
	// 届时 sched 已非 nil）。间隔用编排层默认（DefaultGCInterval = 5m）。
	var sched *orchestrator.Scheduler
	sched, err = orchestrator.New(orchestrator.Options{
		Store:     st,
		Machines:  machines,
		Executor:  exec,
		Validator: validator,
		Config: orchestrator.Config{
			Namespace:                    namespace,
			StorageRoot:                  cfg.StorageRoot,
			StoragePVC:                   cfg.StoragePVC,
			ClusterName:                  cfg.ClusterName,
			SharedMounts:                 cfg.SharedMounts,
			SharedMountsFn:               sharedMountsFromStore(st),
			SharedMountsDisabledProfiles: disabledSharedMountProfiles(cfg),
			MaxGlobalConcurrency:         cfg.MaxGlobalConcurrency,
			AttemptMax:                   cfg.AttemptMax,
			ReconcileInterval:            cfg.ReconcileInterval,
			GCEnabled:                    cfg.JobGCEnabled,
			GCTick: (&gcTicker{st: st, exec: exec,
				stale: func(ctx context.Context, taskID string, attemptNo, seq int) error {
					return sched.MarkStaleRunnerTimeout(ctx, taskID, attemptNo, seq)
				}}).Tick,
		},
		Logf: func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...), "component", "orchestrator")
		},
	})
	if err != nil {
		return nil, err
	}
	// 注册表周期重扫（K-3 的"启动扫描 + 定期重扫"，不做运行时注册）。
	go func() {
		if _, err := reg.Load(ctx, resolveRunnersDir(cfg.RunnersDir, logger)); err != nil && ctx.Err() == nil {
			logger.Warn("Runner 重扫失败（沿用上一次的注册表）", "err", err)
		}
	}()
	_ = ctx
	return sched, nil
}

// volcanoBridge 把 k8s.Volcano（S2 范围，不改）适配成 api.VolcanoProvider。
// 字段集合逐字一致，纯类型搬运；k8s 侧改字段时这里编译会先挂。
type volcanoBridge struct {
	v *k8s.Volcano
}

func (b *volcanoBridge) VolcanoSnapshot(ctx context.Context) api.VolcanoSnapshot {
	snap := b.v.VolcanoSnapshot(ctx)
	out := api.VolcanoSnapshot{Available: snap.Available, Detail: snap.Detail, Stale: snap.Stale}
	out.Queues = make([]api.VolcanoQueue, 0, len(snap.Queues))
	for _, q := range snap.Queues {
		out.Queues = append(out.Queues, api.VolcanoQueue{
			Name: q.Name, State: q.State, Capability: q.Capability, Allocated: q.Allocated,
			PendingJobs: q.PendingJobs, LongestWaitSeconds: q.LongestWaitSeconds, Reason: q.Reason,
		})
	}
	return out
}

// resolveRunnersDir 在默认相对路径不存在时向上找仓库里的 runners/（本地 go run 友好）。
func resolveRunnersDir(dir string, logger *slog.Logger) string {
	candidates := []string{dir}
	if !filepath.IsAbs(dir) {
		candidates = append(candidates, filepath.Join("..", dir), filepath.Join("..", "..", dir))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if c != dir {
				logger.Info("RunnersDir 已回退解析", "configured", dir, "resolved", c)
			}
			return c
		}
	}
	return dir
}

// parseFlags 解析 `--key=value` / `--key value` 形式的参数；未知参数忽略。
func parseFlags(args []string) map[string]string {
	out := make(map[string]string, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		kv := strings.TrimPrefix(a, "--")
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[kv] = args[i+1]
			i++
			continue
		}
		out[kv] = "true"
	}
	return out
}

// runMigrate 执行 `argusd migrate up`：只跑迁移并退出（Helm hook Job 用）。
func runMigrate(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	if len(args) > 0 && args[0] != "up" {
		return fmt.Errorf("不支持的 migrate 子命令 %q（只支持 up）", args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.New(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer st.Close()
	version, err := store.CheckSchemaVersion(ctx, st.Pool())
	if err != nil {
		return err
	}
	logger.Info("数据库迁移完成", "schema_version", version)
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

// disabledSharedMountProfiles 解析 AG_SHARED_MOUNTS_DISABLED_PROFILES（逗号分隔的 Profile 名）。
//
// D-63 ③ 说"Execution Profile 可用 sharedMounts: false 关闭"，但 execution_profiles 表里还没有
// 这一列；先用环境变量表达同一语义，算子落在渲染层（JobSpec.SharedMountsDisabled）。
func disabledSharedMountProfiles(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("AG_SHARED_MOUNTS_DISABLED_PROFILES"), ",") {
		if n := strings.TrimSpace(name); n != "" {
			out[n] = true
		}
	}
	return out
}

// sharedMountsFromStore 返回 D-65 的运行时来源闭包：读 shared_mounts 里启用中的行。
func sharedMountsFromStore(st *store.Store) func(context.Context) ([]domain.SharedMountSpec, error) {
	return func(ctx context.Context) ([]domain.SharedMountSpec, error) {
		return st.SharedMounts.EffectiveSpecs(ctx, nil)
	}
}

// preflightSharedMounts 检查每个统一挂载的 claimName 是否存在（D-63 ④）。
//
// **只告警，不 fail-fast**：存储是集群运维的事，平台只认 PVC 名（§10.4）。
func preflightSharedMounts(ctx context.Context, cfg *config.Config, st *store.Store, logger *slog.Logger) {
	// D-65：DB 是运行时真源；只有表为空时才用 env 种子（预检口径与渲染一致）。
	effective := cfg.SharedMounts
	if st != nil {
		if rows, err := st.SharedMounts.EffectiveSpecs(ctx, nil); err == nil && len(rows) > 0 {
			effective = rows
		}
	}
	if len(effective) == 0 {
		logger.Info("统一共享挂载未配置（D-63/D-65），Runner Job 不注入 /models")
		return
	}
	ns := cfg.Namespace
	client, err := k8s.NewClientset(k8s.Options{Kubeconfig: cfg.Kubeconfig})
	if err != nil {
		logger.Warn("统一共享挂载 preflight 跳过：K8s 客户端不可用", "err", err)
		return
	}
	for _, sm := range effective {
		if _, perr := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, sm.ClaimName, metav1.GetOptions{}); perr != nil {
			logger.Warn("统一共享挂载的 PVC 不存在（继续启动：Pod 会 Pending，由 Runner 报路径不存在）",
				"mountPath", sm.MountPath, "claimName", sm.ClaimName, "namespace", ns, "err", perr)
			continue
		}
		logger.Info("统一共享挂载就绪", "mountPath", sm.MountPath, "claimName", sm.ClaimName, "namespace", ns, "readOnly", sm.ReadOnly)
	}
}
