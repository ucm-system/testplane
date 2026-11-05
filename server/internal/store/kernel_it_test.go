package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// 集成测试：自起 PG（`TESTPLANE_TEST_DATABASE_URL`），没设就 skip。
// 每个测试用独立 id，互不影响；TestMain 里重建 schema，保证从零迁移。

var (
	testStoreOnce sync.Once
	testStoreVal  *Store
	testStoreErr  error
)

func testDSN() string { return os.Getenv("TESTPLANE_TEST_DATABASE_URL") }

func TestMain(m *testing.M) {
	dsn := testDSN()
	if dsn != "" {
		if err := resetSchema(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "重置测试库失败: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func resetSchema(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		return err
	}
	return nil
}

// requireStore 返回共享的 Store（迁移已在首次调用时跑完）；无 DSN 时 skip。
func requireStore(t *testing.T) *Store {
	t.Helper()
	if testDSN() == "" {
		t.Skip("跳过：未设置 TESTPLANE_TEST_DATABASE_URL")
	}
	testStoreOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		testStoreVal, testStoreErr = New(ctx, testDSN(), 8)
	})
	if testStoreErr != nil {
		t.Fatalf("连接测试库失败: %v", testStoreErr)
	}
	return testStoreVal
}

// ---- 种子数据（直接 SQL，模拟"历史状态"）--------------------------------

func seedRun(t *testing.T, st *Store, status domain.RunStatus) string {
	t.Helper()
	id := NewRunID(time.Now())
	_, err := st.Pool().Exec(context.Background(), `
		INSERT INTO runs (id, name, kind, status, manifest, resolved, submitted_by, last_event_seq)
		VALUES ($1, $2, 'run', $3, '{}', '{}', 'tester', 0)`, id, "run-"+id, string(status))
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func seedRunWithEndpoint(t *testing.T, st *Store, status domain.RunStatus) string {
	t.Helper()
	id := seedRun(t, st, status)
	_, err := st.Pool().Exec(context.Background(),
		`UPDATE runs SET endpoint = $2 WHERE id = $1`, id,
		`{"url":"http://127.0.0.1:8000/v1","key":"http://127.0.0.1:8000","source":"service"}`)
	if err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	return id
}

func seedTask(t *testing.T, st *Store, runID string, seq int, status domain.TaskStatus, endpointKey string) string {
	t.Helper()
	id := NewTaskID()
	var key *string
	if endpointKey != "" {
		key = &endpointKey
	}
	_, err := st.Pool().Exec(context.Background(), `
		INSERT INTO tasks (id, run_id, seq, name, tester_name, tester_version, image_ref, profile,
		                   args_raw, args_argv, on_failure, timeout_seconds, status, endpoint_key,
		                   endpoint_exclusive, retry_policy)
		VALUES ($1,$2,$3,$4,'t','1.0.0','img:1','default','', '[]','continue',3600,$5,$6,false,'{"backoff_seconds":30}')`,
		id, runID, seq, fmt.Sprintf("t-%d-%s", seq, id), string(status), key)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

func seedAttempt(t *testing.T, st *Store, taskID string, status domain.AttemptStatus) string {
	t.Helper()
	id := AttemptID(taskID, 1)
	_, err := st.Pool().Exec(context.Background(), `
		INSERT INTO attempts (id, task_id, idx, status, job_id) VALUES ($1,$2,1,$3,$4)`,
		id, taskID, string(status), "j-"+id)
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

func applyInTx(t *testing.T, st *Store, tr domain.Transition) (domain.Applied, error) {
	t.Helper()
	var applied domain.Applied
	err := st.InTx(context.Background(), func(tx pgx.Tx) error {
		a, err := st.Apply(context.Background(), tx, tr)
		if err != nil {
			return err
		}
		applied = a
		return nil
	})
	return applied, err
}

func currentRunStatus(t *testing.T, st *Store, id string) domain.RunStatus {
	t.Helper()
	var s domain.RunStatus
	if err := st.Pool().QueryRow(context.Background(), `SELECT status FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("读 run 状态: %v", err)
	}
	return s
}

func currentTaskStatus(t *testing.T, st *Store, id string) domain.TaskStatus {
	t.Helper()
	var s domain.TaskStatus
	if err := st.Pool().QueryRow(context.Background(), `SELECT status FROM tasks WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("读 task 状态: %v", err)
	}
	return s
}

func currentAttemptStatus(t *testing.T, st *Store, id string) domain.AttemptStatus {
	t.Helper()
	var s domain.AttemptStatus
	if err := st.Pool().QueryRow(context.Background(), `SELECT status FROM attempts WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("读 attempt 状态: %v", err)
	}
	return s
}

func eventCount(t *testing.T, st *Store, runID string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM state_events WHERE run_id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("读事件数: %v", err)
	}
	return n
}

var allRunStatuses = []domain.RunStatus{
	domain.RunSubmitted, domain.RunQueued, domain.RunDeploying, domain.RunTesting,
	domain.RunCleanup, domain.RunCancelling, domain.RunSucceeded, domain.RunCompletedWithErrors,
	domain.RunServiceUnavailable, domain.RunFailed, domain.RunCanceled,
}

var allTaskStatuses = []domain.TaskStatus{
	domain.TaskPending, domain.TaskQueued, domain.TaskPreparing, domain.TaskRunning,
	domain.TaskSucceeded, domain.TaskFailed, domain.TaskCanceled, domain.TaskSkipped,
}

var allAttemptStatuses = []domain.AttemptStatus{
	domain.AttemptPreparing, domain.AttemptRunning, domain.AttemptSucceeded,
	domain.AttemptFailed, domain.AttemptCanceled,
}

// transitionPayload 给各实体转移补上守卫需要的 payload。
func runPayload(to domain.RunStatus) map[string]any {
	if to == domain.RunTesting {
		return map[string]any{"endpoint": map[string]any{
			"url": "http://127.0.0.1:8000/v1", "key": "http://127.0.0.1:8000", "source": "service",
		}}
	}
	return nil
}

// TestKernel_AllLegalEdgesApply 穷举转移表里的每一条边，各造一个前置状态的实体并执行。
func TestKernel_AllLegalEdgesApply(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	t.Run("run", func(t *testing.T) {
		for _, from := range allRunStatuses {
			for _, to := range allRunStatuses {
				spec, ok := domain.RunTransition(from, to)
				if !ok {
					continue
				}
				from, to, spec := from, to, spec
				t.Run(string(from)+"__"+string(to), func(t *testing.T) {
					runID := seedRun(t, st, from)
					before := eventCount(t, st, runID)
					a, err := applyInTx(t, st, domain.Transition{
						Entity: domain.EntityRun, ID: runID,
						From:    []domain.Status{from.Status()},
						To:      to.Status(),
						Command: spec.Command,
						Actor:   "test:kernel",
						Payload: runPayload(to),
					})
					if err != nil {
						t.Fatalf("合法转移 %s->%s 报错: %v", from, to, err)
					}
					if a.Noop {
						t.Fatalf("不应是 Noop: %s->%s", from, to)
					}
					if a.From != from.Status() || a.To != to.Status() {
						t.Fatalf("Applied = %s->%s, 期望 %s->%s", a.From, a.To, from, to)
					}
					if a.Seq <= 0 {
						t.Fatalf("seq 必须 >0, 得到 %d", a.Seq)
					}
					if got := currentRunStatus(t, st, runID); got != to {
						t.Fatalf("库内状态 = %s, 期望 %s", got, to)
					}
					if got := eventCount(t, st, runID); got <= before {
						t.Fatalf("未写事件: before=%d after=%d", before, got)
					}
				})
			}
		}
	})

	t.Run("task", func(t *testing.T) {
		for _, from := range allTaskStatuses {
			for _, to := range allTaskStatuses {
				spec, ok := domain.TaskTransition(from, to)
				if !ok {
					continue
				}
				from, to, spec := from, to, spec
				t.Run(string(from)+"__"+string(to), func(t *testing.T) {
					runID := seedRunWithEndpoint(t, st, domain.RunTesting)
					taskID := seedTask(t, st, runID, 1, from, "http://127.0.0.1:8000")
					reason := ""
					if to == domain.TaskSkipped {
						reason = domain.SkipUpstreamAbort
					}
					before := eventCount(t, st, runID)
					a, err := applyInTx(t, st, domain.Transition{
						Entity: domain.EntityTask, ID: taskID,
						From:    []domain.Status{from.Status()},
						To:      to.Status(),
						Command: spec.Command,
						Reason:  reason,
						Actor:   "test:kernel",
					})
					if err != nil {
						t.Fatalf("合法转移 %s->%s 报错: %v", from, to, err)
					}
					if got := currentTaskStatus(t, st, taskID); got != to {
						t.Fatalf("库内状态 = %s, 期望 %s", got, to)
					}
					if got := eventCount(t, st, runID); got <= before {
						t.Fatalf("未写事件")
					}
					if to == domain.TaskQueued && from == domain.TaskFailed {
						// T11：必须设置 available_at = now + backoff
						var avail *time.Time
						if err := st.Pool().QueryRow(ctx,
							`SELECT available_at FROM tasks WHERE id=$1`, taskID).Scan(&avail); err != nil {
							t.Fatalf("读 available_at: %v", err)
						}
						if avail == nil || !avail.After(time.Now()) {
							t.Fatalf("重试未设置 available_at: %v", avail)
						}
					}
					_ = a
				})
			}
		}
	})

	t.Run("attempt", func(t *testing.T) {
		for _, from := range allAttemptStatuses {
			for _, to := range allAttemptStatuses {
				spec, ok := domain.AttemptTransition(from, to)
				if !ok {
					continue
				}
				from, to, spec := from, to, spec
				t.Run(string(from)+"__"+string(to), func(t *testing.T) {
					runID := seedRunWithEndpoint(t, st, domain.RunTesting)
					taskID := seedTask(t, st, runID, 1, domain.TaskRunning, "http://127.0.0.1:8000")
					attemptID := seedAttempt(t, st, taskID, from)
					a, err := applyInTx(t, st, domain.Transition{
						Entity: domain.EntityAttempt, ID: attemptID,
						From:    []domain.Status{from.Status()},
						To:      to.Status(),
						Command: spec.Command,
						Actor:   "test:kernel",
					})
					if err != nil {
						t.Fatalf("合法转移 %s->%s 报错: %v", from, to, err)
					}
					if got := currentAttemptStatus(t, st, attemptID); got != to {
						t.Fatalf("库内状态 = %s, 期望 %s", got, to)
					}
					_ = a
				})
			}
		}
	})
}

// TestKernel_IllegalEdgesRejected 穷举所有非法边，断言 ErrIllegalState。
func TestKernel_IllegalEdgesRejected(t *testing.T) {
	st := requireStore(t)
	var checked, rejected int
	for _, from := range allRunStatuses {
		for _, to := range allRunStatuses {
			if from == to {
				continue
			}
			if _, ok := domain.RunTransition(from, to); ok {
				continue
			}
			checked++
			runID := seedRun(t, st, from)
			_, err := applyInTx(t, st, domain.Transition{
				Entity: domain.EntityRun, ID: runID,
				From:    []domain.Status{from.Status()},
				To:      to.Status(),
				Payload: runPayload(to),
			})
			if errors.Is(err, domain.ErrIllegalState) {
				rejected++
			} else {
				t.Errorf("非法 Run 转移 %s->%s 应 ErrIllegalState，得到 %v", from, to, err)
			}
		}
	}
	if checked == 0 || rejected != checked {
		t.Fatalf("非法边检查数 %d，拒绝数 %d", checked, rejected)
	}
}

// TestKernel_Noop 目标状态等于当前状态时不写事件。
func TestKernel_Noop(t *testing.T) {
	st := requireStore(t)
	runID := seedRun(t, st, domain.RunQueued)
	before := eventCount(t, st, runID)

	a, err := applyInTx(t, st, domain.Transition{
		Entity: domain.EntityRun, ID: runID,
		To: domain.RunQueued.Status(),
	})
	if err != nil {
		t.Fatalf("Noop 不应报错: %v", err)
	}
	if !a.Noop {
		t.Fatal("期望 Noop=true")
	}
	if a.From != domain.RunQueued.Status() || a.To != domain.RunQueued.Status() {
		t.Fatalf("Applied = %s->%s", a.From, a.To)
	}
	if got := eventCount(t, st, runID); got != before {
		t.Fatalf("Noop 不应写事件: before=%d after=%d", before, got)
	}
}

// TestKernel_CASConflict 并发推进同一 Run：先落库者胜，后到者 ErrConflict。
// 两个命令都声明 From=[submitted]；无论谁先提交，另一个都会因前置状态已变而冲突。
func TestKernel_CASConflict(t *testing.T) {
	st := requireStore(t)
	runID := seedRun(t, st, domain.RunSubmitted)

	submit := func(to domain.RunStatus) error {
		return st.InTx(context.Background(), func(tx pgx.Tx) error {
			_, err := st.Apply(context.Background(), tx, domain.Transition{
				Entity: domain.EntityRun, ID: runID,
				From: []domain.Status{domain.RunSubmitted.Status()},
				To:   to.Status(),
			})
			return err
		})
	}

	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results <- submit(domain.RunQueued) }()
	go func() { defer wg.Done(); results <- submit(domain.RunCancelling) }()
	wg.Wait()
	close(results)

	var okCount, conflictCount int
	for err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, domain.ErrConflict):
			conflictCount++
		default:
			t.Fatalf("意外错误: %v", err)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("期望 1 成功 1 冲突，实际 %d/%d", okCount, conflictCount)
	}
	final := currentRunStatus(t, st, runID)
	if final != domain.RunQueued && final != domain.RunCancelling {
		t.Fatalf("最终状态非法: %s", final)
	}
}

// TestKernel_EventSeqMonotonic 事件 seq 必须逐次 +1 且唯一。
func TestKernel_EventSeqMonotonic(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	runID := seedRunWithEndpoint(t, st, domain.RunSubmitted)

	steps := []struct {
		to   domain.RunStatus
		from []domain.RunStatus
	}{
		{domain.RunQueued, []domain.RunStatus{domain.RunSubmitted}},
		{domain.RunTesting, []domain.RunStatus{domain.RunQueued}},
		{domain.RunCleanup, []domain.RunStatus{domain.RunTesting}},
		{domain.RunSucceeded, []domain.RunStatus{domain.RunCleanup}},
	}
	var prev int64
	for _, s := range steps {
		a, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityRun, ID: runID,
			From:    domain.RunStatuses(s.from...),
			To:      s.to.Status(),
			Payload: runPayload(s.to),
		})
		if err != nil {
			t.Fatalf("%s: %v", s.to, err)
		}
		if a.Seq != prev+1 {
			t.Fatalf("seq 不连续: 上一条 %d，本条 %d", prev, a.Seq)
		}
		prev = a.Seq
	}
	var maxSeq int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT max(seq) FROM state_events WHERE run_id=$1`, runID).Scan(&maxSeq); err != nil {
		t.Fatalf("读 max seq: %v", err)
	}
	if maxSeq != prev {
		t.Fatalf("state_events 最大 seq=%d, 期望 %d", maxSeq, prev)
	}
	var distinct int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(DISTINCT seq) FROM state_events WHERE run_id=$1`, runID).Scan(&distinct); err != nil {
		t.Fatalf("读 distinct: %v", err)
	}
	if int64(distinct) != prev {
		t.Fatalf("seq 不唯一: distinct=%d, max=%d", distinct, prev)
	}
}

// TestKernel_Guards 覆盖 endpoint_key / skip reason / endpoint 三个守卫。
func TestKernel_Guards(t *testing.T) {
	st := requireStore(t)

	t.Run("task queue 需要 endpoint_key", func(t *testing.T) {
		runID := seedRunWithEndpoint(t, st, domain.RunTesting)
		taskID := seedTask(t, st, runID, 1, domain.TaskPending, "")
		_, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From: []domain.Status{domain.TaskPending.Status()},
			To:   domain.TaskQueued.Status(),
		})
		if !errors.Is(err, domain.ErrPrecondition) {
			t.Fatalf("期望 ErrPrecondition，得到 %v", err)
		}
	})

	t.Run("task skipped 需要合法 reason", func(t *testing.T) {
		runID := seedRunWithEndpoint(t, st, domain.RunTesting)
		taskID := seedTask(t, st, runID, 1, domain.TaskPending, "http://127.0.0.1:8000")
		_, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityTask, ID: taskID,
			From:   []domain.Status{domain.TaskPending.Status()},
			To:     domain.TaskSkipped.Status(),
			Reason: "随便写",
		})
		if !errors.Is(err, domain.ErrPrecondition) {
			t.Fatalf("期望 ErrPrecondition，得到 %v", err)
		}
	})

	t.Run("run 进 testing 需要 endpoint", func(t *testing.T) {
		runID := seedRun(t, st, domain.RunQueued)
		_, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityRun, ID: runID,
			From: []domain.Status{domain.RunQueued.Status()},
			To:   domain.RunTesting.Status(),
		})
		if !errors.Is(err, domain.ErrPrecondition) {
			t.Fatalf("期望 ErrPrecondition，得到 %v", err)
		}
	})

	t.Run("cleanup 终态需要全部 Task 终态", func(t *testing.T) {
		runID := seedRun(t, st, domain.RunCleanup)
		seedTask(t, st, runID, 1, domain.TaskRunning, "http://127.0.0.1:8000")
		_, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityRun, ID: runID,
			From: []domain.Status{domain.RunCleanup.Status()},
			To:   domain.RunSucceeded.Status(),
		})
		if !errors.Is(err, domain.ErrPrecondition) {
			t.Fatalf("期望 ErrPrecondition，得到 %v", err)
		}
	})

	t.Run("不存在的实体", func(t *testing.T) {
		_, err := applyInTx(t, st, domain.Transition{
			Entity: domain.EntityRun, ID: "r-nope",
			To: domain.RunQueued.Status(),
		})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("期望 ErrNotFound，得到 %v", err)
		}
	})
}

// TestKernel_CancelClosesTasks 取消 Run 时同事务收口未开始 Task。
func TestKernel_CancelClosesTasks(t *testing.T) {
	st := requireStore(t)
	machines := NewMachines(st, nil)
	runID := seedRunWithEndpoint(t, st, domain.RunTesting)
	pending := seedTask(t, st, runID, 1, domain.TaskPending, "http://127.0.0.1:8000")
	queued := seedTask(t, st, runID, 2, domain.TaskQueued, "http://127.0.0.1:8000")
	running := seedTask(t, st, runID, 3, domain.TaskRunning, "http://127.0.0.1:8000")
	attemptID := seedAttempt(t, st, running, domain.AttemptRunning)
	if _, err := st.Pool().Exec(context.Background(),
		`INSERT INTO endpoint_leases (task_id, endpoint_key, exclusive) VALUES ($1,$2,false)`,
		running, "http://127.0.0.1:8000"); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	if _, err := machines.Runs.Cancel(context.Background(), runID, "user:dev"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := currentTaskStatus(t, st, pending); got != domain.TaskSkipped {
		t.Fatalf("pending task = %s, 期望 skipped", got)
	}
	if got := currentTaskStatus(t, st, queued); got != domain.TaskSkipped {
		t.Fatalf("queued task = %s, 期望 skipped", got)
	}
	if got := currentTaskStatus(t, st, running); got != domain.TaskRunning {
		t.Fatalf("running task 在 cancelling 阶段不应改状态，得到 %s", got)
	}
	var cancelRequested bool
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT cancel_requested FROM tasks WHERE id=$1`, running).Scan(&cancelRequested); err != nil {
		t.Fatalf("读 cancel_requested: %v", err)
	}
	if !cancelRequested {
		t.Fatal("进行中的 Task 应置 cancel_requested")
	}

	// cancelling → canceled：进行中的 Task/Attempt 强制收口。
	if _, err := machines.Runs.Canceled(context.Background(), runID); err != nil {
		t.Fatalf("canceled: %v", err)
	}
	if got := currentTaskStatus(t, st, running); got != domain.TaskCanceled {
		t.Fatalf("running task = %s, 期望 canceled", got)
	}
	if got := currentAttemptStatus(t, st, attemptID); got != domain.AttemptCanceled {
		t.Fatalf("attempt = %s, 期望 canceled", got)
	}
	var leases int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM endpoint_leases WHERE task_id=$1`, running).Scan(&leases); err != nil {
		t.Fatalf("读租约: %v", err)
	}
	if leases != 0 {
		t.Fatal("收口后租约必须释放")
	}
}

// TestCreateRun_SubmitAndEnqueue 覆盖提交事务（Run+Tasks+submitted 事件+audit）与 R1。
func TestCreateRun_SubmitAndEnqueue(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	machines := NewMachines(st, nil)

	runID := NewRunID(time.Now())
	tasks := []*Task{
		{
			ID: NewTaskID(), Seq: 0, Name: "precheck",
			TesterName: "endpoint-probe", TesterVersion: "1.0.0", ImageRef: "probe:1",
			Profile: "default", ArgsRaw: "", ArgsArgv: []byte("[]"),
			OnFailure: domain.OnFailureAbortRun, TimeoutSeconds: 600, Status: string(domain.TaskPending),
		},
		{
			ID: NewTaskID(), Seq: 1, Name: "accuracy",
			TesterName: "aisbench", TesterVersion: "1.0.0", ImageRef: "ais:1",
			Profile: "default", ArgsRaw: "--dataset=/data/a", ArgsArgv: []byte(`["--dataset=/data/a"]`),
			OnFailure: domain.OnFailureContinue, TimeoutSeconds: 3600, Status: string(domain.TaskPending),
		},
	}
	run, created, err := st.CreateRun(ctx, SubmitParams{
		Run: &Run{
			ID: runID, Name: "demo", Kind: "run",
			Manifest: []byte(`{"run":{"name":"demo"}}`), Resolved: []byte(`{"tasks":[]}`),
			CleanupPolicy: "always", SubmittedBy: "dev",
		},
		Tasks: tasks,
		Actor: "user:dev",
		Role:  "operator",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.ID != runID || len(created) != 2 {
		t.Fatalf("返回值异常: run=%v tasks=%d", run.ID, len(created))
	}
	if got := currentRunStatus(t, st, runID); got != domain.RunSubmitted {
		t.Fatalf("提交后状态 = %s", got)
	}
	if n := eventCount(t, st, runID); n != 1 {
		t.Fatalf("提交应写 1 条事件，得到 %d", n)
	}
	var evType string
	var seq int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT type, seq FROM state_events WHERE run_id=$1`, runID).Scan(&evType, &seq); err != nil {
		t.Fatalf("读事件: %v", err)
	}
	if evType != domain.EventRunSubmitted || seq != 1 {
		t.Fatalf("提交事件 = %s seq=%d", evType, seq)
	}
	var audit int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE object_id=$1`, runID).Scan(&audit); err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if audit != 1 {
		t.Fatalf("应写 1 条审计，得到 %d", audit)
	}

	if _, err := machines.Runs.Enqueue(ctx, runID, "system:orchestrator"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := currentRunStatus(t, st, runID); got != domain.RunQueued {
		t.Fatalf("enqueue 后状态 = %s", got)
	}
	if n := eventCount(t, st, runID); n != 2 {
		t.Fatalf("enqueue 后事件数 = %d, 期望 2", n)
	}
}

// TestLease_ExclusiveSemantics 覆盖端点互斥（独占 vs 共享）。
func TestLease_ExclusiveSemantics(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	runID := seedRunWithEndpoint(t, st, domain.RunTesting)
	t1 := seedTask(t, st, runID, 1, domain.TaskQueued, "http://h:8000")
	t2 := seedTask(t, st, runID, 2, domain.TaskQueued, "http://h:8000")

	try := func(taskID string, exclusive bool) bool {
		var ok bool
		if err := st.InTx(ctx, func(tx pgx.Tx) error {
			var err error
			ok, err = st.Leases.TryAcquire(ctx, tx, taskID, "http://h:8000", exclusive)
			return err
		}); err != nil {
			t.Fatalf("TryAcquire: %v", err)
		}
		return ok
	}

	if !try(t1, true) {
		t.Fatal("空端点应能拿到独占租约")
	}
	if try(t2, false) {
		t.Fatal("独占持有期间共享型必须被拒")
	}
	if try(t2, true) {
		t.Fatal("独占持有期间独占型也必须被拒")
	}
	if err := st.Leases.Delete(ctx, nil, t1); err != nil {
		t.Fatalf("释放租约: %v", err)
	}
	if !try(t2, false) {
		t.Fatal("独占释放后共享型应能拿到")
	}
	if !try(t1, false) {
		t.Fatal("共享型之间可以并存")
	}
}
