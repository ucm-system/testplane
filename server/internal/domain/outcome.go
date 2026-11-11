package domain

// Task 终态判定（C-5.8）：从上到下，先命中者胜。
//
//	1. 用户取消完成                                   → canceled
//	2. 平台侧错误（service-deploy 归类 DEPLOY_FAILED、
//	   K8s 持续不可用、内部异常…）                     → failed
//	3. 部署受理但未就绪 / blocked（DEPLOY_BLOCKED）     → service_unavailable
//	4. Pre-Check 失败且错误可重试                       → service_unavailable
//	5. 所有非 skipped 的测试类 runner 均 succeeded      → succeeded
//	6. 其余（存在失败且失败策略允许继续）                → failed
//
// 第 4 条严格按 §7.5：只有"窗口内一直没就绪"这类可重试错误才归 service_unavailable；
// 认证失败等 retryable=false 的 Pre-Check 失败意味着服务在、配置不对，归 failed
// （v1 的 COMPLETED_WITH_ERRORS 语义并入 failed，D-35）。

// RunnerSummary 是终态判定需要的 runner 执行记录摘要（不依赖 store 类型）。
type RunnerSummary struct {
	Seq       int
	Role      string // test | pre_check | deploy | clean
	Status    RunnerStatus
	ErrorCode string
	// Retryable 是**实例级**取值（Failure.retryable）。
	Retryable bool
	// HasRetryable 表示实例里确实带了 retryable 字段。终态判定以实例为准：
	// 有实例值时绝不拿码级默认覆盖它（07-api.md §3「Pre-Check 错误对象约定」）。
	HasRetryable bool
}

// TaskOutcomeInput 是 DecideTaskOutcome 的输入。
type TaskOutcomeInput struct {
	// Canceled 表示用户取消已经完成（clean 之后判定）。
	Canceled bool
	// PlatformError 表示平台侧不可恢复错误（K8s API 持续不可用、内部异常）。
	PlatformError bool
	// Runners 是该 Task（最后一个 attempt）的全部 runner 摘要。
	Runners []RunnerSummary
}

// PrecheckRetryableCodes 是"窗口内一直没就绪"这一类可重试错误（C-5.8 第 4 条）。
var PrecheckRetryableCodes = map[string]bool{
	CodeEndpointUnreachable: true,
	CodePrecheckFailed:      true,
	CodeRunnerTimeout:       true,
}

// DecideTaskOutcome 按 C-5.8 的优先级返回 Task 终态。
func DecideTaskOutcome(in TaskOutcomeInput) TaskStatus {
	switch {
	case in.Canceled:
		return TaskCanceled
	case in.PlatformError:
		return TaskFailed
	}

	var (
		precheck   *RunnerSummary
		deployFail bool
		deployBlk  bool
	)
	for i := range in.Runners {
		r := in.Runners[i]
		switch r.Role {
		case RolePreCheck:
			if r.Status == RunnerFailed && precheck == nil {
				pre := r
				precheck = &pre
			}
		case RoleDeploy:
			switch r.ErrorCode {
			case CodeDeployFailed:
				deployFail = true
			case CodeDeployBlocked:
				deployBlk = true
			}
		}
	}
	// 平台侧失败优先于部署阻塞：service-deploy 调 Genet 失败（DEPLOY_FAILED）是
	// 编排异常，归 failed；blocked/超窗（DEPLOY_BLOCKED）才是服务不可用。
	if deployFail {
		return TaskFailed
	}
	if deployBlk {
		return TaskServiceUnavailable
	}
	if precheck != nil {
		// 07-api.md §3「Pre-Check 错误对象约定」：终态判定以**实例字段** retryable 为准，
		// 不只看码级默认值。所以只有"码落在 §7.5 的窗口内"并且"实例没有明确说不可重试"
		// 时才归 service_unavailable；401/认证失败、200-包错误这类 retryable=false 的实例
		// 一律走 failed，让用户去看具体错误码。
		//
		// 码表只在实例**没有给** retryable 时兜底（PrecheckRetryableCodes 的三个码默认可重试）。
		p := precheck
		if PrecheckRetryableCodes[p.ErrorCode] {
			retryable := p.Retryable
			if !p.HasRetryable {
				retryable = true // 码级兜底：窗口内的码默认可重试
			}
			if retryable {
				return TaskServiceUnavailable
			}
		}
	}

	executed := 0
	allSucceeded := true
	for _, r := range in.Runners {
		if r.Role == RoleClean {
			continue // clean 失败不影响终态（§7.5：只打 cleanup_failed）
		}
		if r.Status == RunnerSkipped {
			continue
		}
		executed++
		if r.Status != RunnerSucceeded {
			allSucceeded = false
		}
	}
	if executed > 0 && allSucceeded {
		return TaskSucceeded
	}
	return TaskFailed
}

// SkipReasonForTaskTerminal 返回 Task 进入该终态时，未开始 runner 应写的 skip 原因。
func SkipReasonForTaskTerminal(to TaskStatus) string {
	switch to {
	case TaskCanceled, TaskCancelling:
		// 取消的两个阶段用同一个原因：cancelling 时先把未开始的 runner 记 skipped，
		// 到 canceled 时已经没有残留。
		return SkipCanceled
	}
	return SkipUpstreamAbort
}
