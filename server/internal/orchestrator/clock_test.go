package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestDeployWindowUsesInjectedClock 用可拨时钟验证"部署总窗口"判定：
// 窗口内不判超窗，把时钟拨过 72h 后才落 service_unavailable（DEPLOY_BLOCKED）。
func TestDeployWindowUsesInjectedClock(t *testing.T) {
	repos := NewMemoryRepos()
	clk := domain.NewFixedClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	const window = 72 * time.Hour
	// 提交于 71h 前：窗口内。
	repos.SeedRun(RunView{
		ID: "r-clk", Status: domain.RunDeploying, HasDeploy: true, CleanupPolicy: "always",
		SubmittedAt: clk.Now().Add(-71 * time.Hour),
		DeployJSON:  []byte(`{"package_type":"stack"}`),
	})
	dep := &fakeDeployer{}
	c, err := New(Options{
		Repos: repos, Executor: &fakeExec{}, Deployer: dep,
		Config: Config{DeployTimeout: window}, Clock: clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 71h：不超窗（继续部署/等待，不落终态）。
	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce(71h): %v", err)
	}
	if run, _ := repos.GetRun(context.Background(), "r-clk"); run.Status == domain.RunServiceUnavailable {
		t.Fatalf("窗口内不应判超窗，却落了 service_unavailable")
	}

	// 拨过窗口（71h + 2h > 72h）→ 超窗。
	clk.Advance(2 * time.Hour)
	if err := c.DeployOnce(context.Background()); err != nil {
		t.Fatalf("DeployOnce(73h): %v", err)
	}
	run, _ := repos.GetRun(context.Background(), "r-clk")
	if run.Status != domain.RunServiceUnavailable {
		t.Fatalf("超窗后 Run 状态 = %s, 期望 service_unavailable", run.Status)
	}
	if run.Failure == nil || run.Failure.Code != domain.CodeDeployBlocked {
		t.Fatalf("失败码 = %+v, 期望 DEPLOY_BLOCKED", run.Failure)
	}
}

// clockRecordingRepos 记录 ListExpiredAttempts 收到的"现在"，用来证明 watchdog 走注入时钟。
type clockRecordingRepos struct {
	Repos
	lastNow time.Time
}

func (r *clockRecordingRepos) ListExpiredAttempts(ctx context.Context, now time.Time) ([]AttemptRef, error) {
	r.lastNow = now
	return r.Repos.ListExpiredAttempts(ctx, now)
}

// TestWatchdogUsesInjectedClock watchdog 的超时判定基准必须是注入时钟。
func TestWatchdogUsesInjectedClock(t *testing.T) {
	base := NewMemoryRepos()
	clk := domain.NewFixedClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	repos := &clockRecordingRepos{Repos: base}

	// 一个 1h 前开始、timeout=30min 的 Attempt（在假时钟下已超时）。
	base.SeedActiveTask(TaskView{
		ID: "t-clk", RunID: "r-clk", Name: "t", Status: domain.TaskRunning,
		TimeoutSeconds: 1800,
	}, AttemptRef{AttemptID: "t-clk-a1", TaskID: "t-clk", RunID: "r-clk", AttemptNo: 1},
		"", clk.Now().Add(-time.Hour))

	c, err := New(Options{
		Repos: repos, Executor: &fakeExec{}, Config: Config{}, Clock: clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.WatchdogOnce(context.Background()); err != nil {
		t.Fatalf("WatchdogOnce: %v", err)
	}
	if !repos.lastNow.Equal(clk.Now()) {
		t.Fatalf("watchdog 传给 Repos 的时间 = %v, 期望注入时钟 %v", repos.lastNow, clk.Now())
	}
	// 拨动时钟后再跑一次，基准应跟着变（证明不是缓存值）。
	clk.Advance(5 * time.Minute)
	if err := c.WatchdogOnce(context.Background()); err != nil {
		t.Fatalf("WatchdogOnce(2): %v", err)
	}
	if !repos.lastNow.Equal(clk.Now()) {
		t.Fatalf("第二次 watchdog 时间 = %v, 期望 %v", repos.lastNow, clk.Now())
	}
}
