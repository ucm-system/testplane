package orchestrator

import (
	"github.com/ucm-system/testplane/server/internal/domain"
)

// TerminalRunStatus 实现 C-5.8 的终态判定优先级（不含"用户取消"与"平台侧错误"：
// 那两条由调用方显式命令写入，不在"全部 Task 终态"这条路径上）：
//
//	3. 部署受理但未就绪 / blocked → service_unavailable（由 DeployBlocked 直接写）
//	4. pre_check 失败且错误**可重试** → service_unavailable
//	5. 所有非 SKIPPED 的 Task 均 succeeded → succeeded
//	6. 其余 → completed_with_errors
//
// **第 4 条的"可重试"只认实例字段 `failure.retryable`**（07-api.md §3 的 Pre-Check
// 错误对象约定：终态判定以实例为准）。不再按错误码查表抬升——否则 Tester 明确写了
// `retryable:false` 的 401 / 200-包错误也会被误判成 service_unavailable。
//
// "实例没有给出 retryable" 的情形由**构造方**负责兜底：编排层自己产生的平台侧失败
// （超时、执行器失联、镜像拉取失败等）在构造 Failure 时就按码级默认值填好 retryable；
// 来自 result.json 的失败则完全尊重 Tester 写的取值。
func TerminalRunStatus(tasks []TaskView) (domain.RunStatus, *domain.Failure) {
	allSucceeded := true
	var failure *domain.Failure
	for _, t := range tasks {
		if t.Status == domain.TaskSkipped {
			continue
		}
		if t.Status != domain.TaskSucceeded {
			allSucceeded = false
			if failure == nil && t.Failure != nil {
				failure = t.Failure
			}
		}
		if failure == nil && t.Failure != nil {
			failure = t.Failure
		}
	}
	if allSucceeded {
		return domain.RunSucceeded, nil
	}
	// 第 4 条：只看 Pre-Check 的失败，且只认实例级 retryable。
	for _, t := range tasks {
		if t.TesterRole != domain.RolePreCheck || t.Status != domain.TaskFailed {
			continue
		}
		if t.Failure == nil {
			continue
		}
		if t.Failure.Retryable {
			return domain.RunServiceUnavailable, t.Failure
		}
		// retryable=false：服务在、配置/权限不对（401、200-包错误等）→ 让用户看具体失败 Task。
		return domain.RunCompletedWithErrors, t.Failure
	}
	return domain.RunCompletedWithErrors, failure
}

// HasPrecheck 报告该 Run 是否包含 Pre-Check Task。
func HasPrecheck(tasks []TaskView) bool {
	for _, t := range tasks {
		if t.TesterRole == domain.RolePreCheck {
			return true
		}
	}
	return false
}
