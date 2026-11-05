package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/ports"
)

// 调度与推进的默认时序（C-4.1、C-5.3、C-6.3）。
const (
	DefaultDispatchInterval = time.Second
	DefaultDeployInterval   = time.Second
	DefaultWatchdogInterval = 10 * time.Second
	DefaultMaxCandidates    = 50
	DefaultTaskTimeout      = 4 * time.Hour
	DefaultDeployTimeout    = 72 * time.Hour
	defaultRunner           = "controller"
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
	// DispatchInterval / DeployInterval / WatchdogInterval 是三个循环的节拍。
	DispatchInterval time.Duration
	DeployInterval   time.Duration
	WatchdogInterval time.Duration
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
	Config    Config
	Logf      func(format string, args ...any)
}

// Controller 是编排主循环：调度 → Submit/Watch → 终态推进 → Deploy/取消/超时。
type Controller struct {
	repos     Repos
	exec      ports.Executor
	deployer  ports.Deployer
	validator ports.ResultValidator
	cfg       Config
	logf      func(format string, args ...any)

	mu      sync.Mutex
	watches map[string]context.CancelFunc // attemptID -> cancel
	wg      sync.WaitGroup
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
	return &Controller{
		repos:     opts.Repos,
		exec:      opts.Executor,
		deployer:  opts.Deployer,
		validator: opts.Validator,
		cfg:       opts.Config.withDefaults(),
		logf:      logf,
		watches:   make(map[string]context.CancelFunc),
	}, nil
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

	c.logf("orchestrator: controller started (ns=%s, concurrency=%d, storage=%s)",
		c.cfg.Namespace, c.cfg.MaxGlobalConcurrency, c.cfg.StoragePVC)

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
		}
	}
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
		FinishedAt: time.Now().UTC(),
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

	phase := o.Phase
	var failure *domain.Failure
	switch o.Phase {
	case executor.PhaseSucceeded:
		// 退出码 0 之后才读 result.json（execution §8.1）。
		if c.validator != nil {
			outputDir := filepath.Join(c.cfg.StorageRoot,
				filepath.FromSlash(mustSubPath(attempt)))
			if f := c.validator.Validate(ctx, attempt.RunID, attempt.TaskID, attempt.AttemptID, outputDir); f != nil {
				phase = "failed"
				failure = f
			}
		}
	default:
		code := executor.OutcomeCode(o)
		if code == "" {
			code = domain.CodeInternalError
		}
		failure = &domain.Failure{
			Code:      code,
			Message:   o.Message,
			Retryable: domain.Retryable(code),
			Detail:    map[string]any{"phase": o.Phase},
		}
		if o.ExitCode != nil {
			failure.Detail["exit_code"] = *o.ExitCode
		}
	}

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
	}
	_ = task
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
				Code:    domain.CodeInternalError,
				Message: "deployer is not configured",
			})
		}
		if err := c.repos.BeginDeploy(ctx, run.ID, "d-"+run.ID); err != nil {
			return err
		}
		// 事务外调用 Genet（C-2.5），内部幂等键 = run_id。
		if err := c.deployer.Start(ctx, ports.DeployRequest{
			RunID:    run.ID,
			DeployID: "d-" + run.ID,
			Timeout:  c.cfg.DeployTimeout,
		}); err != nil {
			return c.repos.FinishDeployAsFailed(ctx, run.ID, &domain.Failure{
				Code:    domain.CodeInternalError,
				Message: err.Error(),
			})
		}
		return nil
	}
	if _, err := c.repos.BeginTesting(ctx, run.ID); err != nil {
		return err
	}
	return c.queueAfterTesting(ctx, run.ID)
}

func (c *Controller) pollDeploy(ctx context.Context, run RunView) error {
	// 总窗口判定（K-6）：排队 + 部署超窗 → service_unavailable，不是 failed。
	if !run.SubmittedAt.IsZero() && time.Since(run.SubmittedAt) > c.cfg.DeployTimeout {
		return c.repos.DeployBlocked(ctx, run.ID, "deploy window exceeded", &domain.Failure{
			Code:    domain.CodeInternalError,
			Message: fmt.Sprintf("deploy did not become ready within %s", c.cfg.DeployTimeout),
		})
	}
	st, err := c.deployer.Poll(ctx, run.ID)
	if err != nil {
		// 平台调不通（网络/5xx/鉴权）→ Run failed（C-5.3）。
		return c.repos.FinishDeployAsFailed(ctx, run.ID, &domain.Failure{
			Code:    domain.CodeInternalError,
			Message: err.Error(),
		})
	}
	if err := c.repos.RecordDeployState(ctx, run.ID, st); err != nil {
		return err
	}
	switch st.Phase {
	case "ready":
		if st.Endpoint.BaseURL == "" {
			return c.repos.DeployBlocked(ctx, run.ID, "ready without endpoint", &domain.Failure{
				Code:    domain.CodeInternalError,
				Message: "Genet reported ready but returned no endpoint",
			})
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
			f = &domain.Failure{Code: domain.CodeInternalError, Message: st.Reason}
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
	hasPrecheck := false
	for _, t := range tasks {
		if t.TesterRole == domain.RolePreCheck {
			hasPrecheck = true
			break
		}
	}
	_, err = c.repos.QueueTasks(ctx, runID, hasPrecheck)
	return err
}

// WatchdogOnce 扫描超时 Attempt（C-6.3）：先落库终态，再删 Job（execution §5.1 的顺序）。
func (c *Controller) WatchdogOnce(ctx context.Context) error {
	expired, err := c.repos.ListExpiredAttempts(ctx, time.Now())
	if err != nil {
		return err
	}
	for _, ref := range expired {
		f := &domain.Failure{
			Code:      domain.CodeTaskTimeout,
			Message:   "Task exceeded its timeout and was terminated by the platform",
			Retryable: domain.Retryable(domain.CodeTaskTimeout),
		}
		if _, err := c.repos.FinishAttempt(ctx, FinishInput{
			AttemptID:  ref.AttemptID,
			TaskID:     ref.TaskID,
			RunID:      ref.RunID,
			Phase:      "timeout",
			Message:    f.Message,
			Failure:    f,
			FinishedAt: time.Now().UTC(),
		}); err != nil {
			c.logf("orchestrator: timeout attempt %s: %v", ref.AttemptID, err)
			continue
		}
		c.cancelJobAsync(ctx, ref)
	}
	return nil
}

// CancelRun 取消整个 Run（C-6.1）：落库取消意图 → 删 Job → 收口。
func (c *Controller) CancelRun(ctx context.Context, runID, actor string) error {
	out, err := c.repos.RequestCancelRun(ctx, runID, actor)
	if err != nil {
		return err
	}
	for _, ref := range out.CancelJobs {
		c.cancelJobAsync(ctx, ref)
	}
	if out.AllStopped {
		return c.repos.FinishRun(ctx, runID, domain.RunCanceled, nil)
	}
	return nil
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
