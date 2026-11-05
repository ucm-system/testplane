package store

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestFinishAttempt_FiresRunFinishedHookOnce 是 Lead 报的 P0 回归：
// 「最后一个 Attempt 结束 → Run 终态」这条最常见路径必须触发报告 hook（恰好一次），
// 且 hook 里写的 report.written 事件要落库。
func TestFinishAttempt_FiresRunFinishedHookOnce(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	machines := NewMachines(st, nil)

	var (
		calls   int32
		hookRun string
		fired   = make(chan string, 4)
	)
	st.SetRunFinishedHook(func(hctx context.Context, runID string) {
		atomic.AddInt32(&calls, 1)
		hookRun = runID
		_, _ = st.EmitRunEvent(hctx, runID, domain.EventReportWritten,
			map[string]any{"path": "report.json"}, "system:report")
		fired <- runID
	})

	// service 模式的 Run：endpoint 在提交时就有，Task 的 endpoint_key 非空。
	ep := domain.Endpoint{Key: "http://hook:8000", BaseURL: "http://hook:8000/v1", ModelName: "m"}
	epJSON, _ := json.Marshal(ep)
	runID := NewRunID(time.Now())
	taskID := NewTaskID()
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "hook-demo", Kind: "run",
			Manifest: []byte(`{"run":{"name":"hook-demo"}}`), Resolved: []byte(`{}`),
			CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: epJSON,
		},
		Tasks: []*Task{{
			ID: taskID, Seq: 1, Name: "only",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 600, Status: string(domain.TaskPending),
			EndpointKey: &ep.Key,
		}},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if _, err := machines.Runs.Enqueue(ctx, runID, "system:orchestrator"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := machines.Runs.BeginTesting(ctx, runID, ep); err != nil {
		t.Fatalf("begin testing: %v", err)
	}
	if _, err := machines.Tasks.Queue(ctx, taskID); err != nil {
		t.Fatalf("queue: %v", err)
	}

	res, claimed, reason, err := st.ClaimForExecution(ctx, ClaimRequest{
		TaskID: taskID, EndpointKey: ep.Key,
	})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v reason=%s err=%v", claimed, reason, err)
	}
	if err := st.MarkAttemptRunning(ctx, res.AttemptID, "testplane", res.JobName); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// 模拟调度器曾写过 waiting_reason（Bug B 的现场）。
	if err := st.Tasks.SetWaitingReason(ctx, nil, taskID, domain.WaitSlotWaiting); err != nil {
		t.Fatalf("set waiting reason: %v", err)
	}

	out, err := st.FinishAttempt(ctx, AttemptFinish{
		AttemptID: res.AttemptID, TaskID: taskID, RunID: runID,
		Succeeded: true, Phase: "succeeded", FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	if out.RunStatus != domain.RunSucceeded {
		t.Fatalf("Run 终态 = %s, 期望 succeeded", out.RunStatus)
	}

	// hook 必须在提交后异步触发，恰好一次。
	select {
	case got := <-fired:
		if got != runID {
			t.Fatalf("hook runID = %s, 期望 %s", got, runID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 终态后 hook 没有被调用（报告不会生成）")
	}
	time.Sleep(100 * time.Millisecond) // 给潜在的重复触发留窗口
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("hook 调用次数 = %d, 期望恰好 1", n)
	}
	if hookRun != runID {
		t.Fatalf("hookRun = %s", hookRun)
	}

	// report.written 事件落库。
	var n int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM state_events WHERE run_id=$1 AND type=$2`,
		runID, domain.EventReportWritten).Scan(&n); err != nil {
		t.Fatalf("查事件: %v", err)
	}
	if n != 1 {
		t.Fatalf("report.written 事件数 = %d, 期望 1", n)
	}

	// Bug B：Task 终态必须清 waiting_reason。
	var waiting *string
	var status domain.TaskStatus
	if err := st.Pool().QueryRow(ctx,
		`SELECT status, waiting_reason FROM tasks WHERE id=$1`, taskID).Scan(&status, &waiting); err != nil {
		t.Fatalf("读 task: %v", err)
	}
	if status != domain.TaskSucceeded {
		t.Fatalf("task status = %s", status)
	}
	if waiting != nil {
		t.Fatalf("终态 Task 的 waiting_reason 应清空，实际 = %q", *waiting)
	}

	// Run 也应该有 error（succeeded 不需要）与终态时间。
	var runStatus domain.RunStatus
	var finishedAt *time.Time
	if err := st.Pool().QueryRow(ctx,
		`SELECT status, finished_at FROM runs WHERE id=$1`, runID).Scan(&runStatus, &finishedAt); err != nil {
		t.Fatalf("读 run: %v", err)
	}
	if runStatus != domain.RunSucceeded || finishedAt == nil {
		t.Fatalf("run = %s finished_at=%v", runStatus, finishedAt)
	}
}
