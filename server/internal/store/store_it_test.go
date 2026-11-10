package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ucm-system/testplane/server/internal/dbtest"
	"github.com/ucm-system/testplane/server/internal/domain"
)

// 本文件是 store 的 DB 集成测试（_it = integration test）。
// 需要 PostgreSQL：设置 AG_TEST_DATABASE_URL（如 postgres://u:p@localhost:5432/argus）。
// 未设置时整文件跳过——**不伪造通过**。

func newITStore(t *testing.T) *Store {
	t.Helper()
	dsn, err := dbtest.URL("store")
	if err != nil {
		t.Fatalf("dbtest.URL: %v", err)
	}
	if dsn == "" {
		t.Skip("AG_TEST_DATABASE_URL 未设置，跳过需要 Postgres 的集成测试")
	}
	if _, err := dbtest.ResetSchema(context.Background(), dsn); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	s, err := New(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func itTask(t *testing.T, now time.Time) (*Task, []*RunnerRun) {
	t.Helper()
	task := &Task{
		ID:          NewTaskID(now),
		Name:        "it-task",
		Manifest:    []byte(`{"task":{"name":"it-task","service":{"url":"http://10.0.0.10:8000/v1","model_name":"m"}}}`),
		Resolved:    []byte(`{"precheck":{"injected":true,"runner":"endpoint-probe","version":"1.0.0"},"runners":[{"seq":1,"name":"service-deploy","runner":"service-deploy","role":"deploy","injected":true},{"seq":2,"name":"precheck","runner":"endpoint-probe","role":"pre_check","injected":true},{"seq":3,"name":"acc","runner":"aisbench-acc","version":"1.0.0","role":"test"}],"schema_version":"2.0"}`),
		Env:         []byte(`{}`),
		SecretEnv:   []byte(`[]`),
		SubmittedBy: "it",
	}
	runners := []*RunnerRun{
		{Seq: 1, Name: "service-deploy", RunnerName: "service-deploy", RunnerVersion: "1.0.0",
			Role: domain.RoleDeploy, ImageRef: "img:1", Profile: "k8s-npu",
			InputRef: "attempts/1/input/1", OnFailure: domain.OnFailureAbortTask,
			TimeoutSeconds: 7200, EndpointExclusive: false},
		{Seq: 2, Name: "precheck", RunnerName: "endpoint-probe", RunnerVersion: "1.0.0",
			Role: domain.RolePreCheck, ImageRef: "img:1", Profile: "k8s-npu",
			InputRef: "attempts/1/input/2", OnFailure: domain.OnFailureAbortTask,
			TimeoutSeconds: 600, EndpointExclusive: true,
			EndpointKey: ptrStr("http://10.0.0.10:8000")},
		{Seq: 3, Name: "acc", RunnerName: "aisbench-acc", RunnerVersion: "1.0.0",
			Role: domain.RoleTest, ImageRef: "img:1", Profile: "k8s-npu",
			InputRef: "attempts/1/input/3", OnFailure: domain.OnFailureContinue,
			TimeoutSeconds: 14400, EndpointExclusive: true,
			EndpointKey: ptrStr("http://10.0.0.10:8000")},
	}
	return task, runners
}

func ptrStr(s string) *string                     { return &s }
func ptrInt(i int) *int                           { return &i }
func failurePtr(f domain.Failure) *domain.Failure { return &f }

func TestIT_SubmitPipelineFinish(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	now := time.Now()

	task, runners := itTask(t, now)
	if _, _, err := s.SubmitTask(ctx, SubmitParams{Task: task, Runners: runners, Actor: "it"}); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	m := NewMachines(s, nil)
	// Task: submitted → queued → running；管道第一个 runner 入队。
	if _, err := m.Tasks.Enqueue(ctx, task.ID, "it"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := m.Tasks.BeginPipeline(ctx, task.ID, "it"); err != nil {
		t.Fatalf("BeginPipeline: %v", err)
	}
	if _, err := m.Runners.Queue(ctx, domain.RunnerKey{TaskID: task.ID, AttemptNo: 1, Seq: 1}); err != nil {
		t.Fatalf("Queue seq1: %v", err)
	}

	// Claim：端点租约（deploy runner 无 key，不建租约）。
	res, claimed, reason, err := s.ClaimForExecution(ctx, ClaimRequest{
		Key:         domain.RunnerKey{TaskID: task.ID, AttemptNo: 1, Seq: 1},
		GlobalLimit: 20,
	})
	if err != nil || !claimed || reason != "" {
		t.Fatalf("claim seq1: claimed=%v reason=%q err=%v", claimed, reason, err)
	}
	if res.JobName == "" {
		t.Fatal("JobName 不能为空")
	}

	// deploy runner 失败（DEPLOY_BLOCKED）→ on_failure=abort_task 传播到 clean 为止。
	fin, err := s.FinishRunnerRun(ctx, RunnerFinish{
		Key:       res.Key,
		Succeeded: false,
		Failure:   failurePtr(domain.NewFailure(domain.CodeDeployBlocked, "blocked", map[string]any{"reason": "blocked"})),
		Phase:     "failed",
		ExitCode:  ptrInt(1),
	})
	if err != nil {
		t.Fatalf("FinishRunnerRun: %v", err)
	}
	if !fin.AbortTriggered || len(fin.Skipped) != 2 {
		t.Fatalf("abort 传播: triggered=%v skipped=%v（应为 precheck+test，clean 除外）", fin.AbortTriggered, fin.Skipped)
	}
	if !fin.TaskTerminal || fin.TaskStatus != domain.TaskServiceUnavailable {
		t.Fatalf("Task 终态 = %v（DEPLOY_BLOCKED → service_unavailable），terminal=%v", fin.TaskStatus, fin.TaskTerminal)
	}

	// 库内断言：runner 状态 + attempt 聚合 + 事件数。
	runs, err := s.RunnerRuns.ListByAttempt(ctx, nil, task.ID, 1)
	if err != nil {
		t.Fatalf("ListByAttempt: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].Status != string(domain.RunnerFailed) || runs[0].ErrorCode == nil || *runs[0].ErrorCode != domain.CodeDeployBlocked {
		t.Fatalf("deploy runner = %+v", runs[0])
	}
	if runs[2].Status != string(domain.RunnerPending) { // service-clean 不被 SKIPPED（finally）
		t.Fatalf("clean 应保持 pending（finally 语义），得到 %s", runs[2].Status)
	}
	att, err := s.Attempts.Get(ctx, nil, task.ID, 1)
	if err != nil {
		t.Fatalf("Attempts.Get: %v", err)
	}
	if att.Status != string(domain.AttemptFailed) {
		t.Fatalf("attempt status = %s, want failed", att.Status)
	}
	tk, err := s.Tasks.Get(ctx, nil, task.ID)
	if err != nil {
		t.Fatalf("Tasks.Get: %v", err)
	}
	if tk.Status != string(domain.TaskServiceUnavailable) {
		t.Fatalf("task status = %s", tk.Status)
	}
}

func TestIT_ContinuePipelineAndRetry(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	now := time.Now()

	task, runners := itTask(t, now)
	// 改成无 deploy 的外接形态：deploy runner 也走 continue 会破坏 abort 语义，
	// 这里保持 abort_task 的 deploy，但让它成功，验证串行推进。
	if _, _, err := s.SubmitTask(ctx, SubmitParams{Task: task, Runners: runners, Actor: "it"}); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	m := NewMachines(s, nil)
	_, _ = m.Tasks.Enqueue(ctx, task.ID, "it")
	if _, err := m.Tasks.BeginPipeline(ctx, task.ID, "it"); err != nil {
		t.Fatalf("BeginPipeline: %v", err)
	}

	k1 := domain.RunnerKey{TaskID: task.ID, AttemptNo: 1, Seq: 1}
	if _, err := m.Runners.Queue(ctx, k1); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if _, err := m.Runners.Prepare(ctx, k1, PrepareParams{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := m.Runners.Start(ctx, k1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fin, err := s.FinishRunnerRun(ctx, RunnerFinish{
		Key: k1, Succeeded: true, ExitCode: ptrInt(0), Phase: "succeeded",
		ExportsRef: "attempts/1/exports/1.exports.env",
	})
	if err != nil {
		t.Fatalf("FinishRunnerRun: %v", err)
	}
	if fin.NextQueued == nil || fin.NextQueued.Seq != 2 {
		t.Fatalf("串行推进应为 seq=2，得到 %+v", fin.NextQueued)
	}

	// 重试：Task 未终态时拒绝。
	if _, err := s.RetryTask(ctx, task.ID, "it", "operator", 3); err == nil {
		t.Fatal("非终态 Task 重试应被拒绝")
	}

	// 把剩余 runner 全部终结 → Task succeeded → 重试产生 attempt#2。
	for seq := 2; seq <= 3; seq++ {
		k := domain.RunnerKey{TaskID: task.ID, AttemptNo: 1, Seq: seq}
		if _, err := m.Runners.Queue(ctx, k); err != nil {
			t.Fatalf("Queue seq%d: %v", seq, err)
		}
		if _, err := m.Runners.Prepare(ctx, k, PrepareParams{}); err != nil {
			t.Fatalf("Prepare seq%d: %v", seq, err)
		}
		if _, err := m.Runners.Start(ctx, k); err != nil {
			t.Fatalf("Start seq%d: %v", seq, err)
		}
		if _, err := s.FinishRunnerRun(ctx, RunnerFinish{Key: k, Succeeded: true, ExitCode: ptrInt(0)}); err != nil {
			t.Fatalf("Finish seq%d: %v", seq, err)
		}
	}
	tk, err := s.Tasks.Get(ctx, nil, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tk.Status != string(domain.TaskSucceeded) {
		t.Fatalf("task status = %s, want succeeded", tk.Status)
	}

	no, err := s.RetryTask(ctx, task.ID, "it", "operator", 3)
	if err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	if no != 2 {
		t.Fatalf("attempt_no = %d, want 2", no)
	}
	// 重试超限 → ATTEMPT_EXHAUSTED。
	if _, err := s.RetryTask(ctx, task.ID, "it", "operator", 1); err == nil ||
		!domain.KnownCode("ATTEMPT_EXHAUSTED") {
		t.Fatalf("attempt 上限应拒绝（err=%v）", err)
	} else {
		var fe *domain.FailureError
		if !asFailureError(err, &fe) || fe.Failure.Code != domain.CodeAttemptExhausted {
			t.Fatalf("err = %v, want ATTEMPT_EXHAUSTED", err)
		}
	}
	// 新 attempt 的执行记录：pending、fresh job_id。
	runs2, err := s.RunnerRuns.ListByAttempt(ctx, nil, task.ID, 2)
	if err != nil {
		t.Fatalf("ListByAttempt(2): %v", err)
	}
	if len(runs2) != 3 || runs2[0].Status != string(domain.RunnerPending) {
		t.Fatalf("attempt#2 runs = %+v", runs2)
	}
	if runs2[0].JobID == "" || runs2[0].JobID == JobName(k1) {
		t.Fatalf("fresh job_id 派生错误: %q", runs2[0].JobID)
	}
}

func asFailureError(err error, target **domain.FailureError) bool {
	var fe *domain.FailureError
	if errors.As(err, &fe) {
		*target = fe
		return true
	}
	return false
}

// errorAs 是 errors.As 的包内别名（多个 IT 用例共用）。
func errorAs(err error, target any) bool { return errors.As(err, target) }

func TestIT_EndpointLeaseExclusive(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	now := time.Now()

	// 两个 Task 各带一个 endpoint_exclusive 的 runner，指向同一 endpoint_key。
	mk := func(idx int) (*Task, []*RunnerRun) {
		task, runners := itTask(t, now)
		task.ID = NewTaskID(now.Add(time.Duration(idx) * time.Second))
		for _, rr := range runners {
			rr.EndpointKey = ptrStr("http://10.0.0.10:8000")
			rr.EndpointExclusive = true
		}
		return task, runners
	}
	t1, r1 := mk(1)
	t2, r2 := mk(2)
	for _, p := range []SubmitParams{
		{Task: t1, Runners: r1, Actor: "it"},
		{Task: t2, Runners: r2, Actor: "it"},
	} {
		if _, _, err := s.SubmitTask(ctx, p); err != nil {
			t.Fatalf("SubmitTask: %v", err)
		}
	}
	m := NewMachines(s, nil)
	for _, id := range []string{t1.ID, t2.ID} {
		_, _ = m.Tasks.Enqueue(ctx, id, "it")
		if _, err := m.Tasks.BeginPipeline(ctx, id, "it"); err != nil {
			t.Fatalf("BeginPipeline %s: %v", id, err)
		}
	}
	k1 := domain.RunnerKey{TaskID: t1.ID, AttemptNo: 1, Seq: 1}
	k2 := domain.RunnerKey{TaskID: t2.ID, AttemptNo: 1, Seq: 1}
	for _, k := range []domain.RunnerKey{k1, k2} {
		if _, err := m.Runners.Queue(ctx, k); err != nil {
			t.Fatalf("Queue %s: %v", k.ID(), err)
		}
	}
	_, c1, _, err := s.ClaimForExecution(ctx, ClaimRequest{
		Key: k1, EndpointKey: "http://10.0.0.10:8000", Exclusive: true, GlobalLimit: 20})
	if err != nil || !c1 {
		t.Fatalf("claim1: claimed=%v err=%v", c1, err)
	}
	// 独占期间第二个 Task 的独占 runner 抢不到。
	_, c2, reason, err := s.ClaimForExecution(ctx, ClaimRequest{
		Key: k2, EndpointKey: "http://10.0.0.10:8000", Exclusive: true, GlobalLimit: 20})
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if c2 || reason != domain.WaitSlotWaiting {
		t.Fatalf("claim2 应被端点互斥拒绝：claimed=%v reason=%q", c2, reason)
	}
	// 释放后可再抢。
	if err := s.Leases.Delete(ctx, nil, k1); err != nil {
		t.Fatalf("lease delete: %v", err)
	}
	_, c2, _, err = s.ClaimForExecution(ctx, ClaimRequest{
		Key: k2, EndpointKey: "http://10.0.0.10:8000", Exclusive: true, GlobalLimit: 20})
	if err != nil || !c2 {
		t.Fatalf("claim2 after release: claimed=%v err=%v", c2, err)
	}
}

func TestIT_RunnerRegistryUpsert(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	reg := NewRunnerRegistry(s)
	n := &Runner{
		Name: "aisbench-acc", Version: "1.0.0", DisplayName: "acc", Description: "d",
		Role: domain.RoleTest, OnFailure: domain.OnFailureContinue,
		Entry: []byte(`["/runner/run.sh"]`), InputFormat: domain.InputFormatJSONL,
		Image: "img:1", SourceFile: "x", ContentSHA: "sha-1", RegisteredBy: "it",
	}
	created, err := reg.UpsertRunner(ctx, n)
	if err != nil || !created {
		t.Fatalf("upsert1: created=%v err=%v", created, err)
	}
	created, err = reg.UpsertRunner(ctx, n)
	if err != nil || created {
		t.Fatalf("upsert2（幂等）: created=%v err=%v", created, err)
	}
	n2 := *n
	n2.ContentSHA = "sha-2"
	if _, err := reg.UpsertRunner(ctx, &n2); err == nil {
		t.Fatal("同版本不同内容应被拒绝（RUNNER_VERSION_PINNED）")
	} else {
		var fe *domain.FailureError
		if errorAs(err, &fe) && fe.Failure.Code != domain.CodeRunnerVersionPinned {
			t.Fatalf("err = %v, want RUNNER_VERSION_PINNED", err)
		}
	}
	if err := reg.MarkRunnerOrphaned(ctx, n.Name, n.Version); err != nil {
		t.Fatalf("MarkRunnerOrphaned: %v", err)
	}
	got, err := reg.GetRunner(ctx, n.Name, n.Version)
	if err != nil || !got.Orphaned {
		t.Fatalf("orphaned = %+v err=%v", got, err)
	}
}

func TestIT_SeedAndProfiles(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	if err := s.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	ok, err := s.Profiles.Exists(ctx, nil, "k8s-npu")
	if err != nil || !ok {
		t.Fatalf("profile exists = %v err=%v", ok, err)
	}
}

func TestIT_DeployStateArchive(t *testing.T) {
	s := newITStore(t)
	ctx := context.Background()
	now := time.Now()
	task, runners := itTask(t, now)
	if _, _, err := s.SubmitTask(ctx, SubmitParams{Task: task, Runners: runners, Actor: "it"}); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	waited := int64(42)
	pods := int64(1)
	if err := s.RecordDeployState(ctx, task.ID, 1, "attempts/1/deploy-state.json", domain.DeployDoc{
		Provider: "genet", ReleaseName: "arg-abc", Phase: "deploying",
		WaitedSeconds: &waited, PodCount: &pods,
		Queue: &domain.DeployQueue{Phase: "Inqueue", Pending: 3, Inqueue: 2, Running: 12},
	}); err != nil {
		t.Fatalf("RecordDeployState: %v", err)
	}
	tk, err := s.Tasks.Get(ctx, nil, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tk.Deploy == nil || !containsStr(string(tk.Deploy), `"position":null`) {
		t.Fatalf("deploy 合并缺 position:null（D-46）: %s", tk.Deploy)
	}
	att, err := s.Attempts.Get(ctx, nil, task.ID, 1)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if att.DeployStateRef == nil || *att.DeployStateRef != "attempts/1/deploy-state.json" {
		t.Fatalf("deploy_state_ref = %+v", att.DeployStateRef)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
