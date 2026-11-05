package domain

import "fmt"

// 本文件是 02-logical-view.md §2 的机器可执行版本：四套状态机的转移表。
//
// 铁律（02 §2、§3.6）：
//   - 转移表是唯一合法边集合；表里没有的边 = ErrIllegalState（编程错误，不重试）；
//   - 有边但前置状态不满足 = ErrConflict（并发/竞态，先落库者胜）；
//   - 终态不可逆：任何 From 集合都不含终态。
//
// 新增状态或转移必须同时改三处：本文件、design/views/02-logical-view.md §2、
// server/migrations 的 CHECK 约束。

// edge 是转移表的主键：一条 from→to 的边。
type edge struct {
	from Status
	to   Status
}

// TransitionSpec 描述一条合法转移的静态属性。
type TransitionSpec struct {
	// Command 是设计文档里的命令名（enqueue / deploy_ready / succeed ...），
	// 只用于日志与事件，不参与判定。
	Command string
	// Event 是写进 state_events.type 的事件类型（07 §7、C-9.1）。
	Event string
}

// 事件类型常量（07 §7）。
const (
	EventRunSubmitted        = "run.submitted"
	EventRunStateChanged     = "run.state_changed"
	EventRunFinished         = "run.finished"
	EventTaskStateChanged    = "task.state_changed"
	EventAttemptStarted      = "attempt.started"
	EventAttemptFinished     = "attempt.finished"
	EventDeployStateChanged  = "deploy.state_changed"
	EventCleanupStarted      = "cleanup.started"
	EventReportWritten       = "report.written"
	EventResyncRequired      = "resync_required"
	EventTesterStateChanged  = "tester.state_changed"
	EventReportPendingIntent = "report.pending"
)

// ---- Run 转移表（R1–R10，02 §2.1）-----------------------------------------

var runTransitions = map[edge]TransitionSpec{
	// R1 提交成功进入编排
	{RunSubmitted.Status(), RunQueued.Status()}: {Command: "Enqueue", Event: EventRunStateChanged},
	// R2 有 deploy，开始部署
	{RunQueued.Status(), RunDeploying.Status()}: {Command: "BeginDeploy", Event: EventDeployStateChanged},
	// R3 无 deploy / service 模式，直接进入 testing
	{RunQueued.Status(), RunTesting.Status()}: {Command: "BeginTesting", Event: EventRunStateChanged},
	// R4 部署就绪，回填 Endpoint
	{RunDeploying.Status(), RunTesting.Status()}: {Command: "DeployReady", Event: EventDeployStateChanged},
	// R5 部署 blocked / 超窗
	{RunDeploying.Status(), RunServiceUnavailable.Status()}: {Command: "DeployBlocked", Event: EventDeployStateChanged},
	// R6 全部 Task 终态
	{RunTesting.Status(), RunCleanup.Status()}: {Command: "BeginCleanup", Event: EventCleanupStarted},
	// R7 终态判定
	{RunCleanup.Status(), RunSucceeded.Status()}:           {Command: "Finish", Event: EventRunFinished},
	{RunCleanup.Status(), RunCompletedWithErrors.Status()}: {Command: "Finish", Event: EventRunFinished},
	{RunCleanup.Status(), RunServiceUnavailable.Status()}:  {Command: "Finish", Event: EventRunFinished},
	// cleanup → failed 同时属于 R7（平台错误收口）与 R10，只登记一次，命令取 R10 的 Fail。
	// R8 任意非终态 → cancelling
	{RunSubmitted.Status(), RunCancelling.Status()}: {Command: "Cancel", Event: EventRunStateChanged},
	{RunQueued.Status(), RunCancelling.Status()}:    {Command: "Cancel", Event: EventRunStateChanged},
	{RunDeploying.Status(), RunCancelling.Status()}: {Command: "Cancel", Event: EventRunStateChanged},
	{RunTesting.Status(), RunCancelling.Status()}:   {Command: "Cancel", Event: EventRunStateChanged},
	{RunCleanup.Status(), RunCancelling.Status()}:   {Command: "Cancel", Event: EventRunStateChanged},
	// R9 取消完成
	{RunCancelling.Status(), RunCanceled.Status()}: {Command: "Canceled", Event: EventRunFinished},
	// R10 任意非终态 → failed（平台侧不可恢复错误）
	{RunSubmitted.Status(), RunFailed.Status()}:  {Command: "Fail", Event: EventRunFinished},
	{RunQueued.Status(), RunFailed.Status()}:     {Command: "Fail", Event: EventRunFinished},
	{RunDeploying.Status(), RunFailed.Status()}:  {Command: "Fail", Event: EventRunFinished},
	{RunTesting.Status(), RunFailed.Status()}:    {Command: "Fail", Event: EventRunFinished},
	{RunCleanup.Status(), RunFailed.Status()}:    {Command: "Fail", Event: EventRunFinished},
	{RunCancelling.Status(), RunFailed.Status()}: {Command: "Fail", Event: EventRunFinished},
}

// TerminalRunStatuses 是 Run 的**终态**集合——唯一真源。
//
// 用途（禁止再各写一遍）：
//   - Go 侧判定：`TerminalRunStatus(st)`；
//   - SQL 侧判定：`status = ANY($n)`，把本切片的字符串形态传进去（`TerminalRunStatusStrings()`）。
//
// 依赖它的地方：retry 前置（run 必须终态才可重试）、"是否已有进行中的 retry"判定。
var TerminalRunStatuses = []RunStatus{
	RunSucceeded,
	RunCompletedWithErrors,
	RunFailed,
	RunCanceled,
	RunServiceUnavailable,
}

// TerminalRunStatus 报告 Run 状态是否终态（与 TerminalRunStatuses 同源）。
func TerminalRunStatus(st RunStatus) bool {
	for _, t := range TerminalRunStatuses {
		if st == t {
			return true
		}
	}
	return false
}

// TerminalRunStatusStrings 返回终态集合的字符串形态，供 SQL 的 `status = ANY($n)` 使用。
func TerminalRunStatusStrings() []string {
	out := make([]string, 0, len(TerminalRunStatuses))
	for _, t := range TerminalRunStatuses {
		out = append(out, string(t))
	}
	return out
}

// ---- Task 转移表（T1–T11，02 §2.2）---------------------------------------

var taskTransitions = map[edge]TransitionSpec{
	// T1 Pre-Check 通过 / precheck off → 入队
	{TaskPending.Status(), TaskQueued.Status()}: {Command: "Queue", Event: EventTaskStateChanged},
	// T2 pending 直接跳过
	{TaskPending.Status(), TaskSkipped.Status()}: {Command: "Skip", Event: EventTaskStateChanged},
	// T3 拿到槽位 + permit
	{TaskQueued.Status(), TaskPreparing.Status()}: {Command: "Prepare", Event: EventTaskStateChanged},
	// T4 queued 跳过
	{TaskQueued.Status(), TaskSkipped.Status()}: {Command: "Skip", Event: EventTaskStateChanged},
	// T5 容器已启动
	{TaskPreparing.Status(), TaskRunning.Status()}: {Command: "Start", Event: EventTaskStateChanged},
	// T6 Job / 镜像 / 挂载错误
	{TaskPreparing.Status(), TaskFailed.Status()}: {Command: "Fail", Event: EventTaskStateChanged},
	// T7 准备期取消
	{TaskPreparing.Status(), TaskCanceled.Status()}: {Command: "Cancel", Event: EventTaskStateChanged},
	// T8 exit=0 且 result 合法
	{TaskRunning.Status(), TaskSucceeded.Status()}: {Command: "Succeed", Event: EventTaskStateChanged},
	// T9 非零退出 / 超时 / 结果非法
	{TaskRunning.Status(), TaskFailed.Status()}: {Command: "Fail", Event: EventTaskStateChanged},
	// T10 运行期取消
	{TaskRunning.Status(), TaskCanceled.Status()}: {Command: "Cancel", Event: EventTaskStateChanged},
	// T11 重试
	{TaskFailed.Status(), TaskQueued.Status()}: {Command: "Retry", Event: EventTaskStateChanged},
}

// ---- Attempt 转移表（02 §2.3）--------------------------------------------

var attemptTransitions = map[edge]TransitionSpec{
	{AttemptPreparing.Status(), AttemptRunning.Status()}:  {Command: "Start", Event: EventAttemptStarted},
	{AttemptPreparing.Status(), AttemptFailed.Status()}:   {Command: "Fail", Event: EventAttemptFinished},
	{AttemptPreparing.Status(), AttemptCanceled.Status()}: {Command: "Cancel", Event: EventAttemptFinished},
	{AttemptRunning.Status(), AttemptSucceeded.Status()}:  {Command: "Succeed", Event: EventAttemptFinished},
	{AttemptRunning.Status(), AttemptFailed.Status()}:     {Command: "Fail", Event: EventAttemptFinished},
	{AttemptRunning.Status(), AttemptCanceled.Status()}:   {Command: "Cancel", Event: EventAttemptFinished},
}

// ---- Tester 转移表（02 §2.4）--------------------------------------------

var testerTransitions = map[edge]TransitionSpec{
	{TesterScanned.Status(), TesterValid.Status()}:       {Command: "Validate", Event: EventTesterStateChanged},
	{TesterScanned.Status(), TesterRejected.Status()}:    {Command: "Reject", Event: EventTesterStateChanged},
	{TesterValid.Status(), TesterRegistered.Status()}:    {Command: "Register", Event: EventTesterStateChanged},
	{TesterRegistered.Status(), TesterOrphaned.Status()}: {Command: "Orphan", Event: EventTesterStateChanged},
	{TesterRegistered.Status(), TesterRejected.Status()}: {Command: "Reject", Event: EventTesterStateChanged},
	{TesterOrphaned.Status(), TesterRegistered.Status()}: {Command: "Register", Event: EventTesterStateChanged},
}

// ---- 查询入口 ------------------------------------------------------------

func tableOf(entity EntityType) (map[edge]TransitionSpec, error) {
	switch entity {
	case EntityRun:
		return runTransitions, nil
	case EntityTask:
		return taskTransitions, nil
	case EntityAttempt:
		return attemptTransitions, nil
	case EntityTester:
		return testerTransitions, nil
	}
	return nil, fmt.Errorf("%w: unknown entity %q", ErrIllegalState, entity)
}

// LookupTransition 返回该实体 from→to 的静态规格；ok=false 表示转移表里没有这条边。
func LookupTransition(entity EntityType, from, to Status) (TransitionSpec, bool) {
	tbl, err := tableOf(entity)
	if err != nil {
		return TransitionSpec{}, false
	}
	spec, ok := tbl[edge{from, to}]
	return spec, ok
}

// RunTransition 报告 Run 的 from→to 是否合法。
func RunTransition(from, to RunStatus) (TransitionSpec, bool) {
	return LookupTransition(EntityRun, from.Status(), to.Status())
}

// TaskTransition 报告 Task 的 from→to 是否合法。
func TaskTransition(from, to TaskStatus) (TransitionSpec, bool) {
	return LookupTransition(EntityTask, from.Status(), to.Status())
}

// AttemptTransition 报告 Attempt 的 from→to 是否合法。
func AttemptTransition(from, to AttemptStatus) (TransitionSpec, bool) {
	return LookupTransition(EntityAttempt, from.Status(), to.Status())
}

// TesterTransition 报告 Tester 的 from→to 是否合法。
func TesterTransition(from, to TesterState) (TransitionSpec, bool) {
	return LookupTransition(EntityTester, from.Status(), to.Status())
}

// CountTransitions 返回该实体转移表的边数；测试与文档对齐用。
func CountTransitions(entity EntityType) int {
	tbl, err := tableOf(entity)
	if err != nil {
		return 0
	}
	return len(tbl)
}

// ---- 状态分组 ------------------------------------------------------------

// RunNonTerminal 是 Run 的全部非终态（R8/R10 的 From 集合）。
func RunNonTerminal() []RunStatus {
	return []RunStatus{RunSubmitted, RunQueued, RunDeploying, RunTesting, RunCleanup, RunCancelling}
}

// TaskNonTerminal 是 Task 的全部非终态。
func TaskNonTerminal() []TaskStatus {
	return []TaskStatus{TaskPending, TaskQueued, TaskPreparing, TaskRunning}
}

// IsTerminal 报告 Attempt 是否终态。
func (s AttemptStatus) IsTerminal() bool {
	switch s {
	case AttemptSucceeded, AttemptFailed, AttemptCanceled:
		return true
	}
	return false
}

// IsTerminal 报告 Tester 是否终态（注册表状态里只有 rejected 是"不会自动前进"的）。
func (s TesterState) IsTerminal() bool { return s == TesterRejected }

// IsTerminalStatus 是给 Kernel 用的通用判定。
func IsTerminalStatus(entity EntityType, s Status) bool {
	switch entity {
	case EntityRun:
		return RunStatus(s).IsTerminal()
	case EntityTask:
		return TaskStatus(s).IsTerminal()
	case EntityAttempt:
		return AttemptStatus(s).IsTerminal()
	case EntityTester:
		return TesterState(s).IsTerminal()
	}
	return false
}

// Statuses 把一组强类型 Run 状态降级成 Kernel 需要的 []Status。
func RunStatuses(in ...RunStatus) []Status {
	out := make([]Status, len(in))
	for i, s := range in {
		out[i] = s.Status()
	}
	return out
}

// TaskStatuses 把一组强类型 Task 状态降级成 Kernel 需要的 []Status。
func TaskStatuses(in ...TaskStatus) []Status {
	out := make([]Status, len(in))
	for i, s := range in {
		out[i] = s.Status()
	}
	return out
}

// AttemptStatuses 把一组强类型 Attempt 状态降级成 Kernel 需要的 []Status。
func AttemptStatuses(in ...AttemptStatus) []Status {
	out := make([]Status, len(in))
	for i, s := range in {
		out[i] = s.Status()
	}
	return out
}

// ---- 守卫（纯函数）------------------------------------------------------

// ValidSkipReason 报告 reason 是否是 tasks.status_reason 允许的 skip 取值
// （03 §3.2 tasks_skipped_reason_ck 的五个值）。
func ValidSkipReason(reason string) bool {
	switch reason {
	case SkipUpstreamAbort, SkipCanceled, SkipDeployFailed, SkipServiceUnavailable, SkipPlatformError:
		return true
	}
	return false
}

// ValidWaitingReason 报告 reason 是否是 tasks.waiting_reason 的六个取值。
func ValidWaitingReason(reason string) bool {
	switch reason {
	case WaitPrecheckGate, WaitSlotWaiting, WaitGlobalLimit, WaitQueueFull,
		WaitInsufficientResources, WaitGangPending:
		return true
	}
	return false
}

// ValidRunStatus / ValidTaskStatus / ValidAttemptStatus 是插入前的枚举兜底校验。
func ValidRunStatus(s Status) bool {
	switch RunStatus(s) {
	case RunSubmitted, RunQueued, RunDeploying, RunTesting, RunCleanup, RunCancelling,
		RunSucceeded, RunCompletedWithErrors, RunServiceUnavailable, RunFailed, RunCanceled:
		return true
	}
	return false
}

func ValidTaskStatus(s Status) bool {
	switch TaskStatus(s) {
	case TaskPending, TaskQueued, TaskPreparing, TaskRunning,
		TaskSucceeded, TaskFailed, TaskCanceled, TaskSkipped:
		return true
	}
	return false
}

func ValidAttemptStatus(s Status) bool {
	switch AttemptStatus(s) {
	case AttemptPreparing, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCanceled:
		return true
	}
	return false
}
