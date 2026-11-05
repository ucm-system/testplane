package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ucm-system/testplane/server/internal/domain"
)

// TestListByRun_EmptyCursorIncludesSeqZero 是 QA 报的回归：
// 空游标首屏必须包含 seq=0 的平台注入 Pre-Check。
func TestListByRun_EmptyCursorIncludesSeqZero(t *testing.T) {
	st := requireStore(t)
	runID := seedRunWithEndpoint(t, st, domain.RunTesting)
	pre := seedTask(t, st, runID, 0, domain.TaskPending, "http://127.0.0.1:8000")
	second := seedTask(t, st, runID, 1, domain.TaskPending, "http://127.0.0.1:8000")

	got, next, err := st.Tasks.ListByRun(context.Background(), nil, runID, "", 50)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("首屏应返回 2 条，得到 %d", len(got))
	}
	if got[0].ID != pre || got[0].Seq != 0 {
		t.Fatalf("首屏第一条应是 seq=0 的 precheck，得到 id=%s seq=%d", got[0].ID, got[0].Seq)
	}
	if got[1].ID != second {
		t.Fatalf("第二条应是 seq=1，得到 %s", got[1].ID)
	}
	if next != "" {
		t.Fatalf("不足一页不应有 next cursor，得到 %q", next)
	}

	// 用返回的游标翻页：seq=0 之后应只剩 seq=1。
	page, _, err := st.Tasks.ListByRun(context.Background(), nil, runID, EncodeSeqCursor(0), 50)
	if err != nil {
		t.Fatalf("ListByRun page2: %v", err)
	}
	if len(page) != 1 || page[0].ID != second {
		t.Fatalf("翻页结果异常: %+v", page)
	}
}

// TestDeployReady_BackfillsEndpointAndArgv 验证 C-5.3：deploy ready 的同一事务里
// 回填全部 Task 的 endpoint_key 与注入后的 injected_argv（Lead 裁决 Q1）。
func TestDeployReady_BackfillsEndpointAndArgv(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	machines := NewMachines(st, nil)

	runID := seedRun(t, st, domain.RunDeploying)
	// 两个 deploy 模式的 Task：同一次 ready 都要被回填。
	t1 := seedTask(t, st, runID, 0, domain.TaskPending, "")
	t2 := seedTask(t, st, runID, 1, domain.TaskPending, "")

	ep := domain.Endpoint{
		Key:       "http://10.0.0.10:8000",
		BaseURL:   "http://10.0.0.10:8000/v1",
		ModelName: "Qwen3-32B",
		ModelPath: "/models/Qwen3-32B",
	}
	if _, err := machines.Runs.DeployReady(ctx, runID, ep); err != nil {
		t.Fatalf("DeployReady: %v", err)
	}

	for _, taskID := range []string{t1, t2} {
		var key *string
		var injected []byte
		var raw []byte
		if err := st.Pool().QueryRow(ctx,
			`SELECT endpoint_key, injected_argv, args_argv FROM tasks WHERE id=$1`, taskID).
			Scan(&key, &injected, &raw); err != nil {
			t.Fatalf("读 task %s: %v", taskID, err)
		}
		if key == nil || *key != ep.Key {
			t.Fatalf("task %s endpoint_key = %v, 期望 %s", taskID, key, ep.Key)
		}
		var argv []string
		if err := json.Unmarshal(injected, &argv); err != nil {
			t.Fatalf("解析 injected_argv: %v", err)
		}
		if len(argv) != 3 {
			t.Fatalf("task %s injected_argv = %v, 期望 3 个公共参数", taskID, argv)
		}
		want := []string{
			"--url=http://10.0.0.10:8000/v1",
			"--model_name=Qwen3-32B",
			"--model_path=/models/Qwen3-32B",
		}
		for i := range want {
			if argv[i] != want[i] {
				t.Fatalf("task %s argv[%d] = %q, 期望 %q", taskID, i, argv[i], want[i])
			}
		}
	}

	// Run 本身要进 testing，且 endpoint 落库。
	var status domain.RunStatus
	var endpoint []byte
	if err := st.Pool().QueryRow(ctx,
		`SELECT status, endpoint FROM runs WHERE id=$1`, runID).Scan(&status, &endpoint); err != nil {
		t.Fatalf("读 run: %v", err)
	}
	if status != domain.RunTesting {
		t.Fatalf("run status = %s, 期望 testing", status)
	}
	var epStored domain.Endpoint
	if err := json.Unmarshal(endpoint, &epStored); err != nil {
		t.Fatalf("解析 runs.endpoint: %v", err)
	}
	if epStored.Key != ep.Key {
		t.Fatalf("runs.endpoint.key = %q", epStored.Key)
	}
}

// TestClaimForExecution_RespectsGlobalLimit 覆盖全局 permit 与槽位原因。
func TestClaimForExecution_RespectsGlobalLimit(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	runID := seedRunWithEndpoint(t, st, domain.RunTesting)
	// 端点 key 每次测试唯一：测试库会被复用，固定 key 会撞上一次残留的租约。
	epKey := "http://" + uniqueEndpointToken() + ":8000"
	epKey2 := "http://" + uniqueEndpointToken() + ":8001"
	taskID := seedTask(t, st, runID, 1, domain.TaskQueued, epKey)

	// limit=0 表示不限制 → 抢到。
	res, claimed, reason, err := st.ClaimForExecution(ctx, ClaimRequest{
		TaskID: taskID, EndpointKey: epKey, GlobalLimit: 0,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claimed || res.AttemptID == "" {
		t.Fatalf("应抢到，claimed=%v reason=%q", claimed, reason)
	}
	if res.JobName != "tp-"+taskID+"-a1" {
		t.Fatalf("JobName = %q", res.JobName)
	}

	// 第二个 Task：全局上限=1（已有 1 个活跃 Attempt）→ global_limit。
	task2 := seedTask(t, st, runID, 2, domain.TaskQueued, epKey2)
	_, claimed, reason, err = st.ClaimForExecution(ctx, ClaimRequest{
		TaskID: task2, EndpointKey: epKey2, GlobalLimit: 1,
	})
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if claimed || reason != domain.WaitGlobalLimit {
		t.Fatalf("期望 global_limit，claimed=%v reason=%q", claimed, reason)
	}

	// 端点被独占时，共享型被拒（slot_waiting）。
	exKey := "http://" + uniqueEndpointToken() + ":8002"
	holder := seedTask(t, st, runID, 3, domain.TaskQueued, exKey)
	var leased bool
	if err := st.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		leased, err = st.Leases.TryAcquire(ctx, tx, holder, exKey, true)
		return err
	}); err != nil || !leased {
		t.Fatalf("独占租约: leased=%v err=%v", leased, err)
	}
	task3 := seedTask(t, st, runID, 4, domain.TaskQueued, exKey)
	_, claimed, reason, err = st.ClaimForExecution(ctx, ClaimRequest{
		TaskID: task3, EndpointKey: exKey, Exclusive: false, GlobalLimit: 0,
	})
	if err != nil {
		t.Fatalf("claim3: %v", err)
	}
	if claimed || reason != domain.WaitSlotWaiting {
		t.Fatalf("期望 slot_waiting，claimed=%v reason=%q", claimed, reason)
	}
}

// uniqueEndpointToken 生成测试内唯一的短标识（避免跨测试运行的租约/键冲突）。
func uniqueEndpointToken() string { return NewTaskID() }