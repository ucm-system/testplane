package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// seedRunningAttempt 造一个"Task 正在 running、Attempt 已 running"的最小现场，
// 返回 run/task/attempt id（复用于 refs 的用例）。
func seedRunningAttempt(t *testing.T, st *Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	machines := NewMachines(st, nil)

	ep := domain.Endpoint{Key: "http://refs:8000", BaseURL: "http://refs:8000/v1", ModelName: "m"}
	epJSON, _ := json.Marshal(ep)
	runID := NewRunID(time.Now())
	taskID := NewTaskID()
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "refs-demo", Kind: "run",
			Manifest: []byte(`{"run":{"name":"refs-demo"}}`), Resolved: []byte(`{}`),
			CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: epJSON,
		},
		Tasks: []*Task{{
			ID: taskID, Seq: 1, Name: "only",
			TesterName: "probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 600, Status: string(domain.TaskPending), EndpointKey: &ep.Key,
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
	res, claimed, reason, err := st.ClaimForExecution(ctx, ClaimRequest{TaskID: taskID, EndpointKey: ep.Key})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v reason=%s err=%v", claimed, reason, err)
	}
	if err := st.MarkAttemptRunning(ctx, res.AttemptID, "testplane", res.JobName); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	return runID, taskID, res.AttemptID
}

// refsOf 读 attempts / tasks 的 ref 列（NULL → 空串）。
func refsOf(t *testing.T, st *Store, taskID, attemptID string) (logRef, artifactRef, attemptResultRef, taskResultRef string) {
	t.Helper()
	ctx := context.Background()
	var lr, ar, rr *string
	if err := st.pool.QueryRow(ctx,
		`SELECT log_ref, artifact_ref, result_ref FROM attempts WHERE id = $1`, attemptID).
		Scan(&lr, &ar, &rr); err != nil {
		t.Fatalf("read attempt refs: %v", err)
	}
	var tr *string
	if err := st.pool.QueryRow(ctx, `SELECT result_ref FROM tasks WHERE id = $1`, taskID).Scan(&tr); err != nil {
		t.Fatalf("read task refs: %v", err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return deref(lr), deref(ar), deref(rr), deref(tr)
}

// ① Attempt 终态时把 refs 写进 attempts，Task 终态写 tasks.result_ref（run 相对路径）。
// 改回去（payload 不带 refs）这条必红：两列都是 NULL。
func TestFinishAttempt_PersistsRefs(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	_, taskID, attemptID := seedRunningAttempt(t, st)

	out, err := st.FinishAttempt(ctx, AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, Succeeded: true, Phase: "succeeded",
		FinishedAt:  time.Now().UTC(),
		LogRef:      "logs/stdout.log",
		ArtifactRef: "artifacts",
		ResultRef:   "result.json",
	})
	if err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	if !out.TaskTerminal {
		t.Fatal("task 应进入终态")
	}

	logRef, artifactRef, attemptResultRef, taskResultRef := refsOf(t, st, taskID, attemptID)
	if logRef != "logs/stdout.log" {
		t.Errorf("attempts.log_ref = %q, want logs/stdout.log", logRef)
	}
	if artifactRef != "artifacts" {
		t.Errorf("attempts.artifact_ref = %q, want artifacts", artifactRef)
	}
	if attemptResultRef != "result.json" {
		t.Errorf("attempts.result_ref = %q, want result.json（相对 Attempt 目录）", attemptResultRef)
	}
	wantTaskRef := "tasks/" + taskID + "/attempts/1/result.json"
	if taskResultRef != wantTaskRef {
		t.Errorf("tasks.result_ref = %q, want %q（相对 Run 目录）", taskResultRef, wantTaskRef)
	}
}

// ② 没有 result.json 时不写 ResultRef（NULL），绝不造死链；log/artifact 仍可单独写。
func TestFinishAttempt_NoResultRefWhenMissing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	_, taskID, attemptID := seedRunningAttempt(t, st)

	if _, err := st.FinishAttempt(ctx, AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, Succeeded: false, Phase: "failed",
		Failure:    &domain.Failure{Code: domain.CodeResultMissing, Message: "no result.json", Retryable: false},
		FinishedAt: time.Now().UTC(),
		LogRef:     "logs/stdout.log", // 有日志
	}); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}

	logRef, _, attemptResultRef, taskResultRef := refsOf(t, st, taskID, attemptID)
	if logRef != "logs/stdout.log" {
		t.Errorf("attempts.log_ref = %q", logRef)
	}
	if attemptResultRef != "" {
		t.Errorf("attempts.result_ref = %q, want NULL（没有 result.json 就不许写引用）", attemptResultRef)
	}
	if taskResultRef != "" {
		t.Errorf("tasks.result_ref = %q, want NULL", taskResultRef)
	}
}

// ③ SetAttemptRefs 只补空（COALESCE）：日志晚到不要冲掉已有引用。
func TestSetAttemptRefs_FillsLogRefOnly(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	_, taskID, attemptID := seedRunningAttempt(t, st)
	if _, err := st.FinishAttempt(ctx, AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, Succeeded: true, Phase: "succeeded",
		FinishedAt: time.Now().UTC(),
		ResultRef:  "result.json",
	}); err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	if err := st.SetAttemptRefs(ctx, attemptID, "logs/stdout.log", "", ""); err != nil {
		t.Fatalf("SetAttemptRefs: %v", err)
	}
	logRef, _, attemptResultRef, _ := refsOf(t, st, taskID, attemptID)
	if logRef != "logs/stdout.log" {
		t.Errorf("log_ref = %q, want 补上", logRef)
	}
	if attemptResultRef != "result.json" {
		t.Errorf("result_ref = %q，被空值冲掉了", attemptResultRef)
	}
}
