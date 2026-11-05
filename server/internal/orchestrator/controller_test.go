package orchestrator

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/genet/mocks"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/report"
)

// fakeExec 记录 Submit 并允许测试直接驱动终态回调。
type fakeExec struct {
	mu        sync.Mutex
	logContent string
	logErr     error
	submitted []ports.JobSpec
	handles   []ports.JobHandle
	canceled  []ports.JobHandle
	submitErr error
}

func (f *fakeExec) Submit(ctx context.Context, spec ports.JobSpec) (ports.JobHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return ports.JobHandle{}, f.submitErr
	}
	h := ports.JobHandle{
		Namespace: spec.Namespace,
		JobName:   executor.Naming(spec.RunID, spec.TaskID, spec.AttemptNo),
	}
	f.submitted = append(f.submitted, spec)
	f.handles = append(f.handles, h)
	return h, nil
}

func (f *fakeExec) Watch(ctx context.Context, h ports.JobHandle, obs ports.JobObserver) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeExec) Cancel(ctx context.Context, h ports.JobHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, h)
	return nil
}

func (f *fakeExec) Logs(ctx context.Context, h ports.JobHandle) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logErr != nil {
		return nil, f.logErr
	}
	content := f.logContent
	if content == "" {
		content = "tester says hello\n"
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeExec) specs() []ports.JobSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.JobSpec, len(f.submitted))
	copy(out, f.submitted)
	return out
}

func newTestController(t *testing.T, repos Repos, exec ports.Executor, cfg Config, dep ports.Deployer, val ports.ResultValidator) *Controller {
	t.Helper()
	cfg.Namespace = "testplane"
	cfg.StoragePVC = "testplane-artifacts"
	cfg.StorageRoot = "/storage-root"
	c, err := New(Options{Repos: repos, Executor: exec, Deployer: dep, Validator: val, Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// seedRunTesting 造一个已进入 testing 的 Run 与其 Task。
func seedRunTesting(m *MemoryRepos, runID string, tasks ...TaskView) {
	m.SeedRun(RunView{
		ID:            runID,
		Status:        domain.RunTesting,
		CleanupPolicy: "always",
		Endpoint:      &domain.Endpoint{Key: "http://ep:8000", BaseURL: "http://ep:8000/v1", ModelName: "m"},
	})
	for _, task := range tasks {
		task.RunID = runID
		m.SeedTask(task)
	}
}

func TestDispatchSubmitsJobFromTask(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1", TaskView{
		ID: "t-1", Seq: 1, Name: "load", TesterName: "load-tester", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "registry/load:1.0.0", Entry: "/tester/run.sh",
		Args: []string{"--qps=10", "--url=http://ep:8000/v1"},
		Env:  map[string]string{"A": "b"}, Status: domain.TaskQueued, EndpointKey: "http://ep:8000",
		TimeoutSeconds: 600,
	})
	c := newTestController(t, repos, exec, Config{}, nil, nil)

	if err := c.DispatchOnce(context.Background()); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	specs := exec.specs()
	if len(specs) != 1 {
		t.Fatalf("submitted = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.RunID != "r-1" || spec.TaskID != "t-1" || spec.AttemptNo != 1 {
		t.Errorf("spec ids = %s/%s/%d", spec.RunID, spec.TaskID, spec.AttemptNo)
	}
	if spec.AttemptID != "t-1-a1" {
		t.Errorf("attempt id = %q", spec.AttemptID)
	}
	if spec.StoragePVC != "testplane-artifacts" || spec.TimeoutSeconds != 600 {
		t.Errorf("spec = %+v", spec)
	}
	if want := "/storage-root/r-1/tasks/t-1/attempts/1"; spec.OutputDir != want {
		t.Errorf("output dir = %q, want %q", spec.OutputDir, want)
	}

	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Status != domain.TaskRunning {
		t.Errorf("task status = %s, want running (T5)", task.Status)
	}
}

func TestPrecheckGateBlocksOtherTasksThenReleases(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "precheck", TesterName: "endpoint-probe", TesterVersion: "1.0.0",
			TesterRole: domain.RolePreCheck, Image: "registry/probe:1.0.0", Entry: "/tester/run.sh",
			OnFailure: domain.OnFailureAbortRun, Status: domain.TaskQueued,
			EndpointKey: "http://ep:8000", Exclusive: true, TimeoutSeconds: 900,
		},
		TaskView{
			ID: "t-1", Seq: 1, Name: "load", TesterName: "load-tester", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "registry/load:1.0.0", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
		},
	)
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()

	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if got := len(exec.specs()); got != 1 {
		t.Fatalf("submitted = %d, want only the pre_check", got)
	}
	if exec.specs()[0].TesterRole != domain.RolePreCheck {
		t.Fatalf("first submitted = %s, want pre_check", exec.specs()[0].TesterRole)
	}
	blocked, _ := repos.GetTask(ctx, "t-1")
	if blocked.Status != domain.TaskQueued {
		t.Fatalf("t-1 status = %s, want queued", blocked.Status)
	}
	if blocked.WaitingReason != domain.WaitPrecheckGate {
		t.Fatalf("t-1 waiting_reason = %q, want %q", blocked.WaitingReason, domain.WaitPrecheckGate)
	}

	// Pre-Check 成功 → 其余 Task 入队 → 下一轮被调度。
	attempts := repos.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v", attempts)
	}
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle:     ports.JobHandle{Namespace: "testplane", JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:      executor.PhaseSucceeded,
		FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	unblocked, _ := repos.GetTask(ctx, "t-1")
	if unblocked.Status != domain.TaskQueued {
		t.Fatalf("t-1 status = %s, want queued after precheck success", unblocked.Status)
	}
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatalf("second DispatchOnce: %v", err)
	}
	if got := len(exec.specs()); got != 2 {
		t.Fatalf("submitted = %d, want 2", got)
	}
}

func TestExclusiveEndpointBlocksSharedTask(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "perf", TesterName: "perf", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep:8000", Exclusive: true, TimeoutSeconds: 600,
		},
		TaskView{
			ID: "t-1", Seq: 1, Name: "shared", TesterName: "shared", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
		},
	)
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()

	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(exec.specs()); got != 1 {
		t.Fatalf("submitted = %d, want 1 (exclusive holder blocks the other)", got)
	}
	if exec.specs()[0].TaskID != "t-0" {
		t.Fatalf("submitted task = %s", exec.specs()[0].TaskID)
	}
	blocked, _ := repos.GetTask(ctx, "t-1")
	if blocked.WaitingReason != domain.WaitSlotWaiting {
		t.Fatalf("waiting_reason = %q, want slot_waiting", blocked.WaitingReason)
	}

	// 独占 Attempt 结束 → 租约释放 → 共享 Task 可并行。
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:  executor.PhaseSucceeded, FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if leases := repos.Leases(); len(leases) != 0 {
		t.Fatalf("leases must be released when the attempt finishes: %+v", leases)
	}
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(exec.specs()); got != 2 {
		t.Fatalf("submitted = %d, want 2", got)
	}
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "a", TesterName: "a", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep-a:8000", TimeoutSeconds: 600,
		},
		TaskView{
			ID: "t-1", Seq: 1, Name: "b", TesterName: "b", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep-b:8000", TimeoutSeconds: 600,
		},
	)
	c := newTestController(t, repos, exec, Config{MaxGlobalConcurrency: 1}, nil, nil)
	ctx := context.Background()

	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(exec.specs()); got != 1 {
		t.Fatalf("submitted = %d, want 1 due to global limit", got)
	}
	tasks, _ := repos.ListTasks(ctx, "r-1")
	var waiting string
	for _, task := range tasks {
		if task.Status == domain.TaskQueued {
			waiting = task.WaitingReason
		}
	}
	if waiting != domain.WaitGlobalLimit {
		t.Fatalf("waiting_reason = %q, want global_limit", waiting)
	}
}

func TestCancelRunReleasesLeasesAndCancelsJobs(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1", TaskView{
		ID: "t-0", Seq: 0, Name: "a", TesterName: "a", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
	})
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()

	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repos.Leases()) != 1 {
		t.Fatalf("lease must be held after dispatch")
	}

	if err := c.CancelRun(ctx, "r-1", "user:1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	waitFor(t, func() bool { return len(repos.Leases()) == 0 }, "lease release after cancel")
	waitFor(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return len(exec.canceled) == 1
	}, "job cancel after CancelRun")

	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunCanceled {
		t.Errorf("run status = %s, want canceled", run.Status)
	}
	task, _ := repos.GetTask(ctx, "t-0")
	if task.Status != domain.TaskCanceled {
		t.Errorf("task status = %s, want canceled", task.Status)
	}
}

func TestPrecheckFailureAbortsRunAsServiceUnavailable(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "precheck", TesterName: "endpoint-probe", TesterVersion: "1.0.0",
			TesterRole: domain.RolePreCheck, Image: "i:1", Entry: "/tester/run.sh",
			OnFailure: domain.OnFailureAbortRun, Status: domain.TaskQueued,
			EndpointKey: "http://ep:8000", Exclusive: true, TimeoutSeconds: 900,
		},
		TaskView{
			ID: "t-1", Seq: 1, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskPending, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
		},
	)
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Pre-Check 以"可重试"错误失败 → 其余 skipped(upstream_abort)、Run service_unavailable（C-5.8 第 4 条）。
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:  executor.PhaseFailed,
		Message: executor.CodeImagePullFailed + ": fake failure",
		FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// ImagePull 失败码不可重试 → completed_with_errors；用可重试的 ENDPOINT_UNREACHABLE 验证第 4 条。
	repos2 := NewMemoryRepos()
	exec2 := &fakeExec{}
	seedRunTesting(repos2, "r-2", TaskView{
		ID: "t-0", Seq: 0, Name: "precheck", TesterName: "endpoint-probe", TesterVersion: "1.0.0",
		TesterRole: domain.RolePreCheck, Image: "i:1", Entry: "/tester/run.sh",
		OnFailure: domain.OnFailureAbortRun, Status: domain.TaskQueued,
		EndpointKey: "http://ep:8000", Exclusive: true, TimeoutSeconds: 900,
	})
	c2 := newTestController(t, repos2, exec2, Config{}, nil, nil)
	if err := c2.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{JobName: executor.Naming("r-2", "t-0", 1)},
		Phase:  executor.PhaseTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	run2, _ := repos2.GetRun(ctx, "r-2")
	if run2.Status != domain.RunServiceUnavailable {
		t.Errorf("run status = %s, want service_unavailable for retryable precheck failure", run2.Status)
	}
}

func TestDeployMockBringsRunToTesting(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunQueued, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(),
	})
	repos.SeedTask(TaskView{
		ID: "t-0", RunID: "r-1", Seq: 0, Name: "precheck", TesterName: "endpoint-probe",
		TesterVersion: "1.0.0", TesterRole: domain.RolePreCheck, Image: "i:1", Entry: "/tester/run.sh",
		OnFailure: domain.OnFailureAbortRun, Status: domain.TaskPending, Exclusive: true, TimeoutSeconds: 900,
	})
	repos.SeedTask(TaskView{
		ID: "t-1", RunID: "r-1", Seq: 1, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		Status: domain.TaskPending, TimeoutSeconds: 600,
	})
	// 用可控时钟把三段式进度显式走一遍：queued → deploying → ready。
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	deployer := mocks.New(mocks.Options{
		Duration: 3 * time.Second,
		Endpoint: domain.Endpoint{BaseURL: "http://mock-llm:8080/v1", ModelName: "mock-model"},
		Now:      clock.Now,
	})
	c := newTestController(t, repos, exec, Config{}, deployer, nil)
	ctx := context.Background()

	// 第一轮：queued → deploying + Genet Start（本轮还会 Poll 一次，状态 queued）。
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunDeploying {
		t.Fatalf("run status = %s, want deploying", run.Status)
	}

	// 第二轮：模拟时间推进到 2s → deploying。
	clock.Advance(2 * time.Second)
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ = repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunDeploying || run.DeployState != "deploying" {
		t.Fatalf("run = %s/%s, want deploying/deploying", run.Status, run.DeployState)
	}

	// 第三轮：时间推进过 3s → ready → testing + 回填 endpoint_key + 只放 seq=0。
	clock.Advance(2 * time.Second)
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ = repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunTesting {
		t.Fatalf("run status = %s, want testing", run.Status)
	}
	if run.Endpoint == nil || run.Endpoint.BaseURL != "http://mock-llm:8080/v1" {
		t.Fatalf("endpoint = %+v", run.Endpoint)
	}
	tasks, _ := repos.ListTasks(ctx, "r-1")
	byID := map[string]TaskView{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if byID["t-0"].Status != domain.TaskQueued {
		t.Errorf("precheck status = %s, want queued", byID["t-0"].Status)
	}
	if byID["t-1"].Status != domain.TaskPending {
		t.Errorf("test task status = %s, want pending (gated by precheck)", byID["t-1"].Status)
	}
	if byID["t-1"].EndpointKey == "" {
		t.Errorf("endpoint_key must be backfilled for every task")
	}
}

func TestDeployFailureSkipsTasks(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunQueued, HasDeploy: true, SubmittedAt: time.Now()})
	repos.SeedTask(TaskView{
		ID: "t-0", RunID: "r-1", Seq: 0, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		Status: domain.TaskPending, TimeoutSeconds: 600,
	})
	deployer := mocks.New(mocks.Options{Blocked: true})
	c := newTestController(t, repos, exec, Config{}, deployer, nil)
	ctx := context.Background()
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunServiceUnavailable {
		t.Fatalf("run status = %s, want service_unavailable", run.Status)
	}
	task, _ := repos.GetTask(ctx, "t-0")
	if task.Status != domain.TaskSkipped || task.StatusReason != domain.SkipServiceUnavailable {
		t.Fatalf("task = %s/%s, want skipped/service_unavailable", task.Status, task.StatusReason)
	}
}

func TestWatchdogTimesOutAttempt(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1", TaskView{
		ID: "t-0", Seq: 0, Name: "a", TesterName: "a", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 1,
	})
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 让 started_at 落在过去。
	repos.mu.Lock()
	for _, a := range repos.attempts {
		a.StartedAt = time.Now().Add(-time.Hour)
	}
	repos.mu.Unlock()

	if err := c.WatchdogOnce(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := repos.GetTask(ctx, "t-0")
	if task.Status != domain.TaskFailed {
		t.Fatalf("task = %s, want failed", task.Status)
	}
	if task.Failure == nil || task.Failure.Code != domain.CodeTaskTimeout {
		t.Fatalf("failure = %+v, want TASK_TIMEOUT", task.Failure)
	}
}

func TestFullHappyPathFinishesRun(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "precheck", TesterName: "endpoint-probe", TesterVersion: "1.0.0",
			TesterRole: domain.RolePreCheck, Image: "i:1", Entry: "/tester/run.sh",
			OnFailure: domain.OnFailureAbortRun, Status: domain.TaskQueued,
			EndpointKey: "http://ep:8000", Exclusive: true, TimeoutSeconds: 900,
		},
		TaskView{
			ID: "t-1", Seq: 1, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
			TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
			Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
		},
	)
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := c.DispatchOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 两次成功回调。
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:  executor.PhaseSucceeded, FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{JobName: executor.Naming("r-1", "t-1", 1)},
		Phase:  executor.PhaseSucceeded, FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunSucceeded {
		t.Fatalf("run status = %s, want succeeded", run.Status)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// ---- 对账（C-3.5）单测：Job 缺失 / Job Failed / Job Success ----------------

// seedReconcileFixture 造一个"库里 running、持租约"的僵尸 Task。
func seedReconcileFixture(repos *MemoryRepos, runID, taskID, endpointKey string) AttemptRef {
	repos.SeedRun(RunView{
		ID: runID, Status: domain.RunTesting, CleanupPolicy: "always",
		Endpoint: &domain.Endpoint{Key: endpointKey, BaseURL: endpointKey + "/v1", ModelName: "m"},
	})
	ref := AttemptRef{
		AttemptID: taskID + "-a1",
		TaskID:    taskID,
		RunID:     runID,
		AttemptNo: 1,
		JobName:   executor.Naming(runID, taskID, 1),
	}
	repos.SeedActiveTask(TaskView{
		ID: taskID, RunID: runID, Seq: 0, Name: "precheck",
		TesterName: "endpoint-probe", TesterVersion: "1.0.0", TesterRole: domain.RolePreCheck,
		Image: "i:1", Entry: "/tester/run.sh", OnFailure: domain.OnFailureAbortRun,
		Status: domain.TaskRunning, EndpointKey: endpointKey, Exclusive: true, TimeoutSeconds: 900,
	}, ref, endpointKey, time.Now())
	return ref
}

func assertTaskAndLease(t *testing.T, repos *MemoryRepos, taskID string, wantStatus domain.TaskStatus, wantCode string) {
	t.Helper()
	task, err := repos.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != wantStatus {
		t.Fatalf("task status = %s, want %s", task.Status, wantStatus)
	}
	if wantCode != "" {
		if task.Failure == nil || task.Failure.Code != wantCode {
			t.Fatalf("failure = %+v, want code %s", task.Failure, wantCode)
		}
	}
	if leases := repos.Leases(); len(leases) != 0 {
		t.Fatalf("endpoint lease must be released, still held: %+v", leases)
	}
}

func TestReconcileLostsJobAndReleasesLease(t *testing.T) {
	repos := NewMemoryRepos()
	cs := fake.NewSimpleClientset() // 集群里没有这个 Job
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskFailed, domain.CodeExecutorUnavailable)
}

func TestReconcileFinishesFailedJob(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")

	exit := int32(3)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "testplane",
			Labels: map[string]string{executor.LabelManaged: "true", "job-name": ref.JobName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: "Error",
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(job, pod)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskFailed, domain.CodeTesterNonzeroExit)
}

func TestReconcileFinishesSucceededJob(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")

	now := metav1.Now()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}},
			CompletionTime: &now,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-1", Namespace: "testplane",
			Labels: map[string]string{executor.LabelManaged: "true", "job-name": ref.JobName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				ImageID: "docker-pullable://repo/app@sha256:" + strings.Repeat("ab", 32),
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Reason: "Completed",
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(job, pod)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskSucceeded, "")
}

func TestReconcileLeavesRunningJobAlone(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	cs := fake.NewSimpleClientset(job)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Status != domain.TaskRunning {
		t.Fatalf("running job must not be touched, task = %s", task.Status)
	}
	if len(repos.Leases()) != 1 {
		t.Fatalf("lease must still be held while the job is running")
	}
}

func TestReconcileCleansStaleLeaseOfTerminalTask(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunTesting})
	repos.SeedTask(TaskView{ID: "t-9", RunID: "r-1", Seq: 1, Status: domain.TaskSucceeded})
	// 直接塞一条僵尸租约（模拟旧版本/历史遗留）。
	repos.mu.Lock()
	repos.leases["t-9"] = "http://ep:8000"
	repos.mu.Unlock()

	cs := fake.NewSimpleClientset()
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if leases := repos.Leases(); len(leases) != 0 {
		t.Fatalf("stale lease of a terminal task must be removed: %+v", leases)
	}
}

// ---- 结果校验的存储防御（result_dir_missing）-----------------------------

type missingResultValidator struct{ calls int }

func (v *missingResultValidator) Validate(ctx context.Context, runID, taskID, attemptID, outputDir string) *domain.Failure {
	v.calls++
	return &domain.Failure{Code: domain.CodeResultMissing, Message: "result.json not found: " + outputDir}
}

func seedSucceededJob(t *testing.T, repos *MemoryRepos) (AttemptRef, *fake.Clientset) {
	t.Helper()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	now := metav1.Now()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status: batchv1.JobStatus{
			Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
			CompletionTime: &now,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "testplane", Labels: map[string]string{
			executor.LabelManaged: "true", "job-name": ref.JobName,
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	return ref, fake.NewSimpleClientset(job, pod)
}

// 存储没挂（StorageRoot 为空）：不能冤枉成 RESULT_MISSING，保留退出码成功 + WARN。
func TestReconcileKeepsExitCodeSuccessWhenStorageNotConfigured(t *testing.T) {
	repos := NewMemoryRepos()
	ref, cs := seedSucceededJob(t, repos)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	val := &missingResultValidator{}
	var warned bool
	c, err := New(Options{
		Repos: repos, Executor: ex, Prober: ex, Validator: val,
		Config: Config{StorageRoot: ""},
		Logf: func(format string, args ...any) {
			if strings.Contains(format, "result_dir_missing") {
				warned = true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskSucceeded, "")
	if val.calls != 0 {
		t.Errorf("validator must not run when the storage root is unconfigured")
	}
	if !warned {
		t.Errorf("expected a result_dir_missing warning")
	}
	c.WaitLogCapture()
	_ = ref
}

// 存储挂了但 controller 看不到该 attempt 目录：同样保留退出码成功 + WARN。
func TestReconcileKeepsExitCodeSuccessWhenDirMissing(t *testing.T) {
	repos := NewMemoryRepos()
	_, cs := seedSucceededJob(t, repos)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	val := &missingResultValidator{}
	var warned bool
	root := t.TempDir() // 存在，但里面没有 r-1/tasks/t-1/attempts/1
	c, err := New(Options{
		Repos: repos, Executor: ex, Prober: ex, Validator: val,
		Config: Config{StorageRoot: root},
		Logf: func(format string, args ...any) {
			if strings.Contains(format, "result_dir_missing") {
				warned = true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskSucceeded, "")
	if val.calls != 0 {
		t.Errorf("validator must not run when the attempt dir is not visible")
	}
	if !warned {
		t.Errorf("expected a result_dir_missing warning")
	}
	c.WaitLogCapture()
}

// 存储可见但 Tester 没写产物：必须照常判 RESULT_MISSING（产物即事实）。
func TestReconcileEnforcesResultValidatorWhenDirPresent(t *testing.T) {
	repos := NewMemoryRepos()
	ref, cs := seedSucceededJob(t, repos)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	val := &missingResultValidator{}
	root := t.TempDir()
	dir := filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		Repos: repos, Executor: ex, Prober: ex, Validator: val,
		Config: Config{StorageRoot: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskFailed, domain.CodeResultMissing)
	c.WaitLogCapture()
	if val.calls != 1 {
		t.Errorf("validator calls = %d, want 1", val.calls)
	}
	if ref.JobName == "" {
		t.Errorf("job name should be set")
	}
}

// ---- 镜像拉取失败：对账必须立即收口（execution §4.3/§4.5）----------------

// ImagePullBackOff 的 Pod 不会让 Job 进入终态，只能等 activeDeadlineSeconds 或对账；
// 对账必须能识别 Pod waiting.reason 并收口成 IMAGE_PULL_FAILED（可重试码）。
func TestReconcileFailsImagePullBackOff(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status:     batchv1.JobStatus{Active: 1}, // Job 本身还没终态
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "testplane", Labels: map[string]string{
			executor.LabelManaged: "true", "job-name": ref.JobName,
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: `Back-off pulling image "img:1"`,
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(job, pod)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskFailed, domain.CodeImagePullFailed)
}

// initContainer 卡在 ErrImagePull 时同样要收口（假镜像 QA 场景常见形态）。
func TestReconcileFailsImagePullOnInitContainer(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "testplane", Labels: map[string]string{
			executor.LabelManaged: "true", "job-name": ref.JobName,
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ErrImagePull",
					Message: `Failed to pull image "img:1"`,
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(job, pod)
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertTaskAndLease(t, repos, "t-1", domain.TaskFailed, domain.CodeImagePullFailed)
}

// ---- New 的默认值兜底：所有 ticker 周期都必须 > 0（漏配 = 启动即 panic）----

func TestNewFillsAllIntervals(t *testing.T) {
	repos := NewMemoryRepos()
	c, err := New(Options{Repos: repos, Executor: &fakeExec{}}) // Config 全零
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := c.Config()
	for name, d := range map[string]time.Duration{
		"DispatchInterval":  cfg.DispatchInterval,
		"DeployInterval":    cfg.DeployInterval,
		"WatchdogInterval":  cfg.WatchdogInterval,
		"ReconcileInterval": cfg.ReconcileInterval,
	} {
		if d <= 0 {
			t.Errorf("%s = %v, must be > 0 (time.NewTicker panics on non-positive interval)", name, d)
		}
	}
	// 这些不是 ticker，但也必须有合理默认值。
	if cfg.MaxGlobalConcurrency <= 0 || cfg.TaskTimeoutDefault <= 0 || cfg.DeployTimeout <= 0 || cfg.MaxCandidates <= 0 {
		t.Errorf("config defaults incomplete: %+v", cfg)
	}
	// 启动主循环不应 panic（短周期跑一小会儿就取消）。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c2, err := New(Options{Repos: repos, Executor: &fakeExec{}, Config: Config{
		DispatchInterval: 10 * time.Millisecond, DeployInterval: 10 * time.Millisecond,
		WatchdogInterval: 10 * time.Millisecond, ReconcileInterval: 10 * time.Millisecond,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
}

// ---- C-7.1 第 4 条：非零退出也要读 result.json 归类 ----------------------

const failedResultJSON = `{
  "schema_version": "1.0",
  "run_id": "r-1",
  "task_id": "t-1",
  "attempt_id": "t-1-a1",
  "tester": {"name": "endpoint-probe", "version": "1.0.0", "image_ref": "img:1"},
  "status": "failed",
  "started_at": "2026-11-05T02:00:00Z",
  "finished_at": "2026-11-05T02:00:05Z",
  "metrics": [],
  "notes": "mock 持续 500",
  "error": {
    "code": "PRECHECK_FAILED",
    "message": "服务在窗口内一直返回 500",
    "retryable": true,
    "detail": {"http_status": 500}
  }
}`

// writeAttemptResult 在 storage root 下写出 attempt 目录与 result.json。
func writeAttemptResult(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNonZeroExitUsesResultJSONErrorCode(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	root := t.TempDir()
	writeAttemptResult(t, root, failedResultJSON)

	ex := &fakeExec{}
	c, err := New(Options{
		Repos: repos, Executor: ex, Validator: &report.Validator{},
		Config: Config{StorageRoot: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := int32(1)
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle:   ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:    executor.PhaseFailed,
		ExitCode: &exit,
		Message:  "TESTER_NONZERO_EXIT: exit code 1",
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()

	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Status != domain.TaskFailed {
		t.Fatalf("task status = %s", task.Status)
	}
	if task.Failure == nil || task.Failure.Code != domain.CodePrecheckFailed {
		t.Fatalf("failure = %+v, want PRECHECK_FAILED (from result.json, not TESTER_NONZERO_EXIT)", task.Failure)
	}
	if !task.Failure.Retryable {
		t.Errorf("retryable must come from result.json (true)")
	}
	if got := task.Failure.Detail["http_status"]; got != float64(500) {
		t.Errorf("detail.http_status = %v, want 500", got)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunServiceUnavailable {
		t.Errorf("run status = %s, want service_unavailable (C-5.8 #4)", run.Status)
	}
	if len(repos.Leases()) != 0 {
		t.Errorf("lease must be released")
	}
}

func TestNonZeroExitWithoutResultJSONFallsBack(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	root := t.TempDir()
	writeAttemptResult(t, root, "") // 目录存在但没有 result.json

	ex := &fakeExec{}
	c, err := New(Options{
		Repos: repos, Executor: ex, Validator: &report.Validator{},
		Config: Config{StorageRoot: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := int32(2)
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle:   ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:    executor.PhaseFailed,
		ExitCode: &exit,
		Message:  "TESTER_NONZERO_EXIT: exit code 2",
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()

	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Failure == nil || task.Failure.Code != domain.CodeTesterNonzeroExit {
		t.Fatalf("failure = %+v, want TESTER_NONZERO_EXIT fallback", task.Failure)
	}
	if task.Failure.Retryable {
		t.Errorf("TESTER_NONZERO_EXIT must not be retryable")
	}
}

// ---- 日志落盘：<attempt>/logs/stdout.log ---------------------------------

func TestAttemptLogsAreFlushedToStorage(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	root := t.TempDir()

	ex := &fakeExec{logContent: "probe: HTTP 200 ok\nprobe: done\n"}
	c, err := New(Options{Repos: repos, Executor: ex, Config: Config{StorageRoot: root}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle: ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:  executor.PhaseSucceeded,
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()

	logPath := filepath.Join(root, "r-1", "tasks", "t-1", "attempts", "1", "logs", "stdout.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read %s: %v", logPath, err)
	}
	if string(raw) != ex.logContent {
		t.Errorf("stdout.log = %q, want %q", string(raw), ex.logContent)
	}
	if _, err := os.Stat(logPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file must be renamed away")
	}
}

// 日志拉取失败只 WARN，不影响 Task 状态。
func TestAttemptLogFailureDoesNotAffectStatus(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	root := t.TempDir()
	ex := &fakeExec{logErr: errors.New("pod not found")}
	var warned bool
	c, err := New(Options{
		Repos: repos, Executor: ex, Config: Config{StorageRoot: root},
		Logf: func(format string, args ...any) {
			if strings.Contains(format, "attempt_logs_failed") {
				warned = true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle: ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:  executor.PhaseSucceeded,
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()
	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Status != domain.TaskSucceeded {
		t.Fatalf("task status = %s, log failure must not change it", task.Status)
	}
	if !warned {
		t.Errorf("expected an attempt_logs_failed warning")
	}
}

// ---- C-5.8 第 4 条：只认实例 retryable（401 / 200-包错误场景）------------

const failedResult401 = `{
  "schema_version": "1.0",
  "run_id": "r-1",
  "task_id": "t-1",
  "attempt_id": "t-1-a1",
  "tester": {"name": "endpoint-probe", "version": "1.0.0", "image_ref": "img:1"},
  "status": "failed",
  "started_at": "2026-11-05T02:00:00Z",
  "finished_at": "2026-11-05T02:00:05Z",
  "metrics": [],
  "notes": "后端返回 401",
  "error": {
    "code": "PRECHECK_FAILED",
    "message": "认证失败：HTTP 401",
    "retryable": false,
    "detail": {"http_status": 401}
  }
}`

// retryable=false 的 precheck 失败（401）→ completed_with_errors，而不是 service_unavailable。
// 码表里有 PRECHECK_FAILED，但**不得**按码抬升（07 §3：终态以实例字段为准）。
func TestPrecheckRetryableFalseGivesCompletedWithErrors(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	root := t.TempDir()
	writeAttemptResult(t, root, failedResult401)

	ex := &fakeExec{}
	c, err := New(Options{
		Repos: repos, Executor: ex, Validator: &report.Validator{},
		Config: Config{StorageRoot: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	exit := int32(2)
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle:   ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:    executor.PhaseFailed,
		ExitCode: &exit,
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()

	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Failure == nil || task.Failure.Code != domain.CodePrecheckFailed {
		t.Fatalf("failure = %+v, want PRECHECK_FAILED", task.Failure)
	}
	if task.Failure.Retryable {
		t.Fatalf("retryable must stay false (Tester said so)")
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunCompletedWithErrors {
		t.Fatalf("run status = %s, want completed_with_errors for retryable=false (C-5.8 #4)", run.Status)
	}
}

// 同一个码、实例 retryable=true → service_unavailable（对照组，防止只改一半）。
func TestPrecheckRetryableTrueGivesServiceUnavailable(t *testing.T) {
	status, failure := TerminalRunStatus([]TaskView{
		{
			ID: "t-1", TesterRole: domain.RolePreCheck, Status: domain.TaskFailed,
			Failure: &domain.Failure{Code: domain.CodePrecheckFailed, Retryable: true},
		},
	})
	if status != domain.RunServiceUnavailable {
		t.Fatalf("status = %s, want service_unavailable", status)
	}
	if failure == nil || failure.Code != domain.CodePrecheckFailed {
		t.Fatalf("failure = %+v", failure)
	}
}

// 表驱动覆盖：实例字段决定终态，码表不参与抬升。
func TestTerminalRunStatusHonoursInstanceRetryable(t *testing.T) {
	cases := []struct {
		name string
		code string
		retr bool
		want domain.RunStatus
	}{
		{"401 precheck", domain.CodePrecheckFailed, false, domain.RunCompletedWithErrors},
		{"200-with-error precheck", domain.CodePrecheckFailed, false, domain.RunCompletedWithErrors},
		{"unreachable precheck", domain.CodeEndpointUnreachable, true, domain.RunServiceUnavailable},
		{"timeout precheck", domain.CodeTaskTimeout, true, domain.RunServiceUnavailable},
		{"auth failure", domain.CodeUnauthenticated, false, domain.RunCompletedWithErrors},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := TerminalRunStatus([]TaskView{{
				ID: "t-0", TesterRole: domain.RolePreCheck, Status: domain.TaskFailed,
				Failure: &domain.Failure{Code: tc.code, Retryable: tc.retr},
			}})
			if status != tc.want {
				t.Fatalf("status = %s, want %s", status, tc.want)
			}
		})
	}
}
