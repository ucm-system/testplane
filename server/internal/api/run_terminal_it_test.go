package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// seedRunWithPendingTasks 造一个"刚入队、Task 还全是 pending"的 Run。
func seedRunWithPendingTasks(t *testing.T, srv *Server, st *store.Store, n int) (string, []string) {
	t.Helper()
	ctx := context.Background()
	runID := store.NewRunID(srv.now())
	epKey := "http://zombie-ep:8000"
	tasks := make([]*store.Task, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := store.NewTaskID()
		ids = append(ids, id)
		tasks = append(tasks, &store.Task{
			ID: id, Seq: i, Name: "t" + string(rune('a'+i)), TesterName: "probe", TesterVersion: "1.0.0",
			ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
			Status: string(domain.TaskPending), EndpointKey: &epKey,
			TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
		})
	}
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "zombie-demo", Kind: "run",
			Manifest:      []byte(`{"run":{"name":"zombie-demo"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", SubmittedAt: srv.now(),
		},
		Tasks: tasks, Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE runs SET status='deploying' WHERE id=$1`, runID); err != nil {
		t.Fatalf("置 deploying: %v", err)
	}
	return runID, ids
}

// assertTasksSkipped 断言这些 Task 都被收口成 skipped，带合法原因，且 waiting_reason 已清空。
func assertTasksSkipped(t *testing.T, st *store.Store, ids []string, wantReason string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		task, err := st.Tasks.Get(ctx, nil, id)
		if err != nil {
			t.Fatalf("读 task %s: %v", id, err)
		}
		if task.Status != string(domain.TaskSkipped) {
			t.Fatalf("task %s status = %s, 期望 skipped（终态 Run 下不得残留非终态 Task）", id, task.Status)
		}
		if task.StatusReason == nil || *task.StatusReason == "" {
			t.Fatalf("task %s 缺 status_reason（要写清为什么被跳过）", id)
		}
		if wantReason != "" && *task.StatusReason != wantReason {
			t.Fatalf("task %s status_reason = %q, 期望 %q", id, *task.StatusReason, wantReason)
		}
		if task.WaitingReason != nil {
			t.Fatalf("task %s waiting_reason = %v, 期望清空（终态后不该还显示在等待列表）",
				id, *task.WaitingReason)
		}
	}
}

// TestRunTerminal_DeployBlockedSkipsPendingTasks 是 D-60 的核心红证：
// deploying → service_unavailable（R5，部署阻塞/超窗）时，必须把 pending Task 收口成 skipped。
// 改前：Effects 只为 canceled/failed 返回 EffectSkipUnfinished → 这里会红。
func TestRunTerminal_DeployBlockedSkipsPendingTasks(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	runID, ids := seedRunWithPendingTasks(t, srv, st, 2)
	f := &domain.Failure{Code: domain.CodeDeployBlocked,
		Message: "deploy did not become ready", Retryable: true,
		Detail: map[string]any{"reason": "timeout"}}
	// R5：deploying → service_unavailable（走 kernel Apply，副作用在事务内）。
	if _, err := store.NewMachines(st, nil).Runs.DeployBlocked(ctx, runID, *f); err != nil {
		t.Fatalf("DeployBlocked: %v", err)
	}
	run, err := st.Runs.Get(ctx, nil, runID)
	if err != nil {
		t.Fatalf("读 run: %v", err)
	}
	if run.Status != string(domain.RunServiceUnavailable) {
		t.Fatalf("run status = %s, 期望 service_unavailable", run.Status)
	}
	assertTasksSkipped(t, st, ids, domain.SkipServiceUnavailable)
}

// TestRunTerminal_CancelSkipsPendingTasks 取消路径同样收口（既有行为，防回归）。
func TestRunTerminal_CancelSkipsPendingTasks(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	runID, ids := seedRunWithPendingTasks(t, srv, st, 2)
	m := store.NewMachines(st, nil)
	if _, _, err := st.RequestCancelRunTx(ctx, runID, "user:dev"); err != nil {
		t.Fatalf("RequestCancelRunTx: %v", err)
	}
	if _, err := m.Runs.Canceled(ctx, runID); err != nil {
		t.Fatalf("Canceled: %v", err)
	}
	run, err := st.Runs.Get(ctx, nil, runID)
	if err != nil {
		t.Fatalf("读 run: %v", err)
	}
	if run.Status != string(domain.RunCanceled) {
		t.Fatalf("run status = %s, 期望 canceled", run.Status)
	}
	assertTasksSkipped(t, st, ids, domain.SkipCanceled)
}

// TestQueueKPIsIgnoreTerminalRuns 是"查询侧防线"的验收：
// 即使库里还有历史僵尸 Task（Run 已终态），排队 KPI 与等待列表也不得统计它们。
func TestQueueKPIsIgnoreTerminalRuns(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	// 手工造一条"历史脏数据"：Run 终态 + Task 仍 pending（模拟修复前的存量）。
	runID, ids := seedRunWithPendingTasks(t, srv, st, 1)
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status='service_unavailable', finished_at=GREATEST(now(), submitted_at) WHERE id=$1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}

	rows, err := st.Tasks.WaitingTasks(ctx, nil, 400)
	if err != nil {
		t.Fatalf("WaitingTasks: %v", err)
	}
	for _, r := range rows {
		if r.RunID == runID {
			t.Fatalf("等待列表仍包含终态 Run %s 的 Task %s", runID, r.TaskID)
		}
	}
	// 等待原因 KPI 也要排除它：把 Run 拉回非终态后，总量必须**恰好增加** len(ids)。
	before, err := st.Tasks.WaitingBreakdown(ctx, nil)
	if err != nil {
		t.Fatalf("WaitingBreakdown: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE runs SET status='testing', finished_at=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("回置 testing: %v", err)
	}
	after, err := st.Tasks.WaitingBreakdown(ctx, nil)
	if err != nil {
		t.Fatalf("WaitingBreakdown(2): %v", err)
	}
	sum := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	if delta := sum(after) - sum(before); delta != len(ids) {
		t.Fatalf("非终态 Run 的 Task 应计入等待统计（增量 %d，期望 %d）；before=%v after=%v",
			delta, len(ids), before, after)
	}
}

// TestRetryTask_TerminalRunRejected 是 D-60 的第二个根因：
// 对**已终态 Run** 的 Task 重试必须 409（否则会造出永远不会被调度的 queued Task）。
func TestRetryTask_TerminalRunRejected(t *testing.T) {
	srv, st, cleanup := integrationServer(t)
	defer cleanup()
	ctx := context.Background()

	runID, ids := seedRunWithPendingTasks(t, srv, st, 1)
	// 让 Task 处于 failed（可重试状态），Run 置终态 completed_with_errors。
	if _, err := st.Pool().Exec(ctx, `
		UPDATE tasks SET status='failed', current_attempt=1,
		       error='{"code":"TESTER_NONZERO_EXIT","message":"exit 1","retryable":true}'::jsonb
		 WHERE id=$1`, ids[0]); err != nil {
		t.Fatalf("置 failed: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status='completed_with_errors', finished_at=GREATEST(now(), submitted_at) WHERE id=$1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}

	rec := doPost(t, srv, "/api/v1/tasks/"+ids[0]+"/retry", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, 期望 409（终态 Run 不可重试单个 Task）: %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Code   string `json:"code"`
		Detail struct {
			Reason string `json:"reason"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != domain.CodeConflict {
		t.Fatalf("code = %q, 期望 CONFLICT", e.Code)
	}
	if e.Detail.Reason != "run_terminal" {
		t.Fatalf("reason = %q, 期望 run_terminal", e.Detail.Reason)
	}
	// Task 仍是 failed（没有被推成 queued）。
	task, err := st.Tasks.Get(ctx, nil, ids[0])
	if err != nil {
		t.Fatalf("读 task: %v", err)
	}
	if task.Status != string(domain.TaskFailed) {
		t.Fatalf("task status = %s, 期望 failed（不该被改成 queued）", task.Status)
	}
}
