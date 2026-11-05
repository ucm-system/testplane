// Command testplaned 是 TestPlane 控制面的唯一入口（05 §2.2）。
//
//	--role / TESTPLANE_ROLE = api | controller | collector | all
//
// 装配顺序：config → logger → store(迁移) → notifier → seed → machines → registry
// → executor / genet-mock → orchestrator → report → api(+webui) → 优雅退出。
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

	"github.com/ucm-system/testplane/server/internal/api"
	"github.com/ucm-system/testplane/server/internal/config"
	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/genet/mocks"
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

	logger.Info("testplaned 启动",
		"version", version, "role", cfg.Role, "addr", cfg.HTTPAddr,
		"storage_root", cfg.StorageRoot, "dsn", cfg.RedactedDSN(),
		"max_global_concurrency", cfg.MaxGlobalConcurrency)
	if cfg.AuthMode != "dev" {
		logger.Warn("TESTPLANE_AUTH_MODE=oidc 在 MVP 未实现，暂按 dev 模式运行（无鉴权）")
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

	// ---- Tester 注册表（启动扫描一次，K-3）----
	reg := registry.New(&registryStorage{reg: store.NewTesterRegistry(st)}, registry.Options{
		Logf: func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...), "component", "registry") },
	})
	testersDir := resolveTestersDir(cfg.TestersDir, logger)
	if _, err := reg.Load(ctx, testersDir); err != nil {
		// K-3：注册表有错 → 拒绝启动（只有 controller 角色需要强校验）。
		if cfg.Role.RunsController() {
			return fmt.Errorf("Tester 注册表校验失败（%s）: %w", testersDir, err)
		}
		logger.Warn("Tester 注册表校验失败，api 继续启动", "err", err, "dir", testersDir)
	}
	logger.Info("Tester 注册表就绪", "dir", testersDir, "count", reg.Len())

	// ---- 报告 ----
	reportWriter := &report.Writer{
		Store: st, Root: cfg.StorageRoot,
		MaxReportBytes: 20 << 20, Logger: logger,
	}
	// Run 落终态（事务提交后）异步生成 report.json / report.md / metadata.json / events.jsonl。
	st.SetRunFinishedHook(func(ctx context.Context, runID string) {
		path, err := reportWriter.WriteRunReport(ctx, runID)
		if err != nil {
			logger.Error("报告生成失败（不回滚终态）", "err", err, "run_id", runID)
			return
		}
		if _, err := st.EmitRunEvent(ctx, runID, domain.EventReportWritten,
			map[string]any{"path": path}, "system:report"); err != nil {
			logger.Warn("写 report.written 事件失败", "err", err, "run_id", runID)
		}
	})
	resultValidator := &report.Validator{MaxBytes: cfg.ResultMaxBytes}

	// ---- 执行器 / 部署器（无 K8s 时只跑 api）----
	var (
		controller *orchestrator.Controller
		runErr     chan error
	)
	if cfg.Role.RunsController() {
		ctrl, err := buildController(ctx, cfg, st, machines, reg, resultValidator, logger)
		if err != nil {
			logger.Warn("controller 未启用（K8s 不可用）", "err", err)
		} else {
			controller = ctrl
		}
	}
	if controller != nil {
		runErr = make(chan error, 2)
		go func() {
			logger.Info("controller 循环启动",
				"dispatch", cfg.MaxGlobalConcurrency, "deploy_timeout", cfg.DeployTimeout)
			runErr <- controller.Run(ctx)
		}()
	}

	// ---- HTTP ----
	srv := api.New(api.Deps{
		Config: cfg, Store: st, Machines: machines,
		Resolver: reg, Logger: logger,
		WebUI: webui.FS(), Version: version,
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
	_ = reportWriter
	logger.Info("testplaned 退出")
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

// buildController 装配编排层；没有可用的 K8s 客户端时返回错误（由调用方降级）。
func buildController(ctx context.Context, cfg *config.Config, st *store.Store,
	machines *store.Machines, reg *registry.Registry,
	validator *report.Validator, logger *slog.Logger) (*orchestrator.Controller, error) {

	client, namespace, err := k8s.New(k8s.Options{Kubeconfig: cfg.Kubeconfig})
	if err != nil {
		return nil, err
	}
	exec := executor.NewWithNamespace(client, namespace, executor.Options{})

	repos := &storeRepos{st: st, machines: machines, reg: reg}
	c, err := orchestrator.New(orchestrator.Options{
		Repos:     repos,
		Executor:  exec,
		Deployer:  mocks.FromEnv(), // MVP：Genet mocks（D-14）
		Validator: validator,
		Config: orchestrator.Config{
			Namespace:            namespace,
			StoragePVC:           cfg.StoragePVC,
			StorageRoot:          cfg.StorageRoot,
			MaxGlobalConcurrency: cfg.MaxGlobalConcurrency,
			TaskTimeoutDefault:   cfg.TaskTimeoutDefault,
			DeployTimeout:        cfg.DeployTimeout,
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
		if err := reg.RunRescan(ctx, resolveTestersDir(cfg.TestersDir, logger), 10*time.Minute); err != nil &&
			ctx.Err() == nil {
			logger.Warn("Tester 重扫退出", "err", err)
		}
	}()
	return c, nil
}

// resolveTestersDir 在默认相对路径不存在时向上找仓库里的 testers/（本地 go run 友好）。
func resolveTestersDir(dir string, logger *slog.Logger) string {
	candidates := []string{dir}
	if !filepath.IsAbs(dir) {
		candidates = append(candidates, filepath.Join("..", dir), filepath.Join("..", "..", dir))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if c != dir {
				logger.Info("TestersDir 已回退解析", "configured", dir, "resolved", c)
			}
			return c
		}
	}
	return dir
}

// parseFlags 解析 `--key=value` / `--key value` 形式的参数；未知参数忽略（保持向前兼容）。
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

// runMigrate 执行 `testplaned migrate up`：只跑迁移并退出（Helm hook Job 用）。
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
