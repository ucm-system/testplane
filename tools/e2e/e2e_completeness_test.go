package main

import (
	"strings"
	"testing"
)

// 回归：GET /tasks/{id}/runners 首屏若漏掉 seq=1 的注入行（service-deploy/precheck），
// e2e 必须判红。
func TestRunnerListCompleteness(t *testing.T) {
	f := newHappyPlane()
	f.omitSeq1FromRunnerList = true
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("runner 列表漏 seq=1 应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "task.runners_list_complete", false)
	if !strings.Contains(c.Detail, "precheck") {
		t.Fatalf("失败详情没指出漏掉的 precheck：%q", c.Detail)
	}
}

// 平台准备期失败（Job 都没建起来）：没有 Pod 日志/result.json 属正常，
// 只警告不判红；Task 终态是 failed（v2 没有 completed_with_errors，D-73）。
func TestPlatformSetupFailureFails(t *testing.T) {
	f := newHappyPlane()
	f.task.Status = "failed"
	f.task.Runners = []RunnerExecution{
		{
			Seq: 1, Name: "precheck", RunnerName: "endpoint-probe", Role: "pre_check", Status: "failed",
			Error: &Failure{
				Code:      "EXECUTOR_UNAVAILABLE",
				Message:   "executor: invalid job spec",
				Retryable: true,
			},
		},
		{Seq: 2, Name: "probe-a", RunnerName: "aisbench-sla", Role: "test", Status: "skipped", StatusReason: "upstream_abort"},
	}
	f.statuses = []string{"queued", "failed"}
	f.reportBody = []byte(`{"task_id":"t-test-1","status":"failed"}`)
	f.logs = map[string]string{}    // 没有 Pod → 日志 404
	f.results = map[string]string{} // 没有 result.json → 404
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		taskEv(3, "queued", "running"),
		runnerEv(4, 1, "pending", "queued"),
		runnerEv(5, 1, "queued", "preparing"),
		runnerEv(6, 1, "preparing", "failed"),
		runnerDone(7, 1, "failed"),
		runnerEv(8, 2, "pending", "skipped"),
		taskEv(9, "running", "failed"),
		ev(10, "task_finished", map[string]any{"status": "failed"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "failed",
		"--expect-runner-error-code", "EXECUTOR_UNAVAILABLE",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("平台准备期失败场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	if res.Actual != "failed" {
		t.Fatalf("终态应为 failed，实际 %q", res.Actual)
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

// skipped 必带 reason（D-06）：status_reason 缺失要判红。
func TestSkippedWithoutReasonFails(t *testing.T) {
	f := newHappyPlane()
	f.task.Runners[1].Status = "skipped"
	f.task.Runners[1].StatusReason = ""
	f.task.RunnerCounts = &RunnerCounts{Total: 2, Skipped: 1, Succeeded: 1}
	f.statuses = []string{"queued", "succeeded"}
	f.task.Status = "succeeded"
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL))
	if code != 1 {
		t.Fatalf("skipped 缺 reason 应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "task.runners_valid", false)
	if !strings.Contains(c.Detail, "skipped") {
		t.Fatalf("失败详情应指出 skipped 缺 reason：%q", c.Detail)
	}
}
