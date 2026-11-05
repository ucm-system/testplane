package store

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// seedRunWithTasks 建一个 service 模式的 Run（resolved 里冻结 precheck.injected），
// 加 n 个 pending Task（seq 0..n-1，都带 endpoint_key），并把 Run 推到 queued。
func seedRunWithTasks(t *testing.T, st *Store, injected bool, testerRole string, seqs ...int) (string, map[int]string) {
	t.Helper()
	ctx := context.Background()
	ep := domain.Endpoint{Key: "http://pre-" + NewTaskID() + ":8000",
		BaseURL: "http://127.0.0.1:8000/v1", ModelName: "m"}
	epJSON, _ := json.Marshal(ep)
	runID := NewRunID(time.Now())
	resolved := []byte(`{"kind":"run","precheck":{"injected":false},"tasks":[]}`)
	if injected {
		resolved = []byte(`{"kind":"run","precheck":{"injected":true,"seq":0},"tasks":[]}`)
	}
	tasks := make([]*Task, 0, len(seqs))
	bySeq := map[int]string{}
	for _, seq := range seqs {
		id := NewTaskID()
		bySeq[seq] = id
		key := ep.Key
		tasks = append(tasks, &Task{
			ID: id, Seq: seq, Name: "t" + string(rune('0'+seq)),
			TesterName: "precheck-tester", TesterVersion: "1.0.0", ImageRef: "img:1",
			Profile: "k8s-npu", ArgsArgv: []byte("[]"), OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 60, Status: string(domain.TaskPending),
			EndpointKey: &key,
		})
	}
	if _, _, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "precheck-sql", Kind: "run",
			Manifest: []byte(`{"run":{"name":"precheck-sql"}}`), Resolved: resolved,
			CleanupPolicy: "always", SubmittedBy: "dev", Endpoint: epJSON,
		},
		Tasks: tasks, Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// 注册表里放一个 role=pre_check 的 Tester：仅角色绝不能被当作判据。
	seedTesterRole(t, st, "precheck-tester", "1.0.0", testerRole, domain.OnFailureContinue)

	if _, err := NewMachines(st, nil).Runs.Enqueue(ctx, runID, "system:orchestrator"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := NewMachines(st, nil).Runs.BeginTesting(ctx, runID, ep); err != nil {
		t.Fatalf("begin testing: %v", err)
	}
	return runID, bySeq
}

// TestQueueTasks_PrecheckOffDoesNotGate #14 回归：
// precheck:"off" + 注册表只有 role=pre_check 的 Tester 时，seq=0 的测试 Task 不是 Pre-Check，
// seq>0 的 Task 不得被 precheck_gate 卡住。
func TestQueueTasks_PrecheckOffDoesNotGate(t *testing.T) {
	st := requireStore(t)
	runID, ids := seedRunWithTasks(t, st, false, domain.RolePreCheck, 0, 1)

	// 正常放行（onlySeqZero=false）：两个 Task 都要能跑。
	queued, err := st.QueueTasksTransition(context.Background(), runID, false)
	if err != nil {
		t.Fatalf("QueueTasksTransition: %v", err)
	}
	got := append([]string(nil), queued...)
	sort.Strings(got)
	want := []string{ids[0], ids[1]}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("放行结果 = %v, 期望两个 Task 都放行 %v", queued, want)
	}

	// 关键：即使调用方传 onlySeqZero=true，也不能把 seq=0 的测试 Task 当成 Pre-Check 放行。
	runID2, ids2 := seedRunWithTasks(t, st, false, domain.RolePreCheck, 0, 1)
	queued2, err := st.QueueTasksTransition(context.Background(), runID2, true)
	if err != nil {
		t.Fatalf("QueueTasksTransition(onlySeqZero): %v", err)
	}
	if len(queued2) != 0 {
		t.Fatalf("injected=false 时不应有 Pre-Check 放行，实际 = %v（seq0=%s seq1=%s）",
			queued2, ids2[0], ids2[1])
	}
}

// TestQueueTasks_InjectedPrecheckGatesSeqZero 对照：真注入了 Pre-Check 时只放 seq=0。
func TestQueueTasks_InjectedPrecheckGatesSeqZero(t *testing.T) {
	st := requireStore(t)
	runID, ids := seedRunWithTasks(t, st, true, domain.RolePreCheck, 0, 1)

	queued, err := st.QueueTasksTransition(context.Background(), runID, true)
	if err != nil {
		t.Fatalf("QueueTasksTransition: %v", err)
	}
	if len(queued) != 1 || queued[0] != ids[0] {
		t.Fatalf("应只放行注入的 seq=0 Pre-Check，实际 = %v（seq0=%s）", queued, ids[0])
	}
	if queued[0] == ids[1] {
		t.Fatal("seq=1 的测试 Task 不应在 Pre-Check 阶段放行")
	}
}

// TestPrecheckFlags 校验批量判定复用同一条判据（供 adapter 填 TaskView.IsPrecheck）。
func TestPrecheckFlags(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	runOn, on := seedRunWithTasks(t, st, true, domain.RolePreCheck, 0, 1)
	runOff, off := seedRunWithTasks(t, st, false, domain.RolePreCheck, 0, 1)

	flags, err := st.PrecheckFlags(ctx, []string{on[0], on[1], off[0], off[1]})
	if err != nil {
		t.Fatalf("PrecheckFlags: %v", err)
	}
	if !flags[on[0]] {
		t.Fatalf("注入的 seq=0 应判为 Pre-Check（run=%s task=%s）", runOn, on[0])
	}
	if flags[on[1]] {
		t.Fatalf("注入时 seq=1 不是 Pre-Check")
	}
	if flags[off[0]] || flags[off[1]] {
		t.Fatalf("injected=false 时都不能是 Pre-Check（run=%s）", runOff)
	}
	// 角色判据必须无效：上面的 tester 都是 role=pre_check。
}
