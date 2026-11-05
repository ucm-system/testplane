package main

import (
	"strings"
	"testing"
)

// 回归：GET /runs/{id}/tasks 首屏若漏掉 seq=0 的 precheck，e2e 必须判红。
func TestTaskListCompleteness(t *testing.T) {
	f := newHappyPlane()
	f.omitSeq0FromTaskList = true
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("任务列表漏 Task 应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "run.tasks_list_complete", false)
	if !strings.Contains(c.Detail, "precheck") {
		t.Fatalf("失败详情没指出漏掉的 precheck：%q", c.Detail)
	}
}

// 平台准备期失败（Job 都没建起来）：没有 Pod 日志/result.json 属正常，
// 只警告不判红；Run 终态是 completed_with_errors（不是 failed）。
func TestPlatformSetupFailureCompletedWithErrors(t *testing.T) {
	f := newHappyPlane()
	f.run.Status = "completed_with_errors"
	f.run.Tasks = []Task{
		{
			ID: "t-1", Name: "precheck", Status: "failed", Attempts: 1,
			Error: &Failure{
				Code:      "EXECUTOR_UNAVAILABLE",
				Message:   "executor: invalid job spec",
				Retryable: true,
			},
		},
		{ID: "t-2", Name: "probe-a", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"testing", "completed_with_errors"}
	f.reportBody = []byte(`{"run_id":"r-test-1","status":"completed_with_errors"}`)
	f.attempts = map[string][]Attempt{
		"t-1": {{ID: "a-1", Idx: 1, TaskID: "t-1", Status: "failed", Error: &Failure{
			Code: "EXECUTOR_UNAVAILABLE", Message: "executor: invalid job spec", Retryable: true,
		}}},
	}
	f.logs = map[string]string{}    // 没有 Pod → 日志 404
	f.results = map[string]string{} // 没有 result.json → 404
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "testing"),
		taskEv(4, "t-1", "pending", "queued"),
		taskEv(5, "t-1", "queued", "preparing"),
		attemptEv(6, "attempt.started", "t-1", "a-1"),
		attemptEv(7, "attempt.finished", "t-1", "a-1"),
		taskEv(8, "t-1", "preparing", "failed"),
		taskEv(9, "t-2", "pending", "skipped"),
		runEv(10, "testing", "cleanup"),
		runEv(11, "cleanup", "completed_with_errors"),
		ev(12, "run.finished", map[string]any{"status": "completed_with_errors"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "completed_with_errors",
		"--expect-task-error-code", "EXECUTOR_UNAVAILABLE",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("平台准备期失败场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	if res.Actual != "completed_with_errors" {
		t.Fatalf("终态应为 completed_with_errors，实际 %q", res.Actual)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("没有日志/result 时应至少有一条警告")
	}
	for _, c := range res.Checks {
		if !c.OK {
			t.Fatalf("不应有失败检查项：%s —— %s", c.Name, c.Detail)
		}
	}
}
