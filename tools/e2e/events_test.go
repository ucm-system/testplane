package main

import "testing"

// 实现的 run 终态可能用 cleanup.started + run.finished（而不是 run.state_changed），
// 事件校验必须认这两种事件，否则会把正确的运行判红。
func TestEventsBackendStyleTerminalEvents(t *testing.T) {
	f := newHappyPlane()
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "testing"),
		taskEv(4, "t-1", "pending", "queued"),
		taskEv(5, "t-1", "queued", "preparing"),
		attemptEv(6, "attempt.started", "t-1", "a-1"),
		taskEv(7, "t-1", "preparing", "running"),
		attemptEv(8, "attempt.finished", "t-1", "a-1"),
		taskEv(9, "t-1", "running", "succeeded"),
		taskEv(10, "t-2", "pending", "queued"),
		taskEv(11, "t-2", "queued", "preparing"),
		attemptEv(12, "attempt.started", "t-2", "a-2"),
		attemptEv(13, "attempt.finished", "t-2", "a-2"),
		taskEv(14, "t-2", "running", "succeeded"),
		ev(15, "cleanup.started", map[string]any{"from": "testing", "to": "cleanup"}),
		ev(16, "run.finished", map[string]any{"from": "cleanup", "to": "succeeded"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL))
	if code != 0 || !res.OK {
		t.Fatalf("cleanup.started/run.finished 形态应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "events.final_state", true)
	mustCheck(t, res, "events.terminal_once", true)
	mustCheck(t, res, "events.transitions_legal", true)
}

// run.finished 只带 status、不带 from/to 时，按 cleanup -> status 归并。
func TestEventsRunFinishedStatusOnly(t *testing.T) {
	f := newHappyPlane()
	f.events = []Event{
		ev(1, "run.submitted", map[string]any{"kind": "run"}),
		runEv(2, "submitted", "queued"),
		runEv(3, "queued", "testing"),
		taskEv(4, "t-1", "pending", "queued"),
		taskEv(5, "t-1", "queued", "preparing"),
		attemptEv(6, "attempt.started", "t-1", "a-1"),
		taskEv(7, "t-1", "preparing", "running"),
		attemptEv(8, "attempt.finished", "t-1", "a-1"),
		taskEv(9, "t-1", "running", "succeeded"),
		taskEv(10, "t-2", "pending", "queued"),
		taskEv(11, "t-2", "queued", "preparing"),
		attemptEv(12, "attempt.started", "t-2", "a-2"),
		attemptEv(13, "attempt.finished", "t-2", "a-2"),
		taskEv(14, "t-2", "running", "succeeded"),
		ev(15, "run.finished", map[string]any{"status": "succeeded"}),
	}
	ts := startFake(t, f)

	code, res, stderr := runCommand(t, baseArgs(ts.URL))
	if code != 0 || !res.OK {
		t.Fatalf("run.finished(status) 形态应通过，code=%d stderr：\n%s", code, stderr)
	}
	mustCheck(t, res, "events.final_state", true)
	mustCheck(t, res, "events.terminal_once", true)
}
