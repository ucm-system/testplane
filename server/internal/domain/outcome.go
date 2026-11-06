package domain

// Run 终态判定（C-5.8）：从上到下，先命中者胜。
//
//	1. 用户取消完成                                   → canceled
//	2. 平台侧错误（Genet 调用失败、K8s 持续不可用…）   → failed
//	3. 部署受理但未就绪 / blocked                      → service_unavailable
//	4. Pre-Check 失败且错误可重试                       → service_unavailable
//	5. 所有非 skipped 的 Task 均 succeeded             → succeeded
//	6. 其余                                            → completed_with_errors
//
// 第 4 条严格按 §7.5：只有"窗口内一直没就绪"这类可重试错误才归 service_unavailable；
// 认证失败等 retryable=false 的 Pre-Check 失败意味着服务在、配置不对，归
// completed_with_errors（详见 C-5.8 的说明）。

// TaskSummary 是终态判定需要的 Task 摘要（不依赖 store 类型）。
type TaskSummary struct {
	Seq       int
	Status    TaskStatus
	ErrorCode string
	// Retryable 是**实例级**取值（Failure.retryable）。
	Retryable bool
	// HasRetryable 表示实例里确实带了 retryable 字段。终态判定以实例为准：
	// 有实例值时绝不拿码级默认覆盖它（07-api.md §3「Pre-Check 错误对象约定」）。
	HasRetryable bool
}

// RunOutcomeInput 是 DecideRunOutcome 的输入。
type RunOutcomeInput struct {
	// Canceled 表示用户取消已经完成（R9 之后判定）。
	Canceled bool
	// PlatformError 表示平台侧不可恢复错误（Genet 调用失败、K8s API 持续不可用、内部异常）。
	PlatformError bool
	// DeployBlocked 表示部署受理但终态 blocked 或排队/部署超窗。
	DeployBlocked bool
	// Precheck 是 seq=0 的 Pre-Check Task 摘要；无 Pre-Check 时为 nil。
	Precheck *TaskSummary
	// Tasks 是该 Run 的全部 Task 摘要（可含 Precheck）。
	Tasks []TaskSummary
}

// PrecheckRetryableCodes 是"窗口内一直没就绪"这一类可重试错误（C-5.8 第 4 条）。
var PrecheckRetryableCodes = map[string]bool{
	CodeEndpointUnreachable: true,
	CodePrecheckFailed:      true,
	CodeTaskTimeout:         true,
}

// DecideRunOutcome 按 C-5.8 的优先级返回 Run 终态。
func DecideRunOutcome(in RunOutcomeInput) RunStatus {
	switch {
	case in.Canceled:
		return RunCanceled
	case in.PlatformError:
		return RunFailed
	case in.DeployBlocked:
		return RunServiceUnavailable
	}

	if in.Precheck != nil && in.Precheck.Status == TaskFailed {
		// 07-api.md §3「Pre-Check 错误对象约定」：终态判定以**实例字段** retryable 为准，
		// 不只看码级默认值。所以只有"码落在 §7.5 的窗口内"并且"实例没有明确说不可重试"
		// 时才归 service_unavailable；401/认证失败、200-包错误这类 retryable=false 的实例
		// 一律走 completed_with_errors，让用户去看具体失败 Task。
		//
		// 码表只在实例**没有给** retryable 时兜底（PrecheckRetryableCodes 的三个码默认可重试）。
		p := in.Precheck
		if PrecheckRetryableCodes[p.ErrorCode] {
			retryable := p.Retryable
			if !p.HasRetryable {
				retryable = true // 码级兜底：窗口内的码默认可重试
			}
			if retryable {
				return RunServiceUnavailable
			}
		}
	}

	executed := 0
	allSucceeded := true
	for _, t := range in.Tasks {
		if t.Status == TaskSkipped {
			continue
		}
		executed++
		if t.Status != TaskSucceeded {
			allSucceeded = false
		}
	}
	if executed > 0 && allSucceeded {
		return RunSucceeded
	}
	if executed == 0 {
		// 全部 skipped：没有任何 Task 真正跑过，按"有错误"收口，由报告解释原因。
		return RunCompletedWithErrors
	}
	return RunCompletedWithErrors
}

// SkipReasonForRunTerminal 返回 Run 进入该终态时，未开始 Task 应写的 skip 原因。
func SkipReasonForRunTerminal(to RunStatus) string {
	switch to {
	case RunCanceled, RunCancelling:
		// 取消的两个阶段用同一个原因：cancelling 时先把未开始的 Task 记 skipped，
		// 到 canceled 时已经没有残留，不能让 cancelled 的 Run 里出现 upstream_abort（D-60）。
		return SkipCanceled
	case RunFailed:
		return SkipPlatformError
	case RunServiceUnavailable:
		return SkipServiceUnavailable
	}
	return SkipUpstreamAbort
}
