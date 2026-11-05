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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/executor"
	"github.com/ucm-system/testplane/server/internal/genet"
	"github.com/ucm-system/testplane/server/internal/genet/mocks"
	"github.com/ucm-system/testplane/server/internal/ports"
	"github.com/ucm-system/testplane/server/internal/report"
)

// fakeExec 记录 Submit 并允许测试直接驱动终态回调。
type fakeExec struct {
	mu         sync.Mutex
	logContent string
	logErr     error
	submitted  []ports.JobSpec
	handles    []ports.JobHandle
	canceled   []ports.JobHandle
	deleted    []ports.JobHandle
	deleteErr  error
	submitErr  error
	// onDelete 在 Delete 真正执行前被调用（票 #20-7：用来断言"删 Job 时库里 Attempt 已终态"）。
	onDelete func() error
	// holdActive 为 true 时 HoldActive 返回 true（hold-until 守卫用例）。
	holdActive bool
}

// Delete 记录正常结束后的回收调用（execution §6）。
func (f *fakeExec) Delete(ctx context.Context, h ports.JobHandle, policy ports.DeletePolicy) error {
	if f.onDelete != nil {
		if err := f.onDelete(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, h)
	return nil
}

// deleteCount 返回 Delete 的调用次数（测试断言"正常结束后 Job 被删"）。
func (f *fakeExec) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deleted)
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
			TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "registry/probe:1.0.0", Entry: "/tester/run.sh",
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
			TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "i:1", Entry: "/tester/run.sh",
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
		Handle:     ports.JobHandle{JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:      executor.PhaseFailed,
		Message:    executor.CodeImagePullFailed + ": fake failure",
		FinishedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// ImagePull 失败码不可重试 → completed_with_errors；用可重试的 ENDPOINT_UNREACHABLE 验证第 4 条。
	repos2 := NewMemoryRepos()
	exec2 := &fakeExec{}
	seedRunTesting(repos2, "r-2", TaskView{
		ID: "t-0", Seq: 0, Name: "precheck", TesterName: "endpoint-probe", TesterVersion: "1.0.0",
		TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "i:1", Entry: "/tester/run.sh",
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
		TesterVersion: "1.0.0", TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "i:1", Entry: "/tester/run.sh",
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
			TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "i:1", Entry: "/tester/run.sh",
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
		TesterName: "endpoint-probe", TesterVersion: "1.0.0", TesterRole: domain.RolePreCheck, IsPrecheck: true,
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
			ID: "t-1", TesterRole: domain.RolePreCheck, IsPrecheck: true, Status: domain.TaskFailed,
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
				ID: "t-0", TesterRole: domain.RolePreCheck, IsPrecheck: true, Status: domain.TaskFailed,
				Failure: &domain.Failure{Code: tc.code, Retryable: tc.retr},
			}})
			if status != tc.want {
				t.Fatalf("status = %s, want %s", status, tc.want)
			}
		})
	}
}

// ---- C-6.1：取消必须收口到 canceled（不能停在 cancelling）----------------

// queued 阶段取消：Job/Pod 从未创建 → 同一轮直接 canceled。
func TestCancelQueuedRunFinishesCanceled(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1",
		TaskView{
			ID: "t-0", Seq: 0, Name: "precheck", TesterName: "probe", TesterVersion: "1.0.0",
			TesterRole: domain.RolePreCheck, IsPrecheck: true, Image: "i:1", Entry: "/tester/run.sh",
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

	// 一个 Job 都还没建就取消。
	if err := c.CancelRun(ctx, "r-1", "user:1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunCanceled {
		t.Fatalf("run status = %s, want canceled (must not stay cancelling)", run.Status)
	}
	if got := len(exec.specs()); got != 0 {
		t.Fatalf("submitted = %d, want 0", got)
	}
	for _, id := range []string{"t-0", "t-1"} {
		task, _ := repos.GetTask(ctx, id)
		if task.Status.IsTerminal() != true {
			t.Errorf("%s status = %s, want terminal", id, task.Status)
		}
	}
}

// 有 running Attempt 时取消：先 cancelling，Attempt 停止后 canceled。
func TestCancelRunningRunGoesThroughCancellingThenCanceled(t *testing.T) {
	repos := NewMemoryRepos()
	exec := &fakeExec{}
	seedRunTesting(repos, "r-1", TaskView{
		ID: "t-0", Seq: 0, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
	})
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	ctx := context.Background()
	if err := c.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if err := c.CancelRun(ctx, "r-1", "user:1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunCancelling {
		t.Fatalf("run status = %s, want cancelling while an attempt is running", run.Status)
	}

	// Attempt 停止（删 Job 后回调；这里用成功回调模拟"自然结束先到"）。
	if err := c.JobFinished(ctx, ports.JobOutcome{
		Handle: ports.JobHandle{Namespace: "testplane", JobName: executor.Naming("r-1", "t-0", 1)},
		Phase:  executor.PhaseSucceeded,
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture()
	run, _ = repos.GetRun(ctx, "r-1")
	if run.Status != domain.RunCanceled {
		t.Fatalf("run status = %s, want canceled after the attempt stopped", run.Status)
	}
}

// 对账自愈：遗留的 cancelling + Task 全终态 + 无活动 Attempt → canceled。
func TestReconcileFinishesStuckCancellingRun(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-legacy", Status: domain.RunCancelling, SubmittedAt: time.Now()})
	repos.SeedTask(TaskView{
		ID: "t-0", RunID: "r-legacy", Seq: 0, Name: "precheck", TesterRole: domain.RolePreCheck, IsPrecheck: true,
		Status: domain.TaskSkipped, StatusReason: domain.SkipUpstreamAbort,
	})
	repos.SeedTask(TaskView{
		ID: "t-1", RunID: "r-legacy", Seq: 1, Name: "load", TesterRole: domain.RoleTest,
		Status: domain.TaskSkipped, StatusReason: domain.SkipUpstreamAbort,
	})
	// attempts 0 行、endpoint_leases 0 行——正是集群里那 4 条的形态。

	cs := fake.NewSimpleClientset()
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-legacy")
	if run.Status != domain.RunCanceled {
		t.Fatalf("run status = %s, want canceled (reconcile must self-heal stuck cancelling)", run.Status)
	}
}

// 对账不能误杀：cancelling 但还有 running Task 的 Run 必须保持原状等 Attempt 停止。
func TestReconcileKeepsCancellingRunWithActiveTask(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunCancelling, SubmittedAt: time.Now()})
	ref := AttemptRef{AttemptID: "t-0-a1", TaskID: "t-0", RunID: "r-1", AttemptNo: 1,
		JobName: executor.Naming("r-1", "t-0", 1)}
	repos.SeedActiveTask(TaskView{
		ID: "t-0", RunID: "r-1", Seq: 0, Name: "load", TesterName: "load", TesterVersion: "1.0.0",
		TesterRole: domain.RoleTest, Image: "i:1", Entry: "/tester/run.sh",
		EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
	}, ref, "http://ep:8000", time.Now())

	cs := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ref.JobName, Namespace: "testplane"},
		Status:     batchv1.JobStatus{Active: 1},
	})
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{PollInterval: 20 * time.Millisecond})
	c, err := New(Options{Repos: repos, Executor: ex, Prober: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunCancelling {
		t.Fatalf("run status = %s, want cancelling (attempt still running)", run.Status)
	}
}

// Task 侧：无活动 Attempt 时取消直接 canceled，不停在 cancel_requested。
func TestCancelTaskWithoutActiveAttemptFinishesCanceled(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunTesting})
	repos.SeedTask(TaskView{
		ID: "t-0", RunID: "r-1", Seq: 0, Name: "load", TesterRole: domain.RoleTest,
		Status: domain.TaskQueued, EndpointKey: "http://ep:8000", TimeoutSeconds: 600,
	})
	exec := &fakeExec{}
	c := newTestController(t, repos, exec, Config{}, nil, nil)
	if err := c.CancelTask(context.Background(), "t-0", "user:1"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	task, _ := repos.GetTask(context.Background(), "t-0")
	if task.Status != domain.TaskSkipped || task.StatusReason != domain.SkipCanceled {
		t.Fatalf("task = %s/%s, want skipped/canceled", task.Status, task.StatusReason)
	}
	if task.CancelRequested {
		t.Fatalf("cancel_requested must not be left behind")
	}
}

// ---- #15：正常结束后回收 Job + 孤儿 GC -----------------------------------

// 成功终态落库后，Job 必须被 Delete(Background) 掉（且日志先落盘）。
func TestFinishedAttemptDeletesJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase string
	}{
		{"succeeded", executor.PhaseSucceeded},
		{"failed", executor.PhaseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := NewMemoryRepos()
			ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
			exec := &fakeExec{}
			c, err := New(Options{Repos: repos, Executor: exec, Config: Config{StorageRoot: t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			exit := int32(0)
			if tc.phase == executor.PhaseFailed {
				exit = 1
			}
			// 票 #20-7：在 Delete 里断言"库里 Attempt 已终态"（而不是只看 Task 状态）。
			exec.onDelete = func() error {
				repos.mu.Lock()
				defer repos.mu.Unlock()
				if len(repos.attempts) != 1 {
					return fmt.Errorf("attempts = %d, want 1", len(repos.attempts))
				}
				for id, a := range repos.attempts {
					switch a.Status {
					case domain.AttemptSucceeded, domain.AttemptFailed, domain.AttemptCanceled:
					default:
						return fmt.Errorf("attempt %s 在 Delete 时还不是终态：%s", id, a.Status)
					}
				}
				return nil
			}
			if err := c.JobFinished(context.Background(), ports.JobOutcome{
				Handle:   ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
				Phase:    tc.phase,
				ExitCode: &exit,
			}); err != nil {
				t.Fatalf("JobFinished: %v", err)
			}
			c.WaitLogCapture()
			if got := exec.deleteCount(); got != 1 {
				t.Fatalf("Delete calls = %d, want 1 (normal completion must reclaim the Job)", got)
			}
			exec.mu.Lock()
			defer exec.mu.Unlock()
			if exec.deleted[0].JobName != ref.JobName {
				t.Errorf("deleted job = %q, want %q", exec.deleted[0].JobName, ref.JobName)
			}
		})
	}
}

// Delete 失败只 WARN，不改状态。
func TestDeleteFailureDoesNotAffectStatus(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	exec := &fakeExec{deleteErr: errors.New("apiserver down")}
	var warned bool
	c, err := New(Options{
		Repos: repos, Executor: exec, Config: Config{StorageRoot: t.TempDir()},
		Logf: func(format string, args ...any) {
			if strings.Contains(format, "job_delete_failed") {
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
		t.Fatalf("task status = %s, delete failure must not change it", task.Status)
	}
	if !warned {
		t.Errorf("expected a job_delete_failed warning")
	}
}

// GC 开关：JobGCEnabled=false 时一次都不扫。
// GC 开关：JobGCEnabled=false 时用**真实 Executor**，断言一次 List 都没发生（票 #20-7）。
func TestGCOnceDisabledByConfig(t *testing.T) {
	off := false
	cs := fake.NewSimpleClientset()
	listCalls := 0
	cs.PrependReactor("list", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return false, nil, nil
	})
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listCalls++
		return false, nil, nil
	})
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{})
	c, err := New(Options{
		Repos: NewMemoryRepos(), Executor: ex,
		Config: Config{JobGCEnabled: &off},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.jobGC {
		t.Fatalf("jobGC must be off")
	}
	if err := c.GCOnce(context.Background()); err != nil {
		t.Fatalf("GCOnce: %v", err)
	}
	if listCalls != 0 {
		t.Fatalf("List calls = %d, want 0（开关关掉时不许扫 K8s）", listCalls)
	}
}

// GC 默认开启，默认周期 10min，默认超时 2min（execution §6.3、票 #20-4）。
func TestGCDefaults(t *testing.T) {
	// 显式给 config，不依赖环境变量（票 #20-7）。
	c, err := New(Options{Repos: NewMemoryRepos(), Executor: &fakeExec{}, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if !c.jobGC {
		t.Errorf("job GC must be enabled by default")
	}
	if got := c.Config().GCInterval; got != DefaultGCInterval {
		t.Errorf("GCInterval = %v, want %v", got, DefaultGCInterval)
	}
	if DefaultGCInterval != 10*time.Minute {
		t.Errorf("DefaultGCInterval = %v, want 10m（execution §6.3）", DefaultGCInterval)
	}
	if GCOnceTimeout != 2*time.Minute {
		t.Errorf("GCOnceTimeout = %v, want 2m", GCOnceTimeout)
	}
}

// hold-until 未过期时，终态 Attempt 的 Job 不能被删（票 #20-1：日志 follower 未收尾）。
func TestFinalizeSkipsHeldJob(t *testing.T) {
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	exec := &fakeExec{holdActive: true}
	c, err := New(Options{Repos: repos, Executor: exec, Config: Config{StorageRoot: t.TempDir()}})
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
	if got := exec.deleteCount(); got != 0 {
		t.Fatalf("Delete calls = %d, want 0（hold-until 未过期时不许删）", got)
	}
}

// GCOnce 用**批量**查询拿 Attempt 状态（票 #20-5：不是 N+1）。
func TestGCOnceUsesBatchAttemptStates(t *testing.T) {
	repos := &batchSpyRepos{MemoryRepos: NewMemoryRepos()}
	cs := fake.NewSimpleClientset()
	ex := executor.NewWithNamespace(cs, "testplane", executor.Options{})
	c, err := New(Options{Repos: repos, Executor: ex, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.GCOnce(context.Background()); err != nil {
		t.Fatalf("GCOnce: %v", err)
	}
	if repos.calls > 1 {
		t.Fatalf("AttemptStatesByJobNames calls = %d, want <=1（一次批量）", repos.calls)
	}
}

// batchSpyRepos 统计批量查询次数。
type batchSpyRepos struct {
	*MemoryRepos
	calls int
}

func (b *batchSpyRepos) AttemptStatesByJobNames(ctx context.Context, jobNames []string) (map[string]executor.AttemptState, error) {
	b.calls++
	return b.MemoryRepos.AttemptStatesByJobNames(ctx, jobNames)
}

// precheck: off 时门控不生效：所有 Task 直接放行。
func TestQueueAfterTestingWithoutPrecheckQueuesAll(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunTesting})
	repos.SeedTask(TaskView{ID: "t-0", RunID: "r-1", Seq: 0, Name: "a", TesterRole: domain.RoleTest,
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	repos.SeedTask(TaskView{ID: "t-1", RunID: "r-1", Seq: 1, Name: "b", TesterRole: domain.RoleTest,
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	c := newTestController(t, repos, &fakeExec{}, Config{}, nil, nil)

	if err := c.queueAfterTesting(context.Background(), "r-1"); err != nil {
		t.Fatalf("queueAfterTesting: %v", err)
	}
	for _, id := range []string{"t-0", "t-1"} {
		task, _ := repos.GetTask(context.Background(), id)
		if task.Status != domain.TaskQueued {
			t.Errorf("%s status = %s, want queued (precheck off ⇒ no gate)", id, task.Status)
		}
	}
}

// 显式在 tests[] 里写了 role: pre_check 但没有注入 precheck → 仍然不是门控。
func TestQueueAfterTestingIgnoresTesterRole(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunTesting})
	repos.SeedTask(TaskView{ID: "t-0", RunID: "r-1", Seq: 0, Name: "explicit",
		TesterRole: domain.RolePreCheck, IsPrecheck: false, // 没注入
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	repos.SeedTask(TaskView{ID: "t-1", RunID: "r-1", Seq: 1, Name: "b", TesterRole: domain.RoleTest,
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	c := newTestController(t, repos, &fakeExec{}, Config{}, nil, nil)
	if err := c.queueAfterTesting(context.Background(), "r-1"); err != nil {
		t.Fatal(err)
	}
	task, _ := repos.GetTask(context.Background(), "t-1")
	if task.Status != domain.TaskQueued {
		t.Fatalf("t-1 = %s, want queued (no injected precheck ⇒ no gate)", task.Status)
	}
}

// injected=true 且有 seq=0：只放 seq=0，其余保持 pending（等门控）。
func TestQueueAfterTestingWithInjectedPrecheckQueuesOnlySeqZero(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunTesting})
	repos.SeedTask(TaskView{ID: "t-0", RunID: "r-1", Seq: 0, Name: "precheck",
		TesterRole: domain.RolePreCheck, IsPrecheck: true,
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	repos.SeedTask(TaskView{ID: "t-1", RunID: "r-1", Seq: 1, Name: "load", TesterRole: domain.RoleTest,
		Status: domain.TaskPending, EndpointKey: "http://ep:8000"})
	c := newTestController(t, repos, &fakeExec{}, Config{}, nil, nil)
	if err := c.queueAfterTesting(context.Background(), "r-1"); err != nil {
		t.Fatal(err)
	}
	t0, _ := repos.GetTask(context.Background(), "t-0")
	if t0.Status != domain.TaskQueued {
		t.Errorf("precheck = %s, want queued", t0.Status)
	}
	t1, _ := repos.GetTask(context.Background(), "t-1")
	if t1.Status != domain.TaskPending {
		t.Errorf("t-1 = %s, want pending (gated by injected precheck)", t1.Status)
	}
}

// HasPrecheck 只看 IsPrecheck。
func TestHasPrecheckUsesIsPrecheck(t *testing.T) {
	if HasPrecheck([]TaskView{{TesterRole: domain.RolePreCheck}}) {
		t.Errorf("tester.yaml role must not be the criterion")
	}
	if !HasPrecheck([]TaskView{{IsPrecheck: true}}) {
		t.Errorf("IsPrecheck=true must be detected")
	}
}

// fakeDeployer 记录 Genet 取消调用。
type fakeDeployer struct {
	mu        sync.Mutex
	starts    int
	polls     int
	cancels   int
	cancelErr error
	startErr  error
	recordReq bool
	lastReq   ports.DeployRequest
	// pollState 是 Poll 的返回值（Phase 为空时默认 deploying）。
	pollState ports.DeployState
	// verifyGone / verifyErr 控制 VerifyUninstalled；verifies 计数。
	verifyGone *bool
	verifyErr  error
	verifies   int
}

func (f *fakeDeployer) Start(ctx context.Context, req ports.DeployRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.recordReq {
		f.lastReq = req
	}
	return f.startErr
}

func (f *fakeDeployer) Poll(ctx context.Context, runID string) (ports.DeployState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.pollState.Phase != "" {
		return f.pollState, nil
	}
	return ports.DeployState{Phase: "deploying"}, nil
}

func (f *fakeDeployer) Cancel(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.cancelErr
}

// 取消 deploying 的 Run 时，必须先请求 Genet 卸载（C-6.1 第 2 条）。
func TestCancelRunRequestsGenetUninstall(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{ID: "r-1", Status: domain.RunDeploying, HasDeploy: true, SubmittedAt: time.Now()})
	dep := &fakeDeployer{cancelErr: errors.New("genet: cancel not applied yet")}
	c, err := New(Options{Repos: repos, Executor: &fakeExec{}, Deployer: dep, Config: Config{}})
	if err != nil {
		t.Fatal(err)
	}
	// 卸载失败（release 未出现）不应让 CancelRun 失败：意图已落库，交给对账补偿。
	if err := c.CancelRun(context.Background(), "r-1", "user:1"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	dep.mu.Lock()
	defer dep.mu.Unlock()
	if dep.cancels != 1 {
		t.Fatalf("deployer.Cancel calls = %d, want 1", dep.cancels)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunCanceled {
		t.Errorf("run status = %s, want canceled", run.Status)
	}
}

// beginRun 必须把冻结的 runs.deploy 作为 Manifest 交给 Deployer.Start（§9.2）。
func TestBeginRunPassesFrozenDeployManifest(t *testing.T) {
	repos := NewMemoryRepos()
	frozen := json.RawMessage(`{"provider":"genet","parameters":{"model_name":"mock-llm"},"deployment_id":"d-r-1"}`)
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunQueued, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(), DeployJSON: frozen,
	})
	dep := &fakeDeployer{}
	dep.recordReq = true
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)

	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	dep.mu.Lock()
	req := dep.lastReq
	dep.mu.Unlock()
	if len(req.Manifest) == 0 {
		t.Fatal("Deployer.Start 收到空 Manifest：编排层必须在 BeginDeploy 后传入冻结的 runs.deploy（§9.2）")
	}
	if !strings.Contains(string(req.Manifest), "mock-llm") {
		t.Errorf("Manifest = %s, want frozen parameters", req.Manifest)
	}
	if req.DeployID != "d-r-1" {
		t.Errorf("DeployID = %q", req.DeployID)
	}
}

// Start 失败 = 平台侧调不通 Genet → DEPLOY_FAILED（不可重试）→ Run failed（D-47）。
func TestDeployStartFailureUsesDeployFailedCode(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunQueued, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(), DeployJSON: json.RawMessage(`{"package_type":"stack"}`),
	})
	dep := &fakeDeployer{startErr: errors.New("genet: 500 boom")}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)

	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	if run.Failure == nil || run.Failure.Code != domain.CodeDeployFailed {
		t.Fatalf("failure = %+v, want DEPLOY_FAILED", run.Failure)
	}
	if run.Failure.Retryable {
		t.Errorf("DEPLOY_FAILED must not be retryable")
	}
}

// 超窗 = 部署侧问题 → DEPLOY_BLOCKED（可重试）→ Run service_unavailable。
func TestDeployTimeoutUsesDeployBlockedCode(t *testing.T) {
	repos := NewMemoryRepos()
	// SubmittedAt 远早于窗口 → pollDeploy 第一行就判超窗。
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunDeploying, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now().Add(-100 * time.Hour), DeployJSON: json.RawMessage(`{"package_type":"stack"}`),
	})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{DeployTimeout: 72 * time.Hour}, dep, nil)

	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunServiceUnavailable {
		t.Fatalf("run status = %s, want service_unavailable", run.Status)
	}
	if run.Failure == nil || run.Failure.Code != domain.CodeDeployBlocked {
		t.Fatalf("failure = %+v, want DEPLOY_BLOCKED", run.Failure)
	}
	if !run.Failure.Retryable {
		t.Errorf("DEPLOY_BLOCKED must be retryable")
	}
}

// ---- ports.DeployCleaner 支撑 -------------------------------------------------

// VerifyUninstalled 假实现：默认 true（已删干净），可注入 false / error。
func (f *fakeDeployer) VerifyUninstalled(ctx context.Context, runID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	if f.verifyGone != nil {
		return *f.verifyGone, nil
	}
	return true, nil
}

// 收口：Run 已终态且 HasDeploy 时，对账必须发 DELETE 并把 uninstall 记成 verified（§9.7）。
func TestTerminalDeployRunIsUninstalledOnReconcile(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now().Add(-time.Minute),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-abc123456789","state":"ready"}`),
	})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	dep.mu.Lock()
	cancels, verifies := dep.cancels, dep.verifies
	dep.mu.Unlock()
	if cancels != 1 {
		t.Fatalf("deployer.Cancel calls = %d, want 1（终态必须卸载）", cancels)
	}
	if verifies != 1 {
		t.Errorf("VerifyUninstalled calls = %d, want 1", verifies)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunSucceeded {
		t.Errorf("run status = %s, want succeeded（卸载不改终态）", run.Status)
	}
	doc := deployDoc(t, run)
	u, _ := doc["uninstall"].(map[string]any)
	if u == nil || u["state"] != "verified" {
		t.Fatalf("uninstall = %v, want state=verified", doc["uninstall"])
	}
	if _, ok := u["verified_at"]; !ok {
		t.Errorf("uninstall.verified_at missing: %v", u)
	}
	if v, ok := u["force"].(bool); !ok || !v {
		t.Errorf("uninstall.force = %v, want true", u["force"])
	}
	// 冻结字段不能被卸载覆盖掉。
	if doc["release_name"] != "tp-abc123456789" {
		t.Errorf("release_name = %v, want preserved", doc["release_name"])
	}
}

// DELETE 502：Run 仍 succeeded，uninstall.state=failed；下一轮对账重试成功后 verified。
func TestCleanupFailureKeepsRunSucceededThenRetries(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now().Add(-time.Minute),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-x","state":"ready"}`),
	})
	gone := false // DELETE 502 且 release 仍在 → 不能算删干净
	dep := &fakeDeployer{cancelErr: errors.New("genet: server error: DELETE 502"), verifyGone: &gone}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if run.Status != domain.RunSucceeded {
		t.Fatalf("run status = %s, want succeeded（卸载失败不改终态）", run.Status)
	}
	doc := deployDoc(t, run)
	u, _ := doc["uninstall"].(map[string]any)
	if u == nil || u["state"] != "failed" {
		t.Fatalf("uninstall = %v, want state=failed", doc["uninstall"])
	}
	if errs, _ := u["cleanup_errors"].([]any); len(errs) == 0 {
		t.Errorf("cleanup_errors must carry the failure: %v", u)
	}

	// 下一轮：Genet 恢复 → verified。
	dep.mu.Lock()
	dep.cancelErr = nil
	okGone := true
	dep.verifyGone = &okGone
	dep.mu.Unlock()
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce(2): %v", err)
	}
	run, _ = repos.GetRun(context.Background(), "r-1")
	doc = deployDoc(t, run)
	u, _ = doc["uninstall"].(map[string]any)
	if u == nil || u["state"] != "verified" {
		t.Fatalf("uninstall = %v, want verified after retry", doc["uninstall"])
	}
}

// DELETE 成功但 "删干净" 校验未过（release 还能 GET 到）→ uninstall.state=failed，且会重试。
func TestCleanupVerifyStillPresentMarksFailed(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now().Add(-time.Minute),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-y","state":"ready"}`),
	})
	gone := false
	dep := &fakeDeployer{verifyGone: &gone}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)

	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	doc := deployDoc(t, run)
	u, _ := doc["uninstall"].(map[string]any)
	if u == nil || u["state"] != "failed" {
		t.Fatalf("uninstall = %v, want failed（GET 200 不算删干净）", doc["uninstall"])
	}
	// 又在待收口列表里 → 下一轮会再试。
	pending, err := repos.ListDeployCleanupPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("pending = %d, want 1（未 verified 的必须进补偿列表）", len(pending))
	}
}

// cleanup_policy=never 的 Run 不卸载。
func TestCleanupNeverPolicySkipsUninstall(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "never",
		SubmittedAt: time.Now().Add(-time.Minute),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-z","state":"ready"}`),
	})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	if err := c.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep.mu.Lock()
	defer dep.mu.Unlock()
	if dep.cancels != 0 {
		t.Errorf("deployer.Cancel calls = %d, want 0（cleanup.policy=never）", dep.cancels)
	}
}

// ---- §4.4 落库 ---------------------------------------------------------------

// deployDoc 取 Run 的 runs.deploy 快照（内存实现里是 RunView.DeployJSON）。
func deployDoc(t *testing.T, run RunView) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if len(run.DeployJSON) == 0 {
		return doc
	}
	if err := json.Unmarshal(run.DeployJSON, &doc); err != nil {
		t.Fatalf("decode deploy: %v", err)
	}
	return doc
}

// beginRun 必须落冻结字段：release_name / provider / package_type / accepted_at（§4.4）。
func TestBeginRunRecordsFrozenDeploySnapshot(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunQueued, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(),
		DeployJSON:  json.RawMessage(`{"provider":"genet","package_type":"stack","parameters":{"model_name":"mock-llm"}}`),
	})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	ctx := context.Background()
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	doc := deployDoc(t, run)
	// release_name = tp-<sha12(run:deploy)>
	want := DeployReleaseName("r-1", "d-r-1")
	if doc["release_name"] != want {
		t.Errorf("release_name = %v, want %s", doc["release_name"], want)
	}
	if doc["provider"] != "genet" {
		t.Errorf("provider = %v", doc["provider"])
	}
	if doc["package_type"] != "stack" {
		t.Errorf("package_type = %v", doc["package_type"])
	}
	for _, key := range []string{"accepted_at", "queued_at", "last_polled_at"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("%s missing: %v", key, doc)
		}
	}
	if v, ok := doc["queue_position"]; !ok || v != nil {
		t.Errorf("queue_position = %v (present=%v), want null", v, ok)
	}
}

// Poll 必须回写两根轴原值 + 队列水位 + 时间线（§4.4）。
func TestPollRecordsBothAxesAndQueue(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunDeploying, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-q","provider":"genet","package_type":"stack"}`),
	})
	dep := &fakeDeployer{pollState: ports.DeployState{
		Phase:          "queued",
		Reason:         "PodGroup Inqueue: insufficient Ascend910",
		ReleaseStatus:  "pending-install",
		EndpointStatus: "pending",
		Queue:          &domain.DeployQueue{Phase: "Inqueue", Pending: 3, Inqueue: 2, Running: 7},
	}}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	ctx := context.Background()
	if err := c.DeployOnce(ctx); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(ctx, "r-1")
	doc := deployDoc(t, run)
	if doc["genet_release_status"] != "pending-install" {
		t.Errorf("genet_release_status = %v", doc["genet_release_status"])
	}
	if doc["endpoint_status"] != "pending" {
		t.Errorf("endpoint_status = %v", doc["endpoint_status"])
	}
	q, _ := doc["queue"].(map[string]any)
	if q == nil || q["phase"] != "Inqueue" {
		t.Errorf("queue = %v", doc["queue"])
	}
	if doc["state"] != "queued" {
		t.Errorf("state = %v", doc["state"])
	}
	if _, ok := doc["last_polled_at"]; !ok {
		t.Errorf("last_polled_at missing")
	}
	// 冻结字段在轮询后仍然存在（合并而不是覆盖）。
	if doc["release_name"] != "tp-q" {
		t.Errorf("release_name lost after poll: %v", doc["release_name"])
	}
}

// 编排层的 release 名必须与 genet 客户端逐字一致（两处实现，交叉校验）。
func TestDeployReleaseNameMatchesGenet(t *testing.T) {
	for _, tc := range [][2]string{{"r-20261105-0001", "d-r-20261105-0001"}, {"r-1", "d-r-1"}} {
		got := DeployReleaseName(tc[0], tc[1])
		want := genet.ReleaseName(tc[0], tc[1])
		if got != want {
			t.Errorf("DeployReleaseName(%s,%s) = %s, genet = %s", tc[0], tc[1], got, want)
		}
	}
}

// HoldActive 让 fakeExec 实现 ports.JobHolder（hold-until 守卫用例）。
func (f *fakeExec) HoldActive(ctx context.Context, h ports.JobHandle) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holdActive, nil
}

// ---- #28：卸载收口必须尊重 cleanup_policy --------------------------------

// never + Run 成功 → 不调用 Deployer.Cancel，uninstall.state=skipped。
func TestCleanupPolicyNeverSkipsUninstall(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "never",
		SubmittedAt: time.Now(), DeployJSON: json.RawMessage(`{"release_name":"tp-never","state":"ready"}`),
	})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	c.cleanupDeploy(context.Background(), "r-1")

	dep.mu.Lock()
	cancels := dep.cancels
	dep.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("deployer.Cancel calls = %d, want 0（cleanup_policy=never）", cancels)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	doc := deployDoc(t, run)
	u, _ := doc["uninstall"].(map[string]any)
	if u == nil || u["state"] != domain.UninstallSkipped {
		t.Fatalf("uninstall = %v, want state=skipped", doc["uninstall"])
	}
}

// on_failure + 全部 Task 成功 → 不卸载（skipped）。
func TestCleanupOnFailureAllSucceededSkips(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunSucceeded, HasDeploy: true, CleanupPolicy: "on_failure",
		SubmittedAt: time.Now(), DeployJSON: json.RawMessage(`{"release_name":"tp-ok","state":"ready"}`),
	})
	repos.SeedTask(TaskView{ID: "t-1", RunID: "r-1", Seq: 0, Name: "a", Status: domain.TaskSucceeded})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	c.cleanupDeploy(context.Background(), "r-1")

	dep.mu.Lock()
	cancels := dep.cancels
	dep.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("deployer.Cancel calls = %d, want 0（on_failure 且全成功）", cancels)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	u, _ := deployDoc(t, run)["uninstall"].(map[string]any)
	if u == nil || u["state"] != domain.UninstallSkipped {
		t.Fatalf("uninstall = %v, want skipped", deployDoc(t, run)["uninstall"])
	}
}

// on_failure + 有失败的 Task → 必须卸载。
func TestCleanupOnFailureWithFailedTaskUninstalls(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunCompletedWithErrors, HasDeploy: true, CleanupPolicy: "on_failure",
		SubmittedAt: time.Now(), DeployJSON: json.RawMessage(`{"release_name":"tp-bad","state":"ready"}`),
	})
	repos.SeedTask(TaskView{ID: "t-1", RunID: "r-1", Seq: 0, Name: "a", Status: domain.TaskSucceeded})
	repos.SeedTask(TaskView{ID: "t-2", RunID: "r-1", Seq: 1, Name: "b", Status: domain.TaskFailed})
	dep := &fakeDeployer{}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	c.cleanupDeploy(context.Background(), "r-1")

	dep.mu.Lock()
	cancels := dep.cancels
	dep.mu.Unlock()
	if cancels != 1 {
		t.Fatalf("deployer.Cancel calls = %d, want 1（有失败 Task → 必须卸载）", cancels)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	u, _ := deployDoc(t, run)["uninstall"].(map[string]any)
	if u == nil || u["state"] != domain.UninstallVerified {
		t.Fatalf("uninstall = %v, want verified", deployDoc(t, run)["uninstall"])
	}
}

// canceled 终态也必须进收口队列（对账列表覆盖 canceled）。
func TestCleanupPendingIncludesCanceled(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunCanceled, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(), DeployJSON: json.RawMessage(`{"release_name":"tp-c","state":"ready"}`),
	})
	pending, err := repos.ListDeployCleanupPending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1（canceled 也要收口）", len(pending))
	}
}

// 票 #31 端到端：Attempt 的 finished_at 落库值必须跟随注入时钟（拨到过去 → 落库也是过去）。
//
// 这条**只验时钟**；异步日志落盘是 `finalizeAttemptAsync` 的副作用（Windows 上会与
// t.TempDir 清理抢），所以输出根目录用独立临时目录 + 带重试的清理，并在断言前等收尾。
func TestAttemptFinishedAtFollowsInjectedClock(t *testing.T) {
	fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	repos := NewMemoryRepos()
	ref := seedReconcileFixture(repos, "r-1", "t-1", "http://ep:8000")
	clock := domain.NewFixedClock(fixed)
	ex := executor.NewWithNamespace(fake.NewSimpleClientset(), "testplane", executor.Options{Clock: clock})
	storage := tempDirNoRace(t)
	c, err := New(Options{Repos: repos, Executor: ex, Config: Config{StorageRoot: storage}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.JobFinished(context.Background(), ports.JobOutcome{
		Handle:     ports.JobHandle{Namespace: "testplane", JobName: ref.JobName},
		Phase:      executor.PhaseSucceeded,
		FinishedAt: clock.Now(),
	}); err != nil {
		t.Fatalf("JobFinished: %v", err)
	}
	c.WaitLogCapture() // 等异步日志落盘收尾，避免与临时目录清理抢
	repos.mu.Lock()
	defer repos.mu.Unlock()
	for id, a := range repos.attempts {
		if !a.FinishedAt.Equal(fixed) {
			t.Fatalf("attempt %s finished_at = %v, want %v（注入时钟）", id, a.FinishedAt, fixed)
		}
	}
}

// tempDirNoRace 造一个"允许异步写入后再删"的临时目录（Windows 上 RemoveAll 会被
// 异步日志落盘抢成 "The directory is not empty"，所以清理带重试）。
func tempDirNoRace(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "testplane-attempts-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("WARN: temp dir not removed: %s", dir)
	})
	return dir
}

// Poll 必须把 Genet 返回的 **namespace** 落进 runs.deploy（QA deploy-logs-check 的缺口：
// 结构体有字段但没人写 → 日志端点回退到 TESTPLANE_NAMESPACE，Pod 在 Genet 的 ns 里读不到）。
func TestPollPersistsGenetNamespace(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunDeploying, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-ns","provider":"genet","package_type":"stack"}`),
	})
	dep := &fakeDeployer{pollState: ports.DeployState{
		Phase:       "deploying",
		Namespace:   "testplane-mock",
		ReleaseName: "tp-ns",
	}}
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	doc := deployDoc(t, run)
	if doc["namespace"] != "testplane-mock" {
		t.Fatalf("runs.deploy.namespace = %v, want testplane-mock（Genet 的 ns 必须落库）", doc["namespace"])
	}
	// 合并语义：同批字段不能被冲掉。
	if doc["release_name"] != "tp-ns" {
		t.Fatalf("release_name 被冲掉了：%v", doc["release_name"])
	}
	if doc["package_type"] != "stack" {
		t.Fatalf("package_type 被冲掉了：%v", doc["package_type"])
	}
}

// namespace 为空时不写（omitempty + jsonb 合并），避免把已经存在的键冲成空。
func TestPollDoesNotBlankNamespace(t *testing.T) {
	repos := NewMemoryRepos()
	repos.SeedRun(RunView{
		ID: "r-1", Status: domain.RunDeploying, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: time.Now(),
		DeployJSON:  json.RawMessage(`{"release_name":"tp-ns","namespace":"testplane-mock","state":"deploying"}`),
	})
	dep := &fakeDeployer{pollState: ports.DeployState{Phase: "deploying"}} // 没带 namespace
	c := newTestController(t, repos, &fakeExec{}, Config{}, dep, nil)
	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce: %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-1")
	if got := deployDoc(t, run)["namespace"]; got != "testplane-mock" {
		t.Fatalf("runs.deploy.namespace = %v, want testplane-mock（空值不许覆盖已有键）", got)
	}
}
