package main

import (
	"testing"
)

// 实现的 Task 终态可能用 task_finished（而不是 state_changed）表达，
// 事件校验必须认这两种形态，否则会把正确的运行判红。
func TestEventsBackendStyleTerminalEvents(t *testing.T) {
	f := newHappyPlane()
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		runnerEv(3, 1, "pending", "queued"),
		runnerEv(4, 1, "queued", "preparing"),
		runnerStart(5, 1),
		runnerEv(6, 1, "preparing", "running"),
		runnerEv(7, 1, "running", "succeeded"),
		runnerDone(8, 1, "succeeded"),
		runnerEv(9, 2, "pending", "queued"),
		runnerEv(10, 2, "queued", "preparing"),
		runnerStart(11, 2),
		runnerEv(12, 2, "preparing", "running"),
		runnerEv(13, 2, "running", "succeeded"),
		runnerDone(14, 2, "succeeded"),
		// 只有 task_finished 带 status、没有 state_changed 终态转移的形态
		ev(15, "task_finished", map[string]any{"status": "succeeded"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL))
	if code != 0 || !res.OK {
		t.Fatalf("task_finished 形态应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "events.final_state", true)
	mustCheck(t, res, "events.terminal_once", true)
	mustCheck(t, res, "events.transitions_legal", true)
}

// task_finished 只带 status、不带 from/to 时，按 running -> status 归并。
func TestEventsTaskFinishedStatusOnly(t *testing.T) {
	f := newHappyPlane()
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		taskEv(3, "queued", "running"),
		runnerEv(4, 1, "pending", "queued"),
		runnerEv(5, 1, "queued", "preparing"),
		runnerEv(6, 1, "preparing", "running"),
		runnerEv(7, 1, "running", "succeeded"),
		runnerDone(8, 1, "succeeded"),
		runnerEv(9, 2, "pending", "queued"),
		runnerEv(10, 2, "queued", "preparing"),
		runnerEv(11, 2, "preparing", "running"),
		runnerEv(12, 2, "running", "succeeded"),
		runnerDone(13, 2, "succeeded"),
		ev(14, "task_finished", map[string]any{"status": "succeeded"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL))
	if code != 0 || !res.OK {
		t.Fatalf("task_finished(status) 形态应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "events.final_state", true)
	mustCheck(t, res, "events.terminal_once", true)
}

// cancel 边：任意非终态 → cancelling → canceled 必须判合法（R6）。
func TestEventsCancelEdgesLegal(t *testing.T) {
	f := newHappyPlane()
	f.events = []Event{
		ev(1, "submitted", nil),
		taskEv(2, "submitted", "queued"),
		taskEv(3, "queued", "running"),
		taskEv(4, "running", "cancelling"),
		taskEv(5, "cancelling", "canceled"),
		ev(6, "task_finished", map[string]any{"status": "canceled"}),
	}
	f.statuses = []string{"running", "canceled"}
	f.task.Status = "canceled"
	f.task.Runners = []RunnerExecution{
		{Seq: 1, Name: "precheck", RunnerName: "endpoint-probe", Role: "pre_check", Status: "canceled"},
		{Seq: 2, Name: "probe-a", RunnerName: "aisbench-sla", Role: "test", Status: "skipped", StatusReason: "canceled"},
	}
	ts := startFake(t, f)

	code, res, _ := runCommand(t, baseArgs(ts.URL, "--expect", "canceled"))
	if code != 0 || !res.OK {
		t.Fatalf("cancelling/canceled 边应判合法，code=%d", code)
	}
	mustCheck(t, res, "events.transitions_legal", true)
	mustCheck(t, res, "task.canceled_runners", true)
}
