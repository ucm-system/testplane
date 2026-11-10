package main

import (
	"strings"
	"testing"
)

// newPrecheckRetryableUnavailablePlane 模拟 §7.5 #4 的终态路径：
// pre_check 在窗口内一直 5xx/不可达（可重试失败）→ Task service_unavailable，
// precheck failed、其余 runner skipped(upstream_abort)。
func newPrecheckRetryableUnavailablePlane() *fakePlane {
	return newUnavailablePlane()
}

// §7.5 #4：可重试的 pre_check 失败归 service_unavailable（不是 failed）。
func TestServiceUnavailablePrecheckRetryable(t *testing.T) {
	ts := startFake(t, newPrecheckRetryableUnavailablePlane())
	code, res, stderr := runCommand(t, baseArgs(ts.URL,
		"--expect", "service_unavailable",
		"--expect-runner-error-code", "PRECHECK_FAILED",
		"--expect-retryable=true",
	))
	if code != 0 || !res.OK {
		t.Fatalf("precheck 可重试失败场景应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "task.unavailable_runners_skipped", true)
	mustCheck(t, res, "expect.runner_error_code", true)
	mustCheck(t, res, "expect.retryable", true)
	mustCheck(t, res, "runner.precheck.logs", true)
	if res.Actual != "service_unavailable" {
		t.Fatalf("实际终态 %q", res.Actual)
	}
}

// 同一份数据用 --expect failed 应判红（防止终态归类放水）。
func TestPrecheckRetryableWrongExpectation(t *testing.T) {
	ts := startFake(t, newPrecheckRetryableUnavailablePlane())
	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "failed",
		"--expect-retryable=true",
	))
	if code != 1 {
		t.Fatalf("终态不符应退出码 1，实际 %d", code)
	}
	mustCheck(t, res, "task.expected_status", false)
}

// §7.5 归类一致性：retryable=true 的探活失败却落到 failed，必须判红。
func TestPrecheckClassificationInconsistent(t *testing.T) {
	f := newPrecheck401Plane()
	// 把 precheck 的失败改成"可重试"，但 Task 仍是 failed
	f.task.Runners[0].Error.Retryable = true
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "failed",
	))
	if code != 1 {
		t.Fatalf("终态归类不一致应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "task.precheck_retryable_consistent", false)
	if !strings.Contains(c.Detail, "PRECHECK_FAILED") || !strings.Contains(c.Detail, "retryable=true") {
		t.Fatalf("失败详情没给 code/retryable：%q", c.Detail)
	}
}

// abort_task 传播：precheck 失败后其余 runner 还停在 pending/running，必须判红。
func TestAbortPropagation(t *testing.T) {
	f := newPrecheck401Plane()
	f.task.Runners[1].Status = "running"
	f.statuses = []string{"queued", "failed"}
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL,
		"--expect", "failed",
	))
	if code != 1 {
		t.Fatalf("abort 未传播应退出码 1，实际 %d", code)
	}
	c := mustCheck(t, res, "task.abort_propagation", false)
	if !strings.Contains(c.Detail, "probe-a=running") {
		t.Fatalf("失败详情没指出未收口的 runner：%q", c.Detail)
	}
}
