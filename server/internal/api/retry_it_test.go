package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ucm-system/testplane/server/internal/domain"
	"github.com/ucm-system/testplane/server/internal/store"
)

// retryTestEnv 准备一个"终态 Run + 一个 succeeded Task（产物齐全）+ 一个 failed Task"
// 的现场，并把产物写到真实临时 storage-root。
func retryTestEnv(t *testing.T) (srv *Server, st *store.Store, runID string,
	succeededTask, failedTask *store.Task, cleanup func()) {
	t.Helper()
	var cl func()
	srv, st, cl = integrationServer(t)
	cleanup = cl
	ctx := context.Background()

	runID = store.NewRunID(srv.now())
	epKey := "http://retry-ep:8000"
	taskA := store.NewTaskID()
	taskB := store.NewTaskID()
	if _, _, err := st.CreateRun(ctx, store.SubmitParams{
		Run: &store.Run{
			ID: runID, Name: "retry-demo", Kind: "run",
			Manifest:      []byte(`{"run":{"name":"retry-demo"}}`),
			Resolved:      []byte(`{"kind":"run","precheck":{"injected":false}}`),
			CleanupPolicy: "always", SubmittedBy: "dev", SubmittedAt: srv.now(),
		},
		Tasks: []*store.Task{
			{
				ID: taskA, Seq: 0, Name: "a", TesterName: "probe", TesterVersion: "1.0.0",
				ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
				Status: string(domain.TaskSucceeded), CurrentAttempt: 1,
				EndpointKey: &epKey, TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
			},
			{
				ID: taskB, Seq: 1, Name: "b", TesterName: "probe", TesterVersion: "1.0.0",
				ImageRef: "probe:1", Profile: "k8s-npu", ArgsArgv: []byte("[]"),
				Status: string(domain.TaskFailed), CurrentAttempt: 1,
				EndpointKey: &epKey, TimeoutSeconds: 60, OnFailure: domain.OnFailureContinue,
				Error: []byte(`{"code":"TESTER_NONZERO_EXIT","message":"exit 1","retryable":true}`),
			},
		},
		Actor: "user:dev",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// 把 Run 推到终态（failed）：直接改库最省事，判据只关心状态。
	if _, err := st.Pool().Exec(ctx, `UPDATE runs SET status='failed', finished_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("置终态: %v", err)
	}

	// 写 succeeded Task 的真实产物（result.json 必须能过附录 A 校验）。
	dir := filepath.Join(srv.cfg.StorageRoot, runID, "tasks", taskA, "attempts", "1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	result := map[string]any{
		"schema_version": "1.0", "run_id": runID, "task_id": taskA, "attempt_id": taskA + "-a1",
		"tester":     map[string]any{"name": "probe", "version": "1.0.0"},
		"status":     "succeeded",
		"started_at": "2026-11-05T01:00:00Z", "finished_at": "2026-11-05T01:00:10Z",
		"metrics": []any{},
	}
	raw, _ := json.Marshal(result)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o640); err != nil {
		t.Fatalf("write result.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.log"), []byte("hello\n"), 0o640); err != nil {
		t.Fatalf("write log: %v", err)
	}

	succeededTask = &store.Task{ID: taskA, RunID: runID, Name: "a", Status: string(domain.TaskSucceeded)}
	failedTask = &store.Task{ID: taskB, RunID: runID, Name: "b", Status: string(domain.TaskFailed)}
	return srv, st, runID, succeededTask, failedTask, cleanup
}

// TestRetryPlan_ReuseAndRerun 验收 retry-plan 的复用/重跑判定：
// 产物齐全的 succeeded → reuse；failed → rerun；且 retryable=true。
func TestRetryPlan_ReuseAndRerun(t *testing.T) {
	srv, _, runID, succeeded, failed, cleanup := retryTestEnv(t)
	defer cleanup()

	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/retry-plan")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var plan struct {
		RunID     string `json:"run_id"`
		Retryable bool   `json:"retryable"`
		Reuse     []struct {
			TaskID string `json:"task_id"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		}
		Rerun []struct {
			TaskID string `json:"task_id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		BlockedItems []struct {
			TaskID string `json:"task_id"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"blocked_items"`
		ManifestDigest string `json:"manifest_digest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !plan.Retryable {
		t.Fatalf("retryable = false, 期望 true（原因 %s）", rec.Body.String())
	}
	if len(plan.Reuse) != 1 || plan.Reuse[0].TaskID != succeeded.ID || plan.Reuse[0].Reason != "succeeded" {
		t.Fatalf("reuse = %+v, 期望只复用 %s", plan.Reuse, succeeded.ID)
	}
	if len(plan.Rerun) != 1 || plan.Rerun[0].TaskID != failed.ID {
		t.Fatalf("rerun = %+v, 期望只重跑 %s", plan.Rerun, failed.ID)
	}
	if len(plan.BlockedItems) != 0 {
		t.Fatalf("blocked_items 应为空: %+v", plan.BlockedItems)
	}
	if plan.ManifestDigest == "" || plan.ManifestDigest[:7] != "sha256:" {
		t.Fatalf("manifest_digest = %q", plan.ManifestDigest)
	}
}

// TestRetryPlan_BlockedWhenArtifactsMissing 产物不存在的 succeeded Task 不能复用 →
// 进 blocked_items + rerun。
func TestRetryPlan_BlockedWhenArtifactsMissing(t *testing.T) {
	srv, _, runID, succeeded, _, cleanup := retryTestEnv(t)
	defer cleanup()
	// 删掉产物，模拟 PVC 上的文件丢了（DB 仍是 succeeded）。
	dir := filepath.Join(srv.cfg.StorageRoot, runID, "tasks", succeeded.ID, "attempts", "1")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm: %v", err)
	}
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/retry-plan")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var plan struct {
		Reuse []struct {
			TaskID string `json:"task_id"`
		}
		Rerun []struct {
			TaskID string `json:"task_id"`
		}
		BlockedItems []struct {
			TaskID string `json:"task_id"`
			Reason string `json:"reason"`
		} `json:"blocked_items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if len(plan.Reuse) != 0 {
		t.Fatalf("产物缺失时不应复用: %+v", plan.Reuse)
	}
	if len(plan.BlockedItems) != 1 || plan.BlockedItems[0].TaskID != succeeded.ID {
		t.Fatalf("blocked_items = %+v", plan.BlockedItems)
	}
	if len(plan.Rerun) != 2 {
		t.Fatalf("rerun 应包含 succeeded+failed 两个: %+v", plan.Rerun)
	}
}

// TestRetryPlan_NotRetryableWhenRunNotTerminal 非终态 Run → retryable=false 且带原因。
func TestRetryPlan_NotRetryableWhenRunNotTerminal(t *testing.T) {
	srv, st, runID, _, _, cleanup := retryTestEnv(t)
	defer cleanup()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE runs SET status='testing' WHERE id=$1`, runID); err != nil {
		t.Fatalf("改状态: %v", err)
	}
	rec := doGet(t, srv, "/api/v1/runs/"+runID+"/retry-plan")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var plan struct {
		Retryable bool   `json:"retryable"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if plan.Retryable || plan.Reason != "run_not_terminal" {
		t.Fatalf("plan = %+v, 期望 retryable=false reason=run_not_terminal", plan)
	}
}

// TestRetryRun_CreatesNewRunWithReuse 验收：202 + new_run_id；新 Run retry_of 指向原 Run，
// 复用 Task 落 succeeded 且 reused_from_task_id 指向旧 Task，产物可列出；重复 retry → 409。
func TestRetryRun_CreatesNewRunWithReuse(t *testing.T) {
	srv, st, runID, succeeded, failed, cleanup := retryTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	rec := doPost(t, srv, "/api/v1/runs/"+runID+"/retry", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RunID    string `json:"run_id"`
		NewRunID string `json:"new_run_id"`
		RetryOf  string `json:"retry_of"`
		Reuse    []struct {
			TaskID string `json:"task_id"`
		}
		Rerun []struct {
			TaskID string `json:"task_id"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if resp.NewRunID == "" || resp.NewRunID == runID {
		t.Fatalf("new_run_id = %q", resp.NewRunID)
	}

	newRun, err := st.Runs.Get(ctx, nil, resp.NewRunID)
	if err != nil {
		t.Fatalf("读新 Run: %v", err)
	}
	if newRun.RetryOf == nil || *newRun.RetryOf != runID {
		t.Fatalf("retry_of = %v, 期望 %s", newRun.RetryOf, runID)
	}
	// 202 响应也直接带 retry_of（一次请求即可断言；必须与库内一致）。
	if resp.RetryOf != runID {
		t.Fatalf("202 响应 retry_of = %q, 期望 %q", resp.RetryOf, runID)
	}
	// 权威值：GET /runs/{new} 的 runDTO.retry_of。
	got := doGet(t, srv, "/api/v1/runs/"+resp.NewRunID)
	if got.Code != http.StatusOK {
		t.Fatalf("GET 新 Run status = %d", got.Code)
	}
	var runBody struct {
		ID      string  `json:"id"`
		RetryOf *string `json:"retry_of"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &runBody); err != nil {
		t.Fatalf("解析 run: %v", err)
	}
	if runBody.RetryOf == nil || *runBody.RetryOf != runID {
		t.Fatalf("GET /runs/{new}.retry_of = %v, 期望 %q（runDTO 没带 retry_of？）", runBody.RetryOf, runID)
	}
	// 原 Run 不是重试产生的 → retry_of 为 null。
	origBody := doGet(t, srv, "/api/v1/runs/"+runID)
	var orig struct {
		RetryOf *string `json:"retry_of"`
	}
	_ = json.Unmarshal(origBody.Body.Bytes(), &orig)
	if orig.RetryOf != nil {
		t.Fatalf("原 Run 的 retry_of 应为 null，实际 %v", *orig.RetryOf)
	}

	newTasks, _, err := st.Tasks.ListByRun(ctx, nil, resp.NewRunID, "", 100)
	if err != nil {
		t.Fatalf("列新 Task: %v", err)
	}
	if len(newTasks) != 2 {
		t.Fatalf("新 Run Task 数 = %d", len(newTasks))
	}
	var reused, rerun *store.Task
	for i := range newTasks {
		switch newTasks[i].Status {
		case string(domain.TaskSucceeded):
			reused = &newTasks[i]
		case string(domain.TaskPending):
			rerun = &newTasks[i]
		}
	}
	if reused == nil || reused.ReusedFromTaskID == nil || *reused.ReusedFromTaskID != succeeded.ID {
		t.Fatalf("复用 Task = %+v, 期望 reused_from_task_id=%s", reused, succeeded.ID)
	}
	if rerun == nil || rerun.Name != failed.Name {
		t.Fatalf("重跑 Task = %+v", rerun)
	}
	// 复用产物指向新目录且 /runs/{new}/artifacts 能列出。
	arts := doGet(t, srv, "/api/v1/runs/"+resp.NewRunID+"/artifacts")
	if arts.Code != http.StatusOK {
		t.Fatalf("artifacts status = %d", arts.Code)
	}
	var list struct {
		Items []struct {
			Path   string `json:"path"`
			TaskID string `json:"task_id"`
		} `json:"items"`
	}
	_ = json.Unmarshal(arts.Body.Bytes(), &list)
	if len(list.Items) == 0 {
		t.Fatalf("新 Run 的 artifacts 为空（复用产物没索引）")
	}
	found := false
	for _, it := range list.Items {
		if it.TaskID == reused.ID && len(it.Path) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("复用 Task 的产物没进新 Run 索引: %+v", list.Items)
	}

	// 重复 retry：新 Run 非终态 → 409。
	rec = doPost(t, srv, "/api/v1/runs/"+runID+"/retry", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复 retry status = %d, 期望 409（body=%s）", rec.Code, rec.Body.String())
	}
	var e struct {
		Code   string `json:"code"`
		Detail struct {
			Reason string `json:"reason"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != domain.CodeConflict || e.Detail.Reason != "retry_in_progress" {
		t.Fatalf("409 体 = %s", rec.Body.String())
	}
}

// TestRetryTask_AttemptNoIncrements 验收：failed Task 重试 → 202 且 attempt_no = 旧+1。
func TestRetryTask_AttemptNoIncrements(t *testing.T) {
	srv, st, _, _, failed, cleanup := retryTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	rec := doPost(t, srv, "/api/v1/tasks/"+failed.ID+"/retry", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TaskID    string `json:"task_id"`
		AttemptNo int    `json:"attempt_no"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.TaskID != failed.ID || resp.AttemptNo != 2 {
		t.Fatalf("resp = %+v, 期望 attempt_no=2（旧 current_attempt=1）", resp)
	}
	task, err := st.Tasks.Get(ctx, nil, failed.ID)
	if err != nil {
		t.Fatalf("读 task: %v", err)
	}
	if task.Status != string(domain.TaskQueued) {
		t.Fatalf("重试后状态 = %s, 期望 queued", task.Status)
	}
	if task.AvailableAt != nil {
		t.Fatalf("手动重试应立即排队（available_at 必须清空），实际 %v", task.AvailableAt)
	}

	// 再点一次：Task 已排队（非 failed）→ 409。
	rec = doPost(t, srv, "/api/v1/tasks/"+failed.ID+"/retry", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复 retry task status = %d, 期望 409", rec.Code)
	}
}

// TestRetryTask_SucceededTaskRejected succeeded 的 Task 不支持 retry（§8 只覆盖 failed）。
func TestRetryTask_SucceededTaskRejected(t *testing.T) {
	srv, _, _, succeeded, _, cleanup := retryTestEnv(t)
	defer cleanup()
	rec := doPost(t, srv, "/api/v1/tasks/"+succeeded.ID+"/retry", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, 期望 409（succeeded 不可重试）", rec.Code)
	}
}

// helper: doPost 发一个 body 可为空的 POST（dev 鉴权 + 无 Cookie → 不需要 CSRF）。
func doPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestRetryPlan_SecondRetryRunNothingToRetry 是 QA retry-check 抓到的回归：
// 对"retry 产生的新 Run"再取 plan，必须不可重试（nothing_to_retry）——
// 新 Run 里的复用 Task（reused_from_task_id 非空、attempts=0）产物在
// tasks/<task>/reused/，不能被误判成"result.json 不存在 → 需重跑"。
func TestRetryPlan_SecondRetryRunNothingToRetry(t *testing.T) {
	srv, st, runID, succeeded, _, cleanup := retryTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	// 第一次 retry：生成 run2（复用 succeeded，另一个 pending）。
	rec := doPost(t, srv, "/api/v1/runs/"+runID+"/retry", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("第一次 retry status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var first struct {
		NewRunID string `json:"new_run_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.NewRunID == "" {
		t.Fatal("没拿到 new_run_id")
	}

	// 模拟 run2 自然跑到终态：pending 的 Task 补一份 result.json 并置 succeeded。
	run2Tasks, _, err := st.Tasks.ListByRun(ctx, nil, first.NewRunID, "", 100)
	if err != nil {
		t.Fatalf("列 run2 Task: %v", err)
	}
	for i := range run2Tasks {
		task := run2Tasks[i]
		if task.ReusedFromTaskID != nil {
			continue // 复用 Task 的产物由 retry 时硬链接进 reused/
		}
		dir := filepath.Join(srv.cfg.StorageRoot, first.NewRunID, "tasks", task.ID, "attempts", "1")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		body := map[string]any{
			"schema_version": "1.0", "run_id": first.NewRunID, "task_id": task.ID,
			"attempt_id": task.ID + "-a1",
			"tester":     map[string]any{"name": "probe", "version": "1.0.0"},
			"status":     "succeeded",
			"started_at": "2026-11-05T02:00:00Z", "finished_at": "2026-11-05T02:00:10Z",
			"metrics": []any{},
		}
		raw, _ := json.Marshal(body)
		if err := os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o640); err != nil {
			t.Fatalf("write result.json: %v", err)
		}
		if _, err := st.Pool().Exec(ctx, `
			UPDATE tasks SET status='succeeded', current_attempt=1, result_ref=$2 WHERE id=$1`,
			task.ID, filepath.ToSlash(filepath.Join(first.NewRunID, "tasks", task.ID, "attempts", "1", "result.json"))); err != nil {
			t.Fatalf("置 succeeded: %v", err)
		}
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status='succeeded', finished_at=now() WHERE id=$1`, first.NewRunID); err != nil {
		t.Fatalf("置 run2 终态: %v", err)
	}

	// 取 run2 的 plan：必须 retryable=false + nothing_to_retry，且复用项都进 reuse。
	planRec := doGet(t, srv, "/api/v1/runs/"+first.NewRunID+"/retry-plan")
	if planRec.Code != http.StatusOK {
		t.Fatalf("plan status = %d", planRec.Code)
	}
	var plan struct {
		Retryable bool   `json:"retryable"`
		Reason    string `json:"reason"`
		Reuse     []struct {
			TaskID string `json:"task_id"`
			Reason string `json:"reason"`
		} `json:"reuse"`
		Rerun []struct {
			TaskID string `json:"task_id"`
		} `json:"rerun"`
		BlockedItems []struct {
			TaskID string `json:"task_id"`
			Reason string `json:"reason"`
		} `json:"blocked_items"`
	}
	if err := json.Unmarshal(planRec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("解析 plan: %v", err)
	}
	if plan.Retryable {
		t.Fatalf("retry 出来的 Run 不应再可重试: %s", planRec.Body.String())
	}
	if plan.Reason != "nothing_to_retry" {
		t.Fatalf("reason = %q, 期望 nothing_to_retry（body=%s）", plan.Reason, planRec.Body.String())
	}
	if len(plan.Rerun) != 0 || len(plan.BlockedItems) != 0 {
		t.Fatalf("复用的 Task 不得进 rerun/blocked_items: rerun=%+v blocked=%+v",
			plan.Rerun, plan.BlockedItems)
	}
	if len(plan.Reuse) != 2 {
		t.Fatalf("reuse 应包含 2 个 Task（复用来源 + 本轮跑成功），实际 %+v", plan.Reuse)
	}
	// 复用来源的那条要标 reason=reused（不是 succeeded），便于前端区分。
	reusedTagged := false
	for _, it := range plan.Reuse {
		if it.Reason == "reused" {
			reusedTagged = true
		}
	}
	if !reusedTagged {
		t.Fatalf("复用来源的 Task 应标 reason=reused: %+v", plan.Reuse)
	}
	// 再 POST retry 也必须 409（nothing_to_retry）。
	if rec := doPost(t, srv, "/api/v1/runs/"+first.NewRunID+"/retry", ""); rec.Code != http.StatusConflict {
		t.Fatalf("对无可重跑项的 Run 再 retry status = %d, 期望 409", rec.Code)
	}
	_ = succeeded
}
