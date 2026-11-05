package domain

import (
	"sort"
	"testing"
)

// TestTerminalStatusSets 锁死"终态集合唯一真源"的内容（票 #40-8 / #33-3）。
//
// 改集合 = 改行为：Run 终态决定 retry 前置与"是否已有进行中的 retry"；
// Task 终态决定 cleanup 守卫、租约回收、Run 空闲计数。所以这里把成员逐个钉死，
// 任何增删都会红，逼着同步设计文档（03 §3.2 / 02 §2）。
func TestTerminalStatusSets(t *testing.T) {
	runWant := []string{"succeeded", "completed_with_errors", "failed", "canceled", "service_unavailable"}
	runGot := append([]string{}, TerminalRunStatusStrings()...)
	sort.Strings(runGot)
	sort.Strings(runWant)
	if len(runGot) != len(runWant) {
		t.Fatalf("Run 终态集合 = %v，期望 %v", runGot, runWant)
	}
	for i := range runWant {
		if runGot[i] != runWant[i] {
			t.Fatalf("Run 终态集合 = %v，期望 %v", runGot, runWant)
		}
	}
	for _, st := range TerminalRunStatuses {
		if !TerminalRunStatus(st) {
			t.Fatalf("TerminalRunStatus(%s) 应为 true", st)
		}
	}
	if TerminalRunStatus(RunTesting) || TerminalRunStatus(RunQueued) {
		t.Fatal("非终态被判成终态")
	}

	taskWant := []string{"succeeded", "failed", "canceled", "skipped"}
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
	if TerminalTaskStatus(TaskRunning) || TerminalTaskStatus(TaskQueued) {
		t.Fatal("非终态 Task 被判成终态")
	}
}

// TestTaskRetryEdgeSkippedToQueued 锁死 T12（skipped → queued）：
// 这是 issue #40-1 的规格（UC-12 / §C-5.8），删掉这条边会让 skipped 重试 500/409。
func TestTaskRetryEdgeSkippedToQueued(t *testing.T) {
	if _, ok := TaskTransition(TaskSkipped, TaskQueued); !ok {
		t.Fatal("skipped → queued 必须是合法边（T12）")
	}
	if _, ok := TaskTransition(TaskFailed, TaskQueued); !ok {
		t.Fatal("failed → queued 必须是合法边（T11）")
	}
}
