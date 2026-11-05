package main

import (
	"strings"
	"testing"
)

// newPrecheckRetryableUnavailablePlane 模拟 C-5.8 #4 的终态路径：
// pre_check 在窗口内一直 5xx/不可达（可重试失败）→ Run service_unavailable，
// precheck failed、其余 Task skipped(upstream_abort)。
func newPrecheckRetryableUnavailablePlane() *fakePlane {
	f := newHappyPlane()
	f.run.Status = "service_unavailable"
	f.run.Error = &Failure{Code: "PRECHECK_FAILED", Message: "窗口内未就绪", Retryable: true}
	f.run.Tasks = []Task{
		{
			ID: "t-1", Name: "precheck", Status: "failed", Attempts: 1,
			Error: &Failure{Code: "PRECHECK_FAILED", Message: "探测超时", Retryable: true},
		},
		{ID: "t-2", Name: "probe-a", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"testing", "cleanup", "service_unavailable"}
	f.reportBody = []byte(`{"run_id":"r-test-1","status":"service_unavailable","failures":[{"code":"PRECHECK_FAILED","retryable":true}]}`)
	f.attempts = map[string][]Attempt{
		"t-1": {{ID: "a-1", Idx: 1, TaskID: "t-1", Status: "failed"}},
	}
	f.logs = map[string]string{
		"t-1:1:stdout": "precheck: 第 3 次探测：未就绪 —— HTTP 500\n",
	}
	f.results = map[string]string{
		"t-1:1": `{"schema_version":"1.0","status":"failed","error":{"code":"PRECHECK_FAILED","retryable":true}}`,
	}
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "testing"),
		taskEv(4, "t-1", "pending", "queued"),
		taskEv(5, "t-1", "queued", "preparing"),
		attemptEv(6, "attempt.started", "t-1", "a-1"),
		taskEv(7, "t-1", "preparing", "running"),
		attemptEv(8, "attempt.finished", "t-1", "a-1"),
		taskEv(9, "t-1", "running", "failed"),
		taskEv(10, "t-2", "pending", "skipped"),
		runEv(11, "testing", "cleanup"),
		runEv(12, "cleanup", "service_unavailable"),
		ev(13, "run.finished", map[string]any{"status": "service_unavailable"}),
	}
	return f
}

// C-5.8 #4：可重试的 pre_check 失败归 service_unavailable（不是 completed_with_errors）。
func TestServiceUnavailablePrecheckRetryable(t *testing.T) {
	ts := startFake(t, newPrecheckRetryableUnavailablePlane())
	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "service_unavailable",
		"--expect-task-error-code", "PRECHECK_FAILED",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("precheck 可重试失败场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "run.unavailable_tasks_skipped", true)
	mustCheck(t, res, "expect.task_error_code", true)
	mustCheck(t, res, "expect.retryable", true)
	mustCheck(t, res, "task.precheck.attempt.1.logs", true)
	if res.Actual != "service_unavailable" {
		t.Fatalf("实际终态 %q", res.Actual)
	}
}

// 同一份数据用 --expect completed_with_errors 应判红（防止终态归类放水）。
func TestPrecheckRetryableWrongExpectation(t *testing.T) {
	ts := startFake(t, newPrecheckRetryableUnavailablePlane())
	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
		"--expect-retryable=true",
	))
	if code != 1 {
		t.Fatalf("终态不符应退出码 1，实际 %d", code)
	}
	mustCheck(t, res, "run.expected_status", false)
}

// C-5.8 归类一致性：retryable=true 的探活失败却落到 completed_with_errors，必须判红。
func TestPrecheckClassificationInconsistent(t *testing.T) {
	f := newPrecheck401Plane()
	// 把 precheck 的失败改成"可重试"，但 Run 仍是 completed_with_errors
	f.run.Tasks[0].Error.Retryable = true
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
	))
	if code != 1 {
		t.Fatalf("终态归类不一致应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "run.precheck_retryable_consistent", false)
	if !strings.Contains(c.Detail, "PRECHECK_FAILED") || !strings.Contains(c.Detail, "retryable=true") {
		t.Fatalf("失败详情没给 code/retryable：%q", c.Detail)
	}
}

// abort_run 传播：precheck 失败后其余 Task 还停在 pending/running，必须判红。
func TestAbortPropagation(t *testing.T) {
	f := newPrecheck401Plane()
	f.run.Tasks[1].Status = "running"
	f.statuses = []string{"testing", "completed_with_errors"}
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
	))
	if code != 1 {
		t.Fatalf("abort 未传播应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "run.abort_propagation", false)
	if !strings.Contains(c.Detail, "probe-a=running") {
		t.Fatalf("失败详情没指出未收口的 Task：%q", c.Detail)
	}
}
