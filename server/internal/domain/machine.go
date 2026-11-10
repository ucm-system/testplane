package domain

import "fmt"

// 本文件是 02-logical-view.md §2 的机器可执行版本：两套状态机的转移表
// （Task 平台层 + runner 执行记录实例层）。
//
// 铁律（02 §2、§3.6）：
//   - 转移表是唯一合法边集合；表里没有的边 = ErrIllegalState（编程错误，不重试）；
//   - 有边但前置状态不满足 = ErrConflict（并发/竞态，先落库者胜）；
//   - 终态不可逆：任何 From 集合都不含终态。
//
// 新增状态或转移必须同时改三处：本文件、design/views/02-logical-view.md §2、
// server/migrations 的 CHECK 约束。
//
// v2 裁决（C-3.1/C-3.2、D-69/D-70）：
//   - runner **没有重试回路**：failed→queued 边不存在；重试 = 整 Task 新 attempt；
//   - pending/queued 的 runner 取消记 skipped（reason），**pending→canceled 与
//     queued→canceled 边不存在**（传播到 clean 为止，service-clean 永不被 SKIPPED）；
//   - Task 没有独立的 deploying/testing/cleanup 平台阶段：RUNNING 的内部推进 =
//     runner 管道按序执行（D-70）。

// edge 是转移表的主键：一条 from→to 的边。
type edge struct {
	from Status
	to   Status
}

// TransitionSpec 描述一条合法转移的静态属性。
type TransitionSpec struct {
	// Command 是设计文档里的命令名（enqueue / pipeline_start / succeed ...），
	// 只用于日志与事件，不参与判定。
	Command string
	// Event 是写进 state_events.type 的事件类型（§12.6、C-9.1）。
	Event string
}

// 事件类型常量（ARCHITECTURE §12.6 + §7.6；03-data-view state_events 注释）。
const (
	EventTaskSubmitted      = "submitted"
	EventTaskStateChanged   = "state_changed"
	EventTaskFinished       = "task_finished"
	EventRunnerStateChanged = "runner.state_changed"
	EventRunnerStarted      = "runner_started"
	EventRunnerFinished     = "runner_finished"
	EventExportsHarvested   = "exports_harvested"
	EventCleanFinished      = "clean_finished"
	EventCleanupFailed      = "cleanup_failed"
	EventReportWritten      = "report.written"
	EventResyncRequired     = "resync_required"
)

// ---- Task 转移表（C-3.2）---------------------------------------------------

var taskTransitions = map[edge]TransitionSpec{
	// 落库完成，进入编排
	{TaskSubmitted.Status(), TaskQueued.Status()}: {Command: "Enqueue", Event: EventTaskStateChanged},
	// 编排器取到全局并发余量，开始推进 runner 管道（无部署阶段，D-70）
	{TaskQueued.Status(), TaskRunning.Status()}: {Command: "BeginPipeline", Event: EventTaskStateChanged},
	// service-clean 终态后的终态判定（C-5.8）
	{TaskRunning.Status(), TaskSucceeded.Status()}:          {Command: "Finish", Event: EventTaskFinished},
	{TaskRunning.Status(), TaskFailed.Status()}:             {Command: "Finish", Event: EventTaskFinished},
	{TaskRunning.Status(), TaskServiceUnavailable.Status()}: {Command: "Finish", Event: EventTaskFinished},
	// 取消：任意非终态 → cancelling（clean 执行完才落 canceled，C-6.1）
	{TaskSubmitted.Status(), TaskCancelling.Status()}: {Command: "Cancel", Event: EventTaskStateChanged},
	{TaskQueued.Status(), TaskCancelling.Status()}:    {Command: "Cancel", Event: EventTaskStateChanged},
	{TaskRunning.Status(), TaskCancelling.Status()}:   {Command: "Cancel", Event: EventTaskStateChanged},
	// 取消完成
	{TaskCancelling.Status(), TaskCanceled.Status()}: {Command: "Canceled", Event: EventTaskFinished},
	// 平台侧不可恢复错误（Executor 不可用、内部异常、DEPLOY_FAILED）：非终态 → failed。
	// running→failed 同时是 Finish 与平台失败两条规则的边，只登记一次，命令取 Fail。
	{TaskSubmitted.Status(), TaskFailed.Status()}: {Command: "Fail", Event: EventTaskFinished},
	{TaskQueued.Status(), TaskFailed.Status()}:    {Command: "Fail", Event: EventTaskFinished},
	// 整 Task 重试（§7.8 / 03 §4.5）：终态 → queued，随后走 queued→running 开新 attempt。
	// **attempt_no + 1 在同一 Task 上追加**（D-68），历史 attempt 不覆盖；canceled 不可重试
	// （取消是终局）。C-3.2 的转移表未列出这条边，按 §7.8 的语义补齐（见实现报告疑点）。
	{TaskSucceeded.Status(), TaskQueued.Status()}:          {Command: "Retry", Event: EventTaskStateChanged},
	{TaskFailed.Status(), TaskQueued.Status()}:             {Command: "Retry", Event: EventTaskStateChanged},
	{TaskServiceUnavailable.Status(), TaskQueued.Status()}: {Command: "Retry", Event: EventTaskStateChanged},
}

// TerminalTaskStatuses 是 Task 的**终态**集合——唯一真源（03 §3.2）。
//
// Go 侧用 `TerminalTaskStatus(st)`，SQL 侧用 `status = ANY($n)` 并传
// `TerminalTaskStatusStrings()`。
var TerminalTaskStatuses = []TaskStatus{
	TaskSucceeded,
	TaskFailed,
	TaskServiceUnavailable,
	TaskCanceled,
}

// TerminalTaskStatus 报告 Task 状态是否终态。
func TerminalTaskStatus(st TaskStatus) bool {
	for _, t := range TerminalTaskStatuses {
		if st == t {
			return true
		}
	}
	return false
}

// TerminalTaskStatusStrings 返回 Task 终态集合的字符串形态（供 SQL 的 ANY($n) 使用）。
func TerminalTaskStatusStrings() []string {
	out := make([]string, 0, len(TerminalTaskStatuses))
	for _, t := range TerminalTaskStatuses {
		out = append(out, string(t))
	}
	return out
}

// ---- runner 执行记录转移表（C-3.1）----------------------------------------

var runnerTransitions = map[edge]TransitionSpec{
	// 前一 runner 出终态（串行推进）或为管道第一个 runner；计算 waiting_reason
	{RunnerPending.Status(), RunnerQueued.Status()}: {Command: "Queue", Event: EventRunnerStateChanged},
	// upstream_abort / canceled（传播到 clean 为止）
	{RunnerPending.Status(), RunnerSkipped.Status()}: {Command: "Skip", Event: EventRunnerStateChanged},
	// 拿到全局 permit +（仅外接 endpoint_exclusive 时）端点槽位；创建 Job
	{RunnerQueued.Status(), RunnerPreparing.Status()}: {Command: "Prepare", Event: EventRunnerStarted},
	// upstream_abort / canceled（传播到 clean 为止）
	{RunnerQueued.Status(), RunnerSkipped.Status()}: {Command: "Skip", Event: EventRunnerStateChanged},
	// informer 观察到容器已启动；写 started_at
	{RunnerPreparing.Status(), RunnerRunning.Status()}: {Command: "Start", Event: EventRunnerStateChanged},
	// Job 创建失败 / 镜像拉取失败 / 挂载缺失；记录错误码
	{RunnerPreparing.Status(), RunnerFailed.Status()}: {Command: "Fail", Event: EventRunnerFinished},
	// 取消：删除 Job，释放租约
	{RunnerPreparing.Status(), RunnerCanceled.Status()}: {Command: "Cancel", Event: EventRunnerFinished},
	// 退出码 0 且产物契约满足（三选一 + result.json 合法 + exports 可解析）；收割 exports
	{RunnerRunning.Status(), RunnerSucceeded.Status()}: {Command: "Succeed", Event: EventRunnerFinished},
	// 非零退出 / 超时 / 结果缺失或非法 / EXPORT_INVALID
	{RunnerRunning.Status(), RunnerFailed.Status()}: {Command: "Fail", Event: EventRunnerFinished},
	// 运行期取消
	{RunnerRunning.Status(), RunnerCanceled.Status()}: {Command: "Cancel", Event: EventRunnerFinished},
}

// TerminalRunnerStatuses 是 runner 执行记录的**终态**集合——唯一真源。
var TerminalRunnerStatuses = []RunnerStatus{
	RunnerSucceeded,
	RunnerFailed,
	RunnerSkipped,
	RunnerCanceled,
}

// TerminalRunnerStatus 报告 runner 执行记录状态是否终态。
func TerminalRunnerStatus(st RunnerStatus) bool {
	for _, t := range TerminalRunnerStatuses {
		if st == t {
			return true
		}
	}
	return false
}

// TerminalRunnerStatusStrings 返回 runner 终态集合的字符串形态（供 SQL 的 ANY($n) 使用）。
func TerminalRunnerStatusStrings() []string {
	out := make([]string, 0, len(TerminalRunnerStatuses))
	for _, t := range TerminalRunnerStatuses {
		out = append(out, string(t))
	}
	return out
}

// ---- 查询入口 ------------------------------------------------------------

func tableOf(entity EntityType) (map[edge]TransitionSpec, error) {
	switch entity {
	case EntityTask:
		return taskTransitions, nil
	case EntityRunnerRun:
		return runnerTransitions, nil
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

// TaskTransition 报告 Task 的 from→to 是否合法。
func TaskTransition(from, to TaskStatus) (TransitionSpec, bool) {
	return LookupTransition(EntityTask, from.Status(), to.Status())
}

// RunnerTransition 报告 runner 执行记录的 from→to 是否合法。
func RunnerTransition(from, to RunnerStatus) (TransitionSpec, bool) {
	return LookupTransition(EntityRunnerRun, from.Status(), to.Status())
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

// TaskNonTerminal 是 Task 的全部非终态。
func TaskNonTerminal() []TaskStatus {
	return []TaskStatus{TaskSubmitted, TaskQueued, TaskRunning, TaskCancelling}
}

// RunnerNonTerminal 是 runner 执行记录的全部非终态。
func RunnerNonTerminal() []RunnerStatus {
	return []RunnerStatus{RunnerPending, RunnerQueued, RunnerPreparing, RunnerRunning}
}

// Statuses 把一组强类型 Task 状态降级成 Kernel 需要的 []Status。
func TaskStatuses(in ...TaskStatus) []Status {
	out := make([]Status, len(in))
	for i, s := range in {
		out[i] = s.Status()
	}
	return out
}

// Statuses 把一组强类型 runner 状态降级成 Kernel 需要的 []Status。
func RunnerStatuses(in ...RunnerStatus) []Status {
	out := make([]Status, len(in))
	for i, s := range in {
		out[i] = s.Status()
	}
	return out
}

// IsTerminalStatus 是给 Kernel 用的通用判定。
func IsTerminalStatus(entity EntityType, s Status) bool {
	switch entity {
	case EntityTask:
		return TaskStatus(s).IsTerminal()
	case EntityRunnerRun:
		return RunnerStatus(s).IsTerminal()
	}
	return false
}

// ---- 守卫（纯函数）------------------------------------------------------

// ValidSkipReason 报告 reason 是否是 task_runners.status_reason 允许的 skip 取值
// （03 §3.2 task_runners_skipped_reason_ck：upstream_abort | canceled）。
func ValidSkipReason(reason string) bool {
	switch reason {
	case SkipUpstreamAbort, SkipCanceled:
		return true
	}
	return false
}

// ValidWaitingReason 报告 reason 是否是 task_runners.waiting_reason 的合法取值
// （C-4.7：平台自身两项 + K8s/Volcano 三项）。
func ValidWaitingReason(reason string) bool {
	switch reason {
	case WaitSlotWaiting, WaitGlobalLimit, WaitQueueFull, WaitInsufficientResources, WaitGangPending:
		return true
	}
	return false
}

// ValidTaskStatus / ValidRunnerStatus / ValidAttemptStatus 是插入前的枚举兜底校验。
func ValidTaskStatus(s Status) bool {
	switch TaskStatus(s) {
	case TaskSubmitted, TaskQueued, TaskRunning, TaskCancelling,
		TaskSucceeded, TaskFailed, TaskServiceUnavailable, TaskCanceled:
		return true
	}
	return false
}

func ValidRunnerStatus(s Status) bool {
	switch RunnerStatus(s) {
	case RunnerPending, RunnerQueued, RunnerPreparing, RunnerRunning,
		RunnerSucceeded, RunnerFailed, RunnerSkipped, RunnerCanceled:
		return true
	}
	return false
}

func ValidAttemptStatus(s Status) bool {
	switch AttemptStatus(s) {
	case AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCanceled:
		return true
	}
	return false
}
