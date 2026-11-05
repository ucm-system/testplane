package orchestrator

import (
	"github.com/ucm-system/testplane/server/internal/domain"
)

// precheckRetryableCodes 是 C-5.8 第 4 条点名的"可重试的 Pre-Check 失败"
// （§7.5：窗口内一直没就绪这类错误才归 service_unavailable）。
var precheckRetryableCodes = map[string]struct{}{
	domain.CodeEndpointUnreachable: {},
	domain.CodePrecheckFailed:      {},
	domain.CodeTaskTimeout:         {},
}

// TerminalRunStatus 实现 C-5.8 的终态判定优先级（不含"用户取消"与"平台侧错误"：
// 那两条由调用方显式命令写入，不在"全部 Task 终态"这条路径上）：
//
//	3. 部署受理但未就绪 / blocked → service_unavailable（由 DeployBlocked 直接写）
//	4. pre_check 失败且错误可重试 → service_unavailable
//	5. 所有非 SKIPPED 的 Task 均 succeeded → succeeded
//	6. 其余 → completed_with_errors
//
// 同时返回应当落到 Run 上的 Failure（用于报告与详情页）。
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
	// 第 4 条：只看 Pre-Check 的失败（它是门控，失败代表服务不可用）。
	for _, t := range tasks {
		if t.TesterRole != domain.RolePreCheck || t.Status != domain.TaskFailed {
			continue
		}
		if t.Failure == nil {
			continue
		}
		if _, ok := precheckRetryableCodes[t.Failure.Code]; ok || t.Failure.Retryable {
			return domain.RunServiceUnavailable, t.Failure
		}
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
