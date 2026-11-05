package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestRetryBackoffUsesInjectedClock 验证退避基准是注入时钟：
// 注入固定时钟 T、backoff_seconds=30 时，failed→queued 的 available_at 必须等于 T+30s，
// 而不是数据库的 now()（05 §6.5：写库时间戳要有单一来源，且单测可拨）。
func TestRetryBackoffUsesInjectedClock(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	fixed := domain.NewFixedClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	st.SetClock(fixed)
	t.Cleanup(func() { st.SetClock(domain.SystemClock) })

	machines := NewMachines(st, nil)
	ep := domain.Endpoint{Key: "http://backoff-" + NewTaskID() + ":8000",
		BaseURL: "http://127.0.0.1:8000/v1", ModelName: "m"}
	epJSON, _ := json.Marshal(ep)
	runID := NewRunID(fixed.Now())
	taskID := NewTaskID()
	retryPolicy, _ := json.Marshal(map[string]any{"max_attempts": 2, "backoff_seconds": 30})
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "backoff-demo", Kind: "run",
			Manifest: []byte(`{}`), Resolved: []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: epJSON,
		},
		Tasks: []*Task{{
			ID: taskID, Seq: 0, Name: "t1", TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 60,
			Status: string(domain.TaskPending), EndpointKey: &ep.Key, RetryPolicy: retryPolicy,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// 跑到 failed：queued → preparing（claim）→ running → failed（Attempt 结束）。
	if _, err := machines.Runs.Enqueue(ctx, runID, "system:orchestrator"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := machines.Runs.BeginTesting(ctx, runID, ep); err != nil {
		t.Fatalf("begin testing: %v", err)
	}
	if _, err := machines.Tasks.Queue(ctx, taskID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	res, claimed, reason, err := st.ClaimForExecution(ctx, ClaimRequest{TaskID: taskID, EndpointKey: ep.Key})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v reason=%s err=%v", claimed, reason, err)
	}
	if err := st.MarkAttemptRunning(ctx, res.AttemptID, "testplane", res.JobName); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := st.FinishAttempt(ctx, AttemptFinish{
		AttemptID: res.AttemptID, TaskID: taskID, RunID: runID,
		Succeeded: false, Phase: "failed",
		Failure:    &domain.Failure{Code: domain.CodeTesterNonzeroExit, Message: "exit 1"},
		FinishedAt: fixed.Now(),
	}); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}

	// failed → queued（T11），available_at 应为 T + 30s。
	if _, err := machines.Tasks.Retry(ctx, taskID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	task, err := st.Tasks.Get(ctx, nil, taskID)
	if err != nil {
		t.Fatalf("读 task: %v", err)
	}
	if task.Status != string(domain.TaskQueued) {
		t.Fatalf("重试后状态 = %s, 期望 queued", task.Status)
	}
	if task.AvailableAt == nil {
		t.Fatal("重试后 available_at 必须非空（退避到期时间）")
	}
	want := fixed.Now().Add(30 * time.Second)
	if diff := task.AvailableAt.Sub(want); diff < -time.Second || diff > time.Second {
		t.Fatalf("available_at = %v, 期望 %v（差 %v，说明没走注入时钟）", task.AvailableAt, want, diff)
	}
}
