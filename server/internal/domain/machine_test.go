package domain

import "testing"

// 这张表是 02-logical-view.md §2 转移表的**独立抄写**：实现与文档任何一侧漂移，
// 这里就会失败。新增/删除转移必须同时改文档、machine.go 与本表。
var wantRunEdges = []struct {
	from, to RunStatus
	command  string
}{
	{RunSubmitted, RunQueued, "Enqueue"},
	{RunQueued, RunDeploying, "BeginDeploy"},
	{RunQueued, RunTesting, "BeginTesting"},
	{RunDeploying, RunTesting, "DeployReady"},
	{RunDeploying, RunServiceUnavailable, "DeployBlocked"},
	{RunTesting, RunCleanup, "BeginCleanup"},
	{RunCleanup, RunSucceeded, "Finish"},
	{RunCleanup, RunCompletedWithErrors, "Finish"},
	{RunCleanup, RunServiceUnavailable, "Finish"},
	// cleanup→failed 同时是 R7 与 R10 的边，只登记一次（命令取 R10 的 Fail），见下表 R10。
	{RunSubmitted, RunCancelling, "Cancel"},
	{RunQueued, RunCancelling, "Cancel"},
	{RunDeploying, RunCancelling, "Cancel"},
	{RunTesting, RunCancelling, "Cancel"},
	{RunCleanup, RunCancelling, "Cancel"},
	{RunCancelling, RunCanceled, "Canceled"},
	{RunSubmitted, RunFailed, "Fail"},
	{RunQueued, RunFailed, "Fail"},
	{RunDeploying, RunFailed, "Fail"},
	{RunTesting, RunFailed, "Fail"},
	{RunCleanup, RunFailed, "Fail"},
	{RunCancelling, RunFailed, "Fail"},
}

var wantTaskEdges = []struct {
	from, to TaskStatus
	command  string
}{
	{TaskPending, TaskQueued, "Queue"},
	{TaskPending, TaskSkipped, "Skip"},
	{TaskQueued, TaskPreparing, "Prepare"},
	{TaskQueued, TaskSkipped, "Skip"},
	{TaskPreparing, TaskRunning, "Start"},
	{TaskPreparing, TaskFailed, "Fail"},
	{TaskPreparing, TaskCanceled, "Cancel"},
	{TaskRunning, TaskSucceeded, "Succeed"},
	{TaskRunning, TaskFailed, "Fail"},
	{TaskRunning, TaskCanceled, "Cancel"},
	{TaskFailed, TaskQueued, "Retry"},
}

var wantAttemptEdges = []struct {
	from, to AttemptStatus
}{
	{AttemptPreparing, AttemptRunning},
	{AttemptPreparing, AttemptFailed},
	{AttemptPreparing, AttemptCanceled},
	{AttemptRunning, AttemptSucceeded},
	{AttemptRunning, AttemptFailed},
	{AttemptRunning, AttemptCanceled},
}

func TestTransitionTable_RunMatchesDesign(t *testing.T) {
	if got, want := CountTransitions(EntityRun), len(wantRunEdges); got != want {
		t.Fatalf("Run 转移表边数 = %d, 设计文档 = %d", got, want)
	}
	for _, e := range wantRunEdges {
		t.Run(string(e.from)+"->"+string(e.to), func(t *testing.T) {
			spec, ok := RunTransition(e.from, e.to)
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
		})
	}
}

func TestTransitionTable_AttemptMatchesDesign(t *testing.T) {
	if got, want := CountTransitions(EntityAttempt), len(wantAttemptEdges); got != want {
		t.Fatalf("Attempt 转移表边数 = %d, 设计文档 = %d", got, want)
	}
	for _, e := range wantAttemptEdges {
		if _, ok := AttemptTransition(e.from, e.to); !ok {
			t.Fatalf("缺少合法转移 %s -> %s", e.from, e.to)
		}
	}
}

// 非法边：终态不可逆、跳步、回退。每类各取代表，且对全表做一次穷举。
func TestTransitionTable_IllegalEdgesRejected(t *testing.T) {
	illegalRuns := []struct{ from, to RunStatus }{
		{RunSucceeded, RunQueued},
		{RunFailed, RunCancelling},
		{RunCanceled, RunTesting},
		{RunSubmitted, RunSucceeded},   // 跳过 cleanup
		{RunQueued, RunCleanup},        // 跳过 testing
		{RunTesting, RunSucceeded},     // 终态只能由 cleanup 判定
		{RunCancelling, RunQueued},     // 取消不可回退
		{RunCleanup, RunTesting},       // cleanup 不能回 testing
	}
	for _, e := range illegalRuns {
		if _, ok := RunTransition(e.from, e.to); ok {
			t.Errorf("非法转移被接受: %s -> %s", e.from, e.to)
		}
	}

	illegalTasks := []struct{ from, to TaskStatus }{
		{TaskPending, TaskRunning},     // 跳步
		{TaskQueued, TaskCanceled},     // C-3.1：未开始的取消记 skipped
		{TaskPending, TaskCanceled},    // 同上
		{TaskSucceeded, TaskFailed},    // 终态不可逆
		{TaskSucceeded, TaskQueued},    // 终态不可逆
		{TaskCanceled, TaskQueued},     // 终态不可逆
		{TaskSkipped, TaskQueued},      // 终态不可逆
		{TaskRunning, TaskPreparing},   // 回退
		{TaskPreparing, TaskQueued},    // 回退
	}
	for _, e := range illegalTasks {
		if _, ok := TaskTransition(e.from, e.to); ok {
			t.Errorf("非法转移被接受: %s -> %s", e.from, e.to)
		}
	}

	illegalAttempts := []struct{ from, to AttemptStatus }{
		{AttemptSucceeded, AttemptRunning},
		{AttemptFailed, AttemptSucceeded},
		{AttemptCanceled, AttemptFailed},
		{AttemptPreparing, AttemptSucceeded}, // 必须先 running
	}
	for _, e := range illegalAttempts {
		if _, ok := AttemptTransition(e.from, e.to); ok {
			t.Errorf("非法转移被接受: %s -> %s", e.from, e.to)
		}
	}
}

// 穷举：任何 from 是终态、to 是任何状态，若表里存在边则说明终态可逆。
func TestTransitionTable_TerminalStatesAreAbsorbing(t *testing.T) {
	runStates := []RunStatus{RunSubmitted, RunQueued, RunDeploying, RunTesting, RunCleanup,
		RunCancelling, RunSucceeded, RunCompletedWithErrors, RunServiceUnavailable, RunFailed, RunCanceled}
	for _, from := range runStates {
		if !from.IsTerminal() {
			continue
		}
		for _, to := range runStates {
			if _, ok := RunTransition(from, to); ok {
				t.Errorf("Run 终态 %s 存在出边 -> %s", from, to)
			}
		}
	}

	taskStates := []TaskStatus{TaskPending, TaskQueued, TaskPreparing, TaskRunning,
		TaskSucceeded, TaskFailed, TaskCanceled, TaskSkipped}
	for _, from := range taskStates {
		// failed 不是终态（可 retry），其余三个是终态。
		if from == TaskFailed || !from.IsTerminal() {
			continue
		}
		for _, to := range taskStates {
			if _, ok := TaskTransition(from, to); ok {
				t.Errorf("Task 终态 %s 存在出边 -> %s", from, to)
			}
		}
	}

	for _, from := range []AttemptStatus{AttemptSucceeded, AttemptFailed, AttemptCanceled} {
		for _, to := range []AttemptStatus{AttemptPreparing, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCanceled} {
			if _, ok := AttemptTransition(from, to); ok {
				t.Errorf("Attempt 终态 %s 存在出边 -> %s", from, to)
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
		{"task 终态释放租约", EntityTask, TaskRunning.Status(), TaskSucceeded.Status(), EffectReleaseLeases, true},
		{"task 成功不写事件之外的东西", EntityTask, TaskRunning.Status(), TaskSucceeded.Status(), EffectWriteReportIntent, false},
		{"task 失败释放租约", EntityTask, TaskRunning.Status(), TaskFailed.Status(), EffectReleaseLeases, true},
		{"task 重试排 backoff", EntityTask, TaskFailed.Status(), TaskQueued.Status(), EffectScheduleRetry, true},
		{"task 入队无副作用", EntityTask, TaskPending.Status(), TaskQueued.Status(), EffectReleaseLeases, false},
		{"run 取消收口", EntityRun, RunTesting.Status(), RunCancelling.Status(), EffectSkipUnfinished, true},
		{"run 取消标记", EntityRun, RunTesting.Status(), RunCancelling.Status(), EffectMarkCancelRequested, true},
		{"run 失败收口", EntityRun, RunTesting.Status(), RunFailed.Status(), EffectSkipUnfinished, true},
		{"run ready 回填", EntityRun, RunDeploying.Status(), RunTesting.Status(), EffectBackfillEndpoint, true},
		{"attempt 终态释放租约", EntityAttempt, AttemptRunning.Status(), AttemptFailed.Status(), EffectReleaseLeases, true},
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
	for _, ok := range []string{SkipUpstreamAbort, SkipCanceled, SkipDeployFailed, SkipServiceUnavailable, SkipPlatformError} {
		if !ValidSkipReason(ok) {
			t.Errorf("LegalReason %q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "whatever", "upstream_abortt"} {
		if ValidSkipReason(bad) {
			t.Errorf("非法 skip reason %q 被接受", bad)
		}
	}
}
