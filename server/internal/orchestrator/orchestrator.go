// Package orchestrator 是编排层 v2：线性管道推进器（design/impl/control-plane.md §C-4 ~ §C-7）。
//
// v2 模型（D-66～D-70）：Run→Task、Task→runner 执行记录（(task_id, attempt_no, seq) 定位）。
// v1 的"门控 + 批量并发入池 + Deploy 阶段轮询 Genet + cleanup.policy + 同 Run 追加 Attempt"
// 全部取消（D-69/D-70）：
//
//   - Task 内 runner 严格串行（线性管道）：[service-deploy?] → [precheck?] → [test…] → [service-clean?]；
//     管道推进由 store.FinishRunnerRun 在同事务内完成（前一 runner 出终态 → 下一 runner 置 queued）；
//   - 部署/拆除 = 内置 service-deploy / service-clean runner 的 Job，平台不直连 Genet（D-70）；
//     部署可见性走 deploy-state.json 副本归档（archiveDeployState）；
//   - clean finally（C-5.7）：成功/失败/取消/超时四路径都排程 service-clean，失败只记
//     cleanup_failed 不改终态；
//   - abort_task 传播（C-5.5）：store.skipUpstreamRunners 批量 SKIPPED（传播到 clean 为止），
//     编排层随后排程 service-clean；
//   - 取消（C-6.1）= 停当前 runner → 推进到 clean → clean 完成落 canceled；
//   - 整 Task 重试（C-5.6）：store.RetryTask 新 attempt 全量重跑含 fresh deploy；
//   - 泄漏重放（§9.3）：POST /tasks/{id}/clean → 追加 service-clean-replay 执行记录。
//
// 分层约定（C-1/C-13）：外部 IO（K8s、NFS）在事务外调用；状态转移只经 store 的
// Kernel/门面/组合事务出口落库；编排层不做任何 SQL。
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/store"
)

// 调度与推进的默认时序（C-4.1、C-6.3、C-3.5）。
const (
	DefaultDispatchInterval  = time.Second
	DefaultWatchdogInterval  = 10 * time.Second
	DefaultReconcileInterval = 30 * time.Second
	// DefaultGCInterval 是维护 tick（Job/Pod GC + clean 兜底对账）的默认周期
	//（execution §6.3：GC 每 10 分钟一轮的量级；本实现默认 5m）。
	DefaultGCInterval = 5 * time.Minute
	// DefaultDeploySampleInterval 是 deploy-state.json 的采样周期（C-5.3.2/D-31）：
	// 部署排队/部署中的状态轨迹要可见，30s 对账 tick 对短部署整段错过，用 5s 专采样。
	DefaultDeploySampleInterval = 5 * time.Second
	// DefaultMaxCandidates 是单轮候选上限（C-4.2 LIMIT 50）。
	DefaultMaxCandidates = 50
	// cleanupScanLimit 是单轮 clean 兜底对账扫描的行数上限。
	cleanupScanLimit = 100
	// AttemptLogFileName 是 runner 日志落盘的目标文件名（API 的 logs 端点直接读它）。
	AttemptLogFileName = "stdout.log"
	// attemptLogTimeout 是日志落盘的硬上限：不阻塞状态推进。
	attemptLogTimeout = 2 * time.Minute
	// jobCancelTimeout 是单次删 Job 的超时。
	jobCancelTimeout = 2 * time.Minute
)

// Config 是编排层配置（由 cmd/argusd 从 internal/config 投影）。
type Config struct {
	// Namespace 是 Job 所在 namespace（AG_EXECUTOR_NAMESPACE）。
	Namespace string
	// StorageRoot 是 controller 侧看到的 NFS 挂载点（<storage-root>）。
	StorageRoot string
	// GenetBaseURL 是 Genet OpenAPI 接入点（AG_GENET_BASE_URL），仅注入
	// role=deploy/clean 的 runner Job（§9.1、§12.4；D-70 平台不直连）。
	GenetBaseURL string
	// MockGenetBaseURL 是 fake-genet 联调接入点（AG_MOCK_GENET_BASE_URL）：deploy.provider=mock
	// 的 Task 渲染 deploy/clean Job 时注入本值（与 genet 同一 OpenAPI 契约，见 job.go）。
	MockGenetBaseURL string
	// StoragePVC 是产物 RWX PVC 名（Job 渲染挂 /workspace/output）。
	StoragePVC string
	// ServiceAccount 为空则不渲染（runner 不调 K8s API）。
	ServiceAccount string
	// ClusterName 写进 executor_ref.cluster（默认 cluster-01）。
	ClusterName string
	// MaxGlobalConcurrency 是全局并发上限（AG_CONCURRENCY_MAX）。
	MaxGlobalConcurrency int
	// AttemptMax 是单 Task attempt 上限（AG_ATTEMPT_MAX，默认 3）。
	AttemptMax int
	// DispatchInterval / WatchdogInterval / ReconcileInterval 是各循环节拍。
	DispatchInterval  time.Duration
	WatchdogInterval  time.Duration
	ReconcileInterval time.Duration
	// MaxCandidates 是单轮候选上限（C-4.2）。
	MaxCandidates int
	// GCEnabled 是 Job/裸 Pod 回收开关（config.JobGCEnabled / AG_JOB_GC_ENABLED 投影）。
	// 关掉时维护 tick 只跑 clean 兜底对账扫描。
	GCEnabled bool
	// GCInterval 是维护 tick（GC + clean 兜底对账）的周期（默认 5m，DefaultGCInterval）。
	GCInterval time.Duration
	// DeploySampleInterval 是 deploy-state.json 采样周期（默认 5s，DefaultDeploySampleInterval；
	// 仅在有 RUNNING 的 service-deploy 时才产生实际归档动作，§9.2）。
	DeploySampleInterval time.Duration
	// GCTick 是一轮 Job/Pod 回收。cmd 装配时绑到 executor.CollectGarbage（GCOptions 由
	// 装配层构造：RunnerRunStates 查 store、OnStaleRunning 回 MarkStaleRunnerTimeout）；
	// 编排层不 import executor，保持依赖方向。nil = 不跑 GC。
	GCTick func(ctx context.Context) error
	// SharedMountsFn 是 D-65 的运行时来源：每次渲染读"启用中的挂载点"（store 提供）。
	// 为 nil 时退回 SharedMounts（静态配置，测试/未接线场景）。
	SharedMountsFn func(ctx context.Context) ([]domain.SharedMountSpec, error)
	// SharedMounts 是 SharedMountsFn 未接线时的回落（AG_SHARED_MOUNTS 种子）。
	SharedMounts []domain.SharedMountSpec
	// SharedMountsDisabledProfiles 是关闭统一挂载的 Profile 名集合（D-63 ③）。
	SharedMountsDisabledProfiles map[string]bool
}

func (c Config) withDefaults() Config {
	if c.Namespace == "" {
		c.Namespace = "argus"
	}
	if c.ClusterName == "" {
		c.ClusterName = "cluster-01"
	}
	if c.MaxGlobalConcurrency <= 0 {
		c.MaxGlobalConcurrency = 20
	}
	if c.AttemptMax <= 0 {
		c.AttemptMax = 3
	}
	if c.DispatchInterval <= 0 {
		c.DispatchInterval = DefaultDispatchInterval
	}
	if c.WatchdogInterval <= 0 {
		c.WatchdogInterval = DefaultWatchdogInterval
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = DefaultReconcileInterval
	}
	if c.GCInterval <= 0 {
		c.GCInterval = DefaultGCInterval
	}
	if c.DeploySampleInterval <= 0 {
		c.DeploySampleInterval = DefaultDeploySampleInterval
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = DefaultMaxCandidates
	}
	return c
}

// Options 是 Scheduler 的构造输入（cmd/argusd 装配用）。
type Options struct {
	// Store 是控制面内核（唯一写库入口，C-1）。
	Store *store.Store
	// Machines 是两个状态机门面（store.NewMachines 装配）。
	Machines *store.Machines
	// Executor 是执行层（K8s Job）。S2 波次改造中：按 ports.Executor 冻结接口编程。
	Executor ports.Executor
	// Validator 是 runner 产物校验器（report.Validator：D-74 三选一 + exports 词法）。
	Validator ports.ResultValidator
	Config    Config
	Logf      func(format string, args ...any)
	// Clock 是业务时间来源（05 §6.5）。为空时用 domain.SystemClock。
	Clock domain.Clock
}

// Scheduler 是线性管道推进器：BeginPipeline → Queue(seq=1) → Claim → Submit →
// Watch → 收割/终态推进（store.FinishRunnerRun）→ … → clean finally → Task 终态。
type Scheduler struct {
	st        *store.Store
	mach      *store.Machines
	exec      ports.Executor
	validator ports.ResultValidator
	cfg       Config
	logf      func(format string, args ...any)
	clock     domain.Clock

	mu      sync.Mutex
	watches map[string]context.CancelFunc // job_id -> cancel
	wg      sync.WaitGroup
	// logWG 跟踪后台日志落盘，供关闭/测试等待（不阻塞状态推进）。
	logWG sync.WaitGroup
}

// New 构造 Scheduler。Store / Machines / Executor 必填。
func New(opts Options) (*Scheduler, error) {
	if opts.Store == nil {
		return nil, errors.New("orchestrator: Store is required")
	}
	if opts.Machines == nil {
		return nil, errors.New("orchestrator: Machines is required")
	}
	if opts.Executor == nil {
		return nil, errors.New("orchestrator: Executor is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	clock := opts.Clock
	if clock == nil {
		clock = domain.SystemClock
	}
	return &Scheduler{
		st:        opts.Store,
		mach:      opts.Machines,
		exec:      opts.Executor,
		validator: opts.Validator,
		cfg:       opts.Config.withDefaults(),
		logf:      logf,
		clock:     clock,
		watches:   make(map[string]context.CancelFunc),
	}, nil
}

// Config 返回生效配置（健康检查与测试用）。
func (s *Scheduler) Config() Config { return s.cfg }

// now 返回业务时间（UTC）。编排层不直调 time.Now()。
func (s *Scheduler) now() time.Time { return s.clock.Now() }

// Run 启动主循环，直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) error {
	dispatch := time.NewTicker(s.cfg.DispatchInterval)
	defer dispatch.Stop()
	watchdog := time.NewTicker(s.cfg.WatchdogInterval)
	defer watchdog.Stop()
	reconcile := time.NewTicker(s.cfg.ReconcileInterval)
	defer reconcile.Stop()
	maintenance := time.NewTicker(s.cfg.GCInterval)
	defer maintenance.Stop()
	deploySample := time.NewTicker(s.cfg.DeploySampleInterval)
	defer deploySample.Stop()

	s.logf("orchestrator: scheduler started (ns=%s, concurrency=%d, attempt_max=%d, gc=%v interval=%s)",
		s.cfg.Namespace, s.cfg.MaxGlobalConcurrency, s.cfg.AttemptMax, s.cfg.GCEnabled && s.cfg.GCTick != nil, s.cfg.GCInterval)

	// 启动即推进一轮：重启后把 queued Task 与 cancel_requested 的 runner 接上（C-3.5）。
	if err := s.AdvanceOnce(ctx); err != nil {
		s.logf("orchestrator: startup advance: %v", err)
	}
	// 启动即做一轮维护：重启后立刻回收孤儿/终态残留 Job（execution §6.3）。
	s.MaintenanceOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			s.stopWatches()
			s.wg.Wait()
			return ctx.Err()
		case <-dispatch.C:
			if err := s.DispatchOnce(ctx); err != nil {
				s.logf("orchestrator: dispatch: %v", err)
			}
		case <-watchdog.C:
			if err := s.WatchdogOnce(ctx); err != nil {
				s.logf("orchestrator: watchdog: %v", err)
			}
		case <-deploySample.C:
			s.archiveDeployStates(ctx)
		case <-reconcile.C:
			if err := s.ReconcileOnce(ctx); err != nil {
				s.logf("orchestrator: reconcile: %v", err)
			}
		case <-maintenance.C:
			s.MaintenanceOnce(ctx)
		}
	}
}

// MaintenanceOnce 是低频维护 tick（默认 5m，与 GC 同节奏）：
//   - Job/裸 Pod 回收（S2 的 CollectGarbage 在此接回；失败只记日志不中断）；
//   - clean 兜底对账扫描（§9.3 兜底的 MVP 形态）。
func (s *Scheduler) MaintenanceOnce(ctx context.Context) {
	if s.cfg.GCEnabled && s.cfg.GCTick != nil {
		if err := s.cfg.GCTick(ctx); err != nil {
			s.logf("orchestrator: WARN gc tick: %v", err)
		}
	}
	s.scanCleanupRequired(ctx)
}

// scanCleanupRequired 是 §9.3 兜底的 MVP 形态：周期扫描「Task 已终态 + tasks.deploy
// 非空 + 该 attempt 无 succeeded 的 clean 记录」的行（store.Tasks.ListCleanupRequired），
// 每个命中行写一条事件并打结构化日志。
//
// 事件类型选型：复用 state_changed（domain.EventTaskStateChanged）+ payload.reason =
// cleanup_required，不新增事件类型——事件类型集合是 SSE 时间线/前端的既有契约，
// 扩集合要动 domain/machine.go（本波次不动 domain）；语义靠 payload.reason 承载。
// **不自动重放**（§14.2 P1）：重放只走 POST /tasks/{id}/clean。
func (s *Scheduler) scanCleanupRequired(ctx context.Context) {
	rows, err := s.st.Tasks.ListCleanupRequired(ctx, nil, cleanupScanLimit)
	if err != nil {
		s.logf("orchestrator: WARN cleanup reconcile scan: %v", err)
		return
	}
	for _, row := range rows {
		if _, err := s.st.EmitTaskEvent(ctx, row.TaskID, domain.EventTaskStateChanged, map[string]any{
			"reason":     "cleanup_required",
			"attempt_no": row.AttemptNo,
			"release":    row.DeployRelease,
			"detail":     "Task 已终态但该 attempt 没有 succeeded 的 clean 记录（deploy 泄漏候选）；确认后走 POST /tasks/{id}/clean 重放",
		}, "system:orchestrator"); err != nil {
			s.logf("orchestrator: WARN cleanup_required event %s: %v", row.TaskID, err)
			continue
		}
		s.logf("orchestrator: WARN cleanup_required: task %s attempt %d (release=%q) 缺少 succeeded 的 clean 记录；不自动重放，走 POST /tasks/%s/clean",
			row.TaskID, row.AttemptNo, row.DeployRelease, row.TaskID)
	}
}

// AdvanceOnce 是一轮"任务级推进"（调度器与对账共用）：
//
//  1. queued Task → running（BeginPipeline）并置第一个 pending runner 为 queued（C-5.4 初始）；
//  2. cancelling Task 的收口（settleCancel：推进 clean / 落 canceled，C-6.1）。
func (s *Scheduler) AdvanceOnce(ctx context.Context) error {
	tasks, _, err := s.st.Tasks.List(ctx, nil, store.TaskFilter{
		Statuses: []string{string(domain.TaskQueued)},
		Limit:    s.cfg.MaxCandidates,
	})
	if err != nil {
		return fmt.Errorf("orchestrator: list queued tasks: %w", err)
	}
	for i := range tasks {
		if err := s.beginTask(ctx, &tasks[i]); err != nil {
			s.logf("orchestrator: begin task %s: %v", tasks[i].ID, err)
		}
	}
	if err := s.settleCancelling(ctx); err != nil {
		s.logf("orchestrator: settle cancelling: %v", err)
	}
	return nil
}

// beginTask 处理 queued 的 Task：queued → running，随后把当前 attempt 的第一个
// pending runner 置 queued（管道入口，C-5.4"初始 seq=1 置 queued"）。
func (s *Scheduler) beginTask(ctx context.Context, t *store.Task) error {
	if _, err := s.mach.Tasks.BeginPipeline(ctx, t.ID, "system:orchestrator"); err != nil {
		// 并发竞争：另一实例先推进了。幂等忽略。
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPrecondition) {
			return nil
		}
		return err
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, t.ID, t.CurrentAttempt)
	if err != nil {
		return err
	}
	return s.queueFirstPending(ctx, t.ID, t.CurrentAttempt, runs)
}

// queueFirstPending 把当前 attempt 的第一个 pending runner 置 queued（串行管道入口）。
func (s *Scheduler) queueFirstPending(ctx context.Context, taskID string, attemptNo int, runs []store.RunnerRun) error {
	for _, rr := range runs {
		if rr.Status != string(domain.RunnerPending) {
			continue
		}
		k := rr.Key()
		if _, err := s.mach.Runners.Queue(ctx, k); err != nil {
			if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPrecondition) {
				continue
			}
			return err
		}
		s.logf("orchestrator: task %s attempt %d runner seq=%d (%s) queued", taskID, attemptNo, rr.Seq, rr.Name)
		return nil
	}
	return nil
}

// DispatchOnce 是一轮调度（C-4.1）：取候选 → Claim（permit/槽位/建 Job 句柄落库）→
// 渲染 JobSpec → executor.Submit → Start → Watch。
func (s *Scheduler) DispatchOnce(ctx context.Context) error {
	if err := s.AdvanceOnce(ctx); err != nil {
		s.logf("orchestrator: advance: %v", err)
	}
	cands, err := s.st.RunnerRuns.Candidates(ctx, nil, s.cfg.MaxCandidates)
	if err != nil {
		return fmt.Errorf("orchestrator: candidates: %w", err)
	}
	for _, cand := range cands {
		k := domain.RunnerKey{TaskID: cand.TaskID, AttemptNo: cand.AttemptNo, Seq: cand.Seq}
		if err := s.dispatchOne(ctx, k, cand); err != nil {
			s.logf("orchestrator: dispatch %s: %v", k.ID(), err)
		}
	}
	return nil
}

// dispatchOne 调度单个候选（C-4.2：Claim 是原子入口，抢不到是正常结果）。
func (s *Scheduler) dispatchOne(ctx context.Context, k domain.RunnerKey, cand store.Candidate) error {
	rr, err := s.st.RunnerRuns.Get(ctx, nil, k)
	if err != nil {
		return err
	}
	if rr.Status != string(domain.RunnerQueued) {
		return nil // 被并发推走 / 已取消：跳过
	}
	endpointKey := ""
	if cand.EndpointKey != nil {
		endpointKey = *cand.EndpointKey
	}
	jobID := rr.JobID
	ref, _ := json.Marshal(map[string]string{
		"cluster":   s.cfg.ClusterName,
		"namespace": s.cfg.Namespace,
		"job":       jobID,
	})
	claimed, ok, reason, err := s.st.ClaimForExecution(ctx, store.ClaimRequest{
		Key:          k,
		EndpointKey:  endpointKey,
		Exclusive:    cand.EndpointExclusive,
		GlobalLimit:  s.cfg.MaxGlobalConcurrency,
		JobID:        jobID,
		JobNamespace: s.cfg.Namespace,
		ExecutorRef:  ref,
	})
	if err != nil {
		return err
	}
	if !ok {
		// 抢不到是**正常结果**（C-4.7）：写回 waiting_reason 后下轮重试。
		if reason != "" {
			if werr := s.st.RunnerRuns.SetWaitingReason(ctx, nil, k, reason); werr != nil {
				return werr
			}
		}
		return nil
	}
	jobID = claimed.JobName
	return s.launchClaimed(ctx, k, rr, jobID)
}

// launchClaimed 把已 Claim（preparing）的执行记录推向物理执行：
// 渲染 JobSpec → Submit → Start → Watch。泄漏重放与常规调度共用本入口。
func (s *Scheduler) launchClaimed(ctx context.Context, k domain.RunnerKey, rr *store.RunnerRun, jobID string) error {
	spec, err := s.buildJobSpec(ctx, rr, jobID)
	if err != nil {
		// 渲染失败是平台侧不可恢复错误：runner 直接 failed 并推进管道（含 clean finally）。
		return s.finishFailed(ctx, k, domain.NewFailure(domain.CodeInternalError,
			fmt.Sprintf("渲染 Job 失败: %v", err), map[string]any{"runner_seq": k.Seq}), nil)
	}
	// 目录预创建（§6.8 约束 3）：output/input 的 subPath 目录必须**先于 Job 存在**——
	// kubelet 在 hostPath(drvfs) PV 上现建 subPath 会悬挂（实测 kind/Windows），
	// input 侧由 persistInputs 连带创建，output 侧没有天然写者，必须这里建。
	for _, dir := range []string{spec.OutputDir, spec.InputDir} {
		if dir == "" {
			continue
		}
		if merr := os.MkdirAll(dir, 0o770); merr != nil {
			return s.finishFailed(ctx, k, domain.NewFailure(domain.CodeInternalError,
				fmt.Sprintf("预创建 runner 目录失败: %v", merr), map[string]any{"runner_seq": k.Seq}), nil)
		}
	}
	handle, err := s.exec.Submit(ctx, spec)
	if err != nil {
		return s.finishFailed(ctx, k, domain.NewFailure(domain.CodeExecutorUnavailable,
			fmt.Sprintf("创建 Job 失败: %v", err), map[string]any{"runner_seq": k.Seq}), nil)
	}
	// Job 已创建：preparing → running（started_at 落库；C-3.1）。
	if _, err := s.mach.Runners.Start(ctx, k); err != nil {
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPrecondition) {
			return err
		}
	}
	s.startWatch(ctx, k, handle)
	// deploy runner 启动即补采早期状态（queued 窗口只有一次轮询周期，5s 采样可能错过）：
	// 启动后 1.5s / 3.5s 各归档一次（文件未写出时为 no-op）。
	if rr.Role == domain.RoleDeploy {
		go func() {
			for _, d := range []time.Duration{1500 * time.Millisecond, 3500 * time.Millisecond} {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
				s.archiveOneDeployState(ctx, rr)
			}
		}()
	}
	s.logf("orchestrator: task %s attempt %d runner seq=%d (%s) submitted as job %s",
		k.TaskID, k.AttemptNo, k.Seq, rr.Name, handle.JobName)
	return nil
}

// startWatch 起一个 goroutine 观测 Job，终态回调 JobFinished（execution §4）。
func (s *Scheduler) startWatch(parent context.Context, k domain.RunnerKey, h ports.JobHandle) {
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if prev, ok := s.watches[h.JobName]; ok {
		prev()
	}
	s.watches[h.JobName] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.watches, h.JobName)
			s.mu.Unlock()
			cancel()
		}()
		if err := s.exec.Watch(ctx, h, s); err != nil && !errors.Is(err, context.Canceled) {
			s.logf("orchestrator: watch %s: %v", h.JobName, err)
		}
	}()
}

func (s *Scheduler) stopWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.watches {
		cancel()
	}
	s.watches = make(map[string]context.CancelFunc)
}

// JobFinished 实现 ports.JobObserver：Job 终态 → 结果校验（D-74）→ exports 收割
// （C-5.4）→ FinishRunnerRun（管道推进 / abort 传播 / Task 终态判定）。
//
// 本回调里只做"外部 IO + 开新事务提交事实"，不做长事务（C-2.5）。
func (s *Scheduler) JobFinished(ctx context.Context, o ports.JobOutcome) error {
	rr, err := s.st.RunnerRuns.GetByJobID(ctx, nil, o.Handle.JobName)
	if err != nil {
		// Job 句柄查不到执行记录：句柄已被重试重建等。记日志，不报错（watch 无法推进）。
		s.logf("orchestrator: WARN job_finished: no task_runner for job %s: %v", o.Handle.JobName, err)
		return nil
	}
	k := rr.Key()

	// 先把 Pod 日志落盘（终态推进后异步删 Job，日志必须先落地）。
	if o.Phase != "canceled" {
		s.captureRunnerLogs(k, o)
	}

	var exitCode *int
	if o.ExitCode != nil {
		v := int(*o.ExitCode)
		exitCode = &v
	}

	switch o.Phase {
	case "succeeded":
		return s.finishSucceeded(ctx, rr, o, exitCode)
	case "canceled":
		// 取消路径：runner → canceled，管道推进交给 settleCancel（C-6.1）。
		if _, err := s.mach.Runners.Cancel(ctx, k, "system:orchestrator"); err != nil {
			if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPrecondition) {
				return nil // 先落库者胜
			}
			return err
		}
		return s.settleCancel(ctx, k.TaskID)
	default: // failed | timeout | unknown
		f := s.failureFromOutcome(o)
		return s.finishFailed(ctx, k, f, exitCode)
	}
}

// finishSucceeded 处理成功的 runner：产物校验（D-74）→ exports 收割（C-5.4）→ 推进。
func (s *Scheduler) finishSucceeded(ctx context.Context, rr *store.RunnerRun, o ports.JobOutcome, exitCode *int) error {
	k := rr.Key()
	dir := s.runnerDir(k.TaskID, k.AttemptNo, k.Seq)

	// 1. 产物契约校验（C-7.1 / D-74 三选一）。校验器不可用或目录不可见时不判失败
	//（平台没挂存储 ≠ runner 没写产物），只记 WARN。
	if s.validator != nil && s.cfg.StorageRoot != "" {
		if f := s.validator.ValidateRunnerOutput(ctx, k, rr.Role, dir, o.ExitCode); f != nil {
			return s.finishFailed(ctx, k, *f, exitCode)
		}
	}

	// 2. exports 收割流水线（仅 test/pre_check/deploy 成功时；C-5.4）。
	exportsRef, keys, kvs, herr := s.harvestExports(ctx, k, rr.Role, dir)
	if herr != nil {
		return s.finishFailed(ctx, k, *herr, exitCode)
	}

	// 3. 终态推进：succeeded + 产物引用（只填真实存在的文件，不造死链）。
	out, err := s.st.FinishRunnerRun(ctx, store.RunnerFinish{
		Key:         k,
		Succeeded:   true,
		Phase:       "succeeded",
		ExitCode:    exitCode,
		ImageDigest: o.ImageDigest,
		FinishedAt:  s.finishAt(o),
		ExportsRef:  exportsRef,
		ResultRef:   s.refIfFile(k, "result.json"),
		LogRef:      s.logRefIfFile(k),
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil // 先落库者胜
		}
		return err
	}
	if exportsRef != "" && len(keys) > 0 {
		if _, err := s.st.EmitRunnerEvent(ctx, k, domain.EventExportsHarvested, map[string]any{
			"keys": keys, "exports_ref": exportsRef,
		}, "system:orchestrator"); err != nil {
			s.logf("orchestrator: WARN exports_harvested event %s: %v", k.ID(), err)
		}
		// service-deploy 的四件套回填 tasks.Endpoint（§5.12 / 03 §4.3）。
		if rr.Role == domain.RoleDeploy {
			s.backfillDeployEndpoint(ctx, k, harvestKeys(kvs))
		}
	}
	s.postFinish(ctx, k, out, rr)
	return nil
}

// finishFailed 处理失败的 runner：校验器语义失败 / EXECUTOR_UNAVAILABLE / RUNNER_TIMEOUT
// 等统一入口；on_failure=abort_task 的传播与 clean 排程由 store.FinishRunnerRun + 本层完成。
func (s *Scheduler) finishFailed(ctx context.Context, k domain.RunnerKey, f domain.Failure, exitCode *int) error {
	out, err := s.st.FinishRunnerRun(ctx, store.RunnerFinish{
		Key:        k,
		Succeeded:  false,
		Failure:    &f,
		Phase:      "failed",
		ExitCode:   exitCode,
		FinishedAt: s.now(),
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil // 先落库者胜（取消/对账已给出结论）
		}
		return err
	}
	s.logf("orchestrator: runner %s failed: %s", k.ID(), f.Code)
	s.postFinish(ctx, k, out, nil)
	return nil
}

// postFinish 是 FinishRunnerRun 之后的编排动作（全部幂等）：
//   - abort_task 触发 → 排程 service-clean（store 只传播 SKIPPED，clean 由本层放行，C-5.5/C-5.7）；
//   - Task 全终态且处于 cancelling → 落 canceled（C-6.1）；
//   - Task 全终态（正常收尾）→ 异步删 Job（GC 兜底）。
func (s *Scheduler) postFinish(ctx context.Context, k domain.RunnerKey, out *store.RunnerFinishResult, rr *store.RunnerRun) {
	if out == nil {
		return
	}
	// deploy runner 终态时补一次 deploy-state.json 归档（终态快照）：
	// 周期对账（30s tick）只采样 RUNNING 状态，短部署（数十秒）会整段错过，
	// 不补归档则 /deploy 的状态轨迹与 Pod 快照为空（deploy-check 断言依赖）。
	if rr != nil && rr.Role == domain.RoleDeploy {
		s.archiveOneDeployState(ctx, rr)
	}
	if out.AbortTriggered {
		if err := s.scheduleClean(ctx, k.TaskID, k.AttemptNo); err != nil {
			s.logf("orchestrator: schedule clean after abort %s: %v", k.TaskID, err)
		}
	}
	if out.TaskTerminal {
		if out.TaskStatus == domain.TaskCancelling {
			// clean finally 完成：cancelling → canceled（C-6.1 第 4 步）。
			if _, err := s.mach.Tasks.Canceled(ctx, k.TaskID, "system:orchestrator"); err != nil {
				s.logf("orchestrator: finish canceled task %s: %v", k.TaskID, err)
			}
		}
		if rr != nil && rr.Role == domain.RoleClean {
			if _, err := s.st.EmitTaskEvent(ctx, k.TaskID, domain.EventCleanFinished, map[string]any{
				"attempt_no": k.AttemptNo, "status": string(domain.RunnerSucceeded),
			}, "system:orchestrator"); err != nil {
				s.logf("orchestrator: WARN clean_finished event %s: %v", k.TaskID, err)
			}
		}
		s.logf("orchestrator: task %s attempt %d reached terminal state (%s)", k.TaskID, k.AttemptNo, out.TaskStatus)
	}
}

// scheduleClean 把当前 attempt 里仍 pending 的 clean runner 放行（pending → queued）。
// clean finally（C-5.7）：成功/失败/取消/超时四路径都排程 service-clean。
func (s *Scheduler) scheduleClean(ctx context.Context, taskID string, attemptNo int) error {
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, taskID, attemptNo)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Role == domain.RoleClean && r.Status == string(domain.RunnerPending) {
			k := r.Key()
			if _, err := s.mach.Runners.Queue(ctx, k); err != nil &&
				!errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPrecondition) {
				return err
			}
			s.logf("orchestrator: service-clean scheduled: task %s attempt %d seq=%d", taskID, attemptNo, r.Seq)
			return nil
		}
	}
	return nil
}

// failureFromOutcome 把 executor 的失败相位翻译成附录 D 的 Failure（E-8.1）。
func (s *Scheduler) failureFromOutcome(o ports.JobOutcome) domain.Failure {
	switch o.Phase {
	case "timeout":
		return domain.Failure{
			Code:      domain.CodeRunnerTimeout,
			Message:   firstNonEmpty(o.Message, "runner exceeded its timeout and was terminated by the platform"),
			Retryable: true,
			Detail:    map[string]any{"phase": o.Phase},
		}
	}
	code := domain.CodeRunnerNonzeroExit
	if o.ExitCode == nil || *o.ExitCode == 0 {
		// 无退出码的失败（Pod 侧异常 / executor 报错）。
		code = domain.CodeInternalError
		if o.K8sReason != "" {
			code = domain.CodeExecutorPodError
		}
	}
	f := domain.Failure{
		Code:      code,
		Message:   firstNonEmpty(o.Message, "runner failed"),
		Retryable: domain.Retryable(code),
		Detail:    map[string]any{"phase": o.Phase},
	}
	if o.K8sReason != "" {
		f.Detail["k8s_reason"] = o.K8sReason
	}
	if o.ExitCode != nil {
		f.Detail["exit_code"] = *o.ExitCode
	}
	return f
}

func (s *Scheduler) finishAt(o ports.JobOutcome) time.Time {
	if o.FinishedAt.IsZero() {
		return s.now()
	}
	return o.FinishedAt
}

// ---- Watchdog（C-6.3）------------------------------------------------------

// WatchdogOnce 扫描超时的活跃执行记录：先落库终态（RUNNER_TIMEOUT），再异步删 Job。
// 权威判定在平台（Job 侧 activeDeadlineSeconds 只是兜底）。
func (s *Scheduler) WatchdogOnce(ctx context.Context) error {
	expired, err := s.st.RunnerRuns.ExpiredRunnerRuns(ctx, nil, s.now())
	if err != nil {
		return fmt.Errorf("orchestrator: expired runner runs: %w", err)
	}
	for _, e := range expired {
		f := domain.Failure{
			Code:      domain.CodeRunnerTimeout,
			Message:   "runner exceeded its timeout and was terminated by the platform",
			Retryable: true,
			Detail:    map[string]any{"runner_seq": e.Key.Seq},
		}
		if err := s.finishFailed(ctx, e.Key, f, nil); err != nil {
			s.logf("orchestrator: timeout %s: %v", e.Key.ID(), err)
			continue
		}
		s.cancelJobAsync(ctx, e.JobName)
	}
	return nil
}

// ---- 对账（C-3.5，最小可用版）----------------------------------------------

// MarkStaleRunnerTimeout 是 GC 规则 4（运行中但超过 25h 的卡死 Job）的落库侧修复：
// 把对应执行记录修成 FAILED(RUNNER_TIMEOUT)（execution §6.3），Job 本体由 GC 删除。
// cmd 装配层把它绑进 executor.GCOptions.OnStaleRunning。已终态的执行记录 CAS 失败
// （先落库者胜），按幂等忽略。
func (s *Scheduler) MarkStaleRunnerTimeout(ctx context.Context, taskID string, attemptNo, seq int) error {
	k := domain.RunnerKey{TaskID: taskID, AttemptNo: attemptNo, Seq: seq}
	return s.finishFailed(ctx, k, domain.Failure{
		Code:      domain.CodeRunnerTimeout,
		Message:   "runner job 卡死（运行中超过 GC 上限 25h），由 GC 修复为超时失败",
		Retryable: true,
		Detail:    map[string]any{"runner_seq": seq, "source": "gc"},
	}, nil)
}

// ReconcileOnce 是一轮对账：
//  1. queued Task / cancelling Task 的推进（AdvanceOnce）；
//  2. 重启恢复：活跃执行记录的 Watch 丢了就重建（Watch 阻塞到终态，幂等）；
//  3. service-deploy 的 deploy-state.json 副本归档（C-5.3.2）；
//  4. 僵尸租约清理：Task 已终态但 endpoint_leases 还有行。
func (s *Scheduler) ReconcileOnce(ctx context.Context) error {
	if err := s.AdvanceOnce(ctx); err != nil {
		s.logf("orchestrator: reconcile advance: %v", err)
	}
	s.reconcileWatches(ctx)
	s.archiveDeployStates(ctx)
	s.releaseStaleLeases(ctx)
	return nil
}

// reconcileWatches 给仍在 preparing/running 的执行记录补 Watch（重启恢复）。
func (s *Scheduler) reconcileWatches(ctx context.Context) {
	active, err := s.st.ListActiveRunnerRuns(ctx, nil, s.cfg.MaxCandidates)
	if err != nil {
		s.logf("orchestrator: reconcile active runners: %v", err)
		return
	}
	for i := range active {
		rr := &active[i]
		s.mu.Lock()
		_, watching := s.watches[rr.JobID]
		s.mu.Unlock()
		if watching {
			continue
		}
		h := ports.JobHandle{Namespace: s.cfg.Namespace, JobName: rr.JobID}
		s.startWatch(ctx, rr.Key(), h)
		s.logf("orchestrator: watch re-established: job %s", rr.JobID)
	}
}

// archiveDeployStates 把 RUNNING 的 service-deploy 产出的 deploy-state.json 归档
// （attempts/<n>/deploy-state.json 副本 + tasks.deploy 增量合并 + deploy.state_changed 事件）。
func (s *Scheduler) archiveDeployStates(ctx context.Context) {
	if s.cfg.StorageRoot == "" {
		return
	}
	active, err := s.st.ListActiveRunnerRuns(ctx, nil, s.cfg.MaxCandidates)
	if err != nil {
		return
	}
	for i := range active {
		rr := &active[i]
		if rr.Role != domain.RoleDeploy || rr.Status != string(domain.RunnerRunning) {
			continue
		}
		s.archiveOneDeployState(ctx, rr)
	}
}

func (s *Scheduler) archiveOneDeployState(ctx context.Context, rr *store.RunnerRun) {
	src := s.runnerDir(rr.TaskID, rr.AttemptNo, rr.Seq) + "/deploy-state.json"
	raw, err := osReadFile(src)
	if err != nil || len(raw) == 0 {
		return // runner 还没写第一份快照：下轮再读
	}
	// 解析失败的快照不判失败、下轮重读（C-5.3.2：runner 原子改写，读到的是完整快照）。
	var doc domain.DeployDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.logf("orchestrator: WARN deploy_state parse %s: %v", src, err)
		return
	}
	ref := fmt.Sprintf("attempts/%d/deploy-state.json", rr.AttemptNo)
	// 归档副本（temp + rename 原子落盘，产物直落 NFS）。
	dst := s.taskDir(rr.TaskID) + "/" + ref
	if err := osWriteFileAtomic(dst, raw); err != nil {
		s.logf("orchestrator: WARN deploy_state archive %s: %v", dst, err)
		return
	}
	if err := s.st.RecordDeployState(ctx, rr.TaskID, rr.AttemptNo, ref, doc); err != nil {
		s.logf("orchestrator: WARN deploy_state record %s: %v", rr.TaskID, err)
	}
}

// releaseStaleLeases 清理"Task 已终态但租约还在"的僵尸行（C-4.3 泄漏兜底）。
func (s *Scheduler) releaseStaleLeases(ctx context.Context) {
	leases, err := s.st.Leases.List(ctx, nil)
	if err != nil {
		s.logf("orchestrator: list leases: %v", err)
		return
	}
	for _, l := range leases {
		t, err := s.st.Tasks.Get(ctx, nil, l.TaskID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				if derr := s.st.Leases.DeleteByTask(ctx, nil, l.TaskID); derr == nil {
					s.logf("orchestrator: released lease of deleted task %s", l.TaskID)
				}
			}
			continue
		}
		if domain.TaskStatus(t.Status).IsTerminal() {
			if err := s.st.Leases.DeleteByTask(ctx, nil, l.TaskID); err != nil {
				s.logf("orchestrator: release stale lease %s: %v", l.TaskID, err)
				continue
			}
			s.logf("orchestrator: released stale lease held by task %s", l.TaskID)
		}
	}
}

// ---- 取消（C-6.1）----------------------------------------------------------

// CancelTask 取消 Task：store.CancelTask 落 cancelling + 批量 SKIPPED（传播到 clean 为止）
// + cancel_requested；本层异步删活跃 Job → runner 落 canceled → 推进 clean。
func (s *Scheduler) CancelTask(ctx context.Context, taskID, actor string) error {
	active, err := s.st.CancelTask(ctx, taskID, actor)
	if err != nil {
		return err
	}
	for _, k := range active {
		s.cancelActiveRunner(ctx, k)
	}
	return s.settleCancel(ctx, taskID)
}

// cancelActiveRunner 删 Job 并把执行记录落成 canceled（尽力而为，不阻塞）。
func (s *Scheduler) cancelActiveRunner(ctx context.Context, k domain.RunnerKey) {
	rr, err := s.st.RunnerRuns.Get(ctx, nil, k)
	if err != nil {
		return
	}
	jobID := rr.JobID
	s.cancelJobAsync(ctx, jobID)
	// Job 已请求删除：CAS preparing|running → canceled（竞态下先落库者胜）。
	if _, err := s.mach.Runners.Cancel(ctx, k, "system:orchestrator"); err != nil {
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPrecondition) {
			s.logf("orchestrator: cancel runner %s: %v", k.ID(), err)
		}
	}
}

// settleCancel 是取消与对账共用的收口：把 cancelling Task 的管道推进到 clean 为止；
// 全部终态且没有 clean 可执行时直接落 canceled。
func (s *Scheduler) settleCancel(ctx context.Context, taskID string) error {
	t, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		return err
	}
	if domain.TaskStatus(t.Status) != domain.TaskCancelling {
		return nil
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, taskID, t.CurrentAttempt)
	if err != nil {
		return err
	}
	// 已 queued 的交给调度器；找第一个 pending（effects 保证只可能是 clean）。
	for _, rr := range runs {
		if rr.Status == string(domain.RunnerQueued) {
			return nil
		}
	}
	for _, rr := range runs {
		if rr.Status == string(domain.RunnerPending) {
			if _, err := s.mach.Runners.Queue(ctx, rr.Key()); err != nil &&
				!errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrPrecondition) {
				return err
			}
			return nil
		}
	}
	// 没有可推进的 runner（外接模式无 clean / clean 已终态）→ 落 canceled。
	if _, err := s.mach.Tasks.Canceled(ctx, taskID, "system:orchestrator"); err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrPrecondition) {
			return nil
		}
		return err
	}
	s.logf("orchestrator: task %s canceled (nothing left to clean)", taskID)
	return nil
}

// settleCancelling 对账兜底：扫描 cancelling 的 Task 并收口（Job 删除可能在上个进程里丢了）。
func (s *Scheduler) settleCancelling(ctx context.Context) error {
	tasks, _, err := s.st.Tasks.List(ctx, nil, store.TaskFilter{
		Statuses: []string{string(domain.TaskCancelling)},
		Limit:    s.cfg.MaxCandidates,
	})
	if err != nil {
		return err
	}
	for i := range tasks {
		if err := s.settleCancel(ctx, tasks[i].ID); err != nil {
			s.logf("orchestrator: settle cancelling %s: %v", tasks[i].ID, err)
		}
	}
	return nil
}

// cancelJobAsync 异步删除 Job（Foreground 兜底；失败只记 CLEANUP_FAILED 语义的 WARN，
// 不回滚状态，C-6.4）。
func (s *Scheduler) cancelJobAsync(parent context.Context, jobID string) {
	if jobID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(parent, jobCancelTimeout)
		defer cancel()
		h := ports.JobHandle{Namespace: s.cfg.Namespace, JobName: jobID}
		if err := s.exec.Cancel(ctx, h); err != nil {
			s.logf("orchestrator: WARN cleanup_failed: cancel job %s: %v", jobID, err)
			if derr := s.exec.Delete(ctx, h, ports.DeleteForeground); derr != nil {
				s.logf("orchestrator: WARN cleanup_failed: foreground delete job %s: %v", jobID, derr)
			}
		}
	}()
}

// ---- 整 Task 重试（C-5.6）--------------------------------------------------

// RetryTask 执行整 Task 重试：新 attempt 全量重跑含 fresh deploy。
// 错误映射：store.ErrRetryConflict → 409 CONFLICT；ATTEMPT_EXHAUSTED 原样透传（409）。
func (s *Scheduler) RetryTask(ctx context.Context, taskID, actor, role string) (int, error) {
	no, err := s.st.RetryTask(ctx, taskID, actor, role, s.cfg.AttemptMax)
	if err != nil {
		if errors.Is(err, store.ErrRetryConflict) {
			return 0, domain.NewFailureError(domain.CodeConflict,
				"Task 尚未结束（或已取消），不能重试", map[string]any{"task_id": taskID})
		}
		return 0, err
	}
	s.logf("orchestrator: task %s retry accepted: attempt %d", taskID, no)
	return no, nil
}

// ---- 泄漏重放（§9.3 / D-72）------------------------------------------------

// Clean replay 的 409 原因（openapi replayTaskClean.detail.reason）。
const (
	ReplayReasonNoDeploy             = "no_deploy"
	ReplayReasonCleanAlreadyVerified = "clean_already_verified"
	ReplayReasonTaskRunning          = "task_running"
	ReplayReasonReplayInProgress     = "clean_replay_in_progress"
	// ReplayReasonNothingToClean 是 deploy.provider=existing 的拒绝原因：existing-deploy
	// 只做已有服务登记（读三元组、原样导出），没有任何平台可拆除对象，clean 不注入，
	// 重放也无从谈起（§9.1）。
	ReplayReasonNothingToClean = "nothing_to_clean"
)

// ReplayCleanResult 是泄漏重放的受理结果。
type ReplayCleanResult struct {
	Key        domain.RunnerKey
	Name       string // 恒 service-clean-replay
	RunnerName string // 恒 service-clean（内置 Runner）
	Status     domain.RunnerStatus
	Accepted   bool
}

// ErrCleanReplayConflict 是泄漏重放前置条件不满足（API 层映射 409 + detail.reason）。
type ErrCleanReplayConflict struct {
	Reason string
	Detail map[string]any
}

func (e *ErrCleanReplayConflict) Error() string {
	return "orchestrator: clean replay conflict: " + e.Reason
}

// ReplayClean 重放 service-clean（泄漏兜底）：用冻结快照在该 attempt 内追加一条
// `service-clean-replay` 执行记录并立即调度执行。重放失败不改 Task 终态。
func (s *Scheduler) ReplayClean(ctx context.Context, taskID string, attemptNo int, force bool, actor, role string) (*ReplayCleanResult, error) {
	t, err := s.st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		return nil, err
	}
	// existing 变体入口即拒绝：无可拆除对象（existing-deploy 原样导出三元组，无 clean 阶段）。
	if deployProviderOf(t) == "existing" {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonNothingToClean,
			Detail: map[string]any{"reason": ReplayReasonNothingToClean, "task_id": taskID,
				"provider": "existing", "detail": "provider=existing 只做已有服务登记，没有任何可拆除对象"}}
	}
	if !domain.TaskStatus(t.Status).IsTerminal() {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonTaskRunning,
			Detail: map[string]any{"reason": ReplayReasonTaskRunning, "task_id": taskID, "status": t.Status}}
	}
	if attemptNo <= 0 {
		attemptNo = t.CurrentAttempt
	}
	if attemptNo < 1 || attemptNo > t.CurrentAttempt {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonNoDeploy,
			Detail: map[string]any{"reason": ReplayReasonNoDeploy, "task_id": taskID, "attempt_no": attemptNo}}
	}
	runs, err := s.st.RunnerRuns.ListByAttempt(ctx, nil, taskID, attemptNo)
	if err != nil {
		return nil, err
	}
	var deployRow, cleanRow, replayRow *store.RunnerRun
	maxSeq := 0
	for i := range runs {
		rr := &runs[i]
		if rr.Seq > maxSeq {
			maxSeq = rr.Seq
		}
		switch {
		case rr.Role == domain.RoleDeploy:
			deployRow = rr
		case rr.Role == domain.RoleClean:
			cleanRow = rr
		case rr.Name == "service-clean-replay":
			replayRow = rr
		}
	}
	if deployRow == nil {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonNoDeploy,
			Detail: map[string]any{"reason": ReplayReasonNoDeploy, "task_id": taskID, "attempt_no": attemptNo}}
	}
	if cleanRow != nil && cleanRow.Status == string(domain.RunnerSucceeded) && !force {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonCleanAlreadyVerified,
			Detail: map[string]any{"reason": ReplayReasonCleanAlreadyVerified, "task_id": taskID, "attempt_no": attemptNo}}
	}
	if replayRow != nil && !domain.RunnerStatus(replayRow.Status).IsTerminal() {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonReplayInProgress,
			Detail: map[string]any{"reason": ReplayReasonReplayInProgress, "task_id": taskID, "attempt_no": attemptNo}}
	}
	if cleanRow == nil {
		return nil, &ErrCleanReplayConflict{Reason: ReplayReasonNoDeploy,
			Detail: map[string]any{"reason": ReplayReasonNoDeploy, "task_id": taskID, "attempt_no": attemptNo,
				"detail": "该 attempt 没有 service-clean 执行记录（无法复用冻结快照）"}}
	}

	// 追加执行记录：复用原 clean 的冻结快照（runner/镜像/profile/输入/超时/on_failure）。
	k := domain.RunnerKey{TaskID: taskID, AttemptNo: attemptNo, Seq: maxSeq + 1}
	row := *cleanRow
	row.TaskID = taskID
	row.AttemptNo = attemptNo
	row.Seq = k.Seq
	row.Name = "service-clean-replay"
	row.Status = string(domain.RunnerPending)
	row.StatusReason = nil
	row.WaitingReason = nil
	row.ExitCode = nil
	row.ErrorCode = nil
	row.Error = nil
	row.ExportsRef = nil
	row.ResultRef = nil
	row.LogRef = nil
	row.Phase = nil
	row.ImageDigest = nil
	row.ExecutorRef = nil
	row.CancelRequested = false
	row.StartedAt = nil
	row.FinishedAt = nil
	row.CreatedAt = s.now()
	row.UpdatedAt = s.now()
	row.JobID = store.JobName(k)
	row.JobNamespace = nil

	err = s.st.InTx(ctx, func(tx pgx.Tx) error {
		if err := s.st.RunnerRuns.Insert(ctx, tx, &row); err != nil {
			return err
		}
		return s.st.Audits.Insert(ctx, tx, store.AuditEntry{
			Actor: actor, Role: firstNonEmpty(role, "operator"),
			Action: "clean_replay", ObjectType: "task", ObjectID: taskID,
			Detail: map[string]any{"result": "ok", "attempt_no": attemptNo, "seq": k.Seq},
		})
	})
	if err != nil {
		return nil, err
	}

	// 终态 Task 不会被调度器的 Candidates 命中（JOIN tasks status='running'）：
	// 重放记录由本调用自己推完 queued → claim → submit → watch。
	if _, err := s.mach.Runners.Queue(ctx, k); err != nil {
		return nil, fmt.Errorf("orchestrator: queue clean replay %s: %w", k.ID(), err)
	}
	s.dispatchOneByKey(ctx, k)
	return &ReplayCleanResult{Key: k, Name: row.Name, RunnerName: row.RunnerName,
		Status: domain.RunnerQueued, Accepted: true}, nil
}

// dispatchOneByKey 按三元组调度一条执行记录（泄漏重放专用入口）。
func (s *Scheduler) dispatchOneByKey(ctx context.Context, k domain.RunnerKey) {
	rr, err := s.st.RunnerRuns.Get(ctx, nil, k)
	if err != nil {
		s.logf("orchestrator: dispatch replay %s: %v", k.ID(), err)
		return
	}
	jobID := rr.JobID
	ref, _ := json.Marshal(map[string]string{
		"cluster":   s.cfg.ClusterName,
		"namespace": s.cfg.Namespace,
		"job":       jobID,
	})
	claimed, ok, reason, err := s.st.ClaimForExecution(ctx, store.ClaimRequest{
		Key:          k,
		GlobalLimit:  s.cfg.MaxGlobalConcurrency,
		JobID:        jobID,
		JobNamespace: s.cfg.Namespace,
		ExecutorRef:  ref,
	})
	if err != nil {
		s.logf("orchestrator: claim replay %s: %v", k.ID(), err)
		return
	}
	if !ok {
		if reason != "" {
			_ = s.st.RunnerRuns.SetWaitingReason(ctx, nil, k, reason)
		}
		return
	}
	if err := s.launchClaimed(ctx, k, rr, claimed.JobName); err != nil {
		s.logf("orchestrator: launch replay %s: %v", k.ID(), err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
