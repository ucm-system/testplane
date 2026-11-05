package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// seedTesterRole 在 testers 表登记一个 Tester 版本（Pre-Check 判别依赖这一行的 role）。
func seedTesterRole(t *testing.T, st *Store, name, version, role, onFailure string) {
	t.Helper()
	if err := st.Testers.Upsert(context.Background(), nil, &Tester{
		Name: name, Version: version, DisplayName: name, Description: "",
		Role: role, OnFailure: onFailure, Entry: "/tester/run.sh", Image: "img:1",
		Resources: []byte("{}"), Concurrency: []byte("{}"), Mounts: []byte("[]"),
		K8s: []byte("{}"), Parameters: []byte("{}"), EnvDecl: []byte("{}"),
		SourceFile: "testers/" + name + "/tester.yaml", ContentSHA: "sha-" + name + version,
		RegisteredBy: "test:seed",
	}); err != nil {
		t.Fatalf("seed tester %s: %v", name, err)
	}
}

// setupSingleTaskRun 建一个 service 模式的 Run + 单个 Task（seq=0，指定 tester），
// 并把流程推进到 Attempt running，返回 (runID, taskID, attemptID)。
func setupSingleTaskRun(t *testing.T, st *Store, tester, testerVersion, onFailure string, precheckInjected bool) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	machines := NewMachines(st, nil)

	ep := domain.Endpoint{Key: "http://outcome-" + NewTaskID() + ":8000",
		BaseURL: "http://127.0.0.1:8000/v1", ModelName: "m"}
	epJSON, _ := json.Marshal(ep)
	runID := NewRunID(time.Now())
	taskID := NewTaskID()
	resolved := []byte(`{"kind":"run","precheck":{"injected":false},"tasks":[]}`)
	if precheckInjected {
		resolved = []byte(`{"kind":"run","precheck":{"injected":true,"seq":0,"tester":"` + tester + `"},"tasks":[]}`)
	}
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "outcome-demo", Kind: "run",
			Manifest: []byte(`{"run":{"name":"outcome-demo"}}`), Resolved: resolved,
			CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: epJSON,
		},
		Tasks: []*Task{{
			ID: taskID, Seq: 0, Name: "only",
			TesterName: tester, TesterVersion: testerVersion, ImageRef: "img:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: onFailure,
			TimeoutSeconds: 5, Status: string(domain.TaskPending),
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
	res, claimed, reason, err := st.ClaimForExecution(ctx, ClaimRequest{TaskID: taskID, EndpointKey: ep.Key})
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v reason=%s err=%v", claimed, reason, err)
	}
	if err := st.MarkAttemptRunning(ctx, res.AttemptID, "testplane", res.JobName); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	return runID, taskID, res.AttemptID
}

func finishWithTimeout(t *testing.T, st *Store, runID, taskID, attemptID string) *AttemptFinishResult {
	t.Helper()
	out, err := st.FinishAttempt(context.Background(), AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, RunID: runID,
		Succeeded: false, Phase: "timeout",
		Failure: &domain.Failure{
			Code: domain.CodeTaskTimeout, Message: "Task 执行超时",
			Retryable: true, Detail: map[string]any{"timeout_seconds": 5},
		},
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	return out
}

// TestRunOutcome_PrecheckOffTimeoutIsFailed 是 QA 的 r-20261105-3J4R 回归：
// `precheck: "off"` 时唯一测试 Task 的 seq 也是 0，**不能被当成 Pre-Check**；
// 它的 TASK_TIMEOUT 属平台侧错误 → Run `failed`（不是 service_unavailable）。
func TestRunOutcome_PrecheckOffTimeoutIsFailed(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "plain-probe", "1.0.0", domain.RoleTest, domain.OnFailureContinue)
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "plain-probe", "1.0.0", domain.OnFailureContinue, false)

	out := finishWithTimeout(t, st, runID, taskID, attemptID)
	if out.RunStatus != domain.RunFailed {
		t.Fatalf("Run 终态 = %s, 期望 failed（precheck:off 的 seq=0 不是 Pre-Check）", out.RunStatus)
	}
	var status domain.RunStatus
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id=$1`, runID).Scan(&status); err != nil {
		t.Fatalf("读 run: %v", err)
	}
	if status != domain.RunFailed {
		t.Fatalf("库内 Run 状态 = %s, 期望 failed", status)
	}
}

// TestRunOutcome_RealPrecheckTimeoutIsServiceUnavailable 真 Pre-Check（role=pre_check）
// 超时且实例 retryable=true → 仍按 C-5.8 #4 判 service_unavailable。
func TestRunOutcome_RealPrecheckTimeoutIsServiceUnavailable(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "pre-probe", "1.0.0", domain.RolePreCheck, domain.OnFailureAbortRun)
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "pre-probe", "1.0.0", domain.OnFailureAbortRun, true)

	out := finishWithTimeout(t, st, runID, taskID, attemptID)
	if out.RunStatus != domain.RunServiceUnavailable {
		t.Fatalf("Run 终态 = %s, 期望 service_unavailable（真 Pre-Check 超时可重试）", out.RunStatus)
	}
}

// TestRunOutcome_PrecheckOffFailureIsCompletedWithErrors 非超时的普通失败
// （precheck:off、单 Task、TESTER_NONZERO_EXIT）→ completed_with_errors，不是 service_unavailable。
func TestRunOutcome_PrecheckOffFailureIsCompletedWithErrors(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "plain-probe-2", "1.0.0", domain.RoleTest, domain.OnFailureContinue)
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "plain-probe-2", "1.0.0", domain.OnFailureContinue, false)

	out, err := st.FinishAttempt(context.Background(), AttemptFinish{
		AttemptID: attemptID, TaskID: taskID, RunID: runID,
		Succeeded: false, Phase: "failed",
		Failure: &domain.Failure{Code: domain.CodeTesterNonzeroExit, Message: "exit 1",
			Retryable: false, Detail: map[string]any{"exit_code": 1}},
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("FinishAttempt: %v", err)
	}
	if out.RunStatus != domain.RunCompletedWithErrors {
		t.Fatalf("Run 终态 = %s, 期望 completed_with_errors", out.RunStatus)
	}
}

// TestRunOutcome_PrecheckOffWithPrecheckTesterIsFailed 是 QA 的 r-20261105-TA6M 复现：
// manifest 写 precheck:"off"，但唯一测试 Task 用了 role=pre_check 的 endpoint-probe。
// 判别必须看"这个 Run 有没有注入 Pre-Check"（resolved.precheck.injected=false），
// 不能看 Tester 角色 → TASK_TIMEOUT 属平台侧错误 → Run failed（不是 service_unavailable）。
func TestRunOutcome_PrecheckOffWithPrecheckTesterIsFailed(t *testing.T) {
	st := requireStore(t)
	seedTesterRole(t, st, "endpoint-probe-lite", "1.0.0", domain.RolePreCheck, domain.OnFailureAbortRun)
	// injected=false：用户把它当普通测试用。
	runID, taskID, attemptID := setupSingleTaskRun(t, st, "endpoint-probe-lite", "1.0.0",
		domain.OnFailureAbortRun, false)

	out := finishWithTimeout(t, st, runID, taskID, attemptID)
	if out.RunStatus != domain.RunFailed {
		t.Fatalf("Run 终态 = %s, 期望 failed（injected=false 时不能用 Tester 角色判 Pre-Check）", out.RunStatus)
	}
}