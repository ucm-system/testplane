package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// 调度与推进的默认时序（C-4.1、C-5.3、C-6.3、C-3.5）。
const (
	DefaultDispatchInterval  = time.Second
	DefaultDeployInterval    = time.Second
	DefaultWatchdogInterval  = 10 * time.Second
	DefaultReconcileInterval = 60 * time.Second
	// DefaultGCInterval 是 Job/Pod 回收周期（execution §6.3：每 10 分钟）。
	DefaultGCInterval = 10 * time.Minute
	// GCOnceTimeout 是单轮 GC 的硬超时：慢 sweep 不能拖住主循环（票 #20-4）。
	GCOnceTimeout        = 2 * time.Minute
	DefaultMaxCandidates = 50
	DefaultTaskTimeout   = 4 * time.Hour
	DefaultDeployTimeout = 72 * time.Hour
	defaultRunner        = "controller"
	// reconcileBatchLimit 是单轮对账处理的 Task 上限（execution §4.2 的 500 条口径的保守值）。
	reconcileBatchLimit = 200
)

// Config 是编排层配置（由 cmd 从 internal/config 投影）。
type Config struct {
	// Namespace 是 Job 所在 namespace（TESTPLANE_NAMESPACE）。
	Namespace string
	// StoragePVC 是产物 RWX PVC 名（Execution Profile 的 output_pvc）。
	StoragePVC string
	// StorageRoot 是 controller 侧看到的 NFS 挂载点（换算 subPath 用，execution §3.1）。
	StorageRoot string
	// ServiceAccount 为空则不渲染（Tester 不得调 K8s API，§5.10）。
	ServiceAccount string
	// MaxGlobalConcurrency 是全局并发上限（TESTPLANE_MAX_GLOBAL_CONCURRENCY）。
	MaxGlobalConcurrency int
	// TaskTimeoutDefault 用于 Task 没有显式超时时兜底。
	TaskTimeoutDefault time.Duration
	// DeployTimeout 是 Deploy 总窗口（默认 72h，K-6）。
	DeployTimeout time.Duration
	// DispatchInterval / DeployInterval / WatchdogInterval / ReconcileInterval 是各循环节拍。
	// ReconcileInterval 默认 60s：启动时先跑一次，之后周期兜底（C-3.5「重启恢复与对账」）。
	DispatchInterval  time.Duration
	DeployInterval    time.Duration
	WatchdogInterval  time.Duration
	ReconcileInterval time.Duration
	// GCInterval 是 Job/裸 Pod 回收周期（默认 10min，execution §6.3）。
	GCInterval time.Duration
	// JobGCEnabled 关闭回收（nil = 读 TESTPLANE_JOB_GC_ENABLED，默认 true）。
	JobGCEnabled *bool
	// MaxCandidates 是单轮候选上限（C-4.2 LIMIT 50）。
	MaxCandidates int
}

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = "testplane"
	}
	if c.MaxGlobalConcurrency <= 0 {
		c.MaxGlobalConcurrency = 20
	}
	if c.TaskTimeoutDefault <= 0 {
		c.TaskTimeoutDefault = DefaultTaskTimeout
	}
	if c.DeployTimeout <= 0 {
		c.DeployTimeout = DefaultDeployTimeout
	}
	if c.DispatchInterval <= 0 {
		c.DispatchInterval = DefaultDispatchInterval
	}
	if c.DeployInterval <= 0 {
		c.DeployInterval = DefaultDeployInterval
	}
	if c.WatchdogInterval <= 0 {
		c.WatchdogInterval = DefaultWatchdogInterval
	}
	if c.ReconcileInterval <= 0 {
		// 漏配会让 time.NewTicker panic（非正周期），这里兜住并回落到默认 60s（C-3.5）。
		c.ReconcileInterval = DefaultReconcileInterval
	}
	if c.GCInterval <= 0 {
		c.GCInterval = DefaultGCInterval
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = DefaultMaxCandidates
	}
	return c
}

// Options 是 Controller 的构造输入（cmd 装配用）。
//
//	orch := orchestrator.New(orchestrator.Options{
//	    Repos:     adapter,            // backend-core 的 store 适配器（或 orchestrator.NewMemoryRepos()）
//	    Executor:  k8sExecutor,        // internal/executor 的 *Executor
//	    Deployer:  genetMock,          // internal/genet/mocks 的 *Genet
//	    Validator: resultValidator,    // 可空：为空时用 executor 的退出码判定
//	    Config:    orchestrator.Config{...},
//	})
type Options struct {
	Repos     Repos
	Executor  ports.Executor
	Deployer  ports.Deployer
	Validator ports.ResultValidator
	// Prober 是对账用的同步观测能力（*executor.Executor 直接满足）。为空则跳过对账。
	Prober JobProber
	Config Config
	Logf   func(format string, args ...any)
	// Clock 是业务时间来源（05 §6.5）：超窗判定、退避、写库时间戳都走它。
	// 为空时用 domain.SystemClock；单测注入可拨时钟。
	Clock domain.Clock
}

// JobProber 是编排层对执行器的"拉"语义观测能力（executor.Executor.Probe）。
// 单独成接口是为了让单测能注入假实现，也避免对账依赖 Watch 的生命周期。
type JobProber interface {
	Probe(ctx context.Context, h ports.JobHandle) (executor.JobState, error)
}

// Controller 是编排主循环：对账 → 调度 → Submit/Watch → 终态推进 → Deploy/取消/超时。
type Controller struct {
	repos Repos
	// recon 是可选的对了能力（C-3.5）；adapter 未实现时为 nil，对账走 WARN + no-op。
	recon     ReconcileRepos
	exec      ports.Executor
	deployer  ports.Deployer
	validator ports.ResultValidator
	prober    JobProber
	// jobGC 是 Job/裸 Pod 回收开关（TESTPLANE_JOB_GC_ENABLED，默认 true）。
	jobGC bool
	cfg   Config
	logf  func(format string, args ...any)
	// clock 是业务时间来源（05 §6.5），默认 SystemClock。
	clock domain.Clock

	mu      sync.Mutex
	watches map[string]context.CancelFunc // attemptID -> cancel
	wg      sync.WaitGroup
	// logWG 跟踪后台日志落盘，供关闭/测试等待（不阻塞状态推进）。
	logWG sync.WaitGroup
	// gcSem 保证同一时刻只有一轮 GC（容量 1 的信号量，票 #20-4）。
	gcSem chan struct{}
}

// New 构造 Controller。Repos / Executor 必填。
func New(opts Options) (*Controller, error) {
	if opts.Repos == nil {
		return nil, errors.New("orchestrator: Repos is required")
	}
	if opts.Executor == nil {
		return nil, errors.New("orchestrator: Executor is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	prober := opts.Prober
	if prober == nil {
		// 允许 Executor 自己兼作 Prober（*executor.Executor 满足），省掉一处装配。
		if p, ok := opts.Executor.(JobProber); ok {
			prober = p
		}
	}
	if _, ok := opts.Repos.(ReconcileRepos); !ok {
		logf("orchestrator: WARN reconcile disabled: Repos adapter lacks ReconcileRepos (ListActiveTasks/AttemptByTask/DeleteLeaseByTask/ListStaleLeases); stale endpoint_leases will not be reclaimed")
	}
	recon, _ := opts.Repos.(ReconcileRepos)
	// 回收开关：只读配置（config 负责读 TESTPLANE_JOB_GC_ENABLED，默认 true；票 #24-2）。
	// Config.JobGCEnabled 显式给出时优先，否则默认开启。
	jobGC := true
	if opts.Config.JobGCEnabled != nil {
		jobGC = *opts.Config.JobGCEnabled
	}
	return &Controller{
		repos:     opts.Repos,
		recon:     recon,
		exec:      opts.Executor,
		deployer:  opts.Deployer,
		validator: opts.Validator,
		prober:    prober,
		jobGC:     jobGC,
		cfg:       opts.Config.withDefaults(),
		logf:      logf,
		clock:     clockOrDefault(opts.Clock),
		watches:   make(map[string]context.CancelFunc),
	}, nil
}

// clockOrDefault 归一业务时钟：未注入时用 SystemClock。
func clockOrDefault(c domain.Clock) domain.Clock {
	if c != nil {
		return c
	}
	return domain.SystemClock
}

// now 返回业务时间（UTC）。**不要**在编排层直调 time.Now()。
func (c *Controller) now() time.Time {
	if c.clock != nil {
		return c.clock.Now()
	}
	return domain.SystemClock.Now()
}

// Config 返回生效配置（健康检查与测试用）。
func (c *Controller) Config() Config { return c.cfg }

// Run 启动主循环，直到 ctx 取消。
func (c *Controller) Run(ctx context.Context) error {
	dispatch := time.NewTicker(c.cfg.DispatchInterval)
	defer dispatch.Stop()
	deploy := time.NewTicker(c.cfg.DeployInterval)
	defer deploy.Stop()
	watchdog := time.NewTicker(c.cfg.WatchdogInterval)
	defer watchdog.Stop()
	reconcile := time.NewTicker(c.cfg.ReconcileInterval)
	defer reconcile.Stop()
	gcTick := time.NewTicker(c.cfg.GCInterval)
	defer gcTick.Stop()

	c.logf("orchestrator: controller started (ns=%s, concurrency=%d, storage=%s)",
		c.cfg.Namespace, c.cfg.MaxGlobalConcurrency, c.cfg.StoragePVC)

	// 启动即对账：controller 重启后先按 K8s 事实收口"进程死掉的 Attempt"，
	// 否则旧 Task 会永久占着 endpoint_leases（C-3.5）。
	// 成功路径也要打一条 INFO：重启演练的脚本靠它断言"启动即对账"。
	if err := c.ReconcileOnce(ctx); err != nil {
		c.logf("orchestrator: startup reconcile: %v", err)
	} else {
		c.logf("orchestrator: startup reconcile done")
	}
	// 启动也先回收一次：把停机期间累积的已终态 Job / 孤儿 Pod 收掉（execution §6.3）。
	if err := c.GCOnce(ctx); err != nil {
		c.logf("orchestrator: startup gc: %v", err)
	} else {
		c.logf("orchestrator: startup gc done")
	}

	for {
		select {
		case <-ctx.Done():
			c.stopWatches()
			c.wg.Wait()
			return ctx.Err()
		case <-dispatch.C:
			if err := c.DispatchOnce(ctx); err != nil {
				c.logf("orchestrator: dispatch: %v", err)
			}
		case <-deploy.C:
			if err := c.DeployOnce(ctx); err != nil {
				c.logf("orchestrator: deploy: %v", err)
			}
		case <-watchdog.C:
			if err := c.WatchdogOnce(ctx); err != nil {
				c.logf("orchestrator: watchdog: %v", err)
			}
		case <-reconcile.C:
			if err := c.ReconcileOnce(ctx); err != nil {
				c.logf("orchestrator: reconcile: %v", err)
			}
		case <-gcTick.C:
			// execution §6.3：每 10 分钟回收已终态的 Job 与孤儿 Pod。
			// 必须**异步 + 超时**：一次慢 sweep 不能停掉 dispatch/watchdog/deploy（票 #20-4）。
			c.gcInBackground(ctx)
		}
	}
}

// ReconcileOnce 是一次对账（C-3.5「重启恢复与对账」，最小可用版）：
//
//  1. 枚举 `preparing|running` 的 Task —— 它们在库里"活着"，但执行它的那个进程可能早已退出；
//  2. 用 `attempts.executor_ref.job` 去 K8s 查 Job：
//     - Job 不存在 → 任务失联，按 `EXECUTOR_LOST` 收口（不可重试）；
//     - Job 已终态 → 用观测到的退出码/错误码走 `FinishAttempt`（成功也要过结果校验）；
//     - Job 仍在跑 → 不动，交给 Watch；
//  3. 兜底清理：Task 已终态但 `endpoint_leases` 仍有行 → 删租约（幂等）。
//
// 关键点：**每一条收口路径都必须释放租约**，否则同 endpoint 的后续任务会永远
// `waiting_reason=slot_waiting`（本次 P0 缺陷的根因）。
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	// 收口补偿先做：它只依赖 Repos + Deployer，不该被 prober/recon 的可用性挡住
	// （否则"平台重启后没人卸载 release"永远修不好，§9.7）。
	c.cleanupPendingDeploys(ctx)
	if c.prober == nil {
		return nil
	}
	if c.recon == nil {
		return nil
	}
	tasks, err := c.recon.ListActiveTasks(ctx, reconcileBatchLimit)
	if err != nil {
		return fmt.Errorf("list active tasks: %w", err)
	}
	for _, task := range tasks {
		if err := c.reconcileTask(ctx, task); err != nil {
			c.logf("orchestrator: reconcile task %s: %v", task.ID, err)
		}
	}
	// 兜底 1：cancelling 但已经没有活动 Attempt 的 Run → 立即 canceled（C-6.1、C-9?）。
	// 这条能自愈"queued 阶段取消、Job 从未创建"而卡在 cancelling 的历史遗留行。
	cancelling, err := c.repos.ListCancellingRuns(ctx, reconcileBatchLimit)
	if err != nil {
		c.logf("orchestrator: list cancelling runs: %v", err)
	} else {
		for _, run := range cancelling {
			stopped, serr := c.runStopped(ctx, run.ID)
			if serr != nil {
				c.logf("orchestrator: reconcile cancelling run %s: %v", run.ID, serr)
				continue
			}
			// 取消补偿：deploy 的卸载可能因 release 未出现而 404，这里每轮重试一次（§9.6）。
			c.cleanupDeploy(ctx, run.ID)
			if !stopped {
				continue
			}
			if ferr := c.repos.FinishRun(ctx, run.ID, domain.RunCanceled, nil); ferr != nil {
				c.logf("orchestrator: finish canceled run %s: %v", run.ID, ferr)
				continue
			}
			c.logf("orchestrator: run %s reconciled as canceled (no active attempt)", run.ID)
		}
	}

	// 兜底 1.5：已终态但 deploy 还没卸载干净的 Run（正常收口 + 重启/抖动后的补偿，§9.7）。
	c.cleanupPendingDeploys(ctx)

	// 兜底 2：历史遗留的僵尸租约（Task 已终态/已不存在，租约还在）。
	stale, err := c.recon.ListStaleLeases(ctx, reconcileBatchLimit)
	if err != nil {
		c.logf("orchestrator: list stale leases: %v", err)
		return nil
	}
	for _, taskID := range stale {
		if err := c.recon.DeleteLeaseByTask(ctx, taskID); err != nil {
			c.logf("orchestrator: release stale lease %s: %v", taskID, err)
			continue
		}
		c.logf("orchestrator: released stale endpoint lease held by task %s", taskID)
	}
	return nil
}

// reconcileTask 对账单个活跃 Task。
func (c *Controller) reconcileTask(ctx context.Context, task TaskView) error {
	attempt, err := c.recon.AttemptByTask(ctx, task.ID)
	if err != nil {
		// 库里没有进行中的 Attempt（事务中断/旧版本遗留）：Task 卡在 preparing/running
		// 但没有可观测对象，直接按 EXECUTOR_LOST 收口并释放租约。
		if errors.Is(err, domain.ErrNotFound) {
			return c.finishLostTask(ctx, task, AttemptRef{TaskID: task.ID, RunID: task.RunID})
		}
		return err
	}
	if attempt.Job() == "" {
		return c.finishLostTask(ctx, task, attempt)
	}

	state, err := c.prober.Probe(ctx, ports.JobHandle{Namespace: c.cfg.Namespace, JobName: attempt.Job()})
	if err != nil {
		return fmt.Errorf("probe %s: %w", attempt.Job(), err)
	}
	switch {
	case !state.Exists:
		// Job 已不存在（被 GC、人工删除、或从未创建成功）→ 任务失联。
		return c.finishLostTask(ctx, task, attempt)
	case !state.Terminal:
		// 仍在跑：不动，等 Watch。
		return nil
	}
	return c.finishReconciledAttempt(ctx, task, attempt, state.Outcome)
}

// finishLostTask 把失联的 Task 收口为 failed(EXECUTOR_UNAVAILABLE)，并释放租约。
// 附录 D 是闭合枚举（§4.5 不新增码），因此复用"执行层不可用/不可观测"的既有语义，
// 用 detail.reason=executor_lost 区分具体原因，retryable=false（不是 Tester 的错，重试无意义）。
func (c *Controller) finishLostTask(ctx context.Context, task TaskView, attempt AttemptRef) error {
	failure := &domain.Failure{
		Code:      domain.CodeExecutorUnavailable,
		Message:   "executor job is gone; the attempt has been reconciled as lost",
		Retryable: false,
		Detail: map[string]any{
			"reason":  "executor_lost",
			"task_id": task.ID,
			"job":     attempt.Job(),
		},
	}
	if task.Failure != nil {
		failure = task.Failure
	}
	if attempt.AttemptID == "" {
		// 没有 Attempt 可收口：直接删租约，避免僵尸占位。
		if err := c.recon.DeleteLeaseByTask(ctx, task.ID); err != nil {
			return err
		}
		c.logf("orchestrator: task %s had no active attempt; released its lease", task.ID)
		return nil
	}
	_, err := c.repos.FinishAttempt(ctx, FinishInput{
		AttemptID:  attempt.AttemptID,
		TaskID:     task.ID,
		RunID:      task.RunID,
		Phase:      "failed",
		Message:    failure.Message,
		Failure:    failure,
		FinishedAt: c.now(),
	})
	if err != nil {
		return err
	}
	c.logf("orchestrator: reconciled lost attempt %s (job %s) -> failed(%s)",
		attempt.AttemptID, attempt.Job(), failure.Code)
	// 双保险：即使 FinishAttempt 的实现忘了释放，这里也幂等删一次。
	return c.recon.DeleteLeaseByTask(ctx, task.ID)
}

// finishReconciledAttempt 用 K8s 的终态事实收口 Attempt（与 Watch 回调共用 resolveAttemptResult）。
func (c *Controller) finishReconciledAttempt(ctx context.Context, task TaskView, attempt AttemptRef, o ports.JobOutcome) error {
	phase, failure := c.resolveAttemptResult(ctx, attempt, o)
	if failure != nil && failure.Detail == nil {
		failure.Detail = map[string]any{}
	}
	if failure != nil {
		failure.Detail["reconciled"] = true
	}

	out, err := c.repos.FinishAttempt(ctx, FinishInput{
		AttemptID:   attempt.AttemptID,
		TaskID:      task.ID,
		RunID:       task.RunID,
		Phase:       phase,
		ExitCode:    o.ExitCode,
		Message:     o.Message,
		ImageDigest: o.ImageDigest,
		Failure:     failure,
		FinishedAt:  o.FinishedAt,
	})
	if err != nil {
		return err
	}
	for _, ref := range out.CancelJobs {
		c.cancelJobAsync(ctx, ref)
	}
	if out.NeedsQueueUnblocked {
		if _, err := c.repos.QueueTasks(ctx, attempt.RunID, false); err != nil {
			c.logf("orchestrator: queue after reconciled precheck: %v", err)
		}
	}
	c.logf("orchestrator: reconciled attempt %s (job %s) -> %s", attempt.AttemptID, attempt.Job(), phase)
	c.finalizeAttemptAsync(attempt)
	return c.recon.DeleteLeaseByTask(ctx, task.ID)
}

// JobGC 是可选的回收能力（*executor.Executor 满足）。
// 单独成接口：无 K8s 的执行器实现（测试用 fake）可以没有它，回收自然跳过。
//
// 注意：参数必须用 `executor.AttemptStateFunc` 这个**命名类型**，不能用匿名的 func 字面量——
// 命名类型与同名匿名类型在接口方法签名上并不相同，写成匿名 func 会导致 `*executor.Executor`
// 不满足本接口，GC 被静默跳过（本仓库踩过一次，见 IMPLEMENTATION §15.3 #13）。
type JobGC interface {
	CollectGarbage(ctx context.Context, opts executor.GCOptions) (executor.GCReport, error)
}

// 编译期断言：真实 Executor 必须实现 JobGC（票 #20-6：PR 描述声称有、实际没有，
// 导致 GC 曾经静默地一次都没跑过）。改 CollectGarbage 签名时这里会先挂。
var _ JobGC = (*executor.Executor)(nil)

// GCOnce 回收"已结束但没人删"的 Job 与孤儿 Pod（execution §6.3）。
//
// 判据见 executor.CollectGarbage：hold-until 守卫 / 孤儿立即删 / 终态且 >1h / 运行中 >25h。
func (c *Controller) GCOnce(ctx context.Context) error {
	if !c.jobGC {
		return nil
	}
	gc, ok := c.exec.(JobGC)
	if !ok {
		return nil
	}
	sweepCtx, cancel := context.WithTimeout(ctx, GCOnceTimeout)
	defer cancel()
	report, err := gc.CollectGarbage(sweepCtx, executor.GCOptions{
		AttemptStates:  c.attemptStatesByJobNames,
		OnStaleRunning: c.repairStaleRunning,
	})
	if err != nil {
		return err
	}
	if report.DeletedJobs > 0 || report.DeletedPods > 0 || len(report.Errors) > 0 {
		c.logf("orchestrator: job gc: deleted_jobs=%d deleted_pods=%d kept=%d errors=%v",
			report.DeletedJobs, report.DeletedPods, report.KeptJobs, report.Errors)
	}
	return nil
}

// gcInBackground 把一轮 GC 放到独立 goroutine（带超时、不重叠），慢 sweep 不阻塞主循环。
func (c *Controller) gcInBackground(ctx context.Context) {
	select {
	case c.gcSem <- struct{}{}:
	default:
		return // 上一轮还没结束：跳过本轮，避免叠加
	}
	go func() {
		defer func() { <-c.gcSem }()
		if err := c.GCOnce(ctx); err != nil && ctx.Err() == nil {
			c.logf("orchestrator: gc: %v", err)
		}
	}()
}

// attemptStatesByJobNames 批量把 Job 名映射成 Attempt 状态（一次查库，票 #20-5）。
func (c *Controller) attemptStatesByJobNames(ctx context.Context, jobNames []string) (map[string]executor.AttemptState, error) {
	if len(jobNames) == 0 {
		return map[string]executor.AttemptState{}, nil
	}
	return c.repos.AttemptStatesByJobNames(ctx, jobNames)
}

// repairStaleRunning 处理"运行中但超过 25h"的 Job：把 Attempt 收口成 FAILED(TASK_TIMEOUT)（§6.3）。
//
// 这里只修库里的状态；Job 的删除由 GC 在回调返回成功之后做。
func (c *Controller) repairStaleRunning(ctx context.Context, job executor.ManagedJob) error {
	ref, err := c.repos.AttemptByJobName(ctx, job.Name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil // 没有 Attempt 可修：交给孤儿规则删
		}
		return err
	}
	f := &domain.Failure{
		Code:      domain.CodeTaskTimeout,
		Message:   fmt.Sprintf("job %s exceeded %s without a terminal condition", job.Name, executor.GCStaleRunningAge),
		Retryable: false,
		Detail:    map[string]any{"reason": "stale_running", "job": job.Name},
	}
	_, err = c.repos.FinishAttempt(ctx, FinishInput{
		AttemptID:  ref.AttemptID,
		TaskID:     ref.TaskID,
		RunID:      ref.RunID,
		Phase:      "failed",
		Message:    f.Message,
		Failure:    f,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil && !errors.Is(err, domain.ErrConflict) {
		return fmt.Errorf("repair stale attempt %s: %w", ref.AttemptID, err)
	}
	c.logf("orchestrator: job gc: repaired stale running job %s as TASK_TIMEOUT", job.Name)
	return nil
}

// DispatchOnce 是一轮调度（C-4.1）：取候选 → 取 permit → 建 Job → 起观测。
func (c *Controller) DispatchOnce(ctx context.Context) error {
	candidates, err := c.repos.Candidates(ctx, c.cfg.MaxCandidates)
	if err != nil {
		return fmt.Errorf("candidates: %w", err)
	}
	for _, cand := range candidates {
		if err := c.dispatchOne(ctx, cand); err != nil {
			c.logf("orchestrator: dispatch task %s: %v", cand.TaskID, err)
		}
	}
	return nil
}

func (c *Controller) dispatchOne(ctx context.Context, cand Candidate) error {
	task, err := c.repos.GetTask(ctx, cand.TaskID)
	if err != nil {
		return err
	}
	run, err := c.repos.GetRun(ctx, cand.RunID)
	if err != nil {
		return err
	}
	attemptNo := task.AttemptNo + 1
	jobName := executor.Naming(run.ID, task.ID, attemptNo)
	ref, err := json.Marshal(map[string]string{
		"cluster":   "default",
		"namespace": c.cfg.Namespace,
		"job":       jobName,
	})
	if err != nil {
		return err
	}

	claimed, isClaimed, reason, err := c.repos.ClaimForExecution(ctx, ClaimRequest{
		TaskID:       task.ID,
		EndpointKey:  cand.EndpointKey,
		Exclusive:    cand.Exclusive,
		GlobalLimit:  c.cfg.MaxGlobalConcurrency,
		AttemptIdx:   attemptNo,
		JobID:        jobName,
		JobNamespace: c.cfg.Namespace,
		ExecutorRef:  ref,
	})
	if err != nil {
		return err
	}
	if !isClaimed {
		if reason != "" {
			return c.repos.SetWaitingReason(ctx, task.ID, reason)
		}
		return nil
	}
	attempt := claimed.Attempt(task.ID, run.ID)

	spec, err := c.jobSpec(run, task, attempt)
	if err != nil {
		return c.failAttempt(ctx, attempt, &domain.Failure{
			Code:      domain.CodeInternalError,
			Message:   err.Error(),
			Retryable: false,
		})
	}

	handle, err := c.exec.Submit(ctx, spec)
	if err != nil {
		code := domain.CodeExecutorUnavailable
		if errors.Is(err, executor.ErrSpecConflict) {
			code = domain.CodeConflict
		}
		// 创建失败：Task 直接 failed，不建 Watch（T6）。
		return c.failAttempt(ctx, attempt, &domain.Failure{
			Code:      code,
			Message:   err.Error(),
			Retryable: code == domain.CodeExecutorUnavailable,
		})
	}

	if err := c.repos.MarkAttemptRunning(ctx, attempt.AttemptID, handle); err != nil {
		return err
	}
	c.startWatch(ctx, attempt, handle)
	c.logf("orchestrator: task %s attempt %d submitted as job %s", task.ID, attemptNo, handle.JobName)
	return nil
}

// jobSpec 把 TaskView + RunView 投影成 ports.JobSpec（execution §2.1 的输入）。
func (c *Controller) jobSpec(run RunView, task TaskView, attempt AttemptRef) (ports.JobSpec, error) {
	timeout := task.TimeoutSeconds
	if timeout <= 0 {
		timeout = int(c.cfg.TaskTimeoutDefault / time.Second)
	}
	subPath, err := executor.OutputSubPath(run.ID, task.ID, attempt.AttemptNo)
	if err != nil {
		return ports.JobSpec{}, err
	}
	// 存储路径一律用 POSIX 形态（它描述的是 PVC 内的相对路径，不是宿主 OS 的路径）。
	outputDir := subPath
	if root := strings.TrimRight(strings.ReplaceAll(c.cfg.StorageRoot, "\\", "/"), "/"); root != "" {
		outputDir = root + "/" + subPath
	}
	return ports.JobSpec{
		RunID:          run.ID,
		TaskID:         task.ID,
		AttemptID:      attempt.AttemptID,
		AttemptNo:      attempt.AttemptNo,
		TesterName:     task.TesterName,
		TesterVersion:  task.TesterVersion,
		TesterRole:     task.TesterRole,
		Image:          task.Image,
		Entry:          task.Entry,
		Args:           append([]string(nil), task.Args...),
		Env:            task.Env,
		Namespace:      c.cfg.Namespace,
		ServiceAccount: c.cfg.ServiceAccount,
		Resources:      task.Resources,
		Mounts:         task.Mounts,
		OutputDir:      outputDir,
		StoragePVC:     c.cfg.StoragePVC,
		TimeoutSeconds: timeout,
		SecretEnv:      append([]string(nil), task.SecretEnv...),
	}, nil
}

// failAttempt 把"建 Job 之前就失败"的 Attempt 落成终态（T6）。
func (c *Controller) failAttempt(ctx context.Context, attempt AttemptRef, f *domain.Failure) error {
	_, err := c.repos.FinishAttempt(ctx, FinishInput{
		AttemptID:  attempt.AttemptID,
		TaskID:     attempt.TaskID,
		RunID:      attempt.RunID,
		Phase:      "failed",
		Message:    f.Message,
		Failure:    f,
		FinishedAt: c.now(),
	})
	return err
}

// startWatch 起一个 goroutine 观测 Job，终态回调 JobFinished（execution §4）。
func (c *Controller) startWatch(parent context.Context, attempt AttemptRef, h ports.JobHandle) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if prev, ok := c.watches[attempt.AttemptID]; ok {
		prev()
	}
	c.watches[attempt.AttemptID] = cancel
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.watches, attempt.AttemptID)
			c.mu.Unlock()
			cancel()
		}()
		if err := c.exec.Watch(ctx, h, c); err != nil && !errors.Is(err, context.Canceled) {
			c.logf("orchestrator: watch %s: %v", h.JobName, err)
		}
	}()
}

func (c *Controller) stopWatches() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.watches {
		cancel()
	}
	c.watches = make(map[string]context.CancelFunc)
}

// JobFinished 实现 ports.JobObserver：终态落库 → 结果校验 → 推进（C-5.4、execution §8.1）。
func (c *Controller) JobFinished(ctx context.Context, o ports.JobOutcome) error {
	attempt, err := c.repos.AttemptByJobName(ctx, o.Handle.JobName)
	if err != nil {
		return err
	}
	task, err := c.repos.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return err
	}

	// 与对账共用同一个入口：无论退出码是否为 0，只要产物目录可读就先读 result.json
	// （C-7.1 第 4 条：非零退出用 error.code + 实例级 retryable 归类）。
	phase, failure := c.resolveAttemptResult(ctx, attempt, o)

	out, err := c.repos.FinishAttempt(ctx, FinishInput{
		AttemptID:   attempt.AttemptID,
		TaskID:      attempt.TaskID,
		RunID:       attempt.RunID,
		Phase:       phase,
		ExitCode:    o.ExitCode,
		Message:     o.Message,
		ImageDigest: o.ImageDigest,
		Failure:     failure,
		FinishedAt:  o.FinishedAt,
	})
	if err != nil {
		return err
	}

	// abort_run / 取消后的删 Job 是异步的（C-5.5：不阻塞状态推进）。
	for _, ref := range out.CancelJobs {
		c.cancelJobAsync(ctx, ref)
	}
	// Pre-Check 通过后放行其余 Task（C-5.4 第 3 条）。
	if out.NeedsQueueUnblocked {
		if _, err := c.repos.QueueTasks(ctx, attempt.RunID, false); err != nil {
			c.logf("orchestrator: queue after precheck: %v", err)
		}
	}
	if out.RunAllTerminal {
		c.logf("orchestrator: run %s reached terminal state", attempt.RunID)
		// Run 终态**已落库**（FinishAttempt 的事务已提交），这里做事务外的卸载：
		// 秒级收口；失败只记 uninstall.state=failed，由 ReconcileOnce 兜底（§9.7）。
		c.cleanupDeployIfTerminal(ctx, attempt.RunID)
	}
	// Attempt 收口后把 Pod 日志落盘（best effort，失败只 WARN）。
	c.finalizeAttemptAsync(attempt)
	_ = task
	return nil
}

// ExitResultValidator 是 ports.ResultValidator 的**可选扩展**：带退出码的校验。
// report.Validator 已经实现了 ValidateExit（C-7.1 第 4 条：非零退出时读 result.error.code），
// 编排层用类型断言接上；老实现只有 Validate 时退化为"只判成功路径"。
type ExitResultValidator interface {
	ValidateExit(ctx context.Context, runID, taskID, attemptID, outputDir string, exitCode *int32) *domain.Failure
}

// attemptDir 返回 controller 侧可见的 Attempt 产物目录（PVC 的 <run>/tasks/<task>/attempts/<n>）。
// 不可见时返回 ok=false。这是"平台没挂存储"与"Tester 真没写产物"的分界线：
// 前者只 WARN（result_dir_missing），绝不判 RESULT_MISSING 冤枉测试。
func (c *Controller) attemptDir(attempt AttemptRef, warn bool) (string, bool) {
	if c.cfg.StorageRoot == "" {
		if warn {
			c.logf("orchestrator: WARN result_dir_missing: storage root is not configured; attempt %s", attempt.AttemptID)
		}
		return "", false
	}
	dir := filepath.Join(c.cfg.StorageRoot, filepath.FromSlash(mustSubPath(attempt)))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		if warn {
			c.logf("orchestrator: WARN result_dir_missing: attempt dir %q is not visible from the controller; attempt %s",
				dir, attempt.AttemptID)
		}
		return "", false
	}
	return dir, true
}

// resolveAttemptResult 把 Job 终态翻译成 Attempt 结论（Watch 回调与对账**共用**这一入口）。
//
// 规则（execution §8.1、control-plane §C-7.1 第 4 条）：
//   - 退出码 0：跑产物校验，不过 → failed(该码)；
//   - 退出码非 0：**先读 result.json**，用 error.code + 实例级 retryable + detail 归类
//     （Pre-Check 靠它区分"没就绪（可重试）"与"认证/4xx（不可重试）"）；
//     读不到/不合法才回退 TESTER_NONZERO_EXIT（retryable=false）；
//   - 超时：平台权威判 TASK_TIMEOUT（§5.1），不拿 result.json 改判；
//   - 存储不可见：不猜产物，保留执行器给出的码（成功路径保留 succeeded）。
func (c *Controller) resolveAttemptResult(ctx context.Context, attempt AttemptRef, o ports.JobOutcome) (string, *domain.Failure) {
	switch o.Phase {
	case executor.PhaseSucceeded:
		dir, ok := c.attemptDir(attempt, true)
		if !ok || c.validator == nil {
			return executor.PhaseSucceeded, nil
		}
		if f := c.validateResult(ctx, attempt, dir, o.ExitCode); f != nil {
			return executor.PhaseFailed, f
		}
		return executor.PhaseSucceeded, nil
	case executor.PhaseTimeout:
		// 实例字段显式给 true：C-5.8 第 4 条把 TASK_TIMEOUT 列为"可重试的 Pre-Check 失败"，
		// 终态判定只认实例字段（码级默认值由这里兜底填写）。
		return executor.PhaseTimeout, &domain.Failure{
			Code:      domain.CodeTaskTimeout,
			Message:   o.Message,
			Retryable: true,
			Detail:    map[string]any{"phase": o.Phase, "task_id": attempt.TaskID, "attempt_id": attempt.AttemptID},
		}
	}

	// 失败路径：先尝试从 result.json 归类（C-7.1 第 4 条 / execution §8.1）。
	// 只有"读不到或不合法"才回退——校验器返回 RESULT_MISSING/RESULT_INVALID 属于
	// "没有可用的归类依据"，不是最终结论，因此不在这里采信。
	if o.ExitCode != nil && *o.ExitCode != 0 {
		if dir, ok := c.attemptDir(attempt, false); ok && c.validator != nil {
			if f := c.validateResult(ctx, attempt, dir, o.ExitCode); f != nil && !unclassifiable(f) {
				return executor.PhaseFailed, f
			}
		}
	}
	code := executor.OutcomeCode(o)
	if code == "" {
		code = domain.CodeInternalError
	}
	failure := &domain.Failure{
		Code:      code,
		Message:   o.Message,
		Retryable: domain.Retryable(code),
		Detail:    map[string]any{"phase": o.Phase},
	}
	if o.ExitCode != nil {
		failure.Detail["exit_code"] = *o.ExitCode
	}
	return executor.PhaseFailed, failure
}

// unclassifiable 报告"校验器没有给出可用的失败归类"：非零退出时这两种码只说明
// result.json 缺失或不可解析，按 execution §8.1 应回退 TESTER_NONZERO_EXIT。
func unclassifiable(f *domain.Failure) bool {
	if f == nil {
		return true
	}
	return f.Code == domain.CodeResultMissing || f.Code == domain.CodeResultInvalid
}

// validateResult 调校验器：优先带退出码的 ValidateExit，老实现退化为 Validate。
func (c *Controller) validateResult(ctx context.Context, attempt AttemptRef, dir string, exitCode *int32) *domain.Failure {
	if v, ok := c.validator.(ExitResultValidator); ok {
		return v.ValidateExit(ctx, attempt.RunID, attempt.TaskID, attempt.AttemptID, dir, exitCode)
	}
	if exitCode != nil && *exitCode != 0 {
		// 老实现读不了失败归类，交给调用方回退（不让它冒充成功/失败结论）。
		return nil
	}
	return c.validator.Validate(ctx, attempt.RunID, attempt.TaskID, attempt.AttemptID, dir)
}

// 日志落盘参数：设计 §7.1 的分段/游标由 collector 负责；MVP 只做"结束时把 Pod 日志
// 整体落到 <attempt>/logs/stdout.log"，供 API 的 /tasks/{id}/attempts/{idx}/logs 直接读文件。
const (
	// AttemptLogFileName 是落盘的目标文件（API 侧直接读它）。
	AttemptLogFileName = "stdout.log"
	// attemptLogTimeout 是日志落盘的硬上限：不阻塞状态推进，超时就放弃并 WARN。
	attemptLogTimeout = 2 * time.Minute
)

// WaitLogCapture 等所有后台日志落盘结束（测试与优雅关闭用，不参与主循环）。
func (c *Controller) WaitLogCapture() { c.logWG.Wait() }

// finalizeAttemptAsync 在 Attempt 终态**落库之后**做两件收尾（execution §5.1 的顺序）：
//  1. 把 Pod 日志落盘（必须在删 Job 之前，否则日志跟着 Pod 一起没了）；
//  2. Background 删除 Job（§6：正常结束 → 删 Job；Pod 由 Job controller 级联回收）。
//
// 全程 best effort：失败只 WARN，绝不改状态（多留一个 Job 比丢一份结论便宜）。
func (c *Controller) finalizeAttemptAsync(attempt AttemptRef) {
	h := ports.JobHandle{Namespace: c.cfg.Namespace, JobName: attempt.Job()}
	c.logWG.Add(1)
	go func() {
		defer c.logWG.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), attemptLogTimeout)
		defer cancel()
		if c.cfg.StorageRoot != "" {
			if err := c.writeAttemptLogs(ctx, attempt); err != nil {
				// 写失败只 WARN（execution §7 的"允许丢日志，不允许伪造连续"）。
				c.logf("orchestrator: WARN attempt_logs_failed: attempt %s: %v", attempt.AttemptID, err)
			}
		}
		// hold-until 守卫：日志 follower 还没收尾时不能删 Job（execution §6.3），留给 GC 下一轮。
		if c.jobHeld(ctx, h) {
			c.logf("orchestrator: job %s held by %s; skip delete (gc will retry)", h.JobName, executor.HoldUntilAnnotation)
		} else if err := c.exec.Delete(ctx, h, ports.DeleteBackground); err != nil {
			c.logf("orchestrator: WARN job_delete_failed: attempt %s job %s: %v", attempt.AttemptID, h.JobName, err)
		}
	}()
}

// writeAttemptLogs 把容器 stdout/stderr 流写到 <attempt>/logs/stdout.log（temp + rename 原子落盘）。
// 从 controller 侧看，Attempt 目录就是 StorageRoot 下 PVC 的 subPath。
func (c *Controller) writeAttemptLogs(ctx context.Context, attempt AttemptRef) error {
	// 目录可能还没建（对账场景）：先 mkdir，再复用同一个路径。
	dir := filepath.Join(c.cfg.StorageRoot, filepath.FromSlash(mustSubPath(attempt)))
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o770); err != nil {
		return fmt.Errorf("create logs dir: %w", err)
	}

	h := ports.JobHandle{Namespace: c.cfg.Namespace, JobName: attempt.Job()}
	rc, err := c.exec.Logs(ctx, h)
	if err != nil {
		return fmt.Errorf("open pod logs: %w", err)
	}
	if rc == nil {
		return fmt.Errorf("open pod logs: executor returned a nil stream")
	}
	defer rc.Close()

	tmpPath := filepath.Join(logsDir, AttemptLogFileName+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpPath, err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy pod logs: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	finalPath := filepath.Join(logsDir, AttemptLogFileName)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename logs: %w", err)
	}
	return nil
}

func mustSubPath(a AttemptRef) string {
	sub, _ := executor.OutputSubPath(a.RunID, a.TaskID, a.AttemptNo)
	return sub
}

func (c *Controller) cancelJobAsync(parent context.Context, ref AttemptRef) {
	go func() {
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		h := ports.JobHandle{Namespace: c.cfg.Namespace, JobName: executor.Naming(ref.RunID, ref.TaskID, ref.AttemptNo)}
		if err := c.exec.Cancel(ctx, h); err != nil {
			c.logf("orchestrator: cancel job %s: %v", h.JobName, err)
			// 兜底：Cancel 里的等待可能超时，这里用 Foreground 再删一次（等 Pod 真正消失，
			// 避免"取消后仍有进程写 NFS"，execution §5.2）。
			if derr := c.exec.Delete(ctx, h, ports.DeleteForeground); derr != nil {
				c.logf("orchestrator: WARN cancel_job_foreground_delete %s: %v", h.JobName, derr)
			}
		}
		if err := c.repos.MarkCanceled(ctx, ref.AttemptID); err != nil {
			c.logf("orchestrator: mark canceled %s: %v", ref.AttemptID, err)
		}
	}()
}

// DeployOnce 推进 queued（无 deploy → testing）与 deploying（轮询 Genet）（C-5.3）。
func (c *Controller) DeployOnce(ctx context.Context) error {
	queued, err := c.repos.ListRunsQueued(ctx, c.cfg.MaxCandidates)
	if err != nil {
		return err
	}
	for _, run := range queued {
		if err := c.beginRun(ctx, run); err != nil {
			c.logf("orchestrator: begin run %s: %v", run.ID, err)
		}
	}

	if c.deployer == nil {
		return nil
	}
	deploying, err := c.repos.ListRunsInDeploy(ctx, c.cfg.MaxCandidates)
	if err != nil {
		return err
	}
	for _, run := range deploying {
		if err := c.pollDeploy(ctx, run); err != nil {
			c.logf("orchestrator: poll deploy %s: %v", run.ID, err)
		}
	}
	return nil
}

// beginRun 处理 queued 的 Run：有 deploy 走 deploying，否则直接 testing。
func (c *Controller) beginRun(ctx context.Context, run RunView) error {
	if run.HasDeploy {
		if c.deployer == nil {
			return c.repos.FinishDeployAsFailed(ctx, run.ID, &domain.Failure{
				Code:      domain.CodeDeployFailed,
				Message:   "deployer is not configured",
				Retryable: false,
			})
		}
		// 先 BeginDeploy 把 runs.deploy 冻结落库（含 release_name / accepted_at），
		// 再调 Start。Genet 的 POST 超时不代表没创建，重启后只能靠库里的冻结值找回 release（§9.2）。
		if err := c.repos.BeginDeploy(ctx, run.ID, "d-"+run.ID); err != nil {
			return err
		}
		// 冻结信息落库：这些键之后每次轮询都只做增量合并，不会被冲掉（§4.4）。
		now := c.now()
		base := DeployManifestFields(run.DeployJSON)
		snap := domain.DeploySnapshot{
			DeployID:     "d-" + run.ID,
			Provider:     firstNonEmpty(base.Provider, "genet"),
			Namespace:    base.Namespace,
			ReleaseName:  DeployReleaseName(run.ID, "d-"+run.ID),
			PackageType:  firstNonEmpty(base.PackageType, "stack"),
			Chart:        base.Chart,
			ChartVersion: base.ChartVersion,
			ValuesDigest: base.ValuesDigest,
			ModelName:    base.ModelName,
			State:        "accepted",
			AcceptedAt:   &now,
			QueuedAt:     &now,
			LastPolledAt: &now,
		}
		if err := c.repos.RecordDeployState(ctx, run.ID, snap); err != nil {
			return err
		}
		// 事务外调用 Genet（C-2.5）；Manifest = 冻结后的 runs.deploy 原样快照。
		if err := c.deployer.Start(ctx, ports.DeployRequest{
			RunID:    run.ID,
			DeployID: "d-" + run.ID,
			Manifest: run.DeployJSON,
			Timeout:  c.cfg.DeployTimeout,
		}); err != nil {
			// 平台侧调不通 Genet（网络/5xx/鉴权/请求构造）→ DEPLOY_FAILED（不可重试，Run failed，D-47）。
			f := &domain.Failure{Code: domain.CodeDeployFailed, Message: err.Error(), Retryable: false}
			_ = c.repos.RecordDeployState(ctx, run.ID, domain.DeploySnapshot{State: "blocked", Failure: f})
			return c.repos.FinishDeployAsFailed(ctx, run.ID, f)
		}
		return nil
	}
	if _, err := c.repos.BeginTesting(ctx, run.ID); err != nil {
		return err
	}
	return c.queueAfterTesting(ctx, run.ID)
}

func (c *Controller) pollDeploy(ctx context.Context, run RunView) error {
	now := c.now()
	// 总窗口判定（K-6）：排队 + 部署超窗 → service_unavailable，不是 failed。
	if !run.SubmittedAt.IsZero() && now.Sub(run.SubmittedAt) > c.cfg.DeployTimeout {
		f := &domain.Failure{
			Code:      domain.CodeDeployBlocked,
			Message:   fmt.Sprintf("deploy did not become ready within %s", c.cfg.DeployTimeout),
			Retryable: true,
			Detail:    map[string]any{"reason": "timeout"},
		}
		_ = c.repos.RecordDeployState(ctx, run.ID, domain.DeploySnapshot{
			State: "blocked", QueueReason: "timeout", Failure: f, LastPolledAt: &now,
		})
		return c.repos.DeployBlocked(ctx, run.ID, "deploy window exceeded", f)
	}
	st, err := c.deployer.Poll(ctx, run.ID)
	if err != nil {
		// 平台调不通（网络/5xx/鉴权）→ DEPLOY_FAILED（不可重试）→ Run failed（C-5.3、D-47）。
		f := &domain.Failure{Code: domain.CodeDeployFailed, Message: err.Error(), Retryable: false}
		_ = c.repos.RecordDeployState(ctx, run.ID, domain.DeploySnapshot{
			State: "blocked", Failure: f, LastPolledAt: &now,
		})
		return c.repos.FinishDeployAsFailed(ctx, run.ID, f)
	}
	// 每次 Poll 回写：状态、两根轴原值、队列水位、时间线（§4.4）。
	//
	// Namespace 从 Genet 的 release 回填（Deploy 的 Pod 在 Genet 的 namespace 里，
	// 平台自己的 namespace 读不到）。空值不写（omitempty + jsonb 合并 → 不会冲掉已有键）。
	snap := domain.DeploySnapshot{
		State:          st.Phase,
		QueueReason:    st.Reason,
		ReleaseStatus:  st.ReleaseStatus,
		EndpointStatus: st.EndpointStatus,
		Namespace:      st.Namespace,
		PackageType:    st.PackageType,
		Chart:          st.Chart,
		ChartVersion:   st.ChartVersion,
		Queue:          st.Queue,
		LastPolledAt:   &now,
	}
	if st.Phase == "ready" {
		ep := st.Endpoint
		snap.ReadyAt = &now
		snap.Endpoint = &ep
	}
	if st.Failure != nil {
		snap.Failure = st.Failure
	}
	if err := c.repos.RecordDeployState(ctx, run.ID, snap); err != nil {
		return err
	}
	switch st.Phase {
	case "ready":
		if st.Endpoint.BaseURL == "" {
			// 说 ready 却没地址：部署侧问题，不是平台调不通 → DEPLOY_BLOCKED（可重试）。
			f := &domain.Failure{
				Code:      domain.CodeDeployBlocked,
				Message:   "Genet reported ready but returned no endpoint",
				Retryable: true,
				Detail:    map[string]any{"reason": "blocked"},
			}
			_ = c.repos.RecordDeployState(ctx, run.ID, domain.DeploySnapshot{State: "blocked", Failure: f})
			return c.repos.DeployBlocked(ctx, run.ID, "ready without endpoint", f)
		}
		if _, err := c.repos.DeployReady(ctx, run.ID, st.Endpoint); err != nil {
			return err
		}
		return c.queueAfterTesting(ctx, run.ID)
	case "blocked":
		var f *domain.Failure
		if st.Failure != nil {
			f = st.Failure
		} else {
			f = &domain.Failure{Code: domain.CodeDeployBlocked, Message: st.Reason, Retryable: true}
		}
		// blocked → service_unavailable（C-5.3），不是 failed。
		return c.repos.DeployBlocked(ctx, run.ID, st.Reason, f)
	}
	return nil
}

// queueAfterTesting 按 C-5.4 入队：有 Pre-Check 只放 seq=0，否则全部放行。
func (c *Controller) queueAfterTesting(ctx context.Context, runID string) error {
	tasks, err := c.repos.ListTasks(ctx, runID)
	if err != nil {
		return err
	}
	// 判据唯一：IsPrecheck（resolved.precheck.injected=true 且 seq=0），
	// 不看 tester.yaml 的 role（03-data-view §4.2）。
	hasPrecheck := HasPrecheck(tasks)
	_, err = c.repos.QueueTasks(ctx, runID, hasPrecheck)
	return err
}

// WatchdogOnce 扫描超时 Attempt（C-6.3）：先落库终态，再删 Job（execution §5.1 的顺序）。
func (c *Controller) WatchdogOnce(ctx context.Context) error {
	expired, err := c.repos.ListExpiredAttempts(ctx, c.now())
	if err != nil {
		return err
	}
	for _, ref := range expired {
		f := &domain.Failure{
			Code:      domain.CodeTaskTimeout,
			Message:   "Task exceeded its timeout and was terminated by the platform",
			Retryable: true,
		}
		if _, err := c.repos.FinishAttempt(ctx, FinishInput{
			AttemptID:  ref.AttemptID,
			TaskID:     ref.TaskID,
			RunID:      ref.RunID,
			Phase:      "timeout",
			Message:    f.Message,
			Failure:    f,
			FinishedAt: c.now(),
		}); err != nil {
			c.logf("orchestrator: timeout attempt %s: %v", ref.AttemptID, err)
			continue
		}
		c.cancelJobAsync(ctx, ref)
	}
	return nil
}

// CancelRun 取消整个 Run（C-6.1）：落库取消意图 → 删 Job → 收口。
//
// 关键：**没有正在跑的 Attempt 时必须同一轮直接收口 canceled**，不停在 cancelling——
// 否则 queued 阶段取消的 Run 会永远挂在 cancelling（需要等一个永远不会到来的
// JobObserver 回调）。这里不盲信 adapter 的 AllStopped，而是用 Task/Attempt 事实再判一次。
func (c *Controller) CancelRun(ctx context.Context, runID, actor string) error {
	out, err := c.repos.RequestCancelRun(ctx, runID, actor)
	if err != nil {
		return err
	}
	// deploying 阶段取消：先落取消意图，再请求 Genet 卸载（C-6.1 第 2 条、§9.6）。
	// 404/409 = release 尚未出现或正忙 → 只 WARN；取消意图已在库，由对账补偿。
	if c.deployer != nil {
		now := c.now()
		_ = c.repos.RecordDeployState(ctx, runID, domain.DeploySnapshot{CancelRequestedAt: &now})
		c.cleanupDeploy(ctx, runID)
	}
	for _, ref := range out.CancelJobs {
		c.cancelJobAsync(ctx, ref)
	}
	if out.AllStopped || len(out.CancelJobs) == 0 {
		stopped, serr := c.runStopped(ctx, runID)
		if serr != nil {
			c.logf("orchestrator: cancel run %s: check stopped: %v", runID, serr)
			return serr
		}
		if stopped {
			return c.repos.FinishRun(ctx, runID, domain.RunCanceled, nil)
		}
	}
	return nil
}

// runStopped 报告该 Run 是否已经没有任何未终态的 Task（= 没有活动 Attempt）。
// 用 Task 状态判定即可：不变量保证"Task 终态 ⇒ 它的 Attempt 已终态"。
func (c *Controller) runStopped(ctx context.Context, runID string) (bool, error) {
	tasks, err := c.repos.ListTasks(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if !t.Status.IsTerminal() {
			return false, nil
		}
	}
	return true, nil
}

// CancelTask 取消单个 Task（C-6.2）。
func (c *Controller) CancelTask(ctx context.Context, taskID, actor string) error {
	out, err := c.repos.RequestCancelTask(ctx, taskID, actor)
	if err != nil {
		return err
	}
	for _, ref := range out.CancelJobs {
		c.cancelJobAsync(ctx, ref)
	}
	return nil
}

// cleanupDeployIfTerminal 在 Run 已终态且仍是 deploy 形态时收口 Genet 资源。
func (c *Controller) cleanupDeployIfTerminal(ctx context.Context, runID string) {
	if c.deployer == nil || runID == "" {
		return
	}
	run, err := c.repos.GetRun(ctx, runID)
	if err != nil || !run.HasDeploy || !run.Status.IsTerminal() {
		return
	}
	c.cleanupDeploy(ctx, runID)
}

// cleanupPendingDeploys 是对账的一轮：把"已终态但 uninstall.state != verified"的 Run 重试卸载。
func (c *Controller) cleanupPendingDeploys(ctx context.Context) {
	if c.deployer == nil {
		return
	}
	pending, err := c.repos.ListDeployCleanupPending(ctx, reconcileBatchLimit)
	if err != nil {
		c.logf("orchestrator: list deploy cleanup pending: %v", err)
		return
	}
	for _, run := range pending {
		c.cleanupDeploy(ctx, run.ID)
	}
}

// jobHeld 判断 Job 的 hold-until 是否还没过期（执行层可选能力：*executor.Executor 实现了）。
func (c *Controller) jobHeld(ctx context.Context, h ports.JobHandle) bool {
	holder, ok := c.exec.(ports.JobHolder)
	if !ok {
		return false
	}
	held, err := holder.HoldActive(ctx, h)
	if err != nil {
		c.logf("orchestrator: WARN hold_until_probe %s: %v", h.JobName, err)
		return false // 读不到就按"没守卫"处理（多删一个 Job 好过卡住回收）
	}
	return held
}
