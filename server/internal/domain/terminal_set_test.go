package domain

import (
	"sort"
	"testing"
)

// TestTerminalStatusSets 锁死"终态集合唯一真源"的内容。
//
// 改集合 = 改行为：Task 终态决定 retry 前置与收口守卫；runner 终态决定管道推进、
// 租约回收。任何增删都会红，逼着同步设计文档（03 §3.2 / 02 §2 / C-3）。
func TestTerminalStatusSets(t *testing.T) {
	taskWant := []string{"succeeded", "failed", "canceled", "service_unavailable"}
	taskGot := append([]string{}, TerminalTaskStatusStrings()...)
	sort.Strings(taskGot)
	sort.Strings(taskWant)
	if len(taskGot) != len(taskWant) {
		t.Fatalf("Task 终态集合 = %v，期望 %v", taskGot, taskWant)
	}
	for i := range taskWant {
		if taskGot[i] != taskWant[i] {
			t.Fatalf("Task 终态集合 = %v，期望 %v", taskGot, taskWant)
		}
	}
	for _, st := range TerminalTaskStatuses {
		if !TerminalTaskStatus(st) {
			t.Fatalf("TerminalTaskStatus(%s) 应为 true", st)
		}
	}
	if TerminalTaskStatus(TaskRunning) || TerminalTaskStatus(TaskCancelling) || TerminalTaskStatus(TaskQueued) {
		t.Fatal("非终态被判成终态")
	}

	runnerWant := []string{"succeeded", "failed", "canceled", "skipped"}
	runnerGot := append([]string{}, TerminalRunnerStatusStrings()...)
	sort.Strings(runnerGot)
	sort.Strings(runnerWant)
	if len(runnerGot) != len(runnerWant) {
		t.Fatalf("runner 终态集合 = %v，期望 %v", runnerGot, runnerWant)
	}
	for i := range runnerWant {
		if runnerGot[i] != runnerWant[i] {
			t.Fatalf("runner 终态集合 = %v，期望 %v", runnerGot, runnerWant)
		}
	}
	for _, st := range TerminalRunnerStatuses {
		if !TerminalRunnerStatus(st) {
			t.Fatalf("TerminalRunnerStatus(%s) 应为 true", st)
		}
	}
	if TerminalRunnerStatus(RunnerRunning) || TerminalRunnerStatus(RunnerQueued) {
		t.Fatal("非终态 runner 被判成终态")
	}
}

// TestNoRetryEdges 锁死 v2 裁决（D-69）：runner 级没有重试回路。
// failed/skipped → queued 的边不存在；重试 = 整 Task 新 attempt（ATTEMPT_EXHAUSTED 上限）。
func TestNoRetryEdges(t *testing.T) {
	if _, ok := RunnerTransition(RunnerFailed, RunnerQueued); ok {
		t.Fatal("failed → queued 必须不存在（D-69 取消 runner 级重试）")
	}
	if _, ok := RunnerTransition(RunnerSkipped, RunnerQueued); ok {
		t.Fatal("skipped → queued 必须不存在（D-69）")
	}
}
