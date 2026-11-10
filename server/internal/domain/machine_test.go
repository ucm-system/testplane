package domain

import "testing"

// 这张表是 02-logical-view.md §2 / impl/control-plane §C-3 转移表的**独立抄写**：
// 实现与文档任何一侧漂移，这里就会失败。新增/删除转移必须同时改文档、machine.go 与本表。
var wantTaskEdges = []struct {
	from, to TaskStatus
	command  string
}{
	{TaskSubmitted, TaskQueued, "Enqueue"},
	{TaskQueued, TaskRunning, "BeginPipeline"},
	{TaskRunning, TaskSucceeded, "Finish"},
	{TaskRunning, TaskFailed, "Finish"},
	{TaskRunning, TaskServiceUnavailable, "Finish"},
	{TaskSubmitted, TaskCancelling, "Cancel"},
	{TaskQueued, TaskCancelling, "Cancel"},
	{TaskRunning, TaskCancelling, "Cancel"},
	{TaskCancelling, TaskCanceled, "Canceled"},
	// 平台侧不可恢复错误（R10 语义保留：Executor 不可用 / 内部异常 / DEPLOY_FAILED）。
	// running→failed 同时是 Finish 与 Fail 的边，只登记一次，命令取 Fail。
	{TaskSubmitted, TaskFailed, "Fail"},
	{TaskQueued, TaskFailed, "Fail"},
	// 整 Task 重试（§7.8 / 03 §4.5）：终态 → queued（同一 Task attempt_no+1，D-68）。
	{TaskSucceeded, TaskQueued, "Retry"},
	{TaskFailed, TaskQueued, "Retry"},
	{TaskServiceUnavailable, TaskQueued, "Retry"},
}

var wantRunnerEdges = []struct {
	from, to RunnerStatus
	command  string
}{
	{RunnerPending, RunnerQueued, "Queue"},
	{RunnerPending, RunnerSkipped, "Skip"},
	{RunnerQueued, RunnerPreparing, "Prepare"},
	{RunnerQueued, RunnerSkipped, "Skip"},
	{RunnerPreparing, RunnerRunning, "Start"},
	{RunnerPreparing, RunnerFailed, "Fail"},
	{RunnerPreparing, RunnerCanceled, "Cancel"},
	{RunnerRunning, RunnerSucceeded, "Succeed"},
	{RunnerRunning, RunnerFailed, "Fail"},
	{RunnerRunning, RunnerCanceled, "Cancel"},
}

func TestTransitionTable_TaskMatchesDesign(t *testing.T) {
	if got, want := CountTransitions(EntityTask), len(wantTaskEdges); got != want {
		t.Fatalf("Task 转移表边数 = %d, 设计文档 = %d", got, want)
	}
	for _, e := range wantTaskEdges {
		t.Run(string(e.from)+"->"+string(e.to), func(t *testing.T) {
			spec, ok := TaskTransition(e.from, e.to)
			if !ok {
				t.Fatalf("缺少合法转移 %s -> %s", e.from, e.to)
			}
			if spec.Command != e.command {
				t.Fatalf("命令 = %q, 期望 %q", spec.Command, e.command)
			}
			if spec.Event == "" {
				t.Fatalf("转移 %s -> %s 缺事件类型", e.from, e.to)
			}
		})
	}
}

func TestTransitionTable_RunnerMatchesDesign(t *testing.T) {
	if got, want := CountTransitions(EntityRunnerRun), len(wantRunnerEdges); got != want {
		t.Fatalf("runner 转移表边数 = %d, 设计文档 = %d", got, want)
	}
	for _, e := range wantRunnerEdges {
		t.Run(string(e.from)+"->"+string(e.to), func(t *testing.T) {
			spec, ok := RunnerTransition(e.from, e.to)
			if !ok {
				t.Fatalf("缺少合法转移 %s -> %s", e.from, e.to)
			}
			if spec.Command != e.command {
				t.Fatalf("命令 = %q, 期望 %q", spec.Command, e.command)
			}
		})
	}
}

// 非法边：终态不可逆、跳步、回退、重试回路。每类各取代表。
func TestTransitionTable_IllegalEdgesRejected(t *testing.T) {
	illegalTasks := []struct{ from, to TaskStatus }{
		{TaskSubmitted, TaskRunning}, // 跳步（无部署/测试平台阶段，D-70）
		{TaskCancelling, TaskQueued}, // 取消不可回退
		{TaskCancelling, TaskFailed}, // 取消只能落 canceled（clean finally）
		{TaskSucceeded, TaskFailed},  // 终态不可逆
		{TaskSucceeded, TaskRunning}, // 重试也要先进 queued
		{TaskCanceled, TaskQueued},   // 取消是终局（canceled 不可重试，§7.6）
	}
	for _, e := range illegalTasks {
		if _, ok := TaskTransition(e.from, e.to); ok {
			t.Errorf("非法转移被接受: %s -> %s", e.from, e.to)
		}
	}

	illegalRunners := []struct{ from, to RunnerStatus }{
		{RunnerPending, RunnerRunning},   // 跳步
		{RunnerQueued, RunnerCanceled},   // C-3.1：未开始的取消记 skipped(canceled)
		{RunnerPending, RunnerCanceled},  // 同上
		{RunnerFailed, RunnerQueued},     // **重试回路不存在**（D-69）
		{RunnerSkipped, RunnerQueued},    // skipped 是终态（重试 = 整 Task 新 attempt）
		{RunnerSucceeded, RunnerFailed},  // 终态不可逆
		{RunnerRunning, RunnerPreparing}, // 回退
	}
	for _, e := range illegalRunners {
		if _, ok := RunnerTransition(e.from, e.to); ok {
			t.Errorf("非法转移被接受: %s -> %s", e.from, e.to)
		}
	}
}

// 穷举：终态的出边只允许**唯一的**整 Task 重试回边（终态→queued，§7.8/03 §4.5），
// 其余任何出边都说明终态可逆。
func TestTransitionTable_TerminalStatesAreAbsorbing(t *testing.T) {
	taskStates := []TaskStatus{TaskSubmitted, TaskQueued, TaskRunning, TaskCancelling,
		TaskSucceeded, TaskFailed, TaskServiceUnavailable, TaskCanceled}
	for _, from := range taskStates {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range taskStates {
			if _, ok := TaskTransition(from, to); ok {
				retryEdge := to == TaskQueued && from != TaskCanceled
				if !retryEdge {
					t.Errorf("Task 终态 %s 存在非重试出边 -> %s", from, to)
				}
			}
		}
	}

	runnerStates := []RunnerStatus{RunnerPending, RunnerQueued, RunnerPreparing, RunnerRunning,
		RunnerSucceeded, RunnerFailed, RunnerSkipped, RunnerCanceled}
	for _, from := range runnerStates {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range runnerStates {
			if _, ok := RunnerTransition(from, to); ok {
				t.Errorf("runner 终态 %s 存在出边 -> %s（v2 无重试回路，D-69）", from, to)
			}
		}
	}
}

func TestTransitionTable_UnknownEntity(t *testing.T) {
	if _, ok := LookupTransition("bogus", Status("a"), Status("b")); ok {
		t.Fatal("未知实体不应有转移")
	}
}

func TestEffectsForTransition(t *testing.T) {
	has := func(entity EntityType, from, to Status, want EffectKind) bool {
		for _, e := range Effects(entity, from, to) {
			if e == want {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name         string
		entity       EntityType
		from, to     Status
		want         EffectKind
		wantPresence bool
	}{
		{"runner 终态释放租约", EntityRunnerRun, RunnerRunning.Status(), RunnerSucceeded.Status(), EffectReleaseLeases, true},
		{"runner 失败释放租约", EntityRunnerRun, RunnerRunning.Status(), RunnerFailed.Status(), EffectReleaseLeases, true},
		{"runner 非终态无副作用", EntityRunnerRun, RunnerQueued.Status(), RunnerPreparing.Status(), EffectReleaseLeases, false},
		{"task 取消收口", EntityTask, TaskRunning.Status(), TaskCancelling.Status(), EffectSkipUnfinished, true},
		{"task 取消标记", EntityTask, TaskRunning.Status(), TaskCancelling.Status(), EffectMarkCancelRequested, true},
		{"task 平台失败收口", EntityTask, TaskRunning.Status(), TaskFailed.Status(), EffectCloseActive, true},
		{"task 终态写报告意图", EntityTask, TaskRunning.Status(), TaskSucceeded.Status(), EffectWriteReportIntent, true},
		{"task 入队无副作用", EntityTask, TaskSubmitted.Status(), TaskQueued.Status(), EffectReleaseLeases, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := has(c.entity, c.from, c.to, c.want); got != c.wantPresence {
				t.Fatalf("Effects(%s,%s->%s) 含 %s = %v, 期望 %v；实际 %v",
					c.entity, c.from, c.to, c.want, got, c.wantPresence,
					Effects(c.entity, c.from, c.to))
			}
		})
	}
}

func TestValidSkipReason(t *testing.T) {
	for _, ok := range []string{SkipUpstreamAbort, SkipCanceled} {
		if !ValidSkipReason(ok) {
			t.Errorf("LegalReason %q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "whatever", "deploy_failed", "platform_error"} {
		if ValidSkipReason(bad) {
			t.Errorf("非法 skip reason %q 被接受", bad)
		}
	}
}

func TestValidWaitingReason(t *testing.T) {
	for _, ok := range []string{WaitSlotWaiting, WaitGlobalLimit, WaitQueueFull, WaitInsufficientResources, WaitGangPending} {
		if !ValidWaitingReason(ok) {
			t.Errorf("waiting reason %q 应合法（C-4.7）", ok)
		}
	}
	for _, bad := range []string{"", "precheck_gate", "unknown"} {
		if ValidWaitingReason(bad) {
			t.Errorf("非法 waiting reason %q 被接受（v2 取消 precheck_gate，D-69）", bad)
		}
	}
}

func TestRunnerKeyRoundTrip(t *testing.T) {
	k := RunnerKey{TaskID: "t-20261110-0001", AttemptNo: 2, Seq: 3}
	if !k.Valid() {
		t.Fatal("key 应有效")
	}
	got, ok := ParseRunnerKey(k.ID())
	if !ok {
		t.Fatalf("ParseRunnerKey(%q) 失败", k.ID())
	}
	if got != k {
		t.Fatalf("round trip = %+v, 期望 %+v", got, k)
	}
	if _, ok := ParseRunnerKey("garbage"); ok {
		t.Fatal("垃圾串不应解析成功")
	}
}
